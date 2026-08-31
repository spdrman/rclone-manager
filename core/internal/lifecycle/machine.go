package lifecycle

import "fmt"

// Transition is one legal (From, To) edge in the lifecycle graph.
type Transition struct {
	From State
	To   State
}

// Transitions is the single source of truth for every legal state change.
// Nothing outside this table is a legal move (current == target is the one
// exception, handled by Validate as an idempotent no-op, not as a row here;
// see the package doc and the comment on Validate for why).
//
// # The nominal path
//
// DISCOVERED through COMPLETE is the happy path FR-11 walks an artifact
// through. Each edge corresponds to one durable journal write, and the
// three that matter most for crash safety are commented individually below.
//
// # FAILED: reachable only before COMMITTED
//
// FAILED is entered from every state before COMMITTED, a permanent,
// non-retryable error (FR-22) at discovery, transfer, verification or the
// durable-commit step itself, and from nowhere at or after COMMITTED. Once
// the local file is durably committed the backup has already succeeded;
// there's no "the backup failed" story left to tell for this artifact, only
// a possible "the copy is bad" one, which is QUARANTINED's job, not
// FAILED's. FAILED has two exits: back to DISCOVERED (the retry policy
// restarts the artifact from scratch) or into QUARANTINED (the retry budget
// is exhausted and this needs a human instead of another attempt). Both
// exits are safe here specifically because FAILED can only be reached
// before COMMITTED, which means the remote delete has never been issued and
// the source is presumptively still there to recover from.
//
// # QUARANTINED vs. QUARANTINED_LOST: does a source still exist to recover from
//
// QUARANTINED covers content that's suspect while a remote copy still
// exists, or at least hasn't been confirmed gone: from VERIFYING, when a
// validator (FR-13) determines the transferred content itself is invalid,
// as opposed to the copy having merely failed (that's FAILED); and from
// COMMITTED or REMOTE_DELETE_PENDING, when later reconciliation (FR-17)
// finds the durable local copy has gone bad after the fact, bit rot, disk
// corruption, before the remote delete has actually happened. COMMITTED
// guarantees the remote is still untouched, and REMOTE_DELETE_PENDING only
// records intent (FR-16 requires re-confirming the remote object's identity
// before any delete is issued), so a fresh DISCOVERED attempt from either
// has a real chance of finding the source. QUARANTINED's one exit, back to
// DISCOVERED, is a genuine recovery path.
//
// QUARANTINED_LOST is a different outcome, not another way into the same
// state. It's entered only from COMPLETE, which is the one state in this
// whole graph that confirms the remote source is already deleted. An
// artifact whose durable local copy is found corrupted at that point has no
// copy anywhere left: not on the remote (COMPLETE said so), not intact
// locally (that's why it's here). Sending that case to DISCOVERED, the way
// QUARANTINED does, would ask the pipeline to rediscover and re-transfer
// something that no longer exists, which fails, lands in FAILED, and
// FAILED -> DISCOVERED sends it right back around: a livelock, and one that
// also mislabels an irrecoverable loss as an ordinary retryable failure.
// QUARANTINED_LOST is a hard stop that surfaces as an operator alarm
// (FR-24's quarantined count should count this too), not a state the state
// machine itself ever tries to route out of: nothing automatic moves an
// artifact out of it, and it has no path back into the pipeline at all
// (see TestOnlyCompletePrecedesQuarantinedLost and
// TestCompleteCannotLivelockThroughQuarantine). This is FR-10's one
// addition beyond the states the issue names. FR-17's reconciliation table
// has no row for "remote absent and local final copy invalid" either, so
// whoever builds FR-17's reconciliation (issue #18) needs that row added,
// targeting QUARANTINED_LOST.
//
// # Reinstatement: trusting a durable local copy again (issue #220)
//
// Both quarantine states also have an operator-only exit that keeps the
// local copy instead of throwing it away: QUARANTINED -> COMMITTED and
// QUARANTINED_LOST -> COMPLETE. They exist because re-ingesting is the
// wrong answer in two cases the product actually meets. The local copy is
// fine and the quarantine was the mistake (a misconfigured validator, a
// restore-test hook that failed for an environmental reason, a checksum
// recorded against the wrong algorithm), so re-transferring gigabytes
// re-establishes a fact a local re-check has just established. Or the
// remote source is gone while the local copy is intact, so there is
// nothing left to re-ingest FROM and the only alternative is to leave a
// perfectly good restore point quarantined forever, where FR-24 keeps
// reporting it and FR-19's last-known-good protection keeps skipping it.
//
// Each edge returns the artifact to the state it already held, never to a
// better one: an artifact quarantined out of VERIFYING never committed at
// all, and ReinstateFromQuarantine proves the artifact previously entered
// the target state by reading the append-only transition log before it
// writes anything. That is also why there is deliberately no
// QUARANTINED -> REMOTE_DELETE_PENDING edge, even though an artifact can
// be quarantined from there: COMMITTED must remain the only predecessor of
// REMOTE_DELETE_PENDING (TestOnlyCommittedPrecedesRemoteDeletePending), so
// an RDP-origin quarantine reinstates one step further back, which is
// strictly the more conservative of the two.
//
// # Why these edges cannot launder a corrupt artifact into a delete
//
// COMMITTED is the only state a remote delete can be reached from, so
// re-entering it is exactly where an edge out of quarantine could turn
// into a way to destroy the remote copy of an artifact that should never
// have been trusted. It cannot, because taking either reinstatement edge
// permanently forfeits the artifact's remote delete: DeleteRemote refuses,
// before it records anything and before it touches the transport, any
// artifact whose transition log contains one of these edges (see
// remotedelete.go and ReinstatementEdges below). The consequence is that a
// reinstated artifact's remote source is preserved indefinitely and an
// operator has to release it themselves, which is the safe direction to
// fail in, and is already the routine outcome of FR-16's identity check
// against the project's own recommended hardened SFTP posture.
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

	// --- entry points into QUARANTINED: content is invalid, source may still exist ---
	{From: Verifying, To: Quarantined},
	{From: Committed, To: Quarantined},
	{From: RemoteDeletePending, To: Quarantined},

	// --- the sole entry into QUARANTINED_LOST: source is confirmed gone ---
	{From: Complete, To: QuarantinedLost},

	// --- exits from the exceptional states ---
	{From: Failed, To: Discovered},
	{From: Failed, To: Quarantined},
	{From: Quarantined, To: Discovered},

	// --- reinstatement: the two edges that re-trust a durable local copy ---
	//
	// Quarantined -> Committed and QuarantinedLost -> Complete each return
	// an artifact to the exact state it already held before something
	// distrusted it, and to nothing else. Neither is an automatic move and
	// neither is reachable from anywhere in this package except
	// ReinstateFromQuarantine (quarantine.go), which refuses without
	// evidence that could actually have failed. See the "Reinstatement"
	// section of this comment above for the whole argument, including why
	// taking either edge permanently forfeits the artifact's remote delete.
	{From: Quarantined, To: Committed},
	{From: QuarantinedLost, To: Complete},
}

