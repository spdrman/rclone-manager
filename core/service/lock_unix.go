//go:build unix

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
