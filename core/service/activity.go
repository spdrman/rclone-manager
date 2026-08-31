package service

import (
	"context"
	"fmt"
	"time"
)

// DefaultActivityLimit is how many events ListActivity returns when a
// caller does not ask for a number. It bounds a read of an append-only
// table that is never pruned, so "no limit" can never mean "the whole
// deployment's history".
const DefaultActivityLimit = 200

// MaxActivityLimit caps what a caller may ask for. A limit above this is
// clamped rather than refused: the caller asked for a feed, and a feed
// that is 200 entries instead of 10,000 is still the thing they asked for.
const MaxActivityLimit = 1000

// ActivityEvent is one durable, already-recorded lifecycle moment: an
// artifact moved from one state to another, when, and what the writer said
// about it.
//
// This is a read of internal/state's append-only state_transitions log,
// not a second event stream. FR-23's event catalog (internal/obs) writes
// the same moments to the process log, but a log line is not queryable
// after the fact, so it cannot be what an operator's activity list is
// built from.
//
// Deliberately NOT modelled here: a severity, a headline and a
// presentation category. Those are the shared Web UI's own vocabulary
// (ui/shared/src/types/operation.ts), derived at the API boundary from
// From/To, and putting them in core/ would make a display decision a
// durable domain concept.
type ActivityEvent struct {
	// ArtifactID is "source/set/name".
	ArtifactID string
	// BackupSetID is "source/set".
	BackupSetID  string
	SourceName   string
	SetName      string
	ArtifactName string

	// From is the state the artifact left, empty for the very first
	// transition (an artifact being discovered leaves nothing).
	From string
	// To is the state it entered. Never empty.
	To string

	OccurredAt time.Time
	// Detail is whatever the writer recorded about this move: a
	// quarantine reason, a validator's verdict, an operator's action.
	// Frequently empty for an ordinary pipeline step.
	Detail string
}

// ListActivity returns the most recent lifecycle transitions across every
// backup set, newest first.
//
// A limit of zero or less means DefaultActivityLimit; anything above
// MaxActivityLimit is clamped to it.
func (b *BackupService) ListActivity(ctx context.Context, limit int) ([]ActivityEvent, error) {
	if limit <= 0 {
		limit = DefaultActivityLimit
	}
	if limit > MaxActivityLimit {
		limit = MaxActivityLimit
	}

	records, err := b.journal.RecentActivity(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("service: listing activity: %w", err)
	}

	out := make([]ActivityEvent, 0, len(records))
	for _, rec := range records {
		out = append(out, ActivityEvent{
			ArtifactID:   rec.Artifact.String(),
			BackupSetID:  rec.Artifact.Set.String(),
			SourceName:   rec.Artifact.Set.Source,
			SetName:      rec.Artifact.Set.Set,
			ArtifactName: rec.Artifact.Name,
			From:         rec.From,
			To:           rec.To,
			OccurredAt:   rec.OccurredAt,
			Detail:       rec.Detail,
		})
	}
	return out, nil
}
