// These cover the graph, and most of them are written as properties of the
// TABLE rather than as scenarios.
//
// That follows from machine.go's central choice. Because legality is data
// and not code, a test can walk every state and every edge and make a
// complete statement, which is a stronger thing than a list of examples:
// TestFailedIsUnreachableOnceCommitted, say, is not three cases that happen
// to be refused, it is the claim that there is no way at all to fail out of
// a committed backup.
//
// Several tests exist to catch the failure that a table makes easy, which is
// a state or an edge added without anyone thinking about the rest of the
// graph. TestEveryStateParticipatesInTheGraph and
// TestTransitionsTableIsWellFormed are that guard, and the entry-point and
// exit tests below pin the individual answers so a new edge into
// QUARANTINED, say, has to be argued rather than merely appended.
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

// sortedStrings renders a state slice for a failure message.
//
// The sort is for the message only, never for the comparison: these
// assertions are about set membership, and sorting the two sides and
// comparing them would additionally pin an order the graph does not
// promise. What it buys is that two failures of the same test print the same
// way, so a diff between runs is about the states and not about their
// order.
func sortedStrings(states []State) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	sort.Strings(out)
	return out
}

// assertStateSet compares two state slices as sets and reports every
// difference, in both directions, in one run.
//
// Reporting both directions matters more than it looks. An edge added to the
// table and an edge removed from it are different mistakes with different
// fixes, and a helper that stopped at the first discrepancy would make a
// change that did both look like only one of them. The duplicate check is
// separate for the same reason: a duplicated member makes the set smaller
// than the slice, which would otherwise hide a genuinely missing state
// behind a coincidentally matching count.
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

// TestTransitionsTableIsWellFormed checks the table's own hygiene before
// any test reads meaning out of it.
//
// The self-loop refusal is the one carrying a decision rather than a
// sanity check. Validate treats current == target as an idempotent no-op
// whether or not the pair is declared, because a crash matrix that kills the
// process after every state means a step routinely retries a move that
// already landed. Declaring self-loops as well would be a second statement
// of the same rule, and the two would drift the first time somebody added a
// state and remembered only one of them.
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

// TestEveryStateParticipatesInTheGraph catches a state constant that was
// declared and then never wired up. Such a state is not inert: Valid reports
// true for it, so the journal would accept it in a row and ParseState would
// read it back, while nothing could ever legally enter or leave it. An
// artifact that reached it would be stuck with no way forward, which is the
// one outcome this machine is built to make impossible.
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

// TestValidateRejectsUnknownStates checks both argument positions, because
// they fail for different reasons and one implementation could easily
// validate only the target.
//
// An unknown CURRENT state is the more important half: that is what a
// corrupted or drifted journal row looks like, and treating it as merely
// "not in the table" would report an illegal transition when the real
// problem is that the row does not say anything this build understands. The
// error type is asserted for that reason, since callers route on it.
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

// TestFailedEntryPoints pins the exact set, not just that some states can
// fail.
//
// Pinning the whole set is what makes the companion test below meaningful:
// together they say FAILED is reachable from precisely the six states before
// COMMITTED and from nowhere else, which is a claim about the entire graph
// rather than about six examples.
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

// TestFailedHasExits checks for emptiness first and then for the exact set,
// and the order is deliberate: an empty result would satisfy the set
// comparison's "no unexpected members" half, so without the length check the
// worst possible outcome, a FAILED artifact with nowhere to go, would pass
// half of this test.
func TestFailedHasExits(t *testing.T) {
	exits := Successors(Failed)
	if len(exits) == 0 {
		t.Fatal("FAILED has no declared successors; an artifact that fails would be stuck there forever")
	}
	assertStateSet(t, "Successors(Failed)", exits, Discovered, Quarantined)
}

// --- QUARANTINED: defined entry points, defined exits, no shortcut back to success ---

