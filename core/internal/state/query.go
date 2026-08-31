package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// querier is satisfied by both *sql.DB and *sql.Tx, so the read helpers
// below work identically whether they run standalone or inside the
// transaction RecordTransition already holds.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

const selectColumns = `
	id, source, backup_set, artifact_name, remote_path, local_path, state,
	discovered_at, updated_at,
	remote_size, remote_mtime, remote_hash, remote_hash_alg, remote_backend_id,
	transfer_bytes, transfer_checksummed,
	local_hash, local_hash_alg, validation_passed, validation_detail,
	retry_count, last_error, next_retry_at,
	remote_deleted_at, remote_delete_error,
	retention_tier, retention_expires_at`

// scanRow is satisfied by *sql.Row and *sql.Rows.
type scanRow interface {
	Scan(dest ...any) error
}

// scanRecord decodes one row selected with selectColumns.
func scanRecord(row scanRow) (Record, int64, error) {
	var (
		rowID                                      int64
		source, backupSet, artifactName            string
		remotePath, localPath, stateCol            string
		discoveredAt, updatedAt                    string
		remoteSize                                 sql.NullInt64
		remoteMtime                                sql.NullString
		remoteHash, remoteHashAlg, remoteBackendID string
		transferBytes                              sql.NullInt64
		transferChecksummed                        int64
		localHash, localHashAlg                    string
		validationPassed                           sql.NullInt64
		validationDetail                           string
		retryCount                                 int64
		lastError                                  string
		nextRetryAt                                sql.NullString
		remoteDeletedAt                            sql.NullString
		remoteDeleteError                          string
		retentionTier                              string
		retentionExpiresAt                         sql.NullString
	)

	err := row.Scan(
		&rowID, &source, &backupSet, &artifactName, &remotePath, &localPath, &stateCol,
		&discoveredAt, &updatedAt,
		&remoteSize, &remoteMtime, &remoteHash, &remoteHashAlg, &remoteBackendID,
		&transferBytes, &transferChecksummed,
		&localHash, &localHashAlg, &validationPassed, &validationDetail,
		&retryCount, &lastError, &nextRetryAt,
		&remoteDeletedAt, &remoteDeleteError,
		&retentionTier, &retentionExpiresAt,
	)
	if err != nil {
		return Record{}, 0, err
	}

	set, err := model.NewBackupSetID(source, backupSet)
	if err != nil {
		return Record{}, 0, fmt.Errorf("state: stored backup set id %q/%q is invalid: %w", source, backupSet, err)
	}
	artifact, err := model.NewArtifactID(set, artifactName)
	if err != nil {
		return Record{}, 0, fmt.Errorf("state: stored artifact id %q is invalid: %w", artifactName, err)
	}

	discovered, err := parseTime(discoveredAt)
	if err != nil {
		return Record{}, 0, fmt.Errorf("state: stored discovered_at %q is invalid: %w", discoveredAt, err)
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return Record{}, 0, fmt.Errorf("state: stored updated_at %q is invalid: %w", updatedAt, err)
	}

	remoteMtimePtr, err := scanOptionalTime(remoteMtime)
	if err != nil {
		return Record{}, 0, fmt.Errorf("state: stored remote_mtime is invalid: %w", err)
	}
	nextRetryPtr, err := scanOptionalTime(nextRetryAt)
	if err != nil {
		return Record{}, 0, fmt.Errorf("state: stored next_retry_at is invalid: %w", err)
	}
	remoteDeletedPtr, err := scanOptionalTime(remoteDeletedAt)
	if err != nil {
		return Record{}, 0, fmt.Errorf("state: stored remote_deleted_at is invalid: %w", err)
	}
	retentionExpiresPtr, err := scanOptionalTime(retentionExpiresAt)
	if err != nil {
		return Record{}, 0, fmt.Errorf("state: stored retention_expires_at is invalid: %w", err)
	}

	rec := Record{
		Artifact:     artifact,
		RemotePath:   remotePath,
		LocalPath:    localPath,
		State:        stateCol,
		DiscoveredAt: discovered,
		UpdatedAt:    updated,
		Remote: RemoteIdentity{
			Size:      scanOptionalInt64(remoteSize),
			ModTime:   remoteMtimePtr,
			Hash:      remoteHash,
			HashAlg:   remoteHashAlg,
			BackendID: remoteBackendID,
		},
		LocalHash:          localHash,
		LocalHashAlg:       localHashAlg,
		ValidationPassed:   scanOptionalBool(validationPassed),
		ValidationDetail:   validationDetail,
		RetryCount:         int(retryCount),
		LastError:          lastError,
		NextRetryAt:        nextRetryPtr,
		RemoteDeletedAt:    remoteDeletedPtr,
		RemoteDeleteError:  remoteDeleteError,
		RetentionTier:      retentionTier,
		RetentionExpiresAt: retentionExpiresPtr,
	}
	if transferBytes.Valid {
		rec.Transfer = &TransferResult{
			BytesTransferred: transferBytes.Int64,
			Checksummed:      transferChecksummed != 0,
		}
	}

	return rec, rowID, nil
}

func scanOptionalInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func scanOptionalBool(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

func scanOptionalTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid {
		return nil, nil
	}
	t, err := parseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func getByRowID(ctx context.Context, q querier, rowID int64) (Record, error) {
	row := q.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM artifacts WHERE id = ?`, rowID)
	rec, _, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("%w: row id %d", ErrArtifactNotFound, rowID)
	}
	if err != nil {
		return Record{}, fmt.Errorf("state: load artifact: %w", err)
	}
	return rec, nil
}

// Get returns the current journal row for artifact.
func (j *Journal) Get(ctx context.Context, artifact model.ArtifactID) (Record, error) {
	row := j.db.QueryRowContext(ctx,
		`SELECT `+selectColumns+` FROM artifacts WHERE source = ? AND backup_set = ? AND artifact_name = ?`,
		artifact.Set.Source, artifact.Set.Set, artifact.Name,
	)
	rec, _, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, artifact)
	}
	if err != nil {
		return Record{}, fmt.Errorf("state: load artifact: %w", err)
	}
	return rec, nil
}

// LastEnteredAt reports when artifact most recently ENTERED state st, and
// whether it ever did.
//
// "Entered" is the load-bearing word: it reads the append-only
// state_transitions log for the newest row whose to_state is st and whose
// from_state is something else, so a same-state st -> st write (an
// internal/revalidate re-check pass, or a refusal recording itself against
// an artifact that is already there) does not count as a fresh entry and
// does not move the answer. The artifacts row's own UpdatedAt cannot make
// that distinction: it is stamped by every transition write there is, which
// is exactly right for "when was this row last touched" and exactly wrong
// for "when did this artifact last become good".
//
// This exists for internal/lifecycle's WP3.2 stable-completion delete gate,
// which needs to measure a safety delay from the moment an artifact reached
// COMMITTED and must not have that clock restarted by a routine re-check or
// by its own retry. Reading it out of the transition log rather than adding
// a column keeps the fact where it already is: the log is append-only and
// idempotency-keyed, so a replayed transition reuses its original
// occurred_at instead of writing a second row (see RecordTransition).
//
// ok is false, with a nil error, when the artifact exists but has never
// entered st. Callers deciding anything destructive on the answer should
// treat that as "no evidence", never as "long ago".
func (j *Journal) LastEnteredAt(ctx context.Context, artifact model.ArtifactID, st string) (time.Time, bool, error) {
	var occurred string
	err := j.db.QueryRowContext(ctx,
		`SELECT t.occurred_at
		   FROM state_transitions t
		   JOIN artifacts a ON a.id = t.artifact_id
		  WHERE a.source = ? AND a.backup_set = ? AND a.artifact_name = ?
		    AND t.to_state = ? AND t.from_state <> ?
		  ORDER BY t.id DESC
		  LIMIT 1`,
		artifact.Set.Source, artifact.Set.Set, artifact.Name, st, st,
	).Scan(&occurred)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("state: last entered %s for %s: %w", st, artifact, err)
	}
	at, err := parseTime(occurred)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("state: last entered %s for %s: parsing occurred_at %q: %w", st, artifact, occurred, err)
	}
	return at, true, nil
}

// LastTransition reports when artifact most recently recorded the exact
// from -> to edge in the append-only transition log, and whether it ever
// did.
//
// LastEnteredAt above answers "when did this artifact last become X".
// This answers the strictly narrower "did it become X by coming from Y",
// and the difference is load-bearing for issue #220's audit requirement:
// an artifact re-trusted after quarantine and one that was never
// distrusted are both simply COMMITTED on the artifacts row, and the only
// place that still distinguishes them is this table. A column would not
// do: the artifacts row is overwritten by every later write, while
// state_transitions is append-only and idempotency-keyed, so a replayed
// transition reuses its original row rather than adding a second one (see
// RecordTransition).
//
// It is what internal/lifecycle's FR-15 delete gate reads to refuse a
// remote delete for an artifact that was reinstated out of quarantine, so
// "no evidence" has to be unmistakably distinguishable from "an answer":
// ok is false, with a nil error, when the edge was never recorded, and a
// caller deciding anything destructive must treat that as no evidence
// rather than as a zero time.
func (j *Journal) LastTransition(ctx context.Context, artifact model.ArtifactID, from, to string) (time.Time, bool, error) {
	var occurred string
	err := j.db.QueryRowContext(ctx,
		`SELECT t.occurred_at
		   FROM state_transitions t
		   JOIN artifacts a ON a.id = t.artifact_id
		  WHERE a.source = ? AND a.backup_set = ? AND a.artifact_name = ?
		    AND t.from_state = ? AND t.to_state = ?
		  ORDER BY t.id DESC
		  LIMIT 1`,
		artifact.Set.Source, artifact.Set.Set, artifact.Name, from, to,
	).Scan(&occurred)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("state: last %s -> %s for %s: %w", from, to, artifact, err)
	}
	at, err := parseTime(occurred)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("state: last %s -> %s for %s: parsing occurred_at %q: %w", from, to, artifact, occurred, err)
	}
	return at, true, nil
}

// ListByState returns every artifact currently recorded in the given state.
// Reconciliation (FR-17) and retry scheduling are the expected callers.
func (j *Journal) ListByState(ctx context.Context, state string) ([]Record, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT `+selectColumns+` FROM artifacts WHERE state = ? ORDER BY id`, state)
	if err != nil {
		return nil, fmt.Errorf("state: list by state: %w", err)
	}
	return scanRecords(rows)
}

