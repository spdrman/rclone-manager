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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateIncomplete is scripts/lib/ci-local-gate.sh's GATE_INCOMPLETE: the
// status that script reserves for a run which performed less than it was
// asked to, distinct from 0 for "performed everything" and from whatever
// an actual failure returns. The same number here on purpose, because
// this is the same problem one level down, and that script's own comment
// states it better than I can: "a skip that is indistinguishable from a
// pass in both the exit code and the final line is the same class of bug
// as a test that asserts nothing: green because it could not look".
const gateIncomplete = 3

// skipLedger records every check in this package that was asked for and
// did not run, so the run cannot end the same colour as one that ran
// everything.
//
// The negative control in TestRun_DoesNotFailASlowButProgressingRun can
// decide that this host cannot be measured, and saying so is the right
// answer (#401). But t.Skip leaves `go test` printing ok and exiting 0,
// so a control that has quietly stopped controlling looks exactly like a
// working one to anyone reading a green run, which is the failure this
// whole file exists to prevent, one level up. Nothing counted those
// skips; this does.
//
// Deliberately not gated on CI or CI_LOCAL. The run people actually stare
// at while editing this file is the local one, so an env-gated version
// would keep the defect in exactly the place it does the most damage, and
// would be a second thing to get wrong. scripts/ci-local.sh does not gate
// its own ledger either.
type skipLedger struct {
	mu    sync.Mutex
	notes []string
}

// note records one check that did not run. Reach it through
// skipCannotMeasure rather than calling it directly, so a skip and its
// ledger entry cannot drift apart.
func (l *skipLedger) note(what string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notes = append(l.notes, what)
}

// verdict turns the ledger into this run's last line and its exit status,
// given the status m.Run() produced.
//
// Three outcomes, three statuses, the same three scripts/ci-local.sh
// uses, so a wrapper never has to parse prose: the status m.Run() gave
// for a run that performed everything, gateIncomplete for one that
// skipped something, and an actual failure's status ahead of both,
// because a failure is the more urgent news and INCOMPLETE would bury it.
func (l *skipLedger) verdict(code int, w io.Writer) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.notes) == 0 {
		return code
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "==> gotestwatch self-test: INCOMPLETE (exit %d). Any PASS above means no test failed. It does not mean the watchdog's negative control ran. These checks did not:\n", gateIncomplete)
	for _, n := range l.notes {
		fmt.Fprintln(w, "        - "+n)
	}
	fmt.Fprintln(w, "==> This run is not evidence that the negative control still fires. Re-run it on a host quiet enough to measure.")
	if code != 0 {
		return code
	}
	return gateIncomplete
}

// controlSkips is this package's ledger. See skipLedger.
var controlSkips skipLedger

func TestMain(m *testing.M) {
	os.Exit(controlSkips.verdict(m.Run(), os.Stderr))
}

// skipCannotMeasure records a check that could not run, then skips it.
// Nothing in this file should call t.Skip directly: the ledger is the
// only reason a skipped control is not the same colour as a passing one.
func skipCannotMeasure(t *testing.T, format string, args ...any) {
	t.Helper()
	why := fmt.Sprintf(format, args...)
	controlSkips.note(t.Name() + ": " + why)
	t.Skip("could not measure: " + why)
}

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
	// controlBroken: gotestwatch's own account of the run contradicts the
	// output that same run replayed, so what failed is gotestwatch, not
	// the host and not the derivation.
	//
	// It fails, like controlNotProved, and it is a separate outcome from
	// it because the two send a reader to different places: one says the
	// derived window stopped being load-bearing, the other says the
	// tracker cannot be believed about anything, including about whether
	// the control could run. This is the bucket the safety review of #406
	// found two real defects hiding in, counted as "could not measure",
	// which is a skip.
	controlBroken
)

func (o controlOutcome) String() string {
	switch o {
	case controlProved:
		return "proved"
	case controlNotProved:
		return "not proved"
	case controlCannotMeasure:
		return "could not measure"
	case controlBroken:
		return "gotestwatch contradicted its own output"
	default:
		return fmt.Sprintf("controlOutcome(%d)", int(o))
	}
}

// replayRecorder is the Stdout a watched run writes its reconstructed
// `go test -v` output to, keeping every chunk with the instant it
// arrived.
//
// The timestamps are the point. Run decides a trip and then kills the
// child, so bytes can still arrive after the decision was made, and only
// what had been replayed BEFORE it says what the watchdog had to work
// with. bytes.Buffer cannot answer that question.
type replayRecorder struct {
	mu     sync.Mutex
	chunks []replayChunk
}

