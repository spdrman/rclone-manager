package machines

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// This file gives the source machine's image build a progress-derived
// timeout instead of a fixed one (issue #309).
//
// It was core/internal/transport/rclone/dockerbuild_test.go until #448 and
// #450. It wrapped buildSFTPFixtureImage, which built an image inside six
// separate test functions; that per-test build is gone, and what wraps this
// watchdog now is ensureSourceImage, the harness's one build per daemon.
// The watchdog is still worth having for it: one build is still a build,
// and a five-minute flat ceiling on a 4 CPU Docker VM under four concurrent
// gate lanes is exactly the number #309 says cannot tell busy from stuck. That fixed budget was wrong for the same reason #247 and
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
// image the source machine Dockerfile describes (one alpine base, one apk
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

// Bytes is a copy, not bytes.Buffer's own backing array: callers keep the
// transcript around to print in a failure message, and handing them a
// slice that a still-running Write can append into behind their back is a
// data race waiting for the one case where the build has not finished
// yet.
func (l *dockerBuildLineTap) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.all.Bytes())
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
