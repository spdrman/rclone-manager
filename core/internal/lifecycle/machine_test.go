package lifecycle

import (
	"sort"
	"testing"
)

// stateSet turns a slice into a set for order-independent comparison; the
// order Predecessors/Successors return (table order) is not itself a
// promise these tests want to pin down.
func stateSet(states []State) map[State]bool {
	m := make(map[State]bool, len(states))
	for _, s := range states {
		m[s] = true
	}
	return m
}

func sortedStrings(states []State) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	sort.Strings(out)
	return out
}

func assertStateSet(t *testing.T, label string, got []State, want ...State) {
	t.Helper()
	gotSet, wantSet := stateSet(got), stateSet(want)
	if len(gotSet) != len(got) {
		t.Errorf("%s: got has duplicates: %v", label, sortedStrings(got))
	}
	for s := range gotSet {
		if !wantSet[s] {
			t.Errorf("%s: unexpected member %q; got=%v want=%v", label, s, sortedStrings(got), sortedStrings(want))
		}
	}
	for s := range wantSet {
		if !gotSet[s] {
			t.Errorf("%s: missing member %q; got=%v want=%v", label, s, sortedStrings(got), sortedStrings(want))
		}
	}
}

// --- the table itself is well formed ---

func TestTransitionsTableIsWellFormed(t *testing.T) {
	seen := map[Transition]bool{}
	for _, tr := range Transitions {
		if !tr.From.Valid() {
			t.Errorf("transition %+v has an unknown From state", tr)
		}
		if !tr.To.Valid() {
			t.Errorf("transition %+v has an unknown To state", tr)
		}
		if tr.From == tr.To {
			t.Errorf("transition %+v is a self-loop; idempotence via Validate's current==target rule makes an explicit self-loop redundant and doubles the maintenance burden", tr)
		}
		if seen[tr] {
			t.Errorf("transition %+v is declared more than once", tr)
		}
		seen[tr] = true
	}
}

// Every declared edge must actually validate, otherwise the table and
// Validate have drifted apart.
func TestDeclaredTransitionsValidate(t *testing.T) {
	for _, tr := range Transitions {
		if err := Validate(tr.From, tr.To); err != nil {
			t.Errorf("declared transition %s -> %s does not validate: %v", tr.From, tr.To, err)
		}
	}
}

func TestEveryStateParticipatesInTheGraph(t *testing.T) {
	touched := map[State]bool{}
	for _, tr := range Transitions {
		touched[tr.From] = true
		touched[tr.To] = true
	}
	for _, s := range AllStates {
		if !touched[s] {
			t.Errorf("%q appears in AllStates but not in any Transition; a state nothing can enter or leave is dead code", s)
		}
	}
}

// --- idempotence vs. illegality ---

// Re-applying a transition that already landed must be a no-op success,
// because the crash matrix restarts the process at every boundary and the
// restarted step doesn't know whether its own last attempt already landed.
func TestSameStateIsAlwaysAnIdempotentNoOp(t *testing.T) {
	for _, s := range AllStates {
		if err := Validate(s, s); err != nil {
			t.Errorf("Validate(%s, %s) = %v, want nil (idempotent no-op)", s, s, err)
		}
	}
}

