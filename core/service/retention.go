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

// ErrRetentionApplyBusy is returned by ApplyRetentionPlan when a backup
// cycle (RunCycle, scheduled or submitted) is executing on this
// BackupService right now. Applying a plan and running a cycle both move
// the very journal rows the plan's own staleness check is computed over,
// so the two are serialised against each other through the same runOnce
// mutex SubmitRunCycle already uses (see BackupService.runOnce's own doc):
// an apply that arrives mid-cycle is refused outright rather than deciding
// against a snapshot the cycle is in the middle of changing.
//
// Deliberately its OWN sentinel rather than ErrRetentionPlanStale: nothing
// about the plan is wrong, the server is simply busy, the plan_id is NOT
// consumed, and a client can retry the same plan_id in a moment. Reporting
// it as staleness would tell an operator to re-preview when they do not
// need to (this issue's own review, mandatory finding M1).
var ErrRetentionApplyBusy = errors.New("service: a backup cycle is running; retention apply refused")

// maxRetentionPlans caps how many previewed-but-unresolved plans this
// BackupService holds at once. PreviewRetention is a plain authenticated
// GET and the UI issues one on every dialog open, every dependency change
// and every "Review new plan", while a plan is only ever removed by an
// apply that names it — so without a ceiling, previews that are never
// applied (the normal case) accumulate for the life of the process, in the
// daemon that is also running the backups (this issue's own review,
// mandatory finding M8).
//
// Eviction is oldest-first and can in principle evict a plan an operator
// is still reading; that surfaces as ErrRetentionPlanNotFound on apply,
// which is the safe direction (re-preview and re-confirm), and the cap is
// far above any plausible number of confirmation dialogs open at once.
const maxRetentionPlans = 64

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

	// Medium is WHERE the copy this verdict is about lives (EPIC E
	// FR-30, issue #239): "local", or the id of a configured storage
	// medium. It is empty only when nothing could confirm the location,
	// which internal/retention always reports as a REFUSE.
	//
	// FR-30 asks the mandatory dry-run to explain per-artifact where a
	// deletion would happen, not only whether, and this is that answer
	// on the surface an operator confirms from. "Delete 40 artifacts"
	// means something very different when half of them are objects in a
	// bucket somebody else pays for.
	Medium string

	Reason string

	// Tiers lists which GFS tier(s) (and/or "LAST_KNOWN_GOOD") kept this
	// artifact, each paired with which of FR-18's two placements selected
	// it there; empty for a DELETE or REFUSE verdict.
	Tiers []RetentionTierSelection
}

// RetentionTierSelection is one tier's claim on an artifact within a
// RetentionArtifactVerdict: internal/retention.GFSTierSelection, as two
// plain strings (see service.go's package doc — nothing from that
// package's own vocabulary is exposed directly past this boundary).
//
// The pair travels together rather than as a tier list beside a placement
// list, and rather than as one placement per verdict, because one
// artifact can be selected by DAILY through one placement and by MONTHLY
// through the other (issue #218). A per-verdict attribution would be
// wrong in exactly that case, which is the case an operator is reading
// the preview to understand.
type RetentionTierSelection struct {
	// Tier is internal/retention.GFSTier's own value: a configured tier
	// name upper-cased, or the reserved "LAST_KNOWN_GOOD". The set is
	// open, because FR-18's chain is operator-defined.
	Tier string

	// SelectedBy is internal/retention.GFSSelectedBy's own value:
	// "DISCOVERY", "PRODUCER", "BOTH", or "PROTECTION" for FR-19's term,
	// which is not a placement at all.
	SelectedBy string
}

