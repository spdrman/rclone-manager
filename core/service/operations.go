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

// ErrInvalidRequest wraps every request-validation failure SubmitRunCycle
// can return (a missing idempotency key, a missing configuration
// revision). It exists so a caller outside core/ (the HTTP layer) can
// distinguish "the caller's own request was malformed" — safe to report
// back verbatim, since the message is always one of this package's own,
// deliberately generic strings — from any other, unclassified error this
// method might someday return, which must NOT be echoed back to a client
// as-is: an unclassified error could, in principle, wrap a state-layer
// failure whose text mentions SQLite internals, and this package's own
// contract (never leak an rclone/SQLite implementation type or string
// past this boundary) has to hold even for the failure paths, not just
// the success shapes.
var ErrInvalidRequest = errors.New("service: invalid request")

// ErrIdempotencyKeyConflict is returned by SubmitRunCycle when
// RunCycleRequest.IdempotencyKey was already used for a request whose
// actor, action or configuration revision differs from this one (see
// internal/state.ErrOperationIdempotencyKeyReused, which this wraps). This
// is deliberately a distinct sentinel from ErrInvalidRequest, not folded
// into it: "you reused an idempotency key across two different logical
// requests" is not the same problem as "your request body itself is
// malformed", and a client needs to be able to tell the two apart
// programmatically — that is the entire point of an idempotency key in the
// first place. apps/common/webhost maps this to its own 409 error code,
// distinct from CONFIG_REVISION_STALE and OPERATION_ALREADY_RUNNING below.
var ErrIdempotencyKeyConflict = errors.New("service: idempotency key already used for a different request")

