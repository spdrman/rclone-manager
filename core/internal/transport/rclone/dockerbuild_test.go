package rclone

// This file gives buildSFTPFixtureImage a no-progress-derived timeout
// instead of the fixed 120-second one it used to wrap `docker build` in
// (issue #309). That fixed budget was wrong for the same reason #247 and
// #256 (tests/crashmatrix, core/cmd/gotestwatch) already identified for a
// harness process and a `go test` package run: under real host/Docker-
// daemon load a build that is still making progress can legitimately take
// longer than any fixed number chosen on a quiet machine, and a timeout
// that cannot tell "busy" from "stuck" kills it anyway, at almost exactly
// the deadline, with an error ("signal: killed" / "context canceled")
// that reads as a Docker-side failure rather than what actually happened.
//
// dockerBuildProgressTracker is progressTracker, one package over
// (tests/crashmatrix/crash_matrix_test.go): parse `docker build
// --progress=plain`'s own output lines as the progress signal instead of
// the harness's custom PROGRESS stream, and derive the same two bounds
// from this run's own slowest observed step rather than a round guess. A
// single build is short-lived compared to a `go test` package run, so
// this follows crash_matrix's plain all-time-max-of-this-run shape
// rather than gotestwatch's decaying ring buffer (built for a much
// longer-lived process where one early outlier must not permanently
// inflate the bound for everything after it) - that decay problem does
// not have room to arise inside one docker build.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// dockerBuildBounds are the two derived bounds' floor, multiplier and
// absolute ceiling. See dockerBuildProgressTracker.window/overallCap for
// how they combine with a run's own measured pace.
type dockerBuildBounds struct {
	stepFloor     time.Duration
	stepFactor    float64
	stepMax       time.Duration
	overallFloor  time.Duration
	overallFactor float64
	overallMax    time.Duration
}

// defaultDockerBuildBounds is sized for the small, few-layer fixture
// image sftpFixtureDockerfile describes (one alpine base, one apk
// install, two COPY/chown steps): normally seconds per step even cold,
// so the floor only has to cover a legitimately slow step under real
// contention, not this build's ordinary pace. stepFactor 10 and
// overallFactor 20 mirror crash_matrix's own reasoning (a step that is
// consistently slow widens both bounds because it raises the "slowest
// step" they are multiples of; only a step that goes quiet for longer
// than its own recent pace justifies trips it).
//
// stepMax and overallMax are the absolute ceilings, and they exist
// because "derived from this run's own pace" is unbounded on its own: one
// 60-second step alone would put the no-progress window at 600s and the
// overall cap at 1200s. This package is NOT run under core/cmd/gotestwatch
// (scripts/ci-local.sh routes only tests/crashmatrix and
// tests/sftpintegration through it), so it still runs under `go test`'s
// own fixed 600-second per-package default, and a bound allowed to widen
// past that just moves #309's failure one level up: `go test` force-kills
// the whole binary with `panic: test timed out`, naming no step and
// orphaning the in-flight `docker build`.
//
// The numbers come from measuring this fixture rather than rounding. On
// this machine a warm build of sftpFixtureDockerfile takes 0.14-0.47s, a
// cold one with no layer cache takes ~1.0s, and a cold pull of the
// alpine:3.20 base adds ~1.4s, so a fully cold build is ~2.5s. A full run
// of this package takes ~64s wall clock and does six fixture builds
// (TestClassify_Docker, TestFixtureImageIsNotSharedBetweenClientKeys
// twice, TestSFTPHostKeyVerification, TestSFTPKeyResolvers,
// TestSFTPKeyResolvers_Passphrase). overallMax of 150s is therefore ~60x
// a fully cold build, comfortably above overallFloor so the derived
// bounds stay the operative mechanism rather than being preempted, and
// small enough that three simultaneously-pathological builds (450s) still
// land inside `go test`'s 600s budget next to this package's own measured
// ~64s of other work. stepMax of 120s sits below it so a build that
// genuinely stalls is still reported as a stall rather than waiting to be
// caught by the overall cap and mislabelled a livelock.
//
// Deliberately NOT a context.WithTimeout wrapped around the whole command,
// which is what this function replaced: a context deadline firing produces
// exactly the bare "signal: killed" / "context canceled" that issue #309
// filed as unreadable. Capping the derived bounds keeps the ceiling and
// the diagnostic at the same time.
var defaultDockerBuildBounds = dockerBuildBounds{
	stepFloor:     20 * time.Second,
	stepFactor:    10,
	stepMax:       120 * time.Second,
	overallFloor:  90 * time.Second,
	overallFactor: 20,
	overallMax:    150 * time.Second,
}