// A genuinely illegal move, skipping steps, going backward, or jumping
// straight to a terminal state, must fail, and must fail with the specific
// error a caller can distinguish from the idempotent case.
func TestIllegalTransitionsFail(t *testing.T) {
	for _, tc := range []struct{ from, to State }{
		{Discovered, Verified},            // skips ahead
		{Discovered, Complete},            // skips the entire pipeline
		{Transferring, Committed},         // skips verification entirely
		{Committed, Verified},             // goes backward
		{Complete, Transferring},          // goes backward from terminal
		{RemoteDeletePending, Committing}, // goes backward
		{Verified, RemoteDeletePending},   // skips commit
		{Failed, Committed},               // failure can't shortcut to success
		{Failed, Complete},                // failure can't shortcut to success
		{Failed, RemoteDeletePending},     // failure can't shortcut to success
		{Failed, QuarantinedLost},         // a pre-commit failure never confirmed the source gone
		{Quarantined, Verified},           // quarantine can't shortcut back in
		{Quarantined, QuarantinedLost},    // quarantine can't declare loss without going through Complete
		{Complete, Committed},             // terminal can't rewind
		{RemoteDeletePending, Discovered}, // can't abandon a delete-pending mid-flight back to the start
		{QuarantinedLost, Discovered},     // no route back into the pipeline, see TestCompleteCannotLivelockThroughQuarantine
		{QuarantinedLost, Quarantined},    // no route from the unrecoverable outcome to the recoverable one
	} {
		err := Validate(tc.from, tc.to)
		if err == nil {
			t.Errorf("Validate(%s, %s) = nil, want an error", tc.from, tc.to)
			continue
		}
		if _, ok := err.(*IllegalTransitionError); !ok {
			t.Errorf("Validate(%s, %s) error is %T, want *IllegalTransitionError", tc.from, tc.to, err)
		}
	}
}

func TestValidateRejectsUnknownStates(t *testing.T) {
	if err := Validate(State("BOGUS"), Discovered); err == nil {
		t.Error("Validate with an unknown current state accepted")
	} else if _, ok := err.(*UnknownStateError); !ok {
		t.Errorf("error is %T, want *UnknownStateError", err)
	}
	if err := Validate(Discovered, State("BOGUS")); err == nil {
		t.Error("Validate with an unknown target state accepted")
	} else if _, ok := err.(*UnknownStateError); !ok {
		t.Errorf("error is %T, want *UnknownStateError", err)
	}
}

// --- the safety spine: nothing reaches REMOTE_DELETE_PENDING except through COMMITTED ---

// This is the property the whole design exists to guarantee: a remote
// delete can only ever be issued after the local durable commit has
// already landed in the journal. Try every state this package knows as a
// predecessor and show only COMMITTED is accepted.
func TestOnlyCommittedPrecedesRemoteDeletePending(t *testing.T) {
	preds := Predecessors(RemoteDeletePending)
	assertStateSet(t, "Predecessors(RemoteDeletePending)", preds, Committed)

	for _, s := range AllStates {
		err := Validate(s, RemoteDeletePending)
		switch s {
		case Committed:
			if err != nil {
				t.Errorf("Validate(COMMITTED, REMOTE_DELETE_PENDING) = %v, want nil", err)
			}
		case RemoteDeletePending:
			// The idempotent no-op case, not a "predecessor" in the graph
			// sense: re-recording the same intent must still succeed.
			if err != nil {
				t.Errorf("Validate(REMOTE_DELETE_PENDING, REMOTE_DELETE_PENDING) = %v, want nil (idempotent)", err)
			}
		default:
			if err == nil {
				t.Errorf("Validate(%s, REMOTE_DELETE_PENDING) = nil, want an error: only COMMITTED may precede REMOTE_DELETE_PENDING", s)
			}
		}
	}
}

// --- the second safety spine: nothing reaches QUARANTINED_LOST except through COMPLETE ---

// QUARANTINED_LOST asserts that the remote source is confirmed gone. That
// assertion is only true coming from COMPLETE, so this proves the same
// shape of property TestOnlyCommittedPrecedesRemoteDeletePending proves for
// the delete boundary: try every known state as a predecessor and show only
// COMPLETE is accepted.
func TestOnlyCompletePrecedesQuarantinedLost(t *testing.T) {
	preds := Predecessors(QuarantinedLost)
	assertStateSet(t, "Predecessors(QuarantinedLost)", preds, Complete)

	for _, s := range AllStates {
		err := Validate(s, QuarantinedLost)
		switch s {
		case Complete:
			if err != nil {
				t.Errorf("Validate(COMPLETE, QUARANTINED_LOST) = %v, want nil", err)
			}
		case QuarantinedLost:
			if err != nil {
				t.Errorf("Validate(QUARANTINED_LOST, QUARANTINED_LOST) = %v, want nil (idempotent)", err)
			}
		default:
			if err == nil {
				t.Errorf("Validate(%s, QUARANTINED_LOST) = nil, want an error: only COMPLETE may precede QUARANTINED_LOST, since that's the only state confirming the remote source is already gone", s)
			}
		}
	}
}

