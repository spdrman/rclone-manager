package webhost

import (
	"context"

	"github.com/spdrman/rclone-manager/core/service"
)

// BackupServiceClient is the seam this package talks to core/ through:
// exactly the subset of core/service.BackupService's method set the HTTP
// handlers below need, expressed as an interface so a test can substitute
// a double instead of standing up a real SQLite journal and rclone
// transport for every handler test (see fixtures_test.go's
// syncFakeBackend and asyncFakeBackend).
//
// *service.BackupService satisfies this directly (see the compile-time
// assertion below); production wiring passes one built with
// service.Open.
type BackupServiceClient interface {
	// ConfigRevision reports the configuration revision the backend is
	// currently running, so a client's stale picture of it can be
	// detected (docs/EPIC-B-multi-nas.md §14).
	ConfigRevision() string

	// Ready reports whether this backend completed
	// docs/EPIC-B-multi-nas.md §46.1's startup sequence. It is the fact
	// behind /health/ready and GET /system/version's "ready" field, and
	// §36 makes it the precondition a client checks before a destructive
	// operation — so it is asked of the backend, which knows, rather than
	// inferred in this package from some other value that happens to be
	// non-empty. See core/service.BackupService.Ready.
	Ready() bool

	// SubmitRunCycle persists and starts (or, replaying an idempotency
	// key, returns the already-submitted) run_cycle operation. See
	// core/service.BackupService.SubmitRunCycle's doc for the durability
	// and decoupling contract this package relies on without
	// re-implementing.
	SubmitRunCycle(ctx context.Context, req service.RunCycleRequest) (service.Operation, error)

	// GetOperation returns the current state of a previously submitted
	// operation, for authenticated polling (docs/EPIC-B-multi-nas.md
	// §15.7).
	GetOperation(ctx context.Context, id string) (service.Operation, error)

	// PreviewRetention computes source/set's current FR-18/FR-19/FR-20
	// retention plan and issues it a fresh, immutable plan_id
	// (docs/EPIC-B-multi-nas.md §15.6, issue #96/B3.1). It is read-only:
	// see GET /api/v1/backup-sets/{source}/{set}/retention/preview
	// (handlers_retention.go), which this package never wraps in the
	// destructive-operations gate.
	PreviewRetention(ctx context.Context, source, set string) (service.RetentionPlan, error)

	// ApplyRetentionPlan applies exactly the plan named by
	// req.PlanID, or applies nothing at all and returns
	// service.ErrRetentionPlanStale (docs/EPIC-B-multi-nas.md §15.6's
	// RETENTION_PLAN_STALE contract). See POST /api/v1/backup-sets/
	// {source}/{set}/retention/apply (handlers_retention.go), which — unlike
	// PreviewRetention — this package DOES wrap in the destructive-
	// operations gate: this is the one operation in the whole EPIC that
	// deletes local restore points.
	ApplyRetentionPlan(ctx context.Context, req service.ApplyRetentionRequest) (service.RetentionPlan, error)

	// ListBackupSets and GetBackupSet back GET /api/v1/backup-sets and
	// GET /api/v1/backup-sets/{id} (issue #146/B2.7): read-only, no
	// destructive gate, matching docs/EPIC-B-multi-nas.md §50's
	// read-only bucket ("list sources", "view configuration").
	ListBackupSets(ctx context.Context) ([]service.BackupSet, error)
	GetBackupSet(ctx context.Context, id string) (service.BackupSet, error)

	// CreateBackupSet backs POST /api/v1/backup-sets: the add-backup-set
	// wizard's (#98) three Save buttons, wired for the first time by
	// issue #146. See service.BackupService.CreateBackupSet's own doc
	// for the persist-then-hot-reload sequence this method performs.
	CreateBackupSet(ctx context.Context, req service.CreateBackupSetRequest) (service.CreateBackupSetResult, error)

	// ImportSSHKey backs POST /api/v1/ssh-keys: the wizard's "Import
	// key" step, persisting client-validated key material server-side
	// for the first time (issue #146).
	ImportSSHKey(ctx context.Context, raw []byte) (service.SSHKeyRef, error)

	// ProbeHostKey backs POST /api/v1/ssh/host-key-probe: the wizard's
	// "Verify server" step, fetching a real fingerprint instead of a
	// mock (issue #146). Read-only, per §50.
	ProbeHostKey(ctx context.Context, host string, port int) (service.HostKeyProbe, error)

	// TestConnection backs POST /api/v1/backup-sets/test-connection: a
	// pre-save, non-destructive reachability/auth check against a
	// candidate source (issue #146). Read-only, per §50 ("test SSH").
	TestConnection(ctx context.Context, req service.ConnectionTestRequest) (service.ConnectionTestResult, error)

	// ListStorageStatus backs GET /api/v1/system/storage: the FR-21
	// capacity assessment for every configured backup set's local
	// destination (docs/EPIC-B-multi-nas.md §56's Storage UX), issue #104
	// (B3.4). Read-only, per §50 ("view configuration"/health-adjacent
	// surface) — see core/service.BackupService.ListStorageStatus and
	// this package's own handlers_storage.go for why nothing reachable
	// from here can turn a "critical" result into a deletion or a call
	// into internal/retention's apply path.
	ListStorageStatus(ctx context.Context) ([]service.StorageStatus, error)
}

var _ BackupServiceClient = (*service.BackupService)(nil)
