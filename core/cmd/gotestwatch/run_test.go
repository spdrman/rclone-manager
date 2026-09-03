// This file is the end-to-end evidence for issue #256, the same way
// tests/crashmatrix/watchdog_test.go is for #247 and #248: the arithmetic
// in tracker_test.go is proved against a synthetic clock so it costs the
// gate milliseconds, and this file proves that arithmetic is really wired
// to a real `go test` subprocess, under deliberately small bounds so it
// stays fast. Neither replaces the other: #247's own comment on this is
// "a fixed condition being made harder to reach is exactly the shape of
// change that can quietly stop detecting the thing it is about", so the
// property is demonstrated here, not just asserted by the unit tests.
//
// testdata/fixtures/hangpkg and testdata/fixtures/slowpkg are real,
// separate Go modules (go tool ignores anything under testdata/, so
// core's own build/vet/test/lint never see them) that this file compiles
// and runs for real via Run.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tightBounds keeps the tests below to seconds. Every ratio matches
// defaultBounds; only the floors are shrunk, because what these tests
// prove is that Run is really connected to a real go test subprocess, not
// what the production floors happen to be.
func tightBounds(floor time.Duration, factor float64) bounds {
	return bounds{
		stepFloor:     floor,
		stepFactor:    factor,
		overallFloor:  30 * time.Second,
		overallFactor: defaultBounds.overallFactor,
	}
}

