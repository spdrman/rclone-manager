package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issues #538 and #540: the incident in #535, planted exactly as it
// happened, and the guard that it cannot be reported as a success.
//
// #535 was `docker exec ... backup-manager backup-set create` against a
// container that was already running the engine. The write landed in
// config.yaml, the CLI adopted it in its own memory, the CLI exited, and
// the engine, which had read that file when it started and has no watcher
// on it, never saw any of it. `sources` (a second CLI process, reading
// the file) listed both sets; the Web UI (the engine, reading its memory)
// listed one. Nothing anywhere reported a problem.
//
// So the fixture here is two processes, not two function calls. The
// engine is this test binary re-executed as `daemon` (the same trick
// daemon_signal_test.go uses, and for the same reason: it runs the same
// run() dispatch the shipped binary runs, so what it holds open is what a
// real deployment holds open), and the mutations are attempted from this
// process while that one is up.
//
// # What the guard actually asserts
//
// Not "the message says something". Two things an operator can check:
// the command did not report success, and config.yaml on disk is byte for
// byte what it was beforehand. Either one alone is weak — a command that
// refused but wrote anyway, or one that wrote nothing but exited 0, is
// still the defect — so both are asserted for every mutating verb.
//
// # And the half that keeps it honest
//
// Every case below is run twice: once with the engine up, once with
// nothing running at all. A guard that fires whichever way the world is
// arranged would strand the CLI on a bare host, which is the case the
// direct path exists for, so the second arm requires the SAME invocation
// to succeed and to change the file. That arm is the reason this table is
// a table rather than five separate refusal tests.

// engineStartTimeout bounds how long this test waits for the child's
// first log line. It is generous because the child builds nothing and
// starts nothing expensive; it only has to open a journal.
const engineStartTimeout = 60 * time.Second