// dockerBuildTrip is watchdogTrip, adapted: what a bound being exceeded
// produces, carrying the measurements the decision was made from so a
// failure teaches rather than just names an exit code.
type dockerBuildTrip struct {
	kind         string // "no-progress" or "overall"
	lastLine     string
	sinceLast    time.Duration
	window       time.Duration
	elapsed      time.Duration
	overallCap   time.Duration
	lines        int
	slowestGap   time.Duration
	slowestLabel string
}

func (tr dockerBuildTrip) String() string {
	measured := fmt.Sprintf("%d progress lines observed, slowest gap %s (%s)", tr.lines, tr.slowestGap.Round(time.Millisecond), tr.slowestLabel)
	if tr.lines == 0 {
		measured = "no progress line had arrived yet, so the window was still the unmeasured floor"
	}
	switch tr.kind {
	case "overall":
		return fmt.Sprintf("docker build kept reporting progress but never finished: %s elapsed against a cap of %s, last line %q. %s. "+
			"That is a livelock, not a slow machine: the cap is derived from this build's own slowest step, so a genuinely slow build would have widened it.",
			tr.elapsed.Round(time.Millisecond), tr.overallCap.Round(time.Millisecond), tr.lastLine, measured)
	default:
		return fmt.Sprintf("docker build stopped making progress: nothing after %q for %s, against a no-progress window of %s (%s elapsed in total). %s. "+
			"This is a hang, not a slow machine: the window is derived from this build's own recent pace, so being consistently slow widens it and only being stuck trips it.",
			tr.lastLine, tr.sinceLast.Round(time.Millisecond), tr.window.Round(time.Millisecond), tr.elapsed.Round(time.Millisecond), measured)
	}
}

// dockerBuildProgressTracker turns docker build's own `--progress=plain`
// line stream into the two derived bounds above. A plain value with an
// explicit clock passed to observe/check, so it is provable at
// millisecond scale without a real subprocess (see
// TestDockerBuildProgressTracker_* below); the end-to-end proof that a
// really-stuck build is really caught is
// TestRunDockerBuildWatched_ARealHangIsCaught, same split crash_matrix's
// own progressTracker/watchdog pair uses.
type dockerBuildProgressTracker struct {
	b dockerBuildBounds

	mu           sync.Mutex
	start        time.Time
	lastAt       time.Time
	lastLine     string
	lines        int
	slowestGap   time.Duration
	slowestLabel string
}

func newDockerBuildProgressTracker(b dockerBuildBounds, start time.Time) *dockerBuildProgressTracker {
	return &dockerBuildProgressTracker{b: b, start: start, lastAt: start, lastLine: "build start"}
}

// observe records one line of `docker build --progress=plain` output.
func (p *dockerBuildProgressTracker) observe(line string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := at.Sub(p.lastAt); d > p.slowestGap {
		p.slowestGap = d
		p.slowestLabel = p.lastLine + " -> " + line
	}
	p.lastAt = at
	p.lastLine = line
	p.lines++
}

