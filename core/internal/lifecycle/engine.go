package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is the one door between a lifecycle step and the journal.
//
// It is three declarations and almost no code, and the point of all three is
// to make a rule mechanical that would otherwise only be a convention. The
// interface narrows what a step can reach for, the Deps struct makes every
// step take its collaborators as arguments so a test can replace them, and
// Advance joins the two halves of the transition rule that live in different
// packages so neither can be bypassed.
//
// Advance's doc has the long argument. The short version is that this is the
// difference between "nothing reaches REMOTE_DELETE_PENDING without passing
// through COMMITTED" being something the tests believe and something the
// running process enforces.

// Journal is the slice of internal/state that lifecycle steps need.
//
// It is an interface rather than a concrete *state.Journal so a step can be
// tested against a fake without standing up SQLite, and so a step cannot reach
// past this surface into migrations or schema concerns.
type Journal interface {
	Get(ctx context.Context, id model.ArtifactID) (state.Record, error)
	RecordTransition(ctx context.Context, t state.Transition) (state.Outcome, error)

	// LastEnteredAt reports when an artifact most recently entered a state
	// from a different one, ignoring same-state writes, and whether it ever
	// did. remotedelete.go's WP3.2 stable-completion gate needs it: the
	// only other timestamp on hand, state.Record.UpdatedAt, is advanced by
	// every transition write there is, including the routine same-state
	// ones that would otherwise keep restarting that gate's safety clock.
	// ReinstateFromQuarantine needs it too, to prove an artifact really did
	// hold the state it is being returned to.
	LastEnteredAt(ctx context.Context, id model.ArtifactID, st string) (time.Time, bool, error)

	// LastTransition reports when an artifact most recently recorded one
	// exact from -> to edge, and whether it ever did. remotedelete.go's
	// issue #220 gate needs the narrower question LastEnteredAt cannot
	// answer: not "when did this become COMMITTED" but "did it become
	// COMMITTED by being reinstated out of quarantine", which is the one
	// fact that permanently forfeits its remote delete and which nothing
	// on the artifacts row records.
	LastTransition(ctx context.Context, id model.ArtifactID, from, to string) (time.Time, bool, error)
}

// Deps is what every lifecycle step is handed. Steps take this rather than
// reaching for globals, so a test can substitute any part of it.
type Deps struct {
	// Journal is required by every step: this package's entire output is
	// journal transitions.
	Journal Journal

	// Transport is only needed by the steps that touch the remote, which is
	// Transfer, Verify's remote hash lookup and DeleteRemote. The rest
	// leave it nil, and a step that needs it says so by checking rather
	// than by panicking, so a caller that forgot gets a sentence instead of
	// a stack trace.
	Transport transport.Transport

	// Now is injectable because several steps stamp times that tests need to
	// control. Nil means time.Now.
	Now func() time.Time
}

// now resolves the clock, normalising to UTC on both branches.
//
// The UTC applies to the injected function too, and that is the part worth
// stating. Every timestamp this package writes goes into the journal and is
// later compared against another one, by the deletion-safety gate and by
// revalidation's due-ness check among others. A test clock returning a time
// in a named location would still compare correctly, but the values stored
// would not match what production writes, and a fixture whose stored form
// differs from production's is how a comparison bug survives its own test.
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
