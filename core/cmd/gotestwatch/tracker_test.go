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
package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

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

func TestTracker_OverallCapCatchesALivelock(t *testing.T) {
	start := time.Now()
	tr := newTracker(testBounds, start)

	// Fast events forever: something that keeps reporting activity but
	// never actually finishes. No no-progress window can ever see that;
	// the overall cap is what catches it, and it stays at the floor
	// because the events themselves stay fast.
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
	if !strings.Contains(trip.String(), "livelock") {
		t.Fatalf("the failure text does not say what kind of failure this is:\n%v", trip)
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
