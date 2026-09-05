//go:build unix

// This file is the two advisory locks §46.1's startup sequence runs on,
// and the reason there are two of them rather than one.
//
// They answer different questions and are held for wildly different
// lengths of time. The startup lock serialises processes that are both
// inside runStartupSequence, and is dropped the moment that sequence
// ends. The journal lock is held for as long as a process has the journal
// open at all, shared by everyone who is merely reading it and taken
// exclusively only by a process about to change the schema. Folding them
// into one lock would force a choice between two wrong answers: hold it
// for the process lifetime and a `backup-manager status` alongside a
// running `serve` becomes an error, or hold it for the startup sequence
// only and a migration can run underneath a process that finished
// starting hours ago.
//
// flock(2) rather than a PID file, because the failure mode being
// designed for is the process not getting to run its cleanup. A container
// is expected to be killed (a restart policy, a rolling update, an
// operator losing patience), and a lock the kernel drops when the holder
// dies cannot leave a stale file that blocks every future start. That is
// also why every acquisition here is non-blocking: a startup that queues
// behind another process's lock is a container that hangs rather than one
// that says what is in its way.
package service

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ErrStartupLocked is returned by acquireStartupLock when another process
// already holds the section-46.1 startup lock for the same state
// directory — section 46.1's "lock service initialization" step refusing
// to let two processes snapshot and migrate the same journal
// concurrently. It is scoped to the startup sequence only (see
// acquireStartupLock's own doc): a second process is refused only while
// the first is actually inside that sequence, never for the lifetime of
// a running daemon, so an operator can still run `backup-manager status`
// (or any other read-only CLI command) against a journal a `serve`
// process already has open.
var ErrStartupLocked = errors.New("service: another process is already running this journal's startup sequence")

// startupLock is an OS-level advisory lock, held only for the duration of
// one startup sequence (runStartupSequence, startup.go, which is where
// exactly when it is acquired and released is spelled out).
type startupLock struct {
	f *os.File
}

// acquireStartupLock takes an exclusive, non-blocking flock(2) on lockPath
// (creating it if necessary). Unlike a PID-file-based lock, this is
// automatically released by the kernel if the holding process dies
// without calling release — including a hard kill — so a crashed or
// killed startup attempt can never leave a stale lock blocking every
// future restart forever, which matters for a container that is expected
// to be restarted routinely (Docker/UGOS restart policies, a rolling
// update) rather than always shut down cleanly.
func acquireStartupLock(lockPath string) (*startupLock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("service: open startup lock %s: %w", lockPath, err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrStartupLocked, lockPath)
		}
		return nil, fmt.Errorf("service: lock %s: %w", lockPath, err)
	}

	return &startupLock{f: f}, nil
}

// release drops the lock and closes the underlying file handle. Safe to
// call on a nil *startupLock (a no-op), so a caller can defer it
// unconditionally even on a path that never successfully acquired one.
func (l *startupLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	// Best-effort explicit unlock before Close: Close alone would also
	// release the flock (it is associated with the open file description),
	// but being explicit here means a future refactor that keeps the file
	// open longer for an unrelated reason cannot silently turn into "the
	// lock is held longer than intended" without this line being touched.
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}

// ErrJournalInUse is returned by acquireExclusiveJournalLock when another
// process still holds this journal open while this one needs to migrate it.
//
// This is the other half of the section-46.1 locking story, and the half
// the startup lock above deliberately does not cover. The startup lock
// serialises two processes that are both inside runStartupSequence; it says
// nothing about a process that finished its own startup sequence long ago
// and is now running with the journal open. Migrating (and, worse,
// restoring a pre-migration snapshot over the top) underneath such a
// process would rename a new inode into place while that process still
// holds the old one open, so its subsequent writes would go to an unlinked
// file and vanish. Refusing to migrate is the fail-closed answer: an
// operator stops the running process and retries, and no data is at risk in
// the meantime.
var ErrJournalInUse = errors.New("service: another process still has this journal open, so it cannot be migrated right now")

// journalLock is an OS-level advisory lock on a journal's own lock file,
// taken SHARED by every process that opens the journal and held for as long
// as that journal stays open, and taken EXCLUSIVE only by a process that is
// about to snapshot and migrate.
//
// Shared-versus-exclusive is what lets both of the things this codebase
// wants be true at once: any number of processes can have the journal open
// together (an operator's `backup-manager status` alongside a live `serve`
// is ordinary use of this CLI), while a process that needs to CHANGE the
// schema can prove, with the kernel rather than with a convention, that it
// is the only one there.
type journalLock struct {
	f *os.File
}

// acquireSharedJournalLock takes a shared flock(2) on lockPath, which
// succeeds against any number of other shared holders and fails only while
// a migrating process holds the exclusive lock.
func acquireSharedJournalLock(lockPath string) (*journalLock, error) {
	return acquireJournalLock(lockPath, unix.LOCK_SH)
}

// acquireExclusiveJournalLock takes an exclusive flock(2) on lockPath. It
// fails with ErrJournalInUse if any other process currently has the journal
// open (holding the shared lock), which is precisely the condition under
// which migrating would be unsafe.
func acquireExclusiveJournalLock(lockPath string) (*journalLock, error) {
	return acquireJournalLock(lockPath, unix.LOCK_EX)
}

func acquireJournalLock(lockPath string, how int) (*journalLock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("service: open journal lock %s: %w", lockPath, err)
	}

	if err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrJournalInUse, lockPath)
		}
		return nil, fmt.Errorf("service: lock %s: %w", lockPath, err)
	}

	return &journalLock{f: f}, nil
}

// downgradeToShared converts an exclusive journal lock into a shared one on
// the same open file description, so the process that just migrated keeps
// its journal open under the same shared lock every other process uses,
// instead of holding everyone else out for its whole lifetime.
//
// flock(2) does not promise the conversion is atomic, and it does not need
// to be here: the only thing that could slip into the gap is another
// process taking the exclusive lock, and to want it that process would have
// to find a migration pending, which the migration this call is the tail of
// has just applied.
func (l *journalLock) downgradeToShared() error {
	if l == nil || l.f == nil {
		return nil
	}
	if err := unix.Flock(int(l.f.Fd()), unix.LOCK_SH); err != nil {
		return fmt.Errorf("service: downgrading journal lock to shared: %w", err)
	}
	return nil
}

// release drops the lock and closes the underlying file handle. Safe to
// call on a nil *journalLock (a no-op), so a caller can defer it
// unconditionally.
func (l *journalLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}
