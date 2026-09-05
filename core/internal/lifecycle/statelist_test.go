package lifecycle

import (
	"strings"
	"testing"
)

// TestNameSetReadsDownTheLifecycle pins the order, because the order is the
// only thing about this rendering a caller cannot check for itself.
//
// Map iteration in Go is deliberately random, so a NameSet that just ranged
// over the map would put a different sentence in front of an operator on
// every run, and two log lines from the same binary would disagree about
// the same set. AllStates order also happens to be the order the package
// doc's own diagram lists them in.
func TestNameSetReadsDownTheLifecycle(t *testing.T) {
	got := NameSet(map[State]bool{
		RemoteRetained:      true,
		Committed:           true,
		Complete:            true,
		RemoteDeletePending: true,
	})
	want := "COMMITTED, REMOTE_DELETE_PENDING, COMPLETE or REMOTE_RETAINED"
	if got != want {
		t.Errorf("NameSet() = %q, want %q", got, want)
	}

	for i := 0; i < 50; i++ {
		if again := NameSet(durableRestorePoints); again != want {
			t.Fatalf("NameSet() = %q on run %d, want %q: the order is following map iteration",
				again, i, want)
		}
	}
}

// TestNameSetRendersEveryMemberItIsGiven is the one property everything
// built on this depends on. A renderer that quietly drops a member is
// exactly the defect issue #505 filed, one layer further down.
func TestNameSetRendersEveryMemberItIsGiven(t *testing.T) {
	for _, s := range AllStates {
		set := map[State]bool{Committed: true, s: true}
		got := NameSet(set)
		if !strings.Contains(got, string(s)) {
			t.Errorf("NameSet(%v) = %q, which does not name %s", set, got, s)
		}
	}

	// A false value is not a member. Rendering it would make every
	// "accepts" sentence built from a set wrong in the other direction.
	got := NameSet(map[State]bool{Committed: true, Failed: false})
	if got != "COMMITTED" {
		t.Errorf("NameSet() = %q, want %q: a false entry is not a member of the set", got, "COMMITTED")
	}
}

// TestNameSetKeepsAStateItDoesNotRecognize is about the failure mode that
// would be worst to hide.
//
// A set holding a string this package does not define is a bug somewhere
// upstream: a hand-built map, a journal row that drifted, a rename that
// only landed in one place. Dropping it from the sentence would leave a
// message that reads as complete and is not, which is the whole shape of
// defect this function exists to end, so it is appended instead.
func TestNameSetKeepsAStateItDoesNotRecognize(t *testing.T) {
	got := NameSet(map[State]bool{Committed: true, State("ARCHIVED"): true})
	if !strings.Contains(got, "ARCHIVED") {
		t.Errorf("NameSet() = %q, which silently dropped a member this package does not define", got)
	}
	if !strings.Contains(got, "COMMITTED") {
		t.Errorf("NameSet() = %q, which lost a real state alongside the unknown one", got)
	}
}

// TestNameSetOfNothingIsNothing states the empty case rather than leaving
// the next reader to find out by accident. No caller should ever hold an
// empty set (a predicate that accepts nothing is its own defect) and
// inventing a placeholder here would only hide that one layer further in.
func TestNameSetOfNothingIsNothing(t *testing.T) {
	if got := NameSet(nil); got != "" {
		t.Errorf("NameSet(nil) = %q, want the empty string", got)
	}
	if got := NameSet(map[State]bool{Complete: false}); got != "" {
		t.Errorf("NameSet() = %q, want the empty string for a set whose only entry is false", got)
	}
}

// TestDurableRestorePointNamesIsTheMapNextToIt keeps the exported rendering
// internal/metrics quotes honest against the map it claims to render.
func TestDurableRestorePointNamesIsTheMapNextToIt(t *testing.T) {
	got := DurableRestorePointNames()
	for _, s := range AllStates {
		named := strings.Contains(got, string(s))
		if want := IsDurableRestorePoint(s); named != want {
			t.Errorf("DurableRestorePointNames() = %q: names %s = %v, but IsDurableRestorePoint(%s) = %v",
				got, s, named, s, want)
		}
	}
}
