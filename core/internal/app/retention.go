package app

import (
	"context"
	"fmt"

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
func (s *Service) RetentionPreview(ctx context.Context, set model.BackupSetID) (RetentionSetReport, error) {
	records, err := s.Journal.ListByBackupSet(ctx, set)
	if err != nil {
		return RetentionSetReport{}, fmt.Errorf("app: retention: listing %s: %w", set, err)
	}
	verdicts, lkg, err := retention.DecideKeep(s.now(), s.Config.Retention, set, records)
	if err != nil {
		return RetentionSetReport{}, fmt.Errorf("app: retention: %s: %w", set, err)
	}
	s.recordRetentionRun(set)
	return RetentionSetReport{Set: set, Verdicts: verdicts, LastKnownGood: lkg}, nil
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
