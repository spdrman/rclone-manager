// These cover RunRestoreCheck, which runs an operator's own executable
// against an artifact and believes what it says.
//
// The organising distinction, and the one that shapes every test here, is
// between a hook that ANSWERS and a hook that does not. A hook that runs and
// exits non-zero has made a statement about the artifact, and that statement
// is a verdict the caller acts on. A hook that could not be started, timed
// out, or was cancelled has said nothing at all, and turning silence into a
// verdict would quarantine backups over a wrong path in a config file. So
// the failure cases below all assert on the ERROR return rather than on
// Passed, and the ones that check Passed are the ones where the process
// really did run.
//
// The rest is the untrusted-subprocess contract: no ambient environment,
// bounded output, its own process group so a timeout kills what the hook
// spawned rather than only the hook. Those are asserted against real
// processes rather than mocked, because every one of them is a property of
// how the process was started and there is nothing left to test once that is
// faked away.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// TestRunRestoreCheck_Passes is the positive control the whole file needs.
// Every other case here expects a refusal or an error, and without one
// success this suite would be satisfied by an implementation that never ran
// anything.
func TestRunRestoreCheck_Passes(t *testing.T) {
	path := verifyWriteLocalFile(t, []byte("dump-bytes"))
	script := mustScript(t, "exit 0\n")

	result, err := RunRestoreCheck(context.Background(), config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}, path)
	if err != nil {
		t.Fatalf("RunRestoreCheck: %v", err)
	}
	if !result.Passed {
		t.Fatalf("Passed = false, want true: %q", result.Detail)
	}
}

// TestRunRestoreCheck_NonZeroExitFails pins that a hook which runs and
// disagrees produces a verdict with no error, and that its stderr survives
// into Detail.
//
// The stderr assertion is the useful half. This detail is written into the
// journal and is the only thing an operator will have to work from six
// months later, so a verdict that recorded only "the hook failed" would
// leave them re-running the hook by hand to find out what it said.
func TestRunRestoreCheck_NonZeroExitFails(t *testing.T) {
	path := verifyWriteLocalFile(t, []byte("dump-bytes"))
	script := mustScript(t, "echo \"restore failed: truncated archive\" >&2\nexit 1\n")

	result, err := RunRestoreCheck(context.Background(), config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}, path)
	if err != nil {
		t.Fatalf("RunRestoreCheck: %v", err)
	}
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Detail, "truncated archive") {
		t.Fatalf("Detail = %q, want it to contain the hook's stderr", result.Detail)
	}
}

// TestRunRestoreCheck_CannotStartIsAnError is the distinction the file
// header describes, staged as the way it actually happens: a path that does
// not exist.
//
// This has to be an error and not a failed verdict. A typo in a config file
// applies to every artifact in the backup set, so reading it as "the backup
// does not restore" would quarantine all of them at once, and the state
// machine offers no way back that does not need a human.
func TestRunRestoreCheck_CannotStartIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := RunRestoreCheck(context.Background(), config.Command{Executable: missing, Timeout: config.Duration(5 * time.Second)}, "/dev/null")
	if err == nil {
		t.Fatal("RunRestoreCheck succeeded against a nonexistent executable, want an error")
	}
}

// TestRunRestoreCheck_ZeroTimeoutIsAnError refuses rather than choosing a
// default.
//
// A zero timeout has two plausible readings, no limit and expire
// immediately, and both are bad: the first hangs a scheduled pass on a hook
// that never returns, and the second fails every artifact. Refusing is the
// only answer that cannot be silently wrong, and config.Validate fills in a
// real default so no validated configuration reaches this.
func TestRunRestoreCheck_ZeroTimeoutIsAnError(t *testing.T) {
	script := mustScript(t, "exit 0\n")
	_, err := RunRestoreCheck(context.Background(), config.Command{Executable: script}, "/dev/null")
	if err == nil {
		t.Fatal("RunRestoreCheck accepted a zero timeout, want an error")
	}
}