// ErrOperationAlreadyRunning is returned by SubmitRunCycle when a brand
// new run_cycle operation cannot be started because another one is
// already executing.
//
// BackupService allows at most one executeRunCycle in flight at a time
// (see that method's own doc and internal/app/cycle.go's "no concurrent
// pass over the same backup set" invariant, which SubmitRunCycle's
// goroutine-per-operation is the first caller in this codebase's history
// to put at risk): a second, genuinely new submission that arrives while
// the first is still running is rejected outright, not silently queued
// and not silently dropped.
//
// Its already-persisted operation row is moved straight to "failed"
// before this error is returned (see SubmitRunCycle), so nothing is ever
// left at "queued" with no path to a terminal status; a client that wants
// to actually run a cycle once the first one finishes must submit again
// with a fresh IdempotencyKey. Replaying the SAME IdempotencyKey as the
// operation already in flight is a different case entirely and is
// unaffected by this: that always succeeds and returns the in-flight
// operation's own current state, exactly like any other idempotent
// replay.
var ErrOperationAlreadyRunning = errors.New("service: another run_cycle operation is already running")

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

	// Progress is the live, ephemeral reading for an operation executing
	// in THIS process right now, and nil for every other operation:
	// finished, queued, or left behind "running" by a process that died.
	//
	// Nil means "no progress is available", which is emphatically not
	// "zero progress". A caller that renders nil as a zero turns "we
	// cannot see inside this" into "nothing has happened", which for a
	// transfer that is in fact half done is simply wrong. See
	// OperationProgress (progress.go) for why this is never persisted
	// alongside the durable fields above it.
	Progress *OperationProgress
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
// The asynchronous execution below is started with b.ctx (BackupService's
// own long-lived, cancellable context; see that field's doc), deliberately
// NOT ctx and NOT context.Background(): ctx belongs to the caller (an HTTP
// handler's request context in the real webhost wiring), and
// docs/EPIC-B-multi-nas.md §14 is explicit that "HTTP request lifetime
// SHALL NOT own operation lifetime". context.Background() would decouple
// execution from the request but ALSO from process shutdown entirely,
// which §9.3 does not allow ("the HTTP server and background scheduler
// SHALL share a common application service and process shutdown
// context") — b.ctx is what lets Close actually ask a still-running
// RunCycle to wind down. ctx (the parameter) is used only for the
// synchronous part of this call (the idempotency-checked insert), which
// is expected to be fast regardless of how long the operation itself ends
// up taking.
//
// # At most one execution in flight (issue #118 item 1)
//
// internal/app/cycle.go's own doc says RunCycle guarantees "no concurrent
// pass over the same backup set" only because, until this package existed,
// the CLI called it once per process invocation and never concurrently
// with itself. A goroutine spawned per submitted operation is what first
// makes two overlapping RunCycle calls possible in this codebase, so this
// method enforces the single-flight invariant explicitly: only the
// goroutine that manages to lock b.runOnce may proceed. When a brand new
// operation (Created == true) loses that race, its already-persisted row
// is moved straight to "failed" — right here, synchronously, before this
// method returns — rather than left at "queued" with nothing ever coming
// along to finish it, and ErrOperationAlreadyRunning is returned instead
// of Operation (see that sentinel's own doc for the full rationale,
// including why this rejects rather than queues). This check only ever
// applies to a genuinely new operation: a replay of an already-in-flight
// operation's own IdempotencyKey is handled entirely by the
// CreateOperation call above it (Created == false) and always succeeds,
// since it is not asking to start a second execution at all.
func (b *BackupService) SubmitRunCycle(ctx context.Context, req RunCycleRequest) (Operation, error) {
	if req.IdempotencyKey == "" {
		return Operation{}, fmt.Errorf("%w: run_cycle request requires an idempotency key", ErrInvalidRequest)
	}
	if req.ConfigRevision == "" {
		return Operation{}, fmt.Errorf("%w: run_cycle request requires a configuration revision", ErrInvalidRequest)
	}
	// One atomic read up front: st.revision is what this whole call
	// checks against and records, so it must be the exact same value
	// throughout, not re-read (and possibly changed by a concurrent
	// CreateBackupSet) between the comparison and the journal write below
	// (see BackupService.state's own doc).
	st := b.state.Load()
	if req.ConfigRevision != st.revision {
		return Operation{}, fmt.Errorf("%w: request carries %q, current is %q", ErrConfigRevisionStale, req.ConfigRevision, st.revision)
	}

	outcome, err := b.journal.CreateOperation(ctx, state.OperationRequest{
		OperationID:    "op_" + uuid.New().String(),
		IdempotencyKey: req.IdempotencyKey,
		Actor:          req.Actor,
		BackupSet:      "",
		ConfigRevision: st.revision,
		Action:         ActionRunCycle,
		Parameters:     "{}",
		CreatedAt:      now(),
	})
	if err != nil {
		if errors.Is(err, state.ErrOperationIdempotencyKeyReused) {
			return Operation{}, fmt.Errorf("%w: idempotency key already used for a different request", ErrIdempotencyKeyConflict)
		}
		// Deliberately not %w-wrapped with err here: err may originate
		// from the state layer (a SQLite failure, a driver error string)
		// and this method's whole contract is that nothing from that
		// layer's vocabulary crosses this boundary, including through an
		// error message a caller might log or display verbatim.
		return Operation{}, fmt.Errorf("service: submit run_cycle: an internal error occurred")
	}

	if !outcome.Created {
		return toOperation(outcome.Operation), nil
	}

	if !b.runOnce.TryLock() {
		reason := "rejected: another run_cycle operation is already in progress"
		if failErr := b.journal.FailOperation(context.Background(), outcome.Operation.OperationID, now(), reason); failErr != nil {
			b.logger.Error(context.Background(), "fail-rejected-run-cycle", failErr)
		}
		return Operation{}, fmt.Errorf("%w: %s", ErrOperationAlreadyRunning, reason)
	}

	b.wg.Add(1)
	go b.executeRunCycle(outcome.Operation.OperationID)

	return toOperation(outcome.Operation), nil
}

