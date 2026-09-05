package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file is gotestwatch's pure decision-making core (see doc.go for
// what the tool is and why it exists), deliberately kept free of any
// process or I/O concerns so it can be proved against a synthetic clock
// (tracker_test.go) the same way tests/crashmatrix's own progressTracker
// is in crash_matrix_test.go, which this is modeled on.

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
// slowest of the run's recently measured steps (see slowestStepMemory:
// deliberately NOT an all-time maximum, so one early outlier cannot
// permanently inflate either bound for the rest of the run), with a
// floor used only until the first step completes. See doc.go for the
// reasoning; the numbers themselves are chosen in main.go.
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

	// reapTimedOut and reapWait are set by run.go, not by anything in
	// this file (tracker.go stays free of process/I/O concerns; see the
	// package doc comment at the top of this file), when the process
	// group a trip sent SIGKILL to did not actually exit within
	// reapWait. SIGKILL cannot end a process stuck in uninterruptible
	// kernel I/O wait, a real possibility for exactly the class of hang
	// this tool targets (stuck Docker/SFTP I/O); a trip that has already
	// correctly diagnosed the problem is still reported when that
	// happens, rather than run.go blocking indefinitely on a reap that
	// may never come.
	reapTimedOut bool
	reapWait     time.Duration
}

// String is the sentence a person reads when this tool kills a run, and it
// is where the whole design either pays off or does not.
//
// A watchdog that says only "timed out" leaves the reader with the same
// question a fixed -timeout leaves them: was this stuck, or just slow? So each
// message names what was still running, how many events had been observed,
// and what the bound was derived from. The overall case says outright that a
// consistently slow run would have widened its own cap, because that is the
// inference the reader has to make and it is not obvious from a number.
func (tr trip) String() string {
	running := "no test was reported as running, so the last thing observed at all is named above"
	if len(tr.running) > 0 {
		running = "test(s) still reported running: " + strings.Join(tr.running, ", ")
	}
	measured := fmt.Sprintf("%d events observed, slowest recent gap %s (%s)", tr.events, tr.slowestStep.Round(time.Millisecond), tr.slowestLabel)
	if tr.events == 0 {
		measured = "no event had arrived yet, so the window was still the unmeasured floor"
	}
	var out string
	switch tr.kind {
	case "overall":
		out = fmt.Sprintf("go test kept reporting progress but never finished: %s elapsed against a cap of %s, last event %q. %s. %s. "+
			"That is a livelock, not a slow machine: the cap is derived from this run's own recent pace, so a genuinely, consistently slow run would have widened it.",
			tr.elapsed.Round(time.Millisecond), tr.overallCap.Round(time.Millisecond), tr.lastEvent, running, measured)
	default:
		out = fmt.Sprintf("go test stopped making progress: nothing after %q for %s, against a no-progress window of %s (%s elapsed in total). %s. %s. "+
			"This is a hang, not a slow machine: the window is derived from this run's own recent pace, so being consistently slow widens it and only being stuck trips it.",
			tr.lastEvent, tr.sinceLast.Round(time.Millisecond), tr.window.Round(time.Millisecond), tr.elapsed.Round(time.Millisecond), running, measured)
	}
	if tr.reapTimedOut {
		out += fmt.Sprintf(" The killed process group had still not exited %s after being sent SIGKILL (likely stuck in uninterruptible I/O, which SIGKILL cannot end); reporting this trip anyway rather than waiting on the reap indefinitely.",
			tr.reapWait.Round(time.Second))
	}
	return out
}

// slowestStepMemory bounds how many of the most recently observed gaps
// contribute to the "slowest step" both derived bounds are multiples of.
// A rolling max over a fixed-size window, not an all-time running
// maximum: an all-time maximum meant one legitimately slow (not hung)
// step anywhere in the run — a Docker pull under concurrent host load,
// say — permanently inflated both the no-progress window and the overall
// livelock cap for the rest of that same `go test` invocation, letting a
// genuine hang occurring afterward go undetected far longer than the
// fixed timeout this tool replaced (found in review of issue #256; see
// TestTracker_ASlowOutlierDoesNotPermanentlyInflateTheWindow). Bounding
// the memory to the most recent slowestStepMemory gaps means a single
// outlier decays out once enough further events establish the run is
// genuinely back to a normal pace, while a run that is consistently slow
// — every recent gap large, not just one — still keeps a wide window,
// which is the whole point of deriving these bounds from the run's own
// measured pace at all. 20 is a plain, round choice: large enough that a
// handful of naturally slower steps in a row (a build step, then its
// linked tests) don't each look like a fresh isolated outlier, small
// enough that a single anomaly is gone from memory within seconds of
// normal-paced activity resuming.
const slowestStepMemory = 20

// tracker turns the `go test -json` event stream into the two derived
// bounds above, plus the set of tests currently in flight. It is a plain
// value with an explicit clock passed to observe/check, exactly like
// tests/crashmatrix's progressTracker, so it can be proved at millisecond
// scale without sleeping and without a subprocess.
type tracker struct {
	b bounds

	mu        sync.Mutex
	start     time.Time
	lastAt    time.Time
	lastEvent string
	events    int

	// recentGaps and recentLabels are a fixed-size ring buffer of the
	// most recently observed gaps (see slowestStepMemory) and what each
	// one was between; recentSlowest derives the current "slowest step"
	// from their max, rather than from an ever-growing all-time running
	// one. recentHead is the index the NEXT gap is written to; recentLen
	// is how many of the slowestStepMemory slots are populated so far
	// (less than its capacity until the run has observed that many
	// gaps).
	recentGaps   [slowestStepMemory]time.Duration
	recentLabels [slowestStepMemory]string
	recentHead   int
	recentLen    int

	running map[string]struct{}
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
	if d := at.Sub(t.lastAt); d > 0 {
		t.recentGaps[t.recentHead] = d
		t.recentLabels[t.recentHead] = t.lastEvent + " -> " + label
		t.recentHead = (t.recentHead + 1) % slowestStepMemory
		if t.recentLen < slowestStepMemory {
			t.recentLen++
		}
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

// recentSlowest returns the largest of the most recently observed gaps
// (see slowestStepMemory) and the label describing what it was between.
// Callers hold t.mu.
func (t *tracker) recentSlowest() (time.Duration, string) {
	var slowest time.Duration
	var label string
	for i := 0; i < t.recentLen; i++ {
		if t.recentGaps[i] > slowest {
			slowest = t.recentGaps[i]
			label = t.recentLabels[i]
		}
	}
	return slowest, label
}

// window is the current no-progress bound. Callers hold t.mu.
func (t *tracker) window() time.Duration {
	slowest, _ := t.recentSlowest()
	if derived := time.Duration(float64(slowest) * t.b.stepFactor); derived > t.b.stepFloor {
		return derived
	}
	return t.b.stepFloor
}

// overallCap is the current total-runtime backstop. Callers hold t.mu.
func (t *tracker) overallCap() time.Duration {
	slowest, _ := t.recentSlowest()
	if derived := time.Duration(float64(slowest) * t.b.overallFactor); derived > t.b.overallFloor {
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

	slowest, slowestLabel := t.recentSlowest()
	tr := trip{
		lastEvent:    t.lastEvent,
		sinceLast:    now.Sub(t.lastAt),
		window:       t.window(),
		elapsed:      now.Sub(t.start),
		overallCap:   t.overallCap(),
		events:       t.events,
		slowestStep:  slowest,
		slowestLabel: slowestLabel,
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
	slowest, label = t.recentSlowest()
	return t.events, slowest, label, t.window()
}
