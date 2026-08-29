package state

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testOperationRequest returns a valid OperationRequest a test can tweak one
// field of, mirroring journal_test.go's testArtifact/testConfig pattern.
func testOperationRequest(operationID, idempotencyKey string) OperationRequest {
	return OperationRequest{
		OperationID:    operationID,
		IdempotencyKey: idempotencyKey,
		Actor:          "alice",
		BackupSet:      "",
		ConfigRevision: "rev-1",
		Action:         "run_cycle",
		Parameters:     "{}",
		CreatedAt:      time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
	}
}

// TestCreateOperation_PersistsRowBeforeCallerActs is issue #94's central
// durability claim made concrete at the journal layer: the row exists,
// durably, the moment CreateOperation returns, independent of whatever the
// caller does next (start a goroutine, return an HTTP response, crash).
// Reading it back through a brand new Get call, rather than trusting the
// value CreateOperation itself returned, is what actually proves it was
// written, not merely held in memory.
func TestCreateOperation_PersistsRowBeforeCallerActs(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	req := testOperationRequest("op_1", "idem-1")
	outcome, err := j.CreateOperation(ctx, req)
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if !outcome.Created {
		t.Fatal("Created = false, want true for a brand new idempotency key")
	}
	if outcome.Operation.Status != OperationQueued {
		t.Fatalf("Status = %q, want %q", outcome.Operation.Status, OperationQueued)
	}
	if outcome.Operation.StartedAt != nil {
		t.Fatalf("StartedAt = %v, want nil before execution begins", outcome.Operation.StartedAt)
	}
	if outcome.Operation.FinishedAt != nil {
		t.Fatalf("FinishedAt = %v, want nil before execution begins", outcome.Operation.FinishedAt)
	}

	got, err := j.GetOperation(ctx, "op_1")
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if got.OperationID != "op_1" {
		t.Errorf("OperationID = %q, want %q", got.OperationID, "op_1")
	}
	if got.IdempotencyKey != "idem-1" {
		t.Errorf("IdempotencyKey = %q, want %q", got.IdempotencyKey, "idem-1")
	}
	if got.Actor != "alice" {
		t.Errorf("Actor = %q, want %q", got.Actor, "alice")
	}
	if got.ConfigRevision != "rev-1" {
		t.Errorf("ConfigRevision = %q, want %q", got.ConfigRevision, "rev-1")
	}
	if got.Action != "run_cycle" {
		t.Errorf("Action = %q, want %q", got.Action, "run_cycle")
	}
	if got.Status != OperationQueued {
		t.Errorf("Status = %q, want %q", got.Status, OperationQueued)
	}
	if !got.CreatedAt.Equal(req.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, req.CreatedAt)
	}
}

// TestCreateOperation_DuplicateIdempotencyKeyReturnsExistingOperation is the
// acceptance criterion "duplicate idempotency key does not create duplicate
// work": a second CreateOperation call presenting the same idempotency key
// (as a naive client retry would after not seeing a response) must return
// the exact original row, not a second one, and must report Created = false
// so the caller knows not to start a second execution.
func TestCreateOperation_DuplicateIdempotencyKeyReturnsExistingOperation(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	first, err := j.CreateOperation(ctx, testOperationRequest("op_1", "idem-1"))
	if err != nil {
		t.Fatalf("first CreateOperation: %v", err)
	}
	if !first.Created {
		t.Fatal("first call: Created = false, want true")
	}

	// A retry generates a fresh OperationID (as a real caller minting a new
	// UUID per attempt would) but replays the same IdempotencyKey and
	// CreatedAt-independent fields; the stored OperationID must stay the
	// first one.
	retryReq := testOperationRequest("op_2_should_be_ignored", "idem-1")
	second, err := j.CreateOperation(ctx, retryReq)
	if err != nil {
		t.Fatalf("replayed CreateOperation: %v", err)
	}
	if second.Created {
		t.Fatal("replayed call: Created = true, want false (already exists)")
	}
	if second.Operation.OperationID != first.Operation.OperationID {
		t.Fatalf("replayed call returned OperationID %q, want the original %q",
			second.Operation.OperationID, first.Operation.OperationID)
	}
	if !second.Operation.CreatedAt.Equal(first.Operation.CreatedAt) {
		t.Fatalf("replayed call returned CreatedAt %v, want the original %v",
			second.Operation.CreatedAt, first.Operation.CreatedAt)
	}

	// Only one row should exist at all.
	var count int
	if err := j.db.QueryRowContext(ctx, `SELECT count(*) FROM operations`).Scan(&count); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	if count != 1 {
		t.Fatalf("operations table has %d rows, want exactly 1", count)
	}
}

