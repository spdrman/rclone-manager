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

	// runSlowest and runSlowestLabel are the slowest gap of the WHOLE
	// run and what it was between, which is a different number from
	// slowestStep above: that one is the slowest of the most recent
	// slowestStepMemory gaps, and it is the one both bounds are derived
	// from. Nothing is derived from these two. They are here because
	// issue #533 is about a run being killed against a cap that had
	// tightened back to its floor while the run's own record said it had
	// been stalled for seconds at a time, over and over, by something
	// else on the machine. A reader cannot see that from the recent
	// window, by construction, since the recent window is what rolled
	// past it.
	runSlowest      time.Duration
	runSlowestLabel string

	// pollInterval and worstPollLag are set by run.go, not by anything
	// in this file (tracker.go stays free of process/I/O concerns; see
	// the package doc comment at the top of this file): the interval
	// gotestwatch's own watchdog loop asked to be run on, and the worst
	// it was actually late by over the run.
	//
	// That lag is the closest this tool gets to a reading of the machine
	// rather than of the run, and it is worth being exact about how
	// close. run.go's pollLag subtracts the loop's own turn, which takes
	// the tracker's mutex and can therefore be blocked by the goroutine
	// draining `go test`'s stdout, so what is left is the host not
	// running a process that was parked and ready. What it cannot
	// subtract is gotestwatch's own GC pauses, and the subtraction is
	// conservative besides, so this is a ceiling on what the host took
	// and not an exact figure (pollLag has the arithmetic and why both
	// errors point the safe way). Zero pollInterval means nothing
	// measured it (the synthetic clock tests, mostly) and the sentence is
	// left out rather than printed as a reading of zero.
	pollInterval time.Duration
	worstPollLag time.Duration

	// reapTimedOut and reapWait are set by run.go, not by anything in
	// this file (tracker.go stays free of process/I/O concerns; see the
	// package doc comment at the top of this file), when the process
	// group a trip sent SIGKILL to did not actually exit within
	// reapWait. SIGKILL cannot end a process stuck in uninterruptible
	// kernel I/O wait, a real possibility for exactly the class of hang
	// this tool targets (stuck Docker/SFTP I/O); a trip that has already
	// been decided is still reported when that happens, rather than
	// run.go blocking indefinitely on a reap that may never come.
	reapTimedOut bool
	reapWait     time.Duration
}

