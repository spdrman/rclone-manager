package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// FR-1's long-running mode, and why the loop is here rather than in the CLI.
//
// The loop is nine lines and cmd/backup-manager could hold it. It does not,
// because two of this product's guarantees are properties of the loop's
// shape: no two passes over a backup set ever overlap, and a shutdown stops
// work at a boundary where nothing is half-written. Both are argued from the
// code being a plain for loop with a timer rather than a ticker, and an
// argument like that is only worth something if it is made somewhere a test
// can exercise it without sending the process a real signal.
//
// So the split is that the CLI decides what a signal MEANS and turns it into
// a cancelled context, and this file decides what happens next. Nothing here
// installs a signal handler, and nothing here should.
//
// The alerting pass beside the cycle loop is the one thing that runs on its
// own timer instead of at the end of a cycle. That is not tidiness: the
// condition most worth reporting is a daemon that is up and producing
// nothing, and a cycle wedged on one slow transfer never reaches its own
// tail. See AlertTick.

// Daemon is FR-1's `daemon` execution mode: it runs RunCycle once
// immediately, then again every interval, until ctx is done.
//
// cmd/backup-manager owns turning SIGTERM/SIGINT into ctx's cancellation
// (via signal.NotifyContext), exactly as FR-1 asks for "handle
// SIGTERM/SIGINT" and "use Go context cancellation" to be read together:
// this package only ever reacts to ctx, and never installs a signal
// handler of its own, so the CLI stays the one place that decides what a
// signal means and this package stays testable without sending itself a
// real signal.
//
// # No overlapping cycles
//
// This is a single, unbuffered for loop: the next RunCycle call is never
// started until the previous one has returned, and the wait between them
// is a plain select against a timer and ctx.Done(), never a
// time.Ticker firing on its own schedule regardless of whether the last
// cycle finished. A cycle that happens to run longer than interval simply
// means the next one starts late, immediately after the slow one returns,
// rather than two cycles ever overlapping. Combined with RunCycle's own
// sequential, non-concurrent processing of every configured backup set
// (see its doc), this makes "no overlapping processing for the same
// backup set" true for every backup set, all the time, by construction.
//
// # Returning
//
// Daemon returns nil whenever ctx becomes done, whether that is observed
// right after a RunCycle call returns or while waiting out the interval
// between cycles: either way this is FR-1's ordinary, expected shutdown
// path, not an error condition cmd/backup-manager needs to distinguish
// from a clean exit. It returns a non-nil error only for a genuine
// argument problem (a non-positive interval) caught before the loop ever
// starts.
func (s *Service) Daemon(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("app: daemon needs a positive poll interval, got %s", interval)
	}

	s.logger().Event(ctx, obs.LevelInfo, "daemon_start", "daemon starting",
		slog.Duration("poll_interval", interval))

	// Work Package 3.5's alerting pass also runs on its own timer, beside
	// the cycle loop rather than inside it (see AlertTick). A cycle that
	// wedges on one slow transfer never reaches the pass at its end, and
	// "the daemon is up but producing nothing" is the exact situation the
	// stale alert exists to report, so it cannot be the situation that
	// silences it. This goroutine stops when ctx does, and the deferred
	// receive below keeps Daemon from returning while it is still running.
	alertsStopped := make(chan struct{})
	go func() {
		defer close(alertsStopped)
		s.runAlertTicks(ctx, interval)
	}()
	defer func() { <-alertsStopped }()

	for {
		s.RunCycle(ctx)

		if ctx.Err() != nil {
			s.logger().Event(ctx, obs.LevelInfo, "daemon_stop", "daemon shutting down", slog.String("reason", ctx.Err().Error()))
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.logger().Event(ctx, obs.LevelInfo, "daemon_stop", "daemon shutting down", slog.String("reason", ctx.Err().Error()))
			return nil
		case <-timer.C:
		}
	}
}
