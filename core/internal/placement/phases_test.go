package placement_test

import (
	"time"

	"context"
	"fmt"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file turns phases.go's prose into properties of the graph.
//
// The safety argument over there is written as a paragraph and a diagram:
// the source is deleted only after VERIFIED is durably recorded, VERIFIED
// is the disposability boundary so nothing is abandoned after it, and a
// destination that fails at the last moment goes back to COPYING with the
// source intact. A paragraph does not fail when somebody adds a row to the
// table, and a diagram in a comment fails even less. So each of those
// sentences is asserted below as a fact about the edges: which phases
// precede SOURCE_DELETE_PENDING, which have an ABANDONED edge, that every
// phase is both reachable and escapable, and that every pair NOT in the
// table is refused.
//
// The last test is the #372 guard rather than a phase rule: a phase can be
// declared, be well-formed, and still have no case in the engine's driver,
// which is how rows sit in a state nothing looks at again. It walks the
// list this package derives rather than one written out here, so a phase
// added with no way out fails a test instead of stalling a move.

// TestThePhaseVocabularyIsExactlyTheSchemas is the drift guard between
// this package, which owns what a phase means, and 0007_placements.sql,
// whose CHECK constraint decides what can be stored. A phase added here
// without a migration cannot be written; a value the schema admits with no
// phase here cannot be produced.
func TestThePhaseVocabularyIsExactlyTheSchemas(t *testing.T) {
	stored := map[string]bool{
		state.MovePlanned:             true,
		state.MoveCopying:             true,
		state.MoveCopied:              true,
		state.MoveVerifying:           true,
		state.MoveVerified:            true,
		state.MoveSourceDeletePending: true,
		state.MoveDone:                true,
		state.MoveAbandoned:           true,
	}
	known := map[string]bool{}
	for _, p := range placement.Phases {
		known[string(p)] = true
	}
	for name := range stored {
		if !known[name] {
			t.Errorf("internal/state stores phase %q but this package has no Phase for it", name)
		}
	}
	for name := range known {
		if !stored[name] {
			t.Errorf("this package has a phase %q internal/state cannot store; 0007_placements.sql's CHECK would refuse it", name)
		}
	}
	if len(placement.Phases) != 8 {
		t.Errorf("the machine has %d phases, want 8", len(placement.Phases))
	}
}

// TestThePhaseTableIsWellFormed walks the declared edges and refuses the
// three shapes that make a table lie: an unknown phase on either end, a
// duplicate edge, and a self-edge (ValidatePhaseChange handles from == to
// as an idempotent no-op, and putting eight of those in the table would
// drown the six edges that carry the safety argument).
func TestThePhaseTableIsWellFormed(t *testing.T) {
	seen := map[placement.PhaseTransition]bool{}
	for _, tr := range placement.PhaseTransitions {
		if !placement.ValidPhase(tr.From) {
			t.Errorf("edge %s -> %s has an unknown From", tr.From, tr.To)
		}
		if !placement.ValidPhase(tr.To) {
			t.Errorf("edge %s -> %s has an unknown To", tr.From, tr.To)
		}
		if tr.From == tr.To {
			t.Errorf("edge %s -> %s is a self-edge and does not belong in the table", tr.From, tr.To)
		}
		if seen[tr] {
			t.Errorf("edge %s -> %s is declared twice", tr.From, tr.To)
		}
		seen[tr] = true
	}
	for tr := range seen {
		if err := placement.ValidatePhaseChange(tr.From, tr.To); err != nil {
			t.Errorf("the table declares %s -> %s and ValidatePhaseChange refuses it: %v", tr.From, tr.To, err)
		}
	}
}

// TestOnlyVerifiedPrecedesSourceDeletePending is the ordering rule, read
// straight off the table. It is FR-30's transposition of
// TestOnlyCommittedPrecedesRemoteDeletePending, and it is what makes "the
// source copy is deleted only after VERIFIED is durably recorded" a
// property of the graph rather than of the functions that walk it.
func TestOnlyVerifiedPrecedesSourceDeletePending(t *testing.T) {
	var predecessors []placement.Phase
	for _, tr := range placement.PhaseTransitions {
		if tr.To == placement.SourceDeletePending {
			predecessors = append(predecessors, tr.From)
		}
	}
	if len(predecessors) != 1 || predecessors[0] != placement.Verified {
		t.Fatalf("SOURCE_DELETE_PENDING's predecessors are %v; only %s may precede it", predecessors, placement.Verified)
	}

	// And every other phase must be refused as a predecessor, checked
	// against every phase this machine knows rather than a hand-written
	// list that could go stale.
	for _, p := range placement.Phases {
		if p == placement.Verified || p == placement.SourceDeletePending {
			continue
		}
		if err := placement.ValidatePhaseChange(p, placement.SourceDeletePending); err == nil {
			t.Errorf("%s -> SOURCE_DELETE_PENDING is allowed, which would let a source delete be reached without VERIFIED", p)
		}
	}
}

// TestNothingIsAbandonedAfterVerified pins the disposability boundary.
//
// ABANDONED means "the destination copy was disposable and has been
// disposed of, and the source was never touched". That is true up to and
// including VERIFYING and stops being true at VERIFIED, because the write
// that records VERIFIED is the write that gives the destination its
// placements row. An ABANDONED edge out of VERIFIED or later would be a
// path that deletes verified data to tidy up.
func TestNothingIsAbandonedAfterVerified(t *testing.T) {
	for _, from := range []placement.Phase{placement.Verified, placement.SourceDeletePending, placement.Done} {
		if err := placement.ValidatePhaseChange(from, placement.Abandoned); err == nil {
			t.Errorf("%s -> ABANDONED is allowed; after VERIFIED the destination is no longer the disposable copy", from)
		}
	}
	for _, from := range []placement.Phase{placement.Planned, placement.Copying, placement.Copied, placement.Verifying} {
		if err := placement.ValidatePhaseChange(from, placement.Abandoned); err != nil {
			t.Errorf("%s -> ABANDONED is refused, and the destination is still disposable there: %v", from, err)
		}
	}
}

// TestNoPhaseIsADeadEndAndNoneIsUnreachable walks the graph. A
// non-terminal phase with no exit is a move nothing can finish, which is
// exactly #372's shape one level down; a phase nothing reaches is a phase
// that cannot be tested.
func TestNoPhaseIsADeadEndAndNoneIsUnreachable(t *testing.T) {
	out := map[placement.Phase]int{}
	in := map[placement.Phase]int{}
	for _, tr := range placement.PhaseTransitions {
		out[tr.From]++
		in[tr.To]++
	}
	for _, p := range placement.Phases {
		if !placement.IsTerminal(p) && out[p] == 0 {
			t.Errorf("%s is not terminal and has no exit, so a move that reaches it can never finish", p)
		}
		if p != placement.Planned && in[p] == 0 {
			t.Errorf("%s is unreachable: nothing in the table leads to it", p)
		}
	}
	if !placement.IsTerminal(placement.Done) || !placement.IsTerminal(placement.Abandoned) {
		t.Error("DONE and ABANDONED must both be terminal")
	}
	if out[placement.Done]+out[placement.Abandoned] != 0 {
		t.Error("a terminal phase has an exit")
	}
}

// TestValidatePhaseChangeRefusesEveryUndeclaredPair is the completeness
// half: every ordered pair the table does not declare is refused, and
// from == to is the one documented exception.
func TestValidatePhaseChangeRefusesEveryUndeclaredPair(t *testing.T) {
	declared := map[placement.PhaseTransition]bool{}
	for _, tr := range placement.PhaseTransitions {
		declared[tr] = true
	}
	for _, from := range placement.Phases {
		for _, to := range placement.Phases {
			err := placement.ValidatePhaseChange(from, to)
			switch {
			case from == to:
				if err != nil {
					t.Errorf("%s -> %s is the idempotent no-op and must be allowed: %v", from, to, err)
				}
			case declared[placement.PhaseTransition{From: from, To: to}]:
				if err != nil {
					t.Errorf("%s -> %s is declared and was refused: %v", from, to, err)
				}
			default:
				if err == nil {
					t.Errorf("%s -> %s is not declared and was allowed", from, to)
				}
			}
		}
	}
	if err := placement.ValidatePhaseChange("MADE_UP", placement.Copying); err == nil {
		t.Error("an unknown phase was accepted as a From")
	}
}

// TestEveryNonTerminalPhaseHasAResumeCase is the direct answer to #372.
//
// #372 is open because COMMITTING was added to a state machine and
// processArtifact was never given a case for it, so a row left there by a
// crash sat forever and nothing looked at it again. The bug was invisible
// because the crash suite drove its own harness's switch, which did handle
// it.
//
// This test drives the REAL engine, once per non-terminal phase, against a
// move planted in that phase in a real journal, and fails if the engine
// has no case for it. It does not care whether the move succeeds; a phase
// the engine cannot even attempt is the failure, and a phase it attempts
// and refuses for a real reason is a different, reported thing.
func TestEveryNonTerminalPhaseHasAResumeCase(t *testing.T) {
	phases := placement.NonTerminalPhases()
	if len(phases) != 6 {
		t.Fatalf("expected six non-terminal phases, got %d (%v)", len(phases), phases)
	}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			plantMoveAt(t, f, phase)

			report, err := f.engine.RunCycle(f.ctx, nil)
			if err != nil {
				t.Fatalf("RunCycle: %v", err)
			}
			if report.Resumed != 1 {
				t.Fatalf("the engine did not pick up the move planted at %s: %+v", phase, report)
			}
			for _, o := range report.Outcomes {
				if containsNoCase(o.Refused) {
					t.Fatalf("resuming from %s stalled because the engine has no case for some phase it reached, which is exactly #372's shape: %s", phase, o.Refused)
				}
			}
			after := f.onlyMove()
			if !placement.IsTerminal(placement.Phase(after.Phase)) {
				t.Fatalf("a move resumed from %s ended at %s, which is not terminal, so nothing will finish it", phase, after.Phase)
			}
		})
	}
}

