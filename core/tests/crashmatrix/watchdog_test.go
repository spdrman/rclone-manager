// This file is the evidence for the two bounds crash_matrix_test.go now
// runs on, one per issue, and for the property both of them had to keep.
//
// Issue #247 replaced a fixed 45-second deadline on a whole harness
// invocation with a no-progress window derived from the slowest step the
// run itself has already completed. Issue #248 replaced a calibrated
// stopwatch, which decided when to kill the harness mid-verify by
// measuring a read and then racing it, with a rendezvous through a pipe
// that has no timer in it at all.
//
// Both changes make a failing condition harder to reach, which is exactly
// the shape of change that can quietly stop detecting the thing it is
// about. So neither is asserted here, both are demonstrated:
//
//   - a genuinely hung harness is still caught, still promptly, and is not
//     left running (TestHarnessWatchdog_CatchesAGenuineHang);
//   - a slow-but-progressing harness is not failed, and it is specifically
//     the derivation that saves it, shown by running the identical harness
//     under bounds with the derivation turned off and watching it fail
//     (TestHarnessWatchdog_DoesNotFailASlowButProgressingRun);
//   - the window still closes on a run that goes quiet, however slow that
//     run had been beforehand, so "derived" never means "unbounded"
//     (TestProgressTracker_AWidenedWindowStillCloses);
//   - a harness that genuinely is not killed is still rejected at every
//     crash point that requires a kill, shown by mutating the kill out and
//     watching the guard reject the run
//     (TestMutation_AnUnkilledHarnessIsStillRejected).
//
// The arithmetic of the bounds is proved against a synthetic clock rather
// than by sleeping, so the documented 45-second floor is pinned exactly and
// this file costs the gate seconds rather than minutes. The end-to-end
// tests then prove that arithmetic is really wired to a real subprocess,
// under deliberately small bounds so they stay fast.
package crashmatrix_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// --- the bounds themselves, against a synthetic clock -------------------

func TestProgressTracker_TripsAtTheDocumentedFloorBeforeAnythingIsMeasured(t *testing.T) {
	start := time.Now()
	p := newProgressTracker(defaultHarnessBounds, start)

	floor := defaultHarnessBounds.stepFloor
	if trip := p.check(start.Add(floor - time.Millisecond)); trip != nil {
		t.Fatalf("tripped %s into a run, one millisecond before the %s floor: %v", floor-time.Millisecond, floor, trip)
	}
	trip := p.check(start.Add(floor + time.Millisecond))
	if trip == nil {
		t.Fatalf("a harness that reported nothing at all for %s was not caught", floor+time.Millisecond)
	}
	if trip.kind != "no-progress" {
		t.Fatalf("trip.kind = %q, want no-progress: %v", trip.kind, trip)
	}
	if trip.lastEvent != "process start" {
		t.Fatalf("trip.lastEvent = %q, want the run's own starting point", trip.lastEvent)
	}
	// The old constant is the new floor deliberately (see harnessBounds's
	// doc), so a hang before the run has measured itself is caught exactly
	// as promptly as it was before this change, never less promptly.
	if floor != 45*time.Second {
		t.Fatalf("stepFloor = %s; #247's whole argument is that this number now bounds ONE operation rather than a whole pipeline, so changing it needs that comment revisited", floor)
	}
}

func TestProgressTracker_WidensWithTheSlowestStepItHasSeen(t *testing.T) {
	start := time.Now()
	p := newProgressTracker(defaultHarnessBounds, start)

	// A machine so loaded that a single SFTP round trip takes ten seconds.
	// Under the old fixed deadline a run of eight of these was already
	// over budget while nothing whatsoever was wrong; here the same
	// evidence widens the window instead.
	at := start
	for i, event := range []string{"discover-start", "discover-done", "transfer-start", "transfer-done"} {
		at = at.Add(10 * time.Second)
		p.observe(event, at)
		if trip := p.check(at); trip != nil {
			t.Fatalf("step %d (%s) was reported as a hang: %v", i, event, trip)
		}
	}

	if got, want := p.window(), 120*time.Second; got != want {
		t.Fatalf("window after four ten-second steps = %s, want %s (stepFactor x the slowest step)", got, want)
	}
	// The number that matters: forty seconds of silence, which the old
	// 45s deadline was already most of the way through by the time the run
	// even started, is not a hang on a machine that has just been watched
	// taking ten seconds over a single round trip.
	if trip := p.check(at.Add(40 * time.Second)); trip != nil {
		t.Fatalf("40s of silence on a machine measured at 10s per step was called a hang: %v", trip)
	}
}