// window is the current no-progress bound, derived from this run's own
// slowest step, never below the floor and never above the absolute
// ceiling. Callers hold p.mu.
func (p *dockerBuildProgressTracker) window() time.Duration {
	w := p.b.stepFloor
	if derived := time.Duration(float64(p.slowestGap) * p.b.stepFactor); derived > w {
		w = derived
	}
	if p.b.stepMax > 0 && w > p.b.stepMax {
		return p.b.stepMax
	}
	return w
}

// overallCap is the current total-runtime backstop, bounded the same way.
// The ceiling is what keeps a build's total runtime from widening past
// `go test`'s own per-package budget; see defaultDockerBuildBounds.
// Callers hold p.mu.
func (p *dockerBuildProgressTracker) overallCap() time.Duration {
	c := p.b.overallFloor
	if derived := time.Duration(float64(p.slowestGap) * p.b.overallFactor); derived > c {
		c = derived
	}
	if p.b.overallMax > 0 && c > p.b.overallMax {
		return p.b.overallMax
	}
	return c
}

// check reports the bound that has been exceeded as of now, or nil if the
// build is still within both.
func (p *dockerBuildProgressTracker) check(now time.Time) *dockerBuildTrip {
	p.mu.Lock()
	defer p.mu.Unlock()

	tr := dockerBuildTrip{
		lastLine:     p.lastLine,
		sinceLast:    now.Sub(p.lastAt),
		window:       p.window(),
		elapsed:      now.Sub(p.start),
		overallCap:   p.overallCap(),
		lines:        p.lines,
		slowestGap:   p.slowestGap,
		slowestLabel: p.slowestLabel,
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

// dockerBuildLineTap captures a stream verbatim while handing complete
// lines to onLine as they arrive.
//
// This is crash_matrix's lineTap, and taking it rather than reading pipes
// by hand is the whole point: os/exec gives a non-*os.File writer its own
// copying goroutine AND makes Cmd.Wait block on that goroutine before it
// returns (awaitGoroutines), so a line reaches the tracker the instant the
// child wrote it and Wait cannot return while output is still in flight.
//
// The first version of runDockerBuildWatched used StdoutPipe/StderrPipe
// with hand-rolled scanner goroutines and ran cmd.Wait concurrently with
// them, which os/exec's own documentation calls out as incorrect: "Wait
// will close the pipe after seeing the command exit, so it is incorrect to
// call Wait before all reads from the pipe have completed." Wait closes
// the parent's read end as soon as it reaps the process, so anything still
// buffered at that moment was lost silently, and under load that was
// almost the entire transcript. Losing the transcript defeats this
// function: the captured output is what buildSFTPFixtureImage prints on a
// real build failure, and what names the step a hung build got stuck
// after.
//
// Splitting on '\n' here rather than through a bufio.Scanner also removes
// that scanner's own failure mode: a Scan() that stopped early on
// bufio.ErrTooLong (a line past the buffer cap) used to end the pump
// silently, and the watchdog then reported the resulting silence as a
// hang. There is no cap and no error to swallow now, and a genuine I/O
// error on the pipe surfaces through cmd.Wait's own return value instead.
type dockerBuildLineTap struct {
	onLine func(line string)

	mu      sync.Mutex
	all     bytes.Buffer
	partial []byte
}

func (l *dockerBuildLineTap) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all.Write(p)
	if l.onLine == nil {
		return len(p), nil
	}
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		line := string(l.partial[:i])
		l.partial = l.partial[i+1:]
		l.onLine(line)
	}
	return len(p), nil
}

func (l *dockerBuildLineTap) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return []byte(l.all.String())
}

