package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/obs"
)

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
