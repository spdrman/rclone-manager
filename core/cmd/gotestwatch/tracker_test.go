package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The tracker's arithmetic, proved against a synthetic clock.
//
// Every bound this tool has is derived from what the run itself has already
// been measured doing, so the cases below are all the same shape: feed
// events at chosen instants, then ask whether the window is open or closed
// at another chosen instant. Nothing sleeps, which is what makes it possible
// to check the interesting cases at all. Demonstrating a livelock or a
// widened-then-closed window against a real clock would cost minutes per
// case and would still only sample the behaviour.
//
// Two of the cells are about the ways this design fails rather than the ways
// it works: an outlier must not inflate the window permanently, or one slow
// package at the start of a run buys every later hang an unlimited budget,
// and a package- or build-level event carries no test name, which is the
// shape that panics if the tracker assumes every event names a test.

var testBounds = bounds{
	stepFloor:     45 * time.Second,
	stepFactor:    12,
	overallFloor:  4 * time.Minute,
	overallFactor: 40,
}

func TestTracker_TripsAtTheFloorBeforeAnythingIsMeasured(t *testing.T) {
	start := time.Now()
	tr := newTracker(testBounds, start)

	floor := testBounds.stepFloor
	if trip := tr.check(start.Add(floor - time.Millisecond)); trip != nil {
		t.Fatalf("tripped %s into a run, one millisecond before the %s floor: %v", floor-time.Millisecond, floor, trip)
	}
	trip := tr.check(start.Add(floor + time.Millisecond))
	if trip == nil {
		t.Fatalf("a run that reported nothing at all for %s was not caught", floor+time.Millisecond)
	}
	if trip.kind != "no-progress" {
		t.Fatalf("trip.kind = %q, want no-progress: %v", trip.kind, trip)
	}
	if trip.lastEvent != "process start" {
		t.Fatalf("trip.lastEvent = %q, want the run's own starting point", trip.lastEvent)
	}
}

func TestTracker_WidensWithTheSlowestGapItHasSeen(t *testing.T) {
	start := time.Now()
	tr := newTracker(testBounds, start)

	// A machine loaded enough that four consecutive tests each take ten
	// seconds. Under go test's fixed default that is still far under 10m,
	// but under a tighter fixed -timeout it would already be trouble; here
	// the same evidence widens the window instead.
	at := start
	for i, name := range []string{"TestA", "TestB", "TestC", "TestD"} {
		at = at.Add(10 * time.Second)
		tr.observe(testEvent{Action: "pass", Test: name}, at)
		if trip := tr.check(at); trip != nil {
			t.Fatalf("event %d (%s) was reported as a hang: %v", i, name, trip)
		}
	}

	if got, want := tr.window(), 120*time.Second; got != want {
		t.Fatalf("window after four ten-second gaps = %s, want %s (stepFactor x the slowest gap)", got, want)
	}
	if trip := tr.check(at.Add(40 * time.Second)); trip != nil {
		t.Fatalf("40s of silence after gaps measured at 10s each was called a hang: %v", trip)
	}
}

