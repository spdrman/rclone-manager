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
}

var _ BackupServiceClient = (*service.BackupService)(nil)
