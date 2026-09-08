package main

import (
	"bufio"
	"io"
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
// byte what it was beforehand. Either one alone is weak, because a
// command that refused but wrote anyway, or one that wrote nothing but
// exited 0, is still the defect. Both are asserted for every mutating
// verb.
//
// The first of those is asserted as an exact status rather than as "not
// zero", which is issue #551. Exit 3 is this refusal and nothing else, so
// a script can wait on it and abort on everything else, and every case
// here that is NOT this refusal pins its own code for the same reason:
// the probe that could not be performed stays at 1 and the usage mistake
// typed beside a running engine stays at 2. A suite that only proved the
// refusal exits 3 could not tell a working binary from one that exits 3
// for everything.
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
	// prepare puts the configuration into the state this invocation needs
	// in order to change anything at all, with nothing running. Only
	// --inherit needs one: clearing a policy off a set that never had one
	// rewrites nothing, so without this the "with nothing running it
	// succeeds AND changes the file" arm would be asserting about a
	// command that never had a change to make.
	prepare func(t *testing.T, configPath string)
}

// configMutations is every CLI invocation that rewrites config.yaml
// THROUGH openBackupService.
//
// That qualifier is the whole of what this list can honestly claim, and
// it used to claim more. `backup-set create` against a configuration path
// that does not exist takes createFirstConfig instead, which reaches
// core/service.FirstRun and never comes near this door, and it is covered
// by its own test below rather than by this table. So is `backup-set
// retention --policy-file -`, which has to be driven through a real
// stdin. A list that says "every invocation that rewrites config.yaml"
// while omitting either of those is the kind of confident enumeration
// this repository has been bitten by before.
//
// Within that door it is deliberately the whole set rather than a sample.
// The refusal is wired in at one place (openBackupService, setup.go) but
// each command still has to declare that it writes, and a command that
// declares the wrong thing is exactly the defect this table exists to
// catch: the same shape as `validate` once passing withTransport=false
// and thereby never reaching a medium.
//
// # What it proves after #543, which is less than it used to and still the
// thing that matters
//
// Four of these six can now be handed to a running engine, so the refusal
// they get here is the one for an engine this command was told nothing
// about. That is exactly the condition under test: nothing in this file
// sets $BACKUP_MANAGER_API_URL, and TestMain clears it, so what every case
// below asserts is that finding an engine and having no way to reach it
// still refuses and still leaves the file alone. It is the case a
// `backup-manager daemon` is always in, since a daemon serves no HTTP at
// all, and the one an operator who has set nothing up is in.
//
// The other half, that a routed write reaches the engine and does not
// touch this file, is engineroute_test.go's.
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
		name: "backup-set retention --inherit",
		args: func(configPath, _ string) []string {
			return retentionArgs(configPath, "--inherit")
		},
		prepare: func(t *testing.T, configPath string) {
			t.Helper()
			args := retentionArgs(configPath, "--daily-days", "5", "--weekly-months", "2", "--monthly-months", "3")
			captureStdout(t, func() {
				if got := run(args); got != 0 {
					t.Fatalf("seeding an override with %v = %d, want 0", args, got)
				}
			})
		},
	},
	{
		name: "settings patch",
		args: func(configPath, _ string) []string {
			return []string{"settings", "--config", configPath, "patch", "--timezone", "America/Toronto"}
		},
	},
	{
		// Issue #595: the deployment's whole chain, which this command
		// used to refuse outright. It belongs in this table rather than
		// only in settingspolicyfile_test.go because it is a
		// CONFIGURATION WRITE, and the two arms below are what prove it
		// is treated as one: refused beside a serving process with the
		// file untouched, and written when nothing is running. A
		// --policy-file that reached config.yaml behind a live engine
		// would be #535 again, on a surface that did not exist when #535
		// was found.
		name: "settings patch --policy-file",
		args: func(configPath, _ string) []string {
			return []string{"settings", "--config", configPath, "patch", "--policy-file", deploymentPolicyFilePath(configPath)}
		},
		prepare: func(t *testing.T, configPath string) {
			t.Helper()
			// A chain, so it replaces the fixture's legacy scalars and
			// the file really does change on the arm that expects it. No
			// medium, because what is under test here is the route
			// rather than the destination.
			body := "tiers:\n  - name: daily\n    granularity: day\n    keep: 9\n"
			if err := os.WriteFile(deploymentPolicyFilePath(configPath), []byte(body), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
		},
	},
}

