// This file covers live progress, where every interesting case is about
// telling absent from zero.
//
// A reading of zero bytes on a transfer that has just started and no
// reading at all are the same JSON to anything that flattens them, and
// they are opposite claims: one says nothing has moved yet, the other
// says nobody can see. So the cases here are deliberately paired around
// that line: a measured zero survives, a finished operation reports
// nothing, an operation left running by a dead process reports nothing,
// and one that is running but has not reported yet also reports nothing.
//
// The last two are the same assertion from different sides, and both are
// needed. Serving the last reading of an operation this process is not
// executing would describe a transfer that no longer exists as though it
// were still moving, which is the one failure a progress bar must never
// have.
package service

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// int64p is a local helper: the byte fields are pointers precisely so a
// measured zero and an unmeasured field are distinguishable, which means
// tests have to be able to write both.
func int64p(v int64) *int64 { return &v }

// submitAndPublish submits a run cycle whose execution is replaced by fn,
// so a test controls exactly what the cycle publishes and when it returns.
// fn receives the observer core/service installed on the cycle's context,
// read back with app.ProgressObserverFrom, which is the seam under test.
func submitAndPublish(t *testing.T, svc *BackupService, fn func(obs app.ProgressObserver)) string {
	t.Helper()
	release := make(chan struct{})
	published := make(chan struct{})
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		obs := app.ProgressObserverFrom(ctx)
		if obs == nil {
			// The seam under test: if core/service does not install an
			// observer on the cycle's context, nothing a cycle publishes
			// can ever reach a client, and every assertion below would be
			// testing the stub rather than the wiring.
			t.Error("core/service ran a cycle without installing a progress observer on its context")
		} else {
			fn(obs)
		}
		close(published)
		<-release
		return app.CycleReport{}
	})
	t.Cleanup(func() { close(release) })

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "progress-" + t.Name(),
		Actor:          "tester",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("the stubbed cycle never ran")
	}
	return op.ID
}

// TestGetOperation_ReportsLiveProgressWhileRunning is the read path issue
// #221 asks for: what the cycle publishes is what an authenticated poll
// gets back, on the operation record itself, while it is running.
func TestGetOperation_ReportsLiveProgressWhileRunning(t *testing.T) {
	svc := newTestService(t, config.Source{Name: "alpha"})
	t.Cleanup(func() { _ = svc.Close() })

	id := submitAndPublish(t, svc, func(obs app.ProgressObserver) {
		obs.ObserveProgress(app.Progress{
			Stage:            app.StageTransferring,
			BackupSetID:      "alpha/nightly",
			BackupSetsDone:   1,
			BackupSetsTotal:  3,
			Artifact:         "nightly.dump",
			ArtifactsDone:    4,
			BytesTransferred: int64p(512),
			BytesTotal:       int64p(2048),
			BytesPerSecond:   int64p(128),
		})
	})

	op, err := svc.GetOperation(context.Background(), id)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Status != state.OperationRunning {
		t.Fatalf("status = %q, want %q", op.Status, state.OperationRunning)
	}
	if op.Progress == nil {
		t.Fatal("a running operation that has published a reading reports no progress")
	}
	p := op.Progress
	if p.Stage != app.StageTransferring {
		t.Errorf("Stage = %q, want %q", p.Stage, app.StageTransferring)
	}
	if p.BackupSetID != "alpha/nightly" || p.Artifact != "nightly.dump" {
		t.Errorf("BackupSetID/Artifact = %q/%q, want alpha/nightly and nightly.dump", p.BackupSetID, p.Artifact)
	}
	if p.BackupSetsDone != 1 || p.BackupSetsTotal != 3 || p.ArtifactsDone != 4 {
		t.Errorf("counters = %d/%d sets, %d artifacts; want 1/3 and 4", p.BackupSetsDone, p.BackupSetsTotal, p.ArtifactsDone)
	}
	// The intermediate reading, end to end: not 0, not the total.
	if p.BytesTransferred == nil || *p.BytesTransferred != 512 {
		t.Errorf("BytesTransferred = %v, want 512", p.BytesTransferred)
	}
	if p.BytesTotal == nil || *p.BytesTotal != 2048 {
		t.Errorf("BytesTotal = %v, want 2048", p.BytesTotal)
	}
	if p.BytesPerSecond == nil || *p.BytesPerSecond != 128 {
		t.Errorf("BytesPerSecond = %v, want 128", p.BytesPerSecond)
	}
	if p.Sequence != 1 {
		t.Errorf("Sequence = %d, want 1 for the first reading", p.Sequence)
	}
	if p.ObservedAt.IsZero() {
		t.Error("ObservedAt is the zero time; a client cannot tell a stalled transfer from a stalled service without it")
	}
}

