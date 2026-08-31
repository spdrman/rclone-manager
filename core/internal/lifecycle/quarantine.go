// This file is the operational half of QUARANTINED that FR-10 declares but
// leaves nowhere else in this codebase implemented: machine.go's
// Transitions table allows exactly one exit, QUARANTINED -> DISCOVERED
// (and QUARANTINED_LOST allows none at all), but until this file nothing
// ever actually called it. An operator who has looked at a quarantined
// artifact and decided it deserves a fresh attempt needs a real, tested
// entry point for that decision, not a bare Advance call reconstructed by
// hand at every call site.
//
// # Making repeated quarantine visible
//
// Phase 4 (docs/EPIC.md) asks for more than an exit: "an artifact must not
// be silently retried into oblivion, so repeated quarantine of the same
// artifact should be visible rather than looking like fresh failures each
// time." ReleaseFromQuarantine is where that gets enforced. Every release
// increments the journal record's RetryCount (state.RetryUpdate), so an
// artifact that has been quarantined, released, and quarantined again
// carries a visible, durable count on its own row instead of looking
// indistinguishable from a first-time failure.
//
// RetryCount already exists in the FR-9 schema for exactly this shape of
// bookkeeping ("how many times has this artifact been sent back for a
// fresh attempt"; see state.RetryUpdate's doc), and nothing in this
// codebase writes to it yet: FR-22's own bounded-backoff retry loop, for
// the FAILED -> DISCOVERED exit, has not been built. That is a deliberate
// reuse, not a coincidence: releasing from QUARANTINED and retrying out of
// FAILED are the same underlying question, "how many times has this
// artifact been sent back to try again from an exceptional state",
// answered from two different starting states. When FR-22 lands, its
// FAILED -> DISCOVERED exit is expected to increment this same counter
// rather than invent a second one; internal/quarantine's repeat-visibility
// report reads it with that combined meaning already in mind.
//
// # QUARANTINED_LOST has no release
//
// See machine.go and state.go's package docs for the full reasoning:
// QUARANTINED_LOST means the durable local copy went bad after COMPLETE,
// when the remote source is already confirmed gone. Releasing it back to
// DISCOVERED would ask the pipeline to re-fetch from a source that is not
// there, so ReleaseFromQuarantine refuses it with a distinct, typed error
// (QuarantinedLostIsTerminalError) rather than silently doing nothing or,
// worse, treating it like an ordinary quarantine. A caller that wants to
// tell the two apart programmatically uses AsQuarantinedLostIsTerminal
// rather than comparing error strings.
//
// The refusal is about THIS exit, not about the state being a dead end.
// ReinstateFromQuarantine, further down this file, does serve
// QUARANTINED_LOST: it keeps the local copy instead of re-fetching, which
// is precisely the case a confirmed-gone source leaves available (issue
// #220).
package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// keyQuarantineReleaseSuffix is appended to QuarantineReleaseParams.AttemptKey
// to build ReleaseFromQuarantine's one journal write, the same
// one-fixed-suffix convention Verify uses (keyVerifySuffix) since a release
// only ever makes a single durable write per call.
const keyQuarantineReleaseSuffix = ":quarantine-release"

// QuarantineReleaseParams is what ReleaseFromQuarantine needs beyond Deps.
type QuarantineReleaseParams struct {
	// Artifact identifies the journal row to release. It must currently be
	// QUARANTINED; anything else is refused (see NotQuarantinedError and
	// QuarantinedLostIsTerminalError).
	Artifact model.ArtifactID

	// AttemptKey is this release's idempotency key base, exactly like
	// VerifyParams.AttemptKey: the same value across a crash-and-resume, a
	// new one for a genuinely new release decision.
	AttemptKey string

	// Note is an optional operator-supplied explanation for why this
	// release is happening (for example "replaced the failing validator
	// binary", "confirmed a false positive"). It is folded into the
	// recorded Detail so a later, repeated quarantine of the same artifact
	// carries the context of what was tried last time, not just that
	// something was tried.
	Note string
}

// NotQuarantinedError reports that ReleaseFromQuarantine was asked to
// release an artifact whose journal state is not currently QUARANTINED.
type NotQuarantinedError struct {
	Artifact model.ArtifactID
	Current  State
}

func (e *NotQuarantinedError) Error() string {
	return fmt.Sprintf("lifecycle: refusing this quarantine action on %s: its journal state is %s, not %s", e.Artifact, e.Current, Quarantined)
}

