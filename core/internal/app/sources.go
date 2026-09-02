package app

import (
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// SourceSummary is `backup-manager sources`' one line of business logic: a
// read-only, presentation-ready view of one configured source and its
// backup sets. It carries nothing config.Source/config.BackupSet don't
// already have; it exists so cmd/backup-manager never reaches into
// internal/config's types directly, keeping the CLI thin and this
// package's shape the one both a future HTTP handler and the CLI render
// from.
type SourceSummary struct {
	Name       string
	BackupSets []BackupSetSummary
}

// BackupSetSummary is one backup set's configured shape, exactly as
// `sources` needs to print it.
type BackupSetSummary struct {
	ID         model.BackupSetID
	RemoteType string
	RemotePath string
	LocalPath  string
	StaleAfter time.Duration
	Disabled   bool
	// ReadOnly is issue #282's resolved answer (config.BackupSet.ReadOnly)
	// for whether this set's remote source may ever be deleted.
	ReadOnly bool
}

// Sources lists every configured source and its backup sets. This never
// touches the journal or a remote: it is a pure read of Config, already
// validated by the time a Service exists.
func (s *Service) Sources() []SourceSummary {
	out := make([]SourceSummary, 0, len(s.Config.Sources))
	for _, src := range s.Config.Sources {
		sum := SourceSummary{Name: src.Name}
		for _, bs := range src.BackupSets {
			sum.BackupSets = append(sum.BackupSets, BackupSetSummary{
				ID:         bs.ID,
				RemoteType: bs.Remote.Type,
				RemotePath: bs.RemotePath,
				LocalPath:  bs.LocalPath,
				StaleAfter: bs.StaleAfter.Duration(),
				Disabled:   bs.Disabled,
				ReadOnly:   bs.ReadOnly,
			})
		}
		out = append(out, sum)
	}
	return out
}
