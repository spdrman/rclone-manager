// This file covers the unattended loop, and the cases split cleanly into
// what it must do and what it must survive.
//
// Ticking and stopping on cancel are the easy half. The other half is
// about a process that hosts an HTTP API alongside this loop: a cycle
// that panics must not take the API down with it, must not walk away
// still holding the single-flight lock, and must not stop the loop. That
// last one is the case a naive recover passes and a wrongly placed one
// fails silently, leaving a scheduler that is technically alive and has
// run nothing since the first bad afternoon.
//
// The overlap case is the invariant the whole design rests on: an
// operator-submitted run and a scheduled tick are two independent callers
// of the same engine, and the engine's own contract is that two passes
// never overlap on one backup set.
package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// TestPollInterval_ReportsTheConfiguredValue is what lets a caller
// outside core/ (apps/generic's serve command) drive RunOnSchedule at the
// operator's own configured cadence without needing internal/config
// access, which it cannot have (§7.2): cmd/backup-manager's own `daemon`
// command reads cfg.PollInterval.Duration() directly off a
// *config.Config it constructed itself, a shortcut apps/ has no
// equivalent for.
func TestPollInterval_ReportsTheConfiguredValue(t *testing.T) {
	cfg := testConfig()
	cfg.PollInterval = config.Duration(37 * time.Minute)
	svc := New(cfg, openTestJournal(t), nil, nil)

	if got, want := svc.PollInterval(), 37*time.Minute; got != want {
		t.Errorf("PollInterval() = %s, want %s", got, want)
	}
}

// TestRunOnSchedule_RejectsNonPositiveInterval mirrors
// internal/app.Service.Daemon's own validation (core/internal/app/daemon.go):
// RunOnSchedule is this package's equivalent entry point for a process
// that also runs an HTTP API (docs/EPIC-B-multi-nas.md §9.3), so it must
// not silently accept an interval that would spin a tight loop.
func TestRunOnSchedule_RejectsNonPositiveInterval(t *testing.T) {
	svc := newTestService(t)
	if err := svc.RunOnSchedule(context.Background(), 0); err == nil {
		t.Fatal("RunOnSchedule(ctx, 0) = nil error, want a non-nil error")
	}
	if err := svc.RunOnSchedule(context.Background(), -time.Second); err == nil {
		t.Fatal("RunOnSchedule(ctx, -1s) = nil error, want a non-nil error")
	}
}

// TestRunOnSchedule_RepeatsAtIntervalAndStopsOnCancel proves the basic
// scheduler-loop contract, the same shape internal/app.Service.Daemon
// already proves for the plain CLI: it runs more than once over several
// multiples of a short interval, and returns once ctx is canceled.
func TestRunOnSchedule_RepeatsAtIntervalAndStopsOnCancel(t *testing.T) {
	svc := newTestService(t)

	var cycles int32
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		atomic.AddInt32(&cycles, 1)
		return app.CycleReport{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- svc.RunOnSchedule(ctx, 20*time.Millisecond) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnSchedule returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunOnSchedule did not return after its context was canceled")
	}

	if got := atomic.LoadInt32(&cycles); got < 2 {
		t.Errorf("runCycle ran %d time(s) over 150ms at a 20ms interval, want at least 2", got)
	}
}

