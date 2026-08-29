package app

import (
	"context"
	"time"

	"github.com/spdrman/rclone-manager/internal/config"
	"github.com/spdrman/rclone-manager/internal/discovery"
	"github.com/spdrman/rclone-manager/internal/model"
	"github.com/spdrman/rclone-manager/internal/reconcile"
)

// BackupSetCycleResult is what one processing cycle did for one backup
// set: FR-17's reconciliation report, FR-8's discovery result, and the
// FR-18/FR-19 retention preview computed at the end, once whatever this
// cycle's own transfer/verify/commit/delete work changed has landed.
//
// Err is set only for a systemic failure that stopped this backup set's
// processing early (a reconcile or discover call exhausting its retry
// budget, or a journal listing failing outright); a per-artifact problem
// never sets it; see processArtifact's own doc for how those are isolated
// instead.
type BackupSetCycleResult struct {
	Set       model.BackupSetID
	Reconcile reconcile.Report
	Discovery discovery.Result
	Retention RetentionSetReport
	Err       error
}

// CycleReport is what RunCycle returns: one BackupSetCycleResult per
// configured backup set this cycle reached, in config order, plus the
// cycle's own timing.
type CycleReport struct {
	StartedAt time.Time
	Duration  time.Duration
	Sets      []BackupSetCycleResult
}

// RunCycle is FR-1's "one processing cycle": the single piece of business
// logic `run` performs once and `daemon` repeats at poll_interval. Both
// commands call exactly this method; neither has, or is allowed to have,
// any cycle logic of its own (see this package's doc and cmd/backup-manager,
// which only wires flags, signals and output formatting around this call).
//
// The cycle order matters and follows the EPIC directly: for each
// configured backup set, in config order,
//
//  1. FR-17 reconcile first, always, before anything else touches this
//     backup set's artifacts;
//  2. FR-8 discover, so any new remote artifact gets journaled as
//     DISCOVERED;
//  3. drive every one of this backup set's in-flight journal rows forward
//     (transfer, verify, commit, delete) as far as processArtifact safely
//     can per artifact, capacity-checked before every transfer;
//  4. an FR-18/FR-19 retention preview, computed last, against whatever
//     this cycle's own work just changed.
//
// # No overlapping processing for the same backup set, structurally
//
// RunCycle is a single, sequential loop: no goroutine, no concurrent
// ticker, nothing that could start a second pass over any backup set
// before this one's loop iteration for it has returned. Daemon (daemon.go)
// never starts a new RunCycle call until the previous one has fully
// returned either. Together that makes "two processing passes over the
// same backup set running at once" impossible within one process, by
// construction, not by a lock this package has to remember to take.
//
// # Continuing after one source fails
//
// Each backup set's own reconcile/discover/process/retention work is
// wrapped in per-set error isolation: a systemic error reconciling or
// discovering one backup set is logged and recorded in that set's own
// BackupSetCycleResult.Err, and the loop moves on to the next configured
// backup set rather than aborting the whole cycle. A per-artifact problem
// is isolated one level deeper still, inside processArtifact itself.
//
// # Shutting down without initiating unsafe source deletion
//
// RunCycle checks ctx before starting each backup set and before starting
// each artifact within it, and every step inside processArtifact repeats
// that check immediately before doing anything (see that function's own
// doc for the specific boundary this package's shutdown-safety proof rests
// on). Once ctx is done, RunCycle stops starting new work and returns
// whatever it has already recorded; nothing left un-started this cycle is
// lost, since every step here is safe to resume on a later cycle.
func (s *Service) RunCycle(ctx context.Context) CycleReport {
	start := s.now()
	cycleID := start.Format(time.RFC3339Nano)
	s.logger().CycleStart(ctx, cycleID)

	report := CycleReport{StartedAt: start}

sourcesLoop:
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if ctx.Err() != nil {
				break sourcesLoop
			}
			report.Sets = append(report.Sets, s.processBackupSet(ctx, src, bs))
		}
	}

	report.Duration = s.now().Sub(start)
	s.logger().CycleEnd(ctx, cycleID, report.Duration, ctx.Err())
	return report
}

// processBackupSet runs one backup set's whole share of RunCycle: FR-17
// reconcile, FR-8 discover, every in-flight artifact driven forward, then
// an FR-18/FR-19 retention preview. See RunCycle's doc for the ordering
// rationale.
func (s *Service) processBackupSet(ctx context.Context, src config.Source, bs config.BackupSet) BackupSetCycleResult {
	result := BackupSetCycleResult{Set: bs.ID}
	source := sourceFor(src, bs)

	if ctx.Err() != nil {
		result.Err = ctx.Err()
		return result
	}
	recRep, err := s.reconcileOne(ctx, source, bs.ID)
	result.Reconcile = recRep
	for _, f := range recRep.Findings {
		if f.Changed() {
			s.logger().Reconciliation(ctx, f.Artifact.String(), string(f.From)+"->"+string(f.To), f.Reason)
		}
	}
	if err != nil {
		s.logger().Error(ctx, "reconcile", err)
		result.Err = err
		return result
	}

	if ctx.Err() != nil {
		result.Err = ctx.Err()
		return result
	}
	discRes, err := s.discoverOne(ctx, source, bs)
	result.Discovery = discRes
	discovered, alreadyKnown, pending, rejected, conflicts, errored := eventDiscoveryCounts(discRes)
	s.logger().Discovery(ctx, bs.ID.String(), discovered, alreadyKnown, pending, rejected, conflicts, errored)
	if err != nil {
		s.logger().Error(ctx, "discover", err)
		result.Err = err
		return result
	}

	records, err := s.Journal.ListByBackupSet(ctx, bs.ID)
	if err != nil {
		s.logger().Error(ctx, "list-artifacts", err)
		result.Err = err
		return result
	}
	for _, rec := range records {
		if ctx.Err() != nil {
			result.Err = ctx.Err()
			break
		}
		s.processArtifact(ctx, source, bs, rec)
	}

	if ctx.Err() == nil {
		retRep, err := s.RetentionPreview(ctx, bs.ID)
		if err != nil {
			s.logger().Error(ctx, "retention", err)
		} else {
			result.Retention = retRep
			for _, v := range retRep.Verdicts {
				decision := "delete"
				tier := ""
				if v.Keep {
					decision = "keep"
					if len(v.Tiers) > 0 {
						tier = string(v.Tiers[0])
					}
				}
				s.logger().Retention(ctx, v.Artifact.String(), bs.ID.String(), tier, decision)
			}
		}
	}

	s.recordSuccessfulPoll(bs.ID)
	return result
}
