// The durable operations table: one row per request an API client made, so
// a client can still find out what happened to it after the process that
// was doing it died.
//
// Everything else in this package is about artifacts. This is about the
// requests that act on them, and the two are kept apart because they
// answer to different people. An artifact row is the manager's own account
// of what it did; an operation row is a receipt held by a caller who is
// going to poll for it. That is why it carries an actor and a
// configuration revision it never interprets, and why the parameters are
// opaque JSON: this package has no idea what actions exist and should not
// learn one. FailInterruptedOperations is where that restraint costs
// something, and it pays it by taking the exception list as an argument
// rather than naming actions here.
//
// Two mechanisms are worth understanding before changing anything.
// CreateOperation is idempotent on a caller-supplied key, in the same
// shape and for the same reason RecordTransition is, and it refuses a key
// presented for a logically different request rather than handing one
// caller's operation, result text included, to another.
//
// And a row only ever moves forward: queued to running to completed or
// failed. The one thing that moves a row nobody is executing any more is
// the startup sweep, because a row left at running by a process that was
// killed would otherwise sit there for ever while a client polls it.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Operation statuses. A row moves strictly forward through these: queued ->
// running -> (completed | failed). Nothing in this package ever moves a row
// backward.
const (
	OperationQueued    = "queued"
	OperationRunning   = "running"
	OperationCompleted = "completed"
	OperationFailed    = "failed"
)

// OperationRequest is everything CreateOperation needs to persist a durable
// operation row (docs/EPIC-B-multi-nas.md §14): the same snapshot fields
// the EPIC lists (actor, backup-set id, configuration revision, requested
// action, safety-relevant parameters, creation timestamp), plus the two
// identifiers this package keys on. Parameters is opaque JSON: this
// package has no opinion on what a given Action's parameters look like, it
// only stores and returns them unchanged.
type OperationRequest struct {
	OperationID    string
	IdempotencyKey string
	Actor          string
	BackupSet      string
	ConfigRevision string
	Action         string
	Parameters     string
	CreatedAt      time.Time
}

// Operation is one durable operation row, read back exactly as stored.
type Operation struct {
	OperationID    string
	IdempotencyKey string
	Actor          string
	BackupSet      string
	ConfigRevision string
	Action         string
	Parameters     string

	Status string

	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time

	Result string
	Error  string
}

// OperationOutcome reports what CreateOperation actually did, mirroring
// Outcome/Applied in journal.go: Created is true only if this call itself
// inserted the row. A caller uses this to decide whether IT is the one
// responsible for starting execution (Created == true) or whether an
// earlier call already owns that (Created == false, meaning "this is a
// retry of a request already in flight or already finished").
type OperationOutcome struct {
	Created   bool
	Operation Operation
}

// validateOperationRequest refuses a request that could not be identified
// or retried later, before a transaction is opened for it.
//
// The two identifiers are separate on purpose and both are required. The
// operation id is what a client polls; the idempotency key is what makes a
// resubmitted request resolve to the row that already exists. A request
// missing either one would insert perfectly well and then be
// unrecoverable, so it is refused here rather than left for a NOT NULL
// error to describe in the schema's words instead of the caller's.
func validateOperationRequest(req OperationRequest) error {
	switch {
	case req.OperationID == "":
		return fmt.Errorf("state: operation requires a non-empty OperationID")
	case req.IdempotencyKey == "":
		return fmt.Errorf("state: operation requires a non-empty IdempotencyKey")
	case req.Action == "":
		return fmt.Errorf("state: operation requires a non-empty Action")
	case req.CreatedAt.IsZero():
		return fmt.Errorf("state: operation requires CreatedAt")
	}
	return nil
}

