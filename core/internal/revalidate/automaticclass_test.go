package revalidate

import (
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/placement"
)

// TestTheAutomaticCeilingIsTheSameRuleInBothPlaces holds the two statements
// of FR-31's automatic ceiling together.
//
// There are two. This package has a constant, and placement.AutomaticClass
// derives the same answer from Class.CostsEgress. AutomaticClass's own doc
// says why it derives rather than names: "raising the automatic ceiling
// means changing the class whose CostsEgress is consulted, not editing a
// constant and finding out from a bill". Nothing consults it, so the
// constant is what the product actually runs, and the derivation was a
// second opinion nobody could disagree with because nobody asked it.
//
// This asks it, for every access state a copy this pass would look at can
// be in, and fails if the two ever say different things. That is the same
// shape internal/archive's class table uses to stay pinned to the closed
// set internal/config accepts, and it is worth having for the same reason:
// two places holding one rule is fine, two places holding one rule with
// nothing checking is how they drift.
//
// Unreachable is not in the list on purpose. AutomaticClass answers the
// empty class for it, meaning nothing can be attempted at all, and this
// pass never learns that a medium is unreachable before it asks: it finds
// out from the request failing, and routes that as a per-artifact error
// rather than as a verdict.
func TestTheAutomaticCeilingIsTheSameRuleInBothPlaces(t *testing.T) {
	for _, s := range []archive.State{archive.Immediate, archive.RequiresRestore, archive.Restoring} {
		got := placement.AutomaticClass(s)
		if got != automaticMediumClass {
			t.Errorf("placement.AutomaticClass(%s) is %q and this package runs %q against a medium copy; "+
				"one of the two has been raised without the other, and the one that decides the bill is this package's",
				s, got, automaticMediumClass)
		}
	}

	// The control. If the constant ever stops being a class that costs
	// nothing, the check above still passes as long as both sides moved
	// together, and both moving together to a class that downloads is
	// exactly the surprise bill FR-31 is about.
	if automaticMediumClass.CostsEgress() {
		t.Fatalf("the automatic class is %q, which downloads the object; FR-31 makes anything that costs egress operator-initiated",
			automaticMediumClass)
	}
}
