// This file is gotestwatch's pure decision-making core (see doc.go for
// what the tool is and why it exists), deliberately kept free of any
// process or I/O concerns so it can be proved against a synthetic clock
// (tracker_test.go) the same way tests/crashmatrix's own progressTracker
// is in crash_matrix_test.go, which this is modeled on.
package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// testEvent is the subset of `go test -json`'s per-line schema (see the
// standard library's cmd/internal/test2json) gotestwatch actually acts on.
// Action is one of "start", "run", "pause", "cont", "bench", "pass",
// "fail", "skip", "output", "build-output" or "build-fail"; Test is empty
// for package- and build-level events. Detail is a short, trimmed snippet
// of Output for the two actions that carry it, used only so two different
// output lines belonging to the same test (its "=== RUN" framing and its
// "--- PASS" one, say) do not produce an identical, uninformative label.
type testEvent struct {
	Action  string
	Package string
	Test    string
	Detail  string
}

// label names an event for humans: the test it belongs to when there is
// one, else the package, else just the action (build-output/build-fail
// carry neither), plus a snippet of what was actually printed for output
// events specifically, since a run's slowest gap is very often between two
// output lines of the very same test.
func (e testEvent) label() string {
	name := e.Action
	switch {
	case e.Test != "":
		name = e.Action + " " + e.Test
	case e.Package != "":
		name = e.Action + " " + e.Package
	}
	if e.Detail != "" {
		return name + " (" + e.Detail + ")"
	}
	return name
}

// bounds mirrors tests/crashmatrix's harnessBounds (issue #247): a
// no-progress window and an overall backstop, both derived from the
// slowest step this run has itself measured, with a floor used only until
// the first step completes. See doc.go for the reasoning; the numbers
// themselves are chosen in main.go.
type bounds struct {
	stepFloor     time.Duration
	stepFactor    float64
	overallFloor  time.Duration
	overallFactor float64
}

// trip is what a bound being exceeded produces. It carries the
// measurements the decision was made from, and which tests were actually
// in flight, because the whole complaint in issue #256 (like #247 and #248
// before it) is that a gate failure that does not show its working teaches
// people to re-run rather than read.
type trip struct {
	kind         string // "no-progress" or "overall"
	lastEvent    string
	sinceLast    time.Duration
	window       time.Duration
	elapsed      time.Duration
	overallCap   time.Duration
	events       int
	slowestStep  time.Duration
	slowestLabel string
	running      []string // tests with a "run" seen but no pass/fail/skip yet, sorted
}

func (tr trip) String() string {
	running := "no test was reported as running, so the last thing observed at all is named above"
	if len(tr.running) > 0 {
		running = "test(s) still reported running: " + strings.Join(tr.running, ", ")
	}
	measured := fmt.Sprintf("%d events observed, slowest gap %s (%s)", tr.events, tr.slowestStep.Round(time.Millisecond), tr.slowestLabel)
	if tr.events == 0 {
		measured = "no event had arrived yet, so the window was still the unmeasured floor"
	}
	switch tr.kind {
	case "overall":
		return fmt.Sprintf("go test kept reporting progress but never finished: %s elapsed against a cap of %s, last event %q. %s. %s. "+
			"That is a livelock, not a slow machine: the cap is derived from this run's own slowest gap, so a genuinely slow run would have widened it.",
			tr.elapsed.Round(time.Millisecond), tr.overallCap.Round(time.Millisecond), tr.lastEvent, running, measured)
	default:
		return fmt.Sprintf("go test stopped making progress: nothing after %q for %s, against a no-progress window of %s (%s elapsed in total). %s. %s. "+
			"This is a hang, not a slow machine: the window is derived from this run's own slowest gap, so being slow widens it and only being stuck trips it.",
			tr.lastEvent, tr.sinceLast.Round(time.Millisecond), tr.window.Round(time.Millisecond), tr.elapsed.Round(time.Millisecond), running, measured)
	}
}

// tracker turns the `go test -json` event stream into the two derived
// bounds above, plus the set of tests currently in flight. It is a plain
// value with an explicit clock passed to observe/check, exactly like
// tests/crashmatrix's progressTracker, so it can be proved at millisecond
// scale without sleeping and without a subprocess.
type tracker struct {
	b bounds

	mu           sync.Mutex
	start        time.Time
	lastAt       time.Time
	lastEvent    string
	events       int
	slowestStep  time.Duration
	slowestLabel string
	running      map[string]struct{}
}

func newTracker(b bounds, start time.Time) *tracker {
	return &tracker{b: b, start: start, lastAt: start, lastEvent: "process start", running: map[string]struct{}{}}
}

// observe records one JSON event, at the wall-clock instant gotestwatch
// itself received it (not the timestamp the event carries): what a
// watchdog can act on is what it can observe, and using the child's own
// clock would let a delayed read look like a gap that never happened.
func (t *tracker) observe(ev testEvent, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	label := ev.label()
	if d := at.Sub(t.lastAt); d > t.slowestStep {
		t.slowestStep = d
		t.slowestLabel = t.lastEvent + " -> " + label
	}
	t.lastAt = at
	t.lastEvent = label
	t.events++

	if ev.Test == "" {
		return
	}
	switch ev.Action {
	case "run":
		t.running[ev.Test] = struct{}{}
	case "pass", "fail", "skip":
		delete(t.running, ev.Test)
	}
}

// window is the current no-progress bound. Callers hold t.mu.
func (t *tracker) window() time.Duration {
	if derived := time.Duration(float64(t.slowestStep) * t.b.stepFactor); derived > t.b.stepFloor {
		return derived
	}
	return t.b.stepFloor
}

// overallCap is the current total-runtime backstop. Callers hold t.mu.
func (t *tracker) overallCap() time.Duration {
	if derived := time.Duration(float64(t.slowestStep) * t.b.overallFactor); derived > t.b.overallFloor {
		return derived
	}
	return t.b.overallFloor
}

func (t *tracker) runningNames() []string {
	names := make([]string, 0, len(t.running))
	for name := range t.running {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// check reports the bound that has been exceeded as of now, or nil if the
// run is still within both.
func (t *tracker) check(now time.Time) *trip {
	t.mu.Lock()
	defer t.mu.Unlock()

	tr := trip{
		lastEvent:    t.lastEvent,
		sinceLast:    now.Sub(t.lastAt),
		window:       t.window(),
		elapsed:      now.Sub(t.start),
		overallCap:   t.overallCap(),
		events:       t.events,
		slowestStep:  t.slowestStep,
		slowestLabel: t.slowestLabel,
		running:      t.runningNames(),
	}
	switch {
	case tr.sinceLast > tr.window:
		tr.kind = "no-progress"
		return &tr
	case tr.elapsed > tr.overallCap:
		tr.kind = "overall"
		return &tr
	}
	return nil
}

// summary is what the run measured itself at, safe to call from outside
// the watching goroutine at any point.
func (t *tracker) summary() (events int, slowest time.Duration, label string, window time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.events, t.slowestStep, t.slowestLabel, t.window()
}
