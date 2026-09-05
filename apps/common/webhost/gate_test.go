// One test, and its value is in what it forbids rather than what it
// checks.
//
// It proves the shipped gate reports false with no constructor argument,
// no flag and no environment variable able to change that. If somebody
// later adds a way to flip it, this test does not fail, because there is
// nothing here for a new switch to break; what it does is stand next to
// gate.go as the record that "always false" is the specified behaviour and
// not an unfinished branch, so a reviewer reading a PR that adds a switch
// has something to point at.
package webhost

import "testing"

// TestNotYetImplementedGate_AlwaysReportsNotPassed pins down the
// INTEGRATION requirement from issue #94: until #92 (B1.3) lands and
// proves a trusted-proxy identity check on real hardware, the only
// DestructiveGate this repository ships MUST report false, unconditionally
// — no environment variable, config flag, or constructor argument can make
// it report true. That is what "fail closed by construction" means here:
// there is nothing to flip.
func TestNotYetImplementedGate_AlwaysReportsNotPassed(t *testing.T) {
	var g NotYetImplementedGate
	if g.Passed() {
		t.Fatal("NotYetImplementedGate.Passed() = true, want false")
	}
}