// AsNotQuarantined reports whether err is, or wraps, a *NotQuarantinedError.
func AsNotQuarantined(err error) (*NotQuarantinedError, bool) {
	var e *NotQuarantinedError
	return e, errors.As(err, &e)
}

// QuarantinedLostIsTerminalError reports that ReleaseFromQuarantine was
// asked to release an artifact that is QUARANTINED_LOST, which has no
// release: see this file's package doc and machine.go for why.
type QuarantinedLostIsTerminalError struct {
	Artifact model.ArtifactID
}

func (e *QuarantinedLostIsTerminalError) Error() string {
	return fmt.Sprintf(
		"lifecycle: %s is QUARANTINED_LOST: the only copy was corrupt and the remote source was already confirmed gone before that was found, so there is no copy anywhere left to recover from; this needs operator action outside the state machine (for example restoring from a different backup-set generation), not a release",
		e.Artifact,
	)
}

// AsQuarantinedLostIsTerminal reports whether err is, or wraps, a
// *QuarantinedLostIsTerminalError. A caller that needs to surface
// QUARANTINED_LOST differently from an ordinary, recoverable quarantine
// (Phase 4 requires exactly this) checks for this rather than comparing
// error strings or the record's State field by hand.
func AsQuarantinedLostIsTerminal(err error) (*QuarantinedLostIsTerminalError, bool) {
	var e *QuarantinedLostIsTerminalError
	return e, errors.As(err, &e)
}

// ReleaseFromQuarantine moves p.Artifact from QUARANTINED back to
// DISCOVERED, the one exit machine.go's Transitions table allows, and
// records that this happened in a way a later repeated quarantine of the
// same artifact can be told apart from a first-time failure (see this
// file's package doc).
//
// It refuses, without writing anything, when the artifact is not currently
// QUARANTINED: *NotQuarantinedError for any other ordinary state, or the
// distinct *QuarantinedLostIsTerminalError for QUARANTINED_LOST, which has
// no release at all. Like every other lifecycle step, a non-nil error here
// means an infrastructure problem or a refusal to release; a release that
// succeeds is reported through the returned Outcome, never through error.
func ReleaseFromQuarantine(ctx context.Context, d Deps, p QuarantineReleaseParams) (state.Outcome, error) {
	if d.Journal == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReleaseFromQuarantine needs a Journal")
	}
	if p.AttemptKey == "" {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReleaseFromQuarantine needs an AttemptKey")
	}
	if err := ctx.Err(); err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReleaseFromQuarantine: %w", err)
	}

	rec, err := d.Journal.Get(ctx, p.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReleaseFromQuarantine: looking up %s: %w", p.Artifact, err)
	}

	switch State(rec.State) {
	case Quarantined:
		// proceed below
	case QuarantinedLost:
		return state.Outcome{}, &QuarantinedLostIsTerminalError{Artifact: p.Artifact}
	default:
		return state.Outcome{}, &NotQuarantinedError{Artifact: p.Artifact, Current: State(rec.State)}
	}

	detail := "operator released from quarantine, attempt " + fmt.Sprint(rec.RetryCount+1)
	if p.Note != "" {
		detail += ": " + p.Note
	}

	out, err := Advance(ctx, d, state.Transition{
		Artifact: p.Artifact,
		Key:      p.AttemptKey + keyQuarantineReleaseSuffix,
		From:     string(Quarantined),
		To:       string(Discovered),
		Detail:   detail,
		Retry: &state.RetryUpdate{
			Count:     rec.RetryCount + 1,
			LastError: QuarantineReason(rec),
		},
	})
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReleaseFromQuarantine: recording release for %s: %w", p.Artifact, err)
	}
	return out, nil
}