// quarantineStates is the set of states that mean "this artifact's content
// is suspect and a human has to decide what happens next". Every way out of
// one of them is an operator decision; nothing automatic moves an artifact
// out of quarantine.
var quarantineStates = map[State]bool{
	Quarantined:     true,
	QuarantinedLost: true,
}

// IsQuarantineState reports whether s is one of the two states that hold a
// suspect artifact for a human.
func IsQuarantineState(s State) bool { return quarantineStates[s] }

// durableRestorePoints is the set of states in which a durable local final
// copy exists and the artifact counts as a restore point. It is the same
// set internal/health's decideState, internal/retention's
// gfsIsManagedComplete and internal/revalidate's eligibleStates each
// already keep their own copy of; this one exists so that
// ReinstatementEdges can be derived from the table rather than
// hand-listed, and it is stated here, next to the table, because whether a
// state is a restore point is a property of the graph.
var durableRestorePoints = map[State]bool{
	Committed:           true,
	RemoteDeletePending: true,
	Complete:            true,
}

// IsDurableRestorePoint reports whether s is a state in which the artifact
// has a durable local final copy the rest of the system may treat as a
// restore point.
func IsDurableRestorePoint(s State) bool { return durableRestorePoints[s] }

// reinstatementEdges is every declared edge that returns an artifact from a
// quarantine state directly to a durable restore point, derived from the
// Transitions table itself rather than hand-listed.
//
// Deriving it is the point. FR-15's delete gate refuses any artifact whose
// transition log contains one of these edges, so a future edge out of
// quarantine into a trusted state is covered by that refusal the moment it
// is declared, without anyone having to remember to add it in two places.
// TestEveryQuarantineExitIntoADurableStateForfeitsRemoteDeletion walks the
// real table and proves the two lists cannot drift apart.
var reinstatementEdges = func() []Transition {
	var out []Transition
	for _, t := range Transitions {
		if IsQuarantineState(t.From) && IsDurableRestorePoint(t.To) {
			out = append(out, t)
		}
	}
	return out
}()

// ReinstatementEdges returns every edge that re-trusts an artifact out of
// quarantine, in table order. The returned slice is a copy: a caller
// cannot reshape the rule the delete gate enforces.
func ReinstatementEdges() []Transition {
	return append([]Transition(nil), reinstatementEdges...)
}

// ReinstatementTarget reports the state a quarantined artifact is returned
// to when it is reinstated, and whether from is a quarantine state that has
// such an exit at all.
//
// There is exactly one target per quarantine state, and it is always a
// state the artifact already held: QUARANTINED came from COMMITTED or
// REMOTE_DELETE_PENDING (reinstating to the earlier of the two, see the
// Transitions doc), and QUARANTINED_LOST came from COMPLETE.
func ReinstatementTarget(from State) (State, bool) {
	for _, t := range reinstatementEdges {
		if t.From == from {
			return t.To, true
		}
	}
	return "", false
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
// current == target is always legal and is treated as an idempotent no-op,
// regardless of whether that pair also happens to be a declared edge. The
// crash matrix (docs/EPIC.md) terminates the process after every one of
// these states, so on restart a step gets retried without knowing whether
// its own last attempt already landed; asking to move to the state you're
// already in has to succeed, or every crash would turn into a stuck
// artifact. Anything else must appear in Transitions or Validate refuses
// it, and that refusal is what catches a real bug (a skipped step, a stale
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
// though current == target is always a legal move via Validate; this
// function answers "what can lead here", and the idempotent no-op isn't a
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
// no-op case a restarted retry produces) and, of course, on error. A caller
// that wants to know "did this attempt just newly land, or was it already
// done" reads changed; a caller that only wants a durable outcome can
// ignore it and check err alone.
func (m *Machine) Apply(target State) (changed bool, err error) {
	if err := Validate(m.current, target); err != nil {
		return false, err
	}
	changed = m.current != target
	m.current = target
	return changed, nil
}