// GetOperation returns the current state of the operation identified by
// id, translating internal/state's ErrOperationNotFound into this
// package's own sentinel so a caller outside core/ never needs to
// errors.Is against a symbol from a package it cannot import.
//
// # Authorization (issue #118 item 9)
//
// GetOperation deliberately does not check Actor against whoever created
// id: any authenticated caller may read any operation's status. That is a
// considered decision, not an oversight, for the model this skeleton
// actually ships under today — docs/EPIC-B-multi-nas.md §13.4's
// admin-only initial release, where every authenticated caller already IS
// the (one) administrator, so there is no other actor to scope reads
// away from. This stops being an obviously-safe default the day multi-user
// auth or a non-admin role lands; whoever adds that should revisit this
// doc and decide explicitly between admin-sees-all (documented here again,
// deliberately) and actor-scoped reads, rather than discovering the gap
// implicitly.
func (b *BackupService) GetOperation(ctx context.Context, id string) (Operation, error) {
	rec, err := b.journal.GetOperation(ctx, id)
	if err != nil {
		if errors.Is(err, state.ErrOperationNotFound) {
			return Operation{}, ErrOperationNotFound
		}
		return Operation{}, fmt.Errorf("service: get operation: %w", err)
	}
	return b.withLiveProgress(toOperation(rec)), nil
}

// withLiveProgress attaches the in-memory reading for op, if this process
// has one.
//
// Two conditions, both necessary. The durable status must be "running",
// which rules out serving a reading for an operation the journal already
// considers finished; and the registry must actually hold one, which rules
// out the operation that was running when a previous process died (its
// reading died with that process, and the startup sweep has moved the row
// to failed anyway). Anything else keeps Progress nil, which the client
// renders as "no reading available" rather than as zero.
func (b *BackupService) withLiveProgress(op Operation) Operation {
	if op.Status != state.OperationRunning {
		return op
	}
	if p, ok := b.progress.snapshot(op.ID); ok {
		op.Progress = &p
	}
	return op
}

