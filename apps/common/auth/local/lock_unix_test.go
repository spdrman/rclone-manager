//go:build unix

// The lock's three behaviours, and the middle one is the point.
//
// A second acquire on a held lock must fail with ErrStoreLocked, and that
// is what makes a duplicate server or a create-admin racing a live one a
// refusal instead of a corrupted store. The reacquire-after-release case is
// the control: without it, an acquireStoreLock that failed unconditionally
// would satisfy the first test perfectly.
//
// The build tag matches lock_unix.go's, so this file compiles exactly
// where the implementation it tests does.
package local

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestAcquireStoreLock_SecondAcquireOnSameFileFails is issue #322's
// concurrency-safety requirement made concrete at the primitive level: a
// second attempt to lock the same store (standing in for a running
// Service and a concurrent `create-admin` invocation both reaching for
// the same store) must be refused, not silently proceed and race the
// first.
func TestAcquireStoreLock_SecondAcquireOnSameFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")

	first, err := acquireStoreLock(path)
	if err != nil {
		t.Fatalf("first acquireStoreLock: %v", err)
	}
	defer func() { _ = first.release() }()

	_, err = acquireStoreLock(path)
	if !errors.Is(err, ErrStoreLocked) {
		t.Fatalf("second acquireStoreLock error = %v, want ErrStoreLocked", err)
	}
}

func TestAcquireStoreLock_ReleaseAllowsAReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")

	first, err := acquireStoreLock(path)
	if err != nil {
		t.Fatalf("first acquireStoreLock: %v", err)
	}
	if err := first.release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := acquireStoreLock(path)
	if err != nil {
		t.Fatalf("second acquireStoreLock after release: %v", err)
	}
	defer func() { _ = second.release() }()
}

func TestAcquireStoreLock_ReleaseOnNilIsANoOp(t *testing.T) {
	var l *storeLock
	if err := l.release(); err != nil {
		t.Fatalf("release on nil *storeLock: %v", err)
	}
}