// RetentionMove is one artifact a retention pass worked out is not on
// the medium its chain says it belongs on: internal/retention.HomeMove,
// as three plain strings (see service.go's package doc — nothing from
// that package's own vocabulary crosses this boundary).
//
// FR-27's home rule is the whole content of it: the first tier in chain
// order that currently selects an artifact names its home. A move is a
// statement about placement and nothing else — planning one never adds an
// artifact to KEEP and never removes one — which is why it travels beside
// the verdicts rather than inside them.
type RetentionMove struct {
	Artifact string

	// FromMedium is where the artifact's one ACTIVE placement is today,
	// and ToMedium is the medium its home tier names. They are always
	// different: an artifact already at home is not a move.
	FromMedium string
	ToMedium   string
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

	// Moves is every artifact this plan would relocate, in verdict order
	// (EPIC E FR-27/FR-30, issue #239). It is empty for a deployment
	// that declares no storage medium, which is every deployment before
	// this EPIC.
	//
	// It is on the plan rather than behind a second call for the same
	// reason Retention is: an apply is confirmed against a plan_id, and
	// what that plan_id commits to has to be the whole of what was
	// shown. A moves section fetched separately could be rendered beside
	// verdicts it does not belong to.
	Moves []RetentionMove

	// UnconfirmedPlacements names every artifact whose current location
	// could not be established, in verdict order. No move is planned for
	// one, and that is exactly why the list exists rather than the
	// artifact being quietly skipped: "I could not confirm where this
	// is" and "this is already where it belongs" produce the same
	// silence and are not the same claim.
	//
	// The two shapes that produce it are an artifact with no ACTIVE
	// placement at all and one with more than one, which is a move
	// already in flight.
	UnconfirmedPlacements []string

	// OperationID names the durable operation row (ActionRetentionApply)
	// this apply was recorded under, pollable through the same
	// GetOperation/GET /api/v1/operations/{id} surface run_cycle already
	// is. Empty on a plan PreviewRetention returns: a preview creates no
	// operation, only an apply does.
	OperationID string

	// Retention is the policy these verdicts were decided under, and
	// RetentionIsOverride says whether that policy is this backup set's
	// own or the deployment's (issue #333).
	//
	// Both are on the plan rather than left to a second call because the
	// question a preview is being read to answer is "why is this artifact
	// about to be deleted", and that has a different answer, and a
	// different fix, depending on which policy was in force. A client
	// that fetched the attribution separately could render a chain beside
	// the wrong source: a plan is pinned to the configuration revision it
	// was computed against, and a second read is not.
	//
	// Retention is the RESOLVED policy (tiers expanded, calendar
	// inherited), which is the only form that can be shown beside a
	// verdict without the reader having to resolve it themselves.
	Retention           RetentionSettings
	RetentionIsOverride bool
}

// ApplyRetentionRequest is what a caller submits to ApplyRetentionPlan.
type ApplyRetentionRequest struct {
	// PlanID must name a plan PreviewRetention previously issued that has
	// not yet been applied, found stale, or expired.
	PlanID string

	// Source and Set name the backup set the caller believes PlanID
	// belongs to — the {source}/{set} an HTTP caller was routed by
	// (apps/common/webhost/router.go), not a hint. ApplyRetentionPlan
	// refuses, and consumes nothing, when they disagree with the backup
	// set the plan was actually issued for. plan_id alone already makes
	// the acting backup set impossible to spoof by editing a URL, so this
	// is not the defence against an attacker; it is the cheap cross-check
	// against the far likelier case of a client bug or stale component
	// state submitting the right-looking plan id for the wrong set (this
	// issue's own review, mandatory findings M3/M5).
	Source string
	Set    string

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

	// reviewedRevision fingerprints the whole plan PreviewRetention
	// actually showed the administrator: the verdicts and, since EPIC E
	// (#239), the moves section beside them (computeReviewedRevision).
	// This is the record's whole answer to "is what would run still what
	// was reviewed": ApplyRetentionPlan re-derives the plan at apply time
	// and refuses unless the fingerprint still matches, so the guarantee
	// is asserted rather than argued from the inputs it happens to have
	// fingerprinted (this issue's own review, mandatory finding M1).
	reviewedRevision string

	createdAt time.Time
	expiresAt time.Time
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

	// One instant, read once and passed down, for the same reason
	// {inner, revision} is read once: the civil date the GFS tier spans
	// are anchored on is as much an input to this plan as the journal and
	// the configuration are (internal/retention/gfs.go), so it is recorded
	// alongside them rather than left to whichever clock reading each
	// layer happens to take. See ApplyRetentionPlan's own doc.
	previewedAt := now()

	plan, err := st.inner.PrunePreviewAt(ctx, id, previewedAt)
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

	return b.newRetentionPlan(id, st.revision, previewedAt, plan), nil
}