// CreateOperation durably persists req as a new operation row with status
// OperationQueued, or recognises that req.IdempotencyKey was already used
// by an earlier call and returns THAT row instead, unchanged
// (OperationOutcome.Created == false). This is the acceptance criterion "a
// duplicate idempotency key does not create duplicate work" made concrete:
// a caller must only start executing an operation when Created == true.
//
// The whole operation (idempotency check, then insert) happens inside one
// SQLite transaction, exactly like RecordTransition in journal.go, and for
// the same reason: that is what makes two concurrent callers racing the
// same IdempotencyKey resolve to exactly one row rather than a
// check-then-insert race creating two.
//
// If IdempotencyKey was already used for a request whose Actor, Action or
// ConfigRevision differs from req, this returns ErrOperationIdempotencyKeyReused:
// an idempotency key is a promise about one specific logical request from
// one specific caller, and silently serving one actor's operation (which
// may include its Result/Error) back to a request presenting a different
// Actor would be an information leak across callers, not a convenience.
func (j *Journal) CreateOperation(ctx context.Context, req OperationRequest) (OperationOutcome, error) {
	if err := validateOperationRequest(req); err != nil {
		return OperationOutcome{}, err
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return OperationOutcome{}, fmt.Errorf("state: begin create operation: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	existing, err := getOperationByIdempotencyKey(ctx, tx, req.IdempotencyKey)
	if err != nil && !errors.Is(err, ErrOperationNotFound) {
		return OperationOutcome{}, err
	}
	if err == nil {
		return commitIdempotentReplay(tx, req, existing)
	}

	_, conflict, err := insertOperation(ctx, tx, req)
	if err != nil {
		return OperationOutcome{}, err
	}
	if conflict {
		// Lost a race with a writer this transaction's own idempotency
		// check above did not see yet: something else committed a row
		// under this exact idempotency_key between that SELECT and this
		// INSERT. db.SetMaxOpenConns(1) (state.go) makes that impossible
		// for two calls sharing one *sql.DB (see
		// TestCreateOperation_ConcurrentSameIdempotencyKeyCreatesExactlyOne),
		// but nothing enforces that across two separate *sql.DB handles on
		// the same underlying SQLite file — the realistic shape being two
		// processes sharing one journal. Re-fetch and treat it exactly
		// like the ordinary sequential-replay path above, rather than
		// surfacing modernc.org/sqlite's raw constraint error to a caller
		// that has no way to act on it.
		existing, err := getOperationByIdempotencyKey(ctx, tx, req.IdempotencyKey)
		if err != nil {
			return OperationOutcome{}, fmt.Errorf("state: re-fetch after idempotency key race: %w", err)
		}
		return commitIdempotentReplay(tx, req, existing)
	}

	createdRow, err := getOperationByID(ctx, tx, req.OperationID)
	if err != nil {
		return OperationOutcome{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationOutcome{}, fmt.Errorf("state: commit create operation: %w", err)
	}

	return OperationOutcome{Created: true, Operation: createdRow}, nil
}

// commitIdempotentReplay is CreateOperation's shared "this idempotency key
// was already used" path, reached whether that was discovered by this
// transaction's own idempotency-key SELECT or only after losing a
// cross-connection race on the INSERT (see CreateOperation's own doc for
// that second case). It still refuses a key reused for a logically
// different request either way: silently serving back an unrelated
// operation would mean telling a caller "your request is already in
// flight" about a request it never actually made.
func commitIdempotentReplay(tx *sql.Tx, req OperationRequest, existing Operation) (OperationOutcome, error) {
	if existing.Actor != req.Actor || existing.Action != req.Action || existing.ConfigRevision != req.ConfigRevision {
		return OperationOutcome{}, fmt.Errorf("%w: key %q", ErrOperationIdempotencyKeyReused, req.IdempotencyKey)
	}
	if err := tx.Commit(); err != nil {
		return OperationOutcome{}, fmt.Errorf("state: commit idempotent replay: %w", err)
	}
	return OperationOutcome{Created: false, Operation: existing}, nil
}

// insertOperation attempts the actual INSERT CreateOperation needs once its
// own idempotency check has found nothing. conflict is true when the
// INSERT itself failed specifically because of a UNIQUE constraint
// violation — operation_id is generated fresh per call (see
// core/service's uuid-based OperationID) so in practice this means
// idempotency_key raced a concurrent writer this transaction's own
// idempotency check above did not see; any other failure is returned as
// err, unclassified, exactly as before this function was extracted.
func insertOperation(ctx context.Context, tx *sql.Tx, req OperationRequest) (created, conflict bool, err error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO operations (
			operation_id, idempotency_key, actor, backup_set, config_revision,
			action, parameters, status, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.OperationID, req.IdempotencyKey, req.Actor, req.BackupSet, req.ConfigRevision,
		req.Action, req.Parameters, OperationQueued, formatTime(req.CreatedAt),
	); err != nil {
		if isUniqueViolation(err) {
			return false, true, nil
		}
		return false, false, fmt.Errorf("state: insert operation: %w", err)
	}
	return true, false, nil
}

// GetOperation returns the current row for operationID.
func (j *Journal) GetOperation(ctx context.Context, operationID string) (Operation, error) {
	return getOperationByID(ctx, j.db, operationID)
}

// GetOperationByIdempotencyKey returns the row an earlier call created
// under key, or ErrOperationNotFound when the key has never been used.
//
// It exists for a caller that has to know whether a request already landed
// BEFORE it does anything irreversible about it, which CreateOperation
// cannot answer because answering it is the same call as writing the row.
// internal/archive is the case that forced it: a restore costs money at
// the provider, so it has to ask "have I already been given this exact
// request" before it asks the provider anything, and yet a request it goes
// on to refuse must leave no row behind. Those two are only compatible if
// the key can be resolved by a read.
//
// It deliberately does not take the rest of the request and does not
// enforce the actor/action/configuration match CreateOperation makes on a
// replay. This is a lookup, not an admission decision, and a caller that
// short-circuits on what it finds owes that check itself; archive's
// Restorer makes it, in those words, and its own suite plants a violation
// against it. Folding the check in here would make the read unusable for
// anything else and would put the refusal two packages away from the
// caller that has to explain it.
func (j *Journal) GetOperationByIdempotencyKey(ctx context.Context, key string) (Operation, error) {
	if key == "" {
		return Operation{}, fmt.Errorf("state: looking an operation up needs a non-empty IdempotencyKey")
	}
	return getOperationByIdempotencyKey(ctx, j.db, key)
}

// FailInterruptedOperations transitions every operation still at queued or
// running to failed, recording finishedAt and reason, EXCEPT those whose
// action appears in exceptActions. This is the startup
// sweep core/service.Open calls once, before serving any request: a row
// still at queued or running when a BackupService is constructed cannot
// belong to anything this process itself has done (this journal, this
// process, has made no SubmitRunCycle call yet), so it can only be left
// over from an earlier process that was killed, or crashed, before it ever
// reached a terminal status. Nothing would otherwise ever move such a row
// out of that state; a client polling GET /api/v1/operations/{id} against
// it would see "running" forever. Returns the number of rows swept, for a
// caller that wants to log it.
//
// # Why there is an exception list at all (EPIC E, FR-34)
//
// The reasoning above rests on one assumption: the work an operation
// describes happens inside the process that submitted it, so a process
// that died really did abandon it. That is true of run_cycle and it is
// false of an archive restore, which runs at the storage provider over
// hours and carries on regardless of what happens here. Sweeping such a
// row would not be tidying up after a crash, it would be this product
// writing down a failure that did not happen, about a job somebody else
// is still doing, that they are still being billed for. So an action
// whose work is external is named by the caller and left alone, and its
// real state is re-derived by asking the provider.
//
// The exception is a caller-supplied list rather than a constant here on
// purpose: internal/state does not know what actions exist, in the same
// way it does not know what parameters mean, and it should not learn.
func (j *Journal) FailInterruptedOperations(ctx context.Context, finishedAt time.Time, reason string, exceptActions ...string) (int64, error) {
	query := `UPDATE operations SET status = ?, finished_at = ?, error = ? WHERE status IN (?, ?)`
	args := []any{OperationFailed, formatTime(finishedAt), reason, OperationQueued, OperationRunning}
	if len(exceptActions) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(exceptActions)), ", ")
		query += ` AND action NOT IN (` + placeholders + `)`
		for _, a := range exceptActions {
			args = append(args, a)
		}
	}
	res, err := j.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("state: fail interrupted operations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("state: fail interrupted operations: check rows affected: %w", err)
	}
	return n, nil
}

