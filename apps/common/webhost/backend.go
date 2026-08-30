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
}

var _ BackupServiceClient = (*service.BackupService)(nil)
