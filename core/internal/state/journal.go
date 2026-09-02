package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// timeLayout is used for every timestamp column. RFC3339Nano round-trips
// exactly through time.Parse, sorts lexically in the same order it sorts
// chronologically (UTC, fixed-width where it matters), and is legible
// straight out of the sqlite3 CLI without any conversion, which matters for
// a journal FR-14 and FR-17 depend on being inspectable during an incident.
const timeLayout = time.RFC3339Nano

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

// Transition describes one durable, idempotent step in an artifact's
// lifecycle (FR-10, FR-11). The state machine that decides legal From/To
// pairs is owned elsewhere (see the FR-10 issue); this package only knows
// how to persist whatever it is told without losing it or applying it
// twice.
//
// Key is the caller's idempotency key for this exact logical attempt. Two
// calls with the same Key are the same attempt: the first one applies the
// transition, and any later one, for example because the process crashed
// after the transaction committed but before the caller observed success,
// and the caller retried, is recognised as already applied and returned as
// such (Outcome.Applied == false) without mutating anything a second time.
//
// This only works if Key is derived deterministically from the attempt
// itself, for example artifact-id + target-state + a monotonically
// increasing attempt counter the caller persists or recomputes the same way
// on retry, rather than freshly generated (a random UUID, time.Now) on every
// call. A fresh Key on retry defeats the guarantee entirely, since the
// journal would see a brand new key and apply the transition again.
type Transition struct {
	Artifact model.ArtifactID
	Key      string

	// From is the state the artifact is expected to currently be in. Empty
	// means "no journal row exists yet for this artifact; create one",
	// which is what Discover uses.
	From string
	To   string

	OccurredAt time.Time
	Detail     string

	// RemotePath is required when From == "" and ignored otherwise: the
	// remote path an artifact was discovered at does not change across
	// later transitions.
	RemotePath string

	// LocalPath, when non-nil, overwrites the recorded local path. Used at
	// TRANSFERRING (the .partial destination) and again at COMMITTED (the
	// final renamed path).
	LocalPath *string

	// Remote is the FR-16 remote identity, set at Discover.
	Remote *RemoteIdentity

	Transfer   *TransferResult
	Hashes     *HashUpdate
	Validation *ValidationUpdate
	Retry      *RetryUpdate
	Deletion   *DeletionUpdate
	Retention  *RetentionUpdate
}

// Outcome reports what RecordTransition actually did.
type Outcome struct {
	// Applied is true if this call performed the mutation. It is false if
	// Key had already been recorded by an earlier call (including one from
	// a previous, crashed process), in which case Record reflects that
	// earlier call's result and this call changed nothing.
	Applied bool
	Record  Record

	// Detail echoes back this call's own Transition.Detail: the free-text
	// note (if any) the caller attached to this exact transition, for
	// example the sentence internal/lifecycle/verify.go writes when an
	// artifact fails or is quarantined. Record itself never carries this
	// text (see this package's own doc on why state_transitions.detail is
	// kept off the artifacts row), so a caller holding only an Outcome,
	// for example internal/app writing an FR-23 log line immediately
	// after the call, would otherwise have no way to report it without a
	// second query against this table (see LastEnteredDetail, the
	// package's answer for a caller that only has an artifact id and no
	// Outcome in hand).
	//
	// On a replayed call (Applied == false) this is still t.Detail from
	// THIS call, not necessarily a re-read of whatever text the original
	// call recorded: idempotency keys are derived deterministically from
	// the attempt itself, so a caller retrying with the same Key is, by
	// construction, the same code path recomputing the same fact, and
	// echoing the replay's own value is cheaper than a second read for
	// the same answer.
	Detail string
}

// Discover records DISCOVERED and creates the artifact's journal row,
// including the FR-16 remote identity captured at discovery time. It is a
// thin, named wrapper over RecordTransition with From == "": see
// RecordTransition for the idempotency contract this inherits.
func (j *Journal) Discover(
	ctx context.Context,
	artifact model.ArtifactID,
	key string,
	remotePath string,
	remote RemoteIdentity,
	occurredAt time.Time,
) (Outcome, error) {
	return j.RecordTransition(ctx, Transition{
		Artifact:   artifact,
		Key:        key,
		From:       "",
		To:         "DISCOVERED",
		OccurredAt: occurredAt,
		RemotePath: remotePath,
		Remote:     &remote,
	})
}

