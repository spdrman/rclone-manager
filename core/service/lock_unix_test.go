//go:build unix

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
