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
	"bufio"
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

// dockerBuildBounds are the two derived bounds' floor and multiplier. See
// dockerBuildProgressTracker.window/overallCap for how they combine with
// a run's own measured pace.
type dockerBuildBounds struct {
	stepFloor     time.Duration
	stepFactor    float64
	overallFloor  time.Duration
	overallFactor float64
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
var defaultDockerBuildBounds = dockerBuildBounds{
	stepFloor:     20 * time.Second,
	stepFactor:    10,
	overallFloor:  90 * time.Second,
	overallFactor: 20,
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

// window is the current no-progress bound. Callers hold p.mu.
func (p *dockerBuildProgressTracker) window() time.Duration {
	if derived := time.Duration(float64(p.slowestGap) * p.b.stepFactor); derived > p.b.stepFloor {
		return derived
	}
	return p.b.stepFloor
}

// overallCap is the current total-runtime backstop. Callers hold p.mu.
func (p *dockerBuildProgressTracker) overallCap() time.Duration {
	if derived := time.Duration(float64(p.slowestGap) * p.b.overallFactor); derived > p.b.overallFloor {
		return derived
	}
	return p.b.overallFloor
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
	// BuildKit's plain progress writes to stderr; the CLI's own summary
	// lines (if any) go to stdout. Piped separately, since exec.Cmd
	// refuses the same io.Writer for both when either is a *Pipe(), and
	// merged by hand below so a caller sees one combined transcript
	// regardless of which stream a given line came from.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("docker build: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("docker build: stderr pipe: %w", err)
	}

	start := time.Now()
	tracker := newDockerBuildProgressTracker(b, start)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker build: start: %w", err)
	}

	var out strings.Builder
	var outMu sync.Mutex
	pump := func(r *bufio.Scanner, wg *sync.WaitGroup) {
		defer wg.Done()
		for r.Scan() {
			line := r.Text()
			now := time.Now()
			tracker.observe(line, now)
			outMu.Lock()
			out.WriteString(line)
			out.WriteByte('\n')
			outMu.Unlock()
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	stdoutScanner := bufio.NewScanner(stdout)
	stdoutScanner.Buffer(make([]byte, 64*1024), 1024*1024)
	stderrScanner := bufio.NewScanner(stderr)
	stderrScanner.Buffer(make([]byte, 64*1024), 1024*1024)
	go pump(stdoutScanner, &wg)
	go pump(stderrScanner, &wg)
	linesDone := make(chan struct{})
	go func() { wg.Wait(); close(linesDone) }()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			<-linesDone
			outMu.Lock()
			captured := out.String()
			outMu.Unlock()
			if err != nil {
				return []byte(captured), fmt.Errorf("docker build: %w", err)
			}
			return []byte(captured), nil
		case <-ticker.C:
			if trip := tracker.check(time.Now()); trip != nil {
				_ = cmd.Process.Kill()
				<-done
				<-linesDone
				outMu.Lock()
				captured := out.String()
				outMu.Unlock()
				return []byte(captured), fmt.Errorf("docker build hit a timeout, not a build failure: %s", trip)
			}
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			<-linesDone
			return nil, ctx.Err()
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

func TestDockerBuildProgressTracker_ASingleSlowStepDoesNotPermanentlyWidenTheCap(t *testing.T) {
	// The property that distinguishes this from an all-time-maximum bound
	// that never shrinks: crash_matrix's own progressTracker has this
	// same shape (an all-time max, not a decaying one) because a single
	// harness invocation is short enough that the distinction gotestwatch
	// needed for a whole `go test` package run does not have room to
	// matter here either. This test exists to make that a checked
	// decision rather than an unstated assumption: one slow step (the
	// alpine pull, say) genuinely does widen the bound for the REST of
	// this one build, on purpose.
	start := time.Now()
	p := newDockerBuildProgressTracker(defaultDockerBuildBounds, start)

	slow := start.Add(60 * time.Second) // one slow step: a 60s image pull
	p.observe("#2 [1/4] FROM alpine:3.20", slow)

	fast := slow.Add(1 * time.Second) // every step after it is fast
	p.observe("#3 [2/4] RUN apk add", fast)

	if got, want := p.window(), 600*time.Second; got != want {
		t.Fatalf("window after one 60s step = %s, want %s: the slow pull must still be what the window is derived from", got, want)
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
}