type replayChunk struct {
	at   time.Time
	text string
}

func (r *replayRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks = append(r.chunks, replayChunk{at: time.Now(), text: string(p)})
	return len(p), nil
}

// upTo returns everything replayed at or before at. Writes are serialised
// on r.mu, so the chunks are already in time order.
func (r *replayRecorder) upTo(at time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, c := range r.chunks {
		if c.at.After(at) {
			break
		}
		b.WriteString(c.text)
	}
	return b.String()
}

func (r *replayRecorder) String() string { return r.upTo(time.Now()) }

// controlWitness is everything the negative control knows that did NOT
// come from the tracker.
//
// That distinction is the whole finding. Every number judgeNegativeControl
// reads off Result — the event count, the running-test names, the slowest
// gap — is produced by the tracker, which is the subsystem this control
// exists to test. So a broken tracker got to decide whether the tracker
// was working, and it decided in its own favour: the safety review of
// #406 put broken event counting and broken running-test attribution into
// the "could not measure" bucket, which skips, rather than into the one
// that fails.
//
// These three facts have a different provenance. Two of them are the
// bytes `go test` itself printed, copied off its stdout pipe and replayed
// by Run, so they say what the child really did whatever the tracker made
// of it. The third is this test's own wall clock. Between them they can
// contradict the tracker's story, and a contradicted tracker is a defect,
// not a busy machine.
type controlWitness struct {
	// sawOutput: `go test` had already replayed at least one byte of its
	// own output by the instant the watchdog decided, so it was past
	// loading packages, linking and starting the binary. observeLine
	// calls tr.observe BEFORE it writes those bytes, so a tracker that
	// then reports zero events observed is contradicting itself.
	sawOutput bool
	// testBInFlight: "=== RUN   TestB" had been replayed and
	// "--- PASS: TestB" had not, so TestB had started and not finished.
	// go test -json emits the "run" action before that first line and the
	// "pass" action after the last one, so a tracker that then reports
	// anything other than [TestB] running is contradicting itself too.
	testBInFlight bool
	// elapsed is how long Run took by this test's own clock, which is the
	// only measurement here the tracker had no hand in.
	elapsed time.Duration
}

// witnessRun reads the two independent facts above off a finished run.
//
// runStart is this test's own clock reading from just before Run, and
// tp.elapsed is measured from Run's own, taken a few calls later, so
// runStart+tp.elapsed lands at or before the instant the watchdog
// actually decided. Erring early is the safe direction: it can only make
// this credit the watchdog with having seen less than it did, never with
// having seen more, so the defect verdicts below are never reached on a
// technicality.
func witnessRun(rec *replayRecorder, runStart time.Time, elapsed time.Duration, tp *trip) controlWitness {
	replayed := rec.String()
	if tp != nil {
		replayed = rec.upTo(runStart.Add(tp.elapsed))
	}
	return controlWitness{
		sawOutput:     strings.TrimSpace(replayed) != "",
		testBInFlight: strings.Contains(replayed, "=== RUN   TestB") && !strings.Contains(replayed, "--- PASS: TestB"),
		elapsed:       elapsed,
	}
}