func TestTracker_ASlowOutlierDoesNotPermanentlyInflateTheWindow(t *testing.T) {
	// Reproduces the review finding on issue #256: under an all-time
	// running maximum, one legitimately slow (not hung) step anywhere in
	// a run permanently inflated both derived bounds for the rest of
	// that run. This plants one ~60s step (the reviewer's own worked
	// example: "a ~60s legitimate outlier inflates the window to 12
	// minutes and the cap to 40 minutes"), then enough normal-paced
	// activity to retire it from slowestStepMemory, then a genuine hang
	// — which must still be caught within a bounded, practical time, not
	// the twelve minutes the stale maximum would have allowed.
	start := time.Now()
	tr := newTracker(testBounds, start)

	at := start
	// One legitimately slow step: a Docker pull under concurrent host
	// load, say. It completes and reports progress — it is not a hang.
	at = at.Add(60 * time.Second)
	tr.observe(testEvent{Action: "pass", Test: "TestSlowDockerPull"}, at)
	if trip := tr.check(at); trip != nil {
		t.Fatalf("the legitimately slow step itself was reported as a hang: %v", trip)
	}

	// Enough further normal-paced (10s) activity for the outlier to
	// fully leave slowestStepMemory: exactly slowestStepMemory more
	// events overwrites every ring-buffer slot, including the one the
	// outlier occupies.
	for i := 0; i < slowestStepMemory; i++ {
		at = at.Add(10 * time.Second)
		tr.observe(testEvent{Action: "pass", Test: fmt.Sprintf("TestNormal%d", i)}, at)
		if trip := tr.check(at); trip != nil {
			t.Fatalf("normal-paced event %d was reported as a hang: %v", i, trip)
		}
	}

	// The outlier is gone from memory: both bounds now reflect the
	// recent 10s pace (matching TestTracker_WidensWithTheSlowestGapItHasSeen's
	// own numbers), not the stale 60s maximum. Under an all-time running
	// maximum these would still read 720s and 2400s.
	if got, want := tr.window(), 120*time.Second; got != want {
		t.Fatalf("window after the outlier decayed out = %s, want %s (the outlier must not still be inflating it)", got, want)
	}
	if got, want := tr.overallCap(), 400*time.Second; got != want {
		t.Fatalf("overall cap after the outlier decayed out = %s, want %s (the outlier must not still be inflating it)", got, want)
	}

	// Now a genuine hang: nothing after the last event for just over the
	// now-decayed window. Under an all-time running maximum this silence
	// (~121s) would have been far short of the stale 720s window and
	// gone completely undetected.
	if trip := tr.check(at.Add(119 * time.Second)); trip != nil {
		t.Fatalf("tripped one second inside the decayed window: %v", trip)
	}
	trip := tr.check(at.Add(121 * time.Second))
	if trip == nil {
		t.Fatal("a genuine hang after the outlier decayed out was not caught within the decayed window; the outlier is still permanently inflating the bound")
	}
	if trip.kind != "no-progress" {
		t.Fatalf("trip.kind = %q, want no-progress: %v", trip.kind, trip)
	}
}

func TestTracker_AWidenedWindowStillCloses(t *testing.T) {
	start := time.Now()
	tr := newTracker(testBounds, start)

	at := start.Add(10 * time.Second)
	tr.observe(testEvent{Action: "run", Test: "TestSlow"}, at)

	if trip := tr.check(at.Add(119 * time.Second)); trip != nil {
		t.Fatalf("tripped one second inside the derived window: %v", trip)
	}
	trip := tr.check(at.Add(121 * time.Second))
	if trip == nil {
		t.Fatal("a run that went quiet for more than its derived window was never caught, so the bound is unbounded")
	}
	if trip.kind != "no-progress" || !strings.Contains(trip.lastEvent, "TestSlow") {
		t.Fatalf("trip = %+v, want a no-progress trip naming TestSlow", *trip)
	}
	if !strings.Contains(trip.String(), "TestSlow") {
		t.Fatalf("the failure text does not name the test the run got stuck at:\n%v", trip)
	}
}

func TestTracker_OverallCapCatchesARunThatNeverFinishes(t *testing.T) {
	start := time.Now()
	tr := newTracker(testBounds, start)

	// Fast events forever: something that keeps reporting activity but
	// never actually finishes. No no-progress window can ever see that;
	// the overall cap is what catches it, and it stays at the floor
	// because the events themselves stay fast.
	//
	// Catching it is the property, and it is unchanged by issue #533.
	// What changed is the sentence: this used to be called a livelock
	// outright, and a livelock is only one of the things that produces
	// exactly this stream (see
	// TestTracker_ALivelockAndALoadedHostProduceTheSameEvidence). So what
	// is asserted here is that the kill still happens and that the report
	// still carries the measurements behind it, without a cause attached.
	at := start
	for at.Sub(start) < testBounds.overallFloor+time.Second {
		at = at.Add(50 * time.Millisecond)
		tr.observe(testEvent{Action: "output", Test: "TestLoop"}, at)
	}
	trip := tr.check(at)
	if trip == nil {
		t.Fatalf("a run reporting fast progress forever ran for %s and was never caught", at.Sub(start))
	}
	if trip.kind != "overall" {
		t.Fatalf("trip.kind = %q, want overall: %v", trip.kind, trip)
	}
	msg := trip.String()
	if phrase := namesACause(msg); phrase != "" {
		t.Fatalf("the failure text says %q, naming a cause it cannot distinguish from a loaded host:\n%v", phrase, trip)
	}
	for _, want := range []string{
		trip.elapsed.Round(time.Millisecond).String(),    // how long it ran
		trip.overallCap.Round(time.Millisecond).String(), // what it ran against
		"TestLoop", // and what it was doing at the time
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the failure text does not report %q, so a reader has nothing to work from now that it names no cause:\n%s", want, msg)
		}
	}
}

