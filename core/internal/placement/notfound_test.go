package placement_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file tests one rule, and it tests it because the rule was written
// down three times and checked zero times.
//
// transport.MediumStore's own doc says a key the medium does not hold is a
// NotFound-classified error and never a zero ObjectInfo, "because a mover
// that confuses them deletes a local copy on the strength of a network
// failure". errSourceAlreadyGone's doc says the same thing again. There is
// then an inline comment inside proveMediumSourceSafe saying it a third
// time, in the body of the branch that implements it.
//
// The branch it guards is two lines apart from the one that refuses:
//
//	if category == transport.NotFound { return errSourceAlreadyGone }
//	if err != nil                    { return refuse(...) }
//
// Collapsing those into one is a one-character edit, and nothing in this
// repository noticed when it was made. That is what a rule with three
// comments and no test is worth.
//
// # Which direction this is, and why
//
// proveMediumSourceSafe runs when the SOURCE of a move is on a medium,
// which is the move back to local disk. So every case here first runs the
// outward move the ordinary way, leaving the artifact on the medium, and
// then asks the engine to bring it home while StatObject answers in one of
// four ways.
//
// The two answers that matter are "the medium answered and the object is
// not there", which is convergence and finishes the move, and everything
// else, which is a fact about the endpoint and finishes nothing. Getting
// that backwards writes GONE over a placement whose object is still on the
// medium, which leaves an orphan nobody will ever look at again, billed
// monthly, and a journal that is simply wrong about where the artifact is.

func TestTheSourceProofTellsAnAbsentObjectFromAnUnreachableMedium(t *testing.T) {
	for _, tc := range []struct {
		name string
		stat error

		// wantPhase is where the reverse move ends up.
		wantPhase placement.Phase
		// wantSourceStatus is what the medium placement is left at.
		wantSourceStatus string
		// wantDeletes is how many objects the engine removed from the
		// medium.
		wantDeletes int
		// wantRefusal is a phrase the engine's own reason must contain,
		// empty when there is nothing to refuse.
		wantRefusal string
	}{
		{
			// The positive control, and this table is worth nothing
			// without it. A medium that answers normally has to let the
			// move finish and has to actually delete the object, or every
			// other row below is satisfied by an engine that does nothing
			// at all.
			name:             "the medium answers and the object is there",
			stat:             nil,
			wantPhase:        placement.Done,
			wantSourceStatus: state.PlacementGone,
			wantDeletes:      1,
		},
		{
			// Convergence. A crash between the delete landing at the
			// endpoint and the DONE write leaves exactly this world, and a
			// guard that refuses here leaves the row stuck for ever with a
			// DELETE_PENDING placement about an object that is not there.
			name:             "the medium answers and the object is not there",
			stat:             &transport.Error{Category: transport.NotFound, Op: "stat", Cause: errors.New("no such key")},
			wantPhase:        placement.Done,
			wantSourceStatus: state.PlacementGone,
			wantDeletes:      0,
		},
		{
			// The one the rule is about. The endpoint said nothing about
			// the object; it said something about itself.
			name:             "the medium could not be reached to ask",
			stat:             &transport.Error{Category: transport.Transient, Op: "stat", Cause: errors.New("connection reset by peer")},
			wantPhase:        placement.SourceDeletePending,
			wantSourceStatus: state.PlacementDeletePending,
			wantDeletes:      0,
			wantRefusal:      "could not be asked about",
		},
		{
			// The row a subtler mutation slips through. "Not NotFound" and
			// "classified as something else" are not the same set: an
			// error the classifier never placed has category
			// Unclassified, and a guard written as `category != NotFound
			// || category == Unclassified` would wave this one through
			// while passing the row above.
			name:             "the medium failed in a way nothing classified",
			stat:             errors.New("stat: the backend returned something nobody has categorised"),
			wantPhase:        placement.SourceDeletePending,
			wantSourceStatus: state.PlacementDeletePending,
			wantDeletes:      0,
			wantRefusal:      "could not be asked about",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})

			// Put the artifact on the medium the ordinary way. After this
			// the medium copy is the source and local disk is empty, which
			// is the world proveMediumSourceSafe runs in.
			f.runCycle()
			f.guard.fail()
			if f.localExists() {
				t.Fatal("the outward move left the local copy, so the reverse move has no medium source to prove")
			}
			if !f.medium.has(f.key) {
				t.Fatal("the outward move left nothing on the medium")
			}
			deletesBefore := f.medium.deleteCount()

			f.medium.statErr = tc.stat
			report, err := f.engine.RunCycle(f.ctx, []placement.Plan{{Artifact: f.artifact, DestinationMedium: state.MediumLocal}})
			if err != nil {
				t.Fatalf("RunCycle: %v", err)
			}
			f.guard.fail()

			// The proof has to have RUN. A row that passes because the
			// engine never got as far as asking is a row that proves
			// nothing about the answer it gives.
			if f.medium.stats == 0 {
				t.Fatal("the engine never called StatObject, so this case never reached the proof it is about")
			}

			mv := latestMove(t, f)
			if placement.Phase(mv.Phase) != tc.wantPhase {
				t.Errorf("the reverse move ended at %s, want %s (reason %q; outcomes %+v)",
					mv.Phase, tc.wantPhase, mv.Error, report.Outcomes)
			}
			src, ok := f.placement(testMedium)
			if !ok {
				t.Fatal("the medium placement disappeared entirely")
			}
			if src.Status != tc.wantSourceStatus {
				t.Errorf("the medium placement is %s, want %s", src.Status, tc.wantSourceStatus)
			}
			if got := f.medium.deleteCount() - deletesBefore; got != tc.wantDeletes {
				t.Errorf("the engine removed %d objects from the medium, want %d", got, tc.wantDeletes)
			}

			var said []string
			for _, o := range report.Outcomes {
				if o.Refused != "" {
					said = append(said, o.Refused)
				}
			}
			joined := strings.Join(append(said, mv.Error), "\n")
			if tc.wantRefusal != "" && !strings.Contains(joined, tc.wantRefusal) {
				t.Errorf("the engine said:\n%s\nwant a refusal containing %q", joined, tc.wantRefusal)
			}

			// Whatever happened, the artifact still has bytes somewhere
			// that somebody can read. The reverse move copies to local
			// before it deletes from the medium, so the local file is
			// there in every row.
			if _, err := os.Lstat(f.localPath()); err != nil {
				t.Fatalf("THE ARTIFACT HAS NO LOCAL COPY after a reverse move: %v", err)
			}
		})
	}
}

