package main

import (
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