func TestRun_CatchesAGenuineHang(t *testing.T) {
	const poll = 50 * time.Millisecond
	dir := filepath.Join("testdata", "fixtures", "hangpkg")

	// The tracker's clock starts when Run does, and `go test` spends real
	// time loading packages, linking and starting the binary before it
	// emits its first event. That cost sits inside the very first window,
	// so a floor smaller than it trips the watchdog before the fixture's
	// first test has run at all: a false trip about this test's setup
	// rather than about the watchdog. It is what the second failure mode
	// in #379 was, "reconstructed output does not show TestOne actually
	// having run".
	//
	// So the floor is measured rather than picked. The first call
	// compiles; the second pays exactly the startup the watched run is
	// about to pay, on this host, right now, and the floor is four times
	// that. On a quiet machine it stays at the 2s this test has always
	// used; on a loaded one it grows with the thing it has to cover.
	warmBuildCache(t, dir)
	startup := warmBuildCache(t, dir)
	floor := 2 * time.Second
	if measured := 4 * startup; measured > floor {
		floor = measured
	}
	t.Logf("no-progress floor %s, from a warm `go test` startup of %s on this host", floor.Round(time.Millisecond), startup.Round(time.Millisecond))

	lag := startSchedulingLagSampler(poll)

	var stdout, stderr bytes.Buffer
	start := time.Now()
	res, err := Run(Options{
		Dir:    dir,
		Args:   []string{"-count=1", "./..."},
		Bounds: tightBounds(floor, defaultBounds.stepFactor),
		Poll:   poll,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	elapsed := time.Since(start)
	worstLag := lag.stop()
	if err != nil {
		t.Fatalf("Run returned an error instead of a trip: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if res.Trip == nil {
		t.Fatalf("a package with a genuinely hung test was never caught\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if res.Trip.kind != "no-progress" {
		t.Fatalf("trip.kind = %q, want no-progress: %v", res.Trip.kind, res.Trip)
	}
	if !strings.Contains(stdout.String(), "=== RUN   TestOne") || !strings.Contains(stdout.String(), "--- PASS: TestOne") {
		t.Fatalf("reconstructed output does not show TestOne actually having run and passed first, so the run's own pace was never measured:\n%s", stdout.String())
	}
	if len(res.Trip.running) != 1 || res.Trip.running[0] != "TestHang" {
		t.Fatalf("trip.running = %v, want exactly [TestHang]: the whole point of issue #256's legibility ask is naming which test was mid-flight", res.Trip.running)
	}
	if !strings.Contains(res.Trip.String(), "TestHang") {
		t.Fatalf("the trip's own failure text does not name TestHang:\n%v", res.Trip)
	}
	// Promptly, and promptly against the watchdog's OWN window rather
	// than against a number chosen here (issue #379).
	//
	// The window is derived, not fixed: max(stepFloor, stepFactor x the
	// slowest recent step), and stepFactor is 12. On a loaded machine the
	// fixture's own TestOne measures slower, so the derived window grows
	// with it, by design. The previous bound here was 10 x the FLOOR,
	// which is unrelated to that, and it was wrong in both directions: it
	// would have passed a watchdog that noticed five times later than it
	// should have on a quiet machine, and it failed a perfectly prompt
	// one on a busy one (observed: sinceLast 34.61s against a window of
	// almost exactly the same, tripping on the first check after its own
	// window closed, which is the behaviour this test exists to prove).
	//
	// Before bounding the overshoot, check the window itself is the
	// derivation of the pace this run measured. tracker_test.go proves
	// that arithmetic against a synthetic clock; this checks it survived
	// the trip out through a real subprocess, which is what this file is
	// for. It also keeps the one piece of cover the old 10 x floor bound
	// gave by accident: a window that had somehow grown unbounded used to
	// fail here, and bounding only the overshoot would stop noticing.
	// Unlike that bound, this one cannot flake, because both sides move
	// together when the host is slow.
	wantWindow := time.Duration(float64(res.Trip.slowestStep) * defaultBounds.stepFactor)
	if wantWindow < floor {
		wantWindow = floor
	}
	if res.Trip.window != wantWindow {
		t.Fatalf("the trip reports a %s no-progress window, but this run's own slowest recent step was %s, which derives %s; the window an operator is shown is not the one the run's measurements imply",
			res.Trip.window, res.Trip.slowestStep, wantWindow)
	}

	// What promptness means is the overshoot past that window, and the
	// overshoot cannot exceed one gap between two consecutive polls: the
	// tick before the tripping one did not trip, so it was still inside
	// the window, and during a hang no event arrives to move the window
	// underneath them. So the quantity to bound is one poll interval plus
	// whatever this host stole from a goroutine asking to wake up on that
	// same interval, which the sampler measured rather than guessed.
	//
	// I allow three intervals rather than the one that derivation gives
	// because the sampler is a different goroutine from the watchdog's:
	// it estimates what this host did to the poll loop, it does not
	// measure it. Those two extra intervals are for that estimate being
	// off by a tick, and for nothing else. They are not a load allowance,
	// because load is already accounted for twice over, in the window and
	// in worstLag. Mutation says this is stricter rather than looser than
	// what it replaces: a watchdog that notices five windows late passes
	// the old bound and fails this one.
	promptness := res.Trip.window + 3*poll + worstLag
	if res.Trip.sinceLast > promptness {
		t.Fatalf("the watchdog took %s to notice its own %s window had closed, which is %s of overshoot; at most %s is prompt here (3 poll intervals plus the %s this host stole from a goroutine sleeping on the same interval)",
			res.Trip.sinceLast.Round(time.Millisecond), res.Trip.window.Round(time.Millisecond),
			(res.Trip.sinceLast - res.Trip.window).Round(time.Millisecond),
			promptness.Round(time.Millisecond), worstLag.Round(time.Millisecond))
	}
	t.Logf("planted hang caught %s after Run started, %s past its own %s window, on a host whose worst %s scheduling lag was %s: %v",
		elapsed.Round(time.Millisecond), (res.Trip.sinceLast - res.Trip.window).Round(time.Millisecond),
		res.Trip.window.Round(time.Millisecond), poll, worstLag.Round(time.Millisecond), res.Trip)
}

// warmBuildCache compiles a fixture module's test binaries without running
// anything, and reports how long that took. `-run ^$` matches no test at
// all, which is the whole point: this is `go test` doing everything except
// running a test, so a second call measures exactly the startup a watched
// run is about to pay on this host, and the first call is what makes the
// second one warm.
func warmBuildCache(t *testing.T, dir string) time.Duration {
	t.Helper()
	cmd := exec.Command("go", "test", "-run", "^$", "-count=1", "./...")
	cmd.Dir = dir
	// GOWORK=off for the same reason Run sets it: the fixture is its own
	// module under testdata/, which this repository's go.work does not
	// and must not list.
	cmd.Env = append(os.Environ(), "GOWORK=off")
	began := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-building the fixture in %s failed, so this test cannot tell a slow compile from a hang: %v\n%s", dir, err, out)
	}
	return time.Since(began)
}

// schedulingLagSampler measures the worst overshoot of a sleep of exactly
// the watchdog's poll interval, for as long as it runs.
//
// It exists because the quantity this file has to bound, how long the
// watchdog took to notice its window had closed, is measured BY the
// watchdog's own poll goroutine, and on a starved host that goroutine is
// precisely what stops being scheduled. A fixed allowance cannot be right
// for both an idle laptop and a machine running nine builds at once. A
// second goroutine asking the same host for the same interval can say how
// much it is actually giving, and that number is what the bound is built
// from.
type schedulingLagSampler struct {
	done    chan struct{}
	stopped chan time.Duration
}

func startSchedulingLagSampler(interval time.Duration) *schedulingLagSampler {
	s := &schedulingLagSampler{done: make(chan struct{}), stopped: make(chan time.Duration, 1)}
	go func() {
		var worst time.Duration
		for {
			select {
			case <-s.done:
				s.stopped <- worst
				return
			default:
			}
			began := time.Now()
			time.Sleep(interval)
			if over := time.Since(began) - interval; over > worst {
				worst = over
			}
		}
	}()
	return s
}

func (s *schedulingLagSampler) stop() time.Duration {
	close(s.done)
	return <-s.stopped
}

// The numbers testdata/fixtures/slowpkg is calibrated around, at package
// scope so TestSlowpkgCalibration_TheControlCanStillFire can read them.
//
// slowpkgFirstStall and slowpkgSecondStall are duplicated as constants of
// the same name in that fixture's own slow_test.go (it is a separate
// module under testdata/, so it cannot be imported from here); keep the
// two in sync by hand.
const (
	// slowpkgFirstStall is TestA's sleep, which is the pace the run
	// measures itself by before TestB stalls.
	slowpkgFirstStall = 1 * time.Second
	// slowpkgSecondStall is TestB's sleep: longer than the floor below,
	// so a fixed window fails it, and well inside stepFactor times the
	// gap TestA just measured, so a derived one absorbs it.
	slowpkgSecondStall = 6 * time.Second
	// slowpkgBaseFloor is the lower bound on the no-progress floor these
	// tests run under. The floor they actually use is derived from this
	// host's own `go test` startup and can be larger; see the test.
	slowpkgBaseFloor = 3 * time.Second
	slowpkgPoll      = 50 * time.Millisecond
)

// controlOutcome is what the negative control in
// TestRun_DoesNotFailASlowButProgressingRun concluded.
//
// It is a value rather than a t.Fatalf on the spot because of issue #401:
// a control that cannot separate "the derivation stopped being
// load-bearing" from "the host was busy" is a control nobody can read.
// Returning the verdict means both branches can be exercised directly
// (TestJudgeNegativeControl_TellsAHostFailureFromARealOne) rather than
// only on a machine that happens to be loaded at the time.
type controlOutcome int

const (
	// controlProved: the watchdog tripped on the planted stall, naming
	// TestB. The derivation is load-bearing and the positive subtest
	// means something.
	controlProved controlOutcome = iota
	// controlNotProved: the run had to trip and did not. The derivation
	// is not doing the work the positive subtest credits it with.
	controlNotProved
	// controlCannotMeasure: the host, not the fixture, decided what
	// happened, so this run says nothing either way and reporting a
	// verdict from it would be reporting on the machine.
	controlCannotMeasure
)

func (o controlOutcome) String() string {
	switch o {
	case controlProved:
		return "proved"
	case controlNotProved:
		return "not proved"
	default:
		return "could not measure"
	}
}

// judgeNegativeControl reads what running slowpkg with the derivation
// switched off (stepFactor 1) produced, and says which of the outcomes
// above happened, with the numbers behind it.
func judgeNegativeControl(res Result, floor, poll, hostSlop time.Duration) (controlOutcome, string) {
	if res.Trip == nil {
		return controlNotProved, fmt.Sprintf(
			"with the derivation off, the %s stall in TestB was still absorbed against a %s floor, so the passing subtest above proves nothing about the derivation itself",
			slowpkgSecondStall, floor.Round(time.Millisecond))
	}
	if len(res.Trip.running) != 1 || res.Trip.running[0] != "TestB" {
		return controlNotProved, fmt.Sprintf("trip.running = %v, want exactly [TestB]: %v", res.Trip.running, res.Trip)
	}
	return controlProved, fmt.Sprintf("with the derivation off, the identical run failed: %v", res.Trip)
}

func TestRun_DoesNotFailASlowButProgressingRun(t *testing.T) {
	floor := slowpkgBaseFloor
	dir := filepath.Join("testdata", "fixtures", "slowpkg")

	t.Run("derived window absorbs it", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		start := time.Now()
		res, err := Run(Options{
			Dir:    dir,
			Args:   []string{"-v", "-count=1", "./..."},
			Bounds: tightBounds(floor, defaultBounds.stepFactor),
			Poll:   slowpkgPoll,
			Stdout: &stdout,
			Stderr: &stderr,
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Run returned an error: %v\nstderr=%s", err, stderr.String())
		}
		if res.Trip != nil {
			t.Fatalf("a slow but genuinely progressing run was failed as a hang: %v\nstdout=%s", res.Trip, stdout.String())
		}
		if res.ExitCode != 0 {
			t.Fatalf("go test exited %d, want 0 (both tests should pass)\nstdout=%s", res.ExitCode, stdout.String())
		}
		if !strings.Contains(stdout.String(), "--- PASS: TestA") || !strings.Contains(stdout.String(), "--- PASS: TestB") {
			t.Fatalf("reconstructed output does not show both tests passing:\n%s", stdout.String())
		}
		if elapsed < slowpkgFirstStall+slowpkgSecondStall {
			t.Fatalf("Run returned in %s, faster than the two real sleeps (%s) sum to; the fixture did not really run", elapsed, slowpkgFirstStall+slowpkgSecondStall)
		}
		t.Logf("a run whose slowest gap was %s (TestB) finished in %s against a %s floor and was not failed",
			slowpkgSecondStall, elapsed.Round(time.Millisecond), floor.Round(time.Millisecond))
	})

	t.Run("without the derivation the same run fails", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		// stepFactor 1 is the bound with its derivation switched off: the
		// window can never grow past the slowest gap measured so far, so
		// it behaves like a fixed deadline again, which is the shape of
		// go test's own -timeout that issue #256 is about.
		res, err := Run(Options{
			Dir:    dir,
			Args:   []string{"-v", "-count=1", "./..."},
			Bounds: tightBounds(floor, 1),
			Poll:   slowpkgPoll,
			Stdout: &stdout,
			Stderr: &stderr,
		})
		if err != nil {
			t.Fatalf("Run returned an error: %v\nstderr=%s", err, stderr.String())
		}
		outcome, why := judgeNegativeControl(res, floor, slowpkgPoll, 0)
		switch outcome {
		case controlProved:
			t.Log(why)
		default:
			t.Fatalf("%s\nstdout=%s", why, stdout.String())
		}
	})
}

// TestJudgeNegativeControl_TellsAHostFailureFromARealOne is the contract
// issue #401 asks for, stated over the verdict function rather than over a
// real run, because the runs that matter are the ones this machine cannot
// be asked to produce on demand.
//
// The negative control has to reach the same verdict on a loaded host as
// on a quiet one, or say outright that it could not measure, and the two
// have to be distinguishable in the output. There are four ways the run
// can come back and only one of them is the control doing its job:
//
//   - it tripped naming TestB: proved, the planted stall is what closed
//     the window.
//   - it tripped with no event observed at all: it caught `go test` still
//     loading packages and linking, which is this host's startup and not
//     anything the fixture did.
//   - it tripped naming some other test: it caught a gap this host
//     produced before TestB's stall.
//   - it did not trip, but this host's own slowest gap was longer than
//     the planted stall, so with the derivation off the window was
//     already wider than the stall before the stall began.
//
// The last three are the same sentence to a reader of the old assertion
// ("want exactly [TestB]"), and that is precisely the complaint in #401.
func TestJudgeNegativeControl_TellsAHostFailureFromARealOne(t *testing.T) {
	const floor = 4 * time.Second
	tests := []struct {
		name     string
		res      Result
		want     controlOutcome
		mentions []string
	}{
		{
			name: "the planted stall is what tripped it",
			res: Result{Trip: &trip{
				kind: "no-progress", events: 7, running: []string{"TestB"},
				lastEvent: "output TestB (=== RUN   TestB)", sinceLast: floor + 40*time.Millisecond,
				window: floor, slowestStep: slowpkgFirstStall,
			}},
			want:     controlProved,
			mentions: []string{"TestB"},
		},
		{
			name: "it tripped on go test's own startup, before any test ran",
			res: Result{Trip: &trip{
				kind: "no-progress", events: 0, running: nil,
				lastEvent: "process start", sinceLast: floor + 3*time.Millisecond, window: floor,
			}},
			want:     controlCannotMeasure,
			mentions: []string{"process start", "no event"},
		},
		{
			name: "it tripped on a gap this host produced before TestB",
			res: Result{Trip: &trip{
				kind: "no-progress", events: 3, running: []string{"TestA"},
				lastEvent: "output TestA (=== RUN   TestA)", sinceLast: floor + 9*time.Millisecond,
				window: floor, slowestStep: 700 * time.Millisecond,
			}},
			want:     controlCannotMeasure,
			mentions: []string{"TestA"},
		},
		{
			name: "no trip, and nothing this host did beat the planted stall",
			res: Result{
				Events: 12, SlowestStep: slowpkgSecondStall + 3*time.Millisecond,
				SlowestLabel: "output TestB (=== RUN   TestB) -> output TestB (--- PASS: TestB (6.0",
				Window:       slowpkgSecondStall + 3*time.Millisecond,
			},
			want:     controlNotProved,
			mentions: []string{"6.003s"},
		},
		{
			name: "no trip, because this host's own slowest gap outran the stall",
			res: Result{
				Events: 12, SlowestStep: 9 * time.Second,
				SlowestLabel: "output TestA (=== RUN   TestA) -> output TestA (--- PASS: TestA (1.0",
				Window:       9 * time.Second,
			},
			want:     controlCannotMeasure,
			mentions: []string{"9s", "TestA"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, why := judgeNegativeControl(tc.res, floor, slowpkgPoll, 20*time.Millisecond)
			if got != tc.want {
				t.Fatalf("outcome = %v, want %v; the control said: %s", got, tc.want, why)
			}
			for _, want := range tc.mentions {
				if !strings.Contains(why, want) {
					t.Errorf("the verdict does not mention %q, so a reader cannot tell this outcome from the others:\n%s", want, why)
				}
			}
		})
	}
}

// TestSlowpkgCalibration_TheControlCanStillFire guards the fixture's four
// numbers rather than any behaviour, because the way the negative control
// above stops working is not a broken assertion. It is a calibration that
// drifts until the control can no longer fire, and a control that never
// fires looks exactly like one that always passes. That is the same guard
// TestKeyCommandTimeoutBudget_CanStillFail keeps over its own three
// constants, for the same reason (#394), and it matters more here because
// the fix for #401 gives this test a skip path.
func TestSlowpkgCalibration_TheControlCanStillFire(t *testing.T) {
	// With the derivation ON the window is stepFactor times the slowest
	// recent gap, and the slowest gap before TestB stalls is TestA's own.
	// If that product does not clear TestB's stall, the positive subtest
	// fails against a perfectly correct watchdog.
	if absorbs := time.Duration(float64(slowpkgFirstStall) * defaultBounds.stepFactor); absorbs <= slowpkgSecondStall {
		t.Errorf("TestA's %s stall derives a %s window at stepFactor %v, which does not clear TestB's %s stall, so the derived window cannot absorb it and the positive subtest fails on a correct watchdog",
			slowpkgFirstStall, absorbs, defaultBounds.stepFactor, slowpkgSecondStall)
	}

	// With the derivation OFF the window is the floor, and the floor is
	// at least slowpkgBaseFloor. TestB's stall has to clear it, with a
	// poll interval to spare so the tick that notices actually lands,
	// otherwise the control never trips on any host and the two subtests
	// stop being opposites.
	if slowpkgSecondStall <= slowpkgBaseFloor+slowpkgPoll {
		t.Errorf("TestB's %s stall does not clear the %s floor plus one %s poll interval, so with the derivation off the window still absorbs it and the negative control cannot fire at all",
			slowpkgSecondStall, slowpkgBaseFloor, slowpkgPoll)
	}

	// And TestA's own stall has to stay UNDER the floor. If it does not,
	// the negative control trips during TestA instead, which is a trip
	// that says nothing about the derivation.
	if slowpkgFirstStall >= slowpkgBaseFloor {
		t.Errorf("TestA's %s stall is at or past the %s floor, so with the derivation off the control trips on TestA before TestB ever stalls",
			slowpkgFirstStall, slowpkgBaseFloor)
	}
}

func TestRun_RefusesATimeoutOrJsonFlagFromTheCaller(t *testing.T) {
	for _, arg := range []string{"-timeout=30s", "-timeout", "-json", "--timeout=1m"} {
		t.Run(arg, func(t *testing.T) {
			_, err := Run(Options{Args: []string{arg, "./..."}, Bounds: defaultBounds})
			if err == nil {
				t.Fatalf("Run accepted %q, which gotestwatch owns itself; a caller-supplied -timeout would silently win the last-flag-wins race against gotestwatch's own -timeout=0 and reintroduce issue #256", arg)
			}
		})
	}
}

// TestKillAndWait_DoesNotBlockForeverIfTheProcessGroupNeverReaps proves the
// review finding on issue #256: after a trip sends SIGKILL, Run used to
// block unboundedly on the child's exit. SIGKILL cannot terminate a
// process stuck in uninterruptible kernel I/O wait (D state), a real
// possibility for exactly the class of hang this tool targets (stuck
// Docker/SFTP I/O) — and a real D-state process cannot be manufactured
// portably or quickly in a test. This instead proves the contract
// killAndWait must uphold, which is what actually matters: if the
// process's exit never arrives on `waited`, killAndWait must still return
// within its bound rather than block forever, exactly the case a
// genuinely stuck process would produce.
//
// pgid is deliberately a pid far outside any real process's range, so the
// SIGKILL this sends is a guaranteed no-op (ESRCH) and this test cannot
// affect any real process on the machine it runs on.
func TestKillAndWait_DoesNotBlockForeverIfTheProcessGroupNeverReaps(t *testing.T) {
	const farOutsideAnyRealPid = 1 << 30
	waited := make(chan error) // deliberately never written to: the reap that never comes

	start := time.Now()
	waitErr, reaped := killAndWait(farOutsideAnyRealPid, waited, 100*time.Millisecond)
	elapsed := time.Since(start)

	if reaped {
		t.Fatal("killAndWait reported the process group reaped when the wait channel never delivered anything")
	}
	if waitErr != nil {
		t.Fatalf("waitErr = %v, want nil when the reap timed out", waitErr)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("killAndWait took %s to give up on a 100ms reap bound; it is not actually bounded", elapsed)
	}
}

// TestKillAndWait_ReturnsPromptlyWhenTheProcessDoesReap proves killAndWait
// does not itself introduce a delay on the ordinary path, where the
// killed process exits normally and `waited` delivers right away.
func TestKillAndWait_ReturnsPromptlyWhenTheProcessDoesReap(t *testing.T) {
	const farOutsideAnyRealPid = 1 << 30
	waited := make(chan error, 1)
	waited <- nil

	start := time.Now()
	waitErr, reaped := killAndWait(farOutsideAnyRealPid, waited, 5*time.Second)
	elapsed := time.Since(start)

	if !reaped {
		t.Fatal("killAndWait reported the process group not reaped when the wait channel delivered immediately")
	}
	if waitErr != nil {
		t.Fatalf("waitErr = %v, want nil", waitErr)
	}
	if elapsed > time.Second {
		t.Fatalf("killAndWait took %s to return an already-delivered result; it waited on the reap bound instead of the channel", elapsed)
	}
}