func TestTracker_NamesTestsStillRunning(t *testing.T) {
	start := time.Now()
	tr := newTracker(testBounds, start)

	at := start
	tr.observe(testEvent{Action: "run", Test: "TestA"}, at)
	at = at.Add(time.Second)
	tr.observe(testEvent{Action: "run", Test: "TestB"}, at)
	at = at.Add(time.Second)
	tr.observe(testEvent{Action: "pass", Test: "TestA"}, at)

	trip := tr.check(at.Add(testBounds.stepFloor + time.Second))
	if trip == nil {
		t.Fatal("expected a no-progress trip")
	}
	if len(trip.running) != 1 || trip.running[0] != "TestB" {
		t.Fatalf("trip.running = %v, want exactly [TestB] (TestA already passed)", trip.running)
	}
	if !strings.Contains(trip.String(), "TestB") {
		t.Fatalf("the failure text does not name the test still running:\n%v", trip)
	}
}

func TestTracker_PackageAndBuildEventsWithNoTestNameDoNotPanic(t *testing.T) {
	start := time.Now()
	tr := newTracker(testBounds, start)
	tr.observe(testEvent{Action: "start", Package: "example.com/pkg"}, start.Add(time.Second))
	tr.observe(testEvent{Action: "build-output"}, start.Add(2*time.Second))
	if trip := tr.check(start.Add(3 * time.Second)); trip != nil {
		t.Fatalf("unexpected trip: %v", trip)
	}
}

// Issue #533: the gap stream cannot say why a run never finished, so the
// trip must not pretend it can.
//
// The overall cap used to end its sentence with "That is a livelock, not a
// slow machine: the cap is derived from this run's own recent pace, so a
// genuinely, consistently slow run would have widened it." That reasoning
// holds against a machine that is uniformly slow and fails against one
// that is bursty, which is what killed a real gate run: a quiet stretch
// set a fast pace, the cap tightened to it, and one burst of load from
// another project on the same machine pushed the run past a cap that had
// been set while nothing was competing. The test it named passed in 8.156s
// on its own minutes later.
//
// The cases below are the evidence for removing the claim rather than
// rewording it, and for what replaced it.

// namesACause reports the phrase, if any, by which msg tells its reader
// what caused the trip.
//
// The defect issue #533 filed is not the word "livelock". The honest
// sentence still uses it, as one possibility among several, and a reader
// who has genuinely livelocked their suite needs to see it there. The
// defect is the assertion: a specific, confident cause picked out of
// evidence that cannot distinguish it from the others. So this looks for
// the assertive shapes, which are the two this code really carried plus
// their near neighbours.
func namesACause(msg string) string {
	for _, phrase := range []string{
		"is a livelock",
		"was a livelock",
		"is a hang,",
		"is a hang.",
		"not a slow machine",
		"not a loaded machine",
	} {
		if strings.Contains(msg, phrase) {
			return phrase
		}
	}
	return ""
}

// gapStream is one run's worth of arrivals: the sequence of gaps between
// consecutive events, and a name for the cause that produced it.
type gapStream struct {
	cause string
	gaps  []time.Duration
}

// feed replays a stream into a tracker, one event per gap, and returns the
// instant the last event landed.
func (s gapStream) feed(tr *tracker, start time.Time) time.Time {
	at := start
	for i, g := range s.gaps {
		at = at.Add(g)
		tr.observe(testEvent{Action: "output", Test: "TestWork", Detail: fmt.Sprintf("line %d", i)}, at)
	}
	return at
}

func (s gapStream) total() time.Duration {
	var sum time.Duration
	for _, g := range s.gaps {
		sum += g
	}
	return sum
}

// spinningLivelock is a test that loops forever and prints a line each
// time round: real output, at a steady pace, with no completion ever
// coming. That is the shape the overall cap exists to catch, and it is
// what tests/crashmatrix's own progressTracker comment describes as "a
// resume loop that never advances a state".
func spinningLivelock(pace, until time.Duration) gapStream {
	s := gapStream{cause: "a test looping forever, printing one line per iteration"}
	for sum := time.Duration(0); sum < until; sum += pace {
		s.gaps = append(s.gaps, pace)
	}
	return s
}

