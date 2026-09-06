// This file holds one test, about one constant, and it is its own file
// because of what it replaced.
//
// The rule it guards, how strong a check an unattended pass may run against
// a copy on a storage medium, used to be stated twice: once as the constant
// this package runs, and once as placement.AutomaticClass, which nothing
// consulted and which did not derive what it claimed to. #438 deleted the
// second statement, and the argument for why one statement is enough is
// long enough to want somewhere to live. It lives here, next to the
// assertions that make it true, rather than buried among the pass-level
// tests in medium_test.go where the constant is only visible through its
// effects.
package revalidate

import (
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/placement"
)

// TestTheAutomaticCeilingIsAClassThisPassMayRunAgainstEveryMedium is
// FR-31's automatic ceiling, stated as three properties of the constant
// this package runs rather than as a second copy of it somewhere else.
//
// It replaces a test that pinned this constant against
// placement.AutomaticClass, which #438 deleted. That function claimed to
// DERIVE the ceiling from Class.CostsEgress, so that raising the ceiling
// meant changing the class whose cost is consulted rather than editing a
// constant and finding out from a bill. It did not derive it: it read the
// ceiling, found that it costs egress, and returned the literal Existence,
// skipping Attested entirely. So the pin held this constant against a
// second constant wearing a derivation's clothes, and losing it costs
// nothing as long as the properties that actually matter are asserted
// here, where the class is run and where the money is spent.
//
// The three, and Attested is the interesting one:
//
//  1. it has to be a rung. The empty class is what a placement nothing
//     verified carries, and a pass must never record one as an
//     achievement;
//  2. it must cost no egress. A scheduled pass that downloads is a
//     surprise bill, and a surprise bill is how an operator learns to
//     turn a safety feature off;
//  3. it must not be a class whose answer is the endpoint's own word.
//     That rules out Attested, which costs no egress at all and is
//     therefore exactly what a derivation written as "the strongest rung
//     that does not download" would pick. Attested is opt-in per medium
//     precisely because it trusts the endpoint's checksum, and
//     checkMediumPlacements runs one class against every ACTIVE medium
//     copy without ever consulting what that copy's medium opted into. An
//     automatic pass that attested would be trusting an endpoint on
//     behalf of an operator who never said it could.
//
// These are properties of the constant. The behaviour they describe is
// asserted against a real pass, by request count, in
// TestRevalidationOfAMediumPlacementIsExistenceAndSaysSo: zero downloads,
// zero attestations, one HEAD.
func TestTheAutomaticCeilingIsAClassThisPassMayRunAgainstEveryMedium(t *testing.T) {
	if !automaticMediumClass.Valid() {
		t.Fatalf("the automatic class is %q, which is not one of %v; the empty class is what an unverified placement carries and it is deliberately not a rung",
			automaticMediumClass, placement.Classes)
	}
	if automaticMediumClass.CostsEgress() {
		t.Errorf("the automatic class is %q, which downloads the object; FR-31 makes anything that costs egress operator-initiated",
			automaticMediumClass)
	}
	if automaticMediumClass == placement.Attested {
		t.Errorf("the automatic class is %q, which is the endpoint's own word about the bytes and is opt-in per medium; "+
			"this pass runs one class against every medium copy and never reads that opt-in, so it cannot be the class that needs it",
			automaticMediumClass)
	}
}