// deploymentPolicyFilePath is where the row above puts its policy file:
// beside the configuration it patches, so both arms reach the same path
// from the one argument the table hands them.
func deploymentPolicyFilePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "deployment-policy.yaml")
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
			if m.prepare != nil {
				m.prepare(t, configPath)
			}
			before := readFile(t, configPath)

			stop := startEngineHolding(t, configPath)

			var code int
			stderr := captureStderr(t, func() {
				code = run(m.args(configPath, keyPath))
			})

			if code != exitEngineHoldsDeployment {
				t.Errorf("%s exited %d against a running engine, want %d; 0 would be the #535 defect the engine never sees, and any other non-zero leaves a script unable to tell this refusal from a deployment that is actually broken\nstderr: %s",
					m.name, code, exitEngineHoldsDeployment, stderr)
			}
			if after := readFile(t, configPath); after != before {
				t.Errorf("%s changed config.yaml behind a running engine\nbefore:\n%s\nafter:\n%s", m.name, before, after)
			}
			for _, want := range []string{
				"another process",
				filepath.Join(filepath.Dir(configPath), "state.db"),
				// The half an operator acts on. "This was refused" and
				// "this was refused and your file is untouched" are
				// different pieces of news, and the second one is the
				// only one that tells somebody staring at a failed
				// script whether they have to go and check the file.
				"nothing was written",
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
			if m.prepare != nil {
				m.prepare(t, configPath)
			}
			before := readFile(t, configPath)

			var code int
			stderr := captureStderr(t, func() {
				code = captureStdoutCode(t, func() int { return run(m.args(configPath, keyPath)) })
			})

			if code != exitOK {
				t.Fatalf("%s exited %d with nothing running, want %d\nstderr: %s", m.name, code, exitOK, stderr)
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
			if code != exitOK {
				t.Errorf("%s exited %d beside a running engine, want %d; reads have always been allowed to share a journal\nstderr: %s", tc.name, code, exitOK, stderr)
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

// TestAConfigurationWriteIsRefusedWhenTheEngineArrivesWhileStdinIsStillBeingRead
// is the window the check used to leave open, and it is an operator-sized
// one rather than a microsecond.
//
// `backup-set retention --policy-file -` reads its policy from standard
// input. When the engine check ran once, at the top of openBackupService,
// everything after it happened unguarded: the startup sequence, and then
// a read of stdin that finishes whenever the thing on the other end
// finishes, which on a fifo or in a pipeline is whenever the operator
// says. So the sequence below (nothing running when the command starts, an
// engine up before the policy arrives) wrote config.yaml behind a live
// engine and exited 0, which is #535 straight through the new guard.
//
// The fixture makes that ordering a fact rather than a hope: the pipe is
// closed only after startEngineHolding has seen the child's daemon_start
// line, so the write this command performs cannot possibly precede the
// engine.
func TestAConfigurationWriteIsRefusedWhenTheEngineArrivesWhileStdinIsStillBeingRead(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	before := readFile(t, configPath)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	realStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = realStdin
		_ = r.Close()
	})

	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int {
			done := make(chan int, 1)
			go func() { done <- run(retentionArgs(configPath, "--policy-file", "-")) }()

			// The engine comes up while the command is still parked on
			// the policy it was told to read from standard input.
			stop := startEngineHolding(t, configPath)
			defer stop()

			if _, err := io.WriteString(w, "daily_days: 3\nweekly_months: 1\nmonthly_months: 2\n"); err != nil {
				t.Errorf("feeding the policy to stdin: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Errorf("closing the stdin pipe: %v", err)
			}
			return <-done
		})
	})

	if code != exitEngineHoldsDeployment {
		t.Errorf("backup-set retention --policy-file - exited %d with an engine that came up while it was reading stdin, want %d; the engine never sees this write, and the refusal for it is the same refusal however late the engine arrived\nstderr: %s",
			code, exitEngineHoldsDeployment, stderr)
	}
	if after := readFile(t, configPath); after != before {
		t.Errorf("backup-set retention --policy-file - changed config.yaml behind an engine that started while it was reading stdin\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestAFirstConfigurationIsRefusedWhileAnEngineServesThatJournal is the
// escape the one-door claim did not cover.
//
// `backup-set create` stats the configuration path and, finding nothing
// there, writes a whole FIRST configuration through core/service.FirstRun
// instead. That path never opens the journal and never went past the
// engine check, so against a live deployment it exited 0, printed the set,
// and wrote a configuration nothing would ever read. Three shapes reach
// it, and none of them is exotic: a mistyped --config, a config.yaml
// renamed out from under a running engine, and (since #571 gave a
// first-run engine something to announce) a genuinely fresh install whose
// engine is still serving its setup flow.
//
// What identifies the deployment in that state is the journal, because
// --state-database names it and carries the same packaged default
// apps/generic's own --state-database does. So the refusal has to be
// decided from the journal, not from a configuration file that is not
// there.
func TestAFirstConfigurationIsRefusedWhileAnEngineServesThatJournal(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	keyPath := writeTestPrivateKey(t)
	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")

	stop := startEngineHolding(t, configPath)
	defer stop()

	// The path the operator meant to type, one letter out. Nothing is
	// there, so `create` takes the first-run path with the live
	// deployment's own journal underneath it.
	mistyped := filepath.Join(filepath.Dir(configPath), "confg.yaml")
	args := createArgs(mistyped, keyPath, "api/postgres", "--state-database", dbPath)

	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})

	if code != exitEngineHoldsDeployment {
		t.Errorf("backup-set create exited %d writing a FIRST configuration at %s while an engine serves %s, want %d; that configuration is one nothing will ever read, and the write that has no route at all is refused for the same reason as the ones that do\nstderr: %s",
			code, mistyped, dbPath, exitEngineHoldsDeployment, stderr)
	}
	if _, err := os.Stat(mistyped); !os.IsNotExist(err) {
		t.Errorf("a first configuration was written at %s while an engine serves %s (stat err = %v)", mistyped, dbPath, err)
	}
	if !strings.Contains(stderr, "nothing was written") {
		t.Errorf("the refusal does not say the file was left alone:\n%s", stderr)
	}
	// #571: the remedy has to be one that exists for THIS write. The
	// shared sentence used to end by naming $BACKUP_MANAGER_API_URL and
	// the four verbs an address can carry, which sends an operator to set
	// three variables and run a command that refuses identically, because
	// a first configuration is the one write with no route.
	if !strings.Contains(stderr, "cannot carry this one") {
		t.Errorf("the refusal does not say that an address cannot carry a first configuration, so an operator is sent round the loop it describes:\n%s", stderr)
	}
}

