package rclone

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
)

// This file is what makes FR-4's "each backend is an architecture
// decision, not an import line" enforceable rather than aspirational.
//
// rclone registers backends through package-level init, so the set this
// binary supports is decided by the blank imports anywhere in its
// dependency graph and is visible nowhere in this repository's own code
// except backends.go's list. Worse, a backend can arrive without anyone
// writing an import for it: crypt is registered today because
// fs/operations imports it to decrypt names for ListJSON, and
// fs/operations is a dependency this adapter genuinely needs.
//
// So the check is against the LIVE registry rather than against the source,
// and it is an exact-set comparison rather than a subset one in either
// direction. A widening fails, which is the point; a narrowing fails too,
// because a backend disappearing from the registry means an import this
// product depends on has gone away and every source of that type has
// silently stopped working.

// TestRegisteredBackendsExactSet is the enforcement FR-4 asks for: the set
// of rclone backends this binary registers at runtime must match
// ExpectedBackends() exactly, not "at least" or "at most". A new blank
// import anywhere in this package that registers another backend, whether
// directly or transitively the way crypt currently arrives through
// fs/operations, changes fs.Registry and fails this test. That turns a
// silent widening of the configuration/dependency surface into a build
// failure someone has to look at and either revert or consciously accept
// in backends.go.
func TestRegisteredBackendsExactSet(t *testing.T) {
	got := RegisteredBackendNames()
	want := ExpectedBackends()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered rclone backends changed:\n  got:  %v\n  want: %v\n"+
			"if this widening is intentional, add the new name (with a reason, "+
			"if it's transitive) to RequiredBackends or AcceptedTransitiveBackends "+
			"in backends.go; if it's not intentional, find and remove whatever "+
			"import pulled it in",
			got, want)
	}
}

// TestAcceptedTransitiveBackendsAreDocumented makes sure nothing gets added
// to AcceptedTransitiveBackends without a real reason. An empty or
// whitespace-only entry would let a future backend widen the registered set
// silently, which is exactly what TestRegisteredBackendsExactSet exists to
// prevent.
func TestAcceptedTransitiveBackendsAreDocumented(t *testing.T) {
	for name, reason := range AcceptedTransitiveBackends {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("AcceptedTransitiveBackends[%q] has no reason recorded", name)
		}
	}
}

// TestRequiredBackendsResolve checks that every backend FR-4 actually asks
// for resolves through rclone's own lookup, the same fs.Find call fsFor
// uses to turn a configured source type into a backend. This is a
// registration check, not a behavioral one: it does not exercise fsFor or
// build a working Fs, it only confirms the name is known to the registry.
func TestRequiredBackendsResolve(t *testing.T) {
	for _, name := range RequiredBackends {
		if _, err := fs.Find(name); err != nil {
			t.Errorf("required backend %q did not resolve: %v", name, err)
		}
	}
}
