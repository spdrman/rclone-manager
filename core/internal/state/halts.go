package state

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// Halt reasons: why the manager could not connect to a backup set at all.
// These are the values the schema's CHECK constraint declares (migration
// 0004, widened by 0005 to admit HaltKeyPermissions), and the schema is
// what makes them a closed set: a reason no reader recognises is refused
// at the write rather than served to an operator later.
//
// They are deliberately not a copy of internal/transport's Category
// vocabulary. Category classifies any transport failure, most of which say
// nothing about whether a connection was ever established; these three
// name the manager's own operator-facing conclusion, which is that the
// connection was refused and therefore that nothing was backed up. The
// translation from one to the other lives in internal/app, where a cycle's
// error and its classification already meet (see halt.go there).
const (
	// HaltHostKeyChanged is FR-6's refusal: the host's SSH key no longer
	// matches its known_hosts entry, so the connection was refused. §77
	// invariant 5 makes re-trusting it an explicit administrator action,
	// which is why this clears only on a later cycle that connected.
	HaltHostKeyChanged = "HOST_KEY_CHANGED"

	// HaltAuthenticationFailed is the credential half of the same thing:
	// the host answered and rejected the login, so again no session was
	// established and no backup ran.
	HaltAuthenticationFailed = "AUTHENTICATION_FAILED"

	// HaltKeyPermissions is issue #293's refusal: the manager never even
	// tried to reach the host, because the configured key_file's on-disk
	// mode no longer matches what it was written with (checked in
	// internal/transport/rclone/ssh.go, before the connection is built).
	// It is worth telling apart from HaltAuthenticationFailed precisely
	// because they call for different fixes: a rejected login is a
	// question for the remote account, a permission drift is a question
	// for this filesystem, and before this reason existed both looked
	// identical from the Web UI, "Backup Manager could not log in".
	HaltKeyPermissions = "KEY_PERMISSIONS"
)

// BackupSetHalt is one backup set's standing connection refusal: which
// set, why, and when it was last observed.
//
// Its presence is the whole claim. There is no "halted: false" row and no
// boolean anywhere in this package, because a set with no row here is a
// set nothing is known about, which is a different statement from "this
// set is reachable" and must not be collapsed into it (issue #231).
type BackupSetHalt struct {
	Set    model.BackupSetID
	Reason string

	// ObservedAt is when this refusal was last seen, not when it started.
	// See migration 0004 for why the difference is worth keeping straight.
	ObservedAt time.Time
}

// RecordBackupSetHalt records that the manager could not connect to set,
// replacing whatever refusal was recorded for it before.
//
// One backup set has at most one standing refusal: a set refused for a
// changed host key and later refused for a rejected login is refused for
// the newer reason, not both. An undeclared reason is refused by the
// schema's own CHECK constraint rather than by a second copy of the
// vocabulary here.
func (j *Journal) RecordBackupSetHalt(ctx context.Context, set model.BackupSetID, reason string, observedAt time.Time) error {
	if set.IsZero() {
		return fmt.Errorf("state: recording a halt needs a backup set id")
	}
	_, err := j.db.ExecContext(ctx,
		`INSERT INTO backup_set_halts (source, backup_set, reason, observed_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (source, backup_set) DO UPDATE SET
		   reason = excluded.reason,
		   observed_at = excluded.observed_at`,
		set.Source, set.Set, reason, formatTime(observedAt),
	)
	if err != nil {
		return fmt.Errorf("state: recording halt %q for %s: %w", reason, set, err)
	}
	return nil
}

// ClearBackupSetHalt removes set's standing refusal, if it has one.
//
// A set with no refusal recorded is not an error: every cycle that
// connects calls this for the set it just ran, and most of those sets were
// never refused. What this must never become is a call that fires on
// weaker evidence than "this cycle actually connected" (see
// internal/app/halt.go).
func (j *Journal) ClearBackupSetHalt(ctx context.Context, set model.BackupSetID) error {
	if set.IsZero() {
		return fmt.Errorf("state: clearing a halt needs a backup set id")
	}
	_, err := j.db.ExecContext(ctx,
		`DELETE FROM backup_set_halts WHERE source = ? AND backup_set = ?`,
		set.Source, set.Set,
	)
	if err != nil {
		return fmt.Errorf("state: clearing halt for %s: %w", set, err)
	}
	return nil
}

// ListBackupSetHalts returns every backup set currently carrying a
// connection refusal, in no particular order.
//
// The result is the whole population, not a per-set lookup, because its
// one caller builds a health report over every configured backup set and
// would otherwise issue a query per set on every status call and dashboard
// load. Sets with no refusal are simply absent.
//
// A row for a backup set that is no longer configured stays here and is
// never reported: the caller walks the configured sets and looks each one
// up in what this returns, so an orphan is inert rather than visible.
// Nothing collects it, because collecting it would mean this package
// knowing what the configuration currently contains, which is exactly the
// policy knowledge its own package doc says it does not have.
func (j *Journal) ListBackupSetHalts(ctx context.Context) ([]BackupSetHalt, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT source, backup_set, reason, observed_at FROM backup_set_halts ORDER BY source, backup_set`)
	if err != nil {
		return nil, fmt.Errorf("state: listing backup set halts: %w", err)
	}
	defer rows.Close()

	var out []BackupSetHalt
	for rows.Next() {
		var source, set, reason, observed string
		if err := rows.Scan(&source, &set, &reason, &observed); err != nil {
			return nil, fmt.Errorf("state: scanning backup set halt: %w", err)
		}
		id, err := model.NewBackupSetID(source, set)
		if err != nil {
			return nil, fmt.Errorf("state: backup set halt has an unusable identity %q/%q: %w", source, set, err)
		}
		at, err := parseTime(observed)
		if err != nil {
			return nil, fmt.Errorf("state: backup set halt for %s has an unparseable observed_at %q: %w", id, observed, err)
		}
		out = append(out, BackupSetHalt{Set: id, Reason: reason, ObservedAt: at})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: listing backup set halts: %w", err)
	}
	return out, nil
}
