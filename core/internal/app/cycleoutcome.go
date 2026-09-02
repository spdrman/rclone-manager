package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/discovery"
)

// This file is issue #361's answer to "did this cycle actually do
// anything": the arithmetic a caller needs to tell a cycle that had
// nothing to do apart from a cycle in which nothing got through.
//
// It is not internal/app/progress.go. That file is the live feed an
// observer subscribes to while a cycle runs (which set, which artifact,
// which stage, right now). This one is a count of what a finished cycle
// achieved, read after the fact, by the two commands that have to turn a
// cycle into an exit status.

// CycleProgress is how much of what one backup set's share of a cycle set
// out to do actually happened: a denominator and a numerator, and the
// whole difficulty of issue #361 is in choosing them.
//
// Walked is the denominator: everything this cycle was still trying to
// turn into a durable local backup. That is every journal row in one of
// the acquisition states (see acquiring, pipeline.go) when the walk
// reached it, plus every candidate discovery could not take in at all,
// which never became a row and would otherwise be invisible here. A
// candidate that errored on a path a walked row already covers is counted
// once.
//
// What it deliberately leaves out is the point. A COMPLETE row is a
// finished backup and is not counted at all, so a set with history cannot
// mask a cycle in which nothing new got through. A COMMITTED row whose
// remote cleanup keeps being refused is not counted either: those bytes
// are already durable, and FR-16's identity re-check refusing the delete
// is the documented steady state against a hardened source rather than a
// backup that did not happen.
//
// Durable is the numerator: how many of Walked ended the cycle holding a
// durable local copy (COMMITTED, REMOTE_DELETE_PENDING, REMOTE_RETAINED
// or COMPLETE), which is what a backup actually is. Asking that rather
// than "did the state change" is what keeps an artifact that moved
// straight into FAILED out of the numerator, and what keeps an artifact
// that transferred but could not be committed out of it too: it moved,
// and it is not a backup yet.
type CycleProgress struct {
	Walked  int
	Durable int
}

// NothingGotThrough is issue #361's arithmetic: this cycle had work in
// front of it and none of that work landed. It is deliberately not
// "Durable == 0", because a cycle with nothing waiting on the remote and
// nothing in flight delivers nothing either, and that is a healthy quiet
// night rather than a failed backup. See CycleVerdict.NothingGotThrough
// for the one case where this arithmetic is true and still the wrong
// thing to say.
func (p CycleProgress) NothingGotThrough() bool {
	return p.Walked > 0 && p.Durable == 0
}

// CycleVerdict is everything the decision "did this backup set's share of
// this cycle fail" is made of. `run` and `fetch` both build one of these
// from their own result type and hand it to the same decision function
// (cycleFailed, core/cmd/backup-manager/setup.go), which is what stops
// the two commands drifting into two definitions of a failed cycle that
// merely happen to agree today (issue #283, and issue #361 where they
// turned out not to agree at all).
type CycleVerdict struct {
	// Set names the backup set, for the reason a non-zero exit prints.
	Set string

	// Systemic is a failure that stopped this backup set's processing
	// early: a reconcile or discover call exhausting its retry budget, a
	// journal listing failing outright, or a shutdown mid-cycle.
	Systemic bool

	// ReconcileErrors is how many of this backup set's already-managed
	// artifacts reconciliation could not reach a verdict on. Unlike a
	// discovery error, this is about an artifact already under
	// management whose integrity could not be established, which is why
	// it fails a cycle on its own rather than only counting toward
	// Progress.
	ReconcileErrors int

	// FailedArtifacts is how many artifacts this cycle walked ended in
	// FAILED, QUARANTINED or QUARANTINED_LOST (issue #283).
	FailedArtifacts int

	// Progress is issue #361's "did anything actually get through".
	Progress CycleProgress
}

// NothingGotThrough is the specific shape issue #361 was filed for, as
// opposed to the other three things that can fail a cycle, so a caller can
// say which of them happened rather than printing one undifferentiated
// failure for all of them.
//
// A cycle that stopped early is excluded even though its arithmetic
// qualifies, because it qualifies vacuously: it walked nothing because it
// never reached its pipeline, not because nothing got through. Saying
// "nothing got through" there would put an invented cause in front of an
// operator who has a real one sitting in the log.
func (v CycleVerdict) NothingGotThrough() bool {
	return !v.Systemic && v.Progress.NothingGotThrough()
}

// Verdict is this cycle result's share of the exit-status decision. See
// CycleVerdict.
func (r BackupSetCycleResult) Verdict() CycleVerdict {
	return CycleVerdict{
		Set:             r.Set.String(),
		Systemic:        r.Err != nil,
		ReconcileErrors: len(r.Reconcile.Errors),
		FailedArtifacts: r.FailedArtifacts,
		Progress:        r.Progress,
	}
}

// Verdict is this fetch result's share of the exit-status decision, built
// exactly as RunCycle's is so `fetch` and `run` cannot disagree. Systemic
// is always false here because Fetch reports a systemic failure by
// returning an error, which its caller has already acted on by the time
// it asks for a verdict.
func (r FetchResult) Verdict() CycleVerdict {
	return CycleVerdict{
		Set:             r.Set.String(),
		Systemic:        false,
		ReconcileErrors: len(r.Reconcile.Errors),
		FailedArtifacts: r.FailedArtifacts,
		Progress:        r.Progress,
	}
}

// foldDiscoveryErrors is the one place a backup set's CycleProgress is
// assembled, from the journal rows a walk drove forward and the candidates
// discovery could not take in at all. RunCycle and Fetch both call it
// rather than each adding up its own, which is what stops the two
// commands reporting different numbers for the same cycle (issue #361).
//
// A candidate whose remote path a walked row already covers is not counted
// again: it is the same object, seen twice by two different parts of the
// same pass.
func foldDiscoveryErrors(walk artifactWalk, discovered discovery.Result) CycleProgress {
	progress := walk.Progress
	for _, e := range discovered.Errors {
		if walk.coveredPaths[e.RemotePath] {
			continue
		}
		progress.Walked++
	}
	return progress
}

// reportBarrenSets writes issue #361's verdict into the FR-23 event
// stream, for every backup set in this cycle that had artifacts in front
// of it and got none of them through.
//
// It exists because `run` is not the only thing that runs a cycle. `run`
// is a report and can answer with an exit status; `daemon` is a service
// whose whole job is to keep going, so it cannot exit on a bad cycle
// without turning one outage into two. Putting the evidence in the stream
// both of them already write means the daemon says the same thing `run`
// says, to whatever is shipping those logs, and keeps running.
//
// It reports nothing else. A systemic failure and a failed artifact are
// both already in the stream from where they happened, and repeating them
// here would double-count them on an operator's screen.
func (s *Service) reportBarrenSets(ctx context.Context, report CycleReport) {
	for _, set := range report.Sets {
		s.reportBarrenSet(ctx, set.Verdict())
	}
}

// reportBarrenSet is the one-backup-set form, so an on-demand Fetch puts
// the same fact in the same stream a scheduled cycle does. A manual fetch
// prints to a terminal as well, but the stream is what a log shipper
// reads, and it should not depend on which command produced the cycle.
func (s *Service) reportBarrenSet(ctx context.Context, v CycleVerdict) {
	if !v.NothingGotThrough() {
		return
	}
	s.logger().Error(ctx, "cycle", fmt.Errorf("%s backed nothing up this cycle: %d walked, %d got through",
		v.Set, v.Progress.Walked, v.Progress.Durable))
}
