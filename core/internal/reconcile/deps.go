package reconcile

import (
	"context"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Journal is the slice of internal/state that reconciliation needs: the
// two methods every lifecycle.Advance call already requires, plus
// ListByBackupSet to enumerate what to reconcile in the first place.
//
// I made this an interface, embedding lifecycle.Journal rather than
// depending on a concrete *state.Journal, for the same reason
// internal/lifecycle.Journal and internal/discovery.Deps.Journal already
// are: a test can substitute a fake without standing up SQLite, and this
// package cannot reach past this surface into migrations or schema
// concerns it does not own.
type Journal interface {
	lifecycle.Journal
	ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]state.Record, error)
}

// Deps is what Reconcile is handed. I mirrored lifecycle.Deps and
// discovery.Deps on purpose: the same field names, the same
// nil-means-time.Now clock convention, and Reconcile forwards this
// straight into lifecycle.Advance through lifecycleDeps below, so a test
// can build one Deps value and reuse it against both this package's
// assertions and any lifecycle-level ones in the same table.
type Deps struct {
	Journal   Journal
	Transport transport.Transport

	// Now is injectable so a test can control both what a reconciled
	// transition's OccurredAt is stamped with and the Deletion.DeletedAt
	// value reconciling to COMPLETE records. Nil means time.Now.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now().UTC()
}

// lifecycleDeps adapts Deps into the lifecycle.Deps shape Advance needs.
// d.Journal satisfies lifecycle.Journal structurally (it is a superset),
// so this is just a field-for-field copy, never a fresh interface value
// wrapping anything.
func (d Deps) lifecycleDeps() lifecycle.Deps {
	return lifecycle.Deps{Journal: d.Journal, Transport: d.Transport, Now: d.Now}
}