// wireDockerBuildOutput points both of cmd's output streams at one tap
// feeding tracker, and returns the tap holding the transcript.
//
// Split out from runDockerBuildWatched so the wiring itself is assertable
// without a Docker daemon (see TestWireDockerBuildOutput_*): which writer
// the streams are attached to is the whole correctness question here, and
// it is not observable from the outside once the command has run.
//
// One tap for both streams. BuildKit's plain progress goes to stderr and
// the CLI's own summary lines to stdout, and a caller wants one transcript
// in the order the child actually wrote it. Handing the same writer to
// both is what makes os/exec give them a single pipe and a single copying
// goroutine (exactly what CombinedOutput does), so there is nothing to
// merge by hand and no way to interleave the two wrongly.
func wireDockerBuildOutput(cmd *exec.Cmd, tracker *dockerBuildProgressTracker) *dockerBuildLineTap {
	tap := &dockerBuildLineTap{onLine: func(line string) { tracker.observe(line, time.Now()) }}
	cmd.Stdout = tap
	cmd.Stderr = tap
	return tap
}

// runDockerBuildWatched runs `docker build --progress=plain -t tag dir`,
// killing it and returning a *dockerBuildTrip-described error the moment
// either derived bound trips, rather than trusting one fixed wall-clock
// budget around the whole command the way buildSFTPFixtureImage used to.
//
// pollEvery controls how often the watchdog checks the tracker; it exists
// as a parameter (rather than a package constant) purely so tests can
// drive it fast without sleeping for real seconds.
func runDockerBuildWatched(ctx context.Context, b dockerBuildBounds, pollEvery time.Duration, tag, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "build", "--progress=plain", "-t", tag, dir)

	start := time.Now()
	tracker := newDockerBuildProgressTracker(b, start)
	tap := wireDockerBuildOutput(cmd, tracker)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker build: start: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			// Wait has already blocked on the copying goroutine, so the
			// tap holds everything the build wrote. No separate drain.
			if err != nil {
				return tap.Bytes(), fmt.Errorf("docker build: %w", err)
			}
			return tap.Bytes(), nil
		case <-ticker.C:
			if trip := tracker.check(time.Now()); trip != nil {
				// Kill it, then still wait, so the watchdog never leaves
				// the process it gave up on running behind. This reaps
				// the local `docker` CLI; whether the daemon tears its
				// server-side BuildKit step down too is BuildKit's own
				// business and not something this can promise.
				_ = cmd.Process.Kill()
				<-done
				return tap.Bytes(), fmt.Errorf("docker build hit a timeout, not a build failure: %s", trip)
			}
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			return tap.Bytes(), ctx.Err()
		}
	}
}

// --- the bounds themselves, against a synthetic clock --------------------
//
// Proved the same way crash_matrix's own progressTracker is
// (tests/crashmatrix/watchdog_test.go): pinned exactly, without sleeping,
// so this costs the gate milliseconds rather than the 120+ seconds a real
// hung build would need to demonstrate the same thing.

func TestDockerBuildProgressTracker_TripsAtTheFloorBeforeAnythingIsMeasured(t *testing.T) {
	start := time.Now()
	p := newDockerBuildProgressTracker(defaultDockerBuildBounds, start)

	floor := defaultDockerBuildBounds.stepFloor
	if trip := p.check(start.Add(floor - time.Millisecond)); trip != nil {
		t.Fatalf("tripped one millisecond before the %s floor: %v", floor, trip)
	}
	trip := p.check(start.Add(floor + time.Millisecond))
	if trip == nil {
		t.Fatalf("a build that printed nothing at all for %s was not caught", floor+time.Millisecond)
	}
	if trip.kind != "no-progress" {
		t.Fatalf("trip.kind = %q, want no-progress: %v", trip.kind, trip)
	}
	if trip.lastLine != "build start" {
		t.Fatalf("trip.lastLine = %q, want the build's own starting point", trip.lastLine)
	}
}

