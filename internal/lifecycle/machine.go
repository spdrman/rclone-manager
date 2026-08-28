package lifecycle

import "fmt"

// Transition is one legal (From, To) edge in the lifecycle graph.
type Transition struct {
	From State
	To   State
}

// Transitions is the single source of truth for every legal state change.
// Nothing outside this table is a legal move (current == target is the one
// exception, handled by Validate as an idempotent no-op, not as a row
// here — see the package doc and the comment on Validate for why).
//
// # The nominal path
//
// DISCOVERED through COMPLETE is the happy path FR-11 walks an artifact
// through. Each edge corresponds to one durable journal write, and the
// three that matter most for crash safety are commented individually below.
//
// # FAILED: reachable only before COMMITTED
//
// FAILED is entered from every state before COMMITTED — a permanent,
// non-retryable error (FR-22) at discovery, transfer, verification or the
// durable-commit step itself — and from nowhere at or after COMMITTED.
// Once the local file is durably committed the backup has already
// succeeded; there's no "the backup failed" story left to tell for this
// artifact, only a possible "the copy is bad" one, which is QUARANTINED's
// job, not FAILED's. FAILED has two exits: back to DISCOVERED (the retry
// policy restarts the artifact from scratch) or into QUARANTINED (the
// retry budget is exhausted and this needs a human instead of another
// attempt).
//
// # QUARANTINED: content is suspect, not that an attempt errored
//
// QUARANTINED has two kinds of entry. From VERIFYING, when a validator
// (FR-13) determines the transferred content itself is invalid, as
// opposed to the copy having merely failed (that's FAILED). And from
// COMMITTED, REMOTE_DELETE_PENDING or COMPLETE, when later reconciliation
// (FR-17) finds the durable local copy has gone bad after the fact — bit
// rot, disk corruption. Those three and only those three, because a
// "final"-named local file (FR-12) exists starting at COMMITTED and never
// before it. Critically, COMMITTED and REMOTE_DELETE_PENDING routing to
// QUARANTINED instead of continuing forward means corruption discovered
// before the remote delete is issued always aborts the delete: the remote
// copy is preserved (FR-16) rather than removed out from under a bad local
// copy. QUARANTINED has exactly one exit, back to DISCOVERED, and that is
// deliberate: reaching COMPLETE again means re-running discovery,
// transfer, verification and commit in full, never a shortcut back onto
// the happy path. A direct edge from QUARANTINED to COMMITTED, to
// REMOTE_DELETE_PENDING, or to COMPLETE is not merely missing by omission —
// TestQuarantineCannotShortcutToSuccess asserts each is refused, because
// that shortcut is exactly the hole this package exists to close.
var Transitions = []Transition{
	// --- nominal path ---
	{From: Discovered, To: Transferring},
	{From: Transferring, To: Transferred},
	{From: Transferred, To: Verifying},
	{From: Verifying, To: Verified},
	{From: Verified, To: Committing},

	// Committing -> Committed: the durable-commit write. See the package
	// doc's crash-safety walkthrough. Nothing may skip ahead of this edge;
	// it is the boundary that makes "the backup already exists safely on
	// disk" true.
	{From: Committing, To: Committed},

	// Committed -> RemoteDeletePending: recording *intent* to delete the
	// remote, strictly before any delete call is issued. Committed is this
	// state's only predecessor (TestOnlyCommittedPrecedesRemoteDeletePending
	// proves it against every state this package knows), which is what
	// guarantees a remote delete can never be reached without the local
	// durable commit already having landed in the journal first.
	{From: Committed, To: RemoteDeletePending},

	// RemoteDeletePending -> Complete: recorded only after the remote
	// delete is confirmed, or after reconciliation (FR-17) independently
	// confirms the remote object was already gone.
	{From: RemoteDeletePending, To: Complete},

	// --- entry points into FAILED: any permanent error before commit ---
	{From: Discovered, To: Failed},
	{From: Transferring, To: Failed},
	{From: Transferred, To: Failed},
	{From: Verifying, To: Failed},
	{From: Verified, To: Failed},
	{From: Committing, To: Failed},

	// --- entry points into QUARANTINED: content is invalid, not merely failed ---
	{From: Verifying, To: Quarantined},
	{From: Committed, To: Quarantined},
	{From: RemoteDeletePending, To: Quarantined},
	{From: Complete, To: Quarantined},

	// --- exits from the exceptional states ---
	{From: Failed, To: Discovered},
	{From: Failed, To: Quarantined},
	{From: Quarantined, To: Discovered},
}