// starvedButProgressing is a real suite with plenty of work still ahead of
// it, on a host that hands this process one slice every `pace`. Every
// event is a different test finishing; nothing is stuck. From outside the
// process it produces the identical arrival pattern to spinningLivelock,
// which is the whole finding.
func starvedButProgressing(pace, until time.Duration) gapStream {
	s := gapStream{cause: "a suite with work left, on a host giving it one slice every " + pace.String()}
	for sum := time.Duration(0); sum < until; sum += pace {
		s.gaps = append(s.gaps, pace)
	}
	return s
}

// degradingLivelock is the shape issue #533 wondered about: a livelock
// whose gaps grow and never recover, a retry loop backing off, say.
func degradingLivelock(first, step time.Duration, n int) gapStream {
	s := gapStream{cause: "a retry loop that never succeeds, backing off as it goes"}
	for i := 0; i < n; i++ {
		s.gaps = append(s.gaps, first+time.Duration(i)*step)
	}
	return s
}

// degradingHost is the same shape from the other cause: a run that is
// making real progress the whole time on a machine another job is taking
// over, so its gaps grow and never recover either.
func degradingHost(first, step time.Duration, n int) gapStream {
	s := gapStream{cause: "a progressing run on a machine another job is ramping up on"}
	for i := 0; i < n; i++ {
		s.gaps = append(s.gaps, first+time.Duration(i)*step)
	}
	return s
}

func TestTracker_ALivelockAndALoadedHostProduceTheSameEvidence(t *testing.T) {
	// Two pairs, each built from two different causes rather than copied
	// from one another. Within a pair the tracker ends up holding exactly
	// the same trip, so any verdict it prints is the same sentence for
	// both, and at most one of the two can be true. That is why the fix
	// is to stop naming a cause, not to name a different one.
	//
	// The second pair is the specific idea worth ruling out: "a
	// livelock's gaps grow without recovering, a burst's recover". Gaps
	// that grow and never recover are equally what a host being taken
	// over by another job does to a run that is progressing perfectly
	// well, so the rule would have fired on exactly the case issue #533
	// was filed about, and it would have missed the fast-spinning
	// livelock in the first pair, whose gaps never grow at all.
	for _, pair := range [][2]gapStream{
		{
			spinningLivelock(50*time.Millisecond, testBounds.overallFloor+time.Second),
			starvedButProgressing(50*time.Millisecond, testBounds.overallFloor+time.Second),
		},
		{
			degradingLivelock(10*time.Millisecond, 2*time.Millisecond, 700),
			degradingHost(10*time.Millisecond, 2*time.Millisecond, 700),
		},
	} {
		start := time.Now()

		trA := newTracker(testBounds, start)
		atA := pair[0].feed(trA, start)
		tripA := trA.check(atA)

		trB := newTracker(testBounds, start)
		atB := pair[1].feed(trB, start)
		tripB := trB.check(atB)

		if tripA == nil || tripB == nil {
			t.Fatalf("one of the two streams was not caught at all (%q: %v, %q: %v); both run past the cap, so both have to trip for the comparison to mean anything",
				pair[0].cause, tripA, pair[1].cause, tripB)
		}
		if tripA.kind != "overall" || tripB.kind != "overall" {
			t.Fatalf("kinds %q and %q, want both overall: these streams never go quiet, so the overall cap is what has to catch them", tripA.kind, tripB.kind)
		}
		if !reflect.DeepEqual(tripA, tripB) {
			t.Fatalf("the tracker holds different evidence for %q and %q:\n  %+v\n  %+v", pair[0].cause, pair[1].cause, *tripA, *tripB)
		}
		if tripA.String() != tripB.String() {
			t.Fatalf("the same evidence produced two different sentences:\n  %q\n  %q", tripA.String(), tripB.String())
		}
		if phrase := namesACause(tripA.String()); phrase != "" {
			t.Fatalf("the trip says %q, but %q and %q left the watchdog holding identical evidence, so the sentence it prints is the same for both and cannot be true of both:\n%v",
				phrase, pair[0].cause, pair[1].cause, tripA)
		}
	}
}