func TestDockerBuildProgressTracker_WidensWithTheSlowestStepItHasSeen(t *testing.T) {
	start := time.Now()
	p := newDockerBuildProgressTracker(defaultDockerBuildBounds, start)

	// A machine so loaded that BuildKit only emits a fresh progress line
	// every eight seconds. Four of those alone (32s) would already be
	// most of the way through the OLD fixed 120s budget while nothing
	// whatsoever is wrong; here the same evidence widens the window
	// instead of spending it.
	at := start
	for i, line := range []string{"#1 [internal] load build definition", "#2 [1/4] FROM alpine:3.20", "#3 [2/4] RUN apk add", "#4 [3/4] COPY authorized_keys"} {
		at = at.Add(8 * time.Second)
		p.observe(line, at)
		if trip := p.check(at); trip != nil {
			t.Fatalf("step %d (%s) was reported as a hang: %v", i, line, trip)
		}
	}

	if got, want := p.window(), 80*time.Second; got != want {
		t.Fatalf("window after four eight-second steps = %s, want %s (stepFactor x the slowest step)", got, want)
	}
	// 79s of silence on a machine just measured at 8s per step is not a
	// hang, even though the OLD fixed 120s budget had far less than that
	// left after four such steps plus the time already spent reaching them.
	if trip := p.check(at.Add(79 * time.Second)); trip != nil {
		t.Fatalf("79s of silence on a machine measured at 8s per step was called a hang: %v", trip)
	}
}

func TestDockerBuildProgressTracker_AWidenedWindowStillCloses(t *testing.T) {
	start := time.Now()
	p := newDockerBuildProgressTracker(defaultDockerBuildBounds, start)

	at := start.Add(10 * time.Second)
	p.observe("#3 [2/4] RUN apk add", at)

	// Derived is not unbounded. However slow this machine measured, going
	// quiet for ten times its own slowest step is still a hang, and still
	// says which line it got stuck after.
	if trip := p.check(at.Add(99 * time.Second)); trip != nil {
		t.Fatalf("tripped one second inside the derived window: %v", trip)
	}
	trip := p.check(at.Add(101 * time.Second))
	if trip == nil {
		t.Fatal("a build that went quiet for more than its derived window was never caught, so the bound is unbounded")
	}
	if trip.kind != "no-progress" || trip.lastLine != "#3 [2/4] RUN apk add" {
		t.Fatalf("trip = %+v, want a no-progress trip naming the RUN step", *trip)
	}
	if !strings.Contains(trip.String(), "RUN apk add") {
		t.Fatalf("the failure text does not name the step the build got stuck after:\n%v", trip)
	}
}

func TestDockerBuildProgressTracker_OverallCapCatchesALivelock(t *testing.T) {
	start := time.Now()
	p := newDockerBuildProgressTracker(defaultDockerBuildBounds, start)

	// A build that reports something small and fast forever - a
	// pathological output loop, say - reports progress continuously, so
	// no no-progress window can ever see it. Its steps are fast, which is
	// what keeps its cap at the floor and kills it promptly rather than
	// leaving it "always about to trip the no-progress window and never
	// quite doing it."
	at := start
	for at.Sub(start) < defaultDockerBuildBounds.overallFloor+time.Second {
		at = at.Add(50 * time.Millisecond)
		p.observe("still building...", at)
	}
	trip := p.check(at)
	if trip == nil {
		t.Fatal("a build that never stopped reporting progress, but also never finished, was not caught by the overall cap")
	}
	if trip.kind != "overall" {
		t.Fatalf("trip.kind = %q, want overall: %v", trip.kind, trip)
	}
}

func TestDockerBuildProgressTracker_OneSlowStepWidensTheCapForTheRestOfTheBuild(t *testing.T) {
	// The property that distinguishes this from a decaying bound:
	// crash_matrix's own progressTracker has this same all-time-max shape
	// because a single harness invocation is short enough that the
	// distinction gotestwatch needed for a whole `go test` package run
	// does not have room to matter here either. This test exists to make
	// that a checked decision rather than an unstated assumption: one slow
	// step (the alpine pull, say) genuinely does widen the bound for the
	// REST of this one build, on purpose, and keeps it widened after
	// faster steps follow.
	start := time.Now()
	p := newDockerBuildProgressTracker(defaultDockerBuildBounds, start)

	slow := start.Add(9 * time.Second) // one slow step: a 9s image pull
	p.observe("#2 [1/4] FROM alpine:3.20", slow)

	fast := slow.Add(1 * time.Second) // every step after it is fast
	p.observe("#3 [2/4] RUN apk add", fast)

	if got, want := p.window(), 90*time.Second; got != want {
		t.Fatalf("window after one 9s step = %s, want %s: the slow pull must still be what the window is derived from, even after a fast step followed it", got, want)
	}
}