// TestRunOnSchedule_NeverOverlapsAnAPISubmittedRunCycle is §9.3's "the
// HTTP server and background scheduler SHALL share a common application
// service" requirement, made concrete: the scheduler loop this method
// drives and SubmitRunCycle's own goroutine-per-operation (operations.go)
// both ultimately call internal/app.Service.RunCycle on the SAME
// BackupService, and internal/app/cycle.go's own doc says two concurrent
// passes over the same backup set must never happen. Without sharing
// BackupService's runOnce guard, a process running both an HTTP API and
// this scheduler loop could violate that the moment an operator submits a
// manual run while a scheduled one is already executing.
//
// This drives that race directly: hold runOnce open (as SubmitRunCycle's
// own executeRunCycle would while actually running) and prove
// RunOnSchedule's own tick, arriving while that lock is held, does NOT
// also enter runCycle concurrently — it must skip that tick instead.
func TestRunOnSchedule_NeverOverlapsAnAPISubmittedRunCycle(t *testing.T) {
	svc := newTestService(t)

	var concurrent int32
	var maxConcurrent int32
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		n := atomic.AddInt32(&concurrent, 1)
		for {
			max := atomic.LoadInt32(&maxConcurrent)
			if n <= max || atomic.CompareAndSwapInt32(&maxConcurrent, max, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		return app.CycleReport{}
	})

	// Simulate an API-submitted operation holding the single-flight lock
	// for the whole test, exactly as executeRunCycle does while inside
	// RunCycle.
	if !svc.runOnce.TryLock() {
		t.Fatal("runOnce.TryLock() failed on a fresh BackupService")
	}
	defer svc.runOnce.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := svc.RunOnSchedule(ctx, 10*time.Millisecond); err != nil {
		t.Fatalf("RunOnSchedule: %v", err)
	}

	if got := atomic.LoadInt32(&maxConcurrent); got > 0 {
		t.Errorf("runCycle ran %d time(s) concurrently with the held API operation, want 0 (every scheduled tick must be skipped while runOnce is held)", got)
	}
}

// TestRunScheduledCycle_RecoversFromPanicAndReleasesTheLock is issue
// #119's review finding 5: runScheduledCycle used to call runCycle with
// no panic recovery at all, unlike its sibling executeRunCycle
// (operations.go), even though this package is what first makes the
// scheduler share a process with the persistent API server that same
// sibling's own doc comment warns an unrecovered panic would crash. This
// test reaching its final assertion at all is itself part of what it
// proves: an unrecovered panic inside runCycle would crash this whole
// test binary, not just fail this one test.
func TestRunScheduledCycle_RecoversFromPanicAndReleasesTheLock(t *testing.T) {
	svc := newTestService(t)

	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		panic("boom: simulated panic inside a scheduled RunCycle pass")
	})

	svc.runScheduledCycle(context.Background())

	// The single-flight lock must have been released by the panic-recovery
	// path, not left held forever: both a subsequent scheduled tick AND a
	// subsequent API-submitted operation must still be able to run
	// afterward.
	if !svc.runOnce.TryLock() {
		t.Fatal("runOnce is still held after a panicking scheduled cycle recovered - the lock was never released")
	}
	svc.runOnce.Unlock()

	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		return app.CycleReport{}
	})
	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-after-scheduled-panic",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle after a recovered scheduled-cycle panic: %v", err)
	}
	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Status != "completed" {
		t.Fatalf("operation submitted after the recovered panic: Status = %q, want %q (Error = %q)", done.Status, "completed", done.Error)
	}
}

// TestRunOnSchedule_ContinuesTickingAfterAPanickingCycle proves the
// panic-recovery fix at RunOnSchedule's own level, not just
// runScheduledCycle in isolation: a tick that panics must not kill the
// scheduler loop itself - it has to keep running at the next interval,
// the same "just try again next tick" behavior a skipped tick already
// gets.
func TestRunOnSchedule_ContinuesTickingAfterAPanickingCycle(t *testing.T) {
	svc := newTestService(t)

	var cycles int32
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		n := atomic.AddInt32(&cycles, 1)
		if n == 1 {
			panic("boom: simulated panic on the first scheduled tick")
		}
		return app.CycleReport{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- svc.RunOnSchedule(ctx, 20*time.Millisecond) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnSchedule returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunOnSchedule did not return after its context was canceled")
	}

	if got := atomic.LoadInt32(&cycles); got < 2 {
		t.Errorf("runCycle ran %d time(s) over 150ms at a 20ms interval after an initial panic, want at least 2 (the loop must survive a panicking tick)", got)
	}
}