// QuarantineReason derives a best-effort, human-readable explanation of why
// rec is (or, after a release, was) quarantined, from whatever the record
// itself durably persisted.
//
// It never reaches for the literal transition detail text that produced
// the quarantine: that string lives only in the FR-9 journal's
// state_transitions log (see internal/state/journal.go), which is
// intentionally not part of this package's, or any other package's, read
// surface (internal/state owns that table; nothing outside it queries it
// directly). What this function can say reliably is limited to the two
// shapes the current pipeline actually produces:
//
//   - an application validator's rejection (config.Validation.Command),
//     whose verdict is durably attached to the record itself
//     (ValidationPassed, ValidationDetail) by verify.go; or
//   - anything else capable of routing an artifact into QUARANTINED or
//     QUARANTINED_LOST today: a hash mismatch caught at VERIFYING
//     (verify.go), or the durable local copy found invalid by FR-17
//     reconciliation after COMMITTED, REMOTE_DELETE_PENDING or COMPLETE
//     (internal/reconcile), or Phase 4's own scheduled revalidation
//     (internal/revalidate). None of those three attach a Validation
//     update, and none of them persists their specific free-text reason
//     onto the record either, so the best this function can honestly say
//     is that a content check failed, plus whatever hash was on file at
//     the time, not which check or why.
//
// A caller that needs the exact wording a specific quarantine was recorded
// with has to read the journal's transition log directly (sqlite3 against
// the state database is the documented, supported way; see
// internal/state's package doc on why that table is kept legible).
func QuarantineReason(rec state.Record) string {
	if rec.ValidationPassed != nil && !*rec.ValidationPassed {
		if rec.ValidationDetail != "" {
			return "application validator rejected the artifact: " + rec.ValidationDetail
		}
		return "application validator rejected the artifact"
	}
	if rec.LocalHash != "" {
		return fmt.Sprintf(
			"the durable local copy failed a content check (recorded %s hash: %s); see the journal's transition log for exactly which check caught it and why",
			orUnknownAlg(rec.LocalHashAlg), rec.LocalHash,
		)
	}
	return "content found invalid; no further detail is persisted on this record (see the journal's transition log for the transition that caused it)"
}

func orUnknownAlg(alg string) string {
	if alg == "" {
		return "unknown-algorithm"
	}
	return alg
}

// keyQuarantineReinstateSuffix is appended to
// QuarantineReinstateParams.AttemptKey to build ReinstateFromQuarantine's
// one journal write, the same one-fixed-suffix convention
// keyQuarantineReleaseSuffix above uses for the same reason: a
// reinstatement only ever makes a single durable write per call.
const keyQuarantineReinstateSuffix = ":quarantine-reinstate"

// ReinstatementEvidence is what a caller proved about the durable local
// copy, right now, in the same call that asks for the artifact to be
// trusted again.
//
// The fields are separate verdicts rather than one combined boolean
// because the combined one is not decidable here and, more importantly,
// because a "pass" is not automatically evidence. The local-copy check
// every caller runs is unconditional, so an artifact with no recorded hash
// baseline and no configured validator "passes" on nothing more than the
// file still being present at its recorded path, and re-trusting a backup
// on the strength of "the file exists" is exactly the fail-open FR-13
// exists to prevent, reached through a different door. What counts is a
// check that could actually have failed on content:
//
//   - HashMatched: a hash baseline recorded at VERIFIED existed, the
//     durable local copy was re-hashed now, and the two still agree. This
//     proves the bytes are the bytes this manager itself verified, and it
//     rests on this manager's own journal, not on anything the remote
//     said (FR-8), so it holds under the threat model the rest of the
//     project is built for.
//
//   - ValidatorPassed: the backup set's configured application validator
//     (FR-13's restore-test hook) actually ran in this call and passed.
//     This proves the artifact still restores, which is a stronger claim
//     than any hash comparison can make.
//
// Neither is required on its own; at least one is. See
// ReinstateFromQuarantine for the third rule, the one that ties the
// evidence to the reason for the distrust.
type ReinstatementEvidence struct {
	HashMatched     bool
	ValidatorPassed bool

	// AnyCheckFailed is true when any check the caller ran did not pass,
	// and it refuses the reinstatement on its own no matter what else is
	// set. A mixed verdict is a failing verdict: a restore-test hook that
	// passes does not excuse a recorded hash that no longer matches,
	// because the copy the hook just exercised is then demonstrably not
	// the copy this manager verified. The caller reports it rather than
	// this package inferring it from the two positives above, since a
	// caller can run checks this package knows nothing about.
	AnyCheckFailed bool

	// Summary is the human-readable rendering of everything the caller
	// checked, recorded verbatim in the transition's detail so a later
	// reader of the append-only log can see which evidence carried the
	// reinstatement rather than only that one did.
	Summary string
}

// conclusive reports whether e contains at least one check that could have
// failed on the artifact's content.
func (e ReinstatementEvidence) conclusive() bool {
	return e.HashMatched || e.ValidatorPassed
}

