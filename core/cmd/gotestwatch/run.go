// Running `go test -json` under the tracker: the process, the stream, and
// putting the human-readable output back together.
//
// The JSON stream is not a preference, it is the only way to know what is
// still running. A plain `go test` prints a package's output when the
// package finishes, so a run that hangs prints nothing at all and the
// watchdog would have nothing to name; -json emits an event per test action
// as it happens, which is both the progress signal the tracker needs and the
// list of tests to blame when the window closes.
//
// Reconstructing the ordinary output from those events is what keeps the
// switch invisible to a caller. Each event carries the exact bytes the test
// binary printed, so what a person reads on the terminal is what `go test -v`
// would have shown, and no caller has to learn to read JSON to use the
// watchdog.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// jsonEvent is `go test -json`'s per-line schema (see the standard
// library's cmd/internal/test2json), trimmed to the fields gotestwatch
// uses. Output carries the exact bytes the test binary itself printed
// (including the "=== RUN"/"--- PASS" framing `-v` produces), which is
// what lets gotestwatch reconstruct ordinary human-readable `go test -v`
// output from the JSON stream instead of forcing every caller to read
// JSON.
type jsonEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// Options configures one watched `go test` invocation.
type Options struct {
	Dir    string   // working directory for `go test`; "" means the caller's own
	Args   []string // arguments after "go test" (package paths, -count=N, ...)
	Bounds bounds
	Poll   time.Duration
	Stdout io.Writer // reconstructed human-readable `go test -v` output
	Stderr io.Writer // raw passthrough of the child's own stderr, plus gotestwatch's own messages
}

// Result is what a completed or tripped invocation produced.
type Result struct {
	ExitCode int
	Trip     *trip

	// What the tracker had measured by the time Run returned: the same
	// four numbers the summary line on stderr carries. A trip already
	// carries its own snapshot of these, taken at the instant the
	// decision was made, so read res.Trip for a tripped run; these are
	// here for the runs that did NOT trip, where nothing but prose on
	// stderr used to say how close the run came to its own window.
	//
	// Issue #401 is what needed them programmatically. A test that
	// plants a stall and expects the watchdog to notice it has two very
	// different reasons to see no trip: the watchdog stopped working, or
	// the host had already produced a gap longer than the planted stall,
	// so the derived window was wider than the stall before the stall
	// even began. Only these numbers separate the two, and parsing the
	// prose above for them is exactly the fragility this replaces.
	Events       int
	SlowestStep  time.Duration
	SlowestLabel string
	Window       time.Duration
}

// reapWait bounds how long killAndWait waits for a killed process group to
// actually exit. SIGKILL cannot terminate a process stuck in
// uninterruptible kernel I/O wait (D state), a real possibility for
// exactly the class of hang this tool targets (stuck Docker/SFTP I/O);
// without this bound, a trip that had already correctly diagnosed the
// problem would reintroduce, one layer further down, the exact "hangs
// forever with no bound" failure mode issues #247/#248/#256 exist to
// close. Fixed, not derived like the tracker's own bounds: at this point
// a trip has already been decided, so there is nothing left to derive a
// bound from, and an operator seeing a report a few extra seconds late is
// far preferable to gotestwatch itself hanging with no report at all.
const reapWait = 15 * time.Second

// killAndWait sends SIGKILL to the process group pgid, then waits up to
// reapWait for waited (fed by a goroutine blocked on cmd.Wait(), as Run
// sets up) to report the child's exit. reaped is false if the group did
// not exit within that bound, in which case waitErr is always nil and the
// caller's cmd.Wait() goroutine is left running: it is harmless on its
// own, since gotestwatch itself is about to report the trip and exit, and
// there is no portable way to abandon a Wait() already blocked in the
// kernel on a process that will not reap.
func killAndWait(pgid int, waited <-chan error, reapWait time.Duration) (waitErr error, reaped bool) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case waitErr = <-waited:
		return waitErr, true
	case <-time.After(reapWait):
		return nil, false
	}
}

// forbiddenGoTestFlag reports the -timeout/-json flag gotestwatch always
// owns itself, if arg is (a prefix of) one, so a caller passing either is
// refused loudly instead of silently overridden or duplicated. go test's
// own flag parsing takes the LAST occurrence of a flag, and gotestwatch
// always puts its own -json/-timeout=0 first, so a caller-supplied
// -timeout would otherwise reintroduce the exact fixed deadline issue
// #256 is about, without anything here noticing.
func forbiddenGoTestFlag(arg string) string {
	for _, name := range []string{"timeout", "json"} {
		if arg == "-"+name || arg == "--"+name ||
			strings.HasPrefix(arg, "-"+name+"=") || strings.HasPrefix(arg, "--"+name+"=") {
			return name
		}
	}
	return ""
}

// lineTap splits a byte stream into lines and hands each complete one to
// onLine as it arrives, verbatim (no trailing newline). os/exec gives a
// non-*os.File writer its own copying goroutine, so each of the child's
// writes reaches onLine at the instant it happens rather than whenever a
// buffer happens to fill; tests/crashmatrix's own runHarnessWatched relies
// on the identical property for the same reason.
type lineTap struct {
	onLine func(line string)

	mu      sync.Mutex
	partial []byte
}

func (l *lineTap) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
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