// TestAFirstConfigurationIsWrittenWhenNothingServesThatJournal is the
// other arm, and the reason the refusal above has to be decided from the
// journal rather than from "is there a config file".
//
// A bare host has never had a process serve this journal, so there is no
// serving lock file beside it and the question comes back "nothing is
// serving". That host must still be able to write a first configuration
// from the command line, because it is the deployment the direct path
// exists for, and a guard that refused here would take it away.
//
// A host running the first-run setup wizard used to be in this arm too,
// and #571 moved it into the one above: a wizard now announces about the
// journal --state-database names before it serves anything, so a create
// typed at it is refused rather than written underneath it.
func TestAFirstConfigurationIsWrittenWhenNothingServesThatJournal(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t)
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "state.db")

	args := createArgs(configPath, keyPath, "api/postgres", "--state-database", dbPath)
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})
	if code != exitOK {
		t.Fatalf("backup-set create exited %d writing a first configuration with nothing running, want %d\nstderr: %s", code, exitOK, stderr)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("no first configuration at %s after an exit-0 create: %v", configPath, err)
	}
}

// TestAConfigurationWriteIsRefusedWhenTheEngineCheckCannotBePerformed is
// the rule this whole design argues hardest for, made into a check.
//
// "I cannot tell" must never be downgraded to "nothing is running": that
// downgrade is precisely what lets #535 happen again, on the host least
// able to notice. And the check genuinely fails in production, not only in
// theory: EACCES on a lock file owned by another uid, ENOTSUP where flock
// is unavailable, EIO on a sick volume, and the entire non-unix build.
//
// The failure here is a real one rather than an injected seam: a symlink
// that points at itself, so opening it is ELOOP for any uid, on any
// filesystem, with no privileges needed to arrange it. Everything else
// about the deployment still works, so what this pins is the check's error
// branch and nothing else.
func TestAConfigurationWriteIsRefusedWhenTheEngineCheckCannotBePerformed(t *testing.T) {
	for _, m := range configMutations {
		t.Run(m.name, func(t *testing.T) {
			configPath := writeTestConfigWithDeploymentPolicy(t)
			keyPath := writeTestPrivateKey(t)
			if m.prepare != nil {
				m.prepare(t, configPath)
			}
			before := readFile(t, configPath)

			// The lock file the check reads, replaced by a loop. Named
			// literally because the check has to be asked the question it
			// really asks: if this suffix ever stops being the one it
			// probes, the command below stops being refused and this test
			// says so.
			loop := filepath.Join(filepath.Dir(configPath), "state.db.serving-lock")
			if err := os.Symlink(loop, loop); err != nil {
				t.Fatalf("Symlink: %v", err)
			}

			var code int
			stderr := captureStderr(t, func() {
				code = captureStdoutCode(t, func() int { return run(m.args(configPath, keyPath)) })
			})

			if code != exitFailure {
				t.Errorf("%s exited %d while the engine check could not be performed, want %d; 0 would mean \"I could not tell\" was downgraded to \"nothing is running\", and %d would send a script off to wait for an engine to stop when what is wrong is the host\nstderr: %s",
					m.name, code, exitFailure, exitEngineHoldsDeployment, stderr)
			}
			if after := readFile(t, configPath); after != before {
				t.Errorf("%s changed config.yaml while the engine check could not be performed\nbefore:\n%s\nafter:\n%s", m.name, before, after)
			}
			if !strings.Contains(stderr, "nothing was written") {
				t.Errorf("%s refusal does not say the file was left alone:\n%s", m.name, stderr)
			}
		})
	}
}

