package lifecycle

import "testing"

// Which states mean the pipeline could not produce a valid backup
// (issue #625).
//
// The names have been in this package's own doc since it was written,
// marked "(exceptional)" beside the three of them, and the set they form
// was nowhere: internal/obs had no way to log a transition into one at a
// severity that says so, and the Web UI carried its own literal copy of
// the three strings to colour a line by. A second list of a closed
// vocabulary is a list that drifts, which is the argument
// IsQuarantineState and IsDurableRestorePoint beside it already make.

// TestExceptionalStatesAreExactlyTheThreeTheDocNames is checked against
// AllStates rather than against a second literal, so a fourteenth state
// added to the machine has to be classified here rather than quietly
// counting as an ordinary one.
func TestExceptionalStatesAreExactlyTheThreeTheDocNames(t *testing.T) {
	want := map[State]bool{Failed: true, Quarantined: true, QuarantinedLost: true}
	for _, s := range AllStates {
		if got := IsExceptionalState(s); got != want[s] {
			t.Errorf("IsExceptionalState(%s) = %v, want %v", s, got, want[s])
		}
	}
	for s := range want {
		if !s.Valid() {
			t.Errorf("%s is named as exceptional and is not a state this package defines", s)
		}
	}
}

// TestEveryQuarantineStateIsExceptional keeps the two sets from drifting
// apart. The quarantine states are a subset of the exceptional ones by
// construction, and this is what says so out loud: an artifact held for a
// human because its content is suspect has, by definition, not produced a
// backup this pipeline may trust.
func TestEveryQuarantineStateIsExceptional(t *testing.T) {
	for _, s := range AllStates {
		if IsQuarantineState(s) && !IsExceptionalState(s) {
			t.Errorf("%s is a quarantine state and is not counted as exceptional", s)
		}
	}
}

// TestNoDurableRestorePointIsExceptional is the negative control the two
// tests above cannot give on their own. A state cannot be both a place a
// usable backup lives and a place an attempt failed, and a predicate that
// said yes to everything would pass every assertion above.
func TestNoDurableRestorePointIsExceptional(t *testing.T) {
	overlap := 0
	for _, s := range AllStates {
		if IsDurableRestorePoint(s) && IsExceptionalState(s) {
			t.Errorf("%s counts as both a durable restore point and an exceptional state", s)
			overlap++
		}
	}
	if overlap == 0 && len(AllStates) == 0 {
		t.Fatal("AllStates is empty, so this comparison passed vacuously")
	}
}
