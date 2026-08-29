package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// ActionRunCycle is the only Action this skeleton supports: run one whole
// internal/app.Service.RunCycle pass across every configured backup set.
// Per-backup-set actions (docs/EPIC-B-multi-nas.md §15.4's
// POST /api/v1/backup-sets/{id}/run) are later-phase work that needs
// backup-set CRUD/addressing this issue deliberately does not build; see
// this package's introducing PR description.
const ActionRunCycle = "run_cycle"

// ErrConfigRevisionStale is returned by SubmitRunCycle when the caller's
// ConfigRevision does not match BackupService.ConfigRevision(): the
// caller's picture of the running configuration is out of date, so the
// request is refused rather than silently applied against whatever the
// configuration actually is now (docs/EPIC-B-multi-nas.md §14, mirroring
// §15.6's RETENTION_PLAN_STALE).
var ErrConfigRevisionStale = errors.New("service: configuration revision is stale")

// ErrOperationNotFound is returned by GetOperation when no operation
// matches the given id.
var ErrOperationNotFound = errors.New("service: operation not found")

// RunCycleRequest is what a caller submits to start (or, replaying the
// same IdempotencyKey, resume observing) one run_cycle operation.
type RunCycleRequest struct {
	// IdempotencyKey identifies this exact logical request. Reusing it (a
	// client retry, a duplicate submission) returns the original
	// operation instead of starting a second one.
	IdempotencyKey string

	// Actor is the authenticated caller's identity, as resolved by
	// whatever capabilities.Authenticator the HTTP layer sits behind. This
	// package does not authenticate anything itself; it only records
	// whatever identity it is handed.
	Actor string

	// ConfigRevision must equal BackupService.ConfigRevision() at the
	// moment this call is made, or the request is refused with
	// ErrConfigRevisionStale. It is required, not optional: a request that
	// does not say which configuration it was issued against cannot be
	// checked for staleness at all, so an empty value is a validation
	// error, never treated as "skip the check".
	ConfigRevision string
}

// Operation is the plain, provider-agnostic shape of one durable operation
// (docs/EPIC-B-multi-nas.md §14's example, plus the polling fields
// §15.7 needs). It carries nothing from internal/state.Operation that
// this package has not deliberately decided to expose.
type Operation struct {
	ID             string
	IdempotencyKey string
	Actor          string
	// BackupSetID is empty for a run_cycle operation (it is not scoped to
	// one configured backup set); see ActionRunCycle's doc.
	BackupSetID    string
	ConfigRevision string
	Action         string

	// Status is one of "queued", "running", "completed", "failed".
	Status string

	CreatedAt time.Time
	// StartedAt and FinishedAt are the zero time.Time until the
	// corresponding event has actually happened; a caller checks
	// IsZero() rather than relying on Status alone.
	StartedAt  time.Time
	FinishedAt time.Time

	// Result is an opaque, human/JSON-readable summary of a completed
	// operation. Error is the same for a failed one. At most one of the
	// two is ever non-empty.
	Result string
	Error  string
}

// SubmitRunCycle persists a new run_cycle operation and starts executing it
// asynchronously, or recognises RunCycleRequest.IdempotencyKey as an
// already-submitted request and returns that operation unchanged.
//
// # Persisted before execution begins
//
// The operation row is written durably (internal/state.Journal.CreateOperation)
// and this function has already returned it to the caller before the
// asynchronous execution below gets a chance to run at all: whatever the
// caller does next (start rendering a response, the browser tab closing,
// the whole HTTP request timing out) cannot un-persist it.
//
// # Decoupled from the caller's context
//
// The asynchronous execution below is started with context.Background(),
// deliberately NOT ctx: ctx belongs to the caller (an HTTP handler's
// request context in the real webhost wiring), and docs/EPIC-B-multi-nas.md
// §14 is explicit that "HTTP request lifetime SHALL NOT own operation
// lifetime". ctx is used only for the synchronous part of this call (the
// idempotency-checked insert), which is expected to be fast regardless of
// how long the operation itself ends up taking.
func (b *BackupService) SubmitRunCycle(ctx context.Context, req RunCycleRequest) (Operation, error) {
	if req.IdempotencyKey == "" {
		return Operation{}, fmt.Errorf("service: run_cycle request requires an idempotency key")
	}
	if req.ConfigRevision == "" {
		return Operation{}, fmt.Errorf("service: run_cycle request requires a configuration revision")
	}
	if req.ConfigRevision != b.revision {
		return Operation{}, fmt.Errorf("%w: request carries %q, current is %q", ErrConfigRevisionStale, req.ConfigRevision, b.revision)
	}

	outcome, err := b.journal.CreateOperation(ctx, state.OperationRequest{
		OperationID:    "op_" + uuid.New().String(),
		IdempotencyKey: req.IdempotencyKey,
		Actor:          req.Actor,
		BackupSet:      "",
		ConfigRevision: b.revision,
		Action:         ActionRunCycle,
		Parameters:     "{}",
		CreatedAt:      now(),
	})
	if err != nil {
		return Operation{}, fmt.Errorf("service: submit run_cycle: %w", err)
	}

	if outcome.Created {
		go b.executeRunCycle(outcome.Operation.OperationID)
	}

	return toOperation(outcome.Operation), nil
}

