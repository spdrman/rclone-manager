package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The daemon's exit status after a signal is a property of the process,
// not of a function call, so it can only be observed from outside one.
// These two environment variables turn this test binary into the daemon
// when the test below re-executes it, which is cheaper and more faithful
// than shelling out to `go build`: the child runs the same run() main
// dispatches to, so what it exits with is what the shipped binary exits
// with.
const (
	daemonChildEnv    = "BACKUP_MANAGER_TEST_DAEMON_CHILD"
	daemonChildConfig = "BACKUP_MANAGER_TEST_DAEMON_CONFIG"
)

// TestDaemonChildProcess is not a test. It is the entry point of the
// child process TestDaemon_SIGTERMIsASuccessfulStop starts, and it skips
// itself in an ordinary run.
func TestDaemonChildProcess(t *testing.T) {
	if os.Getenv(daemonChildEnv) != "1" {
		t.Skip("child-process entry point: only runs when a parent test re-executes this binary")
	}
	os.Exit(run([]string{"daemon", "--config", os.Getenv(daemonChildConfig)}))
}

// TestDaemon_SIGTERMIsASuccessfulStop is issue #190.
//
// FR-1 asks the daemon to handle SIGTERM/SIGINT through context
// cancellation and shut down. A stop the operator asked for, and that the
// daemon performed, is a successful stop, and systemd, Docker and
// Kubernetes all read the exit status to decide whether an ordinary stop
// was a crash: 143 (128 plus SIGTERM) marks a unit failed unless
// SuccessExitStatus=143 is configured, counts against the restart burst
// limit, and alerts. docs/deployment.md documents this binary as a
// long-lived service, so that is a routine event rather than a rare one.
//
// The test waits for the first cycle's commit before signalling, and that
// wait is what makes it a real test rather than a vacuous one: the
// embedded rclone only installs the lib/atexit signal handler that used
// to exit this process with 143 once something has actually copied a file
// through it (fs/operations' copy registers a partial-file cleanup, and
// registering is what starts atexit's handler goroutine). Signalling a
// daemon that had not yet transferred anything would exit 0 whether or
// not the bug were fixed. internal/transport/rclone's own
// TestDisableSignalExit is the direct, both-arms proof of that mechanism.
//
// The shutdown log line is asserted here too, and deliberately over a
// pipe rather than a file. Losing it was the second half of the same
// finding: whoever exits the process first decides whether the line is
// ever written, and "the operator stopped it" versus "it died" is exactly
// what a reader of those logs needs afterwards (FR-23).
func TestDaemon_SIGTERMIsASuccessfulStop(t *testing.T) {
	cfg := writeTestConfig(t)

	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonChildProcess$")
	cmd.Env = append(os.Environ(), daemonChildEnv+"=1", daemonChildConfig+"="+cfg)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the daemon child: %v", err)
	}
	// So a t.Fatal below can never leave a daemon running against this
	// test's temp directory.
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := make(chan string, 128)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	var log []string
	signalled := false
	deadline := time.After(90 * time.Second)

reading:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break reading
			}
			log = append(log, line)
			if !signalled && strings.Contains(line, `"event":"commit"`) {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatalf("sending SIGTERM: %v", err)
				}
				signalled = true
			}
		case <-deadline:
			_ = cmd.Process.Kill()
			t.Fatalf("the daemon child never finished within 90s\nstdout:\n%s\nstderr:\n%s",
				strings.Join(log, "\n"), stderr.String())
		}
	}

	waitErr := cmd.Wait()
	code := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("waiting for the daemon child: %v", waitErr)
		}
		code = exitErr.ExitCode()
	}

	if !signalled {
		t.Fatalf("the daemon child exited before it committed anything, so nothing was ever signalled\nstdout:\n%s\nstderr:\n%s",
			strings.Join(log, "\n"), stderr.String())
	}
	if code != 0 {
		t.Errorf("the daemon exited %d after a SIGTERM it handled, want 0 (143 is 128+SIGTERM, which is what a process that never handled the signal reports)\nstdout:\n%s\nstderr:\n%s",
			code, strings.Join(log, "\n"), stderr.String())
	}
	if !containsEvent(log, "daemon_stop") {
		t.Errorf("the daemon never logged its own shutdown over the pipe, want a daemon_stop event (FR-23)\nstdout:\n%s",
			strings.Join(log, "\n"))
	}
	// The positive control for the assertion above: daemon_start proves
	// this reader really does see the child's NDJSON, so a missing
	// daemon_stop is a missing line rather than a broken pipe.
	if !containsEvent(log, "daemon_start") {
		t.Errorf("the daemon never logged daemon_start either, so this test read nothing at all\nstdout:\n%s",
			strings.Join(log, "\n"))
	}
}

func containsEvent(log []string, event string) bool {
	needle := `"event":"` + event + `"`
	for _, line := range log {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
