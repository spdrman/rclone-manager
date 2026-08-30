package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// ActionRetentionApply names the durable operation row a confirmed
// retention apply (ApplyRetentionPlan) is recorded under, mirroring
// ActionRunCycle above. Unlike run_cycle, applying a retention plan
// finishes synchronously within the same call (see ApplyRetentionPlan's
// own doc for why), but it is still recorded through the identical
// internal/state.Journal operations table a client can already poll
// GET /api/v1/operations/{id} against, for the same durability and
// auditability reasons run_cycle is (docs/EPIC-B-multi-nas.md §71 WP 3.1's
// own "durable retention operation" bullet).
const ActionRetentionApply = "retention_apply"

// retentionPlanTTL bounds how long a previewed retention plan
// (RetentionPlan) stays applyable, independent of whether its inventory or
// configuration precondition ever actually changes. docs/EPIC-B-multi-nas.md
// §15.6 requires a preview response to carry expires_at, and §29.3 (the UI
// wizard) requires the UI to present it before the administrator confirms;
// this is what keeps a plan that has simply sat open in a confirmation
// dialog for a long time from staying applyable forever. It is a var, not
// a const, so a test can shrink it instead of waiting out the real value
// (mirrors closeDrainTimeout in service.go).
var retentionPlanTTL = 10 * time.Minute

// ErrRetentionPlanStale is returned by ApplyRetentionPlan when the plan
// named by ApplyRetentionRequest.PlanID has expired, or when the backup
// set's inventory or this BackupService's own configuration has changed
// since that plan was computed — docs/EPIC-B-multi-nas.md §15.6's own
// RETENTION_PLAN_STALE contract, and this issue's own required Given/When/
// Then example ("inventory changes before apply ... THEN zero files are
// deleted AND RETENTION_PLAN_STALE is returned"). internal/retention.
// PruneApply is never called on this path: see ApplyRetentionPlan's own
// doc for exactly where this check happens relative to the one call that
// could actually delete anything.
var ErrRetentionPlanStale = errors.New("service: retention plan is stale")

// ErrRetentionPlanNotFound is returned by ApplyRetentionPlan when PlanID
// names no plan this BackupService currently holds: it was never issued by
// PreviewRetention, or it already resolved (applied, found stale, or
// expired) on an earlier ApplyRetentionPlan call. Plans are single-use —
// see ApplyRetentionPlan's own doc.
var ErrRetentionPlanNotFound = errors.New("service: retention plan not found")

// RetentionArtifactVerdict is one managed artifact's FR-18/FR-19/FR-20
// classification within a RetentionPlan: internal/retention.PruneVerdict,
// translated into the plain, provider-agnostic shape this package's own
// contract requires past this boundary (see service.go's package doc —
// nothing from internal/retention's own vocabulary, PruneAction included,
// is exposed directly).
type RetentionArtifactVerdict struct {
	Artifact string

	// Action is "KEEP", "DELETE" or "REFUSE" — internal/retention.
	// PruneAction's own three values, as plain strings.
	Action string

	Reason string

	// Tiers lists which GFS tier(s) (and/or "LAST_KNOWN_GOOD") kept this
	// artifact; empty for a DELETE or REFUSE verdict.
	Tiers []string
}

// RetentionPlan is docs/EPIC-B-multi-nas.md §15.6's own preview/apply
// response shape. PreviewRetention returns one; so does ApplyRetentionPlan
// on success, re-expressing the exact plan that was just applied the same
// way a preview would have been, so a caller never has to reconcile two
// different response shapes for "what would happen" versus "what
// happened".
type RetentionPlan struct {
	PlanID            string
	BackupSetID       string
	InventoryRevision string
	ConfigRevision    string
	ExpiresAt         time.Time
	KeepCount         int
	DeleteCount       int
	ReclaimBytes      int64
	Verdicts          []RetentionArtifactVerdict

	// OperationID names the durable operation row (ActionRetentionApply)
	// this apply was recorded under, pollable through the same
	// GetOperation/GET /api/v1/operations/{id} surface run_cycle already
	// is. Empty on a plan PreviewRetention returns: a preview creates no
	// operation, only an apply does.
	OperationID string
}

