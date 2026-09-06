package rclone

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/lib/atexit"
)

// This file is a test about exit statuses, so it cannot run in the process
// that would be exiting.
//
// The claim under test is that after DisableSignalExit, a SIGTERM is
// handled by this binary's own handler and the process leaves with the
// status that handler chose, rather than with rclone's 128+signal. Nothing
// inside a process can observe its own exit status, and a test that
// asserted the handler ran would not be the same claim: the bug (#190) was
// a RACE, where rclone's handler and the program's own both woke and
// rclone's reached os.Exit first. So every case re-executes this test
// binary as a child, in a mode named through an environment variable, and
// asserts on what the child's exit status turned out to be.
//
// Three things make that honest rather than flaky, and all three are worth
// keeping.
//
// The child announces readiness before the parent signals, so no case can
// pass or fail on having signalled a process whose handlers were not
// installed yet. The child spends real time shutting down, because both
// handlers wake on the same signal and with an instantaneous shutdown the
// winner would be a coin toss the test would then report on. And the
// -race build runs the children under a ThreadSanitizer suppression, for
// a genuine data race inside rclone's own lib/atexit that this feature
// provokes every time and that no synchronisation this repository can add
// would fix; without it the detector's exit status lands in the one number
// this test reads.

// Whether a signal ends a process, and with what status, can only be
// observed from outside that process, so these cases run in a child: this
// test binary re-executes itself with signalExitChildEnv set to one of
// the modes below.
const signalExitChildEnv = "RM_SIGNAL_EXIT_CHILD_MODE"

// childReady is printed by the child once its handlers are installed. The
// parent waits for it before signalling, so no case can pass or fail on
// having signalled a process that was not listening yet.
const childReady = "READY"

// shutdownWork is how long the child spends shutting down after the
// signal, standing in for the journal close, the alert goroutine and the
// final log line a real daemon writes there. Both handlers wake on the
// same signal, so without a shutdown that takes any time at all, which of
// them reaches the exit first would be a coin toss, and this test would
// report on the coin rather than on the code.
const shutdownWork = 250 * time.Millisecond

// atexitSuppressions is the ThreadSanitizer suppression file the children
// run under in a -race build, and its own header is where the reasoning
// lives. The short version: rclone v1.75.0's lib/atexit publishes its
// signal channel in a plain package-level variable and writes nil over it
// in IgnoreSignals, which races the read in the goroutine Register
// started. DisableSignalExit calls IgnoreSignals, so the "disabled-after"
// row below provokes that race every single time, in another module, with
// no synchronisation edge this repository can add between the two
// accesses.
//
// Without the suppression the detector's own exit status (66) lands in
// the one number this test is reading, and the row stops reporting on
// what it is about. With it, the row runs and stays honest: the child
// still runs under the detector, everything outside rclone's lib/atexit
// still fails it, and the suppression's own use is asserted below.
const atexitSuppressions = "testdata/rclone-atexit-race.supp"

// suppressionMatchPrefix is what ThreadSanitizer prints at exit under
// print_suppressions=1 when at least one suppression fired, and prints
// not at all when none did. Both directions are load-bearing here.
const suppressionMatchPrefix = "ThreadSanitizer: Matched"

