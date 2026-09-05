// This file is the unattended driver: the loop that keeps running cycles
// when nobody is asking it to, for a process that cannot reach the one
// internal/app already has.
//
// cmd/backup-manager's `daemon` gets that loop from
// internal/app.Service.Daemon directly. The generic Web host cannot, §7.2
// sees to that, and reimplementing it above this boundary would put the
// cadence, the single-flight decision and the panic policy in a package
// that can see none of the state they are about. So the loop lives here,
// on the inside of the seam, and apps/ drives it with a context and an
// interval.
//
// Nothing in this file queues. A tick that lands while an API-submitted
// operation holds the single-flight lock is dropped and logged, not
// buffered, because the work it would have done is the same work the next
// tick will do: backing up whatever is on disk now is not made more
// correct by having been asked for twice. Queueing would turn a slow
// afternoon into a backlog of identical passes that all still have to
// run.
//
// There are two timers here rather than one loop doing two jobs, and the
// alerting one deliberately does not take the single-flight lock. Its
// whole reason for existing is the case where a cycle has wedged, so a
// design where a wedged cycle silences the alerts about wedged cycles
// would be the one shape guaranteed not to work.
//
// Every goroutine started here recovers panics, and that is not defensive
// habit. This code shares a process with a persistent HTTP server: an
// unrecovered panic in a scheduled tick takes the API down with it, and a
// panic that escaped while holding the single-flight lock would leave
// every future tick and every future operator-submitted run waiting on a
// lock nothing will ever release.
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
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

	// Work Package 3.5's alerting pass runs on its own timer beside this
	// loop, not only at the end of a cycle (see internal/app's AlertTick).
	// A cycle that wedges on one slow transfer never reaches the pass at
	// its end, and neither does a tick this loop skipped because an
	// API-submitted operation is stuck holding runOnce - both of which are
	// exactly "the manager is up and not producing backups", the situation
	// the stale alert exists to report. It shares this loop's interval
	// rather than inventing a second cadence, and stops when ctx does; the
	// deferred receive keeps this method from returning while it is still
	// running.
	alertsStopped := make(chan struct{})
	go func() {
		defer close(alertsStopped)
		b.runAlertTicks(ctx, interval)
	}()
	defer func() { <-alertsStopped }()

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
//
// # Panic recovery (issue #119's review, finding 5)
//
// executeRunCycle (operations.go) already recovers a panic inside RunCycle
// specifically because an unrecovered one there "would crash the entire
// persistent API server hosting this BackupService, not just one CLI
// invocation" (that method's own doc) - true here for exactly the same
// reason, and RunOnSchedule (this file) is what first makes THIS method
// share a process with that same persistent API server. The deferred
// recover below is declared AFTER b.runOnce.Unlock() specifically so it
// runs BEFORE it (defer is LIFO): a panic must release the single-flight
// lock the same way a normal return does, or a single panicking cycle
// would permanently wedge every future scheduled tick AND every future
// API-submitted operation behind a lock nothing will ever release again.
// This is deliberately NOT a bare catch-and-continue: the panic is logged
// at error level, loudly, exactly like executeRunCycle's own recovery
// does, rather than silently swallowed - running the next tick against
// state a panic just proved was unexpected is its own risk, and an
// operator watching logs needs to see that this happened, not just that
// the scheduler loop kept ticking as if nothing had.
func (b *BackupService) runScheduledCycle(ctx context.Context) {
	if !b.runOnce.TryLock() {
		b.logger.Event(ctx, obs.LevelInfo, "scheduled_cycle_skipped",
			"skipped scheduled run_cycle: an API-submitted operation is already in progress")
		return
	}
	defer b.runOnce.Unlock()
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error(context.Background(), "scheduled-cycle-panic", fmt.Errorf("recovered panic: %v", r))
		}
	}()

	// Rewrite the registered validators before anything execs one. A
	// scheduled tick is the path that runs unattended for weeks, so it is
	// the one most likely to meet a script that has been replaced or
	// reaped since the process started (validator.go's
	// refreshValidatorScripts). A failure skips this tick rather than
	// running it: a cycle that cannot guarantee its validators are the
	// scripts this build ships is a cycle that must not be allowed to pass
	// an artifact and authorize deleting the remote source. There is no
	// operation row to fail here, so the log line is the record, and the
	// next tick tries again.
	if err := b.refreshValidatorScripts(); err != nil {
		b.logger.Error(ctx, "scheduled-cycle-validator-scripts", err)
		return
	}

	// A scheduled tick reports its progress into cycleWatch (edithold.go)
	// and honours edit holds, exactly like an API-submitted one. Before
	// this, a scheduled tick installed no observer at all, so a transfer
	// the scheduler was running was invisible to every reader in this
	// process, and the scheduler is precisely what runs unattended,
	// which makes it the cycle an operator is most likely to be about to
	// interrupt.
	b.cycleWatch.begin()
	defer b.cycleWatch.end()
	runCycle(b.state.Load().inner,
		app.WithBackupSetHolds(
			app.WithProgressObserver(ctx, b.cycleWatch),
			b.holds))
}

// runAlertTicks repeats one out-of-cycle alerting pass at interval until
// ctx is done, against whatever Service the latest configuration
// hot-reload left in place (b.state is re-read every tick, so a pass
// after a CreateBackupSet speaks about the new config, not the one this
// loop started with).
//
// It deliberately does NOT take runOnce. That lock exists so two passes
// never process the same backup set at once, and this pass processes
// nothing: it reads a health report and hands verdicts to the dispatcher,
// which is safe for concurrent use. Taking it would make this tick skip
// in exactly the case it was added for, a cycle that is stuck holding it.
func (b *BackupService) runAlertTicks(ctx context.Context, interval time.Duration) {
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		b.tickAlerts(ctx)
	}
}

// tickAlerts runs one pass, with the same panic recovery
// runScheduledCycle documents: this goroutine shares a process with a
// persistent API server, so an unrecovered panic here would take that
// server down, and an alerting problem must never be able to do that.
func (b *BackupService) tickAlerts(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error(context.Background(), "alert-tick-panic", fmt.Errorf("recovered panic: %v", r))
		}
	}()

	b.state.Load().inner.AlertTick(ctx)
}
