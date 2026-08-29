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
// when the remote source is already confirmed gone. There is no copy left
// anywhere to recover from, so unlike QUARANTINED this is terminal by
// design, and ReleaseFromQuarantine refuses it with a distinct, typed
// error (QuarantinedLostIsTerminalError) rather than silently doing
// nothing or, worse, treating it like an ordinary quarantine. A caller
// that wants to tell the two apart programmatically uses
// AsQuarantinedLostIsTerminal rather than comparing error strings.
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
	return fmt.Sprintf("lifecycle: refusing to release %s from quarantine: its journal state is %s, not %s", e.Artifact, e.Current, Quarantined)
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
