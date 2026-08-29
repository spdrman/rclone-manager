package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// cmdRun is FR-1's `run` execution mode: perform one processing cycle and
// exit. Its context is cancelled by SIGTERM/SIGINT exactly like daemon's
// (see run's own doc on that signal wiring): `run` is a single cycle, not
// a loop, but the same shutdown-safety property has to hold for it too, a
// `run` invoked from a cron job can be signalled mid-cycle by the same
// operator action (a deploy, a host reboot) that would signal a daemon,
// and internal/app.RunCycle's ctx-checking discipline does not know or
// care which execution mode is driving it.
func cmdRun(args []string) int {
	fs, cfgPath := newFlagSet("run")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, _, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))

	report := svc.RunCycle(ctx)

	failed := false
	for _, s := range report.Sets {
		if s.Err != nil {
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}
