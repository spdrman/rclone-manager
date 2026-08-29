package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/internal/model"
	"github.com/spdrman/rclone-manager/internal/retention"
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
// # This is a preview, on purpose, for both `retention` and `retention --dry-run`
//
// FR-20, "Local Deletion Safety" (the actual, positively-identified,
// symlink-and-traversal-safe local file removal FR-18/FR-19's verdicts
// would drive), is issue #21, open and being worked at the time this
// package was written. internal/retention itself contains no delete
// function at all yet: GFSDecide, LastKnownGoodDecide and DecideKeep only
// ever classify, they never touch a filesystem. So RetentionPreview is,
// today, the whole of what `backup-manager retention` and `backup-manager
// retention --dry-run` can honestly do: report what GFS ∪ last-known-good
// would keep or not keep, identically under either flag, with no actual
// deletion happening in either case. cmd/backup-manager's retention command
// says this explicitly rather than let the absence of `--dry-run` imply a
// destructive action this package does not, and must not, perform on its
// own (see this package's introducing PR description for the precise
// wiring FR-20 will need once issue #21 lands).
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
