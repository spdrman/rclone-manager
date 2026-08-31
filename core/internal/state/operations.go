package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// FailInterruptedOperations transitions every operation still at queued or
// running to failed, recording finishedAt and reason. This is the startup
// sweep core/service.Open calls once, before serving any request: a row
// still at queued or running when a BackupService is constructed cannot
// belong to anything this process itself has done (this journal, this
// process, has made no SubmitRunCycle call yet), so it can only be left
// over from an earlier process that was killed, or crashed, before it ever
// reached a terminal status. Nothing would otherwise ever move such a row
// out of that state; a client polling GET /api/v1/operations/{id} against
// it would see "running" forever. Returns the number of rows swept, for a
// caller that wants to log it.
func (j *Journal) FailInterruptedOperations(ctx context.Context, finishedAt time.Time, reason string) (int64, error) {
	res, err := j.db.ExecContext(ctx,
		`UPDATE operations SET status = ?, finished_at = ?, error = ? WHERE status IN (?, ?)`,
		OperationFailed, formatTime(finishedAt), reason, OperationQueued, OperationRunning,
	)
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

const operationSelectColumns = `
	operation_id, idempotency_key, actor, backup_set, config_revision,
	action, parameters, status, created_at, started_at, finished_at,
	result, error`

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
