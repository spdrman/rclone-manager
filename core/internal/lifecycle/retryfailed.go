// This file is the first of FAILED's two declared exits, and until it
// existed neither one had ever been taken (issue #419).
//
// machine.go has said since it was written that "FAILED has two exits:
// back to DISCOVERED (the retry policy restarts the artifact from scratch)
// or into QUARANTINED (the retry budget is exhausted and this needs a
// human instead of another attempt)". Both edges are in the Transitions
// table. Nothing called either, so an artifact that reached FAILED stopped
// being worked on permanently, and RetryQuarantinedIngestion, the one
// action shaped like a recovery, refuses anything that is not QUARANTINED.
// A state an operator cannot get out of is worse than a loud refusal.
//
// # Why the operator asks, and nothing asks on their behalf
//
// The automatic half of FR-22's retry policy is deliberately still
// unbuilt, and this file is not a step towards building it quietly.
//
// A cycle that sent every FAILED artifact back to DISCOVERED on its own
// would re-run the transfer, which for most artifacts means downloading
// gigabytes again, for a cause nothing has classified. Some of those
// causes clear on their own (a source that was unreachable) and some
// cannot (a backend that cannot serve the configured hash policy), and
// there is nothing durable on a FAILED row that tells the two apart:
// FR-10 gives FAILED no category field, only a free-text detail that
// invariant 12 forbids anything from parsing. Spending a re-transfer on a
// guess is exactly the kind of cost this product refuses to take on its
// own, so the eligibility rule is a person: somebody looked at the reason
// and decided another attempt is worth it.
//
// # Every lineage is offered the exit, and that is not laziness
//
// FAILED is reachable from DISCOVERED, TRANSFERRING, TRANSFERRED,
// VERIFYING, VERIFIED and COMMITTING, and this serves all six. The
// argument is structural rather than case by case, and machine.go already
// makes it: FAILED can only be reached BEFORE COMMITTED, which means the
// remote delete has never been issued and the source is presumptively
// still there to recover from. This edge touches no remote object and
// removes no durable copy; the only thing the transfer step deletes on the
// way past is the .partial a previous attempt left, which it clears on
// every attempt anyway.
//
// Two lineages are worth a word because they look like they should be
// special and are not:
//
//   - A final-name collision (FR-12) refuses again unless the operator
//     moved the file, and it refuses CHEAPLY: Transfer runs its collision
//     guard before it copies a byte. So the retry is exactly the shape of
//     "I dealt with the file, try again" and costs nothing when they have
//     not.
//   - A hash policy the backend cannot serve is a fixed property of the
//     deployment, so a retry against an unchanged configuration
//     re-downloads the artifact and reaches the identical verdict. That is
//     the one lineage where the retry has a real cost and a knowably zero
//     chance of a different answer. It is still not refused, because after
//     an operator changes validation.hash this edge is precisely the
//     mechanism that makes the change take effect on the artifact already
//     sitting in FAILED. What the layers above owe them is to SAY so
//     before they spend the download, not to decide for them.
//
// What IS refused lives one layer up, in internal/app, and it is the same
// refusal RetryQuarantinedIngestion already makes (issue #391): an
// artifact whose backup set the configuration no longer names. A row sent
// to DISCOVERED under a set no cycle walks is a row nothing will ever pick
// up again, no longer FAILED so off that list, and unreachable by every
// recovery path there is.
package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// keyRetryFailedSuffix is appended to RetryFailedParams.AttemptKey to
// build RetryFailed's one journal write, the same one-fixed-suffix
// convention ReleaseFromQuarantine uses since a retry only ever makes a
// single durable write per call.
const keyRetryFailedSuffix = ":retry-failed"

