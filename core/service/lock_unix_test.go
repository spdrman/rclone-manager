//go:build unix

// This file covers the startup lock's actual behaviour against a real
// file: a second acquire is refused, a release makes it available again,
// and releasing nothing is harmless.
//
// The third case exists for the callers rather than for the lock. The
// release is documented as safe on a nil lock precisely so a caller can
// defer it without first working out whether it acquired one, and a
// property that is only promised in prose stops being true the first time
// somebody rearranges a startup path. A panic there would replace the
// error an operator needed to read with a crash in the cleanup.
//
// Refusal is the case worth having on a real file rather than a fake.
// flock's whole value here is that the kernel drops it when a process
// dies, and a lock reimplemented as a Go mutex for the test would prove
// nothing about the property the design is actually buying.
package service

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestAcquireStartupLock_SecondAcquireOnSameFileFails is the RED
// checklist's "lock service initialization" step made concrete: a second
// concurrent attempt to acquire the same startup lock (standing in for a
// second process racing to snapshot-and-migrate the same journal) must be
// refused, not silently proceed and race the first.
func TestAcquireStartupLock_SecondAcquireOnSameFileFails(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.db.startup-lock")

	first, err := acquireStartupLock(lockPath)
	if err != nil {
		t.Fatalf("first acquireStartupLock: %v", err)
	}
	defer func() { _ = first.release() }()

	_, err = acquireStartupLock(lockPath)
	if !errors.Is(err, ErrStartupLocked) {
		t.Fatalf("second acquireStartupLock error = %v, want ErrStartupLocked", err)
	}
}

func TestAcquireStartupLock_ReleaseAllowsAReacquire(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.db.startup-lock")

	first, err := acquireStartupLock(lockPath)
	if err != nil {
		t.Fatalf("first acquireStartupLock: %v", err)
	}
	if err := first.release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := acquireStartupLock(lockPath)
	if err != nil {
		t.Fatalf("second acquireStartupLock after release: %v", err)
	}
	defer func() { _ = second.release() }()
}

func TestAcquireStartupLock_ReleaseOnNilIsANoOp(t *testing.T) {
	var l *startupLock
	if err := l.release(); err != nil {
		t.Fatalf("release on nil *startupLock: %v", err)
	}
}
