package rclone

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/lib/atexit"
)

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
			name:     "disabled after rclone has already registered",
			mode:     "disabled-after",
			wantExit: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := runSignalExitChild(t, tc.mode)
			if got != tc.wantExit {
				t.Errorf("a child in %q mode exited %d after SIGTERM, want %d", tc.mode, got, tc.wantExit)
			}
		})
	}
}

// runSignalExitChild starts one child in mode, waits for it to say it is
// listening, sends it SIGTERM and returns the status it left with.
func runSignalExitChild(t *testing.T, mode string) int {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalExitChildProcess$")
	cmd.Env = append(os.Environ(), signalExitChildEnv+"="+mode)
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
			t.Fatal("the child exited before it was listening for signals")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the child never reported that it was listening for signals")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		if waitErr == nil {
			return 0
		}
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("waiting for the child: %v", waitErr)
		}
		return exitErr.ExitCode()
	case <-time.After(30 * time.Second):
		t.Fatal("the child never exited after SIGTERM")
		return -1
	}
}