// ListByBackupSet returns every artifact recorded for one backup set (FR-7):
// retention and health calculations must never cross this boundary.
func (j *Journal) ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]Record, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT `+selectColumns+` FROM artifacts WHERE source = ? AND backup_set = ? ORDER BY id`,
		set.Source, set.Set,
	)
	if err != nil {
		return nil, fmt.Errorf("state: list by backup set: %w", err)
	}
	return scanRecords(rows)
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	defer rows.Close()

	var out []Record
	for rows.Next() {
		rec, _, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("state: scan artifact: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list artifacts: %w", err)
	}
	return out, nil
}

// ActivityRecord is one row of the append-only state_transitions log,
// joined back to the artifact it belongs to.
//
// This is the durable record of what actually happened, which is a
// different thing from an artifact's CURRENT state: an artifact that went
// DISCOVERED -> TRANSFERRING -> ... -> COMPLETE reads as one row in
// artifacts and six here. FR-23's event catalog (internal/obs) writes the
// same moments to the process log, but a log line is not queryable after
// the fact and is not what an operator's "recent activity" list can be
// built from; this table already is, and RecentActivity below is the read.
type ActivityRecord struct {
	Artifact   model.ArtifactID
	From       string
	To         string
	OccurredAt time.Time
	Detail     string
}

// RecentActivity returns the most recent limit state transitions across
// every backup set, newest first.
//
// Ordered by the table's own primary key rather than by occurred_at: id is
// monotonic in insertion order and unique, so it totally orders two
// transitions recorded within the same clock tick, which occurred_at (an
// RFC3339 string with whatever resolution the writer had) does not. A
// caller rendering a feed needs a stable order more than it needs one
// derived from the timestamps it is also displaying.
//
// A limit of zero or less is refused rather than silently treated as
// "everything": an unbounded read of an append-only table grows without
// end, and the caller that meant "all of it" should say a number.
func (j *Journal) RecentActivity(ctx context.Context, limit int) ([]ActivityRecord, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("state: recent activity: limit must be positive, got %d", limit)
	}

	rows, err := j.db.QueryContext(ctx,
		`SELECT a.source, a.backup_set, a.artifact_name,
		        t.from_state, t.to_state, t.occurred_at, t.detail
		   FROM state_transitions t
		   JOIN artifacts a ON a.id = t.artifact_id
		  ORDER BY t.id DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("state: recent activity: %w", err)
	}
	defer rows.Close()

	var out []ActivityRecord
	for rows.Next() {
		var (
			source, backupSet, name string
			rec                     ActivityRecord
			occurred                string
		)
		if err := rows.Scan(&source, &backupSet, &name, &rec.From, &rec.To, &occurred, &rec.Detail); err != nil {
			return nil, fmt.Errorf("state: recent activity: %w", err)
		}
		set, err := model.NewBackupSetID(source, backupSet)
		if err != nil {
			return nil, fmt.Errorf("state: recent activity: stored backup set %q/%q is invalid: %w", source, backupSet, err)
		}
		artifact, err := model.NewArtifactID(set, name)
		if err != nil {
			return nil, fmt.Errorf("state: recent activity: stored artifact %q is invalid: %w", name, err)
		}
		rec.Artifact = artifact
		if rec.OccurredAt, err = parseTime(occurred); err != nil {
			return nil, fmt.Errorf("state: recent activity: stored occurred_at %q is invalid: %w", occurred, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: recent activity: %w", err)
	}
	return out, nil
}