func TestDockerBuildProgressTracker_NeitherBoundWidensPastItsCeiling(t *testing.T) {
	// The other half of the previous test, and the reason issue #309's
	// fix cannot simply be "derive the bound and trust it". Deriving from
	// this run's own pace is unbounded on its own: one 60-second step puts
	// the raw no-progress window at 600s (stepFactor 10) and the raw
	// overall cap at 1200s (overallFactor 20), both past `go test`'s own
	// fixed 600s per-package budget. This package does not run under
	// gotestwatch, so a bound allowed past that budget just relocates
	// #309's failure: `go test` kills the whole binary, naming no step.
	start := time.Now()
	p := newDockerBuildProgressTracker(defaultDockerBuildBounds, start)

	p.observe("#2 [1/4] FROM alpine:3.20", start.Add(60*time.Second))

	if got, want := p.window(), defaultDockerBuildBounds.stepMax; got != want {
		t.Fatalf("no-progress window after a 60s step = %s, want it clamped to %s; derived alone it would be %s",
			got, want, 600*time.Second)
	}
	if got, want := p.overallCap(), defaultDockerBuildBounds.overallMax; got != want {
		t.Fatalf("overall cap after a 60s step = %s, want it clamped to %s; derived alone it would be %s",
			got, want, 1200*time.Second)
	}

	// The ceilings are only worth anything if they are actually below the
	// budget they exist to protect, with room for the rest of the package.
	// A full run of this package measures ~64s and does six fixture
	// builds; `go test`'s default package budget is 600s.
	if defaultDockerBuildBounds.overallMax >= 600*time.Second {
		t.Fatalf("overallMax %s is not below `go test`'s own 600s package budget, so it guarantees nothing",
			defaultDockerBuildBounds.overallMax)
	}
	if defaultDockerBuildBounds.stepMax >= defaultDockerBuildBounds.overallMax {
		t.Fatalf("stepMax %s is not below overallMax %s, so a stalled build gets reported as a livelock instead of a stall",
			defaultDockerBuildBounds.stepMax, defaultDockerBuildBounds.overallMax)
	}
	if defaultDockerBuildBounds.overallMax <= defaultDockerBuildBounds.overallFloor {
		t.Fatalf("overallMax %s is at or below overallFloor %s, so the ceiling preempts the derived bound entirely and this is a fixed budget again, which is what #309 filed",
			defaultDockerBuildBounds.overallMax, defaultDockerBuildBounds.overallFloor)
	}
}

// --- proof against a real docker build -----------------------------------

func TestRunDockerBuildWatched_ARealBuildSucceeds(t *testing.T) {
	requireDocker(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine:3.20\nRUN echo hello\n"), 0o644); err != nil {
		t.Fatalf("writing Dockerfile: %v", err)
	}
	tag := "rclone-manager-test-dockerbuild-watched-ok:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := runDockerBuildWatched(ctx, defaultDockerBuildBounds, 200*time.Millisecond, tag, dir)
	if err != nil {
		t.Fatalf("a real, ordinary build failed: %v\n%s", err, out)
	}
	if len(out) == 0 {
		t.Fatal("a real build produced no captured progress output at all")
	}
}

