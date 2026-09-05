// What the process actually exits with when it is signalled, which no
// in-process test can observe.
//
// A signal handler's whole contract is about the process rather than about
// a function's return value, so this re-executes the test binary as the
// engine, sends it a real SIGTERM and reads the exit status and the log
// line the operator would see. The environment variables are what turn
// this binary into that child; the same trick the core daemon's own signal
// test uses, and for the same reason: the child runs the same run() the
// shipped binary dispatches to, so what it exits with is what the shipped
// binary exits with.
//
// The shutdown notice is pinned as a literal here rather than imported
// from the command. That looks like duplication and is deliberate: what is
// under test is the text an operator finds in the container log, so an
// assertion that imported the constant would keep passing through a
// rewording that breaks whoever is grepping for it.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// `serve`'s exit status after a signal is a property of the process, not
// of a function call, so it can only be observed from outside one. These
// three environment variables turn this test binary into the engine when
// the test below re-executes it, the same trick
// core/cmd/backup-manager/daemon_signal_test.go uses for `daemon`, and
// for the same reason: the child runs the same run() main dispatches to,
// so what it exits with is what the shipped binary exits with.
const (
	serveChildEnv       = "BACKUP_MANAGER_WEB_TEST_SERVE_CHILD"
	serveChildConfig    = "BACKUP_MANAGER_WEB_TEST_SERVE_CONFIG"
	serveChildAuthStore = "BACKUP_MANAGER_WEB_TEST_SERVE_AUTH_STORE"
)

// serveShutdownNotice is the line `serve` prints once its own shutdown
// has actually finished. Spelled out here as a literal rather than
// imported from the command, exactly the way
// core/cmd/backup-manager/daemon_signal_test.go pins "daemon_stop": what
// this test is about is what an operator reading the container's logs
// sees, so the assertion has to be against the text itself.
const serveShutdownNotice = "backup-manager-web: shutdown complete"

// TestServeChildProcess is not a test. It is the entry point of the child
// process TestServe_SIGTERMIsASuccessfulStop starts, and it skips itself
// in an ordinary run.
//
// Port 0 is deliberate: this test never connects to the child, it only
// signals it, and asking the kernel for a free port is what keeps the
// test from failing on a developer's machine that happens to be serving
// something on the default one.
func TestServeChildProcess(t *testing.T) {
	if os.Getenv(serveChildEnv) != "1" {
		t.Skip("child-process entry point: only runs when a parent test re-executes this binary")
	}
	os.Exit(run([]string{
		"serve",
		"--config", os.Getenv(serveChildConfig),
		"--listen", "127.0.0.1:0",
		"--auth-store", os.Getenv(serveChildAuthStore),
	}))
}

// writeServeTestConfig mirrors core/cmd/backup-manager/main_test.go's own
// writeTestConfig, with one thing added on purpose: a real file sitting
// on the (local-backend) remote, so the first scheduled cycle actually
// transfers something. See the test below for why that matters.
func writeServeTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("web host signal test payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "state.db")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// TestServe_SIGTERMIsASuccessfulStop is issue #212, which is issue #190's
// defect in the second binary, where it matters more.
//
// core/service.Open wires a real rclone transport and drives real backup
// cycles in this process, so `serve` embeds rclone exactly the way
// `daemon` does. rclone's lib/atexit installs its own SIGINT/SIGTERM
// handler the first time anything registers an at-exit function, and
// fs/operations registers one around every non-inplace copy;
// unregistering it when the copy finishes does not remove the handler, so
// the first transfer arms it for the life of the process. When the signal
// arrives that handler ends the process with os.Exit(128+signal), which
// is 143 for SIGTERM. Docker, Kubernetes and systemd all read a nonzero
// exit on stop as a failure, so every routine `docker stop` of the web
// container looked like a crash, counted against restart burst limits and
// alerted.
//
// # Why this waits for a commit before signalling
//
// Exit 0 on its own is a vacuous assertion here: a process that has never
// transferred anything has never armed rclone's handler, so it would exit
// 0 whether or not this were fixed. Waiting for the first cycle's commit
// event on the child's own stdout is what proves the handler really was
// armed at the moment the signal landed.
// core/internal/transport/rclone's own TestDisableSignalExit is the
// direct, both-arms proof of the mechanism itself.
//
// # And why exit 0 alone is still not the whole assertion
//
// The other half of the defect is that the process left when rclone got
// there rather than when the shutdown it was asked to perform had
// finished, so anything the graceful path still had to do (stopping the
// scheduler, draining the HTTP server, closing the state store) might
// never have run. os.Exit runs no deferred function, so the shutdown line
// cmdServe prints from its own deferred close is only reachable on the
// path that really did finish shutting down.
func TestServe_SIGTERMIsASuccessfulStop(t *testing.T) {
	cfg := writeServeTestConfig(t)
	authStore := filepath.Join(t.TempDir(), "local-auth.json")

	cmd := exec.Command(os.Args[0], "-test.run=^TestServeChildProcess$")
	cmd.Env = append(os.Environ(),
		serveChildEnv+"=1",
		serveChildConfig+"="+cfg,
		serveChildAuthStore+"="+authStore,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the serve child: %v", err)
	}
	// So a t.Fatal below can never leave an engine running against this
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
			t.Fatalf("the serve child never finished within 90s\nstdout:\n%s\nstderr:\n%s",
				strings.Join(log, "\n"), stderr.String())
		}
	}

	waitErr := cmd.Wait()
	code := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("waiting for the serve child: %v", waitErr)
		}
		code = exitErr.ExitCode()
	}

	if !signalled {
		t.Fatalf("the serve child exited before it committed anything, so nothing was ever signalled and rclone's handler was never armed\nstdout:\n%s\nstderr:\n%s",
			strings.Join(log, "\n"), stderr.String())
	}
	if code != 0 {
		t.Errorf("serve exited %d after a SIGTERM it handled, want 0 (143 is 128+SIGTERM, which is what a process that never handled the signal reports)\nstdout:\n%s\nstderr:\n%s",
			code, strings.Join(log, "\n"), stderr.String())
	}
	if !strings.Contains(stderr.String(), serveShutdownNotice) {
		t.Errorf("serve never said it finished shutting down (%q), so the stop cut the graceful path short rather than completing it\nstderr:\n%s",
			serveShutdownNotice, stderr.String())
	}
	// The positive control for the assertion above: the startup line
	// proves this test really does read the child's stderr, so a missing
	// shutdown notice is a missing line rather than a pipe that carried
	// nothing at all.
	if !strings.Contains(stderr.String(), "backup-manager-web: runtime profile") {
		t.Errorf("serve never logged its startup line either, so this test read no stderr at all\nstderr:\n%s", stderr.String())
	}
}
