//go:build unix

// The advisory lock that makes store.go's "a single process owns this
// path" true rather than merely assumed.
//
// The mechanism is flock(2) on a sidecar file, chosen over a PID file for
// one reason that matters more than any other property: the kernel drops
// it when the holding process dies, including when it is killed outright.
// A PID-file lock left behind by a crashed server would block every
// subsequent start and every create-admin until somebody found and deleted
// it, which turns one crash into an outage that needs a human.
//
// This is the unix half of a build-tagged pair. lock_other.go refuses
// outright rather than pretending to lock, which is the right answer for a
// platform this project neither ships nor tests: a lock that silently does
// nothing is worse than no lock, because the code above it goes on
// believing it is safe.
package local

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ErrStoreLocked is returned by acquireStoreLock (and, through it,
// CreateAdmin and Service.New - provision.go/service.go) when another
// process already holds path's exclusive advisory lock.
//
// store.go's own doc comment has always assumed "a single process owns
// path"; this is what turns that assumption into something the kernel
// enforces rather than a convention every caller has to honor on its
// own. It matters most for issue #322's own reason to exist: CreateAdmin
// writes straight to the on-disk store, bypassing Service's normal
// read-modify-write cycle (Store.Enroll) entirely, so if a live Service
// happened to be running against the SAME store at the same moment, two
// processes could race that same file with no coordination at all. This
// lock is what makes that impossible instead of merely unlikely.
var ErrStoreLocked = errors.New("local: the administrator store is locked by another process (a running server, or a concurrent create-admin) - stop it first")

// storeLock is an OS-level advisory lock on one local-auth store, held for
// as long as its owner considers the store "open" - a running Service for
// its whole process lifetime, or CreateAdmin for just the duration of one
// write.
type storeLock struct {
	f *os.File
}

// acquireStoreLock takes an exclusive, non-blocking flock(2) on
// path+".lock" (creating it if necessary), mirroring
// core/service/lock_unix.go's acquireStartupLock exactly: unlike a
// PID-file-based lock, this is automatically released by the kernel if
// the holding process dies without calling release - including a hard
// kill - so a crashed server can never leave a stale lock blocking every
// future `create-admin` (or restart) forever.
func acquireStoreLock(path string) (*storeLock, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("local: open store lock: %w", err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrStoreLocked
		}
		return nil, fmt.Errorf("local: lock store: %w", err)
	}

	return &storeLock{f: f}, nil
}

// release drops the lock and closes the underlying file handle. Safe to
// call on a nil *storeLock (a no-op), so a caller can defer it
// unconditionally even on a path that never successfully acquired one.
func (l *storeLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	// Best-effort explicit unlock before Close: Close alone would also
	// release the flock (it is associated with the open file
	// description), but being explicit here means a future refactor that
	// keeps the file open longer for an unrelated reason cannot silently
	// turn into "the lock is held longer than intended" without this
	// line being touched.
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}