func TestProgressTracker_AWidenedWindowStillCloses(t *testing.T) {
	start := time.Now()
	p := newProgressTracker(defaultHarnessBounds, start)

	at := start.Add(10 * time.Second)
	p.observe("discover-done", at)

	// Derived is not the same as unbounded. However slow this machine has
	// been, going quiet for twelve times its own slowest step is still a
	// hang, and still says so.
	if trip := p.check(at.Add(119 * time.Second)); trip != nil {
		t.Fatalf("tripped one second inside the derived window: %v", trip)
	}
	trip := p.check(at.Add(121 * time.Second))
	if trip == nil {
		t.Fatal("a harness that went quiet for more than its derived window was never caught, so the bound is unbounded")
	}
	if trip.kind != "no-progress" || trip.lastEvent != "discover-done" {
		t.Fatalf("trip = %+v, want a no-progress trip naming discover-done", *trip)
	}
	if !strings.Contains(trip.String(), "discover-done") {
		t.Fatalf("the failure text does not name the step the harness got stuck in:\n%v", trip)
	}
}

func TestProgressTracker_OverallCapCatchesALivelock(t *testing.T) {
	start := time.Now()
	p := newProgressTracker(defaultHarnessBounds, start)

	// A resume loop that never advances a state reports progress forever,
	// so no no-progress window can ever see it. Its steps are fast, which
	// is what keeps its cap at the floor and kills it promptly.
	at := start
	for at.Sub(start) < defaultHarnessBounds.overallFloor+time.Second {
		at = at.Add(50 * time.Millisecond)
		p.observe("loop-at-VERIFYING", at)
	}
	trip := p.check(at)
	if trip == nil {
		t.Fatalf("a harness looping forever while reporting progress ran for %s and was never caught", at.Sub(start))
	}
	if trip.kind != "overall" {
		t.Fatalf("trip.kind = %q, want overall: %v", trip.kind, trip)
	}
	if !strings.Contains(trip.String(), "livelock") {
		t.Fatalf("the failure text does not say what kind of failure this is:\n%v", trip)
	}
}

// --- the same bounds, wired to a real subprocess ------------------------

// tightBounds keeps the end-to-end tests below to seconds. Every ratio in
// it matches defaultHarnessBounds; only the floors are shrunk, because what
// these two tests prove is that the arithmetic above is really connected to
// a real process, not what the production floors happen to be.
func tightBounds(floor time.Duration, factor float64) harnessBounds {
	return harnessBounds{
		stepFloor:     floor,
		stepFactor:    factor,
		overallFloor:  2 * time.Minute,
		overallFactor: defaultHarnessBounds.overallFactor,
		poll:          25 * time.Millisecond,
	}
}

