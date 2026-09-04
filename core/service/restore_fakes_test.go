package service

import (
	"context"
	"sync"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// restoreOnlyTransport is a transport with nothing behind it: it satisfies
// transport.Transport and NOTHING else, which is what a deployment with no
// medium boundary looks like from BackupService's point of view.
type restoreOnlyTransport struct{}

func (restoreOnlyTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	return nil, nil
}

func (restoreOnlyTransport) Stat(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
	return transport.RemoteArtifact{}, nil
}

func (restoreOnlyTransport) CopyToLocal(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	return transport.TransferResult{}, nil
}

func (restoreOnlyTransport) RemoteHash(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
	return "", nil
}

func (restoreOnlyTransport) DeleteRemote(context.Context, transport.Source, string) error { return nil }

var _ transport.Transport = restoreOnlyTransport{}

// restoreCapableTransport is restoreOnlyTransport plus the two methods
// archive.Store needs, which is the shape the shipped rclone adapter has.
//
// It is deliberately assembled the same way: one value that satisfies both
// interfaces, discovered by a type assertion rather than by a constructor
// argument, so the test exercises the same capability discovery the
// product does.
type restoreCapableTransport struct {
	restoreOnlyTransport
	store *recordingRestoreStore
}

func (t *restoreCapableTransport) RestoreStatus(ctx context.Context, m transport.Medium, key string) (*transport.RestoreState, error) {
	return t.store.RestoreStatus(ctx, m, key)
}

func (t *restoreCapableTransport) InitiateRestore(ctx context.Context, m transport.Medium, key string, windowDays int) error {
	return t.store.InitiateRestore(ctx, m, key, windowDays)
}

// recordingRestoreStore is a provider that remembers every restore it was
// asked for.
//
// The window list is the point of it. Half of what a restore guard
// promises is about what did NOT happen: a refused request must not reach
// the provider, and a replayed idempotency key must not start a second
// restore. A double that only returned canned answers could not show
// either, because "refused" and "refused after spending the money anyway"
// look identical from outside.
type recordingRestoreStore struct {
	mu       sync.Mutex
	asked    []int
	state    *transport.RestoreState
	statusOf error
}

func (s *recordingRestoreStore) RestoreStatus(context.Context, transport.Medium, string) (*transport.RestoreState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusOf != nil {
		return nil, s.statusOf
	}
	return s.state, nil
}

func (s *recordingRestoreStore) InitiateRestore(_ context.Context, _ transport.Medium, _ string, windowDays int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, windowDays)
	// From here on the provider says a restore is running, which is what a
	// real one does and what makes a second submission a duplicate.
	s.state = &transport.RestoreState{InProgress: true}
	return nil
}

func (s *recordingRestoreStore) windows() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.asked...)
}

// finish makes the provider report the restore as done, readable until
// expiry, which is the only way a restore operation's row can ever reach a
// terminal status.
func (s *recordingRestoreStore) finish(expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := expiry
	s.state = &transport.RestoreState{InProgress: false, ExpiresAt: &at}
}
