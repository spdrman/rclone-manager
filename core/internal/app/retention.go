package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
)

// RetentionSetReport is one backup set's FR-18/FR-19 classification: every
// managed, completed artifact's GFS verdict, unioned with FR-19's
// last-known-good protection, exactly as internal/retention.DecideKeep
// computes it.
type RetentionSetReport struct {
	Set           model.BackupSetID
	Verdicts      []retention.GFSVerdict
	LastKnownGood retention.LastKnownGoodResult

	// RetentionIsOverride reports whether these verdicts were decided
	// under this set's own retention policy rather than the deployment's
	// (issue #333). "Why is this artifact being deleted" has a different
	// answer depending on which one was in force, and an operator reading
	// a preview cannot tell the two apart from the verdicts alone: a set
	// override and a global policy that happen to agree produce identical
	// output.
	RetentionIsOverride bool

	// HomePlan is where these artifacts BELONG, as opposed to whether
	// they are kept (EPIC E FR-27, issue #239). The chain's first
	// selecting tier names each kept artifact's home medium, and this
	// reports the ones that are not on it, plus the ones whose current
	// placement could not be confirmed.
	//
	// It travels on the same report as the verdicts because it is derived
	// from them: a second pass would decide against a chain that a hot
	// reload could have replaced in between, and the moves would then
	// describe a policy that did not produce the verdicts beside them.
	//
	// Nothing executes it here. Planning and moving are separate on
	// purpose, and the mover is #238's.
	HomePlan retention.HomePlan

	// Retention is the fully-resolved policy these verdicts were decided
	// under, whichever of the two it came from. RetentionIsOverride says
	// WHETHER the set decides for itself; this says WHAT it decided with,
	// which is the half an operator reading "why is this being deleted"
	// actually needs. Carrying the policy rather than a rendered sentence
	// keeps the rendering with the caller that has to fit it on a line.
	Retention config.Retention
}

// RetentionPreview computes set's current KEEP/DELETE classification: the
// FR-18 daily/weekly/monthly union with FR-19's last-known-good protection,
// via internal/retention.DecideKeep, against a freshly-loaded snapshot of
// the journal.
//
// # A preview of classification, not of deletion safety
//
// This method reports GFS ∪ last-known-good only: the same DecideKeep this
// package's own doc says it deliberately never went further than. FR-20,
// "Local Deletion Safety" (the actual, positively-identified, symlink-
// and-traversal-safe local file removal FR-18/FR-19's verdicts would
// drive) landed in internal/retention/prune.go under issue #21, and this
// package's own PrunePreview/PruneApply (prune.go, issue #96/B3.1) are
// what actually call into it — see that file's own doc for why it exists
// as a sibling to this method rather than a replacement for it:
// RetentionPreview's classification-only report is still what
// cmd/backup-manager's `retention`/`retention --dry-run` commands render
// today (see that command's own note on the CLI still not calling
// PruneApply for a real, non-dry-run invocation, a separate, narrower gap
// than this doc comment's own past staleness was).
//
// # A set the journal remembers but config no longer names
//
// Since issue #333 this method looks the set's configuration up, because
// the policy it decides under lives on the set. That means it now refuses
// a set id the journal still carries but config no longer names, where it
// used to produce a report from the global policy. That is deliberate,
// and it is the same answer pruneInputsFor already gives for the same
// question: there is no longer a single deployment-wide policy such a set
// could be said to be retained under, so reporting one would be inventing
// the answer rather than finding it. Neither of this method's own callers
// can reach it (RetentionPreviewAll and the processing cycle both iterate
// configured sets), so nothing that worked before stops working; a caller
// holding an id off a journal row gets a *NotFoundError instead of a
// plausible report.
func (s *Service) RetentionPreview(ctx context.Context, set model.BackupSetID) (RetentionSetReport, error) {
	records, err := s.Journal.ListByBackupSet(ctx, set)
	if err != nil {
		return RetentionSetReport{}, fmt.Errorf("app: retention: listing %s: %w", set, err)
	}
	// Issue #333: retain under this set's own resolved policy. Validate
	// fills that in from the set's override when it declares one and from
	// the global policy otherwise, so this one read covers both and the
	// global policy is never consulted directly here.
	_, bs, ok := s.backupSetConfigFor(set)
	if !ok {
		return RetentionSetReport{}, &NotFoundError{Kind: "backup set", Name: set.String()}
	}
	verdicts, lkg, err := retention.DecideKeep(s.now(), bs.Retention, set, records)
	if err != nil {
		return RetentionSetReport{}, fmt.Errorf("app: retention: %s: %w", set, err)
	}
	// FR-27's home-medium pass, over the verdicts that were just decided
	// and the placements the same records carry. Refusing here rather
	// than reporting a partial plan is deliberate: HomeMedium only ever
	// fails on a verdict naming a tier the chain does not contain, which
	// is a disagreement between two things this function just computed
	// together, and a preview that dropped the moves and kept the
	// verdicts would look like a backup set with nothing to move.
	homePlan, err := retention.PlanHomeMoves(bs.Retention.EffectiveTiers(), verdicts, ActiveMediumFromRecords(records))
	if err != nil {
		return RetentionSetReport{}, fmt.Errorf("app: retention: %s: %w", set, err)
	}

	s.recordRetentionRun(set)
	return RetentionSetReport{
		Set:                 set,
		Verdicts:            verdicts,
		LastKnownGood:       lkg,
		HomePlan:            homePlan,
		RetentionIsOverride: bs.RetentionIsOverride(),
		Retention:           bs.Retention,
	}, nil
}

// RetentionPreviewAll computes RetentionPreview for every configured backup
// set, in config order. A per-set error is returned immediately (unlike
// the processing cycle's own per-backup-set error isolation): retention
// classification has no partial-progress concept worth continuing past,
// and an operator running `backup-manager retention` wants to know
// immediately if any one backup set's classification could not be
// computed, not have it silently missing from the printed report.
func (s *Service) RetentionPreviewAll(ctx context.Context) ([]RetentionSetReport, error) {
	var out []RetentionSetReport
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			rep, err := s.RetentionPreview(ctx, bs.ID)
			if err != nil {
				return out, err
			}
			out = append(out, rep)
		}
	}
	return out, nil
}