func TestHarnessWatchdog_CatchesAGenuineHang(t *testing.T) {
	s := newLocalScenario(t, 4096)

	const floor = 3 * time.Second
	// Build first, so what is timed below is the watchdog and not a `go
	// build` that may or may not have already happened in this package.
	buildHarness(t)
	start := time.Now()
	res, trip := runHarnessWatched(t, tightBounds(floor, defaultHarnessBounds.stepFactor),
		append(s.baseArgs(), "-hang-at=discover-start")...)
	elapsed := time.Since(start)

	if trip == nil {
		t.Fatalf("a harness planted to stop dead inside its first remote call was never caught (exit err=%v)\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
	}
	if trip.kind != "no-progress" {
		t.Fatalf("trip.kind = %q, want no-progress: %v", trip.kind, trip)
	}
	if trip.lastEvent != "discover-start" {
		t.Fatalf("trip.lastEvent = %q, want discover-start: a gate failure that does not name where the harness got stuck is the thing both issues complain about", trip.lastEvent)
	}
	// Promptly, not eventually. The window here is the floor, because the
	// steps before the planted hang are milliseconds long. The bound
	// asserted on is how long the watchdog took to notice its window had
	// closed, rather than wall-clock time: this file must not reintroduce
	// the very thing #247 is about by failing on a machine that was merely
	// busy while it ran.
	if trip.sinceLast > 2*floor {
		t.Fatalf("the watchdog took %s to notice a %s window had closed; that is not a prompt failure", trip.sinceLast.Round(time.Millisecond), floor)
	}
	// cmd.Wait returned, which is the process being reaped: the watchdog
	// does not leave behind the process it gave up on.
	if res.err == nil {
		t.Fatalf("the hung harness exited cleanly, so it was never actually killed:\nstderr=%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "CRASHMATRIX_PLANTED_HANG") {
		t.Fatalf("the harness never reached the planted hang, so this test proved nothing:\nstderr=%s", res.stderr)
	}
	t.Logf("planted hang caught %s after the process started (%s of wall clock including spawn): %v",
		trip.elapsed.Round(time.Millisecond), elapsed.Round(time.Millisecond), trip)
}

func TestHarnessWatchdog_DoesNotFailASlowButProgressingRun(t *testing.T) {
	// Two planted stalls. The first is inside the floor and is what the
	// run measures itself by; the second is twice the floor, and is
	// survivable only because the first one widened the window. That makes
	// this an A/B of the derivation itself rather than of a bigger number:
	// the same harness, the same stalls, and the only difference is
	// whether the window is allowed to grow.
	// The floor is three times the first stall rather than twice it, so
	// the measuring step has room for whatever real work happens around
	// its sleep without tripping on a busy machine. Widening it costs
	// nothing: the run's length is set by the stalls, not the floor.
	const (
		floor       = 3 * time.Second
		firstStall  = 1 * time.Second
		secondStall = 6 * time.Second
	)
	stalls := "-stall-at=discover-start:" + firstStall.String() + ",verify-start:" + secondStall.String()

	t.Run("derived window absorbs it", func(t *testing.T) {
		s := newLocalScenario(t, 4096)
		start := time.Now()
		res, trip := runHarnessWatched(t, tightBounds(floor, defaultHarnessBounds.stepFactor), append(s.baseArgs(), stalls)...)
		if trip != nil {
			t.Fatalf("a slow but progressing harness was failed as a hang: %v\nstderr=%s", trip, res.stderr)
		}
		if res.err != nil {
			t.Fatalf("the slow harness did not finish cleanly: %v\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
		}
		if final, ok := res.finalState(); !ok || final != "COMPLETE" {
			t.Fatalf("the slow harness reached %q, want COMPLETE\nstdout=%s", final, res.stdout)
		}
		t.Logf("a run whose slowest step was %s (twice the %s floor) finished in %s and was not failed",
			secondStall, floor, time.Since(start).Round(time.Millisecond))
	})

	t.Run("without the derivation the same run fails", func(t *testing.T) {
		s := newLocalScenario(t, 4096)
		// stepFactor 1 is the bound with its derivation switched off: the
		// window can never grow past the slowest step, so it behaves like
		// the fixed deadline #247 is about.
		res, trip := runHarnessWatched(t, tightBounds(floor, 1), append(s.baseArgs(), stalls)...)
		if trip == nil {
			t.Fatalf("with the derivation off, the %s stall was still absorbed, so the passing case above proves nothing about the derivation\nstdout=%s", secondStall, res.stdout)
		}
		if trip.lastEvent != "verify-start" {
			t.Fatalf("trip.lastEvent = %q, want verify-start (the stalled step): %v", trip.lastEvent, trip)
		}
		t.Logf("with the derivation off, the identical run failed: %v", trip)
	})
}

// --- the property both fixes had to keep --------------------------------

// TestMutation_AnUnkilledHarnessIsStillRejected is the answer to the
// obvious objection to both changes: that making a bound more generous, or
// replacing a race with a rendezvous, could leave a suite that no longer
// notices when the crash it is named after does not happen.
//
// -mutation-suppress-self-kill turns every self-destruct in the harness
// into a no-op, so each of these runs does exactly what a broken kill
// mechanism would do: reaches the crash point, does not die, and carries on
// to a normal terminal state. The guard every one of those crash points
// runs on must reject that, and must say why in terms an operator can act
// on.
func TestMutation_AnUnkilledHarnessIsStillRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			// #248's crash point, on its new deterministic mechanism.
			name: "mid-verify, the deterministic handoff",
			args: []string{"-kill-plan=mid-verify"},
		},
		{
			// A journal-boundary crash point, on the mechanism that was
			// never in question, so the guard is shown to be general
			// rather than something this file arranged for one case.
			name: "after a state is durably journaled",
			args: []string{"-kill-after-state=VERIFYING"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLocalScenario(t, 48<<20)
			args := append(s.baseArgs(), "-mutation-suppress-self-kill")
			res := runHarness(t, append(args, tc.args...)...)

			// First, the mutation really did reproduce the failure it is
			// meant to: an unkilled harness that ran to a normal end.
			if res.killedBy(syscall.SIGKILL) {
				t.Fatalf("the mutation did not take: the harness died anyway, so nothing is being tested here\nstderr=%s", res.stderr)
			}
			if final, ok := res.finalState(); !ok || final != "COMPLETE" {
				t.Fatalf("the suppressed run reached %q, want COMPLETE; the mutation has to reproduce the exact shape of #248's failure\nstdout=%s\nstderr=%s", final, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, "CRASHMATRIX_SELF_KILL:") {
				t.Fatalf("the harness never reached its kill point at all, so this run is not the mutation it claims to be\nstderr=%s", res.stderr)
			}

			// Then the actual assertion: the guard rejects it.
			problem := notKilledProblem(res)
			if problem == "" {
				t.Fatal("a harness that was never killed passed the crash-matrix guard, so that crash point no longer detects anything")
			}
			if !strings.Contains(problem, "COMPLETE") || !strings.Contains(problem, "never") {
				t.Fatalf("the rejection does not say what actually went wrong; #248 asks for a message that does not read as a signal-delivery problem: %q", problem)
			}
			t.Logf("rejected, as it must be: %s", problem)
		})
	}
}

// TestMutation_ATimerThatMissesItsWindowIsStillRejected reproduces #248's
// failure exactly, deterministically, and proves two things about it.
//
// -kill-plan=mid-transfer still races a calibrated timer against the real
// copy (only mid-verify was converted to a rendezvous; see this package's
// doc for why the other raced points do not require the kill to land).
// Setting the fraction well above 1 makes that timer certain to fire after
// the copy it is racing has already finished, which is precisely what
// happened to mid-verify under gate load: the operation finishes first and
// the process runs on to FINAL_STATE=COMPLETE.
//
// So the guard must still reject it, and the rejection must now say the
// kill missed its window, with the numbers. "harness was not killed by
// SIGKILL (err=<nil>)" reads like a signal-delivery problem and is what
// sent the report looking in the wrong place first.
func TestMutation_ATimerThatMissesItsWindowIsStillRejected(t *testing.T) {
	s := newLocalScenario(t, 48<<20)
	res := runHarness(t, append(s.baseArgs(), "-kill-plan=mid-transfer", "-mid-fraction=6.0")...)

	if res.killedBy(syscall.SIGKILL) {
		t.Fatalf("a timer set to six times the calibrated duration still won its race, so this test is not reproducing a miss\nstdout=%s", res.stdout)
	}
	missed, ok := res.killMissed()
	if !ok {
		t.Fatalf("the harness missed its window and never said so; that number is knowable only on the runs that miss\nstdout=%s", res.stdout)
	}

	problem := notKilledProblem(res)
	if problem == "" {
		t.Fatal("a kill that fired after the operation it was racing had already finished passed the guard, so that crash point no longer detects anything")
	}
	if !strings.Contains(problem, "missed its window") {
		t.Fatalf("the rejection still does not say the kill missed its window, which is what #248 asked for: %q", problem)
	}
	t.Logf("reproduced and rejected: %s\nharness reported: %s", problem, missed)
}

// TestMidVerifyHandoff_ReallyKillsInsideTheRead is the positive control for
// the mutation above, and the direct proof of #248's mechanism: the same
// scenario, without the mutation, dies by SIGKILL with VERIFYING the last
// thing durably journaled, and the handoff line on stdout says how much of
// the real file the product had genuinely consumed by then.
//
// TestCrash_MidVerifying_RealInFlightKill already asserts the on-disk
// consequences; this one is about the mechanism, so it asserts what the
// rendezvous itself reported.
func TestMidVerifyHandoff_ReallyKillsInsideTheRead(t *testing.T) {
	s := newLocalScenario(t, 48<<20)
	res := runHarness(t, append(s.baseArgs(), "-kill-plan=mid-verify")...)
	requireKilledBySIGKILL(t, res)

	if !strings.Contains(res.stdout, "HANDOFF verify_read_bytes=") {
		t.Fatalf("the handoff never completed, so the kill did not land where this mechanism claims:\nstdout=%s\nstderr=%s", res.stdout, res.stderr)
	}
	// The reader is what drains a pipe, so a completed handoff of this
	// size is a statement about lifecycle.Verify's own io.Copy having
	// consumed it, not about this harness having written it somewhere.
	if !strings.Contains(res.stderr, "verify-handoff-reader-attached") {
		t.Fatalf("the product never opened the handoff pipe, so no rendezvous happened:\nstderr=%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "provably still in flight") {
		t.Fatalf("the kill was not the handoff's:\nstderr=%s", res.stderr)
	}
	// And nothing was left behind: the pipe lives beside the journal, in
	// the test's own temp directory, so a crashed run must not leave a
	// FIFO where a later reader could block on it forever.
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(s.journalPath), "crashmatrix-verify-handoff-*.fifo"))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		t.Logf("note: the SIGKILLed harness could not run its own deferred cleanup, so its handoff pipe survives at %s (mode %s); it is inside the test's temp directory and goes with it", m, info.Mode())
	}
	t.Logf("harness stdout:\n%s", res.stdout)
}
