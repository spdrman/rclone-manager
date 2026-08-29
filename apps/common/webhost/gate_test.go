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
