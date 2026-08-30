package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTP server timeouts NewHTTPServer applies to every caller: issue
// #119's review flagged that the generic Web host's own http.Server
// values set no request-level timeout at all - the standard Go
// "Slowloris" gap, where a client that opens a connection and trickles
// headers (or a request body) in slowly forever ties up a server
// goroutine indefinitely. An engine sharing a process with the backup
// scheduler (§9.3) means a resource exhausted here has a wider blast
// radius than just one stuck request, so every caller of RunEngine gets
// the same hardening rather than building its own *http.Server by hand.
const (
	ReadHeaderTimeout = 10 * time.Second
	ReadTimeout       = 30 * time.Second
	WriteTimeout      = 30 * time.Second
	IdleTimeout       = 120 * time.Second
)

// NewHTTPServer builds the one *http.Server shape every RunEngine caller
// uses, so the timeouts above can never be set on one provider's HTTP
// surface and forgotten on another's.
func NewHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
	}
}

// Scheduler is the subset of core/service.BackupService's method set
// RunEngine needs to drive the background scheduler loop alongside the
// HTTP server, sharing one process shutdown context (§9.3).
// *service.BackupService satisfies this directly; no adapter needed.
type Scheduler interface {
	PollInterval() time.Duration
	RunOnSchedule(ctx context.Context, interval time.Duration) error
}

// DefaultShutdownGrace bounds how long RunEngine waits for the HTTP
// server's graceful Shutdown, and separately for the scheduler loop's own
// exit, before giving up on a clean stop and returning anyway - the
// process is exiting either way once ctx is canceled; this only decides
// how long RunEngine waits first.
const DefaultShutdownGrace = 10 * time.Second

// RunEngine drives docs/EPIC-B-multi-nas.md §9.3's mandatory
// orchestration: httpServer and, if scheduler is non-nil,
// scheduler.RunOnSchedule run as independent goroutines racing against
// the SAME ctx - neither one tells the other to stop; both react only to
// ctx, or to their own failure (see the three-way select below). That is
// what "failure of the HTTP listener MUST NOT bypass lifecycle safety"
// (§9.3) actually requires: if the HTTP listener dies for its own reasons
// (a bind error, say), that must not itself cancel the caller's ctx and
// tear down the scheduler out from under an in-flight backup - but it
// also must not leave the scheduler goroutine running forever past
// RunEngine's own return, either. RunEngine resolves that by deriving
// schedCtx, a child of ctx, and handing that (not ctx itself) to
// scheduler.RunOnSchedule: the moment the primary select below fires, on
// every branch, RunEngine cancels schedCtx itself, so the scheduler loop
// gets asked to stop before RunEngine returns even when ctx never was and
// never will be canceled by anyone else. The caller's own ctx is never
// touched, so a caller reusing ctx after a non-fatal-looking error is
// still safe: schedCtx, not ctx, is what stops the scheduler here. The
// scheduler's own failure gets the exact same treatment, symmetrically:
// this used to be missing (moved here from
// apps/generic/cmd/backup-manager-web's former cmdServe, which only raced
// ctx.Done() against the HTTP listener's own error channel - a scheduler
// that failed on its own, independent of ctx cancellation or a listener
// failure, was invisible until some other event eventually triggered
// shutdown, or forever if nothing else did).
//
// scheduler may be nil (e.g. a UI-host-only container with no
// BackupService of its own, apps/generic's serve-ui) - RunEngine then
// just runs httpServer under ctx.
//
// warnings receives non-fatal notices (currently just "the scheduler loop
// didn't stop within shutdownGrace") that don't themselves change
// RunEngine's returned error - pass io.Discard to ignore them, or a real
// writer (a caller's os.Stderr) to surface them the way
// apps/generic/cmd/backup-manager-web's former cmdServe did.
func RunEngine(ctx context.Context, httpServer *http.Server, scheduler Scheduler, shutdownGrace time.Duration, warnings io.Writer) error {
	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- httpServer.ListenAndServe() }()

	// schedCtx is the scheduler's own child of ctx, so RunEngine can stop
	// the scheduler loop on every exit path without ever canceling the
	// caller's ctx. The defer is a safety net (and keeps `go vet`'s
	// lostcancel check happy); the explicit call right after the primary
	// select below is what actually matters, since it lets the scheduler
	// notice cancellation immediately instead of only once RunEngine's
	// caller eventually cancels ctx or the process exits.
	schedCtx, cancelSched := context.WithCancel(ctx)
	defer cancelSched()

	var schedulerErrCh chan error
	if scheduler != nil {
		schedulerErrCh = make(chan error, 1)
		go func() { schedulerErrCh <- scheduler.RunOnSchedule(schedCtx, scheduler.PollInterval()) }()
	}

	var exitErr error
	schedulerAlreadyReported := false

	// schedulerErrCh is nil when scheduler == nil: a nil channel in a
	// select case simply never becomes ready, so this case is dead code
	// (in the good sense) for a caller with no scheduler at all - the
	// same three-way select serves both apps/generic's serve (scheduler
	// set) and serve-ui (scheduler nil) without a separate code path.
	select {
	case <-ctx.Done():
		// Normal shutdown path: SIGTERM/SIGINT, or a caller-driven cancel.
	case err := <-serverErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			exitErr = fmt.Errorf("http server: %w", err)
		}
	case err := <-schedulerErrCh:
		schedulerAlreadyReported = true
		if err != nil {
			exitErr = fmt.Errorf("scheduler: %w", err)
		}
	}
	// Ask the scheduler to stop now, unconditionally, regardless of which
	// branch above fired - including the serverErrCh branch, where ctx
	// itself is deliberately left uncanceled (see the doc comment above).
	// Without this call, an HTTP-listener failure would leave the
	// scheduler goroutine with nothing able to stop it, running past
	// RunEngine's own return.
	cancelSched()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && exitErr == nil {
		exitErr = fmt.Errorf("http server shutdown: %w", err)
	}

	if scheduler != nil && !schedulerAlreadyReported {
		select {
		case err := <-schedulerErrCh:
			if err != nil && exitErr == nil {
				exitErr = fmt.Errorf("scheduler: %w", err)
			}
		case <-time.After(shutdownGrace):
			_, _ = fmt.Fprintln(warnings, "backup-manager-web: timed out waiting for the scheduler loop to stop")
		}
	}

	return exitErr
}
