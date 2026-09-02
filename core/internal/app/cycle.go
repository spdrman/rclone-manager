package app

import (
	"context"
	"errors"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/reconcile"
	"github.com/spdrman/rclone-manager/core/internal/state"
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

	// Walked and Durable are issue #361's half of "did this cycle do its
	// job": how many artifacts this pass had a reason to touch, and how
	// many of those ended it with their bytes on local disk. FailedArtifacts
	// alone cannot answer that, because the interesting failure is the one
	// where nothing failed and nothing got through either: every candidate
	// refused before discovery could identify it, and the one that was
	// identified refused its transfer, leaving it pre-durable for a later
	// cycle exactly as a transient error should. See CycleOutcome
	// (outcome.go) for what the two numbers mean in detail and for the rule
	// they feed.
	Walked  int
	Durable int
}

// Outcome is the evidence this backup set's cycle produced about whether
// it succeeded, in the one shape every caller decides from. Fetch has an
// identical method (fetch.go) so `run` and `fetch` cannot end up reading
// different evidence about the same cycle.
//
// The discovery candidates that errored are folded in here rather than in
// processArtifacts, because they are the artifacts that never got as far
// as being a journal row: a source that answers a listing and then refuses
// every per-object connection produces a pass where nothing was walked at
// all by the pipeline, and the only trace of the three artifacts that
// should have been backed up is in this list. A candidate whose path one
// of the walked rows already covers is not counted twice.
func (r BackupSetCycleResult) Outcome() CycleOutcome {
	return CycleOutcome{
		Set:             r.Set,
		SystemicFailure: r.Err != nil,
		FailedArtifacts: r.FailedArtifacts,
		Walked:          r.Walked,
		Durable:         r.Durable,
	}
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
			report.Sets = append(report.Sets, s.processBackupSet(ctx, src, bs))
		}
	}

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

	// Deferred, so a set counts as finished however this returns. A set
	// whose reconcile or discovery failed is still a set this cycle is
	// done with, and leaving it uncounted would freeze "set 2 of 5" for
	// the rest of the run.
	prog := progressFrom(ctx)
	prog.enterSet(bs.ID.String())
	defer prog.finishSet()

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
	tally := s.processArtifacts(ctx, source, bs, records)
	result.FailedArtifacts = tally.Failed
	result.Walked = tally.Walked + undiscoverableCandidates(discRes, records)
	result.Durable = tally.Durable
	if ctx.Err() != nil {
		result.Err = ctx.Err()
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
	s.logCycleOutcome(ctx, result)
	return result
}

// undiscoverableCandidates counts the remote objects this pass could not
// identify at all and that no journal row already accounts for.
//
// Both halves matter. A candidate discovery could not stat never became a
// journal row, so without this the pipeline's own walk would report a pass
// over an unreachable source as having touched nothing, which is exactly
// how a cycle that failed to back up three artifacts looked like a cycle
// with nothing to do. And a candidate whose path a walked row already
// covers (a re-discovery of something already in flight) is the same
// artifact seen twice, so counting it again would inflate a number an
// operator is meant to read literally.
func undiscoverableCandidates(res discovery.Result, walked []state.Record) int {
	if len(res.Errors) == 0 {
		return 0
	}
	covered := make(map[string]struct{}, len(walked))
	for _, rec := range walked {
		covered[rec.RemotePath] = struct{}{}
	}
	n := 0
	for _, e := range res.Errors {
		if _, ok := covered[e.RemotePath]; !ok {
			n++
		}
	}
	return n
}

// logCycleOutcome is how a cycle that got nothing through reaches an
// operator who is not reading an exit status, which is every operator
// running `daemon` (issue #361).
//
// This is the whole of the daemon's answer to the question, and it is a
// deliberate one. `run` is a report and can exit non-zero to tell its cron
// job the truth; a daemon's entire job is to keep going, so exiting would
// turn one bad cycle into an outage of its own. What it owes an operator
// instead is to stop describing a cycle that backed nothing up in the same
// INFO-level "cycle finished" line as one that worked. It is logged at
// ERROR, from the same numbers the exit status is computed from, and then
// the next cycle runs.
//
// A cycle that failed systemically already logged its own error at the
// point of failure, and one with artifacts in a failure state already
// logged each of those, so this says nothing about either: it fires only
// for the shape that had no voice of its own.
func (s *Service) logCycleOutcome(ctx context.Context, result BackupSetCycleResult) {
	outcome := result.Outcome()
	if outcome.SystemicFailure || !outcome.NothingGotThrough() {
		return
	}
	s.logger().Error(ctx, "cycle", errors.New("this cycle backed nothing up: "+outcome.Summary()))
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
