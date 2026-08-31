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

	// errOnStorage is ListStorageStatus's equivalent, so a test can drive
	// systemStorage's own 500 branch (handlers_storage.go) rather than
	// only its success path.
	errOnStorage error

	// notReady makes Ready report false, standing in for a backend whose
	// §46.1 startup sequence did not complete. It is a field rather than a
	// second fake type because readiness is now a fact the backend owns
	// (BackupServiceClient.Ready), so "not ready" is a state a real
	// backend can be in, not only a missing one.
	notReady bool

	// --- issue #211's surfaces: what the fake holds, what it refuses
	// with, and what the handler asked it for.
	artifacts     []service.Artifact
	activity      []service.ActivityEvent
	operationList []service.Operation
	health        service.HealthReport
	catalog       service.CatalogReport

	revalidateResult          service.ArtifactCheck
	persistedConnectionResult service.ConnectionTestResult

	errOnArtifacts      error
	errOnActivity       error
	errOnListOperations error
	errOnHealth         error
	errOnCatalog        error
	errOnRevalidate     error
	errOnRetry          error
	errOnSetEnabled     error
	errOnTestPersisted  error

	lastArtifactFilter    service.ArtifactFilter
	lastActivityLimit     int
	lastOperationsLimit   int
	lastRevalidated       string
	lastRetried           string
	lastSetEnabled        setEnabledCall
	lastTestedBackupSetID string
}

func newSyncFakeBackend() *syncFakeBackend {
	return &syncFakeBackend{
		configRevision:            "rev-1",
		ops:                       map[string]service.Operation{},
		plans:                     map[string]service.RetentionPlan{},
		persistedConnectionResult: service.ConnectionTestResult{OK: true},
	}
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
			// One KEEP whose tiers were selected by DIFFERENT placements
			// (issue #218), so the wire shape cannot be satisfied by a
			// single per-verdict attribution, and one DELETE, which
			// carries no tiers and so no attribution either.
			{Artifact: "kept.dump", Action: "KEEP", Reason: "kept by the DAILY and MONTHLY tiers (test fixture)", Tiers: []service.RetentionTierSelection{
				{Tier: "DAILY", SelectedBy: "DISCOVERY"},
				{Tier: "MONTHLY", SelectedBy: "PRODUCER"},
				{Tier: "LAST_KNOWN_GOOD", SelectedBy: "PROTECTION"},
			}},
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
	// The real service cross-checks the {source}/{set} the request was
	// routed by against the backup set the plan was issued for, and
	// consumes nothing when they disagree (see
	// service.ApplyRetentionRequest.Source's own doc). This fake enforces
	// the same thing, so a handler that dropped the URL parameters on the
	// floor could not pass handlers_retention_test.go.
	if plan.BackupSetID != req.Source+"/"+req.Set {
		return service.RetentionPlan{}, fmt.Errorf("%w: plan %s was not issued for backup set %s/%s", service.ErrInvalidRequest, req.PlanID, req.Source, req.Set)
	}
	delete(f.plans, req.PlanID)
	plan.OperationID = "op_test_retention_apply"
	return plan, nil
}

func (f *syncFakeBackend) Ready() bool { return !f.notReady }

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

// The five methods below satisfy BackupServiceClient's issue #146
// surface. syncFakeBackend's own tests (handlers_operations_test.go) never
// call any of these — they exist only so this type still compiles as a
// BackupServiceClient; handlers_backupsets_test.go and
// handlers_ssh_test.go use backupSetFakeBackend (below) instead, which
// actually exercises them.
func (f *syncFakeBackend) ListBackupSets(context.Context) ([]service.BackupSet, error) {
	return nil, nil
}

func (f *syncFakeBackend) GetBackupSet(context.Context, string) (service.BackupSet, error) {
	return service.BackupSet{}, service.ErrBackupSetNotFound
}

func (f *syncFakeBackend) CreateBackupSet(context.Context, service.CreateBackupSetRequest) (service.CreateBackupSetResult, error) {
	return service.CreateBackupSetResult{}, errors.New("syncFakeBackend: CreateBackupSet not implemented")
}

func (f *syncFakeBackend) ImportSSHKey(context.Context, []byte) (service.SSHKeyRef, error) {
	return service.SSHKeyRef{}, errors.New("syncFakeBackend: ImportSSHKey not implemented")
}

