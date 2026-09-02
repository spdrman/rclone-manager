package app

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
// out to do actually happened.
//
// Walked counts everything the cycle was still trying to turn into a
// durable local backup: every journal row that was in one of the
// acquisition states (see acquiring, pipeline.go) when the walk reached
// it, plus every candidate discovery could not take in at all. It
// deliberately does not count a row with nothing left to do (COMPLETE,
// or a COMMITTED row whose remote cleanup is being refused by a hardened
// source, which is the documented steady state for those hosts and not a
// backup failure), because "nothing was waiting" and "nothing got
// through" are different outcomes and only one of them is a problem.
//
// Advanced counts how many of those actually moved forward. An artifact
// that moved straight into FAILED or QUARANTINED did not advance: it is
// counted as a failure by FailedArtifacts instead, and counting it here
// as well would let a cycle in which every artifact failed describe
// itself as one where everything got through.
type CycleProgress struct {
	Walked   int
	Advanced int
}

// NothingGotThrough is issue #361's condition: this cycle had work in
// front of it and none of that work landed. It is deliberately not
// "Advanced == 0", because a cycle with nothing waiting on the remote and
// nothing in flight advances nothing either, and that is a healthy quiet
// night rather than a failed backup.
func (p CycleProgress) NothingGotThrough() bool {
	return p.Walked > 0 && p.Advanced == 0
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
