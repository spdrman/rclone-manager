package app

import (
	"context"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/reconcile"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// ReconcileSetReport pairs one backup set's FR-17 reconciliation Report
// with any systemic error reconciling it hit (Deps incomplete, listing the
// journal failed, or every retry attempt against a Transient transport
// failure was exhausted). Err is nil on a clean pass; Report can still be
// non-empty even when it is not (reconcile.Reconcile isolates per-artifact
// problems into Report.Errors, never aborting the rest of the set over
// one bad artifact).
type ReconcileSetReport struct {
	Set    model.BackupSetID
	Report reconcile.Report
	Err    error
}

// reconcileOne runs internal/reconcile.Reconcile for one backup set,
// retrying the whole call under Service.retryPolicy when it fails with a
// transport.Transient-classified error.
//
// Retrying the whole call, rather than only whatever internal Stat call
// happened to fail, is safe specifically because FR-17 requires
// reconciliation to be idempotent (internal/reconcile's own package doc:
// "Reconciliation SHALL be idempotent") and because a Stat failure inside
// one artifact's reconcileOne never partially mutates that artifact's
// journal row (see internal/reconcile/remote.go's statRemote: any error
// other than a confirmed transport.NotFound propagates as an
// ArtifactError without touching the journal). A transient blip part-way
// through therefore leaves nothing for a full retry to disagree with.
func (s *Service) reconcileOne(ctx context.Context, source transport.Source, set model.BackupSetID) (reconcile.Report, error) {
	deps := reconcile.Deps{Journal: s.Journal, Transport: s.Transport, Now: s.Now}
	var rep reconcile.Report
	err := retry.Do(ctx, s.retryPolicy(), nil, func(ctx context.Context) error {
		var err error
		rep, err = reconcile.Reconcile(ctx, deps, source, set)
		return err
	})
	return rep, err
}

// ReconcileAll runs reconcileOne for every configured backup set, in
// config order, isolating each backup set's own systemic error rather than
// aborting the rest (mirroring FR-1's "continue processing unrelated
// sources after one source fails", applied to reconciliation the same way
// cycle.go applies it to the rest of one processing cycle).
func (s *Service) ReconcileAll(ctx context.Context) []ReconcileSetReport {
	out := make([]ReconcileSetReport, 0, len(s.Config.Sources))
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if err := ctx.Err(); err != nil {
				out = append(out, ReconcileSetReport{Set: bs.ID, Err: err})
				return out
			}
			source := sourceFor(src, bs)
			rep, err := s.reconcileOne(ctx, source, bs.ID)
			out = append(out, ReconcileSetReport{Set: bs.ID, Report: rep, Err: err})
		}
	}
	return out
}