func (f *syncFakeBackend) ProbeHostKey(context.Context, string, int) (service.HostKeyProbe, error) {
	return service.HostKeyProbe{}, errors.New("syncFakeBackend: ProbeHostKey not implemented")
}

func (f *syncFakeBackend) TestConnection(context.Context, service.ConnectionTestRequest) (service.ConnectionTestResult, error) {
	return service.ConnectionTestResult{}, errors.New("syncFakeBackend: TestConnection not implemented")
}

func (f *syncFakeBackend) ListStorageStatus(context.Context) ([]service.StorageStatus, error) {
	if f.errOnStorage != nil {
		return nil, f.errOnStorage
	}
	return nil, nil
}

// --- issue #211's read surface and its two quarantine actions ---
//
// Each of these is an in-memory store plus one error-injection field, so
// handlers_artifacts_test.go, handlers_activity_test.go,
// handlers_catalog_test.go and handlers_health_test.go can drive both the
// success shape and every mapped refusal without a real journal. The
// stores are exported through the fake's own fields rather than through
// setters: a test arranges the fixture, it does not script it.

func (f *syncFakeBackend) SetBackupSetEnabled(_ context.Context, id string, enabled bool) (service.BackupSet, error) {
	if f.errOnSetEnabled != nil {
		return service.BackupSet{}, f.errOnSetEnabled
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSetEnabled = setEnabledCall{id: id, enabled: enabled}
	return service.BackupSet{ID: id, Disabled: !enabled}, nil
}

func (f *syncFakeBackend) TestBackupSetConnection(_ context.Context, id string) (service.ConnectionTestResult, error) {
	if f.errOnTestPersisted != nil {
		return service.ConnectionTestResult{}, f.errOnTestPersisted
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastTestedBackupSetID = id
	return f.persistedConnectionResult, nil
}

func (f *syncFakeBackend) ListArtifacts(_ context.Context, filter service.ArtifactFilter) ([]service.Artifact, error) {
	if f.errOnArtifacts != nil {
		return nil, f.errOnArtifacts
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastArtifactFilter = filter
	out := make([]service.Artifact, 0, len(f.artifacts))
	for _, a := range f.artifacts {
		if filter.QuarantinedOnly && !a.Quarantined {
			continue
		}
		if filter.BackupSetID != "" && a.BackupSetID != filter.BackupSetID {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *syncFakeBackend) GetArtifact(_ context.Context, id string) (service.Artifact, error) {
	if f.errOnArtifacts != nil {
		return service.Artifact{}, f.errOnArtifacts
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.artifacts {
		if a.ID == id {
			return a, nil
		}
	}
	return service.Artifact{}, fmt.Errorf("%w: %s", service.ErrArtifactNotFound, id)
}

func (f *syncFakeBackend) RevalidateArtifact(_ context.Context, id string) (service.ArtifactCheck, error) {
	if f.errOnRevalidate != nil {
		return service.ArtifactCheck{}, f.errOnRevalidate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRevalidated = id
	return f.revalidateResult, nil
}

func (f *syncFakeBackend) RetryArtifactIngestion(_ context.Context, id string) error {
	if f.errOnRetry != nil {
		return f.errOnRetry
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRetried = id
	return nil
}

func (f *syncFakeBackend) ListActivity(_ context.Context, limit int) ([]service.ActivityEvent, error) {
	if f.errOnActivity != nil {
		return nil, f.errOnActivity
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastActivityLimit = limit
	return f.activity, nil
}

func (f *syncFakeBackend) ListOperations(_ context.Context, limit int) ([]service.Operation, error) {
	if f.errOnListOperations != nil {
		return nil, f.errOnListOperations
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastOperationsLimit = limit
	return f.operationList, nil
}

func (f *syncFakeBackend) Health(context.Context) (service.HealthReport, error) {
	if f.errOnHealth != nil {
		return service.HealthReport{}, f.errOnHealth
	}
	return f.health, nil
}

func (f *syncFakeBackend) ScanCatalog(context.Context) (service.CatalogReport, error) {
	if f.errOnCatalog != nil {
		return service.CatalogReport{}, f.errOnCatalog
	}
	report := f.catalog
	report.DryRun = true
	return report, nil
}

func (f *syncFakeBackend) RebuildCatalog(context.Context) (service.CatalogReport, error) {
	if f.errOnCatalog != nil {
		return service.CatalogReport{}, f.errOnCatalog
	}
	report := f.catalog
	report.DryRun = false
	return report, nil
}

// setEnabledCall records what crossed the HTTP-to-core seam, so a test can
// assert on the id and the flag the handler actually built rather than
// only on what came back out.
type setEnabledCall struct {
	id      string
	enabled bool
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

func (f *asyncFakeBackend) Ready() bool { return true }

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

// asyncFakeBackend's own tests (disconnect_test.go) only exercise
// SubmitRunCycle/GetOperation's async-disconnect behavior; these five
// stubs exist purely so it still satisfies BackupServiceClient, same
// reasoning as syncFakeBackend's identical block above.
func (f *asyncFakeBackend) ListBackupSets(context.Context) ([]service.BackupSet, error) {
	return nil, nil
}

func (f *asyncFakeBackend) GetBackupSet(context.Context, string) (service.BackupSet, error) {
	return service.BackupSet{}, service.ErrBackupSetNotFound
}

func (f *asyncFakeBackend) CreateBackupSet(context.Context, service.CreateBackupSetRequest) (service.CreateBackupSetResult, error) {
	return service.CreateBackupSetResult{}, errors.New("asyncFakeBackend: CreateBackupSet not implemented")
}

func (f *asyncFakeBackend) ImportSSHKey(context.Context, []byte) (service.SSHKeyRef, error) {
	return service.SSHKeyRef{}, errors.New("asyncFakeBackend: ImportSSHKey not implemented")
}

func (f *asyncFakeBackend) ProbeHostKey(context.Context, string, int) (service.HostKeyProbe, error) {
	return service.HostKeyProbe{}, errors.New("asyncFakeBackend: ProbeHostKey not implemented")
}

func (f *asyncFakeBackend) TestConnection(context.Context, service.ConnectionTestRequest) (service.ConnectionTestResult, error) {
	return service.ConnectionTestResult{}, errors.New("asyncFakeBackend: TestConnection not implemented")
}

func (f *asyncFakeBackend) ListStorageStatus(context.Context) ([]service.StorageStatus, error) {
	return nil, nil
}

// Issue #211's surfaces, on the async double. This fake exists only to
// prove operation lifetime survives a disconnected client
// (disconnect_test.go), so none of these has behaviour worth modelling:
// each returns the empty answer, which is a real answer for a deployment
// that has done nothing yet, rather than an error a test would then have
// to route around.
func (f *asyncFakeBackend) SetBackupSetEnabled(_ context.Context, id string, enabled bool) (service.BackupSet, error) {
	return service.BackupSet{ID: id, Disabled: !enabled}, nil
}

func (f *asyncFakeBackend) TestBackupSetConnection(context.Context, string) (service.ConnectionTestResult, error) {
	return service.ConnectionTestResult{OK: true}, nil
}

func (f *asyncFakeBackend) ListArtifacts(context.Context, service.ArtifactFilter) ([]service.Artifact, error) {
	return nil, nil
}

func (f *asyncFakeBackend) GetArtifact(_ context.Context, id string) (service.Artifact, error) {
	return service.Artifact{}, fmt.Errorf("%w: %s", service.ErrArtifactNotFound, id)
}

func (f *asyncFakeBackend) RevalidateArtifact(context.Context, string) (service.ArtifactCheck, error) {
	return service.ArtifactCheck{}, nil
}

func (f *asyncFakeBackend) RetryArtifactIngestion(context.Context, string) error { return nil }

func (f *asyncFakeBackend) ListActivity(context.Context, int) ([]service.ActivityEvent, error) {
	return nil, nil
}

func (f *asyncFakeBackend) ListOperations(context.Context, int) ([]service.Operation, error) {
	return nil, nil
}

func (f *asyncFakeBackend) Health(context.Context) (service.HealthReport, error) {
	return service.HealthReport{}, nil
}

func (f *asyncFakeBackend) ScanCatalog(context.Context) (service.CatalogReport, error) {
	return service.CatalogReport{DryRun: true}, nil
}

func (f *asyncFakeBackend) RebuildCatalog(context.Context) (service.CatalogReport, error) {
	return service.CatalogReport{}, nil
}

var errBoom = errors.New("boom")

// backupSetFakeBackend is a BackupServiceClient double for
// handlers_backupsets_test.go and handlers_ssh_test.go: an in-memory
// store for backup sets and imported SSH keys, so those tests can drive
// every request/response/error branch the issue #146 handlers add
// without a real config file, journal or transport. Operations-surface
// methods (SubmitRunCycle/GetOperation/ConfigRevision) delegate to an
// embedded syncFakeBackend, since createBackupSet's RunImmediately path
// calls through to those too.
type backupSetFakeBackend struct {
	*syncFakeBackend

	mu   sync.Mutex
	sets map[string]service.BackupSet
	keys map[string]service.SSHKeyRef

	// lastCreate records the exact service.CreateBackupSetRequest the
	// handler built, so a test can assert on what crossed the HTTP-to-core
	// seam (issue #162's validator_id, in particular) rather than only on
	// what came back out of it.
	lastCreate service.CreateBackupSetRequest

	errOnCreate  error
	errOnList    error
	errOnGet     error
	errOnImport  error
	errOnProbe   error
	errOnConnect error

	probeResult      service.HostKeyProbe
	connectionResult service.ConnectionTestResult
}

func newBackupSetFakeBackend() *backupSetFakeBackend {
	return &backupSetFakeBackend{
		syncFakeBackend:  newSyncFakeBackend(),
		sets:             map[string]service.BackupSet{},
		keys:             map[string]service.SSHKeyRef{},
		connectionResult: service.ConnectionTestResult{OK: true},
	}
}

func (f *backupSetFakeBackend) ListBackupSets(context.Context) ([]service.BackupSet, error) {
	if f.errOnList != nil {
		return nil, f.errOnList
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]service.BackupSet, 0, len(f.sets))
	for _, s := range f.sets {
		out = append(out, s)
	}
	return out, nil
}

func (f *backupSetFakeBackend) GetBackupSet(_ context.Context, id string) (service.BackupSet, error) {
	if f.errOnGet != nil {
		return service.BackupSet{}, f.errOnGet
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sets[id]
	if !ok {
		return service.BackupSet{}, service.ErrBackupSetNotFound
	}
	return s, nil
}

func (f *backupSetFakeBackend) CreateBackupSet(ctx context.Context, req service.CreateBackupSetRequest) (service.CreateBackupSetResult, error) {
	if f.errOnCreate != nil {
		return service.CreateBackupSetResult{}, f.errOnCreate
	}
	sourceName := req.SourceName
	if sourceName == "" {
		sourceName = "api"
	}
	set := service.BackupSet{
		ID:                 sourceName + "/" + req.Name,
		SourceName:         sourceName,
		Name:               req.Name,
		Host:               req.Host,
		Port:               req.Port,
		User:               req.User,
		RemotePath:         req.RemotePath,
		LocalPath:          req.LocalPath,
		Include:            req.Include,
		CompletionStrategy: req.CompletionStrategy,
		ValidatorID:        req.ValidatorID,
		Disabled:           req.Disabled,
	}
	f.mu.Lock()
	f.lastCreate = req
	f.sets[set.ID] = set
	f.mu.Unlock()

	result := service.CreateBackupSetResult{Set: set}
	if req.RunImmediately && !req.Disabled {
		op, err := f.SubmitRunCycle(ctx, service.RunCycleRequest{
			IdempotencyKey: "create:" + set.ID,
			Actor:          req.Actor,
			ConfigRevision: f.ConfigRevision(),
		})
		if err != nil {
			return result, err
		}
		result.Operation = &op
	}
	return result, nil
}

func (f *backupSetFakeBackend) ImportSSHKey(_ context.Context, raw []byte) (service.SSHKeyRef, error) {
	if f.errOnImport != nil {
		return service.SSHKeyRef{}, f.errOnImport
	}
	if len(raw) == 0 {
		return service.SSHKeyRef{}, service.ErrInvalidRequest
	}
	ref := service.SSHKeyRef{ID: "key_test_1", KeyFile: "/fake/ssh_keys/key_test_1", Algorithm: "ssh-ed25519", Fingerprint: "SHA256:faketestfingerprint"}
	f.mu.Lock()
	f.keys[ref.ID] = ref
	f.mu.Unlock()
	return ref, nil
}

func (f *backupSetFakeBackend) ProbeHostKey(context.Context, string, int) (service.HostKeyProbe, error) {
	if f.errOnProbe != nil {
		return service.HostKeyProbe{}, f.errOnProbe
	}
	if f.probeResult.Algorithm == "" {
		return service.HostKeyProbe{
			Algorithm:      "ssh-ed25519",
			Fingerprint:    "SHA256:faketesthostfingerprint",
			KnownHostsLine: "example.internal ssh-ed25519 AAAAfaketest",
		}, nil
	}
	return f.probeResult, nil
}

func (f *backupSetFakeBackend) TestConnection(context.Context, service.ConnectionTestRequest) (service.ConnectionTestResult, error) {
	if f.errOnConnect != nil {
		return service.ConnectionTestResult{}, f.errOnConnect
	}
	return f.connectionResult, nil
}

// defaultRetentionSettings is the resolved FR-18 default chain plus
// FR-19's default protection, spelled exactly the way core/service's own
// Settings reports it for a config file that names neither the tiers list
// nor the three legacy scalars. The fakes below start here so a handler
// test asserts against the real default policy rather than a fixture
// invented to make an assertion pass.
func defaultRetentionSettings() service.RetentionSettings {
	return service.RetentionSettings{
		Timezone:     "UTC",
		WeekStartsOn: "monday",
		Tiers: []service.RetentionTier{
			{Name: "daily", Granularity: service.GranularityDay, Keep: 7},
			{Name: "weekly", Granularity: service.GranularityWeek, Keep: 3, WindowUnit: service.GranularityMonth},
			{Name: "monthly", Granularity: service.GranularityMonth, Keep: 12},
		},
		ProtectLastKnownGood: true,
	}
}

// Settings and UpdateSettings on syncFakeBackend keep the seam satisfied
// for every test double built on it (backupSetFakeBackend,
// storageFakeBackend). They are deliberately inert: the tests that
// actually exercise issue #140's settings surface use
// settingsFakeBackend below, which records what crossed the seam.
func (f *syncFakeBackend) Settings(context.Context) (service.Settings, error) {
	return service.Settings{Retention: defaultRetentionSettings()}, nil
}

func (f *syncFakeBackend) UpdateSettings(context.Context, service.UpdateSettingsRequest) (service.Settings, error) {
	return service.Settings{Retention: defaultRetentionSettings()}, nil
}

func (f *asyncFakeBackend) Settings(context.Context) (service.Settings, error) {
	return service.Settings{Retention: defaultRetentionSettings()}, nil
}

func (f *asyncFakeBackend) UpdateSettings(context.Context, service.UpdateSettingsRequest) (service.Settings, error) {
	return service.Settings{Retention: defaultRetentionSettings()}, nil
}

// settingsFakeBackend is a BackupServiceClient double for
// handlers_settings_test.go: an in-memory settings store that applies a
// partial update the same way core/service.UpdateSettings does (only the
// named fields move), and records the exact service.UpdateSettingsRequest
// the handler built so a test can assert on what crossed the HTTP-to-core
// seam rather than only on what came back out of it.
//
// It deliberately does NOT re-implement config.Validate: refusals are
// driven through errOnUpdate, because what these tests prove is the HTTP
// layer's request parsing, error mapping and middleware wiring. That the
// validator actually refuses each shape is core/service's own
// settings_test.go, and that the two agree end to end is the CLI boundary
// test (settings_boundary_test.go).
type settingsFakeBackend struct {
	*syncFakeBackend

	settings service.Settings

	lastUpdate  service.UpdateSettingsRequest
	updateCalls int

	errOnRead   error
	errOnUpdate error
}

func newSettingsFakeBackend() *settingsFakeBackend {
	return &settingsFakeBackend{
		syncFakeBackend: newSyncFakeBackend(),
		settings:        service.Settings{Retention: defaultRetentionSettings()},
	}
}

func (f *settingsFakeBackend) Settings(context.Context) (service.Settings, error) {
	if f.errOnRead != nil {
		return service.Settings{}, f.errOnRead
	}
	return f.settings, nil
}

func (f *settingsFakeBackend) UpdateSettings(_ context.Context, req service.UpdateSettingsRequest) (service.Settings, error) {
	f.updateCalls++
	f.lastUpdate = req
	if f.errOnUpdate != nil {
		return service.Settings{}, f.errOnUpdate
	}
	if req.Retention != nil {
		if req.Retention.Timezone != nil {
			f.settings.Retention.Timezone = *req.Retention.Timezone
		}
		if req.Retention.WeekStartsOn != nil {
			f.settings.Retention.WeekStartsOn = *req.Retention.WeekStartsOn
		}
		if len(req.Retention.Tiers) > 0 {
			f.settings.Retention.Tiers = append([]service.RetentionTier(nil), req.Retention.Tiers...)
		}
		if req.Retention.ProtectLastKnownGood != nil {
			f.settings.Retention.ProtectLastKnownGood = *req.Retention.ProtectLastKnownGood
		}
	}
	return f.settings, nil
}
