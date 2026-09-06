// These cover the vocabulary in state.go, and they exist because the string
// forms are a contract rather than an implementation detail.
//
// The FR-9 journal stores a state as a plain string column, so every row
// already on disk holds one of these literals, and every log line an
// operator has read quotes one. Renaming a constant's underlying string is
// therefore a data migration and an FR-35 conversation, not a rename, and
// the point of this file is that such a change cannot happen quietly.
//
// The tests are deliberately mechanical: the literals are written out again
// here rather than derived from the constants, since a test that computed
// the expected value the same way the code does could not detect the code
// changing it.
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
		{RemoteRetained, "REMOTE_RETAINED"},
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

// TestAllStatesAreValidAndDistinct is the guard on the derived lookups.
//
// validStates, holdsLocalCopy and several tests are all built from
// AllStates, so a constant declared and not added to that slice is invisible
// everywhere: Valid would report false for a state the package defines, and
// the capacity guard would stop counting it. The count assertion is what
// catches that, which is why it carries the arithmetic in its message rather
// than just a number.
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
	// in machine_test.go), which makes 12; REMOTE_RETAINED is issue #282's
	// addition, which makes 13.
	if len(AllStates) != 13 {
		t.Fatalf("AllStates has %d states, want 13 (11 named by FR-10, plus QUARANTINED_LOST, plus REMOTE_RETAINED)", len(AllStates))
	}
}

// TestUnknownStateIsInvalid is the negative control, and the inputs are
// chosen to be near misses rather than obvious junk.
//
// Lower case, a plausible alternative spelling, and the same name with a
// leading or trailing space are the three ways a real bad value arrives: a
// hand-edited row, a hand-written migration, or a value that went through a
// text field somewhere. All three have to be refused, because a state
// comparison that trimmed or folded case would make two different rows read
// as the same one.
func TestUnknownStateIsInvalid(t *testing.T) {
	for _, raw := range []string{"", "discovered", "COMPLETED", "COMPLETE ", " COMPLETE", "BOGUS"} {
		if State(raw).Valid() {
			t.Errorf("State(%q).Valid() = true, want false", raw)
		}
	}
}

// TestParseStateRoundTrips walks AllStates rather than a fixed list, so a
// state added to the package is covered without anyone remembering to
// extend this. It is the positive control for the rejection test below,
// which on its own would be satisfied by a parser that refused
// everything.
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

// TestParseStateRejectsUnknown checks the error TYPE as well as its
// presence, because callers route on it: ParseState is what reads a value
// back out of the journal, and *UnknownStateError is how a schema drift or a
// build mismatch is told apart from an ordinary failure.
//
// The trailing NUL is the interesting input. It is what a value that came
// through a C string boundary or a corrupted column looks like, and it is
// invisible in a log line, so a parser that accepted it would produce a
// state that prints identically to a legitimate one and compares unequal to
// it.
func TestParseStateRejectsUnknown(t *testing.T) {
	for _, raw := range []string{"", "discovered", "IN_PROGRESS", "COMPLETE\x00"} {
		if _, err := ParseState(raw); err == nil {
			t.Errorf("ParseState(%q) accepted junk", raw)
		} else if _, ok := err.(*UnknownStateError); !ok {
			t.Errorf("ParseState(%q) error is %T, want *UnknownStateError", raw, err)
		}
	}
}
