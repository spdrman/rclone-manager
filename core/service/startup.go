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
	"errors"
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

// journalLockSuffix names the second lock file, alongside the first, that
// every process holding this journal open keeps a SHARED lock on for as
// long as it has it open. A process that needs to migrate takes it
// EXCLUSIVELY, which is what makes "nobody else has this journal open"
// something the kernel answers rather than something this package assumes.
// See lock_unix.go's journalLock for why the two locks are separate files
// rather than one.
const journalLockSuffix = ".journal-lock"

// runStartupSequence performs §46.1's ordered startup steps against
// dbPath and returns the opened, fully migrated journal together with the
// release func for the shared journal lock the caller must hold for as
// long as it keeps that journal open.
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
// # Snapshot only when a migration is actually pending
//
// state.PendingMigration is consulted before anything is copied, and on a
// start where the on-disk schema already matches this binary's embedded
// migration set — which is every start after the first, and every CLI
// subcommand run against a journal a daemon is already serving — this
// function takes no snapshot and arms no restore path at all. It opens the
// journal under the shared lock and returns.
//
// That ordering is the point, not an optimisation. The restore below
// rename-overwrites the journal's files, so an armed restore is the one
// thing in this codebase that can destroy an FR-9 journal. Keeping it
// armed on every `backup-manager status` bought nothing (there was no
// migration to undo) and risked everything, so it is now reached only on a
// start that genuinely is about to change the schema.
//
// # Two locks, shared and exclusive
//
// The startup lock is held for this function's duration only, released
// unconditionally on every return path by the deferred call below. Its job
// is serialising two concurrent snapshot-then-migrate attempts against one
// journal (a container restart racing an old process's shutdown against a
// new one's start).
//
// It cannot, on its own, say anything about a process that finished its
// own startup sequence long ago and is still running with the journal
// open, which is exactly the process a migration (or a restore) would
// destroy data underneath. So there is a second lock, taken on every
// successful start and held by the caller for as long as it keeps the
// journal: SHARED, so any number of processes coexist (`backup-manager
// status` alongside a live `serve` stays ordinary use of this CLI), and
// taken EXCLUSIVELY by a process that needs to migrate, which therefore
// refuses with ErrJournalInUse rather than migrating underneath a live
// holder.
//
// What each failure does to the data already on disk:
//
//   - validateStateDir, either lock, or the pending-migration check fails:
//     nothing has touched the journal yet, so there is nothing to undo. The
//     files on disk are already exactly as they were.
//   - snapshotSQLite fails: same, and it fails BEFORE migration precisely
//     so a journal that could not be captured is never migrated. Refusing
//     to start is the honest outcome; migrating anyway with no way back
//     is not.
//   - state.Open (the migration) fails with one of internal/state's own
//     refusals, ErrUnknownSchemaVersion or ErrSchemaDrift: nothing is
//     restored, because neither refusal applies anything. They are both
//     decided before applyMigration is ever called, so the files on disk
//     are untouched and putting a copy of them back over themselves could
//     only ever make things worse.
//   - state.Open fails for any other reason, having possibly half-applied
//     a migration: the pre-migration snapshot is restored before returning,
//     so whatever a partially applied attempt left behind is replaced by
//     the exact bytes that were there beforehand.
func runStartupSequence(ctx context.Context, dbPath string) (*state.Journal, func() error, error) {
	if err := validateStateDir(dbPath); err != nil {
		return nil, nil, err
	}

	startupLock, err := acquireStartupLock(dbPath + startupLockSuffix)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = startupLock.release() }()

	pending, err := state.PendingMigration(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}

	if !pending {
		return openUnderSharedLock(ctx, dbPath)
	}
	return migrateUnderExclusiveLock(ctx, dbPath)
}

// openUnderSharedLock is the ordinary start: the schema is already what
// this binary expects, so no snapshot is taken, no restore is armed, and
// the journal is opened under the shared lock alongside whatever other
// processes already have it open.
func openUnderSharedLock(ctx context.Context, dbPath string) (*state.Journal, func() error, error) {
	lock, err := acquireSharedJournalLock(dbPath + journalLockSuffix)
	if err != nil {
		return nil, nil, err
	}

	journal, err := state.Open(ctx, dbPath)
	if err != nil {
		_ = lock.release()
		return nil, nil, fmt.Errorf("service: open state: %w", err)
	}
	return journal, lock.release, nil
}

// migrateUnderExclusiveLock is the rare start that is actually about to
// change the schema: a first-ever deployment, or an upgrade to a binary
// carrying migrations this journal has not seen. It is the only path that
// snapshots, and therefore the only one on which a restore can ever fire.
func migrateUnderExclusiveLock(ctx context.Context, dbPath string) (*state.Journal, func() error, error) {
	lock, err := acquireExclusiveJournalLock(dbPath + journalLockSuffix)
	if err != nil {
		return nil, nil, err
	}

	snap, err := snapshotSQLite(dbPath)
	if err != nil {
		_ = lock.release()
		return nil, nil, err
	}

	journal, err := state.Open(ctx, dbPath)
	if err != nil {
		_ = lock.release()
		return nil, nil, migrationFailure(snap, err)
	}

	if err := lock.downgradeToShared(); err != nil {
		_ = journal.Close()
		_ = lock.release()
		return nil, nil, err
	}
	return journal, lock.release, nil
}

// migrationFailure turns a failed state.Open into the error this sequence
// reports, restoring the pre-migration snapshot first — but only when the
// failure is one that could have changed something.
//
// internal/state's two refusals are decided by migrate() before
// applyMigration is called at all (see that function's own doc), so on
// either of them the files on disk are provably exactly what they were a
// moment ago. Restoring over them would be a rename-overwrite of a journal
// nothing has touched, for no benefit, and on a machine where another
// process has since opened that journal it is an active hazard. So those
// two are returned as they are.
func migrationFailure(snap *sqliteSnapshot, cause error) error {
	wrapped := fmt.Errorf("service: open state: %w", cause)
	if errors.Is(cause, state.ErrUnknownSchemaVersion) || errors.Is(cause, state.ErrSchemaDrift) {
		return wrapped
	}
	return restoreSnapshotAfter(snap, wrapped)
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
