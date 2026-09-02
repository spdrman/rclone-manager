package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

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

func TestRunRestoreCheck_CannotStartIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := RunRestoreCheck(context.Background(), config.Command{Executable: missing, Timeout: config.Duration(5 * time.Second)}, "/dev/null")
	if err == nil {
		t.Fatal("RunRestoreCheck succeeded against a nonexistent executable, want an error")
	}
}

func TestRunRestoreCheck_ZeroTimeoutIsAnError(t *testing.T) {
	script := mustScript(t, "exit 0\n")
	_, err := RunRestoreCheck(context.Background(), config.Command{Executable: script}, "/dev/null")
	if err == nil {
		t.Fatal("RunRestoreCheck accepted a zero timeout, want an error")
	}
}

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
	script := mustScript(t, warmingPrelude+fmt.Sprintf("echo $$ > %s\nsleep 30\necho done > %s\n", shQuote(pidFile), shQuote(markerFile)))
	mustWarmScript(t, script)

	start := time.Now()
	result, err := RunRestoreCheck(context.Background(), config.Command{Executable: script, Timeout: config.Duration(2 * time.Second)}, path)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunRestoreCheck: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("RunRestoreCheck took %s to return. That is the assertion that actually catches an abandoned process: os/exec's own WaitDelay kills it 5s after the context is done, so a hook that was abandoned rather than killed returns at about 7s while one that was killed returns at about 2s", elapsed)
	}
	if result.Passed {
		t.Fatal("Passed = true, want false: a hook that never answers must fail closed")
	}
	if !strings.Contains(result.Detail, "timeout") {
		t.Fatalf("Detail = %q, want it to mention the timeout", result.Detail)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading pid file (the hook should have written it immediately on starting): %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parsing pid: %v", err)
	}

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
