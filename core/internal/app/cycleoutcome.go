package app

import (
	"context"
	"errors"
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
// candidate that errored on a path this backup set already has a journal
// row for, in any state, is not counted at all: the object is already
// under management, and on a read-only set (whose remote objects live
// forever by design, and are therefore re-read every cycle) that is the
// difference between a blip and a failed cycle.
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

	// Stopped is the other way a pass can end early, and it is not a
	// failure: this manager was ASKED to stop, by an operator taking an
	// edit hold on the set (issue #350). It is separate from Systemic
	// rather than folded into it because the two mean opposite things to
	// a reader, and separate from "not failed" because a stopped pass
	// has the arithmetic of a barren one without being one. See
	// NothingGotThrough.
	Stopped bool

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
//
// Stopped is excluded for the same reason and it is the sharper case,
// because a pass held for editing is stopped MID-WALK rather than before
// it: its rows are counted, none of them got through, and the arithmetic
// alone therefore says "backed nothing up this cycle" about a set an
// operator deliberately paused a second ago. That is the false alarm
// issue #350 exists to remove, arriving by a second route.
func (v CycleVerdict) NothingGotThrough() bool {
	return !v.Systemic && !v.Stopped && v.Progress.NothingGotThrough()
}

// Verdict is this cycle result's share of the exit-status decision. See
// CycleVerdict.
func (r BackupSetCycleResult) Verdict() CycleVerdict {
	return CycleVerdict{
		Set:             r.Set.String(),
		Systemic:        r.SystemicFailure(),
		Stopped:         r.StoppedForEditing(),
		ReconcileErrors: len(r.Reconcile.Errors),
		FailedArtifacts: r.FailedArtifacts,
		Progress:        r.Progress,
	}
}

// Verdict is this fetch result's share of the exit-status decision, built
// exactly as RunCycle's is so `fetch` and `run` cannot disagree. Systemic
// is always false here because Fetch reports a systemic failure by
// returning an error, which its caller has already acted on by the time
// it asks for a verdict. Stopped is always false for a narrower reason:
// Fetch does not install a hold watcher at all (see withHoldCancellation,
// whose only caller is processBackupSet), so nothing can stop one of
// these passes on purpose.
func (r FetchResult) Verdict() CycleVerdict {
	return CycleVerdict{
		Set:             r.Set.String(),
		Systemic:        false,
		Stopped:         false,
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
	s.reportEmptyCycle(ctx, report)
	for _, set := range report.Sets {
		s.reportBarrenSet(ctx, set.Verdict())
	}
}

// CycleCoverage is what a cycle had in front of it before it ran: how
// many backup sets exist at all, and how many of those RunCycle was not
// allowed to touch. It is issue #412's half of the same question issue
// #361 asked one level down.
//
// #361 counted artifacts inside a backup set. That count cannot see the
// case where there was no backup set to count artifacts in, because
// reportBarrenSets reaches it by iterating report.Sets and an empty
// report has nothing to iterate. So a deployment with every set switched
// off writes cycle_start and cycle_end and nothing else, every poll
// interval, and reads exactly like a deployment that is working.
//
// Configured counts the configuration snapshot rather than the report,
// which is the only place the difference between "nothing exists" and
// "everything is switched off" is visible at all: both leave report.Sets
// empty, and an operator fixes them by opposite actions (create a backup
// set, or turn one back on).
//
// Disabled and Held are recounted here rather than carried out of
// RunCycle's loop, and Held in particular is read after the fact, so a
// hold placed or released while the cycle ran can leave Disabled+Held
// short of Configured. That is a diagnostic drifting by one during an
// edit, not a verdict changing: what makes the cycle empty is that it
// visited nothing, which the report already settles.
type CycleCoverage struct {
	// Configured is every backup set in the configuration, including the
	// ones this cycle will skip.
	Configured int

	// Disabled is how many of Configured are saved disabled (issue
	// #146's "Save disabled" tier), which RunCycle skips before the set
	// is ever appended to the report.
	Disabled int

	// Held is how many of Configured are held for editing (issue #350),
	// skipped in the same place and for a different reason: nobody
	// switched anything off, an operator is part-way through a change.
	Held int
}

// cycleCoverage counts the configuration this cycle ran against, plus
// whatever holds ctx carries. With no holds registry on ctx, Held is
// zero, exactly as RunCycle's own loop behaves.
func (s *Service) cycleCoverage(ctx context.Context) CycleCoverage {
	var c CycleCoverage
	holds := BackupSetHoldsFrom(ctx)
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			c.Configured++
			switch {
			case bs.Disabled:
				c.Disabled++
			case holds != nil && holds.Held(bs.ID.String()):
				c.Held++
			}
		}
	}
	return c
}

// reportEmptyCycle is issue #412: a cycle whose report covers no backup
// sets is as much a cycle that backed nothing up as one whose sets all
// came back barren, and it has to be as loud, in the same stream, under
// the same op, so a log-shipping rule that already catches the one
// catches the other.
//
// It says which of the two reasons it is, because they are not the same
// problem. Nothing configured is fixed by creating a backup set; every
// set disabled or held is fixed by turning one back on, or by finishing
// the edit. One message covering both would send half its readers to a
// screen with nothing on it.
//
// A cycle whose context is already done is excluded, for the reason
// CycleVerdict.NothingGotThrough excludes a stopped pass: its emptiness
// is vacuous. RunCycle breaks out of its loop before the first set on a
// cancelled context, so the report is empty because the process is
// shutting down, and the shutdown is already in cycle_end one line
// below. Announcing a configuration problem there would invent a second
// cause in front of the real one.
func (s *Service) reportEmptyCycle(ctx context.Context, report CycleReport) {
	if len(report.Sets) > 0 || ctx.Err() != nil {
		return
	}
	c := s.cycleCoverage(ctx)
	if c.Configured == 0 {
		s.logger().Error(ctx, "cycle", errors.New("this cycle backed nothing up: no backup sets are configured"))
		return
	}
	s.logger().Error(ctx, "cycle", fmt.Errorf(
		"this cycle backed nothing up: every configured backup set was skipped (%d configured, %d disabled, %d held for editing)",
		c.Configured, c.Disabled, c.Held))
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
