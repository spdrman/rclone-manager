package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdDaemon is FR-1's `daemon` execution mode: repeat the same processing
// cycle `run` performs once, at the configured poll_interval, until
// SIGTERM or SIGINT.
//
// This is the one place in the whole binary that installs a signal
// handler, via signal.NotifyContext: FR-1 asks for "handle SIGTERM/SIGINT"
// and "use Go context cancellation" together, and this is exactly that
// pairing. internal/app.Service.Daemon never installs one of its own (see
// its doc); it only ever reacts to ctx, which is what keeps that package
// testable without a test having to send itself a real signal, and keeps
// this command the one place that decides what a signal means for this
// process.
func cmdDaemon(args []string) int {
	fs, cfgPath := newFlagSet("daemon")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, cfg, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))

	if err := svc.Daemon(ctx, cfg.PollInterval.Duration()); err != nil {
		return fail(err)
	}
	return 0
}