// startEngineHolding re-executes this test binary as `backup-manager
// daemon --config configPath` and returns once that process has the
// journal open, plus a func that stops it and waits for it to be gone.
//
// Waiting for the daemon_start line, rather than for a duration, is what
// makes this fixture a fact rather than a hope: that line is written from
// inside app.Service.Daemon, which is reached only after openService has
// returned, which is what takes the shared journal lock. Sleeping instead
// would make every assertion below conditional on a race.
func startEngineHolding(t *testing.T, configPath string) (stop func()) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonChildProcess$")
	cmd.Env = append(os.Environ(), daemonChildEnv+"=1", daemonChildConfig+"="+configPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the engine child: %v", err)
	}

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		// Waited for, not just killed: the kernel releases the flock when
		// the process is reaped, and an assertion that "nothing is running
		// now" made against a zombie would be reading the previous state.
		_ = cmd.Wait()
	}
	t.Cleanup(stop)

	started := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"event":"daemon_start"`) {
				close(started)
				break
			}
		}
		// Drained so the child never blocks on a full pipe while this
		// test runs its commands against it.
		for sc.Scan() {
		}
	}()

	select {
	case <-started:
		return stop
	case <-time.After(engineStartTimeout):
		stop()
		t.Fatalf("the engine child never logged daemon_start within %s", engineStartTimeout)
		return stop
	}
}

// mutation is one configuration-writing invocation, named by what an
// operator would call it.
type mutation struct {
	name string
	args func(configPath, keyPath string) []string
}

// configMutations is every CLI invocation that rewrites config.yaml.
//
// It is deliberately the whole set rather than a sample. The refusal is
// wired in at one place (openBackupService, setup.go) but each command
// still has to declare that it writes, and a command that declares the
// wrong thing is exactly the defect this table exists to catch: the same
// shape as `validate` once passing withTransport=false and thereby never
// reaching a medium.
var configMutations = []mutation{
	{
		name: "backup-set create",
		args: func(configPath, keyPath string) []string {
			return createArgs(configPath, keyPath, "api/postgres")
		},
	},
	{
		name: "backup-set patch",
		args: func(configPath, _ string) []string {
			return []string{"backup-set", "--config", configPath, "patch", cliSet, "--stale-after", "48h"}
		},
	},
	{
		name: "backup-set remove",
		args: func(configPath, _ string) []string {
			return []string{"backup-set", "--config", configPath, "remove", cliSet}
		},
	},
	{
		name: "backup-set retention",
		args: func(configPath, _ string) []string {
			return retentionArgs(configPath, "--daily-days", "3", "--weekly-months", "1", "--monthly-months", "2")
		},
	},
	{
		name: "settings patch",
		args: func(configPath, _ string) []string {
			return []string{"settings", "--config", configPath, "patch", "--timezone", "America/Toronto"}
		},
	},
}

// TestAConfigurationWriteIsRefusedWhileAnEngineHoldsIt is #538's
// acceptance and #540's guard in one: with a real engine process up, no
// mutating command may report success, and none may leave config.yaml
// changed.
func TestAConfigurationWriteIsRefusedWhileAnEngineHoldsIt(t *testing.T) {
	for _, m := range configMutations {
		t.Run(m.name, func(t *testing.T) {
			configPath := writeTestConfigWithDeploymentPolicy(t)
			keyPath := writeTestPrivateKey(t)
			before := readFile(t, configPath)

			stop := startEngineHolding(t, configPath)

			var code int
			stderr := captureStderr(t, func() {
				code = run(m.args(configPath, keyPath))
			})

			if code == 0 {
				t.Errorf("%s exited 0 against a running engine; the engine never sees this write, so reporting success is the #535 defect\nstderr: %s", m.name, stderr)
			}
			if after := readFile(t, configPath); after != before {
				t.Errorf("%s changed config.yaml behind a running engine\nbefore:\n%s\nafter:\n%s", m.name, before, after)
			}
			for _, want := range []string{
				"another process",
				filepath.Join(filepath.Dir(configPath), "state.db"),
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("%s refusal does not name %q, so an operator cannot tell what it found:\n%s", m.name, want, stderr)
				}
			}
			stop()
		})
	}
}

// TestAConfigurationWriteSucceedsWhenNothingIsRunning is the other arm,
// and the reason the refusal above is worth anything: with no engine, the
// CLI is the only authority there is, so the identical invocation has to
// write and report success. A detector that answered "engine" on a bare
// host would take the direct path away from the deployment that needs it.
func TestAConfigurationWriteSucceedsWhenNothingIsRunning(t *testing.T) {
	for _, m := range configMutations {
		t.Run(m.name, func(t *testing.T) {
			configPath := writeTestConfigWithDeploymentPolicy(t)
			keyPath := writeTestPrivateKey(t)
			before := readFile(t, configPath)

			var code int
			stderr := captureStderr(t, func() {
				code = captureStdoutCode(t, func() int { return run(m.args(configPath, keyPath)) })
			})

			if code != 0 {
				t.Fatalf("%s exited %d with nothing running, want 0\nstderr: %s", m.name, code, stderr)
			}
			if after := readFile(t, configPath); after == before {
				t.Errorf("%s exited 0 with nothing running but left config.yaml unchanged, so this arm proves nothing about the write", m.name)
			}
		})
	}
}

// TestReadsAreStillAnsweredWhileAnEngineHoldsTheConfiguration keeps the
// refusal narrow. A CLI read alongside a live engine is ordinary use of
// this binary and has always been (core/service's own startup.go says so:
// the journal lock is SHARED precisely so `status` beside a `serve` keeps
// working), so nothing here may start refusing it.
func TestReadsAreStillAnsweredWhileAnEngineHoldsTheConfiguration(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	stop := startEngineHolding(t, configPath)
	defer stop()

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"settings", []string{"settings", "--config", configPath}},
		{"backup-set retention, shown", retentionArgs(configPath)},
		{"sources", []string{"sources", "--config", configPath}},
		{"artifacts", []string{"artifacts", "--config", configPath}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() {
				code = captureStdoutCode(t, func() int { return run(tc.args) })
			})
			if code != 0 {
				t.Errorf("%s exited %d beside a running engine, want 0; reads have always been allowed to share a journal\nstderr: %s", tc.name, code, stderr)
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(raw)
}

// captureStdoutCode is captureStdout for a fn that returns an exit code,
// so a command's own stdout does not land in the test log while its
// status is still the thing being asserted.
func captureStdoutCode(t *testing.T, fn func() int) int {
	t.Helper()
	var code int
	captureStdout(t, func() { code = fn() })
	return code
}
