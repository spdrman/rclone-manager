package lifecycle

import "testing"

// TestEveryStateIsClassifiedAsHoldingALocalCopyOrNot is the drift guard the
// whole file exists for. A thirteenth state added to AllStates and to
// neither list would otherwise be silently treated as occupying nothing,
// which under-counts this manager's own usage, which is how an enforced cap
// stops enforcing.
func TestEveryStateIsClassifiedAsHoldingALocalCopyOrNot(t *testing.T) {
	seen := map[State]int{}
	for _, s := range StatesHoldingLocalCopy {
		seen[s]++
	}
	for _, s := range StatesWithNoLocalCopy {
		seen[s]++
	}

	for _, s := range AllStates {
		switch seen[s] {
		case 0:
			t.Errorf("state %s is in neither StatesHoldingLocalCopy nor StatesWithNoLocalCopy: classify it, or the capacity cap will silently not count it", s)
		case 1:
		default:
			t.Errorf("state %s is classified %d times", s, seen[s])
		}
		delete(seen, s)
	}
	for s := range seen {
		t.Errorf("state %s is classified but is not in AllStates", s)
	}
}

// TestDiscoveredAndFailedHoldNothing pins the two judgement calls by name,
// so a change to either is a deliberate edit to a test rather than a
// number moving.
func TestDiscoveredAndFailedHoldNothing(t *testing.T) {
	if HoldsLocalCopy(Discovered) {
		t.Error("Discovered holds a local copy: nothing has been written yet at that point")
	}
	if HoldsLocalCopy(Failed) {
		t.Error("Failed holds a local copy: transfer.go clears a stale .partial before every attempt")
	}
}

// TestQuarantineStillOccupiesTheDisk is the one most likely to be got
// wrong. A quarantined artifact is not trusted, which is a different thing
// from not being there, and issue #220's reinstatement path only makes
// sense because the bytes are still on the disk.
func TestQuarantineStillOccupiesTheDisk(t *testing.T) {
	for _, s := range []State{Quarantined, QuarantinedLost} {
		if !HoldsLocalCopy(s) {
			t.Errorf("%s does not hold a local copy: a quarantined artifact's local file is deliberately retained for a human", s)
		}
	}
}

// TestTransferringOnwardsHoldALocalCopy walks the happy path, since the
// first written byte is the whole boundary this file draws.
func TestTransferringOnwardsHoldALocalCopy(t *testing.T) {
	for _, s := range []State{Transferring, Transferred, Verifying, Verified, Committing, Committed, RemoteDeletePending, Complete} {
		if !HoldsLocalCopy(s) {
			t.Errorf("%s does not hold a local copy", s)
		}
	}
}

// TestAnUnknownStateClaimsNothing: this package will not assert disk usage
// for a state it does not define.
func TestAnUnknownStateClaimsNothing(t *testing.T) {
	if HoldsLocalCopy(State("NOT_A_STATE")) {
		t.Error("an unrecognised state was reported as holding a local copy")
	}
}

// TestStatesHoldingLocalCopyStringsMatchesTheStateList keeps the journal-
// facing spelling from drifting away from the typed one.
func TestStatesHoldingLocalCopyStringsMatchesTheStateList(t *testing.T) {
	got := StatesHoldingLocalCopyStrings()
	if len(got) != len(StatesHoldingLocalCopy) {
		t.Fatalf("StatesHoldingLocalCopyStrings() has %d entries, want %d", len(got), len(StatesHoldingLocalCopy))
	}
	for i, s := range StatesHoldingLocalCopy {
		if got[i] != string(s) {
			t.Errorf("entry %d = %q, want %q", i, got[i], s)
		}
	}
}