// executeRunCycle is the asynchronous half of SubmitRunCycle: mark the
// operation running, actually call the wrapped internal/app.Service's
// RunCycle (this is "API invokes BackupService" happening for real, not a
// simulated response), and record the outcome. It runs on its own
// goroutine, entirely independent of whatever HTTP request originally
// called SubmitRunCycle; see that method's own doc for why RunCycle itself
// runs on b.ctx rather than context.Background() or the request's ctx.
//
// It always does exactly three things before returning, in order, no
// matter how RunCycle behaves: release b.runOnce (so the next submitted
// operation can proceed), signal b.wg (so Close knows this goroutine is
// done), and, if RunCycle panicked, recover and record the operation as
// failed instead of letting the panic escape the goroutine — an
// unrecovered panic here would crash the entire persistent API server
// hosting this BackupService, not just one CLI invocation, since this
// package is what first makes RunCycle reachable from a goroutine inside
// an always-on process. See the deferred calls below for exactly how, and
// in what order.
//
// Every error this function encounters is logged via b.logger.Error
// (a safe no-op if this BackupService was built with a nil logger) rather
// than silently discarded: by the time one of these could happen, the
// caller that could have acted on it is long gone (this runs after
// SubmitRunCycle has already returned), so a log line is the only record
// of it that will ever exist.
func (b *BackupService) executeRunCycle(operationID string) {
	defer b.wg.Done()
	// Registered here, second, so it runs LAST but one: defers unwind in
	// reverse, so this fires after the recover below has had its chance to
	// write a terminal status. Clearing the live reading before that write
	// would leave a window where the row still says "running" and there is
	// nothing to read, which is a correct answer but a needlessly confusing
	// one to hand a client that is polling every second.
	live := b.progress.begin(operationID)
	defer b.progress.end(operationID)
	defer b.runOnce.Unlock()
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error(context.Background(), "execute-run-cycle-panic", fmt.Errorf("recovered panic: %v", r))
			if err := b.journal.FailOperation(context.Background(), operationID, now(),
				"an internal error occurred while running this operation"); err != nil {
				b.logger.Error(context.Background(), "fail-operation-after-panic", err)
			}
		}
	}()

	if err := b.journal.MarkOperationRunning(context.Background(), operationID, now()); err != nil {
		b.logger.Error(context.Background(), "mark-operation-running", err)
		return
	}

	// Rewrite the registered validators before anything execs one
	// (validator.go's refreshValidatorScripts), and refuse the whole cycle
	// if that cannot be done. Nothing else on this path re-checks them:
	// resolution happened at load or create time and internal/lifecycle
	// execs the Command it was handed then, so a script replaced with one
	// that exits 0 would pass every artifact in the set and authorize
	// deleting every remote source behind it.
	if err := b.refreshValidatorScripts(); err != nil {
		b.logger.Error(context.Background(), "refresh-validator-scripts", err)
		if failErr := b.journal.FailOperation(context.Background(), operationID, now(),
			"refusing to run: "+err.Error()); failErr != nil {
			b.logger.Error(context.Background(), "fail-operation", failErr)
		}
		return
	}

	// b.ctx, not the caller's: see this method's own doc. The observer
	// rides on it because live progress is scoped to exactly this
	// operation, and a field on the shared internal/app.Service would be
	// one cycle's reading in a place a second cycle could overwrite.
	report := runCycle(b.state.Load().inner, app.WithProgressObserver(b.ctx, live))

	var failed string
	for _, set := range report.Sets {
		if set.Err != nil {
			failed = set.Err.Error()
			break
		}
	}

	if failed != "" {
		if err := b.journal.FailOperation(context.Background(), operationID, now(), failed); err != nil {
			b.logger.Error(context.Background(), "fail-operation", err)
		}
		return
	}

	if err := b.journal.CompleteOperation(context.Background(), operationID, now(), summarizeCycle(report)); err != nil {
		b.logger.Error(context.Background(), "complete-operation", err)
	}
}

// runCycle is a seam over (*app.Service).RunCycle, exactly like now (in
// service.go) is a seam over time.Now: a test can substitute a stand-in
// that panics (see TestExecuteRunCycle_RecoversFromPanicAndRecordsFailedOperation)
// or blocks on a channel the test controls (see the Close tests), without
// this package needing an interface around *app.Service for the sake of
// this one call site. Nothing overrides this in production.
var runCycle = func(inner *app.Service, ctx context.Context) app.CycleReport {
	return inner.RunCycle(ctx)
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

// DefaultOperationListLimit is how many operations ListOperations returns
// when a caller does not ask for a number, and MaxOperationListLimit is
// the most it will return however large a number is asked for. The
// operations table is append-only and never pruned, so an unbounded read
// grows with the deployment's whole history.
const (
	DefaultOperationListLimit = 100
	MaxOperationListLimit     = 1000
)

// ListOperations returns the most recent durable operation records, newest
// first, across every actor and every backup set.
//
// This is the list counterpart of GetOperation (§15.7's polling read). It
// exists because a client that has just reconnected, or that never
// submitted the operation in the first place, has no operation id to poll
// with and would otherwise have no way to learn that anything is running
// at all.
//
// A limit of zero or less means DefaultOperationListLimit; anything above
// MaxOperationListLimit is clamped to it.
func (b *BackupService) ListOperations(ctx context.Context, limit int) ([]Operation, error) {
	if limit <= 0 {
		limit = DefaultOperationListLimit
	}
	if limit > MaxOperationListLimit {
		limit = MaxOperationListLimit
	}

	records, err := b.journal.ListOperations(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("service: listing operations: %w", err)
	}

	out := make([]Operation, 0, len(records))
	for _, rec := range records {
		out = append(out, b.withLiveProgress(toOperation(rec)))
	}
	return out, nil
}