// String is the sentence a person reads when this tool kills a run, and it
// is where the whole design either pays off or does not.
//
// A watchdog that says only "timed out" leaves the reader with the same
// question a fixed -timeout leaves them: was this stuck, or just slow? So
// each message names what was still running, how many events had been
// observed, what the bound was derived from, how the run's own worst gap
// compares to the recent ones the bound came from, and how late the host
// was running gotestwatch's own watchdog loop while all of that happened.
//
// What it no longer does is name a cause, and issue #533 is why. The
// overall case used to end "That is a livelock, not a slow machine: the
// cap is derived from this run's own recent pace, so a genuinely,
// consistently slow run would have widened it", and it killed a gate run
// with that sentence while another project on the same machine was running
// a Playwright suite and the load was swinging between 11 and 72. The test
// it named passed in 8.156s on its own minutes later. The reasoning is
// sound against a machine that is CONSISTENTLY slow and false against a
// bursty one: a quiet stretch sets a fast pace, the cap tightens to match
// it, and then one burst pushes the run past a cap that was set while
// nothing was competing.
//
// It is not a wording problem, so it did not get a wording fix. A run that
// keeps reporting progress and never finishes produces the same arrival
// pattern whether it is livelocked or being starved of the machine, down
// to the individual gap (see
// TestTracker_ALivelockAndALoadedHostProduceTheSameEvidence, which builds
// both from their own causes and finds the watchdog holding an identical
// trip), so no rule over what the tracker can see separates them: not the
// recent maximum, not the whole-run one, not whether the gaps are growing.
// Naming a cause it cannot distinguish is worse than naming none, because
// a guard that is wrong in a particular direction sends the next person
// hunting a deadlock that does not exist.
func (tr trip) String() string {
	running := "no test was reported as running, so the last thing observed at all is named above"
	if len(tr.running) > 0 {
		running = "test(s) still reported running: " + strings.Join(tr.running, ", ")
	}
	measured := fmt.Sprintf("%s observed, slowest recent gap %s (%s)", eventCount(tr.events), tr.slowestStep.Round(time.Millisecond), tr.slowestLabel)
	switch {
	case tr.events == 0:
		measured = "no event had arrived yet, so the window was still the unmeasured floor"
	case tr.runSlowest > tr.slowestStep:
		// The mechanism from issue #533, stated as the measurement it
		// is: this run has already been through a gap the bound it was
		// killed against can no longer see.
		measured += fmt.Sprintf("; the slowest gap of the whole run was %s (%s), which the recent window has already rolled past",
			tr.runSlowest.Round(time.Millisecond), tr.runSlowestLabel)
	}
	host := ""
	if tr.pollInterval > 0 {
		// The percentage is not decoration. A bare "late by up to
		// 1.019ms" is a number nobody can judge; the same number as 2%
		// of the interval it was late for says at a glance that this
		// host was fine, and 300% says it was not.
		host = fmt.Sprintf(" gotestwatch's own watchdog loop asked for a turn every %s and the worst it was run late by was %s, %s of that interval: time the host had it parked and ready, with the loop's own work taken out but not its GC pauses, so read it as a ceiling rather than an exact reading.",
			tr.pollInterval.Round(time.Millisecond), tr.worstPollLag.Round(time.Microsecond), shareOf(tr.worstPollLag, tr.pollInterval))
	}
	var out string
	switch tr.kind {
	case "overall":
		out = fmt.Sprintf("go test kept reporting progress but never finished: %s elapsed against a cap of %s, last event %q. %s. %s.%s "+
			"Why it never finished is not something this can tell you. A run reporting progress it never completes looks the same from out here whether it is livelocked, whether something else on this machine is taking the CPU away from it, or whether it is simply a suite that needs longer than the cap, so take the measurements above and not a cause from this line.",
			tr.elapsed.Round(time.Millisecond), tr.overallCap.Round(time.Millisecond), tr.lastEvent, running, measured, host)
	default:
		out = fmt.Sprintf("go test stopped making progress: nothing after %q for %s, against a no-progress window of %s (%s elapsed in total). %s. %s.%s "+
			"The window is derived from this run's own recent pace, so a consistently slow run widens it and survives, and a silence this much longer than the pace the run had just been keeping is most likely a hang. It is not proof of one: load that arrives after that pace was measured stalls a healthy run the same way, which is what the numbers above are for.",
			tr.lastEvent, tr.sinceLast.Round(time.Millisecond), tr.window.Round(time.Millisecond), tr.elapsed.Round(time.Millisecond), running, measured, host)
	}
	if tr.reapTimedOut {
		out += fmt.Sprintf(" The killed process group had still not exited %s after being sent SIGKILL (likely stuck in uninterruptible I/O, which SIGKILL cannot end); reporting this trip anyway rather than waiting on the reap indefinitely.",
			tr.reapWait.Round(time.Second))
	}
	return out
}

// shareOf renders d as a percentage of whole, which is what turns a
// duration nobody has a feel for into one they can judge. Never a
// division by zero: every caller checks the interval first, and this
// returns a shrug rather than an infinity if one ever stops.
func shareOf(d, whole time.Duration) string {
	if whole <= 0 {
		return "an unknown share"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(d)/float64(whole))
}

// eventCount is "1 event" or "N events". A small thing, and the sentence
// it goes in is read by somebody at the exact moment their gate just died,
// so "1 events observed" is worth not printing at them.
func eventCount(n int) string {
	if n == 1 {
		return "1 event"
	}
	return fmt.Sprintf("%d events", n)
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

	// runSlowest and runSlowestLabel are the same measurement kept over
	// the whole run instead of the recent window, and deliberately not
	// used to derive anything (see the field comment on trip, and issue
	// #533). Keeping both is the point: the rolling one is what the
	// bounds are made of, and the difference between them is the only
	// record a run has that it was stalled and recovered, which is
	// exactly what a bound made of the recent window cannot show.
	runSlowest      time.Duration
	runSlowestLabel string

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
		gap := t.lastEvent + " -> " + label
		t.recentGaps[t.recentHead] = d
		t.recentLabels[t.recentHead] = gap
		t.recentHead = (t.recentHead + 1) % slowestStepMemory
		if t.recentLen < slowestStepMemory {
			t.recentLen++
		}
		if d > t.runSlowest {
			t.runSlowest = d
			t.runSlowestLabel = gap
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

		runSlowest:      t.runSlowest,
		runSlowestLabel: t.runSlowestLabel,
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