// QuarantineReinstateParams is what ReinstateFromQuarantine needs beyond
// Deps.
type QuarantineReinstateParams struct {
	// Artifact identifies the journal row to reinstate. It must currently
	// be QUARANTINED or QUARANTINED_LOST; anything else is refused with
	// *NotQuarantinedError.
	Artifact model.ArtifactID

	// AttemptKey is this reinstatement's idempotency key base, exactly
	// like QuarantineReleaseParams.AttemptKey.
	AttemptKey string

	// Evidence is what the caller proved about the durable local copy in
	// this same call. See ReinstatementEvidence, and see this function's
	// doc for why the check and the write have to be one operation.
	Evidence ReinstatementEvidence

	// Note is an optional operator-supplied explanation for why this
	// reinstatement is happening ("replaced the failing validator
	// binary", "the backup volume was not mounted when the check ran").
	// It is folded into the recorded detail alongside the evidence.
	Note string
}

// InsufficientEvidenceError reports that ReinstateFromQuarantine refused
// because what the caller proved does not justify trusting the artifact
// again. It is a refusal, not a failure: nothing was written, and the
// artifact is exactly where it was.
type InsufficientEvidenceError struct {
	Artifact model.ArtifactID
	Reason   string
}

func (e *InsufficientEvidenceError) Error() string {
	return fmt.Sprintf("lifecycle: refusing to reinstate %s from quarantine: %s", e.Artifact, e.Reason)
}

// AsInsufficientEvidence reports whether err is, or wraps, an
// *InsufficientEvidenceError. A caller that has to tell "the operator can
// fix this and try again" apart from "the copy is genuinely bad" checks
// for this rather than comparing error strings.
func AsInsufficientEvidence(err error) (*InsufficientEvidenceError, bool) {
	var e *InsufficientEvidenceError
	return e, errors.As(err, &e)
}

// NeverHeldTargetStateError reports that ReinstateFromQuarantine was asked
// to return an artifact to a state the append-only transition log has no
// record of it ever having held.
//
// The case this catches is an artifact quarantined out of VERIFYING: its
// recorded local path is still a .partial, the durable commit never ran,
// and reinstating it to COMMITTED would present a half-written file as a
// restore point. Its way back is the ordinary re-ingest
// (ReleaseFromQuarantine), which re-runs transfer, verification and commit
// properly.
type NeverHeldTargetStateError struct {
	Artifact model.ArtifactID
	Target   State
}

func (e *NeverHeldTargetStateError) Error() string {
	return fmt.Sprintf(
		"lifecycle: refusing to reinstate %s to %s: the transition log has no record of this artifact ever reaching %s, so there is no durable local copy to re-trust; re-ingest it instead",
		e.Artifact, e.Target, e.Target,
	)
}

// AsNeverHeldTargetState reports whether err is, or wraps, a
// *NeverHeldTargetStateError.
func AsNeverHeldTargetState(err error) (*NeverHeldTargetStateError, bool) {
	var e *NeverHeldTargetStateError
	return e, errors.As(err, &e)
}

