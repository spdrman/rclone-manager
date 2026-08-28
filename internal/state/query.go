package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/internal/model"
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
