package movecrash_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// TestCrashMatrix is the matrix. Each cell kills a real process at one
// real boundary, then restarts the real engine against what it left
// behind. See this package's doc for what every cell asserts and why the
// invariant check is not a sample.
func TestCrashMatrix(t *testing.T) {
	for _, cell := range []struct {
		// name is the boundary, in the words of the design written into
		// issue #238.
		name string
		// kill is the harness flag that produces it.
		kill []string
		// proves is what this cell is here for, and it is printed on
		// failure so a red cell says what was lost, not only which flag
		// was passed.
		proves string
		// want is the phase the restart must converge to.
		want placement.Phase
		// sourceSurvives says whether the local copy must still be there
		// after the restart.
		sourceSurvives bool
		// setup runs before the crash.
		setup func(t *testing.T, w *world)
		// corruptOnRestart makes the restarting process's own store keep
		// something other than what it was handed, so a bad destination
		// stays bad across the restart.
		corruptOnRestart bool
	}{
		{
			name:   "C1 after PLANNED is durably recorded",
			kill:   []string{"-plan", "-kill-after-plan"},
			proves: "recorded intent on its own deletes nothing, and a plan alone converges",
			want:   placement.Done,
		},
		{
			name:   "C2 after COPYING is durably recorded, before the copy",
			kill:   []string{"-plan", "-kill-after-phase=COPYING"},
			proves: "a COPYING row with no object at the key converges instead of stalling",
			want:   placement.Done,
		},
		{
			name:   "C3 the instant the copy returns, before anything is journaled",
			kill:   []string{"-plan", "-kill-after-copy"},
			proves: "an object that exists with no journal entry naming it is found by the move row and reused, not duplicated",
			want:   placement.Done,
		},
		{
			name: "C3b a COPYING row with a half-written object at the key",
			kill: []string{"-plan", "-kill-after-phase=COPYING"},
			setup: func(t *testing.T, w *world) {
				w.plantPartialObject(t, "half a")
			},
			proves: "the deterministic key means a resumed copy overwrites the interrupted object rather than leaving a second one",
			want:   placement.Done,
		},
		{
			name:   "C4 after COPIED is durably recorded",
			kill:   []string{"-plan", "-kill-after-phase=COPIED"},
			proves: "an unverified destination has no placement row, so nothing can rely on it, and the restart verifies before it deletes",
			want:   placement.Done,
		},
		{
			name:   "C5 after VERIFYING is durably recorded, mid-verification",
			kill:   []string{"-plan", "-kill-after-phase=VERIFYING"},
			proves: "verification is redone from scratch and never inferred from the phase",
			want:   placement.Done,
		},
		{
			name:   "C6 after VERIFIED is durably recorded",
			kill:   []string{"-plan", "-kill-after-phase=VERIFIED"},
			proves: "the write that authorises a delete is still not trusted on its own: the restart re-verifies before it removes anything",
			want:   placement.Done,
		},
		{
			name:   "C7 after SOURCE_DELETE_PENDING is durably recorded, before the delete",
			kill:   []string{"-plan", "-kill-after-phase=SOURCE_DELETE_PENDING"},
			proves: "intent is recorded before the delete, so a crash in that window leaves a journal that says what was about to happen",
			want:   placement.Done,
		},
		{
			name:   "C8 the instant the source is removed, before DONE is journaled",
			kill:   []string{"-plan", "-kill-after-source-delete"},
			proves: "the last window converges: the delete is idempotent and the row reaches DONE on the next cycle",
			want:   placement.Done,
		},
		{
			name:   "C9 a destination that kept the wrong bytes, crashed at COPIED",
			kill:   []string{"-plan", "-corrupt-after-copy", "-kill-after-phase=COPIED"},
			proves: "a destination the journal never verified is never what the artifact ends up relying on: the restart re-verifies, throws the bad object away and copies again, and the surviving copy really hashes to the recorded SHA-256",
			want:   placement.Done,
		},
		{
			name:             "C10 a destination that is persistently wrong, crashed at COPIED",
			kill:             []string{"-plan", "-corrupt-after-copy", "-kill-after-phase=COPIED"},
			corruptOnRestart: true,
			proves:           "the destination is the disposable copy: an endpoint that is always wrong ends with the move abandoned and the source still on disk",
			want:             placement.Abandoned,
			sourceSurvives:   true,
		},
	} {
		t.Run(cell.name, func(t *testing.T) {
			w := newWorld(t)
			if cell.setup != nil {
				cell.setup(t, w)
			}

			res := w.crash(cell.kill...)
			if !res.killed() {
				t.Fatalf("the harness was not killed by SIGKILL (err=%v)\nstdout:\n%s\nstderr:\n%s",
					res.err, res.stdout, res.stderr)
			}
			if v := res.violations(); len(v) > 0 {
				t.Fatalf("the crashed process reported invariant violations: %v", v)
			}

			// 1. The invariant held at the instant of the crash, judged
			// from the journal the dead process left on disk.
			j := w.journalNow()
			w.checkInvariantNow(j, "at the instant of the crash ("+cell.name+")")

			// 2. The restart converges, driven by the same RunCycle the
			// product uses, told nothing about where the move was.
			engine, guard := w.restartEngine(j, cell.corruptOnRestart)
			report, err := engine.RunCycle(w.ctx, nil)
			if err != nil {
				t.Fatalf("the restart failed: %v", err)
			}
			guard.fail()
			if report.Resumed != 1 {
				t.Fatalf("the restart did not pick the move up: %+v\nthis cell proves: %s", report, cell.proves)
			}

			// 3. The bytes are real.
			rec := w.assertConverged(j, cell.want)

			if cell.sourceSurvives && !w.localExists() {
				t.Fatalf("THE SOURCE WAS DELETED. This cell proves: %s", cell.proves)
			}
			if !cell.sourceSurvives {
				if w.localExists() {
					t.Errorf("the source copy is still on disk after a completed move")
				}
				if !hasActive(rec, crashMedium) {
					t.Errorf("no ACTIVE placement records the destination copy after a completed move")
				}
			}
			if n := w.objectCount(t); n > 1 {
				t.Errorf("the medium holds %d objects; a resumed copy must converge on one key", n)
			}
		})
	}
}