// ReinstateFromQuarantine returns a quarantined artifact to the durable
// state it already held (QUARANTINED -> COMMITTED, QUARANTINED_LOST ->
// COMPLETE), on evidence the caller gathered in this same call.
//
// # What this is for
//
// The other exit, ReleaseFromQuarantine, throws the local copy away and
// re-fetches from the remote. That is right when the local copy is bad and
// the source is still there, and useless when the local copy is fine or
// the source is gone. This one keeps the local copy. See machine.go's
// "Reinstatement" section for the full argument.
//
// # The rules, and why each one is here
//
//  1. The artifact is QUARANTINED or QUARANTINED_LOST, else
//     *NotQuarantinedError. Unlike ReleaseFromQuarantine, QUARANTINED_LOST
//     is NOT refused: that state means the remote is confirmed gone, which
//     is exactly the case where re-ingesting cannot help and keeping the
//     local copy is the only recovery there is.
//
//  2. Nothing that ran failed. A mixed verdict is a failing verdict; see
//     ReinstatementEvidence.AnyCheckFailed.
//
//  3. The evidence is conclusive: at least one check that could have
//     failed on content actually ran and passed. See
//     ReinstatementEvidence.
//
//  4. If the artifact's own record carries a FAILED validator verdict,
//     the validator itself has to have run and passed now. Hash evidence
//     cannot answer a validator's rejection: it proves the bytes are
//     unchanged, and the unchanged bytes are precisely what the validator
//     refused. The evidence has to address the reason for the distrust,
//     not merely be strong in the abstract.
//
//  5. The append-only transition log records this artifact having entered
//     the target state before, else *NeverHeldTargetStateError.
//
// Every refusal happens before any write, so a refused reinstatement
// leaves no mark at all.
//
// # Why the caller may not check first and write later
//
// The evidence has to be gathered in the same call that writes, not handed
// in from an earlier one. A caller that re-checked an artifact, showed an
// operator a verdict, and then asked for the reinstatement in a second
// request would be writing on the strength of a fact that was true when it
// was measured and may not be now. That is why this takes an evidence
// value rather than reading a stored verdict, and why the only caller in
// this repository (internal/app.ReinstateQuarantined) runs the checks
// immediately above its call to this.
//
// # What it costs the artifact
//
// Taking either edge permanently forfeits the artifact's remote delete:
// DeleteRemote refuses any artifact whose transition log contains a
// reinstatement edge, before it records intent and before it touches the
// transport. That is what makes this edge safe to have at all, and it is
// not a cooling-off period; there is no way back to delete eligibility.
// See remotedelete.go.
func ReinstateFromQuarantine(ctx context.Context, d Deps, p QuarantineReinstateParams) (state.Outcome, error) {
	if d.Journal == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReinstateFromQuarantine needs a Journal")
	}
	if p.AttemptKey == "" {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReinstateFromQuarantine needs an AttemptKey")
	}
	if err := ctx.Err(); err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReinstateFromQuarantine: %w", err)
	}

	rec, err := d.Journal.Get(ctx, p.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReinstateFromQuarantine: looking up %s: %w", p.Artifact, err)
	}

	current := State(rec.State)
	target, ok := ReinstatementTarget(current)
	if !ok {
		return state.Outcome{}, &NotQuarantinedError{Artifact: p.Artifact, Current: current}
	}

	if p.Evidence.AnyCheckFailed {
		return state.Outcome{}, &InsufficientEvidenceError{
			Artifact: p.Artifact,
			Reason: "at least one check that ran did not pass, and a mixed verdict is a failing verdict: whatever else was proved, something about this durable local copy is not what it was when this manager verified it" +
				summarySuffix(p.Evidence.Summary),
		}
	}

	if !p.Evidence.conclusive() {
		return state.Outcome{}, &InsufficientEvidenceError{
			Artifact: p.Artifact,
			Reason: "nothing that could have failed was actually checked. Re-trusting a backup needs either a hash recorded at verification that the durable local copy still matches, or the backup set's application validator running and passing now; the local file merely still being present is not evidence" +
				summarySuffix(p.Evidence.Summary),
		}
	}

	if rec.ValidationPassed != nil && !*rec.ValidationPassed && !p.Evidence.ValidatorPassed {
		return state.Outcome{}, &InsufficientEvidenceError{
			Artifact: p.Artifact,
			Reason: "this artifact's recorded validator verdict is a rejection, and the validator did not run and pass in this call. A matching hash proves the bytes are unchanged, and the unchanged bytes are exactly what the validator refused, so the evidence has to include the validator itself" +
				summarySuffix(p.Evidence.Summary),
		}
	}

	if _, held, err := d.Journal.LastEnteredAt(ctx, p.Artifact, string(target)); err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReinstateFromQuarantine: reading the transition log for %s: %w", p.Artifact, err)
	} else if !held {
		return state.Outcome{}, &NeverHeldTargetStateError{Artifact: p.Artifact, Target: target}
	}

	detail := fmt.Sprintf("operator reinstated from %s to %s on re-checked evidence: %s", current, target, p.Evidence.Summary)
	if p.Note != "" {
		detail += " (operator note: " + p.Note + ")"
	}
	detail += ". The remote source is preserved from here on: a reinstated artifact never authorises a remote delete."

	t := state.Transition{
		Artifact: p.Artifact,
		Key:      p.AttemptKey + keyQuarantineReinstateSuffix,
		From:     string(current),
		To:       string(target),
		Detail:   detail,
	}
	// Only the validator's own re-run may overwrite the validator's own
	// recorded verdict. A hash-carried reinstatement leaves whatever
	// validation_passed held, because a hash comparison says nothing about
	// whether the artifact restores.
	if p.Evidence.ValidatorPassed {
		t.Validation = &state.ValidationUpdate{Passed: true, Detail: p.Evidence.Summary}
	}

	out, err := Advance(ctx, d, t)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReinstateFromQuarantine: recording the reinstatement of %s: %w", p.Artifact, err)
	}
	return out, nil
}

func summarySuffix(summary string) string {
	if summary == "" {
		return ""
	}
	return ". What was checked: " + summary
}