// TestAnUnreachableMediumNeverRecordsItsSourceAsGone is the same finding
// stated as the thing that must not happen, without a phase in it.
//
// The four rows above pin four different worlds. This pins the one
// sentence that has to be true in all of them and that the collapsed
// branch makes false: a placement is only ever recorded GONE because
// something proved the object is not there, never because nothing could be
// proved either way.
func TestAnUnreachableMediumNeverRecordsItsSourceAsGone(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.runCycle()
	f.guard.fail()

	f.medium.statErr = &transport.Error{Category: transport.Transient, Op: "stat", Cause: errors.New("i/o timeout")}
	// The first cycle plans the move home; the two after it resume the
	// move the first one left in flight, which is what a daemon does while
	// a medium stays unreachable.
	plans := []placement.Plan{{Artifact: f.artifact, DestinationMedium: state.MediumLocal}}
	for i := 0; i < 3; i++ {
		if _, err := f.engine.RunCycle(f.ctx, plans); err != nil {
			t.Fatalf("RunCycle: %v", err)
		}
		plans = nil
	}
	f.guard.fail()

	if f.medium.stats == 0 {
		t.Fatal("the engine never called StatObject, so nothing here reached the proof this test is about")
	}
	if mv := latestMove(t, f); placement.Phase(mv.Phase) != placement.SourceDeletePending {
		t.Fatalf("the move is at %s, want %s; if it never got that far this test is checking nothing",
			mv.Phase, placement.SourceDeletePending)
	}

	src, ok := f.placement(testMedium)
	if !ok {
		t.Fatal("the medium placement disappeared entirely")
	}
	if src.Status == state.PlacementGone {
		t.Error("a medium that never answered had its copy recorded GONE; the object is still there and nothing will ever look for it again")
	}
	if !f.medium.has(f.key) {
		t.Error("the object was deleted from a medium that could not be asked whether it held it")
	}
}

// latestMove is the most recently planned move for the fixture's artifact.
// These tests run two, the outward one that puts the artifact on the
// medium and the reverse one under test, so onlyMove cannot be used.
func latestMove(t *testing.T, f *fixture) state.Move {
	t.Helper()
	moves := f.moves()
	if len(moves) == 0 {
		t.Fatal("no move was recorded at all")
	}
	latest := moves[0]
	for _, m := range moves {
		if m.ID > latest.ID {
			latest = m
		}
	}
	return latest
}