// TestWireDockerBuildOutput_AttachesAWriterRatherThanPipes is the
// regression net for the capture bug the first version of this file had.
//
// That version called cmd.StdoutPipe()/StderrPipe(), read them with
// scanner goroutines, and ran cmd.Wait() concurrently with those reads.
// os/exec documents that as incorrect ("Wait will close the pipe after
// seeing the command exit, so it is incorrect to call Wait before all
// reads from the pipe have completed"), because Wait closes the parent's
// read end as soon as it reaps the process and discards whatever is still
// buffered behind it.
//
// The invariant is structural rather than statistical, so this pins it
// structurally, and deliberately not by trying to observe lost output: I
// could not get the old wiring to drop a single line on this platform
// across many trials, with a real docker build and with a plain burst-and-
// exit child, contended and uncontended. A test that tried to catch it by
// counting lines would therefore pass against the very code it exists to
// reject, which is worse than no test. What IS reliably true is that
// attaching an io.Writer makes os/exec own the copy and makes Wait block
// on it (awaitGoroutines), while StdoutPipe leaves Stdout nil and hands
// the caller a race it has to get right by hand. So: assert the wiring.
func TestWireDockerBuildOutput_AttachesAWriterRatherThanPipes(t *testing.T) {
	cmd := exec.Command("docker", "build", ".")
	tracker := newDockerBuildProgressTracker(defaultDockerBuildBounds, time.Now())
	tap := wireDockerBuildOutput(cmd, tracker)

	if cmd.Stdout == nil || cmd.Stderr == nil {
		t.Fatal("Stdout/Stderr left nil, which is what StdoutPipe/StderrPipe do; os/exec then cannot synchronise Wait against the reads and the transcript can be truncated by Wait closing the pipe")
	}
	if cmd.Stdout != cmd.Stderr {
		t.Error("the two streams are not the same writer, so os/exec gives each its own pipe and the merged transcript can interleave the two out of order")
	}
	if cmd.Stdout != any(tap) {
		t.Errorf("Stdout is %T, want the returned *dockerBuildLineTap: the tap is what feeds the tracker, so a stream not pointed at it is a stream the watchdog cannot see progress on", cmd.Stdout)
	}

	// And the tap really does feed the tracker, since the wiring being
	// right is only useful if what it is wired to measures something.
	if _, err := tap.Write([]byte("#5 [2/4] RUN apk add\n")); err != nil {
		t.Fatalf("tap.Write: %v", err)
	}
	tracker.mu.Lock()
	lines, last := tracker.lines, tracker.lastLine
	tracker.mu.Unlock()
	if lines != 1 || last != "#5 [2/4] RUN apk add" {
		t.Errorf("after one complete line the tracker saw lines=%d last=%q, want 1 and the line itself", lines, last)
	}
}

func TestDockerBuildLineTap_SplitsLinesAcrossWritesAndKeepsEverything(t *testing.T) {
	// The tap replaced a bufio.Scanner, so the line splitting is this
	// file's own responsibility now: a line arriving in pieces must still
	// reach the tracker exactly once, and the verbatim transcript must
	// keep every byte including a trailing partial line that never got its
	// newline (a build killed mid-write leaves exactly that, and it is
	// often the most interesting part of the output).
	var got []string
	tap := &dockerBuildLineTap{onLine: func(line string) { got = append(got, line) }}

	for _, chunk := range []string{"#1 load", " definition\n#2 FROM al", "pine\n#3 partial-no-newline"} {
		if _, err := tap.Write([]byte(chunk)); err != nil {
			t.Fatalf("tap.Write(%q): %v", chunk, err)
		}
	}

	want := []string{"#1 load definition", "#2 FROM alpine"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines handed to the tracker = %q, want %q", got, want)
	}
	if wantAll := "#1 load definition\n#2 FROM alpine\n#3 partial-no-newline"; string(tap.Bytes()) != wantAll {
		t.Errorf("captured transcript = %q, want %q: the unterminated last line must survive", tap.Bytes(), wantAll)
	}
}