// burstyHost is issue #533's own shape: quiet stretches that set a fast
// pace, one stall each time another project on the machine takes the CPU,
// then recovery back to the same fast pace. The quiet stretch after each
// burst is deliberately longer than slowestStepMemory, so by the time the
// cap closes the recent window has rolled every stall out of memory and
// the cap is back at its unmeasured floor, exactly as if the run had been
// quick and smooth throughout. That is the blind spot: the run's own
// record says it has been stalled repeatedly, and the bound the run is
// killed against cannot see any of it.
func burstyHost(pace, burst, until time.Duration) gapStream {
	s := gapStream{cause: "a progressing run on a machine another project keeps taking the CPU on"}
	var sum time.Duration
	quiet := func() {
		for i := 0; i < 3*slowestStepMemory; i++ {
			s.gaps = append(s.gaps, pace)
			sum += pace
		}
	}
	quiet()
	for sum < until {
		s.gaps = append(s.gaps, burst)
		sum += burst
		quiet()
	}
	return s
}

func TestTracker_ABurstyHostIsNotReportedAsALivelock(t *testing.T) {
	start := time.Now()
	tr := newTracker(testBounds, start)

	stream := burstyHost(50*time.Millisecond, 4*time.Second, testBounds.overallFloor)
	at := stream.feed(tr, start)

	trip := tr.check(at)
	if trip == nil {
		t.Fatalf("a run that spent %s being stalled and restarted was never caught at all", stream.total().Round(time.Millisecond))
	}
	if trip.kind != "overall" {
		t.Fatalf("trip.kind = %q, want overall: nothing here goes quiet for longer than the no-progress window, so the cap is what closes: %v", trip.kind, trip)
	}

	// The mechanism, before the sentence: the cap this run was killed
	// against is the floor, derived from a recent window in which every
	// gap is the quiet 50ms pace, while the run as a whole has been
	// stalled for four seconds at a time over and over.
	if trip.slowestStep > time.Second {
		t.Fatalf("the recent window still holds a %s gap, so the bursts had not rolled out of it and this fixture is not reproducing the blind spot", trip.slowestStep)
	}
	if trip.runSlowest < 4*time.Second {
		t.Fatalf("the run's slowest gap is reported as %s, but this stream stalled for 4s at a time; the whole-run measurement is not being kept", trip.runSlowest)
	}

	if phrase := namesACause(trip.String()); phrase != "" {
		t.Fatalf("a run stalled repeatedly by another job on the host was told it %q:\n%v", phrase, trip)
	}
	// Removing the claim is only half of it. What replaces it has to say
	// what was actually seen, and the stalls are the part a reader needs.
	if !strings.Contains(trip.String(), trip.runSlowest.Round(time.Millisecond).String()) {
		t.Fatalf("the trip does not report the %s stall this run had already been through, which is the observation that argues against a livelock:\n%v", trip.runSlowest, trip)
	}
}

func TestTrip_ReportsWhatTheHostDidToTheWatchdogItself(t *testing.T) {
	// The one reading gotestwatch has on the host is what the host did to
	// gotestwatch: its watchdog loop asks to run on a fixed interval and
	// can measure how late it was actually run. run.go fills these in
	// (tracker.go stays free of process concerns), so they are set
	// directly here.
	tr := trip{
		kind:         "overall",
		lastEvent:    "output TestPhase1Gate",
		elapsed:      4 * time.Minute,
		overallCap:   4 * time.Minute,
		events:       4000,
		slowestStep:  50 * time.Millisecond,
		runSlowest:   4 * time.Second,
		pollInterval: 250 * time.Millisecond,
		worstPollLag: 812 * time.Millisecond,
	}
	got := tr.String()
	for _, want := range []string{"250ms", "812ms"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the trip does not report %s, so an operator cannot see what the host was doing to a process that only wanted a turn:\n%s", want, got)
		}
	}

	// And a run where nothing measured the loop at all must not invent a
	// reading of zero, which would read as "the host was idle".
	unmeasured := tr
	unmeasured.pollInterval = 0
	unmeasured.worstPollLag = 0
	if strings.Contains(unmeasured.String(), "watchdog loop") {
		t.Fatalf("a trip with no poll measurement still talks about the watchdog loop, which would read as a measurement of zero:\n%s", unmeasured.String())
	}
}
