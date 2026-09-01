// This file is issue #282's read-only path: a backup set an operator has
// declared "pull from here, never delete here" (config.BackupSet.ReadOnly)
// never reaches remotedelete.go's DeleteRemote at all. RetainRemote is what
// core/internal/app's pipeline calls instead, for exactly the same two
// states DeleteRemote would otherwise be offered (COMMITTED and
// REMOTE_DELETE_PENDING), and it moves the artifact straight to
// REMOTE_RETAINED, the terminal state that records the remote copy as kept
// by policy rather than pending deletion.
//
// # Why this is a different function, not a flag on DeleteRemoteRequest
//
// The issue's own acceptance criterion is that "no code path can reach
// DeleteRemote for that set", proven by a test that asserts the transport's
// delete is never invoked, not by asserting a refusal. A boolean added to
// DeleteRemoteRequest that made DeleteRemote itself refuse early would
// still be reachable: transport.Transport.DeleteRemote would sit right
// there in the same function body, one more revalidation check away from
// running, and every future change to that function would have to keep
// noticing the flag correctly forever. Splitting the read-only path into
// its own function removes that risk structurally rather than by
// vigilance: RetainRemote's signature has no Source field (transport.Source
// is DeleteRemoteRequest's, not this one) and its body never reads
// Deps.Transport at all, so there is no expression here that could reach
// transport.Transport.DeleteRemote even if every argument were adversarial,
// and the caller (core/internal/app/pipeline.go's processArtifact) chooses
// between the two functions before either is invoked, never inside one.
package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// RetainRemoteRequest is everything one attempt at issue #282's retain
// transition needs. It deliberately carries no transport.Source and no
// completion-strategy/safety-delay fields: none of DeleteRemote's
// revalidation questions ("is the remote object still what was discovered",
// "has the stable-completion safety delay elapsed") are meaningful here,
// because this function is never going to touch the remote either way, on
// any completion strategy.
type RetainRemoteRequest struct {
	// Artifact identifies the journal row this attempt is acting on.
	Artifact model.ArtifactID

	// AttemptKey is this attempt's idempotency seed, exactly the same
	// contract DeleteRemoteRequest.AttemptKey documents: derive it
	// deterministically per logical attempt, never mint a fresh one on a
	// restart-driven retry.
	AttemptKey string
}

// RetainRemote moves an artifact from COMMITTED or REMOTE_DELETE_PENDING to
// REMOTE_RETAINED: issue #282's read-only path. It is the only function in
// this package that takes either edge (see machine.go's Transitions table),
// and it never calls, references, or requires Deps.Transport; a caller does
// not even need to supply one.
//
// current == RemoteRetained is accepted as the idempotent no-op Validate
// already grants any same-state move: an artifact already retained on a
// previous cycle, whose owning backup set is still read-only, is retained
// again harmlessly rather than refused as a caller bug.
//
// Any other current state is refused outright, mirroring DeleteRemote's own
// "journal state" check: it catches a caller bug or a corrupted row before
// this package writes anything.
func RetainRemote(ctx context.Context, d Deps, req RetainRemoteRequest) (state.Outcome, error) {
	if d.Journal == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: RetainRemote needs a Journal")
	}
	if req.AttemptKey == "" {
		return state.Outcome{}, fmt.Errorf("lifecycle: RetainRemote requires a non-empty AttemptKey")
	}

	rec, err := d.Journal.Get(ctx, req.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: RetainRemote: load journal record for %s: %w", req.Artifact, err)
	}

	current := State(rec.State)
	switch current {
	case Committed, RemoteDeletePending, RemoteRetained:
		// legal starting points; proceed.
	default:
		return state.Outcome{}, fmt.Errorf(
			"lifecycle: RetainRemote: journal records %s for %s, which is not COMMITTED, REMOTE_DELETE_PENDING or REMOTE_RETAINED",
			rec.State, req.Artifact)
	}

	return Advance(ctx, d, state.Transition{
		Artifact: req.Artifact,
		Key:      req.AttemptKey,
		From:     string(current),
		To:       string(RemoteRetained),
		Detail:   "issue #282: this backup set is declared read-only; the remote source is retained by policy and transport.Transport.DeleteRemote is never called for it",
	})
}

// ReleaseFromRetentionRequest is everything one call to ReleaseFromRetention
// needs.
type ReleaseFromRetentionRequest struct {
	// Artifact identifies the journal row to release.
	Artifact model.ArtifactID

	// AttemptKey is this release's idempotency key base.
	AttemptKey string

	// Note is an optional operator-supplied explanation, folded into the
	// recorded Detail, mirroring QuarantineReleaseParams.Note.
	Note string
}

// NotRemoteRetainedError reports that ReleaseFromRetention was asked to act
// on an artifact whose journal state is not currently REMOTE_RETAINED.
type NotRemoteRetainedError struct {
	Artifact model.ArtifactID
	Current  State
}

func (e *NotRemoteRetainedError) Error() string {
	return fmt.Sprintf("lifecycle: refusing to release %s from retention: its journal state is %s, not %s", e.Artifact, e.Current, RemoteRetained)
}

// AsNotRemoteRetained reports whether err is, or wraps, a
// *NotRemoteRetainedError.
func AsNotRemoteRetained(err error) (*NotRemoteRetainedError, bool) {
	var e *NotRemoteRetainedError
	return e, errors.As(err, &e)
}

// ReleaseFromRetention is REMOTE_RETAINED's one declared exit
// (machine.go's Transitions table): an explicit, operator-triggered
// decision that an artifact this manager was retaining by policy should
// re-enter the ordinary FR-15 delete-eligible pipeline after all. It moves
// the artifact back to COMMITTED, exactly where RetainRemote's own two
// edges both originate from leaving, so a released artifact is revalidated
// from scratch by DeleteRemote on the next cycle like any other COMMITTED
// artifact, never trusted on the strength of having once been retained.
//
// Like ReleaseFromQuarantine and ReinstateFromQuarantine, this is never
// called by RunCycle, a scheduler or a retry policy; it exists purely as an
// operator-facing primitive for whichever future issue wires a `release
// remote-retention` use case on top of it (config.BackupSet.ReadOnly still
// governs whether the NEXT cycle retains this artifact again; flipping
// that is that future caller's job, not this function's).
func ReleaseFromRetention(ctx context.Context, d Deps, req ReleaseFromRetentionRequest) (state.Outcome, error) {
	if d.Journal == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReleaseFromRetention needs a Journal")
	}
	if req.AttemptKey == "" {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReleaseFromRetention requires a non-empty AttemptKey")
	}

	rec, err := d.Journal.Get(ctx, req.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: ReleaseFromRetention: load journal record for %s: %w", req.Artifact, err)
	}

	current := State(rec.State)
	if current != RemoteRetained {
		return state.Outcome{}, &NotRemoteRetainedError{Artifact: req.Artifact, Current: current}
	}

	detail := "issue #282: an operator released this artifact from read-only retention; it re-enters the ordinary FR-15 delete-eligible pipeline"
	if req.Note != "" {
		detail += ": " + req.Note
	}

	return Advance(ctx, d, state.Transition{
		Artifact: req.Artifact,
		Key:      req.AttemptKey,
		From:     string(RemoteRetained),
		To:       string(Committed),
		Detail:   detail,
	})
}