// ApplyRetentionRequest is what a caller submits to ApplyRetentionPlan.
type ApplyRetentionRequest struct {
	// PlanID must name a plan PreviewRetention previously issued that has
	// not yet been applied, found stale, or expired.
	PlanID string

	// Actor is the authenticated caller's identity, recorded on the
	// durable operation row (see ActionRetentionApply), exactly like
	// RunCycleRequest.Actor.
	Actor string
}

// retentionPlanRecord is one previewed plan's bookkeeping: enough to
// detect staleness (inventoryRevision, configRevision, expiresAt) and to
// know which backup set to re-run PruneApply against. It deliberately does
// NOT keep the previewed verdicts themselves: ApplyRetentionPlan re-derives
// them fresh from internal/retention.PruneApply once it has confirmed
// nothing this plan depended on has changed — see that method's own doc
// for why re-deriving, rather than replaying a cached verdict set, is
// still "applying the exact plan the administrator reviewed" and not a
// second, possibly-divergent decision.
type retentionPlanRecord struct {
	planID            string
	set               model.BackupSetID
	inventoryRevision string
	configRevision    string
	expiresAt         time.Time
}

// PreviewRetention computes backup set source/set's current FR-18/FR-19/
// FR-20 retention plan via internal/app.Service.PrunePreview (itself a
// thin wrapper over internal/retention.PruneDecide — see that method's own
// doc), and issues it a fresh, immutable plan_id. It touches nothing:
// PruneDecide's own no-mutation contract holds all the way up to this
// boundary.
//
// The returned plan is also stored, keyed by its own PlanID, so a later
// ApplyRetentionPlan call naming that PlanID can compare its own freshly
// observed inventory/configuration against exactly what THIS call saw,
// rather than against whatever the caller might claim it saw.
func (b *BackupService) PreviewRetention(ctx context.Context, source, set string) (RetentionPlan, error) {
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		return RetentionPlan{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// One atomic read of {inner, revision} up front, exactly as
	// SubmitRunCycle does (operations.go): the configuration revision this
	// plan records as its own must be the revision of the very
	// internal/app.Service that computed it, never one from a hot-reload
	// that landed in between (see BackupService.state's own doc). Reading
	// the two separately is precisely the mismatched pair configState
	// exists to make impossible, and here it would mean recording a
	// staleness baseline no observation ever actually saw.
	st := b.state.Load()

	plan, err := st.inner.PrunePreview(ctx, id)
	if err != nil {
		var nf *app.NotFoundError
		if errors.As(err, &nf) {
			return RetentionPlan{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
		}
		// Deliberately not %w-wrapped with err: err may originate from the
		// state layer (a SQLite failure) and this method's contract is
		// that nothing from that layer's vocabulary crosses this boundary
		// — see SubmitRunCycle's identical reasoning in operations.go.
		return RetentionPlan{}, fmt.Errorf("service: preview retention: an internal error occurred")
	}

	return b.newRetentionPlan(id, st.revision, plan), nil
}

// newRetentionPlan issues plan a fresh plan_id, records its bookkeeping
// for ApplyRetentionPlan's later staleness check, and returns the plain
// RetentionPlan shape this package hands to callers outside core/.
//
// configRevision is passed in rather than re-read from b.state here, so it
// is guaranteed to be the revision of the exact configState whose inner
// produced plan: see PreviewRetention's own doc for why re-reading would
// reopen the mismatched-pair window.
func (b *BackupService) newRetentionPlan(set model.BackupSetID, configRevision string, plan app.PrunePlan) RetentionPlan {
	created := now()
	planID := "retplan_" + uuid.New().String()
	inventoryRevision := computeInventoryRevision(plan.Records)
	expiresAt := created.Add(retentionPlanTTL)

	b.retentionMu.Lock()
	b.retentionPlans[planID] = retentionPlanRecord{
		planID:            planID,
		set:               set,
		inventoryRevision: inventoryRevision,
		configRevision:    configRevision,
		expiresAt:         expiresAt,
	}
	b.retentionMu.Unlock()

	return summarizeRetentionPlan(set, planID, inventoryRevision, configRevision, expiresAt, "", plan)
}

// ApplyRetentionPlan applies exactly the plan named by req.PlanID, or
// applies nothing at all.
//
// # Where the staleness check sits relative to the one call that can delete anything
//
// The whole point of this method, and of issue #96 (B3.1), is that these
// two things never happen in the wrong order: first, this method compares
// a freshly-computed inventory_revision (over the backup set's current
// journal rows) and this BackupService's current ConfigRevision() against
// what PreviewRetention recorded when it issued PlanID; only if BOTH still
// match does this method go on to call internal/app.Service.PruneApply
// (internal/retention.PruneApply underneath it) at all. If either has
// changed, or the plan has simply expired, this returns
// ErrRetentionPlanStale immediately and PruneApply is never invoked — not
// "invoked but its result discarded", never invoked — so zero files are
// deleted, exactly as docs/EPIC-B-multi-nas.md §15.6 and this issue's own
// Given/When/Then example require.
//
// # Re-deriving, not replaying, the confirmed plan
//
// This method does not keep PreviewRetention's own verdict set around to
// "replay" at apply time; it calls PruneApply fresh, against whatever the
// journal says right now. That is still "the exact plan the administrator
// reviewed" and not a second, possibly-divergent decision, precisely
// because the staleness check just above proves the inputs (this backup
// set's journal rows, this BackupService's configuration) have not moved
// since PreviewRetention last read them — and internal/retention.
// PruneDecide/PruneApply are pure functions of exactly those inputs (see
// that package's own "Determinism" doc): identical inputs can only ever
// produce the identical verdict set. Re-deriving also means PruneApply's
// own second, immediately-before-delete safety re-check (see that
// function's doc) runs against the real current disk state rather than a
// cached decision from moments earlier, closing the same TOCTOU gap that
// function's own doc already discusses, instead of reopening a new one at
// this layer.
//
// # Single-use
//
// PlanID is consumed by this call whether it succeeds, is found stale, or
// fails outright: a second ApplyRetentionPlan call for the same PlanID
// afterward gets ErrRetentionPlanNotFound, not a replay of the earlier
// result. A caller that wants to try again must call PreviewRetention
// again and confirm the new plan, exactly as docs/EPIC-B-multi-nas.md
// §29.3 step 6 requires ("if stale, abort with no deletions and require
// re-preview").
func (b *BackupService) ApplyRetentionPlan(ctx context.Context, req ApplyRetentionRequest) (RetentionPlan, error) {
	if req.PlanID == "" {
		return RetentionPlan{}, fmt.Errorf("%w: retention apply requires a plan_id", ErrInvalidRequest)
	}

	b.retentionMu.Lock()
	stored, ok := b.retentionPlans[req.PlanID]
	b.retentionMu.Unlock()
	if !ok {
		return RetentionPlan{}, fmt.Errorf("%w: %s", ErrRetentionPlanNotFound, req.PlanID)
	}

	nowT := now()
	if nowT.After(stored.expiresAt) {
		b.discardRetentionPlan(req.PlanID)
		return RetentionPlan{}, fmt.Errorf("%w: plan %s expired at %s", ErrRetentionPlanStale, req.PlanID, stored.expiresAt.Format(time.RFC3339))
	}

	// One atomic read of {inner, revision} for the whole rest of this
	// call: the revision the staleness check below compares against, the
	// revision written to the operations row, and the internal/app.Service
	// that ultimately deletes anything must all come from the same
	// configState. Re-reading b.state at each of those three points would
	// let a hot-reload land in between and produce exactly the situation
	// this method exists to prevent, a delete carried out under a
	// configuration the staleness check never saw.
	st := b.state.Load()

	currentRecords, err := b.journal.ListByBackupSet(ctx, stored.set)
	if err != nil {
		return RetentionPlan{}, fmt.Errorf("service: apply retention: an internal error occurred")
	}
	if computeInventoryRevision(currentRecords) != stored.inventoryRevision || st.revision != stored.configRevision {
		b.discardRetentionPlan(req.PlanID)
		return RetentionPlan{}, fmt.Errorf("%w: backup set %s changed since plan %s was previewed", ErrRetentionPlanStale, stored.set, req.PlanID)
	}

	opID := "op_" + uuid.New().String()
	if _, err := b.journal.CreateOperation(ctx, state.OperationRequest{
		OperationID:    opID,
		IdempotencyKey: "retention_apply_" + req.PlanID,
		Actor:          req.Actor,
		BackupSet:      stored.set.String(),
		ConfigRevision: st.revision,
		Action:         ActionRetentionApply,
		Parameters:     fmt.Sprintf(`{"plan_id":%q}`, req.PlanID),
		CreatedAt:      nowT,
	}); err != nil {
		// Deliberately not %w-wrapped: see PreviewRetention's identical
		// reasoning above.
		return RetentionPlan{}, fmt.Errorf("service: apply retention: an internal error occurred")
	}
	if err := b.journal.MarkOperationRunning(context.Background(), opID, now()); err != nil {
		b.logger.Error(context.Background(), "mark-retention-apply-running", err)
	}

	applied, err := st.inner.PruneApply(ctx, stored.set)
	b.discardRetentionPlan(req.PlanID)
	if err != nil {
		if failErr := b.journal.FailOperation(context.Background(), opID, now(), "an internal error occurred while applying retention"); failErr != nil {
			b.logger.Error(context.Background(), "fail-retention-apply", failErr)
		}
		return RetentionPlan{}, fmt.Errorf("service: apply retention: an internal error occurred")
	}

	result := summarizeRetentionPlan(stored.set, req.PlanID, stored.inventoryRevision, stored.configRevision, stored.expiresAt, opID, applied)

	if err := b.journal.CompleteOperation(context.Background(), opID, now(), summarizeRetentionApply(result)); err != nil {
		b.logger.Error(context.Background(), "complete-retention-apply", err)
	}

	return result, nil
}

// discardRetentionPlan removes planID from this BackupService's in-memory
// plan store: see ApplyRetentionPlan's "Single-use" doc for why every one
// of its return paths (applied, found stale, or failed outright) calls
// this.
func (b *BackupService) discardRetentionPlan(planID string) {
	b.retentionMu.Lock()
	delete(b.retentionPlans, planID)
	b.retentionMu.Unlock()
}

// summarizeRetentionPlan translates one internal/app.PrunePlan (and the
// plan_id/revision/expiry bookkeeping that goes with it, from whichever of
// PreviewRetention or ApplyRetentionPlan is calling) into the plain
// RetentionPlan shape this package hands to callers outside core/.
//
// ReclaimBytes sums Transfer.BytesTransferred (internal/state.Record) over
// every PruneDelete verdict: the number of bytes this backup set's own
// completed transfer step actually wrote for that artifact, which for a
// managed, completed local artifact (the only kind PruneDecide ever
// classifies at all — see that function's own doc) is the artifact's
// local file size at commit time. An artifact with no recorded Transfer
// result (defensive only: DecideKeep/PruneDecide never classify anything
// outside the managed-complete states, which by construction cannot exist
// without one) contributes zero rather than guessing.
func summarizeRetentionPlan(set model.BackupSetID, planID, inventoryRevision, configRevision string, expiresAt time.Time, operationID string, plan app.PrunePlan) RetentionPlan {
	recByArtifact := make(map[model.ArtifactID]state.Record, len(plan.Records))
	for _, r := range plan.Records {
		recByArtifact[r.Artifact] = r
	}

	verdicts := make([]RetentionArtifactVerdict, len(plan.Verdicts))
	var keepCount, deleteCount int
	var reclaimBytes int64
	for i, v := range plan.Verdicts {
		switch v.Action {
		case retention.PruneKeep:
			keepCount++
		case retention.PruneDelete:
			deleteCount++
			if rec, ok := recByArtifact[v.Artifact]; ok && rec.Transfer != nil {
				reclaimBytes += rec.Transfer.BytesTransferred
			}
		}

		tiers := make([]string, len(v.Tiers))
		for j, t := range v.Tiers {
			tiers[j] = string(t)
		}
		verdicts[i] = RetentionArtifactVerdict{
			Artifact: v.Artifact.Name,
			Action:   string(v.Action),
			Reason:   v.Reason,
			Tiers:    tiers,
		}
	}

	return RetentionPlan{
		PlanID:            planID,
		BackupSetID:       set.String(),
		InventoryRevision: inventoryRevision,
		ConfigRevision:    configRevision,
		ExpiresAt:         expiresAt,
		KeepCount:         keepCount,
		DeleteCount:       deleteCount,
		ReclaimBytes:      reclaimBytes,
		Verdicts:          verdicts,
		OperationID:       operationID,
	}
}

// computeInventoryRevision fingerprints records: the exact journal
// snapshot a retention plan (either PreviewRetention's own PrunePreview
// call, or ApplyRetentionPlan's freshly re-read one) was computed against.
// Two calls over byte-for-byte identical records produce the same
// revision; any difference in the set (a new artifact discovered, a state
// transition, a changed path) changes it. Mirrors computeConfigRevision's
// own approach (service.go) — hash a canonical encoding — for the same
// reason: deterministic without this package needing to canonicalize
// anything itself beyond a stable sort order, since map iteration is not a
// concern here (records is already a plain slice) but the order records
// was fetched in should not matter to the fingerprint any more than
// GFSDecide's own output is allowed to depend on the order records was
// passed in (see that function's own doc).
//
// This intentionally hashes every field of every record for this backup
// set (via encoding/json, not just the handful PruneDecide's own decision
// actually reads): "inventory changed" is meant broadly here, favouring a
// plan going stale too often over a plan silently staying applyable
// against a backup set that changed in some way this function did not
// think to single out.
func computeInventoryRevision(records []state.Record) string {
	sorted := append([]state.Record(nil), records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Artifact.Name < sorted[j].Artifact.Name })

	b, err := json.Marshal(sorted)
	if err != nil {
		// records is plain data (strings, ints, time.Time, a few pointer/
		// named-string fields) that already round-trips through SQLite via
		// internal/state's own scanRecord: Marshal failing here would mean
		// this package's assumption about that shape no longer holds,
		// which is a programmer error to notice loudly, not a runtime
		// condition to paper over with a fallback revision that would
		// silently never change. Mirrors computeConfigRevision's identical
		// reasoning.
		panic(fmt.Sprintf("service: computing inventory revision: %v", err))
	}
	sum := sha256.Sum256(b)
	return "inv_" + hex.EncodeToString(sum[:])[:16]
}

// retentionApplySummary is the opaque JSON blob stored in a completed
// retention_apply operation's Result (state.Journal.CompleteOperation),
// mirroring cycleSummary's own role for run_cycle (operations.go):
// deliberately narrow, nothing from internal/retention's own report types
// leaks into it.
type retentionApplySummary struct {
	KeepCount    int   `json:"keep_count"`
	DeleteCount  int   `json:"delete_count"`
	ReclaimBytes int64 `json:"reclaim_bytes"`
}

func summarizeRetentionApply(r RetentionPlan) string {
	b, err := json.Marshal(retentionApplySummary{
		KeepCount:    r.KeepCount,
		DeleteCount:  r.DeleteCount,
		ReclaimBytes: r.ReclaimBytes,
	})
	if err != nil {
		// retentionApplySummary is a plain struct of ints; Marshal cannot
		// actually fail against it (mirrors summarizeCycle's identical
		// fallback).
		return "{}"
	}
	return string(b)
}
