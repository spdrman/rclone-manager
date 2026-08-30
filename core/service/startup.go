// This file is docs/EPIC-B-multi-nas.md §46.1's startup sequence, issue
// #104 (B3.4), extracted out of OpenConfigAndJournal (service.go) so the
// ordering of its steps, and what each failure between them does to the
// data already on disk, is one readable block rather than something a
// reader has to reconstruct from the middle of a config-loading function.
//
// The pieces it drives all live in their own files alongside this one:
// validateStateDir (statedir.go), acquireStartupLock (lock_unix.go /
// lock_other.go) and snapshotSQLite (snapshot.go). The migration itself
// stays exactly where it already was, inside internal/state.Open — this
// package never reimplements, bypasses or retries around it, including
// around its existing ErrUnknownSchemaVersion refusal for a schema newer
// than this binary knows.
package service

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/state"
)

// startupLockSuffix names the advisory lock file next to the journal
// itself, rather than in a shared location like /var/run: two processes
// racing to snapshot-then-migrate matters only when they are racing over
// the SAME journal, and deriving the lock's path from that journal's own
// path is what makes "same journal" and "same lock" the identical
// question, with no configuration to get wrong.
const startupLockSuffix = ".startup-lock"

// runStartupSequence performs §46.1's ordered startup steps against
// dbPath and returns the opened, fully migrated journal.
//
// The spec draws the sequence as:
//
//	load version -> lock init -> validate state dir ->
//	backup/prepare SQLite -> run migrations ->
//	start BackupService -> start daemon/API
//
// with two deliberate differences here. First, "load version" is the
// caller's own concern (BuildVersion, service.go), not a step of this
// function. Second, the state directory is validated BEFORE the lock is
// taken rather than after: the lock file lives inside that very
// directory, so on a brand-new deployment whose state directory does not
// exist yet, locking first would fail on a missing parent directory and
// report it as a locking problem instead of the "your state directory
// isn't there yet, and here is what happened when I tried to create it"
// that validateStateDir gives an operator. Reordering two steps that
// touch nothing but the directory itself costs the spec's intent nothing
// and buys a legible first-run error.
//
// The last two steps, starting BackupService and the daemon/API, are not
// this function's to perform, and that is precisely how §46.1's
// migration-failure requirement is met: on ANY failure below this
// returns a nil *state.Journal, and every caller (OpenConfigAndJournal ->
// Open, and cmd/backup-manager's openService) already treats that as
// fatal and constructs no BackupService at all, so nothing downstream of
// it — no scheduler tick, no cycle, no transfer, no delete — ever begins.
//
// What each failure does to the data already on disk:
//
//   - validateStateDir or acquireStartupLock fails: nothing has touched
//     the journal yet, so there is nothing to undo. The files on disk are
//     already exactly as they were.
//   - snapshotSQLite fails: same, and it fails BEFORE migration precisely
//     so a journal that could not be captured is never migrated. Refusing
//     to start is the honest outcome; migrating anyway with no way back
//     is not.
//   - state.Open (the migration) fails, for any reason including
//     internal/state's own ErrUnknownSchemaVersion downgrade refusal: the
//     pre-migration snapshot is restored before returning, so whatever a
//     partially applied attempt left behind is replaced by the exact
//     bytes that were there beforehand.
//
// The lock is held for this function's duration only, released
// unconditionally on every return path by the deferred call below. Its
// job is serialising two concurrent snapshot-then-migrate attempts
// against one journal (a container restart racing an old process's
// shutdown against a new one's start), not keeping an operator's
// `backup-manager status` from running alongside a live `serve` — those
// coexisting is ordinary use of this CLI, and by the time `status` calls
// this function, `serve`'s own call released the lock long ago.
func runStartupSequence(ctx context.Context, dbPath string) (*state.Journal, error) {
	if err := validateStateDir(dbPath); err != nil {
		return nil, err
	}

	lock, err := acquireStartupLock(dbPath + startupLockSuffix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.release() }()

	snap, err := snapshotSQLite(dbPath)
	if err != nil {
		return nil, err
	}

	journal, err := state.Open(ctx, dbPath)
	if err != nil {
		return nil, restoreSnapshotAfter(snap, fmt.Errorf("service: open state: %w", err))
	}

	return journal, nil
}

// restoreSnapshotAfter puts snap back and returns cause, so a caller
// reads "this failed, and the previous data was put back" as one
// expression. A restore that ITSELF fails is reported alongside cause
// rather than replacing it: an operator needs to know both what went
// wrong and that the usual guarantee ("your previous data is untouched")
// did not hold this time, since that second half is what decides whether
// they can simply retry or have to reach for their own backup of the
// state directory.
func restoreSnapshotAfter(snap *sqliteSnapshot, cause error) error {
	if restoreErr := snap.restore(); restoreErr != nil {
		return fmt.Errorf("%w (restoring the pre-migration snapshot also failed, previous data may be at risk: %v)", cause, restoreErr)
	}
	return cause
}
