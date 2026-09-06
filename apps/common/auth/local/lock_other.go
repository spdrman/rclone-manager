//go:build !unix

package local

import (
	"errors"
	"fmt"
	"runtime"
)

// The non-unix half of the store lock, which is a refusal rather than an
// implementation.
//
// A stub that returned a working-looking lock without locking anything
// would compile everywhere and be wrong exactly where it matters: two
// processes would both proceed, both read the store, and one would
// overwrite the other's administrator record. So this returns an error
// naming the GOOS instead, and everything above it fails to start.
//
// The build tag is the whole platform story. This project ships and tests
// linux/amd64, linux/arm64 and darwin, all of which are unix, so this file
// is compiled by nobody in practice and exists to keep the tree building
// for anyone who tries a fourth platform, loudly.

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

// release is a no-op because nothing was ever acquired: acquireStoreLock
// on this build always fails, so no caller can hold one of these.
func (l *storeLock) release() error { return nil }