// TestRunRestoreCheck_DoesNotInheritAmbientEnvironment asserts three things
// at once, and it needs all three.
//
// The secret must not be visible: this hook is an operator-supplied
// executable running inside a process that holds remote credentials, and an
// inherited environment is how those reach it. The artifact path must be
// visible in BOTH the environment and argv, because either alone is a
// plausible interface and hooks in the wild are written against whichever
// one they found first.
//
// The script exits 1 so its output is preserved in Detail, which is the only
// channel a test has for seeing what the child process could actually
// see.
func TestRunRestoreCheck_DoesNotInheritAmbientEnvironment(t *testing.T) {
	path := verifyWriteLocalFile(t, []byte("dump-bytes"))
	t.Setenv("RCLONE_MANAGER_TEST_SECRET", "super-secret-value")

	script := mustScript(t, "echo \"SECRET=$RCLONE_MANAGER_TEST_SECRET\"\necho \"ARTIFACT=$RCLONE_MANAGER_ARTIFACT_PATH\"\necho \"ARG1=$1\"\nexit 1\n")

	result, err := RunRestoreCheck(context.Background(), config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}, path)
	if err != nil {
		t.Fatalf("RunRestoreCheck: %v", err)
	}
	if strings.Contains(result.Detail, "super-secret-value") {
		t.Fatalf("the hook saw an ambient secret it must never inherit: %q", result.Detail)
	}
	if !strings.Contains(result.Detail, "ARTIFACT="+path) {
		t.Fatalf("the hook did not see its artifact path via env: %q", result.Detail)
	}
	if !strings.Contains(result.Detail, "ARG1="+path) {
		t.Fatalf("the hook did not see its artifact path via argv[1]: %q", result.Detail)
	}
}

// TestRunRestoreCheck_OutputIsBounded pushes a megabyte through the hook,
// which is what a hook that dumps a log or a diff does on a bad day.
//
// Two things would break without the bound, and they break in different
// places: the capture buffer grows without limit in this process, and the
// detail string is written into the journal, so one unlucky hook run would
// bloat a row that an operator later has to read. The allowance of 64 bytes
// over the limit is for the truncation marker.
func TestRunRestoreCheck_OutputIsBounded(t *testing.T) {
	path := verifyWriteLocalFile(t, []byte("dump-bytes"))
	script := mustScript(t, "yes A | head -c 1000000\nexit 1\n")

	result, err := RunRestoreCheck(context.Background(), config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}, path)
	if err != nil {
		t.Fatalf("RunRestoreCheck: %v", err)
	}
	if len(result.Detail) > maxRestoreCheckOutput+64 {
		t.Fatalf("Detail length = %d, an untrusted hook's output must be bounded near %d", len(result.Detail), maxRestoreCheckOutput)
	}
}

// TestRunRestoreCheck_Timeout_KillsProcess proves the same fail-closed,
// actually-kill-the-process contract verify.go's runValidator already has
// (TestVerify_Validator_Timeout_KillsProcess_Quarantines), independently,
// for this package's own separate implementation.
func TestRunRestoreCheck_Timeout_KillsProcess(t *testing.T) {
	path := verifyWriteLocalFile(t, []byte("dump-bytes"))

	pidFile := filepath.Join(t.TempDir(), "pid")
	markerFile := filepath.Join(t.TempDir(), "marker")
	script := mustScript(t, fmt.Sprintf("echo $$ > %s\nsleep %d\necho done > %s\n", shQuote(pidFile), int(hookNeverAnswers.Seconds()), shQuote(markerFile)))

	start := time.Now()
	result, err := RunRestoreCheck(context.Background(), config.Command{Executable: script, Timeout: config.Duration(hookTimeoutBudget)}, path)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunRestoreCheck: %v", err)
	}
	if elapsed > hookReturnBudget {
		t.Fatalf("RunRestoreCheck took %s to return; the hook should have been killed well before its %s sleep finished", elapsed, hookNeverAnswers)
	}
	if result.Passed {
		t.Fatal("Passed = true, want false: a hook that never answers must fail closed")
	}
	if !strings.Contains(result.Detail, "timeout") {
		t.Fatalf("Detail = %q, want it to mention the timeout", result.Detail)
	}

	pid := timedOutHookPID(t, pidFile)

	deadline := time.Now().Add(2 * time.Second)
	for {
		killErr := syscall.Kill(pid, 0)
		if errors.Is(killErr, syscall.ESRCH) {
			break // confirmed: the process is actually gone
		}
		if time.Now().After(deadline) {
			t.Fatalf("hook process %d is still alive well after its timeout; it was abandoned, not killed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := os.Stat(markerFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker file exists: the hook ran to completion despite its timeout, so it was not actually killed")
	}
}

// TestRunRestoreCheck_OuterCancellationIsAnError covers the shutdown case:
// the daemon is stopping while a hook is mid-run.
//
// It is an error rather than a failed verdict for the same reason a hook
// that could not start is. The hook said nothing, and a cancelled pass that
// recorded failures would quarantine artifacts as a side effect of somebody
// pressing Ctrl-C. The message is asserted to mention cancellation, because
// this is the one error here whose cause is entirely outside the artifact
// and the operator's configuration.
func TestRunRestoreCheck_OuterCancellationIsAnError(t *testing.T) {
	path := verifyWriteLocalFile(t, []byte("dump-bytes"))
	script := mustScript(t, "sleep 5\n")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := RunRestoreCheck(ctx, config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}, path)
	if err == nil {
		t.Fatal("RunRestoreCheck succeeded despite the outer context being cancelled, want an error")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error = %v, want it to say the call was cancelled", err)
	}
}
