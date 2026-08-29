package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/internal/state"
)

// ArtifactFilter narrows ListArtifacts to a subset of configured backup
// sets. An empty Source matches every source; an empty Set matches every
// backup set within whatever sources match Source. This mirrors config's
// own source-then-set nesting (FR-7: identity is source-plus-set, never
// set alone), so a Set filter with no Source is deliberately not a
// shortcut across sources: two different sources may each have a set
// with the same name.
type ArtifactFilter struct {
	Source string
	Set    string
}

func (f ArtifactFilter) matches(sourceName, setName string) bool {
	if f.Source != "" && f.Source != sourceName {
		return false
	}
	if f.Set != "" && f.Set != setName {
		return false
	}
	return true
}

// ListArtifacts is `backup-manager artifacts`' use case: every journal
// record for every backup set filter selects, in config order (source
// order, then backup-set order within each source), which is the same
// deterministic order Sources() renders in.
func (s *Service) ListArtifacts(ctx context.Context, filter ArtifactFilter) ([]state.Record, error) {
	var out []state.Record
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if !filter.matches(src.Name, bs.Name) {
				continue
			}
			records, err := s.Journal.ListByBackupSet(ctx, bs.ID)
			if err != nil {
				return out, fmt.Errorf("app: artifacts: listing %s: %w", bs.ID, err)
			}
			out = append(out, records...)
		}
	}
	return out, nil
}