// Run executes `go test -json -timeout=0 <opts.Args...>` under a derived
// no-progress watchdog (see tracker.go and doc.go) instead of go test's
// own fixed -timeout, and reconstructs ordinary -v output on opts.Stdout
// as it goes.
//
// The child runs in its own process group (Setpgid), and a trip kills the
// whole group, not just the immediate `go test` process: go test's own
// -timeout panics the test binary without touching anything it spawned,
// which is exactly the orphan risk this replaces. See doc.go.
func Run(opts Options) (Result, error) {
	for _, arg := range opts.Args {
		if name := forbiddenGoTestFlag(arg); name != "" {
			return Result{}, fmt.Errorf("gotestwatch already sets -%s itself; remove %q from the arguments passed to it", name, arg)
		}
	}

	goArgs := append([]string{"test", "-json", "-timeout=0"}, opts.Args...)
	cmd := exec.Command("go", goArgs...)
	cmd.Dir = opts.Dir
	// GOWORK=off regardless of the caller's own environment: opts.Dir is
	// routinely a directory deliberately excluded from this repo's
	// go.work (core/'s real packages under CI_LOCAL_FAST, or a testdata
	// fixture module under a unit test), and Go's workspace auto-discovery
	// walks upward from cmd.Dir looking for one, so an ambient go.work
	// this child was never meant to join would otherwise make module
	// resolution fail before go test can even start.
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	tr := newTracker(opts.Bounds, start)

	stdout := &lineTap{onLine: func(line string) {
		observeLine(tr, line, opts.Stdout)
	}}
	cmd.Stdout = stdout

	stderr := &lineTap{onLine: func(line string) {
		// go test's own stderr is normally empty once -json is set (build
		// errors are wrapped as build-output/build-fail events on
		// stdout instead), but anything that does land here is still
		// real activity, and passing it straight through keeps a
		// caller's expectations about where compiler/toolchain noise
		// goes intact.
		tr.observe(testEvent{Action: "stderr"}, time.Now())
		if opts.Stderr != nil {
			_, _ = fmt.Fprintln(opts.Stderr, line)
		}
	}}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("starting go test: %w", err)
	}
	pgid := cmd.Process.Pid

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	poll := opts.Poll
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var (
		waitErr error
		tripped *trip
	)
watch:
	for {
		select {
		case waitErr = <-waited:
			break watch
		case now := <-ticker.C:
			if tripped = tr.check(now); tripped != nil {
				// Kill the whole group, then still wait: a watchdog
				// that leaves the tree it gave up on running behind
				// defeats the one correctness property (see doc.go)
				// that motivated owning process-group membership at
				// all. That wait is itself bounded (see killAndWait):
				// SIGKILL cannot terminate a process stuck in
				// uninterruptible kernel I/O wait, and this trip has
				// already been correctly diagnosed, so it is reported
				// either way rather than risking gotestwatch itself
				// hanging forever on the reap.
				var reaped bool
				waitErr, reaped = killAndWait(pgid, waited, reapWait)
				if !reaped {
					tripped.reapTimedOut = true
					tripped.reapWait = reapWait
				}
				break watch
			}
		}
	}

	// What the run measured itself at, on every invocation, not just a
	// tripped one: go test's own summary line does not say how close a
	// run came to gotestwatch's derived window, and that is the number an
	// operator needs to trust "ok" rather than just read it (mirrors
	// tests/crashmatrix's own t.Logf for the identical reason). It goes
	// on Result as well as on stderr so a caller can act on it instead of
	// parsing the sentence (see Result).
	events, slowest, label, window := tr.summary()
	res := Result{Events: events, SlowestStep: slowest, SlowestLabel: label, Window: window}
	if tripped != nil {
		res.Trip = tripped
		return res, nil
	}
	if opts.Stderr != nil {
		_, _ = fmt.Fprintf(opts.Stderr, "gotestwatch: %d events observed, slowest gap %s (%s), no-progress window %s\n",
			events, slowest.Round(time.Millisecond), label, window.Round(time.Millisecond))
	}
	if waitErr == nil {
		return res, nil
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, fmt.Errorf("running go test: %w", waitErr)
}

// observeLine parses one line of `go test -json` output, feeds it to the
// tracker, and, for an "output" or "build-output" action, writes its exact
// text to dst: those two actions are the only ones that carry the child's
// own printed bytes, so replaying just them reconstructs ordinary
// `go test -v` output byte for byte. A line that fails to parse as JSON
// (defensive: should not happen with -json set) is still counted as
// progress and still passed through, so a surprise never turns into a
// silent hang on gotestwatch's own part.
func observeLine(tr *tracker, line string, dst io.Writer) {
	now := time.Now()
	var ev jsonEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Action == "" {
		tr.observe(testEvent{Action: "unparsed"}, now)
		if dst != nil {
			_, _ = fmt.Fprintln(dst, line)
		}
		return
	}
	detail := ""
	if ev.Action == "output" || ev.Action == "build-output" {
		detail = strings.TrimSpace(ev.Output)
		if len(detail) > 40 {
			detail = detail[:40] + "…"
		}
	}
	tr.observe(testEvent{Action: ev.Action, Package: ev.Package, Test: ev.Test, Detail: detail}, now)
	if ev.Action != "output" && ev.Action != "build-output" {
		return
	}
	if dst == nil {
		return
	}
	// Output already carries its own trailing newline (test2json's
	// contract); Fprint, not Fprintln, keeps a byte-for-byte replay.
	_, _ = fmt.Fprint(dst, ev.Output)
}
