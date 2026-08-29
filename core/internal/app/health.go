package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/health"
)

// BuildHealthReport is `backup-manager status`' use case (FR-24). It calls
// internal/health.ComputeBackupSetHealth once per configured backup set,
// against that set's freshly-loaded journal rows, and bundles the result
// with the process-liveness half of FR-24 built from versionInfo.
//
// # Two of the injected inputs are always empty for a one-shot command
//
// internal/health.BackupSetInputs asks for LastSuccessfulPollAt and
// LastRetentionRunAt, and this package does track both, in memory, for
// backup sets a *running* Service has itself already cycled through (see
// app.go's recordSuccessfulPoll/recordRetentionRun, called from cycle.go).
// A `status` invocation is normally its own short-lived process, though,
// entirely separate from whatever `run` or `daemon` process last actually
// touched a backup set, and neither timestamp is persisted anywhere in the
// FR-9 journal schema. So in the common case, a freshly-constructed
// Service reports both as nil here, honestly reflecting "unknown from this
// process's own history" rather than fabricating a value.
//
// See this package's introducing PR description for the follow-up this
// implies: persisting a last-poll and last-retention-run timestamp
// somewhere durable (a small dedicated table, or per-backup-set columns)
// is a real gap this package cannot close on its own, since it would mean
// changing internal/state's schema, which is out of this package's file
// scope.
//
// FreeBytes, by contrast, never depends on process history: it is a live
// capacity.StatPath reading against the backup set's configured LocalPath,
// taken fresh on every call, exactly the way FR-24 names "free space" as
// something to be reported, not remembered.
func (s *Service) BuildHealthReport(ctx context.Context, versionInfo VersionInfo) (health.Report, error) {
	now := s.now()
	process := health.NewProcessHealth(health.ProcessInputs{
		BinaryVersion: versionInfo.BinaryVersion,
		RcloneVersion: versionInfo.RcloneVersion,
	})

	var sets []health.BackupSetHealth
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			records, err := s.Journal.ListByBackupSet(ctx, bs.ID)
			if err != nil {
				return health.Report{}, fmt.Errorf("app: health: listing %s: %w", bs.ID, err)
			}

			in := health.BackupSetInputs{
				LastSuccessfulPollAt: s.lastPollAt(bs.ID),
				LastRetentionRunAt:   s.lastRetentionAt(bs.ID),
			}
			if stat, statErr := capacity.StatPath(bs.LocalPath); statErr == nil {
				free := stat.AvailableBytes
				in.FreeBytes = &free
			}

			sets = append(sets, health.ComputeBackupSetHealth(bs.ID, records, bs.StaleAfter.Duration(), in, now))
		}
	}
	return health.NewReport(process, sets, now), nil
}
