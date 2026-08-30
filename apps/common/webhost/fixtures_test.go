package webhost

import (
	"context"
	"errors"
	"fmt"
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
// branch handlers_operations.go maps to an HTTP status. errOnPreview and
// errOnApply are their retention equivalents, for handlers_retention.go's
// own error-mapping tests.
type syncFakeBackend struct {
	mu             sync.Mutex
	configRevision string
	ops            map[string]service.Operation
	errOnSubmit    error
	nextID         int

	// plans holds every plan PreviewRetention has issued and
	// ApplyRetentionPlan has not yet consumed, mirroring core/service's
	// own single-use plan store closely enough for handlers_retention_test.go
	// to exercise a real preview-then-apply round trip (including the
	// stale/not-found paths) without needing a real journal.
	plans        map[string]service.RetentionPlan
	planNextID   int
	errOnPreview error
	errOnApply   error
}

func newSyncFakeBackend() *syncFakeBackend {
	return &syncFakeBackend{configRevision: "rev-1", ops: map[string]service.Operation{}, plans: map[string]service.RetentionPlan{}}
}

func (f *syncFakeBackend) ConfigRevision() string { return f.configRevision }

// PreviewRetention returns a fixed, single-artifact DELETE plan for any
// source/set, storing it exactly once so a matching ApplyRetentionPlan
// call can consume it and any other plan_id is correctly reported not
// found.
func (f *syncFakeBackend) PreviewRetention(_ context.Context, source, set string) (service.RetentionPlan, error) {
	if f.errOnPreview != nil {
		return service.RetentionPlan{}, f.errOnPreview
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planNextID++
	plan := service.RetentionPlan{
		PlanID:            fmt.Sprintf("retplan_test_%d", f.planNextID),
		BackupSetID:       source + "/" + set,
		InventoryRevision: "inv-test-1",
		ConfigRevision:    f.configRevision,
		ExpiresAt:         time.Now().UTC().Add(10 * time.Minute),
		KeepCount:         0,
		DeleteCount:       1,
		ReclaimBytes:      1024,
		Verdicts: []service.RetentionArtifactVerdict{
			{Artifact: "backup.dump", Action: "DELETE", Reason: "no GFS tier selects this artifact (test fixture)"},
		},
	}
	f.plans[plan.PlanID] = plan
	return plan, nil
}

// ApplyRetentionPlan consumes a plan PreviewRetention issued (single-use,
// mirroring core/service.BackupService.ApplyRetentionPlan's own contract),
// or reports service.ErrRetentionPlanNotFound for any plan_id it does not
// hold.
func (f *syncFakeBackend) ApplyRetentionPlan(_ context.Context, req service.ApplyRetentionRequest) (service.RetentionPlan, error) {
	if f.errOnApply != nil {
		return service.RetentionPlan{}, f.errOnApply
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	plan, ok := f.plans[req.PlanID]
	if !ok {
		return service.RetentionPlan{}, fmt.Errorf("%w: %s", service.ErrRetentionPlanNotFound, req.PlanID)
	}
	delete(f.plans, req.PlanID)
	plan.OperationID = "op_test_retention_apply"
	return plan, nil
}

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

// PreviewRetention and ApplyRetentionPlan below only exist to satisfy
// BackupServiceClient: asyncFakeBackend's whole reason to exist is
// disconnect_test.go's SubmitRunCycle-specific race, and no test in this
// package needs it to behave like a real retention backend too.
func (f *asyncFakeBackend) PreviewRetention(_ context.Context, _, _ string) (service.RetentionPlan, error) {
	return service.RetentionPlan{}, errors.New("asyncFakeBackend: PreviewRetention is not implemented")
}

func (f *asyncFakeBackend) ApplyRetentionPlan(_ context.Context, _ service.ApplyRetentionRequest) (service.RetentionPlan, error) {
	return service.RetentionPlan{}, errors.New("asyncFakeBackend: ApplyRetentionPlan is not implemented")
}

// release lets every SubmitRunCycle call currently blocked on f.gate
// finish. Safe to call exactly once.
func (f *asyncFakeBackend) release() { close(f.gate) }

var errBoom = errors.New("boom")