// TestSettingsPatchWithNoFlagsComplainsAboutTheFlagsBesideARunningEngine
// keeps the refusal from answering a question the operator did not ask.
//
// `settings patch` with no flags is a usage mistake, and beside a running
// engine it used to be reported as an engine problem, because the engine
// check ran before anything looked at the command line. That sends
// somebody off to stop a daemon over a missing --timezone. `backup-set
// patch` has always got this right by refusing an empty patch before it
// opens anything, and this is the same rule.
func TestSettingsPatchWithNoFlagsComplainsAboutTheFlagsBesideARunningEngine(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	stop := startEngineHolding(t, configPath)
	defer stop()

	args := []string{"settings", "--config", configPath, "patch"}
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})

	if code != exitUsage {
		t.Fatalf("run(%v) = %d, want %d; a patch that names no setting is a usage mistake wherever it is typed, and answering it with the engine's own exit code would send a script off to wait for a daemon to stop over a missing flag\nstderr: %s",
			args, code, exitUsage, stderr)
	}
	if strings.Contains(stderr, "another process") {
		t.Errorf("`settings patch` with no flags was answered with the engine refusal, so an operator is sent to stop a daemon over a missing flag:\n%s", stderr)
	}
	if !strings.Contains(stderr, "settings patch") {
		t.Errorf("`settings patch` with no flags does not name the command whose usage is wrong:\n%s", stderr)
	}
}