// --- FAILED: defined entry points, defined exits ---

func TestFailedEntryPoints(t *testing.T) {
	assertStateSet(t, "Predecessors(Failed)", Predecessors(Failed),
		Discovered, Transferring, Transferred, Verifying, Verified, Committing)
}

// Once an artifact is durably COMMITTED the backup has already succeeded,
// so nothing at or after COMMITTED may fail out of existence.
func TestFailedIsUnreachableOnceCommitted(t *testing.T) {
	for _, s := range []State{Committed, RemoteDeletePending, Complete} {
		if err := Validate(s, Failed); err == nil {
			t.Errorf("Validate(%s, FAILED) = nil, want an error: nothing after COMMITTED may fail", s)
		}
	}
}

func TestFailedHasExits(t *testing.T) {
	exits := Successors(Failed)
	if len(exits) == 0 {
		t.Fatal("FAILED has no declared successors; an artifact that fails would be stuck there forever")
	}
	assertStateSet(t, "Successors(Failed)", exits, Discovered, Quarantined)
}

// --- QUARANTINED: defined entry points, defined exits, no shortcut back to success ---

func TestQuarantinedEntryPoints(t *testing.T) {
	// VERIFYING: a validator found the content itself invalid.
	// COMMITTED / REMOTE_DELETE_PENDING: reconciliation found the durable,
	// final-named local copy corrupted after the fact, but before the
	// remote delete has actually happened, so a source may still exist.
	// COMPLETE is deliberately excluded here: by then the remote is
	// confirmed gone, so that case routes to QUARANTINED_LOST instead (see
	// TestOnlyCompletePrecedesQuarantinedLost).
	// FAILED: the retry budget is exhausted and this needs a human instead
	// of another automatic attempt.
	assertStateSet(t, "Predecessors(Quarantined)", Predecessors(Quarantined),
		Verifying, Committed, RemoteDeletePending, Failed)
}

// QUARANTINED has exactly two exits, and they answer two different
// questions. DISCOVERED re-ingests: throw the local copy away and fetch the
// artifact again from the remote, which is the right answer when the local
// copy really is bad. COMMITTED reinstates: keep the local copy and trust
// it again, which is the right answer when the local copy is provably
// intact and the remote may be gone (issue #220). Neither is automatic;
// both are operator decisions, and the reinstatement one additionally
// requires evidence that could have failed and forfeits the artifact's
// remote delete permanently (see quarantine.go and remotedelete.go).
func TestQuarantinedHasExits(t *testing.T) {
	exits := Successors(Quarantined)
	if len(exits) == 0 {
		t.Fatal("QUARANTINED has no declared successors; an artifact that's quarantined would be stuck there forever, which is a leak")
	}
	assertStateSet(t, "Successors(Quarantined)", exits, Discovered, Committed)
}