// RetryFailedParams is what RetryFailed needs beyond Deps.
type RetryFailedParams struct {
	// Artifact identifies the journal row to retry. It must currently be
	// FAILED; anything else is refused (see NotFailedError).
	Artifact model.ArtifactID

	// AttemptKey is this retry's idempotency key base, exactly like
	// QuarantineReleaseParams.AttemptKey: the same value across a
	// crash-and-resume, a new one for a genuinely new decision.
	AttemptKey string

	// RecoveringFrom is the failure this attempt is being started in spite
	// of, recorded on the row as state.Record.LastError so a later look at
	// the artifact still carries what the last attempt hit.
	//
	// It is passed IN rather than derived here, and that is deliberate.
	// FR-10 gives FAILED no reason field: the sentence lives only in the
	// journal's state_transitions log, which internal/state owns and this
	// package's Journal interface does not expose. internal/app already
	// reads it (GetArtifactDetail's FailureReason, issue #284), so the
	// caller that has it hands it over rather than this package growing a
	// second read path, or worse, a reconstruction like QuarantineReason
	// that would say something less true.
	//
	// Empty is legitimate: a FAILED transition recorded with no detail has
	// nothing to carry forward.
	RecoveringFrom string

	// Note is an optional operator-supplied explanation for why this retry
	// is happening ("replaced the failing credential", "the NAS came
	// back"). It is folded into the recorded Detail so a later failure of
	// the same artifact carries the context of what was tried last time,
	// not just that something was.
	Note string
}

// NotFailedError reports that RetryFailed was asked to retry an artifact
// whose journal state is not currently FAILED.
//
// Its own type, like NotQuarantinedError, because the API layer turns it
// into a named refusal rather than a 500: asking to retry an artifact that
// is quietly making progress, or one a human has already quarantined, is
// something an operator does with a stale screen in front of them, not a
// bug.
type NotFailedError struct {
	Artifact model.ArtifactID
	Current  State
}

func (e *NotFailedError) Error() string {
	return fmt.Sprintf("lifecycle: refusing to retry %s: its journal state is %s, not %s", e.Artifact, e.Current, Failed)
}

// AsNotFailed reports whether err is, or wraps, a *NotFailedError.
func AsNotFailed(err error) (*NotFailedError, bool) {
	var e *NotFailedError
	return e, errors.As(err, &e)
}

// RetryFailed puts one FAILED artifact back into DISCOVERED so the
// ordinary pipeline attempts it again from the start.
//
// It is the FAILED counterpart of ReleaseFromQuarantine and shares its
// bookkeeping deliberately: every retry increments the record's RetryCount
// (state.RetryUpdate), which is the same counter a quarantine release and
// a stalled verification move, because all three answer the same question.
// How many attempts has this artifact already spent from an exceptional
// state. internal/quarantine reports it under that combined meaning.
//
// See this file's package doc for why the operator asks rather than a
// cycle, and for why every lineage into FAILED is offered this exit.
func RetryFailed(ctx context.Context, d Deps, p RetryFailedParams) (state.Outcome, error) {
	if d.Journal == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: RetryFailed needs a Journal")
	}
	if p.AttemptKey == "" {
		return state.Outcome{}, fmt.Errorf("lifecycle: RetryFailed needs an AttemptKey")
	}
	if err := ctx.Err(); err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: RetryFailed: %w", err)
	}

	rec, err := d.Journal.Get(ctx, p.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: RetryFailed: looking up %s: %w", p.Artifact, err)
	}
	if State(rec.State) != Failed {
		return state.Outcome{}, &NotFailedError{Artifact: p.Artifact, Current: State(rec.State)}
	}

	detail := "operator-triggered retry from " + string(Failed) + ", attempt " + fmt.Sprint(rec.RetryCount+1)
	if p.Note != "" {
		detail += ": " + p.Note
	}

	out, err := Advance(ctx, d, state.Transition{
		Artifact: p.Artifact,
		Key:      p.AttemptKey + keyRetryFailedSuffix,
		From:     string(Failed),
		To:       string(Discovered),
		Detail:   detail,
		Retry:    &state.RetryUpdate{Count: rec.RetryCount + 1, LastError: p.RecoveringFrom},
	})
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: RetryFailed: recording the retry for %s: %w", p.Artifact, err)
	}
	return out, nil
}