// MarkOperationRunning transitions operationID from queued to running,
// recording when execution actually started. It is called by the
// background executor immediately before it starts calling BackupService,
// never by the HTTP handler that created the row: the row already exists
// (queued) the instant CreateOperation returned, well before this.
func (j *Journal) MarkOperationRunning(ctx context.Context, operationID string, startedAt time.Time) error {
	res, err := j.db.ExecContext(ctx,
		`UPDATE operations SET status = ?, started_at = ? WHERE operation_id = ?`,
		OperationRunning, formatTime(startedAt), operationID,
	)
	if err != nil {
		return fmt.Errorf("state: mark operation running: %w", err)
	}
	return requireRowsAffected(res, operationID)
}

// CompleteOperation transitions operationID to completed, recording when it
// finished and an opaque, caller-supplied result summary.
func (j *Journal) CompleteOperation(ctx context.Context, operationID string, finishedAt time.Time, result string) error {
	res, err := j.db.ExecContext(ctx,
		`UPDATE operations SET status = ?, finished_at = ?, result = ? WHERE operation_id = ?`,
		OperationCompleted, formatTime(finishedAt), result, operationID,
	)
	if err != nil {
		return fmt.Errorf("state: complete operation: %w", err)
	}
	return requireRowsAffected(res, operationID)
}

