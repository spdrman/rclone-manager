package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// cmdDaemon is FR-1's `daemon` execution mode: repeat the same processing
// cycle `run` performs once, at the configured poll_interval, until
// SIGTERM or SIGINT.
//
// The signal handling here is signal.NotifyContext: FR-1 asks for "handle
// SIGTERM/SIGINT" and "use Go context cancellation" together, and this is
// exactly that pairing. internal/app.Service.Daemon never installs one of
// its own (see its doc); it only ever reacts to ctx, which is what keeps
// that package testable without a test having to send itself a real
// signal, and keeps this command the place that decides what a signal
// means for this process.
//
// # Deciding, rather than racing
//
// Installing a handler is not enough on its own to be the one deciding.
// The embedded rclone installs a SIGTERM/SIGINT handler of its own the
// first time a transfer registers an at-exit function, and that handler
// ends the process with 128+signal, so an ordinary stop reported 143 and
// arrived whenever rclone got there rather than when this daemon had
// finished shutting down (issue #190). Under systemd that marks the unit
// failed unless SuccessExitStatus=143 is configured, so `systemctl stop`
// and every restart look like crashes: they count against the burst limit
// and they alert. Docker and Kubernetes read a nonzero exit on stop the
// same way, and docs/deployment.md documents running this as a long-lived
// service, so an operator stopping it is routine. A stop that was
// requested, and that the daemon performed, is a successful stop, so this
// takes the signal away from rclone before it can take the process, and
// runs rclone's registered exit handlers itself on the way out. What it
// does not do is flatten a genuine failure: an error out of Daemon is
// still reported through fail, and still exits 1.
func cmdDaemon(args []string) int {
	fs, cfgPath := newFlagSet("daemon")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Before anything can reach a remote, so rclone's own handler is never
	// installed at all. See rclone.DisableSignalExit.
	rclone.DisableSignalExit()
	defer rclone.RunExitHandlers()

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