// newRetentionPlan issues plan a fresh plan_id, records its bookkeeping
// for ApplyRetentionPlan's later staleness check, and returns the plain
// RetentionPlan shape this package hands to callers outside core/.
//
// configRevision is passed in rather than re-read from b.state here, so it
// is guaranteed to be the revision of the exact configState whose inner
// produced plan: see PreviewRetention's own doc for why re-reading would
// reopen the mismatched-pair window.
func (b *BackupService) newRetentionPlan(set model.BackupSetID, configRevision string, created time.Time, plan app.PrunePlan) RetentionPlan {
	planID := "retplan_" + uuid.New().String()
	inventoryRevision := computeInventoryRevision(plan.Records)
	expiresAt := created.Add(retentionPlanTTL)

	b.retentionMu.Lock()
	b.evictRetentionPlansLocked(created)
	b.retentionPlans[planID] = retentionPlanRecord{
		planID:            planID,
		set:               set,
		inventoryRevision: inventoryRevision,
		configRevision:    configRevision,
		reviewedRevision:  computeReviewedRevision(plan),
		createdAt:         created,
		expiresAt:         expiresAt,
	}
	b.retentionMu.Unlock()

	return summarizeRetentionPlan(set, planID, inventoryRevision, configRevision, expiresAt, "", plan)
}