// TestGetOperation_ProgressIsMonotonicAndSequenced proves the feed moves.
// A reading that never advances is indistinguishable from a service that
// has stopped sampling, which is exactly the state issue #221 says the UI
// is stuck in today.
func TestGetOperation_ProgressIsMonotonicAndSequenced(t *testing.T) {
	svc := newTestService(t, config.Source{Name: "alpha"})
	t.Cleanup(func() { _ = svc.Close() })

	id := submitAndPublish(t, svc, func(obs app.ProgressObserver) {
		for _, done := range []int64{0, 700, 1400, 2048} {
			obs.ObserveProgress(app.Progress{
				Stage:            app.StageTransferring,
				Artifact:         "nightly.dump",
				BytesTransferred: int64p(done),
				BytesTotal:       int64p(2048),
			})
		}
	})

	op, err := svc.GetOperation(context.Background(), id)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Progress == nil {
		t.Fatal("a running operation that has published four readings reports no progress")
	}
	if op.Progress.Sequence != 4 {
		t.Errorf("Sequence = %d, want 4 after four readings", op.Progress.Sequence)
	}
	if op.Progress.BytesTransferred == nil || *op.Progress.BytesTransferred != 2048 {
		t.Errorf("BytesTransferred = %v, want the latest reading (2048)", op.Progress.BytesTransferred)
	}
}

// TestGetOperation_MeasuredZeroIsNotAnAbsentReading is the distinction the
// whole design turns on, at the layer that serves it. A copy that has
// started and moved nothing yet is a measured zero; it must arrive as a
// present reading holding zero, never as "no progress".
func TestGetOperation_MeasuredZeroIsNotAnAbsentReading(t *testing.T) {
	svc := newTestService(t, config.Source{Name: "alpha"})
	t.Cleanup(func() { _ = svc.Close() })

	id := submitAndPublish(t, svc, func(obs app.ProgressObserver) {
		obs.ObserveProgress(app.Progress{
			Stage:            app.StageTransferring,
			Artifact:         "nightly.dump",
			BytesTransferred: int64p(0),
			BytesTotal:       int64p(2048),
		})
	})

	op, err := svc.GetOperation(context.Background(), id)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Progress == nil {
		t.Fatal("a measured zero arrived as no progress at all; those are different answers")
	}
	if op.Progress.BytesTransferred == nil {
		t.Fatal("a measured zero arrived as an unmeasured field")
	}
	if *op.Progress.BytesTransferred != 0 {
		t.Errorf("BytesTransferred = %d, want 0", *op.Progress.BytesTransferred)
	}
}

// TestGetOperation_FinishedOperationReportsNoProgress is the other half.
// Once a cycle has stopped, the last reading it managed to take describes
// nothing that is still happening, and serving it would show an operator a
// transfer that no longer exists.
func TestGetOperation_FinishedOperationReportsNoProgress(t *testing.T) {
	svc := newTestService(t, config.Source{Name: "alpha"})
	t.Cleanup(func() { _ = svc.Close() })

	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		if obs := app.ProgressObserverFrom(ctx); obs != nil {
			obs.ObserveProgress(app.Progress{
				Stage:            app.StageTransferring,
				Artifact:         "nightly.dump",
				BytesTransferred: int64p(1024),
				BytesTotal:       int64p(2048),
			})
		}
		return app.CycleReport{}
	})

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "finished-progress",
		Actor:          "tester",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Progress != nil {
		t.Fatalf("a %s operation still reports progress %+v; the reading describes a cycle that has stopped", done.Status, done.Progress)
	}
}

