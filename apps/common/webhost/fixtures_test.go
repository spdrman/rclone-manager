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
	return nil, nil
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
		Disabled:           req.Disabled,
	}
	f.mu.Lock()
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
