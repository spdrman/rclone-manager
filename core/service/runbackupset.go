package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/spdrman/rclone-manager/core/apicontract"
	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Running exactly one backup set on demand (issue #597, EPIC G's G1.4).
//
// # Why this is an action and not a route
//
// It joins run_cycle and restore_placement on POST /api/v1/operations
// rather than getting a route of its own, for the reason restore gives at
// submitRestore: /operations is where durable, idempotency-keyed,
// configuration-revision-checked long work is started, and a second route
// would give this deployment two answers to how a long job begins. The
// durable row has always had a backup set id column that run_cycle
// correctly leaves empty; this is the action that fills it in.
//
// # The engine half already existed
//
// internal/app.Service.Fetch has been "an operator-triggered, on-demand
// run of exactly one backup set's share of the same cycle RunCycle
// performs" since FR-1, and `backup-manager fetch --backup-set` has
// called it all along. Nothing about the pipeline is new here: this is
// the missing way to reach it from a serving process, so the work takes
// the engine's single-flight lock, lands in the engine's journal and
// shows up in the engine's feeds, instead of running in a second process
// against the same journal.
//
// # It is in the same gate tier as run_cycle, and that is structural
//
// requireDestructiveGate is route middleware that runs before the body is
// decoded, so the route cannot see which action was asked for. Splitting
// the two into different tiers would need a generator change and a
// per-action gate, which is a different piece of work; both actions can
// end in FR-15 deleting a remote source, so one tier is also the right
// answer rather than merely the cheap one.
//
// # A disabled set still runs, and that is deliberate
//
// RunCycle skips a disabled set because a sweep over everything must not
// act on one an operator switched off. Naming that one set explicitly is
// the opposite intent, and `backup-manager fetch --backup-set` has always
// honoured it, so refusing here would make the two surfaces disagree
// about the same request. The browser does not offer the control for a
// disabled set; an operator who means it can still say so.
//
// An edit hold is different and IS refused below: that is two writers on
// one definition, which is the race #350 exists to prevent, and no amount
// of operator intent makes it safe.

// ActionRunBackupSet is POST /api/v1/operations' second run action: one
// internal/app.Service.Fetch pass over exactly the backup set the request
// names, rather than RunCycle's pass over every enabled one.
const ActionRunBackupSet = apicontract.ActionRunBackupSet

// ErrBackupSetHeldForEditing is returned by SubmitRunBackupSet when the
// named set is currently held for editing (edithold.go).
//
// Its own sentinel rather than ErrOperationAlreadyRunning, because the
// two send an operator to different places: one means "wait for the run
// in flight", the other means "leave edit mode, here or in the other tab
// you left open". Both are 409, because both are states a page that has
// moved on produces and both clear on their own.
var ErrBackupSetHeldForEditing = errors.New("service: this backup set is held for editing")

// RunBackupSetRequest is SubmitRunBackupSet's input. It is
// RunCycleRequest plus the one field that makes it per-set, spelled as
// the same "source/backup-set" id every surface in this product prints.
type RunBackupSetRequest struct {
	IdempotencyKey string
	Actor          string
	ConfigRevision string
	BackupSetID    string
}

// runBackupSetParameters is what the durable row records about this
// request, beside the backup set id the row carries in its own column.
//
// Recorded even though it duplicates that column, because the column is
// this deployment's addressing and the parameters are the request as it
// arrived. A row read back by a later build needs the second to explain
// the first if the two ever disagree.
type runBackupSetParameters struct {
	BackupSetID string `json:"backup_set_id"`
}

// SubmitRunBackupSet persists and starts a per-set run, with exactly the
// durability contract SubmitRunCycle has: the row exists before anything
// runs, the answer outlives the request, and execution rides on this
// service's own lifetime rather than the caller's.
//
// It shares b.runOnce with SubmitRunCycle and with the scheduler's ticks,
// which is what makes "a per-set run and a deployment-wide run cannot
// overlap" true without a second mechanism. The loser is refused with
// ErrOperationAlreadyRunning rather than queued, which is the existing
// decision at SubmitRunCycle and the right one: backing up whatever is on
// disk now is not made more correct by having been asked for twice.
func (b *BackupService) SubmitRunBackupSet(ctx context.Context, req RunBackupSetRequest) (Operation, error) {
	if req.IdempotencyKey == "" {
		return Operation{}, fmt.Errorf("%w: %s request requires an idempotency key", ErrInvalidRequest, ActionRunBackupSet)
	}
	if req.ConfigRevision == "" {
		return Operation{}, fmt.Errorf("%w: %s request requires a configuration revision", ErrInvalidRequest, ActionRunBackupSet)
	}
	if req.BackupSetID == "" {
		return Operation{}, fmt.Errorf("%w: %s request requires a backup set id", ErrInvalidRequest, ActionRunBackupSet)
	}
	sourceName, setName, ok := splitBackupSetID(req.BackupSetID)
	if !ok {
		// A syntactically impossible id cannot name anything, and gets
		// the same sentinel a well-formed unknown one gets, so a caller
		// never has to tell the two apart (unconfiguredSetID's rule).
		return Operation{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, req.BackupSetID)
	}

	// One atomic read, for the reason SubmitRunCycle takes one: the
	// revision this call checks against, records, and resolves the set
	// against all have to be the same picture of the configuration.
	st := b.state.Load()
	if req.ConfigRevision != st.revision {
		return Operation{}, fmt.Errorf("%w: request carries %q, current is %q", ErrConfigRevisionStale, req.ConfigRevision, st.revision)
	}
	if !configuresBackupSet(st.inner, sourceName, setName) {
		return Operation{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, req.BackupSetID)
	}
	if b.holds != nil && b.holds.Held(req.BackupSetID) {
		return Operation{}, fmt.Errorf("%w: %s", ErrBackupSetHeldForEditing, req.BackupSetID)
	}

	parameters, err := json.Marshal(runBackupSetParameters{BackupSetID: req.BackupSetID})
	if err != nil {
		// A struct of one string; Marshal cannot fail against it.
		parameters = []byte("{}")
	}

	outcome, err := b.journal.CreateOperation(ctx, state.OperationRequest{
		OperationID:    "op_" + uuid.New().String(),
		IdempotencyKey: req.IdempotencyKey,
		Actor:          req.Actor,
		BackupSet:      req.BackupSetID,
		ConfigRevision: st.revision,
		Action:         ActionRunBackupSet,
		Parameters:     string(parameters),
		CreatedAt:      now(),
	})
	if err != nil {
		if errors.Is(err, state.ErrOperationIdempotencyKeyReused) {
			return Operation{}, fmt.Errorf("%w: idempotency key already used for a different request", ErrIdempotencyKeyConflict)
		}
		// Deliberately not %w-wrapped: err may carry a state-layer
		// sentence naming SQLite internals, and nothing from that
		// vocabulary crosses this boundary (SubmitRunCycle's rule).
		return Operation{}, fmt.Errorf("service: submit %s: an internal error occurred", ActionRunBackupSet)
	}

	if !outcome.Created {
		return toOperation(outcome.Operation), nil
	}

	if !b.runOnce.TryLock() {
		reason := "rejected: another run is already in progress"
		if failErr := b.journal.FailOperation(context.Background(), outcome.Operation.OperationID, now(), reason); failErr != nil {
			b.logger.Error(context.Background(), "fail-rejected-run-backup-set", failErr)
		}
		return Operation{}, fmt.Errorf("%w: %s", ErrOperationAlreadyRunning, reason)
	}

	b.wg.Add(1)
	go b.executeRunBackupSet(outcome.Operation.OperationID, sourceName, setName)

	return toOperation(outcome.Operation), nil
}

// configuresBackupSet reports whether the running configuration holds a
// backup set with this source/name pair.
//
// It reads the snapshot the caller already Load()ed rather than re-reading
// config.yaml, because a run acts on the configuration this process is
// serving and the revision check above is what pins which one that is. An
// on-disk re-read here would let a hand edit made since the last reload
// decide whether a run is accepted, against a configuration the run would
// then not use.
func configuresBackupSet(inner *app.Service, sourceName, setName string) bool {
	if inner == nil || inner.Config == nil {
		return false
	}
	for _, src := range inner.Config.Sources {
		if src.Name != sourceName {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == setName {
				return true
			}
		}
	}
	return false
}

// configuresBackupSetID is configuresBackupSet against an id spelled the
// way the wire spells one, source/set.
//
// It is what the live feed asks before opening a bucket (liveactivity.go),
// so it reads the same atomic snapshot every other reader does and moves
// with a hot reload rather than with whatever configuration this process
// started on. A syntactically impossible id names nothing, exactly as it
// does for a run.
func (b *BackupService) configuresBackupSetID(id string) bool {
	if b == nil {
		return false
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return false
	}
	st := b.state.Load()
	if st == nil {
		return false
	}
	return configuresBackupSet(st.inner, sourceName, setName)
}

// executeRunBackupSet is the asynchronous half, and it is deliberately
// shaped like executeRunCycle rather than merely similar to it: the same
// deferred guarantees registered in the same order, so they unwind in the
// same one (a recovered panic writes a terminal status, then the cycle
// watch stops answering, then the single-flight lock is released, then
// the live reading is cleared, then the wait group is signalled), the
// same validator refresh before anything execs one, and the same two
// feeds bracketed around the work.
//
// The cycle watch matters more here than it looks. It is what
// BackupSetEditState reads to answer "would entering edit mode interrupt
// something", and it matches on the set id the readings carry. Leaving it
// out would mean an operator opening the edit form during a per-set run
// of that very set got no prompt at all, which is the two-writers race
// #350 exists to warn about, arriving through the one door that did not
// have the warning on it.
func (b *BackupService) executeRunBackupSet(operationID, sourceName, setName string) {
	defer b.wg.Done()
	// The live reading, registered and torn down exactly as
	// executeRunCycle does, so GET /api/v1/operations/{id} can answer
	// "how far has this got" for a per-set run too. Fetch publishes into
	// it through the observer installed below (core/internal/app's
	// progress.go), which is why this is a real registration and not a
	// placeholder that would render as a row of zeroes.
	live := b.progress.begin(operationID)
	defer b.progress.end(operationID)
	defer b.runOnce.Unlock()
	// Registered AFTER runOnce.Unlock so it runs BEFORE it, exactly as
	// executeRunCycle does and for the same reason: releasing the
	// single-flight lock first would let a scheduled tick take it, begin
	// its own watch and publish a reading, only for this deferred end()
	// to wipe it.
	b.cycleWatch.begin()
	defer b.cycleWatch.end()
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error(context.Background(), "execute-run-backup-set-panic", fmt.Errorf("recovered panic: %v", r))
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

	// The same refusal executeRunCycle makes, for the same reason: a
	// validator script replaced with one that exits 0 would pass every
	// artifact in the set and authorize deleting every remote source
	// behind it, and nothing else on this path re-checks them.
	if err := b.refreshValidatorScripts(); err != nil {
		b.logger.Error(context.Background(), "refresh-validator-scripts", err)
		if failErr := b.journal.FailOperation(context.Background(), operationID, now(),
			"refusing to run: "+err.Error()); failErr != nil {
			b.logger.Error(context.Background(), "fail-operation", failErr)
		}
		return
	}

	b.activity.beginCycle()
	defer b.activity.endCycle()

	startedAt := now()

	// b.ctx, not context.Background(): a per-set run belongs to this
	// service's lifetime for the same reason a cycle does, so Close can
	// ask it to stop. The observer rides on the context, which is what
	// puts this run's readings in front of the operation poll and in the
	// live activity feed under this set's own id.
	result, err := runBackupSetFetch(
		b.state.Load().inner,
		app.WithProgressObserver(b.ctx, progressFanout{live, b.cycleWatch, b.activity}),
		sourceName, setName)
	if err != nil {
		// Not err.Error(): Fetch's errors come up from internal/app and
		// can name a transport or a journal failure verbatim. The row's
		// error field is read back by clients, so it says what happened
		// in this package's own vocabulary and the detail goes to the log.
		b.logger.Error(context.Background(), "run-backup-set", err)
		if failErr := b.journal.FailOperation(context.Background(), operationID, now(),
			"this backup set's run did not finish"); failErr != nil {
			b.logger.Error(context.Background(), "fail-operation", failErr)
		}
		return
	}

	if err := b.journal.CompleteOperation(context.Background(), operationID, now(), summarizeFetch(result, startedAt)); err != nil {
		b.logger.Error(context.Background(), "complete-operation", err)
	}
}

// runBackupSetFetch is a seam over (*app.Service).Fetch, exactly like
// runCycle is one over RunCycle and for the same reason: a test can
// substitute a stand-in that panics or blocks without this package
// needing an interface around *app.Service for one call site. Nothing
// overrides it in production.
var runBackupSetFetch = func(inner *app.Service, ctx context.Context, sourceName, setName string) (app.FetchResult, error) {
	return inner.Fetch(ctx, sourceName, setName, false)
}

// summarizeFetch records what a per-set run got done, in the SAME shape
// summarizeCycle records for a deployment-wide one.
//
// One shape rather than two, so every reader above this layer (the
// operation row's cycle block, the dashboard's last-cycle panel, the CLI)
// keeps working without learning a second summary format. The count of
// backup sets processed is 1 by construction, which is the honest answer
// and also the one that makes a per-set row legible beside a cycle's.
//
// There are no move counts. FR-30's move pass is a deployment-wide bound
// that RunCycle runs once after every set's pass; Fetch does not run it,
// so reporting zeroes here would say "every move was refused" about a run
// that never attempted one. Absent is a different answer from zero, and
// parseCycleSummary already reads these as pointers for exactly that.
func summarizeFetch(result app.FetchResult, startedAt time.Time) string {
	progress := result.Verdict().Progress
	b, err := json.Marshal(fetchSummary{
		BackupSetsProcessed: 1,
		ArtifactsWalked:     progress.Walked,
		ArtifactsThrough:    progress.Durable,
		DurationMillis:      now().Sub(startedAt).Milliseconds(),
		StartedAt:           startedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return "{}"
	}
	return string(b)
}

// fetchSummary is cycleSummary (operations.go) with the two move counts
// left out rather than written as zeroes.
//
// It is a second struct rather than omitempty on the first, because
// omitempty on those two would also drop them from a real cycle that
// genuinely attempted no moves, which is a different fact and one
// parseCycleSummary is built to report. Here they are absent because the
// pass this summary describes has no move phase at all.
type fetchSummary struct {
	BackupSetsProcessed int    `json:"backup_sets_processed"`
	ArtifactsWalked     int    `json:"artifacts_walked"`
	ArtifactsThrough    int    `json:"artifacts_through"`
	DurationMillis      int64  `json:"duration_ms"`
	StartedAt           string `json:"started_at"`
}
