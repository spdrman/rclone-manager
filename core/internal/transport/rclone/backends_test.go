package rclone

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/spdrman/rclone-manager/core/internal/backend"
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

// TestEveryBundledManifestNamesABackendThisBinaryRegisters is issue
// #665's own cross-package pin, test 25: every manifest's
// rclone_backend, and every member of backend.SupportedRcloneBackends,
// has to resolve against the LIVE fs.Registry - the same registry
// TestRegisteredBackendsExactSet checks the whole binary's set against,
// not against a list either package could drift from independently.
//
// This is deliberately a SUBSET check, not the exact-set comparison
// above: backend.SupportedRcloneBackends names the backends a
// DESTINATION may be declared on, and the binary also registers
// backends nothing may be declared on at all (the transitive ones
// AcceptedTransitiveBackends records, crypt among them), so it must
// resolve against what the binary registers without needing to equal
// it. Since #731 it names the same three as RequiredBackends, which is
// TestSupportedRcloneBackendsIsItsOwnReviewedList's business rather
// than this test's.
func TestEveryBundledManifestNamesABackendThisBinaryRegisters(t *testing.T) {
	registered := map[string]bool{}
	for _, name := range RegisteredBackendNames() {
		registered[name] = true
	}
	if len(registered) == 0 {
		t.Fatal("RegisteredBackendNames() returned nothing, so this test checks nothing")
	}

	for name := range backend.SupportedRcloneBackends {
		if !registered[name] {
			t.Errorf("backend.SupportedRcloneBackends names %q, and this binary's live fs.Registry does not register it", name)
		}
	}

	reg, err := backend.Bundled()
	if err != nil {
		t.Fatalf("backend.Bundled(): %v", err)
	}
	checked := 0
	for _, id := range reg.IDs() {
		m, err := reg.Backend(id)
		if err != nil {
			t.Fatalf("backend.Backend(%q): %v", id, err)
		}
		checked++
		if !registered[m.RcloneBackend] {
			t.Errorf("manifest %q declares rclone_backend %q, and this binary's live fs.Registry does not register it", id, m.RcloneBackend)
		}
		if !backend.SupportedRcloneBackends[m.RcloneBackend] {
			t.Errorf("manifest %q declares rclone_backend %q, which is not in backend.SupportedRcloneBackends", id, m.RcloneBackend)
		}
	}
	if checked == 0 {
		t.Fatal("the bundled registry declared no backend, so this test checked nothing")
	}
}

// TestSupportedRcloneBackendsIsItsOwnReviewedList pins section 4.2's own
// claim: backend.SupportedRcloneBackends is the list of backends a
// DESTINATION may be declared on, it must resolve as a subset of
// RequiredBackends, and every member of it is a reviewed decision rather
// than something that arrived because the backend was already linked for
// another reason.
//
// Until #731 that second half was checked by refusing a
// SupportedRcloneBackends that had become equal to RequiredBackends,
// which was a proxy for "somebody reused that list instead of writing
// this one". #731 is the reviewed diff section 4.2 asked for - sftp,
// required as a SOURCE backend since FR-4 and now offered as a
// destination too - so the two lists hold the same three names and the
// proxy expired. What replaces it is the pin itself: the set is written
// out here, a fourth destination backend fails this test by name, and
// "reuse RequiredBackends" is not even reachable, since
// core/internal/backend may import nothing from this package (backend/
// doc.go's import rule, enforced by the cycle this file's own import
// would create).
func TestSupportedRcloneBackendsIsItsOwnReviewedList(t *testing.T) {
	required := map[string]bool{}
	for _, name := range RequiredBackends {
		required[name] = true
	}
	for name := range backend.SupportedRcloneBackends {
		if !required[name] {
			t.Errorf("backend.SupportedRcloneBackends names %q, which is not even in RequiredBackends", name)
		}
	}

	want := []string{"local", "s3", "sftp"}
	if got := sortedKeys(backend.SupportedRcloneBackends); !reflect.DeepEqual(got, want) {
		t.Errorf("backend.SupportedRcloneBackends = %v, want %v.\n"+
			"A destination backend is its own decision: a name added here is a place this product will write "+
			"backups to, and it stays a reviewed diff (issue #665 section 4.2) even when the rclone backend "+
			"behind it is already registered for a source.", got, want)
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
