package movecrash_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The matrix itself, and the three checks that keep it from certifying its
// own harness.
//
// Each cell kills a real process at a real boundary and restarts the real
// engine against whatever the dead process left behind. That shape has a
// specific way of going quietly wrong: the kill can stop landing where the
// cell says it does, or the harness can grow a phase machine of its own and
// start proving things about itself instead of about the engine. So the
// matrix travels with a control that fails if the kill assertion is vacuous,
// a check that the harness has no phase logic of its own, and, for the
// staged medium-to-medium cells, a check that they really do stage
// something.
//
// Two of those controls read the harness source with go/ast rather than
// running it. That is deliberate: the property is "this code does not exist
// here", and an absence cannot be demonstrated by execution.

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

// TestCrashMatrixOverAStagedMediumToMediumHop is the same discipline
// applied to the hop #429 added: an artifact whose only copy is on one
// medium, moving to another, through a staging copy on the backup set's
// own disk.
//
// # Why this needed its own table rather than a flag on the one above
//
// The phase machine is unchanged. A staged hop is the same six phases; the
// staging happens INSIDE the copy phase, which is what keeps the eleven
// cells above covering the same boundaries they always did. That argument
// is sound, and it is exactly the kind of sound argument that turns out to
// have an exception nobody looked for, so the boundaries are re-run over
// the staged leg rather than reasoned about, and the two crash points that
// are genuinely new get cells of their own:
//
//   - part-way through reading the source down onto local disk, and
//   - the instant that local file is complete, before anything is
//     uploaded or journaled about it.
//
// # What the two new points are actually testing
//
// Convergence after a crash rests on two things: the staging path is
// deterministic, so a resumed hop targets the same file rather than
// leaving one per attempt, and what is already at that path is CHECKED
// rather than trusted. Those two cells are what say which of them is
// carrying the weight at each point.
//
// artifactstore.Local.Put writes through a temporary file and links it
// into place, so the two points leave genuinely different things behind: a
// kill during the read leaves a temp file and NOTHING at the staging name,
// and a kill after the write leaves a complete, correct file at it. The
// first has to be downloaded again; the second must not be, because that
// egress has already been paid for. Both are asserted, and the second is
// asserted by counting reads of the source rather than by looking at the
// outcome, because a build that re-downloaded would converge just as well
// and cost twice as much.
//
// The bad-leftover half of the same rule (a file at the staging name that
// is NOT the artifact) is deliberately not claimed here as a crash
// outcome, because this engine cannot produce one: Put is atomic, so a
// crash leaves the file complete or absent. It is still checked, against a
// planted one, in internal/placement's own suite. Saying that out loud is
// the point: a cell that claimed to reach it by crashing would be
// describing a state the crash cannot make.
func TestCrashMatrixOverAStagedMediumToMediumHop(t *testing.T) {
	for _, cell := range []struct {
		name   string
		kill   []string
		proves string
		want   placement.Phase
		// sourceSurvivesOnFirstMedium says the artifact must still be on
		// the medium it started on after the restart, which is what an
		// abandoned hop means here.
		sourceSurvivesOnFirstMedium bool
		corruptOnRestart            bool
		// noSourceRead says the restart must not have read the source
		// object at all, because a staging copy it could use was already
		// on disk.
		noSourceRead bool
		// afterCrash inspects what the dead process actually left, before
		// the restart tidies it away. It is how a cell pins WHERE it
		// died: "the process was killed and the engine recovered" is
		// true of a kill at a harmless moment too, and a crash cell that
		// cannot tell those apart is not testing the boundary it names.
		afterCrash func(t *testing.T, w *world)
	}{
		{
			name:   "S1 part-way through reading the source down onto local disk",
			kill:   []string{"-plan", "-kill-during-stage"},
			proves: "an interrupted read leaves no file at the staging name, so the resumed hop downloads again and converges rather than uploading a fragment",
			want:   placement.Done,
			// The kill landed in the middle of the download and nowhere
			// else: bytes have started arriving, nothing is at the
			// staging name because the local store links only a
			// finished file into place, and nothing has been uploaded.
			afterCrash: func(t *testing.T, w *world) {
				left := w.stagingLeftovers()
				if len(left) != 1 {
					t.Fatalf("the staging area holds %v; an interrupted download leaves exactly one temporary file", left)
				}
				if left[0] == crashArtifact {
					t.Fatalf("the staging area holds the artifact under its own name, so the download had finished and this cell did not interrupt one")
				}
				if n := w.objectCountOn(t, w.secondBucket); n != 0 {
					t.Fatalf("%q holds %d object(s) after a crash during the download, so the kill landed after the upload", crashSecondMedium, n)
				}
			},
		},
		{
			name:         "S2 the instant the staging copy is complete, before the upload",
			kill:         []string{"-plan", "-kill-after-stage"},
			proves:       "a staging copy a dead process finished is the artifact, and is re-used rather than downloaded again: the egress was already paid for",
			want:         placement.Done,
			noSourceRead: true,
		},
		{
			name:   "S3 the instant the upload returns, before anything is journaled",
			kill:   []string{"-plan", "-kill-after-copy"},
			proves: "an object on the second medium with no journal row naming it, and a staging copy nothing has cleared, both converge and the staging area ends empty",
			want:   placement.Done,
		},
		{
			name:   "S4 after the copy phase is durably recorded, before anything is staged",
			kill:   []string{"-plan", "-kill-after-phase=COPYING"},
			proves: "a copy-phase row with nothing staged and nothing uploaded converges instead of stalling",
			want:   placement.Done,
		},
		{
			name:   "S5 after the verified write, before the source object is deleted",
			kill:   []string{"-plan", "-kill-after-phase=VERIFIED"},
			proves: "the write that authorises a delete is still not trusted on its own, and the copy the restart re-verifies is on a medium rather than on disk",
			want:   placement.Done,
		},
		{
			name:   "S6 the instant the source object is removed, before the move is finished",
			kill:   []string{"-plan", "-kill-after-object-delete"},
			proves: "the last window converges when the source is an OBJECT: the delete is idempotent and the row reaches a terminal phase on the next cycle",
			want:   placement.Done,
		},
		{
			name:                        "S7 a second medium that is persistently wrong",
			kill:                        []string{"-plan", "-corrupt-after-copy", "-kill-after-phase=COPIED"},
			corruptOnRestart:            true,
			proves:                      "the destination is still the disposable copy when the source is on a medium: the hop abandons, the source object survives, and no staging copy is left holding disk",
			want:                        placement.Abandoned,
			sourceSurvivesOnFirstMedium: true,
		},
	} {
		t.Run(cell.name, func(t *testing.T) {
			w := newWorld(t)

			// The artifact reaches the first medium through a real,
			// uninterrupted move, so the hop this cell crashes starts
			// from a placement the product wrote.
			w.firstHop()

			res := w.crashTo(crashSecondMedium, cell.kill...)
			if !res.killed() {
				t.Fatalf("the harness was not killed by SIGKILL (err=%v)\nstdout:\n%s\nstderr:\n%s",
					res.err, res.stdout, res.stderr)
			}
			if v := res.violations(); len(v) > 0 {
				t.Fatalf("the crashed process reported invariant violations: %v", v)
			}
			if cell.afterCrash != nil {
				cell.afterCrash(t, w)
			}

			// 1. The invariant held at the instant of the crash, judged
			// from the journal the dead process left on disk. A staged hop
			// has three copies of the bytes in the world at once and the
			// invariant rests on the SOURCE for all of it, so this is the
			// check that says the staging copy never had to carry it.
			j := w.journalNow()
			w.checkInvariantNow(j, "at the instant of the crash ("+cell.name+")")

			// 2. The restart converges, driven by the same RunCycle the
			// product uses, told nothing about where the hop was.
			engine, guard := w.restartEngine(j, cell.corruptOnRestart)
			report, err := engine.RunCycle(w.ctx, nil)
			if err != nil {
				t.Fatalf("the restart failed: %v", err)
			}
			guard.fail()
			if report.Resumed != 1 {
				t.Fatalf("the restart did not pick the hop up: %+v\nthis cell proves: %s", report, cell.proves)
			}

			// 3. The bytes are real, wherever the journal says they are.
			rec := w.assertStagedConverged(j, cell.want)

			// 4. The staging area is empty. This is the claim the whole
			// table is here for: a staged hop that leaves its copy behind
			// costs one artifact-sized file per crash on the very disk
			// the next hop's size check is about, and a temp file left by
			// an interrupted write counts.
			if left := w.stagingLeftovers(); len(left) != 0 {
				t.Errorf("the staging area holds %v after the restart\nthis cell proves: %s", left, cell.proves)
			}

			if cell.noSourceRead {
				if n := w.store.opensOf(crashMedium, w.key); n != 0 {
					t.Errorf("the restart read the source object %d time(s) over a staging copy that was already complete on disk\nthis cell proves: %s",
						n, cell.proves)
				}
			}

			if cell.sourceSurvivesOnFirstMedium {
				if !hasActive(rec, crashMedium) {
					t.Fatalf("THE SOURCE COPY IS GONE from %q after an abandoned hop. This cell proves: %s", crashMedium, cell.proves)
				}
				if n := w.objectCountOn(t, w.bucket); n != 1 {
					t.Errorf("%q holds %d object(s) after an abandoned hop, want the source and nothing else", crashMedium, n)
				}
				return
			}

			if !hasActive(rec, crashSecondMedium) {
				t.Errorf("no ACTIVE placement records the copy on %q after a completed hop", crashSecondMedium)
			}
			if hasActive(rec, crashMedium) {
				t.Errorf("the copy on %q is still ACTIVE after a completed hop, so this was not a move", crashMedium)
			}
			if n := w.objectCountOn(t, w.bucket); n != 0 {
				t.Errorf("%q still holds %d object(s) after a completed hop away from it", crashMedium, n)
			}
			if n := w.objectCountOn(t, w.secondBucket); n != 1 {
				t.Errorf("%q holds %d object(s); a resumed hop must converge on one key", crashSecondMedium, n)
			}
			if w.localExists() {
				t.Errorf("the artifact has a copy at its own local path after a staged hop; the staging copy must not land there")
			}
		})
	}
}