// The hole this whole package exists to close: a quarantined artifact must
// never be able to silently resume the happy path.
//
// Issue #220 added one exit that does return an artifact to a state that
// "looks done", QUARANTINED -> COMMITTED, and it is deliberately not in
// the list below. What keeps the guarantee intact is that the table alone
// is no longer the whole proof for that one edge: nothing in this package
// records it except ReinstateFromQuarantine, which refuses without
// evidence that could have failed (see quarantine.go), and an artifact
// that takes it can never reach REMOTE_DELETE_PENDING again, because
// DeleteRemote refuses every reinstated artifact outright (remotedelete.go,
// TestDeleteRemoteRefusesAnArtifactReinstatedFromQuarantine).
//
// Everything else this test named still holds exactly as it did, including
// the two that matter most: quarantine cannot re-enter the middle of the
// pipeline, and it cannot reach REMOTE_DELETE_PENDING or COMPLETE, the two
// states that stand between an artifact and a destroyed remote source.
func TestQuarantineCannotShortcutToSuccess(t *testing.T) {
	for _, target := range []State{Transferring, Transferred, Verifying, Verified, Committing, RemoteDeletePending, Complete, QuarantinedLost} {
		if err := Validate(Quarantined, target); err == nil {
			t.Errorf("Validate(QUARANTINED, %s) = nil, want an error: quarantine must not shortcut back onto the happy path", target)
		}
	}
}

// --- QUARANTINED_LOST: the terminal, unrecoverable outcome ---

// This is the fix for the gap found reviewing this same issue: COMPLETE
// means the remote source is already deleted, so an artifact whose only
// local copy corrupts after COMPLETE has no source left to recover from.
// Routing that case through the recoverable QUARANTINED -> DISCOVERED exit
// would send the pipeline back to rediscover and re-transfer something
// that no longer exists anywhere, which fails transfer, lands in FAILED,
// and FAILED -> DISCOVERED sends it right back around: a livelock, and
// worse, one that reports an unrecoverable loss as an ordinary retryable
// failure. This test proves that loop cannot form.
func TestCompleteCannotLivelockThroughQuarantine(t *testing.T) {
	// COMPLETE must not reach the recoverable QUARANTINED at all...
	if err := Validate(Complete, Quarantined); err == nil {
		t.Fatal("Validate(COMPLETE, QUARANTINED) accepted; COMPLETE must route to QUARANTINED_LOST, not to the recoverable QUARANTINED")
	}
	// ...it must land in QUARANTINED_LOST instead...
	if err := Validate(Complete, QuarantinedLost); err != nil {
		t.Fatalf("Validate(COMPLETE, QUARANTINED_LOST) = %v, want nil", err)
	}
	// ...and QUARANTINED_LOST must have no way back into the PIPELINE,
	// which is what actually breaks the loop. Its one exit (issue #220) is
	// back to COMPLETE, the state it came from: an operator who can prove
	// the durable local copy is intact after all gets the restore point
	// back. That cannot cycle the way a DISCOVERED exit would. Re-entering
	// COMPLETE re-attempts nothing, since the pipeline stops there and
	// reconciliation's COMPLETE row only re-reads the local copy, and
	// getting there at all needs both a passing check and a deliberate
	// operator action, where the loop this test exists to prevent needed
	// neither.
	assertStateSet(t, "Successors(QuarantinedLost)", Successors(QuarantinedLost), Complete)
	for _, forbidden := range []State{Discovered, Transferring, Transferred, Verifying, Verified, Committing, Committed, RemoteDeletePending, Failed} {
		if err := Validate(QuarantinedLost, forbidden); err == nil {
			t.Errorf("Validate(QUARANTINED_LOST, %s) = nil, want an error: the one exit is back to the state it came from, never into the pipeline", forbidden)
		}
	}
}

// --- no state is a dead end, and two move only when an operator says so ---

// noAutomaticExit lists the states this package deliberately gives no
// AUTOMATIC exit: every edge out of one of them is recorded by an
// operator-triggered use case and by nothing else, never by the cycle, the
// scheduler, or a retry policy. Landing in one is not "the artifact is
// stuck" in the sense the issue calls a design bug; it is a hard stop that
// surfaces as an operator-visible alarm (FR-24) and waits for a human.
//
// This replaced an earlier "terminalByDesign" map holding QUARANTINED_LOST
// alone, when that state had no declared successors at all. Issue #220
// gave it exactly one, back to the COMPLETE it came from, so the property
// worth pinning is no longer "has no exits" but "has no exit anything
// automatic can take", which is the property both quarantine states have
// always actually had. The successor sets themselves are pinned exactly,
// by TestQuarantinedHasExits and TestCompleteCannotLivelockThroughQuarantine,
// so this map cannot quietly absorb a new edge.
var noAutomaticExit = map[State]bool{
	Quarantined:     true,
	QuarantinedLost: true,
}