func containsNoCase(s string) bool {
	return s != "" && len(s) > 0 && (contains(s, "has no case for"))
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}()
}

// plantMoveAt puts a real move row in a real journal at the given phase,
// with the world in the state a crash at that phase boundary would have
// left it: the object present from COPIED onward, the destination
// placement present from VERIFIED onward, and the source marked
// DELETE_PENDING at SOURCE_DELETE_PENDING.
func plantMoveAt(t *testing.T, f *fixture, target placement.Phase) state.Move {
	t.Helper()
	ctx := context.Background()

	mv, err := f.journal.PlanMove(ctx, state.MovePlan{
		Artifact: f.artifact, SourceMedium: state.MediumLocal,
		DestinationMedium: testMedium, DestinationKey: f.key,
		OccurredAt: f.clock,
	})
	if err != nil {
		t.Fatalf("planting the move: %v", err)
	}
	if target == placement.Planned {
		return mv
	}

	advance := func(from, to string, placements ...state.PlacementUpdate) {
		t.Helper()
		f.clock = f.clock.Add(time.Second)
		mv, err = f.journal.AdvanceMove(ctx, state.MoveAdvance{
			MoveID: mv.ID, From: from, To: to, OccurredAt: f.clock, Placements: placements,
		})
		if err != nil {
			t.Fatalf("planting %s -> %s: %v", from, to, err)
		}
	}

	advance(state.MovePlanned, state.MoveCopying)
	if target == placement.Copying {
		return mv
	}

	// From COPIED onward the object really is on the medium, because that
	// is what a crash after the upload leaves behind.
	if _, err := f.medium.UploadFromLocal(ctx, transport.Medium{ID: testMedium}, f.localPath(), f.key, transport.UploadOptions{}); err != nil {
		t.Fatalf("planting the destination object: %v", err)
	}
	size := int64(len(f.content))
	f.clock = f.clock.Add(time.Second)
	mv, err = f.journal.AdvanceMove(ctx, state.MoveAdvance{
		MoveID: mv.ID, From: state.MoveCopying, To: state.MoveCopied,
		OccurredAt: f.clock, BytesCopied: &size,
	})
	if err != nil {
		t.Fatalf("planting COPYING -> COPIED: %v", err)
	}
	if target == placement.Copied {
		return mv
	}

	advance(state.MoveCopied, state.MoveVerifying)
	if target == placement.Verifying {
		return mv
	}

	verified := f.clock
	dst := state.PlacementUpdate{
		Medium: testMedium, Location: f.key, Size: &size,
		Hash: f.hash, HashAlg: "sha256",
		VerificationClass: state.VerificationContent, VerifiedAt: &verified,
		Status: state.PlacementActive,
	}
	advance(state.MoveVerifying, state.MoveVerified, dst)
	if target == placement.Verified {
		return mv
	}

	src, ok := f.placement(state.MediumLocal)
	if !ok {
		t.Fatal("the seeded artifact has no local placement to mark DELETE_PENDING")
	}
	advance(state.MoveVerified, state.MoveSourceDeletePending, src.Update().WithStatus(state.PlacementDeletePending))
	if target == placement.SourceDeletePending {
		return mv
	}

	t.Fatalf("plantMoveAt does not know how to reach %s", target)
	return state.Move{}
}

var _ = fmt.Sprintf