// FailOperation transitions operationID to failed, recording when it
// stopped and why.
func (j *Journal) FailOperation(ctx context.Context, operationID string, finishedAt time.Time, errMsg string) error {
	res, err := j.db.ExecContext(ctx,
		`UPDATE operations SET status = ?, finished_at = ?, error = ? WHERE operation_id = ?`,
		OperationFailed, formatTime(finishedAt), errMsg, operationID,
	)
	if err != nil {
		return fmt.Errorf("state: fail operation: %w", err)
	}
	return requireRowsAffected(res, operationID)
}

// requireRowsAffected turns "the UPDATE matched zero rows" into
// ErrOperationNotFound instead of silently succeeding: every one of this
// package's UPDATE statements above is keyed on operation_id alone, so zero
// rows affected means that id simply does not exist.
func requireRowsAffected(res sql.Result, operationID string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrOperationNotFound, operationID)
	}
	return nil
}

// operationSelectColumns is spelled once because scanOperation decodes it
// by position, so a read that listed its own columns could put an actor
// into a backup set field without anything failing. Every read below goes
// through this constant and that one scanner.
const operationSelectColumns = `
	operation_id, idempotency_key, actor, backup_set, config_revision,
	action, parameters, status, created_at, started_at, finished_at,
	result, error`

// getOperationByID and getOperationByIdempotencyKey are the same read
// under the table's two unique keys, and they are kept apart rather than
// parameterised because the not-found messages have to differ. A caller
// polling an operation id and a caller replaying an idempotency key are
// asking different questions, and being told "operation not found: <a key
// you never saw>" is a worse answer than no answer.
//
// Both take a querier so CreateOperation can use them inside the
// transaction it is holding, where the row it is about to insert either
// exists or does not without a second writer changing the answer partway.
func getOperationByID(ctx context.Context, q querier, operationID string) (Operation, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+operationSelectColumns+` FROM operations WHERE operation_id = ?`, operationID,
	)
	op, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, fmt.Errorf("%w: %s", ErrOperationNotFound, operationID)
	}
	if err != nil {
		return Operation{}, fmt.Errorf("state: load operation: %w", err)
	}
	return op, nil
}

func getOperationByIdempotencyKey(ctx context.Context, q querier, key string) (Operation, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+operationSelectColumns+` FROM operations WHERE idempotency_key = ?`, key,
	)
	op, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, fmt.Errorf("%w: idempotency key %s", ErrOperationNotFound, key)
	}
	if err != nil {
		return Operation{}, fmt.Errorf("state: load operation by idempotency key: %w", err)
	}
	return op, nil
}

// scanOperation decodes one row of operationSelectColumns.
//
// sql.ErrNoRows is passed through untouched rather than wrapped, because
// its two callers turn it into ErrOperationNotFound with the identifier
// they were actually given, and wrapping it here would leave them
// unwrapping their own error to find it.
func scanOperation(row scanRow) (Operation, error) {
	var (
		op                    Operation
		createdAt             string
		startedAt, finishedAt sql.NullString
	)

	err := row.Scan(
		&op.OperationID, &op.IdempotencyKey, &op.Actor, &op.BackupSet, &op.ConfigRevision,
		&op.Action, &op.Parameters, &op.Status, &createdAt, &startedAt, &finishedAt,
		&op.Result, &op.Error,
	)
	if err != nil {
		return Operation{}, err
	}

	created, err := parseTime(createdAt)
	if err != nil {
		return Operation{}, fmt.Errorf("state: stored operation created_at %q is invalid: %w", createdAt, err)
	}
	op.CreatedAt = created

	startedPtr, err := scanOptionalTime(startedAt)
	if err != nil {
		return Operation{}, fmt.Errorf("state: stored operation started_at is invalid: %w", err)
	}
	op.StartedAt = startedPtr

	finishedPtr, err := scanOptionalTime(finishedAt)
	if err != nil {
		return Operation{}, fmt.Errorf("state: stored operation finished_at is invalid: %w", err)
	}
	op.FinishedAt = finishedPtr

	return op, nil
}

// ListOperations returns the most recent limit durable operation records,
// newest first.
//
// Ordered by created_at then rowid: created_at is what a reader means by
// "most recent", and the rowid tiebreak totally orders two operations
// created inside the same clock tick, which the timestamp alone does not.
//
// A limit of zero or less is refused rather than read as "everything": the
// operations table is append-only and never pruned, so an unbounded read
// grows with the deployment's whole history.
func (j *Journal) ListOperations(ctx context.Context, limit int) ([]Operation, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("state: list operations: limit must be positive, got %d", limit)
	}

	rows, err := j.db.QueryContext(ctx,
		`SELECT `+operationSelectColumns+` FROM operations ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("state: list operations: %w", err)
	}
	defer rows.Close()

	var out []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("state: list operations: %w", err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list operations: %w", err)
	}
	return out, nil
}