func TestCreateOperation_DifferentIdempotencyKeysCreateDistinctOperations(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	first, err := j.CreateOperation(ctx, testOperationRequest("op_1", "idem-1"))
	if err != nil {
		t.Fatalf("first CreateOperation: %v", err)
	}
	second, err := j.CreateOperation(ctx, testOperationRequest("op_2", "idem-2"))
	if err != nil {
		t.Fatalf("second CreateOperation: %v", err)
	}
	if !first.Created || !second.Created {
		t.Fatalf("both calls should create a new row: first.Created=%v second.Created=%v", first.Created, second.Created)
	}
	if first.Operation.OperationID == second.Operation.OperationID {
		t.Fatal("two different idempotency keys produced the same OperationID")
	}
}

// TestCreateOperation_ReusedKeyForDifferentActionIsRefused mirrors
// RecordTransition's ErrIdempotencyKeyReused: an idempotency key is a
// promise about one specific logical request, not a namespace a second,
// different request can reuse and get silently served the first request's
// result.
func TestCreateOperation_ReusedKeyForDifferentActionIsRefused(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	if _, err := j.CreateOperation(ctx, testOperationRequest("op_1", "idem-1")); err != nil {
		t.Fatalf("first CreateOperation: %v", err)
	}

	mismatched := testOperationRequest("op_2", "idem-1")
	mismatched.Action = "some_other_action"
	_, err := j.CreateOperation(ctx, mismatched)
	if !errors.Is(err, ErrOperationIdempotencyKeyReused) {
		t.Fatalf("CreateOperation error = %v, want errors.Is(err, ErrOperationIdempotencyKeyReused)", err)
	}
}

// TestCreateOperation_ReusedKeyForDifferentActorIsRefused is the security
// half of the same claim: a different caller presenting the same
// idempotency key must never be handed back the original caller's
// operation (which may carry that caller's Result/Error), even when the
// Action and ConfigRevision happen to match.
func TestCreateOperation_ReusedKeyForDifferentActorIsRefused(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	first := testOperationRequest("op_1", "idem-1")
	first.Actor = "alice"
	if _, err := j.CreateOperation(ctx, first); err != nil {
		t.Fatalf("first CreateOperation: %v", err)
	}

	second := testOperationRequest("op_2", "idem-1")
	second.Actor = "mallory"
	_, err := j.CreateOperation(ctx, second)
	if !errors.Is(err, ErrOperationIdempotencyKeyReused) {
		t.Fatalf("CreateOperation error = %v, want errors.Is(err, ErrOperationIdempotencyKeyReused)", err)
	}
}

func TestCreateOperation_RequiresIdempotencyKeyOperationIDAndAction(t *testing.T) {
	base := testOperationRequest("op_1", "idem-1")

	tests := []struct {
		name string
		req  OperationRequest
	}{
		{"missing idempotency key", func() OperationRequest { r := base; r.IdempotencyKey = ""; return r }()},
		{"missing operation id", func() OperationRequest { r := base; r.OperationID = ""; return r }()},
		{"missing action", func() OperationRequest { r := base; r.Action = ""; return r }()},
		{"missing created at", func() OperationRequest { r := base; r.CreatedAt = time.Time{}; return r }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j, _ := openJournal(t)
			if _, err := j.CreateOperation(context.Background(), tt.req); err == nil {
				t.Fatal("CreateOperation: error = nil, want a validation error")
			}
		})
	}
}

func TestGetOperation_UnknownIDReturnsErrOperationNotFound(t *testing.T) {
	j, _ := openJournal(t)
	_, err := j.GetOperation(context.Background(), "op_does_not_exist")
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("GetOperation error = %v, want errors.Is(err, ErrOperationNotFound)", err)
	}
}