func hasActive(rec state.Record, medium string) bool {
	for _, p := range rec.Placements {
		if p.Medium == medium && p.Status == state.PlacementActive {
			return true
		}
	}
	return false
}

// TestTheHarnessKillAssertionIsNotVacuous proves the "the harness was
// killed by SIGKILL" check every cell rests on can actually fail. Without
// it, a harness that silently stopped self-destructing would turn the
// whole matrix into a suite of ordinary, uninterrupted runs, and every
// cell would still be green.
func TestTheHarnessKillAssertionIsNotVacuous(t *testing.T) {
	w := newWorld(t)
	res := w.crash("-plan", "-kill-after-phase=VERIFIED", "-suppress-kill")
	if res.killed() {
		t.Fatal("the harness died even with -suppress-kill set, so the flag does not do what the matrix relies on")
	}
	if !strings.Contains(res.stderr, "MOVECRASH_SELF_KILL_SUPPRESSED") {
		t.Fatalf("the harness never reached the kill point, so this test proved nothing:\n%s", res.stderr)
	}
	if !strings.Contains(res.stdout, "FINISHED") {
		t.Fatalf("the suppressed run did not finish:\n%s\n%s", res.stdout, res.stderr)
	}
}

// TestTheCrashHarnessHasNoPhaseMachineOfItsOwn is the guard that keeps
// this suite from repeating #372.
//
// #372 is open because the lifecycle crash matrix proves convergence about
// its own harness: crashmatrix/harness carries a second state machine that
// handles COMMITTING, and processArtifact, the code an operator actually
// runs, does not. The suite proves lifecycle.Commit resumes; nothing
// proves the pipeline gives it the chance.
//
// A harness that cannot name a phase cannot drive one. This test parses
// the harness's own source and fails if any phase name appears in it, or
// if it reaches for the engine by any route other than the one entry point
// the product uses. Run against crashmatrix/harness, its equivalent would
// have failed on day one.
func TestTheCrashHarnessHasNoPhaseMachineOfItsOwn(t *testing.T) {
	const path = "harness/main.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the harness source: %v", err)
	}
	text := string(src)

	// The phase names, taken from the machine rather than written out
	// here, so a phase added later is covered without anyone remembering.
	for _, p := range placement.Phases {
		if strings.Contains(text, string(p)) {
			t.Errorf("the harness source names the phase %q; a harness that can spell a phase can drive one, and then the suite is testing the harness (see #372)", p)
		}
	}
	for _, ident := range []string{
		"placement.Planned", "placement.Copying", "placement.Copied", "placement.Verifying",
		"placement.Verified", "placement.SourceDeletePending", "placement.Done", "placement.Abandoned",
		"placement.PhaseTransitions", "placement.NonTerminalPhases", "placement.IsTerminal",
	} {
		if strings.Contains(text, ident) {
			t.Errorf("the harness reaches for %s; it must only be able to start a cycle, not reason about phases", ident)
		}
	}

	// And it must reach the engine through exactly one method. A second
	// entry point is how a harness ends up with a driver of its own even
	// without naming a phase.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parsing the harness source: %v", err)
	}
	calls := map[string]int{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "engine" {
			calls[sel.Sel.Name]++
		}
		return true
	})
	if len(calls) != 1 || calls["RunCycle"] == 0 {
		t.Errorf("the harness calls %v on the engine; it must call RunCycle and nothing else", calls)
	}
}
