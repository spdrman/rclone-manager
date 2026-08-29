package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Journal is the slice of internal/state that lifecycle steps need.
//
// It is an interface rather than a concrete *state.Journal so a step can be
// tested against a fake without standing up SQLite, and so a step cannot reach
// past this surface into migrations or schema concerns.
type Journal interface {
	Get(ctx context.Context, id model.ArtifactID) (state.Record, error)
	RecordTransition(ctx context.Context, t state.Transition) (state.Outcome, error)
}

// Deps is what every lifecycle step is handed. Steps take this rather than
// reaching for globals, so a test can substitute any part of it.
type Deps struct {
	Journal   Journal
	Transport transport.Transport

	// Now is injectable because several steps stamp times that tests need to
	// control. Nil means time.Now.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now().UTC()
}

// Advance is the only way a lifecycle step should move an artifact.
//
// WHY THIS EXISTS. The journal and the state machine each enforce half of the
// rule and neither enforces the other's half. RecordTransition checks that the
// state name is one the schema knows and that From matches what is on disk, but
// it holds no opinion on whether the move is legal, because the transition
// table lives here. Validate knows the table but writes nothing, so on its own
// it protects only the code paths that remember to call it.
//
// Split like that, the guarantee that nothing reaches REMOTE_DELETE_PENDING
// without passing through COMMITTED is a unit-test fact rather than a runtime
// one. Joining them here makes it a runtime fact: an illegal move is refused
// before the journal is touched at all, so a step that forgets the rule cannot
// write its way past it.
func Advance(ctx context.Context, d Deps, t state.Transition) (state.Outcome, error) {
	if d.Journal == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: Advance needs a Journal")
	}

	// From == "" means "create the row", which is Discover's job and has no
	// predecessor to validate against.
	if t.From != "" {
		if err := Validate(State(t.From), State(t.To)); err != nil {
			return state.Outcome{}, fmt.Errorf("lifecycle: refusing to record %s to %s: %w", t.From, t.To, err)
		}
	} else if !State(t.To).Valid() {
		return state.Outcome{}, fmt.Errorf("lifecycle: refusing to create an artifact in unknown state %q", t.To)
	}

	if t.OccurredAt.IsZero() {
		t.OccurredAt = d.now()
	}
	return d.Journal.RecordTransition(ctx, t)
}
