//go:build unix

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
