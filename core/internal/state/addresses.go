package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// BackupSetAddress is where one backup set id was last configured to pull
// from and land in: the three fields that together decide which data a
// backup set is about (core/service/backupsetrepoint.go names them and
// says why those three).
//
// It is recorded when a backup set's configuration is REMOVED, and read
// when a set is created over an id that already has artifacts on record,
// so the create path can tell "the same data coming back" from "a
// different dataset taking over somebody else's history". Migration 0008
// carries the whole argument for why it is stored at all, why removal is
// the only writer, and why a row here is not a tombstone.
type BackupSetAddress struct {
	Set model.BackupSetID

	// Host is the machine the data came from, and "" for a remote type
	// that has none. Empty is a real answer, not a missing one: a
	// local-type set has no host, so a later create naming one is a move
	// like any other.
	Host string

	RemotePath string
	LocalPath  string

	// RecordedAt is when this address was recorded, which is when the
	// set's configuration was removed.
	RecordedAt time.Time
}

// RecordBackupSetAddress records where set was pointing, replacing
// whatever was recorded for that id before.
//
// Replacing rather than appending, because the only question a reader
// asks is where the id was pointing LAST. A set removed, created again
// somewhere else and removed again is recorded at the second address; the
// first one is not history anything reads, and keeping it would need a
// "latest" query to answer the same question.
func (j *Journal) RecordBackupSetAddress(ctx context.Context, addr BackupSetAddress) error {
	if addr.Set.IsZero() {
		return fmt.Errorf("state: recording a backup set address needs a backup set id")
	}
	if addr.RecordedAt.IsZero() {
		return fmt.Errorf("state: recording the address of %s needs a time", addr.Set)
	}
	_, err := j.db.ExecContext(ctx,
		`INSERT INTO backup_set_addresses (source, backup_set, host, remote_path, local_path, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (source, backup_set) DO UPDATE SET
		   host = excluded.host,
		   remote_path = excluded.remote_path,
		   local_path = excluded.local_path,
		   recorded_at = excluded.recorded_at`,
		addr.Set.Source, addr.Set.Set, addr.Host, addr.RemotePath, addr.LocalPath, formatTime(addr.RecordedAt),
	)
	if err != nil {
		return fmt.Errorf("state: recording the address of %s: %w", addr.Set, err)
	}
	return nil
}

// BackupSetAddress returns the address recorded for set, and whether
// there is one at all.
//
// The bool is the whole point of the signature. "No row" is a real and
// common answer (an id whose configuration was removed by a build older
// than migration 0008, or one that vanished from config.yaml by a hand
// edit rather than through the removal path), and it means "nothing is
// recorded", which a caller has to be able to tell apart from an address
// whose fields happen to be empty. Collapsing the two would let a create
// over an unrecorded id read as a create at an address of "" and be
// refused for a move that nobody can show ever happened.
func (j *Journal) BackupSetAddress(ctx context.Context, set model.BackupSetID) (BackupSetAddress, bool, error) {
	if set.IsZero() {
		return BackupSetAddress{}, false, fmt.Errorf("state: reading a backup set address needs a backup set id")
	}
	var host, remotePath, localPath, recorded string
	err := j.db.QueryRowContext(ctx,
		`SELECT host, remote_path, local_path, recorded_at
		   FROM backup_set_addresses
		  WHERE source = ? AND backup_set = ?`,
		set.Source, set.Set,
	).Scan(&host, &remotePath, &localPath, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupSetAddress{}, false, nil
	}
	if err != nil {
		return BackupSetAddress{}, false, fmt.Errorf("state: reading the recorded address of %s: %w", set, err)
	}
	at, err := parseTime(recorded)
	if err != nil {
		return BackupSetAddress{}, false, fmt.Errorf("state: the recorded address of %s has an unparseable recorded_at %q: %w", set, recorded, err)
	}
	return BackupSetAddress{
		Set:        set,
		Host:       host,
		RemotePath: remotePath,
		LocalPath:  localPath,
		RecordedAt: at,
	}, true, nil
}
