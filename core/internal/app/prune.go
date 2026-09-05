package app

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// FR-20: the file where this package actually deletes a backup.
//
// retention.go computes classification and touches nothing. This is the
// other side of that split, and everything unusual about the shapes here
// comes from one requirement: an administrator confirms a plan on a screen,
// and some seconds or minutes later something applies it. What must not
// happen in between is the applied plan quietly becoming a different plan.
//
// That is why the instant and the record snapshot are parameters rather than
// things this code reads for itself. GFS tiers are anchored on a civil date,
// so the same journal and the same configuration produce a different and
// entirely correct verdict set either side of a day boundary, and two
// derivations that each read their own clock cannot be compared at all.
// core/service pins one instant and one snapshot and passes both to the
// preview and to the apply, which is what makes "is this still the plan you
// confirmed" a question with an answer. PrunePlan.Records is carried for the
// same reason and only for that caller.
//
// Pinning the decision does not pin the safety check. internal/retention's
// PruneApply re-runs containment, symlink and last-known-good checks against
// the real disk immediately before every delete, so the reviewed thing is the
// decision and the fresh thing is the evidence.
//
// The plan_id, the TTL and the staleness comparison all live in core/service
// and are deliberately invisible here, the same way RunCycle knows nothing
// about an Idempotency-Key header.

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

	// Retention is the resolved policy these verdicts were decided under,
	// whichever of the two it came from. RetentionIsOverride says WHERE
	// it came from; this says WHAT it says, and a preview surface needs
	// both to answer "why is this artifact being deleted": the source
	// tells an operator where to go and edit, the chain tells them what
	// they will find when they get there.
	//
	// It is carried on the plan rather than left to the caller to look up
	// from the config, for the same reason Records is: a second lookup is
	// a second observation, and this one would be a second observation of
	// a configuration that a hot reload can replace between the two.
	Retention config.Retention

	// HomePlan is where these artifacts BELONG, as opposed to whether they
	// are kept (EPIC E FR-27, issue #239). It is the same plan
	// RetentionSetReport carries, on the plan the preview/apply envelope
	// is built over, because a preview has to show every MOVE as well as
	// every deletion before anything runs, and core/service's staleness
	// fingerprint has to cover the moves section or a plan whose moves
	// changed would stay applyable against reasoning nobody reviewed.
	//
	// It is derived from the same verdicts and the same records the
	// verdicts were decided from, in one pass, for the reason
	// RetentionSetReport gives: a second pass would decide against a chain
	// a hot reload could have replaced in between.
	HomePlan retention.HomePlan
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
	verdicts, err := retention.PruneDecide(at, bs.Retention, bs, records, ActiveMediumFromRecords(records))
	if err != nil {
		return PrunePlan{}, fmt.Errorf("app: prune preview: %s: %w", set, err)
	}
	homePlan, err := s.homePlanFor(at, bs, records)
	if err != nil {
		return PrunePlan{}, fmt.Errorf("app: prune preview: %s: %w", set, err)
	}
	return PrunePlan{Set: set, Verdicts: verdicts, Records: records, RetentionIsOverride: bs.RetentionIsOverride(), Retention: bs.Retention, HomePlan: homePlan}, nil
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
	verdicts, err := retention.PruneApply(ctx, at, bs.Retention, bs, records, ActiveMediumFromRecords(records), s.mediumPruner())
	if err != nil {
		return PrunePlan{}, fmt.Errorf("app: prune apply: %s: %w", set, err)
	}
	homePlan, err := s.homePlanFor(at, bs, records)
	if err != nil {
		return PrunePlan{}, fmt.Errorf("app: prune apply: %s: %w", set, err)
	}
	s.recordRetentionRun(set)
	return PrunePlan{Set: set, Verdicts: verdicts, Records: records, RetentionIsOverride: bs.RetentionIsOverride(), Retention: bs.Retention, HomePlan: homePlan}, nil
}

// homePlanFor is FR-27's home-medium pass over one backup set, computed
// from the same records and the same chain the verdicts beside it were,
// for the reason RetentionPreview states: a second pass could decide under
// a chain a hot reload replaced in between, and the moves would then
// describe a policy that did not produce the verdicts they travel with.
//
// The instant is passed rather than read, because a home is derived from a
// verdict and a verdict is anchored on a civil date: reading a second
// clock here could put an artifact on one side of a day boundary and its
// KEEP/DELETE decision on the other.
func (s *Service) homePlanFor(at time.Time, bs config.BackupSet, records []state.Record) (retention.HomePlan, error) {
	verdicts, _, err := retention.DecideKeep(at, bs.Retention, bs.ID, records)
	if err != nil {
		return retention.HomePlan{}, err
	}
	return retention.PlanHomeMoves(bs.Retention.EffectiveTiers(), verdicts, ActiveMediumFromRecords(records))
}

// mediumPruner is what FR-20's prune deletes an object through, or nil
// when this deployment has no medium to reach.
//
// Nil is the fail-safe: internal/retention turns a nil MediumPruner into a
// REFUSE rather than a pass, so a deployment with no medium store wired up
// cannot delete an object, and a medium-free deployment never reaches the
// branch at all.
func (s *Service) mediumPruner() retention.MediumPruner {
	if s.MediumStore == nil || len(s.Config.StorageMediums) == 0 {
		return nil
	}
	return &placement.Reclaimer{
		Store:   s.MediumStore,
		Mediums: MediumResolver(s.Config.StorageMediums),
		Now:     s.now,
	}
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
