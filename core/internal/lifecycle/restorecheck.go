// This file is the untrusted-subprocess primitive Phase 4's scheduled
// revalidation (internal/revalidate) needs for its restore-test hook: the
// stronger form of revalidation that proves an artifact still actually
// restores, not merely that its bytes are unchanged.
//
// RunRestoreCheck's contract is deliberately identical to verify.go's
// unexported runValidator, which already implements this exact untrusted-
// subprocess shape for FR-13's one-time, at-verification application
// validator: a fixed, minimal environment, its own process group so a
// timeout can kill anything it spawned, bounded captured output, and
// fail-closed on a timeout or a non-zero exit. It is a separate,
// independent implementation rather than an exported alias for
// runValidator, on purpose: verify.go is already-shipped, already-tested
// FR-13 code (see this package's other files), and this PR does not touch
// it. The two are small enough (under a hundred lines each) that keeping
// them independent costs little, and a future change that wants to unify
// them can do so once both have their own callers and their own tests to
// prove it did not change behavior for either.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// maxRestoreCheckOutput mirrors verify.go's maxValidatorOutput: it bounds
// how much of a restore-test hook's combined stdout+stderr this package
// keeps, both while capturing it (so a runaway untrusted process cannot
// exhaust memory) and in the detail string a caller might persist to the
// journal (so one hook cannot bloat it without limit).
const maxRestoreCheckOutput = 16 << 10 // 16 KiB

// RestoreCheckResult is RunRestoreCheck's verdict.
type RestoreCheckResult struct {
	// Passed is the hook's pass/fail verdict: true only for a clean exit
	// (status 0). It is meaningless when RunRestoreCheck also returns a
	// non-nil error, which means the hook never got to render a verdict at
	// all.
	Passed bool

	// Detail is the hook's captured stdout+stderr, bounded to
	// maxRestoreCheckOutput, plus a trailing note when it was killed for
	// exceeding its own timeout.
	Detail string
}

// RunRestoreCheck runs cmd against localPath and reports its pass/fail
// verdict.
//
// err is non-nil only when the hook could not be run at all (a bad
// executable, permission denied to exec it) or when ctx itself was
// cancelled or timed out, both infrastructure conditions distinct from the
// hook having an opinion about localPath. Once the process actually
// starts, whatever happens to it next, a clean non-zero exit or being
// killed for exceeding cmd.Timeout, is the hook's verdict, reported
// through the returned RestoreCheckResult with a nil error: a restore-test
// hook that never answers is treated the same as an explicit "no" (fail
// closed), never as license to treat the artifact as still good.
func RunRestoreCheck(ctx context.Context, cmd config.Command, localPath string) (RestoreCheckResult, error) {
	timeout := cmd.Timeout.Duration()
	if timeout <= 0 {
		return RestoreCheckResult{}, fmt.Errorf("lifecycle: RunRestoreCheck: timeout must be positive, got %s", cmd.Timeout)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := exec.CommandContext(runCtx, cmd.Executable, localPath)

	// A fixed, minimal environment: never this process's own os.Environ(),
	// which could carry secrets this manager holds (an sftp private key
	// path, ambient credentials). Mirrors verify.go's runValidator exactly,
	// including the env var duplicating argv[1] for a hook that prefers
	// reading its target from the environment.
	c.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"RCLONE_MANAGER_ARTIFACT_PATH=" + localPath,
	}

	// A fresh process group, so the timeout below can kill whatever the
	// hook spawned along with it, not just the hook itself.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		if killErr := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			return killErr
		}
		return nil
	}
	c.WaitDelay = 5 * time.Second

	out := &boundedWriter{limit: maxRestoreCheckOutput}
	c.Stdout = out
	c.Stderr = out

	runErr := c.Run()
	detail := out.String()

	// See verify.go's runValidator for why classification reads the
	// contexts' own Err() directly rather than inspecting runErr's type or
	// wrapped chain: once c.Cancel above kills the process promptly, Go's
	// exec package is not guaranteed to surface ctx.Err() as the returned
	// error at all.
	switch {
	case runErr == nil:
		return RestoreCheckResult{Passed: true, Detail: detail}, nil
	case ctx.Err() != nil:
		return RestoreCheckResult{}, fmt.Errorf("lifecycle: RunRestoreCheck: cancelled: %w", ctx.Err())
	case runCtx.Err() != nil:
		detail += fmt.Sprintf("\n(rclone-manager: restore-test hook killed after exceeding its %s timeout)", timeout)
		return RestoreCheckResult{Passed: false, Detail: detail}, nil
	default:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return RestoreCheckResult{Passed: false, Detail: detail}, nil
		}
		return RestoreCheckResult{}, fmt.Errorf("lifecycle: RunRestoreCheck: could not start %q: %w", cmd.Executable, runErr)
	}
}
