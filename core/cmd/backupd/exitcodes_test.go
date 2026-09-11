package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Issue #551: the exit statuses this binary promises, driven rather than
// read off the source.
//
// # What was wrong with one failure code
//
// `fail` returned 1 for everything, so "another process is serving this
// deployment, so nothing was written", "your config.yaml is malformed" and
// "the disk is full" were the same news to anything reading $?. That was
// survivable while every non-usage failure meant roughly "something went
// wrong, read the message". EPIC #536 ended it: the refusal it added is
// the first failure here that is both expected and retryable, and a
// provisioning script wants to wait on that one and abort on the others.
// Without a code of its own the only way to tell them apart is to match on
// the message, which is exactly the coupling core/tests/compat exists to
// stop people relying on.
//
// # Why the assertions below come in pairs
//
// A test that only proves the refusal exits 3 cannot tell a working
// implementation from one that returns 3 for everything. So every case
// here has a partner that must NOT be 3: the probe that could not be
// performed, the usage mistake made beside the same running engine, the
// configuration that will not load, and the identical invocation with
// nothing serving. liveengine_test.go's tables carry the same pairing for
// the six configuration writes, which is why they are not repeated here.

// TestTheUsageBlockDocumentsEveryExitCode holds the stated contract
// against the constants.
//
// #551 asks for the table to be IN the usage block, so that an operator
// branching a script on $? is reading a promise rather than reverse
// engineering setup.go. A table that drifted from the constants would be
// worse than no table, because a script written against it would be wrong
// in a way nothing complains about.
//
// What this cannot see, said out loud rather than left as a gap somebody
// discovers: a fifth constant added below without a row here. Nothing in
// Go lets this file enumerate the constants, so the row count is asserted
// against the four that exist and a new one is a two-sided edit like any
// other. The behavioural tests in this file and in liveengine_test.go are
// what make each documented row real.
func TestTheUsageBlockDocumentsEveryExitCode(t *testing.T) {
	documented := exitCodesInUsage(t)

	for _, want := range []struct {
		code int
		what string
	}{
		{exitOK, "success"},
		{exitFailure, "an ordinary failure"},
		{exitUsage, "a usage mistake"},
		{exitEngineHoldsDeployment, "the engine-holds-this-deployment refusal"},
	} {
		if _, ok := documented[want.code]; !ok {
			t.Errorf("the usage block's exit-code table has no row for %d (%s), so a script branching on it is reading the source instead", want.code, want.what)
		}
	}
	if len(documented) != 4 {
		t.Errorf("the usage block documents %d exit codes (%v) and this binary defines 4; a code in one list and not the other is a contract nobody can rely on", len(documented), sortedCodes(documented))
	}
}

// exitCodeRow matches one row of the table in the usage block: two spaces,
// the code, and the sentence that introduces it.
var exitCodeRow = regexp.MustCompile(`^  ([0-9])   (\S.*)$`)

// exitCodesInUsage reads the table out of what an operator actually sees,
// rather than out of a constant this test could keep in step with itself.
func exitCodesInUsage(t *testing.T) map[int]string {
	t.Helper()
	out := map[int]string{}
	for _, line := range strings.Split(captureStderr(t, usage), "\n") {
		m := exitCodeRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		code, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parsing %q out of the usage block: %v", m[1], err)
		}
		out[code] = m[2]
	}
	if len(out) == 0 {
		t.Fatalf("the usage block has no exit-code table at all, so nothing below is comparing against a stated contract:\n%s", captureStderr(t, usage))
	}
	return out
}

func sortedCodes(m map[int]string) []int {
	out := make([]int, 0, len(m))
	for code := range m {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// TestASecondDaemonIsRefusedWithTheRefusalExitCode is the other half of
// the same fact, and the reason exit 3 is not only about configuration
// writes.
//
// The usage block already says a `daemon` is refused rather than started
// when another process is serving that state database, and that refusal is
// the same sentence core/service's ErrAlreadyServing carries. It is also
// the most retryable failure this binary has: a supervisor replacing a
// container meets it whenever the outgoing process has not let go yet, and
// waiting is exactly the right answer. Leaving it on 1 while a
// configuration write refused for the identical reason exits 3 would make
// the table this issue adds a lie about the one command it names.
//
// Observed from outside a process, like the signal test next door and for
// the same reason: an exit status is a property of a process, and calling
// cmdDaemon in-process would also flip the embedded rclone's global
// at-exit state under every other test in this package.
func TestASecondDaemonIsRefusedWithTheRefusalExitCode(t *testing.T) {
	configPath := writeTestConfig(t)
	stop := startEngineHolding(t, configPath)
	defer stop()

	code, output := runDaemonChild(t, configPath)
	if code != exitEngineHoldsDeployment {
		t.Errorf("a second daemon against a served deployment exited %d, want %d; a supervisor cannot tell \"something else is already serving this\" from a broken deployment\n%s",
			code, exitEngineHoldsDeployment, output)
	}
	if !strings.Contains(output, "already serving this deployment") {
		t.Errorf("the second daemon did not say what it found:\n%s", output)
	}
}

// TestADaemonThatCannotLoadItsConfigurationStillExitsOne is that test's
// partner. The child is started against a path with no configuration at
// all, so nothing is serving anything and the failure is an ordinary one.
// Without this, a `daemon` that exited 3 for every reason would pass the
// test above.
func TestADaemonThatCannotLoadItsConfigurationStillExitsOne(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "config.yaml")

	code, output := runDaemonChild(t, absent)
	if code != exitFailure {
		t.Errorf("a daemon whose configuration is not there exited %d, want %d; that is a broken deployment rather than one somebody else is serving\n%s",
			code, exitFailure, output)
	}
}

// runDaemonChild re-executes this test binary as `backupd daemon
// --config configPath` and returns the status it exited with, plus
// everything it printed.
//
// It is the refusing counterpart of startEngineHolding: that one waits for
// a child that keeps running, this one waits for a child that stops. Both
// go through TestDaemonChildProcess, so both run the same run() dispatch
// the shipped binary runs.
func runDaemonChild(t *testing.T, configPath string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonChildProcess$")
	cmd.Env = append(os.Environ(), daemonChildEnv+"=1", daemonChildConfig+"="+configPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the daemon child exited 0 against %s, and every case here expects it to refuse:\n%s", configPath, out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("running the daemon child: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

// TestOnlyTheEngineRefusalGetsTheRetryableCode walks the failures a script
// meets most often and requires each one to keep the status it had.
//
// This is the guard in the direction that is easy to forget. Adding a code
// is only worth something if the codes around it did not move: a script
// that starts retrying a malformed config.yaml because it now looks like a
// busy deployment is worse off than it was with one failure code for
// everything.
func TestOnlyTheEngineRefusalGetsTheRetryableCode(t *testing.T) {
	configPath := writeTestConfig(t)
	absent := filepath.Join(t.TempDir(), "not-there.yaml")

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"a configuration that is not there", []string{"check", "--config", absent}, exitFailure},
		{"an artifact nobody has", []string{"artifacts", "--config", configPath, "production/postgres-primary/no-such.dump"}, exitFailure},
		{"an unknown command", []string{"definitely-not-a-command"}, exitUsage},
		{"an unknown flag", []string{"check", "--config", configPath, "--nope"}, exitUsage},
		{"a command that works", []string{"check", "--config", configPath}, exitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() {
				code = captureStdoutCode(t, func() int { return run(tc.args) })
			})
			if code != tc.want {
				t.Errorf("run(%v) = %d, want %d\nstderr: %s", tc.args, code, tc.want, stderr)
			}
		})
	}
}
