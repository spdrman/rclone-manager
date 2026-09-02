package app

import (
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// CycleOutcome is the evidence one processing cycle produces about
// whether it did the job it exists to do, in the four numbers a caller
// needs to decide that and in no other form. RunCycle produces one per
// backup set (BackupSetCycleResult.Outcome) and Fetch produces one for the
// single backup set it ran (FetchResult.Outcome), so the two commands
// wrapped around them cannot end up reading different evidence.
//
// # What an exit status is claiming, and why this type exists (issue #361)
//
// A backup manager reporting success on a cycle that backed nothing up is
// close to the worst thing it can do, because every layer above it (a cron
// job, a systemd timer, a monitoring check on exit status) reads that
// claim and stops looking. That happened: discovery could not identify two
// of three remote objects, the third refused its transfer, nothing landed
// on disk, and the process exited 0.
//
// Nothing was wrong with any individual decision that produced it. A
// per-candidate discovery problem is isolated into discovery.Result.Errors
// precisely so one unreadable remote object cannot hide every other
// artifact's result. A transfer that cannot reach the source leaves its
// artifact where it is for the next cycle rather than burning it. Both are
// right on their own. What was missing was anything that looked at the
// conjunction, and the conjunction is the interesting fact: a pass in
// which every artifact either could not be discovered or could not be
// transferred did not succeed, whatever each individual reason was called.
//
// So the rule this type carries (see Failed) is deliberately not "any
// error fails the cycle". A single transient error must not fail a pass
// that otherwise did its work, or a poll interval becomes a pager. The
// rule is about throughput instead: a cycle fails when it had artifacts to
// move and moved none of them to safety.
//
// # Walked and Durable, and why "nothing to do" is not "nothing got done"
//
// This is the distinction the reported bug turns on, and the one an exit
// code has to keep. A cycle with an empty remote and a settled journal did
// exactly what it was asked; a cycle with three artifacts waiting and
// nothing to show for it did not. Both used to look identical from the
// outside, because both committed nothing.
//
// Walked counts every artifact the cycle had a reason to touch, and
// Durable counts how many of those ended it holding a durable local copy,
// which is what a backup is. An idle cycle walks nothing, so the rule
// cannot fire on it. A settled backup set whose artifacts are all COMPLETE
// walks them and counts them all Durable, because their bytes are on disk;
// a cycle that moves nothing because there is nothing to move has not
// failed to deliver anything.
//
// # The daemon and `run` get the same verdict, and use it differently
//
// The verdict is one thing and what a command does with it is another,
// which is why this type answers only the first. `run` is a report: it
// exits non-zero so the cron job that scheduled it learns the truth.
// `daemon` is a service whose entire job is to keep going, so it cannot
// exit on a bad cycle without turning an outage into a second outage; it
// says so loudly in the FR-23 event stream instead (see
// Service.logCycleOutcome) and runs the next cycle. Same evidence, same
// verdict, two honest responses to it.
//
// # Why there is no separate exit code for partial failure
//
// It was tempting, and it is a trap. Exit statuses here are consumed by
// cron, by systemd, by `&&` and by container healthchecks, and every one
// of those reads "not zero" and stops. A wrapper that special-cased a
// third code would be the only thing that noticed it, and a wrapper
// written before the code existed would treat it as a plain failure
// anyway. The gradation belongs in the message and in `status`, which
// have room to say which artifacts got through and which did not; the exit
// status keeps the one bit everything above it can actually act on.
type CycleOutcome struct {
	Set model.BackupSetID

	// Err is the failure that stopped this backup set's processing early
	// rather than affecting one artifact within it: a reconcile or
	// discover call exhausting its retry budget, a journal listing
	// failing outright, or a shutdown mid-cycle.
	//
	// It carries the error rather than a bool because a caller reporting
	// this outcome has to be able to say what happened. A cycle that
	// stopped before the pipeline ran walked nothing and delivered
	// nothing, so its counts are all zero and read exactly like an idle
	// cycle's; the error is the only thing that distinguishes them on an
	// operator's screen.
	Err error

	// FailedArtifacts is how many of the artifacts this cycle walked
	// ended it in FAILED, QUARANTINED or QUARANTINED_LOST (issue #283),
	// whether this cycle's own pipeline put them there or its
	// reconciliation pass found them already rotten.
	FailedArtifacts int

	// Walked is how many artifacts this cycle had a reason to touch:
	// every journal row it drove forward, plus every remote candidate
	// discovery could not even identify, which never became a row at all
	// and would otherwise be invisible here. A candidate that errored on a
	// path one of those rows already covers is counted once, not twice.
	//
	// It is not a count of artifacts that needed work. A settled COMPLETE
	// artifact is walked, finds nothing to do, and is walked all the same,
	// because Walked is the denominator of "how much of what this cycle
	// looked at is safe", and a COMPLETE artifact is safe.
	Walked int

	// Durable is how many of Walked ended the cycle holding a durable
	// local copy: COMMITTED, REMOTE_DELETE_PENDING, REMOTE_RETAINED or
	// COMPLETE. Those are exactly the states in which the bytes are on
	// local disk, verified and fsynced (see internal/lifecycle's state
	// doc); everything before COMMITTED is either still in flight or not
	// yet proven, and everything else is a failure state.
	Durable int
}

// Failed is the one decision `run`, `fetch` and `daemon` all reach through
// (issue #361, extending issue #283's own seam). See CycleOutcome's doc for
// the reasoning behind each of the three clauses; in short, a systemic
// failure or an artifact left in a failure state has always counted, and
// the third clause is the one this issue added: a cycle that had artifacts
// to move and moved none of them to safety did not succeed, however
// forgivable each individual reason was.
//
// The Walked > 0 guard is what keeps an idle cycle out of it. That is not
// a detail, it is the whole reason this can ship: a daemon polling an
// empty remote every fifteen minutes must stay silent, and a rule that
// only asked "did anything commit" would page on every one of those.
func (o CycleOutcome) Failed() bool {
	return o.Err != nil || o.FailedArtifacts > 0 || (o.Walked > 0 && o.Durable == 0)
}

// NothingGotThrough reports the specific shape issue #361 was filed for,
// as opposed to the two failure shapes that already had a verdict. A
// caller uses it to say which of the two happened rather than printing one
// undifferentiated failure for both.
func (o CycleOutcome) NothingGotThrough() bool {
	return o.Err == nil && o.Walked > 0 && o.Durable == 0
}

// Summary is the sentence a command prints when it refuses to call a cycle
// successful, in the terms issue #361 asks for: how many artifacts this
// cycle walked and how many of them got through. The counts come from the
// same struct the exit status came from, so the number an operator reads
// and the verdict they got cannot drift apart.
func (o CycleOutcome) Summary() string {
	noun := "artifacts"
	if o.Walked == 1 {
		noun = "artifact"
	}
	return fmt.Sprintf("%s: %d %s walked, %d through to a durable local copy, %d left in a failure state",
		o.Set, o.Walked, noun, o.Durable, o.FailedArtifacts)
}

// artifactTally is what one walk over a backup set's journal rows tells
// the cycle around it, in the three numbers CycleOutcome needs from it.
// processArtifacts fills it in; nothing else builds one.
type artifactTally struct {
	Walked  int
	Durable int
	Failed  int
}

// count folds one artifact's final state, as the journal itself holds it,
// into the tally.
func (t *artifactTally) count(final lifecycle.State) {
	t.Walked++
	switch final {
	case lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.RemoteRetained, lifecycle.Complete:
		t.Durable++
	case lifecycle.Failed, lifecycle.Quarantined, lifecycle.QuarantinedLost:
		t.Failed++
	}
}
