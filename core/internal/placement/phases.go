package placement

import (
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is FR-30's move state machine, and it is written the way
// internal/lifecycle/machine.go is written, for the same reason: a crash
// can land the process anywhere, so which changes are legal has to be a
// table a test can walk rather than a rule spread across the functions
// that happen to make each change.
//
// # The ordering this table exists to make structural
//
// A move is copy, verify, delete source, and the whole safety argument is
// the order. The source copy is deleted only after VERIFIED is durably
// recorded. Everything below is arranged so that the wrong order is not a
// mistake somebody can make in a function body; it is an edge that does
// not exist.
//
//	PLANNED -> COPYING -> COPIED -> VERIFYING -> VERIFIED -> SOURCE_DELETE_PENDING -> DONE
//	   \          \         \          \                            |
//	    \          \         \          \                           v
//	     ---------- ABANDONED -----------                        COPYING
//
// # VERIFIED is the disposability boundary, and that is why it has no
// ABANDONED edge
//
// ABANDONED means "the destination copy was disposable, and it has been
// disposed of; the source was never touched". That sentence is true at
// PLANNED, COPYING, COPIED and VERIFYING, and it stops being true at
// VERIFIED, because the write that records VERIFIED is the write that
// gives the destination its placements row. From that instant the
// destination is a copy this product has verified and told its own journal
// about, and cleaning it up would mean deleting verified data to tidy up.
// So VERIFIED has exactly one successor, and abandoning after it is not a
// decision anyone has to remember not to take.
//
// # Why SOURCE_DELETE_PENDING can go back to COPYING
//
// FR-30's restart semantics: a move found at SOURCE_DELETE_PENDING
// re-verifies the destination and, on failure, "returns to COPYING with
// the source still intact". That is a real edge and not a tidy-up: the
// destination has just failed a verification it previously passed, which
// means the bytes there are not the artifact, and the answer is to put the
// source placement back to ACTIVE, throw the destination away and copy
// again. The engine takes it only after it has restored the source, so
// there is no instant at which the journal says both copies are
// disposable.

// Phase is one point in a move's own small state machine. It is not a
// lifecycle state and it never appears on an artifact row: FR-30 is
// explicit that the artifact stays COMPLETE throughout a move, and this
// vocabulary lives here so nothing is tempted to put it on the lifecycle
// machine.
type Phase string

const (
	// Planned is durable intent and nothing else. No byte has moved.
	Planned Phase = Phase(state.MovePlanned)

	// Copying means the upload (or the download, for a move back to
	// local) has been decided and may be in flight. The destination key
	// is deterministic, so re-entering this phase after a crash targets
	// the same object and converges instead of leaving a second one.
	Copying Phase = Phase(state.MoveCopying)

	// Copied means the destination reported the bytes written. It does
	// NOT mean anything has looked at them.
	Copied Phase = Phase(state.MoveCopied)

	// Verifying means verification has been decided and may be in
	// flight. Nothing about it is resumable, so a move found here
	// re-verifies from scratch.
	Verifying Phase = Phase(state.MoveVerifying)

	// Verified means the destination copy has been checked at the class
	// the medium requires, and the placements row saying so is durable.
	// This is the only phase from which a source delete is reachable.
	Verified Phase = Phase(state.MoveVerified)

	// SourceDeletePending means the source delete has been decided and
	// durably recorded and may not have happened yet. It is the exact
	// shape REMOTE_DELETE_PENDING already has for FR-15.
	SourceDeletePending Phase = Phase(state.MoveSourceDeletePending)

	// Done means the source copy is gone and the destination is the
	// artifact's home.
	Done Phase = Phase(state.MoveDone)

	// Abandoned means the destination was cleaned up and the source was
	// never touched.
	Abandoned Phase = Phase(state.MoveAbandoned)
)

// Phases is every phase, in the order a nominal move walks them, with the
// two terminal ones last.
var Phases = []Phase{
	Planned, Copying, Copied, Verifying, Verified, SourceDeletePending, Done, Abandoned,
}

// PhaseTransition is one legal (From, To) edge.
type PhaseTransition struct {
	From Phase
	To   Phase
}

// PhaseTransitions is the single source of truth for every legal phase
// change. Nothing outside this table is a legal move, with the one
// exception Validate names: From == To, which is how a caller records a
// placement fact or an error without changing phase.
var PhaseTransitions = []PhaseTransition{
	// --- the nominal path ---
	{From: Planned, To: Copying},
	{From: Copying, To: Copied},
	{From: Copied, To: Verifying},

	// Verifying -> Verified is the write that authorises everything
	// dangerous that follows, and it is also the write that creates the
	// destination's placements row. The engine takes it only with a
	// verification Result whose Passed is true at the class the medium
	// requires; there is no path that reaches it from a class that was
	// merely attempted.
	{From: Verifying, To: Verified},

	// Verified -> SourceDeletePending records the intent to delete the
	// source, strictly before any delete is issued, exactly as
	// COMMITTED -> REMOTE_DELETE_PENDING does for the remote half.
	{From: Verified, To: SourceDeletePending},

	// SourceDeletePending -> Done is recorded after the source copy is
	// confirmed gone.
	{From: SourceDeletePending, To: Done},

	// SourceDeletePending -> Copying is FR-30's restart answer to a
	// destination that fails re-verification at the last moment. See the
	// file comment: the source placement is restored to ACTIVE in the
	// same durable write that takes this edge.
	{From: SourceDeletePending, To: Copying},

	// Verifying -> Copying: the same answer reached the ordinary way,
	// when the verification that would have produced VERIFIED fails. The
	// destination is deleted and copied again.
	{From: Verifying, To: Copying},

	// --- abandonment: every phase in which the destination is still the
	// disposable copy, and no phase after that ---
	{From: Planned, To: Abandoned},
	{From: Copying, To: Abandoned},
	{From: Copied, To: Abandoned},
	{From: Verifying, To: Abandoned},
}

// terminalPhases is the set a move has finished in.
var terminalPhases = map[Phase]bool{Done: true, Abandoned: true}

// IsTerminal reports whether p is a phase a move has finished in.
func IsTerminal(p Phase) bool { return terminalPhases[p] }

// NonTerminalPhases returns every phase a restart can find a move sitting
// in, derived from Phases and IsTerminal rather than hand-listed.
//
// Deriving it is the point, and it is aimed at a specific bug this project
// already has once: #372 is open because a phase was added to a state
// machine and the code that drives artifacts forward was never given a
// case for it, so rows sat in that state forever and nothing looked at
// them again. The engine's resume switch is checked against this list by a
// test, so a phase added here with no way out fails a test instead of
// stalling a move.
func NonTerminalPhases() []Phase {
	var out []Phase
	for _, p := range Phases {
		if !IsTerminal(p) {
			out = append(out, p)
		}
	}
	return out
}

// NonTerminalPhaseStrings is NonTerminalPhases in the spelling
// internal/state stores, ready to hand to ListMoves.
func NonTerminalPhaseStrings() []string {
	phases := NonTerminalPhases()
	out := make([]string, 0, len(phases))
	for _, p := range phases {
		out = append(out, string(p))
	}
	return out
}

// ValidPhase reports whether p is a phase this machine knows.
func ValidPhase(p Phase) bool {
	for _, known := range Phases {
		if p == known {
			return true
		}
	}
	return false
}

// ValidatePhaseChange reports whether from -> to is a change this machine
// allows.
//
// from == to is allowed and is not a row in the table, for
// lifecycle.Validate's reason: it is an idempotent no-op, which is what a
// caller recording a placement fact or an error without changing phase is
// doing, and putting eight self-edges in the table would drown the six
// that carry the safety argument.
func ValidatePhaseChange(from, to Phase) error {
	if !ValidPhase(from) {
		return fmt.Errorf("placement: %q is not a move phase", from)
	}
	if !ValidPhase(to) {
		return fmt.Errorf("placement: %q is not a move phase", to)
	}
	if from == to {
		return nil
	}
	for _, t := range PhaseTransitions {
		if t.From == from && t.To == to {
			return nil
		}
	}
	return fmt.Errorf("placement: %s -> %s is not a legal move phase change", from, to)
}