// GetOperation returns the current state of the operation identified by
// id, translating internal/state's ErrOperationNotFound into this
// package's own sentinel so a caller outside core/ never needs to
// errors.Is against a symbol from a package it cannot import.
func (b *BackupService) GetOperation(ctx context.Context, id string) (Operation, error) {
	rec, err := b.journal.GetOperation(ctx, id)
	if err != nil {
		if errors.Is(err, state.ErrOperationNotFound) {
			return Operation{}, ErrOperationNotFound
		}
		return Operation{}, fmt.Errorf("service: get operation: %w", err)
	}
	return toOperation(rec), nil
}

// executeRunCycle is the asynchronous half of SubmitRunCycle: mark the
// operation running, actually call the wrapped internal/app.Service's
// RunCycle (this is "API invokes BackupService" happening for real, not a
// simulated response), and record the outcome. It runs on its own
// goroutine, on context.Background(), entirely independent of whatever
// HTTP request originally called SubmitRunCycle; see that function's doc
// for why.
//
// Errors marking the row running/completed/failed are deliberately not
// surfaced anywhere beyond this function: by the time they could happen,
// the caller that could have acted on them is long gone (this runs after
// SubmitRunCycle has already returned), and there is nothing left to
// retry against other than the journal itself, which internal/state's own
// durability guarantees already cover.
func (b *BackupService) executeRunCycle(operationID string) {
	ctx := context.Background()

	if err := b.journal.MarkOperationRunning(ctx, operationID, now()); err != nil {
		return
	}

	report := b.inner.RunCycle(ctx)

	var failed string
	for _, set := range report.Sets {
		if set.Err != nil {
			failed = set.Err.Error()
			break
		}
	}

	if failed != "" {
		_ = b.journal.FailOperation(ctx, operationID, now(), failed)
		return
	}

	_ = b.journal.CompleteOperation(ctx, operationID, now(), summarizeCycle(report))
}

// cycleSummary is the opaque JSON blob stored in a completed run_cycle
// operation's Result. It is deliberately narrow: a count and a duration,
// nothing that reaches back into internal/discovery, internal/reconcile or
// internal/retention's own report types, exactly as this package's own
// doc requires (nothing from core/internal leaks past this boundary).
type cycleSummary struct {
	BackupSetsProcessed int    `json:"backup_sets_processed"`
	DurationMillis      int64  `json:"duration_ms"`
	StartedAt           string `json:"started_at"`
}

func summarizeCycle(report app.CycleReport) string {
	b, err := json.Marshal(cycleSummary{
		BackupSetsProcessed: len(report.Sets),
		DurationMillis:      report.Duration.Milliseconds(),
		StartedAt:           report.StartedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		// cycleSummary is a plain struct of strings/numbers; Marshal
		// cannot actually fail against it.
		return "{}"
	}
	return string(b)
}

func toOperation(rec state.Operation) Operation {
	op := Operation{
		ID:             rec.OperationID,
		IdempotencyKey: rec.IdempotencyKey,
		Actor:          rec.Actor,
		BackupSetID:    rec.BackupSet,
		ConfigRevision: rec.ConfigRevision,
		Action:         rec.Action,
		Status:         rec.Status,
		CreatedAt:      rec.CreatedAt,
		Result:         rec.Result,
		Error:          rec.Error,
	}
	if rec.StartedAt != nil {
		op.StartedAt = *rec.StartedAt
	}
	if rec.FinishedAt != nil {
		op.FinishedAt = *rec.FinishedAt
	}
	return op
}