// evictRetentionPlansLocked makes room for one more plan: it drops every
// record that has already expired (an expired plan is unapplyable, so it
// is pure dead weight), and then, if the store is still at its ceiling,
// the oldest remaining records until it is not. Called with retentionMu
// held, from the one place a record is ever added — see maxRetentionPlans'
// own doc for why a sweeper hanging off the existing critical section is
// the whole fix rather than a background goroutine.
func (b *BackupService) evictRetentionPlansLocked(nowT time.Time) {
	for id, rec := range b.retentionPlans {
		if nowT.After(rec.expiresAt) {
			delete(b.retentionPlans, id)
		}
	}

	for len(b.retentionPlans) >= maxRetentionPlans {
		oldestID := ""
		var oldestAt time.Time
		for id, rec := range b.retentionPlans {
			if oldestID == "" || rec.createdAt.Before(oldestAt) {
				oldestID, oldestAt = id, rec.createdAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(b.retentionPlans, oldestID)
	}
}

// ApplyRetentionPlan applies exactly the plan named by req.PlanID, or
// applies nothing at all.
//
// # What "exactly the plan" is asserted against
//
// A retention verdict is a function of three inputs, not two: this backup
// set's journal rows, this BackupService's configuration, and the instant
// the decision is taken (internal/retention/gfs.go anchors every GFS tier
// span on the civil date "now" falls in, so a plan previewed at 23:58 and
// applied at 00:01 is a genuinely different plan, with no concurrency and
// no configuration change needed). PreviewRetention records a fingerprint
// of all three: the inventory revision, the configuration revision, and —
// the one that closes the gap — a fingerprint of the verdict set the
// administrator was actually shown, moves included
// (computeReviewedRevision).
//
// This method re-derives the verdicts through PrunePreviewAt, which
// mutates nothing, and refuses with ErrRetentionPlanStale unless the
// re-derived fingerprint still equals the reviewed one. That is an
// assertion of the invariant rather than an argument from its inputs: it
// holds regardless of which input moved, including the clock, and
// including any input a future change adds that nobody thought to
// fingerprint. Only once it holds does anything call PruneApply — not
// "call it and discard its result", never call it — so zero files are
// deleted on refusal, exactly as docs/EPIC-B-multi-nas.md §15.6 and this
// issue's own Given/When/Then example require.
//
// The delete itself then runs through PruneApplySnapshot against exactly
// the records snapshot and exactly the instant that comparison was made
// over, so there is no third derivation between "what was compared" and
// "what runs". internal/retention.PruneApply's own immediately-before-
// delete safety re-check still runs against the real current disk state
// (see that function's doc), so the decision is the reviewed one while the
// safety check stays fresh.
//
// # Serialised against RunCycle
//
// A cycle writes the very journal rows the comparison above is computed
// over, so this method takes the same b.runOnce mutex SubmitRunCycle and
// the scheduler take, before it claims the plan or reads anything. An
// apply that arrives while a cycle is executing is refused with
// ErrRetentionApplyBusy and consumes nothing — see that sentinel's own doc
// for why a busy server is not reported as a stale plan.
//
// # Single-use, claimed once
//
// The plan is claimed (looked up and removed) in one critical section, so
// of two concurrent applies naming the same plan_id exactly one can ever
// own it and the other gets ErrRetentionPlanNotFound before reaching
// anything destructive. PlanID is consumed by the claim whether the call
// then succeeds, is found stale, or fails outright: a second
// ApplyRetentionPlan call for the same PlanID afterward gets
// ErrRetentionPlanNotFound, not a replay of the earlier result. A caller
// that wants to try again must call PreviewRetention again and confirm the
// new plan, exactly as docs/EPIC-B-multi-nas.md §29.3 step 6 requires ("if
// stale, abort with no deletions and require re-preview"). The two
// refusals that happen before the claim (a busy server, and a plan_id
// submitted against the wrong backup set) deliberately consume nothing.
//
// # A successful apply invalidates this backup set's other plans
//
// Deleting a file does not change the journal, so the inventory
// fingerprint of a plan previewed before the apply still matches
// afterward. Every other outstanding plan for this backup set is therefore
// dropped on success: single-use is per plan_id, and this makes it per
// effect too, so a superseded plan cannot be applied against a backup set
// whose files it no longer describes (this issue's own review, mandatory
// finding M6; the journal-side record of a pruned artifact is tracked as
// its own follow-up, since it changes internal/state's schema).
func (b *BackupService) ApplyRetentionPlan(ctx context.Context, req ApplyRetentionRequest) (RetentionPlan, error) {
	if req.PlanID == "" {
		return RetentionPlan{}, fmt.Errorf("%w: retention apply requires a plan_id", ErrInvalidRequest)
	}
	want, err := model.NewBackupSetID(req.Source, req.Set)
	if err != nil {
		return RetentionPlan{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// Before the claim, so a refusal here costs the operator nothing.
	if !b.runOnce.TryLock() {
		return RetentionPlan{}, fmt.Errorf("%w: backup set %s", ErrRetentionApplyBusy, want)
	}
	defer b.runOnce.Unlock()

	stored, err := b.claimRetentionPlan(req.PlanID, want)
	if err != nil {
		return RetentionPlan{}, err
	}

	// One instant for the whole rest of this call: the expiry check, the
	// verdicts the comparison below is made over, the verdicts the delete
	// is made over, and the operation row all read the same clock, for the
	// same reason they all read the same configState.
	nowT := now()
	if nowT.After(stored.expiresAt) {
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
	if st.revision != stored.configRevision {
		return RetentionPlan{}, fmt.Errorf("%w: backup set %s changed since plan %s was previewed", ErrRetentionPlanStale, stored.set, req.PlanID)
	}

	current, err := st.inner.PrunePreviewAt(ctx, stored.set, nowT)
	if err != nil {
		// Deliberately not %w-wrapped with err: see PreviewRetention's
		// identical reasoning above.
		return RetentionPlan{}, fmt.Errorf("service: apply retention: an internal error occurred")
	}
	if computeInventoryRevision(current.Records) != stored.inventoryRevision ||
		computeReviewedRevision(current) != stored.reviewedRevision {
		return RetentionPlan{}, fmt.Errorf("%w: backup set %s changed since plan %s was previewed", ErrRetentionPlanStale, stored.set, req.PlanID)
	}

	opID := "op_" + uuid.New().String()
	outcome, err := b.journal.CreateOperation(ctx, state.OperationRequest{
		OperationID:    opID,
		IdempotencyKey: "retention_apply_" + req.PlanID,
		Actor:          req.Actor,
		BackupSet:      stored.set.String(),
		ConfigRevision: st.revision,
		Action:         ActionRetentionApply,
		Parameters:     fmt.Sprintf(`{"plan_id":%q}`, req.PlanID),
		CreatedAt:      nowT,
	})
	if err != nil {
		// Deliberately not %w-wrapped: see PreviewRetention's identical
		// reasoning above.
		return RetentionPlan{}, fmt.Errorf("service: apply retention: an internal error occurred")
	}
	if !outcome.Created {
		// internal/state.CreateOperation's own contract: a caller may only
		// start executing an operation when Created is true. A replay here
		// means a durable row for this exact plan_id already exists, so
		// this plan's deletions have already been carried out (by an
		// earlier process, since the in-memory claim above is what stops a
		// second one inside this process) and must not run a second time.
		return RetentionPlan{}, fmt.Errorf("%w: plan %s was already applied", ErrRetentionPlanNotFound, req.PlanID)
	}
	if err := b.journal.MarkOperationRunning(context.Background(), opID, now()); err != nil {
		b.logger.Error(context.Background(), "mark-retention-apply-running", err)
	}

	applied, err := st.inner.PruneApplySnapshot(ctx, stored.set, nowT, current.Records)
	if err != nil {
		if failErr := b.journal.FailOperation(context.Background(), opID, now(), "an internal error occurred while applying retention"); failErr != nil {
			b.logger.Error(context.Background(), "fail-retention-apply", failErr)
		}
		return RetentionPlan{}, fmt.Errorf("service: apply retention: an internal error occurred")
	}

	b.invalidateRetentionPlansFor(stored.set)

	result := summarizeRetentionPlan(stored.set, req.PlanID, stored.inventoryRevision, stored.configRevision, stored.expiresAt, opID, applied)

	if err := b.journal.CompleteOperation(context.Background(), opID, now(), summarizeRetentionApply(result)); err != nil {
		b.logger.Error(context.Background(), "complete-retention-apply", err)
	}

	return result, nil
}

// claimRetentionPlan takes ownership of planID: it looks the record up and
// removes it in ONE critical section, so of two concurrent callers naming
// the same plan_id exactly one is handed the record and the other gets
// ErrRetentionPlanNotFound. Looking up and removing in two separate
// critical sections (with the destructive work in between) is the
// check-then-act shape that let both callers proceed.
//
// want is the backup set the caller says the plan belongs to: a mismatch
// refuses with ErrInvalidRequest and leaves the plan claimable, since the
// caller submitted the wrong plan for this backup set rather than a plan
// that is in any way wrong. See ApplyRetentionRequest.Source's own doc.
func (b *BackupService) claimRetentionPlan(planID string, want model.BackupSetID) (retentionPlanRecord, error) {
	b.retentionMu.Lock()
	defer b.retentionMu.Unlock()

	stored, ok := b.retentionPlans[planID]
	if !ok {
		return retentionPlanRecord{}, fmt.Errorf("%w: %s", ErrRetentionPlanNotFound, planID)
	}
	if stored.set != want {
		return retentionPlanRecord{}, fmt.Errorf("%w: plan %s was not issued for backup set %s", ErrInvalidRequest, planID, want)
	}
	delete(b.retentionPlans, planID)
	return stored, nil
}

// invalidateRetentionPlansFor drops every plan this BackupService still
// holds for set — see ApplyRetentionPlan's own "A successful apply
// invalidates this backup set's other plans" doc.
func (b *BackupService) invalidateRetentionPlansFor(set model.BackupSetID) {
	b.retentionMu.Lock()
	defer b.retentionMu.Unlock()
	for id, rec := range b.retentionPlans {
		if rec.set == set {
			delete(b.retentionPlans, id)
		}
	}
}

// computeReviewedRevision fingerprints everything a retention plan showed,
// or would show: the answer to "is what would run still exactly what was
// reviewed", independent of which input moved to change it. Deliberately
// hashes every field of every verdict (the action, the path, the medium,
// the tiers that kept it and the human reason each one carries), not just
// the artifacts selected for deletion, for the same reason
// computeInventoryRevision hashes whole records: a plan going stale too
// often is the cheap failure, and a plan staying applyable while the
// operator's reviewed reasoning no longer holds is the expensive one.
//
// # Why the moves section is in here (EPIC E FR-27, issue #239)
//
// Since this EPIC a plan is not only a list of deletions. It also says
// which artifacts are not on the medium their chain says they belong on,
// and an operator confirming a plan_id is confirming that too. Every
// input the moves section is derived from is, today, also an input to one
// of the other two revisions, so a divergence would probably be caught
// transitively. "Probably, transitively" is exactly the argument this
// function's own mandatory finding M1 rejected for the verdicts: the
// guarantee is about the reviewed OUTPUT, so the reviewed output is what
// gets hashed, and it keeps holding when a later change adds an input
// nobody thought to fingerprint.
//
// Sorted by artifact id first, so this is a fingerprint of the verdict
// SET and not of whatever order internal/retention happened to emit it in.
// The moves keep the order they were planned in, which is verdict order,
// and is therefore already a function of the same sort.
func computeReviewedRevision(plan app.PrunePlan) string {
	sorted := append([]retention.PruneVerdict(nil), plan.Verdicts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Artifact.String() < sorted[j].Artifact.String() })

	b, err := json.Marshal(struct {
		Verdicts []retention.PruneVerdict
		Home     retention.HomePlan
	}{sorted, plan.HomePlan})
	if err != nil {
		// See computeInventoryRevision's identical reasoning below:
		// PruneVerdict and HomePlan are plain data, and a Marshal failure
		// here is a programmer error to notice loudly rather than paper
		// over with a fallback revision that would silently never change.
		panic(fmt.Sprintf("service: computing reviewed revision: %v", err))
	}
	sum := sha256.Sum256(b)
	return "vdt_" + hex.EncodeToString(sum[:])[:16]
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

		tiers := make([]RetentionTierSelection, len(v.Tiers))
		for j, t := range v.Tiers {
			tiers[j] = RetentionTierSelection{Tier: string(t.Tier), SelectedBy: string(t.By)}
		}
		verdicts[i] = RetentionArtifactVerdict{
			Artifact: v.Artifact.Name,
			Action:   string(v.Action),
			Medium:   v.Medium,
			Reason:   v.Reason,
			Tiers:    tiers,
		}
	}

	// FR-27's moves, rendered the same way every other name on this
	// boundary is: the artifact's own name, not its fully-qualified id.
	// A plan is already scoped to one backup set (BackupSetID above), so
	// re-spelling the set on every row would be noise a client has to
	// strip to render a table.
	var moves []RetentionMove
	for _, m := range plan.HomePlan.Moves {
		moves = append(moves, RetentionMove{
			Artifact:   m.Artifact.Name,
			FromMedium: m.From,
			ToMedium:   m.To,
		})
	}
	var unconfirmed []string
	for _, a := range plan.HomePlan.Unconfirmed {
		unconfirmed = append(unconfirmed, a.Name)
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

		Moves:                 moves,
		UnconfirmedPlacements: unconfirmed,
		OperationID:           operationID,
		// Issue #333: taken from the plan the decision was actually made
		// on, not re-read from the running config here. A hot reload
		// between the two would attribute these verdicts to a policy that
		// did not decide them.
		Retention:           toRetentionSettings(plan.Retention),
		RetentionIsOverride: plan.RetentionIsOverride,
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