// TestTheStagedCrashCellsReallyStageSomething is the control the table
// above cannot carry itself.
//
// Every cell in it asserts that the staging area ends EMPTY, and an engine
// that never staged anything would satisfy that trivially, as would a
// world whose second hop was never medium-to-medium in the first place. So
// this runs the same first hop and then a suppressed-kill second hop, and
// requires that the staging copy was really written and really removed.
func TestTheStagedCrashCellsReallyStageSomething(t *testing.T) {
	w := newWorld(t)
	w.firstHop()

	res := w.crashTo(crashSecondMedium, "-plan", "-kill-after-stage", "-suppress-kill")
	if res.killed() {
		t.Fatal("the harness died even with -suppress-kill set, so the staged cells' own kill flag does not do what they rely on")
	}
	if !strings.Contains(res.stderr, "MOVECRASH_SELF_KILL_SUPPRESSED") {
		t.Fatalf("the harness never reached the staging kill point, so the staged cells are crashing somewhere else entirely:\n%s", res.stderr)
	}
	staged := filepath.Join(w.root, placement.StagingDirName, crashArtifact)
	if !strings.Contains(res.stderr, staged) {
		t.Errorf("the kill point named %q rather than the staging path %q, so -kill-after-stage is firing on some other local write",
			res.stderr, staged)
	}
	if !strings.Contains(res.stdout, "FINISHED") {
		t.Fatalf("the suppressed run did not finish:\n%s\n%s", res.stdout, res.stderr)
	}
	if left := w.stagingLeftovers(); len(left) != 0 {
		t.Errorf("an uninterrupted staged hop left %v in the staging area", left)
	}
}