var transitionSet = func() map[Transition]bool {
	m := make(map[Transition]bool, len(Transitions))
	for _, t := range Transitions {
		m[t] = true
	}
	return m
}()

// IllegalTransitionError reports an attempted move the lifecycle graph does
// not allow. This must fail loudly: asking to move to a state that isn't
// reachable from the current one is a caller bug or a corrupted journal
// row, not something to retry or paper over.
type IllegalTransitionError struct {
	From State
	To   State
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("lifecycle: %s -> %s is not a legal transition", e.From, e.To)
}

// UnknownStateError reports a state string that is not one of the names
// this package defines. The journal (FR-9) stores the state as a plain
// string column, so a schema drift, a hand-edited row, or a build mismatch
// could hand this package a name it doesn't recognize; refuse it rather
// than silently treating it as anything in particular.
type UnknownStateError struct {
	Raw string
}

func (e *UnknownStateError) Error() string {
	return fmt.Sprintf("lifecycle: %q is not a known state", e.Raw)
}

// Validate reports whether moving an artifact from current to target is
// legal.
//
// current == target is always legal and is treated as an idempotent
// no-op, regardless of whether that pair also happens to be a declared
// edge. The crash matrix (docs/EPIC.md) terminates the process after every
// one of these states, so on restart a step gets retried without knowing
// whether its own last attempt already landed; asking to move to the state
// you're already in has to succeed, or every crash would turn into a stuck
// artifact. Anything else must appear in Transitions or Validate refuses
// it — that refusal is what catches a real bug (a skipped step, a stale
// in-memory copy of the state, a corrupted journal row) instead of letting
// it silently corrupt the journal.
func Validate(current, target State) error {
	if !current.Valid() {
		return &UnknownStateError{Raw: string(current)}
	}
	if !target.Valid() {
		return &UnknownStateError{Raw: string(target)}
	}
	if current == target {
		return nil
	}
	if transitionSet[Transition{From: current, To: target}] {
		return nil
	}
	return &IllegalTransitionError{From: current, To: target}
}

// Predecessors returns every state Transitions declares as a legal source
// for target, in table order. It does not include target itself, even
// though current == target is always a legal move via Validate — this
// function answers "what can lead here", the idempotent no-op isn't a
// lead-in, it's staying put.
func Predecessors(target State) []State {
	var out []State
	for _, t := range Transitions {
		if t.To == target {
			out = append(out, t.From)
		}
	}
	return out
}

// Successors returns every state Transitions declares as a legal
// destination from, in table order.
func Successors(from State) []State {
	var out []State
	for _, t := range Transitions {
		if t.From == from {
			out = append(out, t.To)
		}
	}
	return out
}

// Machine holds one artifact's current lifecycle state and refuses to move
// it anywhere Transitions doesn't allow.
//
// Callers that persist state elsewhere, such as the FR-9 journal, don't
// need this type; Validate is the primitive they actually need, likely
// right before a conditional UPDATE. Machine exists for in-process callers
// that want a value carrying its own current state, one that can't be
// pointed at an illegal one.
type Machine struct {
	current State
}

// NewMachine starts a Machine at current. It's an error to start one at a
// state this package doesn't recognize.
func NewMachine(current State) (*Machine, error) {
	if !current.Valid() {
		return nil, &UnknownStateError{Raw: string(current)}
	}
	return &Machine{current: current}, nil
}

// Current returns the machine's current state.
func (m *Machine) Current() State { return m.current }

// Apply moves the machine to target if Validate allows it. On rejection the
// machine's current state is left exactly as it was; a caller can inspect
// Current afterward and know a failed Apply never partially applied.
//
// changed reports whether this call actually moved the machine anywhere.
// It's false both when target equals the current state (the idempotent
// no-op case a restarted retry produces) and, of course, on error. A
// caller that wants to know "did this attempt just newly land, or was it
// already done" reads changed; a caller that only wants a durable outcome
// can ignore it and check err alone.
func (m *Machine) Apply(target State) (changed bool, err error) {
	if err := Validate(m.current, target); err != nil {
		return false, err
	}
	changed = m.current != target
	m.current = target
	return changed, nil
}
