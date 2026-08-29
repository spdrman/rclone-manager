package webhost

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/core/service"
)

// fakeAuthenticator is a minimal, always-succeeding or always-failing
// capabilities.Authenticator, standing in for whatever real local-auth
// (#106's reserved apps/common/auth/local) or platform-auth (#92) adapter
// eventually gets wired into a production PlatformAdapter. This package's
// own tests only need to prove the router calls SOME Authenticator and
// respects its verdict, not exercise a real one.
type fakeAuthenticator struct {
	authenticated bool
	username      string
}

func (f fakeAuthenticator) Authenticate(_ context.Context, _ capabilities.AuthRequest) (capabilities.AuthContext, error) {
	if !f.authenticated {
		return capabilities.AuthContext{}, nil
	}
	return capabilities.AuthContext{
		Authenticated: true,
		Username:      f.username,
		Mode:          capabilities.AuthModeLocalAccount,
	}, nil
}

// fakePlatformAdapter is a minimal capabilities.PlatformAdapter a test can
// configure with a specific Authenticator and PlatformCapabilities. It
// deliberately does NOT embed capabilities.BasePlatformAdapter, so tests
// that want "no auth wired at all" construct one directly instead (see
// noAuthWiredAdapter), matching how a provider that has genuinely not
// implemented NativeAuth yet would look.
type fakePlatformAdapter struct {
	caps capabilities.PlatformCapabilities
	auth capabilities.Authenticator
}

func (f fakePlatformAdapter) ID() capabilities.PlatformID { return capabilities.PlatformGeneric }

func (f fakePlatformAdapter) Capabilities() capabilities.PlatformCapabilities { return f.caps }

func (f fakePlatformAdapter) Authenticator() capabilities.Authenticator { return f.auth }

func (f fakePlatformAdapter) Notifier() capabilities.Notifier { return nil }

func (f fakePlatformAdapter) PlatformInfo(_ context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: capabilities.PlatformGeneric, Name: "fake"}, nil
}

// allowingPlatform returns a PlatformAdapter whose Authenticator succeeds
// for every request, as the username given.
func allowingPlatform(username string) capabilities.PlatformAdapter {
	return fakePlatformAdapter{
		caps: capabilities.PlatformCapabilities{NativeAuth: true, StoragePicker: true},
		auth: fakeAuthenticator{authenticated: true, username: username},
	}
}

// noAuthWiredAdapter is a bare-minimum PlatformAdapter that embeds only
// capabilities.BasePlatformAdapter: exactly what a provider app looks like
// before it has wired ANY real Authenticator in (today, for every
// provider: local-auth is #106's reserved apps/common/auth/local,
// platform-auth is #92, neither is built yet). Its Authenticator() is
// BasePlatformAdapter's null object, which always fails with
// ErrCapabilityUnsupported. Router tests use this to prove the API is
// unreachable by construction until a real Authenticator exists, not
// merely by some handler remembering to check a flag.
type noAuthWiredAdapter struct {
	capabilities.BasePlatformAdapter
}

func (noAuthWiredAdapter) ID() capabilities.PlatformID { return capabilities.PlatformGeneric }

func (noAuthWiredAdapter) Capabilities() capabilities.PlatformCapabilities {
	return capabilities.PlatformCapabilities{}
}

func (noAuthWiredAdapter) PlatformInfo(_ context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: capabilities.PlatformGeneric, Name: "no-auth"}, nil
}

// alwaysPassGate lets a test exercise the "gate has passed" branch of the
// destructive-operations check. NotYetImplementedGate (gate.go) is the
// only implementation this repository actually ships/wires in production;
// this type exists purely so router_test.go and handlers_operations_test.go
// can prove the gate is actually consulted (both when it fails and when it
// succeeds), not merely assume it.
type alwaysPassGate struct{}

func (alwaysPassGate) Passed() bool { return true }

// syncFakeBackend is a BackupServiceClient double that resolves
// SubmitRunCycle synchronously (no goroutine, no delay), for the handler
// unit tests that only care about request parsing, status codes and JSON
// shapes and would otherwise have to poll for an async result that adds
// nothing to what they are proving. errOnSubmit, when non-nil, is returned
// by SubmitRunCycle unconditionally, letting a test drive every error
// branch handlers_operations.go maps to an HTTP status.
type syncFakeBackend struct {
	mu             sync.Mutex
	configRevision string
	ops            map[string]service.Operation
	errOnSubmit    error
	nextID         int
}