// judgeNegativeControl reads what running slowpkg with the derivation
// switched off (stepFactor 1) produced, and says which of the outcomes
// above happened, with the numbers behind it.
//
// floor is the no-progress floor that run was given, poll its watchdog
// interval, and hostSlop the worst overshoot a sampler goroutine measured
// on this host asking for that same interval while the run was going.
func judgeNegativeControl(res Result, floor, poll, hostSlop time.Duration, w controlWitness) (controlOutcome, string) {
	if res.Trip == nil {
		// With the derivation off the window can never grow past the
		// slowest gap already measured, so the only way TestB's stall
		// survives is that this host had ALREADY produced a gap as long
		// as the stall, before the stall began. The stall's own gap only
		// enters the tracker's memory once TestB finishes, so on a host
		// that did nothing of the sort the run's slowest gap IS the
		// planted stall and nothing longer, which is what makes the two
		// separable at all. "Under the stall" would not: after the run
		// the stall is always in there.
		outran := slowpkgSecondStall + poll + hostSlop
		if res.SlowestStep > outran {
			// Before believing that story, check it against the one
			// clock in here the tracker had no hand in. A run's gaps
			// partition its timeline and never overlap, so a run that
			// really produced a gap this long ALSO produced TestB's own
			// stall, one after the other, and took at least the two of
			// them added together. If it did not last that long, the gap
			// is not a fact about this host: it is the tracker
			// misreporting itself, and the excuse it buys the control is
			// one the tracker wrote about the tracker.
			if need := res.SlowestStep + slowpkgSecondStall; w.elapsed < need {
				return controlBroken, fmt.Sprintf(
					"the derivation was off and the run still finished, and the tracker says this host's own slowest gap was %s (%s). A run's gaps never overlap, so a run that really produced that gap spent it AND TestB's own %s stall one after the other, %s at the least. This run took %s. "+
						"The gap is the tracker misreporting itself, not something this host did, and it would otherwise have bought the control a skip on the word of the very thing the control is testing",
					res.SlowestStep.Round(time.Millisecond), res.SlowestLabel, slowpkgSecondStall,
					need.Round(time.Millisecond), w.elapsed.Round(time.Millisecond))
			}
			return controlCannotMeasure, fmt.Sprintf(
				"the derivation was off and the run still finished, but this host's own slowest gap was %s (%s), past the %s planted stall plus one %s poll and the %s this host was measured stealing from a goroutine sleeping on that interval. "+
					"With the derivation off the window is that slowest gap, so it was already wider than the stall before the stall began, and nothing here says whether the derivation is load-bearing",
				res.SlowestStep.Round(time.Millisecond), res.SlowestLabel, slowpkgSecondStall, poll, hostSlop.Round(time.Millisecond))
		}
		return controlNotProved, fmt.Sprintf(
			"with the derivation off, the %s stall in TestB was still absorbed against a %s floor, and the slowest gap this run measured at all was %s (%s), which is the planted stall itself and nothing longer. "+
				"So the host did not decide this: no-progress detection did not fire on the one run it has to fire on, and the passing subtest above proves nothing about the derivation itself",
			slowpkgSecondStall, floor.Round(time.Millisecond), res.SlowestStep.Round(time.Millisecond), res.SlowestLabel)
	}
	if res.Trip.events == 0 {
		if w.sawOutput {
			// observeLine counts the event BEFORE it writes those bytes,
			// and both happen under the tracker's own mutex relative to
			// check, so output replayed before the decision means an
			// event was counted before the decision. A trip that says
			// otherwise is not this host's startup outgrowing the floor.
			// It is gotestwatch failing to count, which is exactly the
			// defect this branch used to absorb.
			return controlBroken, fmt.Sprintf(
				"the watchdog tripped %s after %q reporting 0 events observed, but `go test` had already replayed its own output before that decision. "+
					"observeLine counts an event before it writes those bytes, so this is not this host's startup outgrowing the %s floor: it is gotestwatch not counting what it printed",
				res.Trip.sinceLast.Round(time.Millisecond), res.Trip.lastEvent, floor.Round(time.Millisecond))
		}
		return controlCannotMeasure, fmt.Sprintf(
			"the watchdog tripped %s after %q with no event observed yet, so it caught `go test` itself still loading packages, linking and starting the binary rather than anything the fixture did. "+
				"The %s floor derived for this run did not clear this host's own startup, so the control never reached TestB's %s stall",
			res.Trip.sinceLast.Round(time.Millisecond), res.Trip.lastEvent, floor.Round(time.Millisecond), slowpkgSecondStall)
	}
	if len(res.Trip.running) != 1 || res.Trip.running[0] != "TestB" {
		if w.testBInFlight {
			// go test -json emits the "run" action before a test's first
			// output line and the "pass" action after its last, so a run
			// that had replayed "=== RUN   TestB" and not
			// "--- PASS: TestB" had TestB and only TestB in flight. The
			// trip had to name it, and naming anything else is broken
			// attribution rather than a gap this host produced early.
			return controlBroken, fmt.Sprintf(
				"the watchdog tripped with %v reported running, %d events in, but TestB had started and not finished by the moment it decided: `go test` had replayed \"=== RUN   TestB\" and not \"--- PASS: TestB\". "+
					"So the planted stall is what closed the window and the trip did not say so. That is running-test attribution broken, not a gap this host produced before TestB",
				res.Trip.running, res.Trip.events)
		}
		return controlCannotMeasure, fmt.Sprintf(
			"the watchdog tripped with %v reported running rather than [TestB], %d events in: nothing after %q for %s against a %s window. "+
				"It closed on a gap this host produced before TestB stalled, not on the planted stall, so this run cannot say whether the derivation is load-bearing",
			res.Trip.running, res.Trip.events, res.Trip.lastEvent,
			res.Trip.sinceLast.Round(time.Millisecond), res.Trip.window.Round(time.Millisecond))
	}
	return controlProved, fmt.Sprintf("with the derivation off, the identical run failed: %v", res.Trip)
}