// TestSignalExitChildProcess is not a test. It is the entry point of the
// child processes TestDisableSignalExit starts, and it skips itself in an
// ordinary run.
func TestSignalExitChildProcess(t *testing.T) {
	mode := os.Getenv(signalExitChildEnv)
	if mode == "" {
		t.Skip("child-process entry point: only runs when a parent test re-executes this binary")
	}

	// Registering an at-exit function is what makes the embedded rclone
	// install its own SIGINT/SIGTERM handler, and it is not a contrivance:
	// fs/operations does exactly this around every copy that is not
	// in-place, to remove a partial file if the process is killed. One
	// transfer through this adapter is therefore enough to arm it for the
	// rest of the process's life, because unregistering the function does
	// not stop the handler.
	switch mode {
	case "unguarded":
		atexit.Register(func() {})
	case "disabled-first":
		DisableSignalExit()
		atexit.Register(func() {})
	case "disabled-after":
		atexit.Register(func() {})
		DisableSignalExit()
	default:
		fmt.Fprintf(os.Stderr, "unknown child mode %q\n", mode)
		os.Exit(99)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println(childReady)
	<-ctx.Done()
	time.Sleep(shutdownWork)
	os.Exit(0)
}

// TestDisableSignalExit proves what DisableSignalExit is for, in both
// directions.
//
// The "unguarded" row is the negative control, and the reason the fix is
// not a no-op: a process that handles SIGTERM itself, cancels its context
// and returns 0 still leaves with 143, because the embedded rclone's
// lib/atexit handler calls os.Exit(128+signal) out from under it. That is
// what an operator's `systemctl stop` or `docker stop` reads as a crash.
//
// The other two rows are the fix, in the two orders it can be reached in:
// before rclone has ever registered anything (which is what
// cmd/backup-manager's daemon does, at startup, before its first
// transfer) and after it already has (which is what a later call, or any
// reordering of that startup, would hit). Both have to hold, because
// lib/atexit installs its handler lazily on the first registration and
// stops it a different way once it exists.
func TestDisableSignalExit(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		wantExit int
		// wantAtexitRace is whether this row provokes the data race in
		// rclone's lib/atexit that testdata/rclone-atexit-race.supp
		// describes. Only the row that calls IgnoreSignals after
		// something has registered does, and asserting the other two do
		// NOT is what keeps that suppression from quietly covering
		// anything else in this package.
		wantAtexitRace bool
	}{
		{
			name:     "rclone exits the process for us when it is left alone",
			mode:     "unguarded",
			wantExit: 143,
		},
		{
			name:     "disabled before rclone registers anything",
			mode:     "disabled-first",
			wantExit: 0,
		},
		{
			name:           "disabled after rclone has already registered",
			mode:           "disabled-after",
			wantExit:       0,
			wantAtexitRace: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, stderr := runSignalExitChild(t, tc.mode)
			if got != tc.wantExit {
				t.Errorf("a child in %q mode exited %d after SIGTERM, want %d\nchild stderr:\n%s", tc.mode, got, tc.wantExit, stderr)
			}
			if !raceDetectorEnabled {
				return
			}
			// Under the detector the child runs with the suppression
			// file and print_suppressions=1, so it says at exit whether
			// anything was suppressed. Both answers are assertions.
			matched := strings.Contains(stderr, suppressionMatchPrefix)
			switch {
			case tc.wantAtexitRace && !matched:
				t.Errorf("this row provokes a data race in rclone's lib/atexit and the suppression in %s was not used, so either the row stopped provoking it or rclone has fixed it upstream.\n"+
					"Check which, and if it is fixed, delete that file and this expectation rather than leaving a suppression nobody can account for.\nchild stderr:\n%s",
					atexitSuppressions, stderr)
			case !tc.wantAtexitRace && matched:
				t.Errorf("this row is not supposed to need %s, and something in it was suppressed. A suppression that covers a row nobody wrote it for is a race nobody is looking at.\nchild stderr:\n%s",
					atexitSuppressions, stderr)
			}
		})
	}
}

// childGORACE is the GORACE setting the children run under in a -race
// build, and it is empty in every other build.
//
// suppressions points at testdata/rclone-atexit-race.supp, whose header
// carries the whole reason it exists. print_suppressions=1 is what makes
// this honest rather than convenient: ThreadSanitizer then says at exit
// what it suppressed, TestDisableSignalExit asserts that exactly the row
// that provokes rclone's race used it and no other row did, and the day
// rclone fixes the race upstream that assertion goes red and the file
// gets deleted.
func childGORACE(t *testing.T) string {
	t.Helper()
	if !raceDetectorEnabled {
		return ""
	}
	abs, err := filepath.Abs(atexitSuppressions)
	if err != nil {
		t.Fatalf("resolving %s: %v", atexitSuppressions, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("the suppression file this test runs its children under is not there: %v", err)
	}
	return "suppressions=" + abs + " print_suppressions=1"
}

// lockedBuffer collects a child's stderr. The write happens on os/exec's
// own goroutine and the read happens here, so it is exactly the shape the
// detector this file is now run under exists to complain about.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runSignalExitChild starts one child in mode, waits for it to say it is
// listening, sends it SIGTERM and returns the status it left with,
// together with everything it wrote to stderr.
//
// The stderr is not only for diagnostics: in a -race build it carries
// ThreadSanitizer's own report of what it suppressed, which is what
// TestDisableSignalExit reads to keep testdata/rclone-atexit-race.supp
// accounted for.
func runSignalExitChild(t *testing.T, mode string) (int, string) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalExitChildProcess$")
	// GORACE is dropped from the inherited environment rather than
	// appended to: a duplicate key in an exec environment is resolved by
	// whoever reads it first, which is not a thing to leave to chance
	// when the value decides whether this test can read the child's exit
	// status at all.
	cmd.Env = append(withoutEnv(os.Environ(), "GORACE"), signalExitChildEnv+"="+mode)
	if gorace := childGORACE(t); gorace != "" {
		cmd.Env = append(cmd.Env, "GORACE="+gorace)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == childReady {
				ready <- true
				return
			}
		}
		ready <- false
	}()

	select {
	case ok := <-ready:
		if !ok {
			t.Fatalf("the child exited before it was listening for signals\nchild stderr:\n%s", stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the child never reported that it was listening for signals\nchild stderr:\n%s", stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		if waitErr == nil {
			return 0, stderr.String()
		}
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("waiting for the child: %v\nchild stderr:\n%s", waitErr, stderr.String())
		}
		return exitErr.ExitCode(), stderr.String()
	case <-time.After(30 * time.Second):
		t.Fatalf("the child never exited after SIGTERM\nchild stderr:\n%s", stderr.String())
		return -1, ""
	}
}

// withoutEnv returns env with every VAR=... entry for name removed.
func withoutEnv(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
