package app

import (
	"context"
	"errors"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/reconcile"
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
//
// FailedArtifacts is the other half of "did this cycle fail" (issue
// #283): how many of the artifacts this call itself walked through
// processArtifacts ended in FAILED, QUARANTINED or QUARANTINED_LOST. Err
// alone used to be the only thing a caller checked, which is exactly how
// a cycle where every artifact discovered fine and then failed
// verification could report success: nothing about that outcome is a
// systemic error, so Err stayed nil. This also covers a loss this
// cycle's own reconcile pass discovered on its own -- a previously-
// durable artifact whose local copy turned out corrupted or missing --
// since processArtifacts lists the journal after reconcileOne has
// already written that verdict (see processArtifacts's own doc); a
// successful reconciliation pass that finds rot is not a systemic
// failure either, but it must still make this cycle count as failed.
type BackupSetCycleResult struct {
	Set             model.BackupSetID
	Reconcile       reconcile.Report
	Discovery       discovery.Result
	Retention       RetentionSetReport
	Err             error
	FailedArtifacts int

	// Progress is issue #361's count of what this cycle actually
	// achieved for this backup set (see CycleProgress): how much work
	// was in front of it, and how much of that moved.
	Progress CycleProgress
}

// SystemicFailure is the question every consumer of Err is actually
// asking: did this backup set's pass FAIL, as opposed to having been
// stopped on purpose.
//
// The distinction exists because issue #350's edit hold gave this report
// a second reason to end a set early. Cancelling a pass because an
// operator pressed Edit is something this manager was asked to do, and a
// report that spells it the same way it spells a source that has gone
// unreachable makes an ordinary edit look like a backup that broke:
// core/service would fail the operation the operator submitted,
// `backup-manager run` would exit 1, and the activity feed would carry
// "context canceled" as the reason a backup did not happen. In a product
// whose whole job is to be believed about backups, a false alarm is not
// a cosmetic defect.
//
// It says nothing about FailedArtifacts, which is the other half of "did
// this cycle fail" (see this type's own doc) and is counted the same way
// however the pass ended. A stopped pass leaves its in-flight artifact
// pre-durable rather than FAILED, so that half comes out zero on its
// own; this method deliberately does not reach over and decide it.
func (r BackupSetCycleResult) SystemicFailure() bool {
	return r.Err != nil && !errors.Is(r.Err, ErrBackupSetHeldForEditing)
}

// StoppedForEditing is the positive half of the same distinction: this
// pass ended early because an operator took an edit hold on the set.
//
// SystemicFailure being false is not enough to conclude it, because a
// pass that simply finished has no error at all, and the two have to be
// told apart by anything that reasons about how much of the set's work
// actually happened. CycleVerdict.NothingGotThrough is the one that
// matters: a pass stopped mid-walk has rows counted and none of them
// through, so on the arithmetic alone it looks exactly like a set that
// backed nothing up.
func (r BackupSetCycleResult) StoppedForEditing() bool {
	return errors.Is(r.Err, ErrBackupSetHeldForEditing)
}

// CycleReport is what RunCycle returns: one BackupSetCycleResult per
// configured backup set this cycle reached, in config order, plus the
// cycle's own timing.
type CycleReport struct {
	StartedAt time.Time
	Duration  time.Duration
	Sets      []BackupSetCycleResult

	// Moves is what FR-30's move engine did with the homes this cycle's
	// retention passes worked out (EPIC E FR-27/FR-30, issue #239). It is
	// one report for the whole cycle rather than one per backup set,
	// because max_moves_per_cycle is a deployment-wide bound and the
	// engine resumes every non-terminal move in the journal, which is not
	// a per-set list either. See movecycle.go.
	//
	// The zero value is what a deployment with no storage medium gets,
	// which is every deployment before this EPIC.
	Moves placement.CycleReport

	// MovesErr is set when this deployment DECLARES a storage medium and
	// the move pass could not run at all: no way to reach a medium, or a
	// journal that cannot record a move.
	//
	// It is its own field rather than folded into a set's Err because it
	// belongs to no set: it is a deployment-level misconfiguration, and
	// attributing it to whichever backup set happened to be first would
	// send an operator to edit the wrong thing. It never fails the cycle's
	// backup work, which has already happened by the time it is set.
	MovesErr error
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

	// Live progress, for a caller that installed an observer (progress.go).
	// It has to be attached before the loop starts, so the first thing an
	// observer hears is the cycle beginning rather than the first artifact
	// it happens to reach, and it carries the one denominator this feed
	// reports: how many backup sets this cycle will visit, which is known
	// now because it is a count of the configuration snapshot the cycle
	// started with. Reassigning ctx keeps cancellation flowing exactly as
	// before; this only adds a value to it.
	ctx = beginCycle(ctx, s.enabledBackupSetCount())

	report := CycleReport{StartedAt: start}

sourcesLoop:
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if ctx.Err() != nil {
				break sourcesLoop
			}
			// A backup set saved "disabled" (issue #146's "Save disabled"
			// wizard tier, config.BackupSet.Disabled) is skipped entirely:
			// no reconcile, no discovery, nothing that would touch its
			// journal rows or its remote. It stays configured (visible to
			// `sources`/GET /backup-sets) without ever being acted on,
			// until an operator re-enables it.
			if bs.Disabled {
				continue
			}
			// A backup set held for editing (issue #350) is skipped for
			// the same reason and in the same place a disabled one is:
			// starting a pass against a definition an operator is
			// currently changing is two writers on one set, and stopping
			// the in-flight run while leaving the poll interval free
			// would be the same race with extra steps. Unlike Disabled
			// this is not persisted anywhere; see holds.go.
			if holds := BackupSetHoldsFrom(ctx); holds != nil && holds.Held(bs.ID.String()) {
				continue
			}
			report.Sets = append(report.Sets, s.processBackupSet(ctx, src, bs))
		}
	}

	// FR-30's moves, after every backup set's own pass and before anything
	// that reads the cycle's state (issue #239). It runs here, once,
	// rather than inside processBackupSet, because the bound it honours is
	// a deployment-wide one and the engine resumes moves that are not this
	// set's; see movecycle.go's own doc for both halves.
	//
	// It is deliberately AFTER the retention previews it is driven from:
	// FR-30's own sentence is "after a retention pass computes each
	// artifact's home medium, the engine plans moves", and the plans below
	// are read off the reports those passes already produced rather than
	// re-derived, so nothing here decides against a journal the cycle has
	// since changed.
	if ctx.Err() == nil {
		var plans []placement.Plan
		for _, set := range report.Sets {
			plans = append(plans, HomeMovePlans(set.Retention.HomePlan)...)
		}
		moves, err := s.RunHomeMoves(ctx, plans)
		report.Moves, report.MovesErr = moves, err
		if err != nil {
			s.logger().Error(ctx, "move", err)
		}
	}

	// Issue #361's verdict, in the event stream, before anything that
	// reads the cycle's state. `run` turns this into an exit status too
	// (cmd/backup-manager/setup.go), but `daemon` has no exit status to
	// turn it into, and a cycle that backed nothing up has to be visible
	// to whatever is shipping these logs either way.
	s.reportBarrenSets(ctx, report)

	// Issue #418: what this deployment is holding outside every
	// configured backup set. It is here rather than only on a screen
	// because `daemon` has no exit status and nobody typing commands at
	// it, and the whole shape of this problem is that it arrives slowly
	// and invisibly. See reportUngoverned (unconfigured.go).
	s.reportUngoverned(ctx)

	// FR-30's half of the same verdict (cycleoutcome.go). It is a separate
	// call under a separate op rather than a branch inside the one above,
	// because a cycle that backed nothing up and a cycle that moved
	// nothing are different problems an operator fixes in different
	// places; see reportBarrenMoves.
	s.reportBarrenMoves(ctx, report)

	// What this cycle learned about which backup sets can be connected to
	// at all, written down before the alert pass so the health report the
	// alert pass then builds already carries it (issue #245). Like
	// alerting, it computes nothing of its own, starts no work, and
	// cannot fail this cycle.
	s.recordConnectionOutcomes(ctx, report)

	// Work Package 3.5's proactive alerting pass, last, so it weighs the
	// journal state this cycle's own work just produced rather than the
	// picture from before it ran. It is a no-op unless an administrator
	// opted in AND a provider app supplied a delivery mechanism (see
	// alerts.go's EnableAlerts), it computes no condition of its own, and
	// it can neither fail this cycle nor start any work: an alert is a
	// notification, never a trigger for retention or deletion (§71).
	s.evaluateAlerts(ctx, report)

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
	source := sourceFor(s.Config, src, bs)

	// From here on this set runs on its own context, cancelled the moment
	// an edit hold lands on it (issue #350, holds.go). Every ctx.Err()
	// check below and inside processArtifact is already positioned so a
	// cancellation stops the pass at a safe boundary, which is why a hold
	// needs no new stopping mechanism of its own. With no holds registry
	// on ctx this is ctx itself and a no-op cancel.
	ctx, cancelOnHold, stoppedByHold := withHoldCancellation(ctx, bs.ID.String())
	defer cancelOnHold()

	// stopReason turns "this context is done" into the reason it is done,
	// so a pass an operator stopped is not reported in the same words as
	// a pass that failed. See SystemicFailure for why the difference is
	// load bearing rather than cosmetic.
	stopReason := func() error {
		err := ctx.Err()
		if err == nil {
			return nil
		}
		if stoppedByHold() {
			return ErrBackupSetHeldForEditing
		}
		return err
	}

	// Deferred, so a set counts as finished however this returns. A set
	// whose reconcile or discovery failed is still a set this cycle is
	// done with, and leaving it uncounted would freeze "set 2 of 5" for
	// the rest of the run.
	prog := progressFrom(ctx)
	prog.enterSet(bs.ID.String())
	defer prog.finishSet()

	if err := stopReason(); err != nil {
		result.Err = err
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

	if err := stopReason(); err != nil {
		result.Err = err
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
	walk := s.processArtifacts(ctx, source, bs, records)
	result.FailedArtifacts = walk.Failed
	result.Progress = foldDiscoveryErrors(walk, discRes)
	if err := stopReason(); err != nil {
		result.Err = err
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
						// The tier name alone, deliberately: this field is
						// a machine-readable label in the FR-23 event
						// stream, and issue #218's placement attribution
						// belongs on the preview an operator reads, not
						// spliced into a log field's value.
						tier = string(v.Tiers[0].Tier)
					}
				}
				s.logger().Retention(ctx, v.Artifact.String(), bs.ID.String(), tier, decision)
			}
		}
	}

	s.recordSuccessfulPoll(bs.ID)
	return result
}

// enabledBackupSetCount is how many backup sets RunCycle will actually
// visit: every configured set except the ones saved disabled, which the
// loop above skips entirely. It is the denominator live progress reports,
// and it counts what the cycle will do rather than what the configuration
// contains, because a count that included a set nothing will process would
// stop one short of finishing every time.
func (s *Service) enabledBackupSetCount() int {
	n := 0
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if !bs.Disabled {
				n++
			}
		}
	}
	return n
}
