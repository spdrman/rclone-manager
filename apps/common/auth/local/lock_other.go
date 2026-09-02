//go:build !unix

package local

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrStoreLocked mirrors lock_unix.go's sentinel of the same name so
// callers on any GOOS can compare against one identifier; it is never
// actually returned on this build (acquireStoreLock below always fails
// with a different, more specific error first).
var ErrStoreLocked = errors.New("local: the administrator store is locked by another process (a running server, or a concurrent create-admin) - stop it first")

// storeLock is the non-unix stand-in for lock_unix.go's real
// implementation.
type storeLock struct{}

// acquireStoreLock fails loudly on any GOOS outside the "unix" build tag:
// this project ships and is CI-tested only for linux/amd64, linux/arm64
// and darwin (see core/service/lock_other.go and
// internal/capacity/statfs_other.go for the same precedent), so there is
// no advisory-locking primitive wired up for anything else. Refusing
// outright here is consistent with this codebase's own rule that a
// safety condition it cannot honestly assess is reported, never silently
// skipped.
func acquireStoreLock(path string) (*storeLock, error) {
	return nil, fmt.Errorf("local: store locking is not implemented on %s", runtime.GOOS)
}

func (l *storeLock) release() error { return nil }
