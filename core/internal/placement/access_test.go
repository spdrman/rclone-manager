package placement

import (
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The tests in this file are EPIC E, FR-34's read half (#240). What they
// are protecting against is one specific failure, and it is worth naming
// because this repository has shipped it once already in a different
// medium: issue #361 was a run cycle that backed nothing up and reported
// success. The same defect in a surface is a placement row that reads
// "stored on offsite_s3" for a copy nobody can reach or nobody has
// checked. An operator acts on a green tick, and the moment they find out
// it was decorative is the moment they needed the backup.

// TestAccessOf_ALocalCopyIsReadableNow is the ordinary case, and the one
// every deployment written before EPIC E has for every artifact after
// migration 0007's backfill.
func TestAccessOf_ALocalCopyIsReadableNow(t *testing.T) {
	local := state.Placement{Medium: state.MediumLocal, Location: "/srv/backups/a.dump", Status: state.PlacementActive}
	if got := AccessOf(local, MediumFacts{}); got != AccessImmediate {
		t.Errorf("AccessOf(local) = %q, want %q", got, AccessImmediate)
	}
}

// TestAccessOf_AnArchiveClassSaysSoBeforeAnybodyRelevantOnIt is FR-34's
// central claim. GLACIER and DEEP_ARCHIVE objects exist and cannot be
// read: the wait is hours, and the product has to say that BEFORE an
// operator plans a restore around it, not at the moment the download
// fails.
func TestAccessOf_AnArchiveClassSaysSoBeforeAnybodyReliesOnIt(t *testing.T) {
	for _, class := range []string{config.StorageClassGlacier, config.StorageClassDeepArchive} {
		t.Run(class, func(t *testing.T) {
			p := state.Placement{Medium: "offsite_cold", Location: "k/a.dump", Status: state.PlacementActive}
			got := AccessOf(p, MediumFacts{Declared: true, StorageClass: class})
			if got != AccessRequiresRestore {
				t.Errorf("AccessOf(%s) = %q, want %q: an object on this class cannot be read on demand", class, got, AccessRequiresRestore)
			}
		})
	}
}

// TestAccessOf_TheOtherClassesServeOnDemand is the other half of that
// pair, and GLACIER_IR is the case that makes it worth writing. Instant
// Retrieval is a PRICE tier, not an access tier: it serves objects on
// demand. Reporting it as requiring a restore would send an operator away
// for hours to wait for a file they could have had at once, which is the
// same kind of lie in the opposite direction.
func TestAccessOf_TheOtherClassesServeOnDemand(t *testing.T) {
	for _, class := range []string{
		config.StorageClassStandard,
		config.StorageClassStandardIA,
		config.StorageClassOneZoneIA,
		config.StorageClassIntelligentTiering,
		config.StorageClassGlacierIR,
	} {
		t.Run(class, func(t *testing.T) {
			p := state.Placement{Medium: "offsite_s3", Location: "k/a.dump", Status: state.PlacementActive}
			got := AccessOf(p, MediumFacts{Declared: true, StorageClass: class})
			if got != AccessImmediate {
				t.Errorf("AccessOf(%s) = %q, want %q", class, got, AccessImmediate)
			}
		})
	}
}

// TestAccessOf_AMediumTheConfigurationNoLongerDeclaresIsUnreachable is the
// distinction this whole file exists for.
//
// An operator removes a storage_mediums block, or restores a config from
// before they added one, while artifacts still live in that bucket. The
// journal still holds the placement, because the copy really was made and
// nothing has said otherwise. But this deployment now has no bucket, no
// endpoint and no credential to reach it with, so it cannot confirm the
// copy and it cannot deny it either.
//
// "We cannot confirm a copy here" and "there is no copy here" are
// different facts with opposite responses, and the second one is not
// available to this function at all: absence of a copy is absence of a
// placement row, which never reaches here.
func TestAccessOf_AMediumTheConfigurationNoLongerDeclaresIsUnreachable(t *testing.T) {
	p := state.Placement{
		Medium:            "offsite_s3",
		Location:          "rclone-manager/db01/set/a.dump",
		Status:            state.PlacementActive,
		VerificationClass: state.VerificationContent,
	}

	got := AccessOf(p, MediumFacts{Declared: false})
	if got != AccessUnreachable {
		t.Fatalf("AccessOf(undeclared medium) = %q, want %q", got, AccessUnreachable)
	}
	// And the fallback must not be the cheerful one. A copy this
	// deployment cannot reach reported as "readable now" is precisely the
	// #361 shape: an outcome nobody verified, rendered as a success.
	if got == AccessImmediate {
		t.Error("an unreachable copy was reported as readable on demand")
	}
}

// TestAccessOf_AnUndeclaredArchiveMediumIsUnreachableRatherThanRestorable
// pins the precedence between the two "not now" answers, because they are
// not interchangeable advice. "Ask for a restore" is an action an operator
// can take; against a medium this deployment cannot reach, it is an action
// that cannot even be submitted, and telling them to take it wastes the
// hours they were told to wait.
func TestAccessOf_AnUndeclaredArchiveMediumIsUnreachableRatherThanRestorable(t *testing.T) {
	p := state.Placement{Medium: "offsite_cold", Location: "k/a.dump", Status: state.PlacementActive}
	got := AccessOf(p, MediumFacts{Declared: false, StorageClass: config.StorageClassDeepArchive})
	if got != AccessUnreachable {
		t.Errorf("AccessOf(undeclared archive medium) = %q, want %q", got, AccessUnreachable)
	}
}

// TestAccesses_IsTheClosedVocabularyAndOneRungHasNoProducerYet is the
// coordination note, held as a test rather than left in a comment.
//
// The vocabulary is closed at four, and the contract serves all four so
// every surface narrows against the final set once. Three of them have a
// producer here. "restoring" does not, because the restore operation is
// #241 (E2.4). Asserting that plainly is what stops a later reader
// concluding that AccessOf forgot a branch, and what makes #241's job
// visible: give this function the fact that a restore is in flight.
func TestAccesses_IsTheClosedVocabularyAndOneRungHasNoProducerYet(t *testing.T) {
	want := []Access{AccessImmediate, AccessRequiresRestore, AccessRestoring, AccessUnreachable}
	if len(Accesses) != len(want) {
		t.Fatalf("Accesses = %v, want %v", Accesses, want)
	}
	for i, a := range want {
		if Accesses[i] != a {
			t.Errorf("Accesses[%d] = %q, want %q", i, Accesses[i], a)
		}
		if !a.Valid() {
			t.Errorf("%q is in the vocabulary but Valid() rejects it", a)
		}
	}
	if Access("stored").Valid() {
		t.Error("Valid() accepted a value that is not in the vocabulary")
	}

	// Nothing this issue ships returns "restoring". Sweeping every input
	// this function accepts is cheap, and it is the honest way to say so.
	for _, declared := range []bool{true, false} {
		for _, class := range append([]string{"", "NOT_A_CLASS"}, config.StorageClasses()...) {
			for _, medium := range []string{state.MediumLocal, "offsite_s3"} {
				p := state.Placement{Medium: medium, Status: state.PlacementActive}
				if got := AccessOf(p, MediumFacts{Declared: declared, StorageClass: class}); got == AccessRestoring {
					t.Fatalf("AccessOf produced %q, which #241 has not landed the producer for yet", got)
				}
			}
		}
	}
}