func TestRunDockerBuildWatched_CapturesEveryLineARealBuildEmits(t *testing.T) {
	// End-to-end companion to the wiring assertion above: whatever the
	// build prints comes back. Sized well under BuildKit's own output cap
	// on a single step, which I measured at 16,608 lines on this daemon:
	// past that, output is dropped by BuildKit before this code ever sees
	// it, so a bigger assertion here would fail for a reason that has
	// nothing to do with this function.
	requireDocker(t)

	const wantLines = 200
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	dir := t.TempDir()
	// The nonce keeps the RUN layer out of the build cache, so the step
	// really re-executes and really re-emits rather than being replayed.
	dockerfile := fmt.Sprintf("FROM alpine:3.20\nRUN echo %s >/dev/null && for i in $(seq 1 %d); do echo ZZLINE-$i; done\n", nonce, wantLines)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("writing Dockerfile: %v", err)
	}
	tag := "rclone-manager-test-dockerbuild-capture:" + nonce
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	out, err := runDockerBuildWatched(ctx, defaultDockerBuildBounds, 200*time.Millisecond, tag, dir)
	if err != nil {
		t.Fatalf("a real, ordinary build failed: %v\n%s", err, out)
	}
	// Counted per exact token rather than by substring: the Dockerfile's
	// own command text is echoed back by BuildKit as the step header, and
	// it contains the bare prefix, so a substring count is off by one.
	missing := 0
	for i := 1; i <= wantLines; i++ {
		if !strings.Contains(string(out), fmt.Sprintf("ZZLINE-%d\n", i)) {
			missing++
		}
	}
	if missing != 0 {
		t.Fatalf("%d of the %d lines the build emitted never reached the caller, so output is being lost between the child writing it and this function returning it (%d bytes captured)",
			missing, wantLines, len(out))
	}
}

func TestRunDockerBuildWatched_ARealHangIsCaught(t *testing.T) {
	requireDocker(t)
	dir := t.TempDir()
	// Prints once, then goes quiet forever: real progress, then a real
	// stall, the exact shape a fixed-budget timeout cannot tell apart
	// from "still building" and this mechanism exists to tell apart.
	dockerfile := "FROM alpine:3.20\nRUN echo about-to-hang && sleep 300\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("writing Dockerfile: %v", err)
	}
	tag := "rclone-manager-test-dockerbuild-watched-hang:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })

	// Deliberately small bounds, so a genuine hang is caught in single-digit
	// seconds of test time rather than needing to wait out anything close
	// to the 300s the RUN step itself would otherwise take. Not smaller:
	// docker build's own subprocess/daemon-connection startup latency
	// before its first real progress line arrives is itself real time a
	// floor has to cover, or the watchdog trips on that instead of on the
	// hang this test means to prove it catches.
	tinyBounds := dockerBuildBounds{
		stepFloor:     3 * time.Second,
		stepFactor:    3,
		overallFloor:  5 * time.Second,
		overallFactor: 3,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	out, err := runDockerBuildWatched(ctx, tinyBounds, 20*time.Millisecond, tag, dir)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("a build with a 300s sleep in it returned success after %s", elapsed)
	}
	if !strings.Contains(err.Error(), "docker build hit a timeout, not a build failure") {
		t.Fatalf("error does not say this was a timeout, not a build failure: %v", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("took %s to catch a hang under bounds sized to catch it in single-digit seconds, nowhere near the 300s the RUN step's own sleep would otherwise take; the watchdog is not really watching", elapsed)
	}
	if !strings.Contains(string(out), "about-to-hang") {
		t.Fatalf("captured output does not contain the real progress emitted before the hang:\n%s", out)
	}
	// What this proves, stated exactly: the bound tripped, the error says
	// it was a timeout rather than a build failure, and the local `docker`
	// CLI was reaped (cmd.Wait returned, or this test would still be
	// blocked in runDockerBuildWatched). It does NOT prove the daemon tore
	// the server-side BuildKit step down: killing the client does not
	// reliably cancel a running server-side build, which is BuildKit's own
	// behaviour and outside what this function can promise. The same was
	// true of the fixed-timeout version this replaced.
}
