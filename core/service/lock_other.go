//go:build !unix

package service

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrStartupLocked mirrors lock_unix.go's sentinel of the same name so
// callers on any GOOS can compare against one identifier; it is never
// actually returned on this build (acquireStartupLock below always fails
// with a different, more specific error first).
var ErrStartupLocked = errors.New("service: another process is already running this journal's startup sequence")

// startupLock is the non-unix stand-in for lock_unix.go's real
// implementation.
type startupLock struct{}

// acquireStartupLock fails loudly on any GOOS outside the "unix" build
// tag: this project ships and is CI-tested only for linux/amd64,
// linux/arm64 and darwin (see internal/capacity/statfs_other.go for the
// same precedent), so there is no advisory-locking primitive wired up for
// anything else. Refusing outright here is consistent with this
// codebase's own rule that a safety condition it cannot honestly assess
// is reported, never silently skipped.
func acquireStartupLock(lockPath string) (*startupLock, error) {
	return nil, fmt.Errorf("service: startup locking is not implemented on %s", runtime.GOOS)
}

func (l *startupLock) release() error { return nil }
