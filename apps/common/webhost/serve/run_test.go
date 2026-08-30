// run_test.go pins the §9.3 orchestration contract this issue moves here
// from apps/generic/cmd/backup-manager-web's former cmdServe: the HTTP
// server and the background scheduler run as two independent goroutines
// racing against one shared shutdown context, and neither one's own
// failure is allowed to go unnoticed.
//
// TestRunEngine_SchedulerErrorIsReportedPromptly is the one test in this
// file proving a real behavior change, not just a relocation: the
// original cmdServe's primary select only ever raced ctx.Done() against
// serverErrCh, never schedulerErrCh, so a scheduler that failed on its
// own (independent of ctx cancellation or an HTTP listener failure) was
// invisible until something else eventually triggered shutdown - or
// forever, if nothing else ever did. RunEngine's primary select adds that
// missing case (see run.go).
package serve_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
)

// fakeScheduler is a serve.Scheduler test double whose RunOnSchedule
// behavior a test controls directly, without a real
// core/service.BackupService or its SQLite journal.
type fakeScheduler struct {
	pollInterval time.Duration
	runFunc      func(ctx context.Context, interval time.Duration) error
}

func (f fakeScheduler) PollInterval() time.Duration { return f.pollInterval }

func (f fakeScheduler) RunOnSchedule(ctx context.Context, interval time.Duration) error {
	return f.runFunc(ctx, interval)
}

const testShutdownGrace = 200 * time.Millisecond

func newLoopbackServer(handler http.Handler) *http.Server {
	return serve.NewHTTPServer("127.0.0.1:0", handler)
}

// TestRunEngine_StopsCleanlyOnContextCancel proves the normal shutdown
// path: canceling ctx stops both the HTTP server and the scheduler loop,
// and RunEngine returns a nil error.
func TestRunEngine_StopsCleanlyOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	scheduler := fakeScheduler{
		pollInterval: time.Hour,
		runFunc: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return nil
		},
	}

	httpServer := newLoopbackServer(http.NotFoundHandler())

	done := make(chan error, 1)
	go func() { done <- serve.RunEngine(ctx, httpServer, scheduler, testShutdownGrace, io.Discard) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunEngine() = %v, want nil after a normal context-cancel shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunEngine did not return within 2s of ctx cancellation")
	}
}

// TestRunEngine_NilSchedulerRunsHTTPServerOnly proves a nil Scheduler is a
// legal, fully-supported input (the shape apps/generic's serve-ui command
// needs: no BackupService, no scheduler loop, just an HTTP server sharing
// the same shutdown-context orchestration).
func TestRunEngine_NilSchedulerRunsHTTPServerOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	httpServer := newLoopbackServer(http.NotFoundHandler())

	done := make(chan error, 1)
	go func() { done <- serve.RunEngine(ctx, httpServer, nil, testShutdownGrace, io.Discard) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunEngine(nil scheduler) = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunEngine(nil scheduler) did not return within 2s of ctx cancellation")
	}
}

// TestRunEngine_SchedulerErrorIsReportedPromptly is this file's central
// regression test - see this file's own doc comment above.
func TestRunEngine_SchedulerErrorIsReportedPromptly(t *testing.T) {
	wantErr := errors.New("scheduler exploded")
	scheduler := fakeScheduler{
		pollInterval: time.Hour,
		runFunc: func(context.Context, time.Duration) error {
			return wantErr // fails immediately, independent of ctx
		},
	}

	httpServer := newLoopbackServer(http.NotFoundHandler())

	done := make(chan error, 1)
	// context.Background(): deliberately never canceled by this test -
	// the whole point is that RunEngine must notice the scheduler's own
	// failure without any help from ctx cancellation.
	go func() {
		done <- serve.RunEngine(context.Background(), httpServer, scheduler, testShutdownGrace, io.Discard)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunEngine() = nil, want a non-nil error reporting the scheduler's failure")
		}
		if !errors.Is(err, wantErr) {
			t.Errorf("RunEngine() = %v, want an error wrapping %v", err, wantErr)
		}
		if !strings.Contains(err.Error(), "scheduler") {
			t.Errorf("RunEngine() error = %q, want it to mention \"scheduler\"", err.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunEngine did not report the scheduler's own failure within 2s - the primary select must race schedulerErrCh alongside ctx.Done()/serverErrCh, not just the latter two")
	}
}

// TestRunEngine_ServerErrorIsReported proves the mirror-image case (the
// one the original cmdServe already handled): an HTTP listener that fails
// on its own is reported, and does not hang waiting for ctx or the
// scheduler.
func TestRunEngine_ServerErrorIsReported(t *testing.T) {
	scheduler := fakeScheduler{
		pollInterval: time.Hour,
		runFunc: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return nil
		},
	}

	// An invalid address makes net.Listen (inside ListenAndServe) fail
	// synchronously, well before any real network I/O.
	httpServer := serve.NewHTTPServer(":-1", http.NotFoundHandler())

	done := make(chan error, 1)
	go func() {
		done <- serve.RunEngine(context.Background(), httpServer, scheduler, testShutdownGrace, io.Discard)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunEngine() = nil, want a non-nil error reporting the HTTP listener's failure")
		}
		if !strings.Contains(err.Error(), "http server") {
			t.Errorf("RunEngine() error = %q, want it to mention \"http server\"", err.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunEngine did not report the HTTP listener's own failure within 2s")
	}
}

// TestNewHTTPServer_SetsTimeouts is issue #119's review finding that
// neither http.Server the generic Web host built set any request-level
// timeout at all (the standard Go "Slowloris" gap) - moved here from
// apps/generic/cmd/backup-manager-web's former newHTTPServer helper,
// which every caller (serve and serve-ui) now gets from this shared
// constructor instead of building its own.
func TestNewHTTPServer_SetsTimeouts(t *testing.T) {
	s := serve.NewHTTPServer(":0", http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout is not set (Slowloris protection)")
	}
	if s.ReadTimeout <= 0 {
		t.Error("ReadTimeout is not set")
	}
	if s.WriteTimeout <= 0 {
		t.Error("WriteTimeout is not set")
	}
	if s.IdleTimeout <= 0 {
		t.Error("IdleTimeout is not set")
	}
}