// RecordTransition durably applies t, or recognises that it was already
// applied by an earlier call with the same Key and returns that result
// unchanged. See the Transition and Outcome docs for the idempotency
// contract.
//
// The whole operation, idempotency check, the artifacts row insert or
// update, and the state_transitions log entry, happens inside one SQLite
// transaction. That is what makes "exactly once" true across a crash: if
// the process dies before the transaction commits, none of it happened and
// a retry with the same Key starts clean; if it dies after the transaction
// commits, the write already happened and a retry with the same Key will
// find it via the idempotency check below and report Applied == false
// instead of repeating it. There is no state in between where the
// transition is half-recorded.
func (j *Journal) RecordTransition(ctx context.Context, t Transition) (Outcome, error) {
	if err := validateTransition(t); err != nil {
		return Outcome{}, err
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return Outcome{}, fmt.Errorf("state: begin transition: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	replay, err := findByIdempotencyKey(ctx, tx, t.Key)
	if err != nil {
		return Outcome{}, err
	}
	if replay != nil {
		rec, err := getByRowID(ctx, tx, replay.artifactRowID)
		if err != nil {
			return Outcome{}, err
		}
		if replay.toState != t.To || rec.Artifact != t.Artifact {
			return Outcome{}, fmt.Errorf("%w: key %q", ErrIdempotencyKeyReused, t.Key)
		}
		if err := tx.Commit(); err != nil {
			return Outcome{}, fmt.Errorf("state: commit idempotent replay: %w", err)
		}
		return Outcome{Applied: false, Record: rec, Detail: t.Detail}, nil
	}

	redact := j.redact.Load()

	var artifactRowID int64
	if t.From == "" {
		artifactRowID, err = insertArtifact(ctx, tx, t)
	} else {
		artifactRowID, err = updateArtifact(ctx, tx, t, redact)
	}
	if err != nil {
		return Outcome{}, err
	}

	// Issue #295's one seam for the journal: t.Detail is what a caller
	// wrote (often, per the FAILED-transition call sites throughout
	// internal/lifecycle, literally err.Error() from a transport failure
	// that originated inside rclone or Go's own net dialer), filtered here,
	// once, immediately before the durable write, rather than trusted to
	// have already been scrubbed by whichever of this package's many
	// callers built it. redact.Filter is nil-receiver-safe, so this runs
	// unconditionally: a Journal nobody ever called SetRedactor on writes
	// t.Detail exactly as given, byte for byte.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO state_transitions (artifact_id, idempotency_key, from_state, to_state, occurred_at, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		artifactRowID, t.Key, t.From, t.To, formatTime(t.OccurredAt), redact.Filter(t.Detail),
	); err != nil {
		return Outcome{}, fmt.Errorf("state: record transition: %w", err)
	}

	rec, err := getByRowID(ctx, tx, artifactRowID)
	if err != nil {
		return Outcome{}, err
	}

	if err := tx.Commit(); err != nil {
		return Outcome{}, fmt.Errorf("state: commit transition: %w", err)
	}

	return Outcome{Applied: true, Record: rec, Detail: t.Detail}, nil
}

func validateTransition(t Transition) error {
	switch {
	case t.Key == "":
		return fmt.Errorf("state: transition requires a non-empty idempotency key")
	case t.To == "":
		return fmt.Errorf("state: transition requires a target state")
	case t.Artifact.Name == "" || t.Artifact.Set.IsZero():
		return fmt.Errorf("state: transition requires a valid artifact id")
	case t.OccurredAt.IsZero():
		return fmt.Errorf("state: transition requires OccurredAt")
	case t.From == "" && t.RemotePath == "":
		return fmt.Errorf("state: the first transition for an artifact requires RemotePath")
	}
	return nil
}

type replayRow struct {
	artifactRowID int64
	toState       string
}

func findByIdempotencyKey(ctx context.Context, tx *sql.Tx, key string) (*replayRow, error) {
	var r replayRow
	err := tx.QueryRowContext(ctx,
		`SELECT artifact_id, to_state FROM state_transitions WHERE idempotency_key = ?`, key,
	).Scan(&r.artifactRowID, &r.toState)
	switch {
	case err == nil:
		return &r, nil
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	default:
		return nil, fmt.Errorf("state: check idempotency key: %w", err)
	}
}

func insertArtifact(ctx context.Context, tx *sql.Tx, t Transition) (int64, error) {
	localPath := ""
	if t.LocalPath != nil {
		localPath = *t.LocalPath
	}

	var remote RemoteIdentity
	if t.Remote != nil {
		remote = *t.Remote
	}

	occurred := formatTime(t.OccurredAt)

	res, err := tx.ExecContext(ctx,
		`INSERT INTO artifacts (
			source, backup_set, artifact_name, remote_path, local_path, state,
			discovered_at, updated_at,
			remote_size, remote_mtime, remote_hash, remote_hash_alg, remote_backend_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Artifact.Set.Source, t.Artifact.Set.Set, t.Artifact.Name,
		t.RemotePath, localPath, t.To,
		occurred, occurred,
		optionalInt64(remote.Size), optionalTimeText(remote.ModTime),
		remote.Hash, remote.HashAlg, remote.BackendID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: %s", ErrAlreadyDiscovered, t.Artifact)
		}
		return 0, fmt.Errorf("state: insert artifact: %w", err)
	}

	rowID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("state: insert artifact: %w", err)
	}

	// FR-29: an artifact row and its local placement are created together,
	// in this transaction, so there is no instant at which the journal
	// holds an artifact with nowhere recorded to look for it.
	if err := insertLocalPlacement(ctx, tx, rowID, localPath, occurred); err != nil {
		return 0, err
	}

	return rowID, nil
}

// redact filters t.Deletion.Error, issue #295's other confirmed durable
// leak route beyond state_transitions.detail: internal/lifecycle/
// remotedelete.go's persistDeleteOutcome writes a real transport failure's
// err.Error() straight into this column when a remote delete call itself
// fails, and that column, remote_delete_error, is not state_transitions.
// detail and so would otherwise sail straight past the filtering above.
// redact may be nil (no Journal.SetRedactor call was ever made); Filter
// is nil-receiver-safe.
func updateArtifact(ctx context.Context, tx *sql.Tx, t Transition, redact *obs.Redactor) (int64, error) {
	var rowID int64
	var currentState string
	err := tx.QueryRowContext(ctx,
		`SELECT id, state FROM artifacts WHERE source = ? AND backup_set = ? AND artifact_name = ?`,
		t.Artifact.Set.Source, t.Artifact.Set.Set, t.Artifact.Name,
	).Scan(&rowID, &currentState)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: %s", ErrArtifactNotFound, t.Artifact)
	case err != nil:
		return 0, fmt.Errorf("state: look up artifact: %w", err)
	}

	if currentState != t.From {
		return 0, fmt.Errorf("%w: %s is %q, not %q", ErrStateMismatch, t.Artifact, currentState, t.From)
	}

	set := []string{"state = ?", "updated_at = ?"}
	args := []any{t.To, formatTime(t.OccurredAt)}

	if t.LocalPath != nil {
		set = append(set, "local_path = ?")
		args = append(args, *t.LocalPath)
	}
	if t.Remote != nil {
		set = append(set, "remote_size = ?", "remote_mtime = ?", "remote_hash = ?", "remote_hash_alg = ?", "remote_backend_id = ?")
		args = append(args, optionalInt64(t.Remote.Size), optionalTimeText(t.Remote.ModTime), t.Remote.Hash, t.Remote.HashAlg, t.Remote.BackendID)
	}
	if t.Transfer != nil {
		set = append(set, "transfer_bytes = ?", "transfer_checksummed = ?")
		args = append(args, t.Transfer.BytesTransferred, boolToInt(t.Transfer.Checksummed))
	}
	if t.Hashes != nil {
		set = append(set, "local_hash = ?", "local_hash_alg = ?")
		args = append(args, t.Hashes.Hash, t.Hashes.Alg)
	}
	if t.Validation != nil {
		set = append(set, "validation_passed = ?", "validation_detail = ?")
		args = append(args, boolToInt(t.Validation.Passed), t.Validation.Detail)
	}
	if t.Retry != nil {
		set = append(set, "retry_count = ?", "last_error = ?", "next_retry_at = ?")
		args = append(args, t.Retry.Count, t.Retry.LastError, optionalTimeText(t.Retry.NextAttempt))
	}
	if t.Deletion != nil {
		set = append(set, "remote_deleted_at = ?", "remote_delete_error = ?")
		args = append(args, optionalTimeText(t.Deletion.DeletedAt), redact.Filter(t.Deletion.Error))
	}
	if t.Retention != nil {
		set = append(set, "retention_tier = ?", "retention_expires_at = ?")
		args = append(args, t.Retention.Tier, optionalTimeText(t.Retention.ExpiresAt))
	}

	args = append(args, rowID)
	query := "UPDATE artifacts SET " + join(set, ", ") + " WHERE id = ?"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return 0, fmt.Errorf("state: update artifact: %w", err)
	}

	// FR-29: whatever this transition just changed about where the local
	// copy is, or about what is known of it, is mirrored onto the local
	// placement in the same transaction.
	if err := updateLocalPlacement(ctx, tx, rowID, t); err != nil {
		return 0, err
	}

	return rowID, nil
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func optionalInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func optionalTimeText(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}
