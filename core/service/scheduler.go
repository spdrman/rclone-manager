package service

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// PollInterval reports the poll_interval this BackupService was
// configured with (config.Config.PollInterval), the same value
// cmd/backup-manager's own `daemon` command reads directly off a
// *config.Config it constructed itself - a shortcut apps/ has no
// equivalent for, since it cannot import internal/config at all (§7.2).
// A caller composing this BackupService with an HTTP API (the generic
// Web host's `serve` command) needs this to drive RunOnSchedule at the
// operator's own configured cadence, rather than inventing a second,
// possibly-drifting interval of its own.
func (b *BackupService) PollInterval() time.Duration {
	return b.pollInterval
}

// RunOnSchedule repeats one internal/app.Service.RunCycle pass at the
// given interval until ctx is done, the same repeated-cycle shape
// internal/app.Service.Daemon already gives cmd/backup-manager's own
// `daemon` command — but reachable from apps/ (core/internal is not,
// docs/EPIC-B-multi-nas.md §7.2), which is what a process composing this
// BackupService with an HTTP API (the generic Web host's `serve` command,
// §9.2/§9.3) needs instead of reimplementing the loop against
// internal/app directly.
//
// # Shares BackupService's single-flight guard with SubmitRunCycle
//
// A process that runs this method AND serves POST /api/v1/operations
// (SubmitRunCycle, operations.go) against the same BackupService has two
// independent callers that can each want to run
// internal/app.Service.RunCycle at once: a scheduled tick here, and an
// operator-submitted run there. internal/app/cycle.go's own doc says two
// concurrent passes over the same backup set must never happen, and
// SubmitRunCycle already enforces that for its own callers via
// BackupService's runOnce mutex (see that method's doc). This method
// reuses the exact same mutex, via runScheduledCycle below, rather than
// running RunCycle unguarded: when a tick lands while runOnce is already
// held by an in-flight SubmitRunCycle, it skips that tick instead of
// running concurrently or blocking the loop, and simply tries again at
// the next interval. Skipping (not queueing or blocking) mirrors
// SubmitRunCycle's own ErrOperationAlreadyRunning choice: a scheduled
// tick that cannot run right now is not lost information worth blocking
// the loop over, it runs on the next tick instead.
//
// ctx governs the loop directly (unlike SubmitRunCycle's async execution,
// which deliberately uses BackupService's own longer-lived b.ctx instead
// of a caller's context — see that method's doc for why): RunOnSchedule
// has no shorter-lived caller to decouple from in the first place, it IS
// the top-level driver for as long as the process runs, exactly like
// internal/app.Service.Daemon is for the CLI's own `daemon` command. A
// caller (the `serve` command) is expected to pass the same
// signal-derived, process-shutdown context it also uses for the HTTP
// server's own graceful shutdown, which is what §9.3's "share ... a
// process shutdown context" actually means in practice: both halves stop
// because the same ctx was canceled, not because one tells the other to.
func (b *BackupService) RunOnSchedule(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("service: RunOnSchedule needs a positive poll interval, got %s", interval)
	}

	for {
		b.runScheduledCycle(ctx)

		if ctx.Err() != nil {
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// runScheduledCycle runs exactly one RunCycle pass, guarded by the same
// runOnce mutex executeRunCycle (operations.go) uses, so a tick that
// lands while an API-submitted operation is already running skips rather
// than overlapping it. Unlike executeRunCycle, there is no operation row
// to fail here: a skipped scheduled tick was never persisted as an
// operation in the first place (it is not caller-submitted work with an
// idempotency key to account for), it simply runs again at the next
// tick.
func (b *BackupService) runScheduledCycle(ctx context.Context) {
	if !b.runOnce.TryLock() {
		b.logger.Event(ctx, obs.LevelInfo, "scheduled_cycle_skipped",
			"skipped scheduled run_cycle: an API-submitted operation is already in progress")
		return
	}
	defer b.runOnce.Unlock()

	runCycle(b.inner, ctx)
}