// TestQuarantinedEntryPoints pins which states may quarantine, with the
// reasoning for each written out inline because the absences carry as much
// weight as the presences: COMPLETE is excluded here on purpose, since by
// then the remote is confirmed gone and that case has to route to
// QUARANTINED_LOST instead.
func TestQuarantinedEntryPoints(t *testing.T) {
	// VERIFYING: a validator found the content itself invalid.
	// COMMITTED / REMOTE_DELETE_PENDING: reconciliation found the durable,
	// final-named local copy corrupted after the fact, but before the
	// remote delete has actually happened, so a source may still exist.
	// REMOTE_RETAINED (issue #315): the same finding, for a retained,
	// read-only-source artifact this manager was never going to delete
	// the remote copy of anyway; the remote is presumptively still there
	// since it was never touched, on purpose.
	// COMPLETE is deliberately excluded here: by then the remote is
	// confirmed gone, so that case routes to QUARANTINED_LOST instead (see
	// TestOnlyCompletePrecedesQuarantinedLost).
	// FAILED: the retry budget is exhausted and this needs a human instead
	// of another automatic attempt.
	assertStateSet(t, "Predecessors(Quarantined)", Predecessors(Quarantined),
		Verifying, Committed, RemoteDeletePending, RemoteRetained, Failed)
}

// QUARANTINED has exactly three exits, and they answer two different
// questions. DISCOVERED re-ingests: throw the local copy away and fetch the
// artifact again from the remote, which is the right answer when the local
// copy really is bad. COMMITTED and REMOTE_RETAINED (issue #315) each
// reinstate: keep the local copy and trust it again, which is the right
// answer when the local copy is provably intact and the remote may be gone
// (issue #220) or is retained by policy and was never examined (issue
// #315). Neither reinstatement target is automatic; both are operator
// decisions gated on evidence that could have failed, and both forfeit the
// artifact's remote delete permanently (see quarantine.go and
// remotedelete.go). Which of the two applies to one specific artifact is
// resolved per artifact, from its own history, not by this table: see
// quarantine.go's quarantineOrigins.
func TestQuarantinedHasExits(t *testing.T) {
	exits := Successors(Quarantined)
	if len(exits) == 0 {
		t.Fatal("QUARANTINED has no declared successors; an artifact that's quarantined would be stuck there forever, which is a leak")
	}
	assertStateSet(t, "Successors(Quarantined)", exits, Discovered, Committed, RemoteRetained)
}

// HasReinstatementExit only promises existence, never a resolved target
// (see its own doc for why that is all it is safe to promise now that
// QUARANTINED declares two reinstatement edges). This pins down that
// existence answer for both quarantine states and for a sample of states
// that were never quarantined at all.
func TestHasReinstatementExit(t *testing.T) {
	cases := []struct {
		from State
		want bool
	}{
		{Quarantined, true},
		{QuarantinedLost, true},
		{Discovered, false},
		{Verifying, false},
		{Failed, false},
		{Committed, false},
		{Complete, false},
	}
	for _, c := range cases {
		if got := HasReinstatementExit(c.from); got != c.want {
			t.Errorf("HasReinstatementExit(%s) = %v, want %v", c.from, got, c.want)
		}
	}
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

// TestNewMachineRejectsUnknownState pins that a Machine cannot be built
// around a state name nobody defined. The constructor is where a value read
// out of the journal becomes a walkable position, so accepting an
// unrecognised one would mean every later Apply reasoned about a state the
// table has no rows for and refused everything for the wrong reason.
func TestNewMachineRejectsUnknownState(t *testing.T) {
	if _, err := NewMachine(State("BOGUS")); err == nil {
		t.Fatal("NewMachine accepted an unknown state")
	}
}

// TestMachineWalksTheNominalPath drives DISCOVERED to COMPLETE one step at
// a time, which is the happy path FR-11 describes.
//
// The changed flag is asserted on every step, not just the final state. That
// is what separates this from the idempotence test below it: a genuinely new
// move must report changed, and a repeat must not, and an implementation
// that always reported one or the other would satisfy exactly one of the two
// tests.
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