// "The artifact is stuck" is a design bug per the issue, so prove it can't
// happen: every state has somewhere to go, and the two that only an
// operator can move are exactly the two that say so.
func TestNoStateIsALeak(t *testing.T) {
	for _, s := range AllStates {
		successors := Successors(s)
		if len(successors) == 0 {
			t.Errorf("%q has no declared successors; an artifact reaching it would be stuck there forever with no documented reason", s)
		}
	}
	for s := range noAutomaticExit {
		if !IsQuarantineState(s) {
			t.Errorf("%q is listed as having no automatic exit but is not a quarantine state; the two lists have drifted apart", s)
		}
	}
	for _, s := range AllStates {
		if IsQuarantineState(s) && !noAutomaticExit[s] {
			t.Errorf("%q is a quarantine state but is not listed in noAutomaticExit; every way out of quarantine must be an operator decision", s)
		}
	}
}

// --- Machine: the ergonomic, stateful wrapper ---

func TestNewMachineRejectsUnknownState(t *testing.T) {
	if _, err := NewMachine(State("BOGUS")); err == nil {
		t.Fatal("NewMachine accepted an unknown state")
	}
}

func TestMachineWalksTheNominalPath(t *testing.T) {
	m, err := NewMachine(Discovered)
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	path := []State{Transferring, Transferred, Verifying, Verified, Committing, Committed, RemoteDeletePending, Complete}
	for _, next := range path {
		changed, err := m.Apply(next)
		if err != nil {
			t.Fatalf("Apply(%s) from %s: %v", next, m.Current(), err)
		}
		if !changed {
			t.Fatalf("Apply(%s) reported changed=false on a genuinely new transition", next)
		}
		if m.Current() != next {
			t.Fatalf("after Apply(%s), Current() = %s", next, m.Current())
		}
	}
}

// Re-applying the transition that just happened, the exact shape a
// restarted retry produces, must be a no-op success, not an error.
func TestMachineApplyIsIdempotentOnRepeat(t *testing.T) {
	m, err := NewMachine(Committed)
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	changed, err := m.Apply(RemoteDeletePending)
	if err != nil || !changed {
		t.Fatalf("first Apply(REMOTE_DELETE_PENDING): changed=%v err=%v, want true, nil", changed, err)
	}
	// Simulate the restarted caller retrying the exact same call, not
	// knowing whether it landed before the crash.
	changed, err = m.Apply(RemoteDeletePending)
	if err != nil {
		t.Fatalf("repeat Apply(REMOTE_DELETE_PENDING): unexpected error %v", err)
	}
	if changed {
		t.Fatal("repeat Apply(REMOTE_DELETE_PENDING) reported changed=true, want false: it was already there")
	}
	if m.Current() != RemoteDeletePending {
		t.Fatalf("Current() = %s after idempotent repeat, want REMOTE_DELETE_PENDING", m.Current())
	}
}

// A rejected Apply must not mutate the machine at all, so a caller can
// trust Current() after a failed call.
func TestMachineApplyLeavesStateUntouchedOnRejection(t *testing.T) {
	m, err := NewMachine(Transferring)
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	if changed, err := m.Apply(Complete); err == nil {
		t.Fatal("Apply(COMPLETE) from TRANSFERRING was accepted, want an error")
	} else if changed {
		t.Fatal("rejected Apply reported changed=true")
	}
	if m.Current() != Transferring {
		t.Fatalf("Current() = %s after a rejected Apply, want unchanged TRANSFERRING", m.Current())
	}
}