func TestRun_DoesNotFailASlowButProgressingRun(t *testing.T) {
	dir := filepath.Join("testdata", "fixtures", "slowpkg")

	// The floor is measured rather than picked, for the reason
	// TestRun_CatchesAGenuineHang above already measures its own (#379):
	// the tracker's clock starts when Run does, and `go test` spends real
	// time loading packages, linking and starting the binary before it
	// emits its first event. That cost sits inside the very first window,
	// so a floor smaller than it trips the watchdog on this test's own
	// setup rather than on anything the fixture did.
	//
	// That is #401 exactly, and it needs no load to reproduce: a cold
	// GOCACHE with a single-CPU compile pushes startup past 3s, the
	// control below trips at "process start" with zero events observed,
	// and it fails saying it wanted [TestB]. Three runs out of three.
	//
	// The first call compiles; the second pays exactly the startup the
	// watched runs are about to pay, on this host, right now, and the
	// floor is four times that. On a quiet machine it stays at the
	// slowpkgBaseFloor this test has always used; on a loaded one it
	// grows with the thing it has to cover. Four times, not two, so that
	// the startup gap can never be the slowest gap the window derives
	// from either: the floor dominates it by construction.
	warmBuildCache(t, dir)
	startup := warmBuildCache(t, dir)
	floor := slowpkgBaseFloor
	if measured := 4 * startup; measured > floor {
		floor = measured
	}
	t.Logf("no-progress floor %s, from a warm `go test` startup of %s on this host", floor.Round(time.Millisecond), startup.Round(time.Millisecond))

	t.Run("derived window absorbs it", func(t *testing.T) {
		stdout := &replayRecorder{}
		var stderr bytes.Buffer
		start := time.Now()
		res, err := Run(Options{
			Dir:    dir,
			Args:   []string{"-v", "-count=1", "./..."},
			Bounds: tightBounds(floor, defaultBounds.stepFactor),
			Poll:   slowpkgPoll,
			Stdout: stdout,
			Stderr: &stderr,
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Run returned an error: %v\nstderr=%s", err, stderr.String())
		}
		if res.Trip != nil && res.Trip.events == 0 {
			// The same startup story as the control below, from the
			// other side: the run tripped before `go test` emitted an
			// event, so this host's startup outgrew a floor derived to
			// be four times the startup measured moments earlier. That
			// is the machine changing under the test, not a watchdog
			// failing a progressing run.
			//
			// Unless `go test` had in fact already printed something, in
			// which case the tracker is contradicting the bytes the same
			// run replayed, and this skip would be waving through a
			// gotestwatch that counts no events at all (safety review of
			// #406). Same guard as judgeNegativeControl's, because it is
			// the same hole seen from the positive side.
			if w := witnessRun(stdout, start, elapsed, res.Trip); w.sawOutput {
				t.Fatalf("the watchdog tripped %s after %q reporting 0 events observed, but `go test` had already replayed its own output before that decision. observeLine counts an event before it writes those bytes, so this is not this host's startup outgrowing the %s floor: it is gotestwatch not counting what it printed\nstdout=%s",
					res.Trip.sinceLast.Round(time.Millisecond), res.Trip.lastEvent, floor.Round(time.Millisecond), stdout)
			}
			skipCannotMeasure(t, "the watchdog tripped %s after %q with no event observed yet, on a %s floor derived from a %s warm startup. `go test`'s own startup outgrew that floor between the warm-up and this run, so nothing here says whether a progressing run survives",
				res.Trip.sinceLast.Round(time.Millisecond), res.Trip.lastEvent, floor.Round(time.Millisecond), startup.Round(time.Millisecond))
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
		// Past this point the floor this host forced on the run is no
		// longer under the stall the fixture plants, so there is no
		// window that both clears `go test`'s startup and closes on
		// TestB's sleep. The control cannot be run here at all, and
		// saying so is the honest answer; running it anyway would report
		// on the machine.
		if floor+slowpkgPoll >= slowpkgSecondStall {
			skipCannotMeasure(t, "this host's %s warm `go test` startup derives a %s floor, which leaves less than one %s poll under TestB's %s stall. No floor separates startup from the planted stall here, so the control has nothing to trip on",
				startup.Round(time.Millisecond), floor.Round(time.Millisecond), slowpkgPoll, slowpkgSecondStall)
		}

		// The same sampler TestRun_CatchesAGenuineHang uses, for the same
		// reason: the no-trip branch of the verdict has to know how much
		// this host stretches a sleep, and a number measured during the
		// run beats one picked here.
		lag := startSchedulingLagSampler(slowpkgPoll)
		stdout := &replayRecorder{}
		var stderr bytes.Buffer
		// stepFactor 1 is the bound with its derivation switched off: the
		// window can never grow past the slowest gap measured so far, so
		// it behaves like a fixed deadline again, which is the shape of
		// go test's own -timeout that issue #256 is about.
		start := time.Now()
		res, err := Run(Options{
			Dir:    dir,
			Args:   []string{"-v", "-count=1", "./..."},
			Bounds: tightBounds(floor, 1),
			Poll:   slowpkgPoll,
			Stdout: stdout,
			Stderr: &stderr,
		})
		elapsed := time.Since(start)
		worstLag := lag.stop()
		if err != nil {
			t.Fatalf("Run returned an error: %v\nstderr=%s", err, stderr.String())
		}
		outcome, why := judgeNegativeControl(res, floor, slowpkgPoll, worstLag, witnessRun(stdout, start, elapsed, res.Trip))
		switch outcome {
		case controlProved:
			t.Log(why)
		case controlCannotMeasure:
			skipCannotMeasure(t, "%s", why)
		default:
			t.Fatalf("%s\nstdout=%s", why, stdout)
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
		witness  controlWitness
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
			witness:  controlWitness{sawOutput: true, testBInFlight: true, elapsed: 5 * time.Second},
			want:     controlProved,
			mentions: []string{"TestB"},
		},
		{
			name: "it tripped on go test's own startup, before any test ran",
			res: Result{Trip: &trip{
				kind: "no-progress", events: 0, running: nil,
				lastEvent: "process start", sinceLast: floor + 3*time.Millisecond, window: floor,
			}},
			witness:  controlWitness{elapsed: floor + 20*time.Millisecond},
			want:     controlCannotMeasure,
			mentions: []string{"process start", "no event"},
		},
		{
			// The same zero event count as the case above, and the
			// opposite verdict, because `go test` had already printed
			// something by the time the watchdog decided. observeLine
			// counts the event before it writes those bytes, so a run
			// that replayed output and reported no events is gotestwatch
			// contradicting itself, not a host too slow to start.
			name: "it counted no events, but the fixture's output was already on the wire",
			res: Result{Trip: &trip{
				kind: "no-progress", events: 0, running: nil,
				lastEvent: "process start", sinceLast: floor + 3*time.Millisecond, window: floor,
			}},
			witness:  controlWitness{sawOutput: true, elapsed: floor + 20*time.Millisecond},
			want:     controlBroken,
			mentions: []string{"0 events observed", "not counting what it printed"},
		},
		{
			name: "it tripped on a gap this host produced before TestB",
			res: Result{Trip: &trip{
				kind: "no-progress", events: 3, running: []string{"TestA"},
				lastEvent: "output TestA (=== RUN   TestA)", sinceLast: floor + 9*time.Millisecond,
				window: floor, slowestStep: 700 * time.Millisecond,
			}},
			witness:  controlWitness{sawOutput: true, elapsed: floor + 1200*time.Millisecond},
			want:     controlCannotMeasure,
			mentions: []string{"TestA"},
		},
		{
			// Same shape as the case above and the opposite verdict, for
			// the same reason: TestB had started and had not finished, so
			// whatever the tracker named, TestB was the test in flight.
			name: "it named no test at all while TestB was in flight",
			res: Result{Trip: &trip{
				kind: "no-progress", events: 7, running: nil,
				lastEvent: "output TestB (=== RUN   TestB)", sinceLast: floor + 40*time.Millisecond,
				window: floor, slowestStep: slowpkgFirstStall,
			}},
			witness:  controlWitness{sawOutput: true, testBInFlight: true, elapsed: 5 * time.Second},
			want:     controlBroken,
			mentions: []string{"TestB had started and not finished", "attribution"},
		},
		{
			name: "it named TestA while TestB was in flight",
			res: Result{Trip: &trip{
				kind: "no-progress", events: 7, running: []string{"TestA"},
				lastEvent: "output TestB (=== RUN   TestB)", sinceLast: floor + 40*time.Millisecond,
				window: floor, slowestStep: slowpkgFirstStall,
			}},
			witness:  controlWitness{sawOutput: true, testBInFlight: true, elapsed: 5 * time.Second},
			want:     controlBroken,
			mentions: []string{"TestB had started and not finished", "attribution"},
		},
		{
			name: "no trip, and nothing this host did beat the planted stall",
			res: Result{
				Events: 12, SlowestStep: slowpkgSecondStall + 3*time.Millisecond,
				SlowestLabel: "output TestB (=== RUN   TestB) -> output TestB (--- PASS: TestB (6.0",
				Window:       slowpkgSecondStall + 3*time.Millisecond,
			},
			witness:  controlWitness{sawOutput: true, elapsed: 7300 * time.Millisecond},
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
			// A 9s gap and TestB's own 6s stall are separate intervals of
			// the same timeline, so a run that really produced both took
			// at least 15s. This one took 16s, so the story holds.
			witness:  controlWitness{sawOutput: true, elapsed: 16 * time.Second},
			want:     controlCannotMeasure,
			mentions: []string{"9s", "TestA"},
		},
		{
			// The same claimed 9s gap, and the opposite verdict, because
			// the run finished in 7.3s and two disjoint gaps of 9s and 6s
			// do not fit inside 7.3s. The claim is the tracker's; the
			// clock is not.
			name: "no trip, and the slowest gap it claims does not fit in the time the run took",
			res: Result{
				Events: 12, SlowestStep: 9 * time.Second,
				SlowestLabel: "output TestA (=== RUN   TestA) -> output TestA (--- PASS: TestA (1.0",
				Window:       9 * time.Second,
			},
			witness:  controlWitness{sawOutput: true, elapsed: 7300 * time.Millisecond},
			want:     controlBroken,
			mentions: []string{"misreporting itself", "7.3s"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, why := judgeNegativeControl(tc.res, floor, slowpkgPoll, 20*time.Millisecond, tc.witness)
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

// TestSkipLedger_ASkipIsNeverTheSameColourAsAPass is the second half of
// the safety review's finding against #406, and the half that matters
// more. Narrowing the skip stops real defects landing in it; this stops a
// skip that IS legitimate from being read as a pass.
//
// The vocabulary is scripts/lib/ci-local-gate.sh's, not a new one: ok is
// the status the run already had, INCOMPLETE is gateIncomplete, and an
// actual failure outranks both.
func TestSkipLedger_ASkipIsNeverTheSameColourAsAPass(t *testing.T) {
	tests := []struct {
		name     string
		notes    []string
		code     int
		want     int
		mentions []string
		absent   []string
	}{
		{
			name:   "everything ran, so the run keeps its own verdict and says nothing extra",
			code:   0,
			want:   0,
			absent: []string{"INCOMPLETE"},
		},
		{
			name:     "a skip is not a pass",
			notes:    []string{"the negative control: this host's 2s startup left no floor under the stall"},
			code:     0,
			want:     gateIncomplete,
			mentions: []string{"INCOMPLETE", "no floor under the stall", "not evidence"},
		},
		{
			name:     "a real failure outranks INCOMPLETE, and the skip is still named",
			notes:    []string{"the negative control: this host's 2s startup left no floor under the stall"},
			code:     1,
			want:     1,
			mentions: []string{"INCOMPLETE", "no floor under the stall"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var l skipLedger
			for _, n := range tc.notes {
				l.note(n)
			}
			var out bytes.Buffer
			got := l.verdict(tc.code, &out)
			if got != tc.want {
				t.Fatalf("verdict(%d) = %d, want %d; %d skipped check(s) were recorded and the run reported %d, so a reader of the exit status cannot tell this from a run that performed everything\nsaid: %s",
					tc.code, got, tc.want, len(tc.notes), got, out.String())
			}
			for _, want := range tc.mentions {
				if !strings.Contains(out.String(), want) {
					t.Errorf("the verdict does not mention %q, so the skip is invisible to anyone reading the run:\n%s", want, out.String())
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(out.String(), unwanted) {
					t.Errorf("the verdict says %q on a run that skipped nothing:\n%s", unwanted, out.String())
				}
			}
		})
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