func newSyncFakeBackend() *syncFakeBackend {
	return &syncFakeBackend{configRevision: "rev-1", ops: map[string]service.Operation{}}
}

func (f *syncFakeBackend) ConfigRevision() string { return f.configRevision }

func (f *syncFakeBackend) SubmitRunCycle(_ context.Context, req service.RunCycleRequest) (service.Operation, error) {
	if f.errOnSubmit != nil {
		return service.Operation{}, f.errOnSubmit
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range f.ops {
		if op.IdempotencyKey == req.IdempotencyKey {
			return op, nil
		}
	}
	f.nextID++
	op := service.Operation{
		ID:             "op_test_" + strconv.Itoa(f.nextID),
		IdempotencyKey: req.IdempotencyKey,
		Actor:          req.Actor,
		ConfigRevision: req.ConfigRevision,
		Action:         service.ActionRunCycle,
		Status:         "completed",
		CreatedAt:      time.Now().UTC(),
		FinishedAt:     time.Now().UTC(),
		Result:         `{"backup_sets_processed":0}`,
	}
	f.ops[op.ID] = op
	return op, nil
}

func (f *syncFakeBackend) GetOperation(_ context.Context, id string) (service.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op, ok := f.ops[id]
	if !ok {
		return service.Operation{}, service.ErrOperationNotFound
	}
	return op, nil
}

// asyncFakeBackend is a BackupServiceClient double whose SubmitRunCycle
// behaves like the real core/service.BackupService's own contract:
// persist synchronously, then finish the work later on a goroutine that
// uses context.Background(), never the ctx SubmitRunCycle itself was
// called with. gate is closed by the test to release whatever executions
// are currently waiting, at a time of the test's choosing — specifically
// AFTER the HTTP client that submitted them has already disconnected —
// which is what makes disconnect_test.go's proof deterministic instead of
// racing a real sleep against a real network teardown.
type asyncFakeBackend struct {
	mu   sync.Mutex
	ops  map[string]service.Operation
	gate chan struct{}
}

func newAsyncFakeBackend() *asyncFakeBackend {
	return &asyncFakeBackend{ops: map[string]service.Operation{}, gate: make(chan struct{})}
}

func (f *asyncFakeBackend) ConfigRevision() string { return "rev-1" }

func (f *asyncFakeBackend) SubmitRunCycle(_ context.Context, req service.RunCycleRequest) (service.Operation, error) {
	f.mu.Lock()
	op := service.Operation{
		ID:             "op_disconnect_test",
		IdempotencyKey: req.IdempotencyKey,
		Actor:          req.Actor,
		ConfigRevision: req.ConfigRevision,
		Action:         service.ActionRunCycle,
		Status:         "queued",
		CreatedAt:      time.Now().UTC(),
	}
	f.ops[op.ID] = op
	f.mu.Unlock()

	// Deliberately context.Background(), not the ctx parameter above: this
	// is the exact property docs/EPIC-B-multi-nas.md §14 requires ("HTTP
	// request lifetime SHALL NOT own operation lifetime"), reproduced here
	// the same way core/service.BackupService.executeRunCycle actually
	// does it, so this fake can stand in for the real thing in an HTTP-
	// layer test without dragging in a real journal/config/transport.
	go func() {
		<-f.gate
		f.mu.Lock()
		done := f.ops[op.ID]
		done.Status = "completed"
		done.FinishedAt = time.Now().UTC()
		done.Result = `{"backup_sets_processed":0}`
		f.ops[op.ID] = done
		f.mu.Unlock()
	}()

	return op, nil
}

func (f *asyncFakeBackend) GetOperation(_ context.Context, id string) (service.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op, ok := f.ops[id]
	if !ok {
		return service.Operation{}, service.ErrOperationNotFound
	}
	return op, nil
}

// release lets every SubmitRunCycle call currently blocked on f.gate
// finish. Safe to call exactly once.
func (f *asyncFakeBackend) release() { close(f.gate) }

var errBoom = errors.New("boom")