// TestMarkOperationRunning_ThenCompleteOperation_RecordsTimestampsAndResult
// exercises the full lifecycle a successful asynchronous run drives an
// operation through: queued -> running -> completed, each step's timestamp
// recorded durably so a poller can distinguish "still queued", "in
// progress" and "done" without ever touching the HTTP request that
// originally submitted it.
func TestMarkOperationRunning_ThenCompleteOperation_RecordsTimestampsAndResult(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	if _, err := j.CreateOperation(ctx, testOperationRequest("op_1", "idem-1")); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	startedAt := time.Date(2026, 8, 29, 12, 0, 1, 0, time.UTC)
	if err := j.MarkOperationRunning(ctx, "op_1", startedAt); err != nil {
		t.Fatalf("MarkOperationRunning: %v", err)
	}

	running, err := j.GetOperation(ctx, "op_1")
	if err != nil {
		t.Fatalf("GetOperation after MarkOperationRunning: %v", err)
	}
	if running.Status != OperationRunning {
		t.Errorf("Status = %q, want %q", running.Status, OperationRunning)
	}
	if running.StartedAt == nil || !running.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt = %v, want %v", running.StartedAt, startedAt)
	}

	finishedAt := time.Date(2026, 8, 29, 12, 0, 5, 0, time.UTC)
	if err := j.CompleteOperation(ctx, "op_1", finishedAt, `{"backup_sets_processed":0}`); err != nil {
		t.Fatalf("CompleteOperation: %v", err)
	}

	done, err := j.GetOperation(ctx, "op_1")
	if err != nil {
		t.Fatalf("GetOperation after CompleteOperation: %v", err)
	}
	if done.Status != OperationCompleted {
		t.Errorf("Status = %q, want %q", done.Status, OperationCompleted)
	}
	if done.FinishedAt == nil || !done.FinishedAt.Equal(finishedAt) {
		t.Errorf("FinishedAt = %v, want %v", done.FinishedAt, finishedAt)
	}
	if done.Result != `{"backup_sets_processed":0}` {
		t.Errorf("Result = %q, want the recorded result", done.Result)
	}
}

func TestFailOperation_RecordsErrorAndFinishedAt(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	if _, err := j.CreateOperation(ctx, testOperationRequest("op_1", "idem-1")); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if err := j.MarkOperationRunning(ctx, "op_1", time.Now()); err != nil {
		t.Fatalf("MarkOperationRunning: %v", err)
	}

	finishedAt := time.Date(2026, 8, 29, 12, 0, 9, 0, time.UTC)
	if err := j.FailOperation(ctx, "op_1", finishedAt, "reconcile: transport unreachable"); err != nil {
		t.Fatalf("FailOperation: %v", err)
	}

	got, err := j.GetOperation(ctx, "op_1")
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if got.Status != OperationFailed {
		t.Errorf("Status = %q, want %q", got.Status, OperationFailed)
	}
	if got.Error != "reconcile: transport unreachable" {
		t.Errorf("Error = %q, want the recorded failure", got.Error)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finishedAt) {
		t.Errorf("FinishedAt = %v, want %v", got.FinishedAt, finishedAt)
	}
}

func TestMarkOperationRunning_UnknownIDReturnsErrOperationNotFound(t *testing.T) {
	j, _ := openJournal(t)
	err := j.MarkOperationRunning(context.Background(), "op_does_not_exist", time.Now())
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("MarkOperationRunning error = %v, want errors.Is(err, ErrOperationNotFound)", err)
	}
}

func TestCompleteOperation_UnknownIDReturnsErrOperationNotFound(t *testing.T) {
	j, _ := openJournal(t)
	err := j.CompleteOperation(context.Background(), "op_does_not_exist", time.Now(), "{}")
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("CompleteOperation error = %v, want errors.Is(err, ErrOperationNotFound)", err)
	}
}

func TestFailOperation_UnknownIDReturnsErrOperationNotFound(t *testing.T) {
	j, _ := openJournal(t)
	err := j.FailOperation(context.Background(), "op_does_not_exist", time.Now(), "boom")
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("FailOperation error = %v, want errors.Is(err, ErrOperationNotFound)", err)
	}
}

// TestCreateOperation_ConcurrentSameIdempotencyKeyCreatesExactlyOne is the
// same race proof journal_test.go's
// TestRecordTransition_ConcurrentSameKeyAppliesExactlyOnce already
// establishes for state transitions, applied here to operations: many
// goroutines racing to submit the same idempotency key (the realistic
// shape of "a client's retry landed concurrently with its original,
// still-in-flight request", not just a sequential replay) must still
// produce exactly one row.
func TestCreateOperation_ConcurrentSameIdempotencyKeyCreatesExactlyOne(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	const goroutines = 8
	var created int64
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			outcome, err := j.CreateOperation(ctx, testOperationRequest("op_race", "idem-race"))
			errs[i] = err
			if err == nil && outcome.Created {
				atomic.AddInt64(&created, 1)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: CreateOperation: %v", i, err)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d, want exactly 1", created)
	}

	var count int
	if err := j.db.QueryRowContext(ctx, `SELECT count(*) FROM operations`).Scan(&count); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	if count != 1 {
		t.Fatalf("operations table has %d rows, want exactly 1", count)
	}
}