// TestGetOperation_OperationRunningBeforeARestartReportsNoProgress is the
// restart case, asserted explicitly rather than assumed.
//
// A row left at "running" by a process that died cannot have live progress
// in this one: the registry that held it died with the process. The
// startup sweep moves such a row to failed, and the read path must not
// invent a reading for it either way. Both halves are checked, because
// only checking the status would leave the interesting failure (a stale
// reading resurrected from somewhere) undetected.
func TestGetOperation_OperationRunningBeforeARestartReportsNoProgress(t *testing.T) {
	journal := openTestJournal(t)
	ctx := context.Background()

	// A row exactly as a killed process would leave it: created, marked
	// running, never finished.
	const id = "op_interrupted"
	if _, err := journal.CreateOperation(ctx, state.OperationRequest{
		OperationID:    id,
		IdempotencyKey: "interrupted-key",
		Actor:          "tester",
		ConfigRevision: "rev-1",
		Action:         ActionRunCycle,
		Parameters:     "{}",
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if err := journal.MarkOperationRunning(ctx, id, time.Now().UTC()); err != nil {
		t.Fatalf("MarkOperationRunning: %v", err)
	}

	// The restart: a brand new BackupService over the same journal, which
	// is what core/service.Open does on start.
	restarted := New(testConfig(config.Source{Name: "alpha"}), journal, nil, nil)
	t.Cleanup(func() { _ = restarted.Close() })

	op, err := restarted.GetOperation(ctx, id)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Progress != nil {
		t.Fatalf("an operation that was running before the restart reports progress %+v; "+
			"a reading from a process that no longer exists is not stale data, it is a transfer that is not happening", op.Progress)
	}
	if op.Status != state.OperationFailed {
		t.Errorf("status = %q, want %q: the startup sweep is what stops such a row polling as running forever", op.Status, state.OperationFailed)
	}
}

// TestGetOperation_RunningWithNoReadingYetReportsNoProgress covers the
// window between "this process registered the operation" and "the cycle
// published its first reading". It is short and it is real, and an
// all-zero reading served in it would render as a stalled bar.
func TestGetOperation_RunningWithNoReadingYetReportsNoProgress(t *testing.T) {
	svc := newTestService(t, config.Source{Name: "alpha"})
	t.Cleanup(func() { _ = svc.Close() })

	id := submitAndPublish(t, svc, func(obs app.ProgressObserver) {})

	op, err := svc.GetOperation(context.Background(), id)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Status != state.OperationRunning {
		t.Fatalf("status = %q, want %q", op.Status, state.OperationRunning)
	}
	if op.Progress != nil {
		t.Fatalf("an operation that has published nothing reports progress %+v, want none", op.Progress)
	}
}

// TestListOperations_CarriesProgressForTheRunningOneOnly proves the list
// read agrees with the single read: a client that reconnects and lists
// operations sees the live one measured and the historical ones not.
func TestListOperations_CarriesProgressForTheRunningOneOnly(t *testing.T) {
	svc := newTestService(t, config.Source{Name: "alpha"})
	t.Cleanup(func() { _ = svc.Close() })

	// One finished operation first.
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		return app.CycleReport{}
	})
	first, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "list-finished", Actor: "tester", ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	waitForTerminalStatus(t, svc, first.ID)

	running := submitAndPublish(t, svc, func(obs app.ProgressObserver) {
		obs.ObserveProgress(app.Progress{
			Stage: app.StageVerifying, Artifact: "nightly.dump", BackupSetsTotal: 1,
		})
	})

	ops, err := svc.ListOperations(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	byID := map[string]Operation{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	if got, ok := byID[running]; !ok || got.Progress == nil {
		t.Fatalf("the running operation is listed without progress (present=%v)", ok)
	} else if got.Progress.Stage != app.StageVerifying {
		t.Errorf("listed progress stage = %q, want %q", got.Progress.Stage, app.StageVerifying)
	}
	if got, ok := byID[first.ID]; !ok || got.Progress != nil {
		t.Errorf("the finished operation is listed with progress %+v, want none", got.Progress)
	}
}

// TestExecuteRunCycle_LeavesNoLiveProgressBehind is the registry's own
// housekeeping, asserted directly rather than through a read path.
//
// GetOperation refuses to serve a reading for a non-running operation
// anyway, so a stale entry would not reach a client through it. That makes
// this the only place the leak is visible: the registry is a map in a
// process that runs for months, and an entry per operation that is never
// removed grows with the deployment's whole history.
func TestExecuteRunCycle_LeavesNoLiveProgressBehind(t *testing.T) {
	svc := newTestService(t, config.Source{Name: "alpha"})
	t.Cleanup(func() { _ = svc.Close() })

	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		if obs := app.ProgressObserverFrom(ctx); obs != nil {
			obs.ObserveProgress(app.Progress{Stage: app.StageTransferring, Artifact: "nightly.dump"})
		}
		return app.CycleReport{}
	})

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "registry-housekeeping", Actor: "tester", ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	waitForTerminalStatus(t, svc, op.ID)

	if _, ok := svc.progress.snapshot(op.ID); ok {
		t.Errorf("the registry still holds a reading for %s after it finished", op.ID)
	}
	svc.progress.mu.RLock()
	held := len(svc.progress.running)
	svc.progress.mu.RUnlock()
	if held != 0 {
		t.Errorf("the registry holds %d entries after every operation finished, want 0", held)
	}
}
