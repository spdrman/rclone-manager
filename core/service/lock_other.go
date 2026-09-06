//go:build !unix

package service

import (
	"errors"
	"fmt"
	"runtime"
	"time"
)

// This file is what lock_unix.go's advisory locking becomes on a GOOS
// this project does not ship: nothing, loudly.
//
// It exists so the rest of the package compiles everywhere without
// spelling out a build tag at every call site, and every function in it
// refuses rather than pretending. A no-op lock is the worst available
// answer, because it would let two processes snapshot and migrate the
// same journal at once while both believed they had been serialised, and
// the only evidence would be the damage afterwards. A safety condition
// this codebase cannot honestly assess is reported, never quietly
// skipped, and that rule is what these stubs implement.
//
// The sentinels are mirrored rather than moved into a shared file so that
// a caller comparing against ErrStartupLocked or ErrJournalInUse names
// one identifier on every platform. Neither is ever returned from this
// build: acquiring fails first, with an error that names the GOOS, which
// is the thing somebody porting this actually needs to read.

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

// acquireStartupLockWithin mirrors lock_unix.go's bounded-wait variant.
// There is nothing to wait for on a build with no locking, so it refuses
// at once rather than sleeping first.
func acquireStartupLockWithin(lockPath string, wait time.Duration) (*startupLock, error) {
	return acquireStartupLock(lockPath)
}

func (l *startupLock) release() error { return nil }

// ErrJournalInUse mirrors lock_unix.go's sentinel of the same name so
// callers on any GOOS can compare against one identifier; it is never
// actually returned on this build.
var ErrJournalInUse = errors.New("service: another process still has this journal open, so it cannot be migrated right now")

// journalLock is the non-unix stand-in for lock_unix.go's real
// implementation.
type journalLock struct{}

func acquireSharedJournalLock(lockPath string) (*journalLock, error) {
	return nil, fmt.Errorf("service: journal locking is not implemented on %s", runtime.GOOS)
}

func acquireExclusiveJournalLock(lockPath string) (*journalLock, error) {
	return nil, fmt.Errorf("service: journal locking is not implemented on %s", runtime.GOOS)
}

func (l *journalLock) downgradeToShared() error { return nil }

func (l *journalLock) release() error { return nil }

// ErrAlreadyServing mirrors lock_unix.go's sentinel of the same name so
// callers on any GOOS name one identifier; it is never actually returned
// on this build.
var ErrAlreadyServing = errors.New("service: another process is already serving this deployment")

// servingLock is the non-unix stand-in for lock_unix.go's real
// implementation.
type servingLock struct{}

// acquireServingLock refuses rather than pretending, for the same reason
// every other acquire in this file does: a process that believed it had
// announced itself as this deployment's engine, on a build where nothing
// can hear the announcement, would be invisible to exactly the check that
// exists to find it.
func acquireServingLock(lockPath string) (*servingLock, error) {
	return nil, fmt.Errorf("service: serving locks are not implemented on %s", runtime.GOOS)
}

func (l *servingLock) release() error { return nil }

// servingLockHeld cannot be answered on a GOOS with no advisory locking
// wired up, and says so rather than answering "no".
//
// "No" is the dangerous default here: it is the answer that lets a second
// process rewrite a configuration a running engine holds and report
// success, which is issue #535. This file's whole rule is that a safety
// condition this codebase cannot honestly assess is reported, never
// quietly skipped, and a detector is exactly the place that rule earns
// its keep.
func servingLockHeld(lockPath string) (bool, error) {
	return false, fmt.Errorf("service: cannot tell whether another process is serving this deployment on %s, because advisory locking is not implemented there", runtime.GOOS)
}
