package lifecycle

import "testing"

// The rendered string is a contract with the FR-9 journal, which stores it
// as a plain string column and was told not to define this type itself.
// Renaming a constant's underlying string without meaning to would silently
// break that contract, so pin every one of them explicitly.
func TestStateStringIsExactlyTheContractName(t *testing.T) {
	for _, tc := range []struct {
		state State
		want  string
	}{
		{Discovered, "DISCOVERED"},
		{Transferring, "TRANSFERRING"},
		{Transferred, "TRANSFERRED"},
		{Verifying, "VERIFYING"},
		{Verified, "VERIFIED"},
		{Committing, "COMMITTING"},
		{Committed, "COMMITTED"},
		{RemoteDeletePending, "REMOTE_DELETE_PENDING"},
		{Complete, "COMPLETE"},
		{Failed, "FAILED"},
		{Quarantined, "QUARANTINED"},
		{QuarantinedLost, "QUARANTINED_LOST"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("%#v.String() = %q, want %q", tc.state, got, tc.want)
		}
		if string(tc.state) != tc.want {
			t.Errorf("string(%#v) = %q, want %q", tc.state, string(tc.state), tc.want)
		}
	}
}

func TestAllStatesAreValidAndDistinct(t *testing.T) {
	seen := map[State]bool{}
	for _, s := range AllStates {
		if !s.Valid() {
			t.Errorf("%q is in AllStates but Valid() is false", s)
		}
		if seen[s] {
			t.Errorf("%q appears more than once in AllStates", s)
		}
		seen[s] = true
	}
	// FR-10 names 11 states; QUARANTINED_LOST is this package's addition
	// (see the package doc and TestCompleteCannotLivelockThroughQuarantine
	// in machine_test.go), which makes 12.
	if len(AllStates) != 12 {
		t.Fatalf("AllStates has %d states, want 12 (11 named by FR-10 plus QUARANTINED_LOST)", len(AllStates))
	}
}

func TestUnknownStateIsInvalid(t *testing.T) {
	for _, raw := range []string{"", "discovered", "COMPLETED", "COMPLETE ", " COMPLETE", "BOGUS"} {
		if State(raw).Valid() {
			t.Errorf("State(%q).Valid() = true, want false", raw)
		}
	}
}

func TestParseStateRoundTrips(t *testing.T) {
	for _, s := range AllStates {
		got, err := ParseState(s.String())
		if err != nil {
			t.Fatalf("ParseState(%q): %v", s, err)
		}
		if got != s {
			t.Fatalf("ParseState(%q) = %q, want %q", s, got, s)
		}
	}
}

func TestParseStateRejectsUnknown(t *testing.T) {
	for _, raw := range []string{"", "discovered", "IN_PROGRESS", "COMPLETE\x00"} {
		if _, err := ParseState(raw); err == nil {
			t.Errorf("ParseState(%q) accepted junk", raw)
		} else if _, ok := err.(*UnknownStateError); !ok {
			t.Errorf("ParseState(%q) error is %T, want *UnknownStateError", raw, err)
		}
	}
}
