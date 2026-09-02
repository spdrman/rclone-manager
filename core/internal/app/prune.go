package app

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// PrunePlan is one backup set's current FR-20 KEEP/DELETE/REFUSE verdict
// set (internal/retention.PruneDecide's/PruneApply's own output), plus the
// exact journal snapshot (Records) that verdict set was computed against.
//
// Records exists for one reason: issue #96 (B3.1)'s preview/apply envelope,
// built on top of this package in core/service, needs to fingerprint
// "which journal rows was this plan computed from" so a later apply call
// can detect the inventory changing out from under a previously-issued
// plan_id before ever calling PruneApply again (see core/service's
// retention.go). This package has no such envelope concept itself, and
// nothing in this package's own callers (cmd/backup-manager's `retention`
// command) needs Records at all — it is carried here purely so
// core/service does not have to re-query the journal a second time, with
// its own small risk of observing a different snapshot than the one the
// verdicts actually reflect, just to compute that fingerprint.
type PrunePlan struct {
	Set      model.BackupSetID
	Verdicts []retention.PruneVerdict
	Records  []state.Record

	// RetentionIsOverride reports whether this plan was computed under the
	// set's own retention policy rather than the deployment's (issue
	// #333), for the same reason RetentionSetReport carries it: the
	// verdicts alone cannot distinguish an override from a global policy
	// that agrees with it, and the answer changes what an operator should
	// go and edit.
	RetentionIsOverride bool
}

// PrunePreview computes set's current FR-20 KEEP/DELETE/REFUSE verdicts via
// internal/retention.PruneDecide, against a freshly-loaded snapshot of the
// journal. Like PruneDecide itself, this performs no mutation whatsoever.
//
// # A sibling to RetentionPreview, not a replacement for it
//
// RetentionPreview (retention.go) reports FR-18/FR-19 classification only
// (KEEP vs "not kept by GFS"); this method (and PruneApply below) is what
// actually wires this package to FR-20's positively-identified,
// symlink-and-traversal-safe local deletion, via internal/retention/
// prune.go's PruneDecide/PruneApply (issue #21). cmd/backup-manager's
// `retention` command still only calls RetentionPreviewAll today, not this
// method — see that command's own note for the CLI-side gap this leaves,
// tracked separately from this issue (#96/B3.1), which only needed an
// API-facing preview/apply, not a CLI one.
func (s *Service) PrunePreview(ctx context.Context, set model.BackupSetID) (PrunePlan, error) {
	return s.PrunePreviewAt(ctx, set, s.now())
}

// PrunePreviewAt is PrunePreview against a caller-supplied instant rather
// than this Service's own clock.
//
// The instant is an input to the verdict set, not an implementation
// detail: internal/retention/gfs.go anchors every GFS tier span on the
// civil date `at` falls in, so the same journal snapshot and the same
// configuration produce a different, entirely correct verdict set either
// side of a civil-day boundary (or a DST transition). core/service's
// preview/apply envelope has to decide "was the plan the administrator
// confirmed still the plan that would run" across a window of up to its
// own plan TTL, and it cannot answer that if each of its two derivations
// silently reads a different clock — so it pins one instant and passes it
// to both this method and PruneApplySnapshot below. See core/service/
// retention.go's ApplyRetentionPlan for the comparison that pinning makes
// possible.
func (s *Service) PrunePreviewAt(ctx context.Context, set model.BackupSetID, at time.Time) (PrunePlan, error) {
	records, bs, err := s.pruneInputsFor(ctx, set)
	if err != nil {
		return PrunePlan{}, err
	}
	// Issue #333: the set's own resolved policy, which Validate filled
	// in from its override when it declares one and from the global
	// policy otherwise. Reading s.Config.Retention here instead would
	// silently ignore every per-set override.
	verdicts, err := retention.PruneDecide(at, bs.Retention, bs, records)
	if err != nil {
		return PrunePlan{}, fmt.Errorf("app: prune preview: %s: %w", set, err)
	}
	return PrunePlan{Set: set, Verdicts: verdicts, Records: records, RetentionIsOverride: bs.RetentionIsOverride()}, nil
}

// PruneApply computes set's current FR-20 verdicts and deletes the local
// file behind every PruneDelete result, via internal/retention.PruneApply.
//
// This method has no plan_id, revision-comparison or staleness concept of
// its own, by design: PruneDecide/PruneApply are re-derived from whatever
// the journal says right now, on every single call, which is exactly what
// core/service's retention envelope (the actual owner of "is the plan a
// caller confirmed still the plan that would run") needs underneath it.
// See core/service/retention.go's own doc for where that comparison and
// the whole immutable-plan-id contract actually lives; this package
// deliberately stays as unaware of an HTTP-facing plan_id as
// SubmitRunCycle's own internal/app.Service.RunCycle call is of an
// Idempotency-Key header.
func (s *Service) PruneApply(ctx context.Context, set model.BackupSetID) (PrunePlan, error) {
	records, _, err := s.pruneInputsFor(ctx, set)
	if err != nil {
		return PrunePlan{}, err
	}
	return s.PruneApplySnapshot(ctx, set, s.now(), records)
}

// PruneApplySnapshot deletes the local file behind every PruneDelete
// verdict derived from exactly the records snapshot and the instant the
// caller hands over, instead of re-reading the journal and re-reading the
// clock for itself.
//
// This is what lets core/service apply the verdict set it actually
// compared against the one the administrator confirmed, rather than a
// second set derived from a journal snapshot and a clock reading nobody
// ever reviewed. internal/retention.PruneApply's own immediately-before-
// delete safety re-check (path containment, symlinks, last-known-good,
// against the real current disk state) still runs on every artifact, so
// pinning the decision does not pin the safety check to stale evidence:
// the decision is the reviewed one, the disk check is fresh.
func (s *Service) PruneApplySnapshot(ctx context.Context, set model.BackupSetID, at time.Time, records []state.Record) (PrunePlan, error) {
	_, bs, ok := s.backupSetConfigFor(set)
	if !ok {
		return PrunePlan{}, &NotFoundError{Kind: "backup set", Name: set.String()}
	}
	verdicts, err := retention.PruneApply(at, bs.Retention, bs, records)
	if err != nil {
		return PrunePlan{}, fmt.Errorf("app: prune apply: %s: %w", set, err)
	}
	s.recordRetentionRun(set)
	return PrunePlan{Set: set, Verdicts: verdicts, Records: records, RetentionIsOverride: bs.RetentionIsOverride()}, nil
}

// pruneInputsFor loads the two things PruneDecide/PruneApply both need for
// set: a freshly-loaded snapshot of its journal rows, and its configured
// config.BackupSet (for FR-20's local-root containment check). Returns
// *NotFoundError if set names no backup set in this Service's own loaded
// configuration — the journal may still remember artifacts from a backup
// set an operator has since removed, but FR-20 has nothing to check
// containment against for one, so preview/apply refuse rather than guess.
func (s *Service) pruneInputsFor(ctx context.Context, set model.BackupSetID) ([]state.Record, config.BackupSet, error) {
	records, err := s.Journal.ListByBackupSet(ctx, set)
	if err != nil {
		return nil, config.BackupSet{}, fmt.Errorf("app: prune: listing %s: %w", set, err)
	}
	_, bs, ok := s.backupSetConfigFor(set)
	if !ok {
		return nil, config.BackupSet{}, &NotFoundError{Kind: "backup set", Name: set.String()}
	}
	return records, bs, nil
}
