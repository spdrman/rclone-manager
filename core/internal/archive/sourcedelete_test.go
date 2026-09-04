package archive

import (
	"errors"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// theLocalCopy is the last copy anybody can actually read: an ordinary
// local file, ACTIVE, content-verified. It is what a move to an archive
// class is about to delete.
func theLocalCopy() Copy {
	body := []byte("the only copy of this backup anybody can read today")
	return Copy{
		Placement: localPlacement("/srv/backups/production/postgres/dump.zst", int64(len(body)), hashOf(body)),
		Class:     "",
		Access:    Immediate,
	}
}

// theArchivedDestination is the copy a move to DEEP_ARCHIVE just made.
//
// Read its fields carefully, because they are the whole point. The row is
// ACTIVE. Its recorded verification class is `content`, and that record is
// TRUE: the upload really was read back and re-hashed at the moment it
// landed, before the bucket's lifecycle rule transitioned it. Nothing
// about the journal is wrong or stale or half-written.
//
// It is, nonetheless, hours away from being readable by anybody.
func theArchivedDestination() Copy {
	body := []byte("the only copy of this backup anybody can read today")
	return Copy{
		Placement: mediumPlacement("cold-store", "prefix/production/postgres/dump.zst",
			int64(len(body)), hashOf(body), state.VerificationContent),
		Class:  config.StorageClassDeepArchive,
		Access: RequiresRestore,
	}
}

// TestAnUnretrievableArchiveCopyCannotJustifyDeletingTheLastReadableOne is
// the data-loss path of this lane, written as the attempt to cause it.
//
// A move engine reading only the journal sees a destination that is ACTIVE
// and content-verified, concludes the invariant is satisfied, and deletes
// the source. Every one of those steps is individually defensible and the
// result is an artifact that exists, is durable, is provably intact, and
// cannot be got hold of for hours by anyone at any price.
//
// # Why this is not a vacuous refusal
//
// Every OTHER reason to refuse has been removed from the destination on
// purpose. It is not DELETE_PENDING. It is not GONE. It is not
// existence-verified. It is not unverified. It is not on an unknown
// medium. The one and only thing wrong with it is that its bytes are not
// readable right now, and TestARestoredArchiveCopyCanJustifyIt below
// flips exactly that one field and gets a different answer, which is what
// shows the refusal is about the thing it claims to be about.
func TestAnUnretrievableArchiveCopyCannotJustifyDeletingTheLastReadableOne(t *testing.T) {
	local := theLocalCopy()
	archived := theArchivedDestination()

	if !archived.Verified() {
		t.Fatal("the destination in this test is supposed to be content-verified; if it is not, the refusal below proves nothing")
	}
	if archived.Placement.Status != state.PlacementActive {
		t.Fatal("the destination in this test is supposed to be ACTIVE; if it is not, the refusal below proves nothing")
	}

	err := CheckSourceDelete(local, []Copy{local, archived})
	if !errors.Is(err, ErrNoRetrievableCopy) {
		t.Fatalf("CheckSourceDelete allowed the last readable copy to be deleted (err = %v); the artifact would still exist and nobody could read it", err)
	}
	if !strings.Contains(err.Error(), config.StorageClassDeepArchive) {
		t.Errorf("the refusal does not say what is actually wrong: %v", err)
	}
}

// TestARestoredArchiveCopyCanJustifyIt is the positive control for the
// test above, and it changes exactly one field.
//
// Same artifact, same journal row, same recorded content verification, and
// the only difference is that a restore has finished, so the bytes are
// readable. This has to be allowed, or the guard is not a guard, it is
// just a refusal to ever move anything to an archive class.
func TestARestoredArchiveCopyCanJustifyIt(t *testing.T) {
	local := theLocalCopy()
	restored := theArchivedDestination()
	restored.Access = Immediate

	if err := CheckSourceDelete(local, []Copy{local, restored}); err != nil {
		t.Fatalf("CheckSourceDelete refused a source delete against a restored, content-verified copy: %v", err)
	}
}

// TestWhatCannotStandInForACopy covers the rest of the reasons a copy is
// not a good enough excuse to delete another one, so that the archive
// reason above is a rule in a set rather than a special case.
func TestWhatCannotStandInForACopy(t *testing.T) {
	base := theArchivedDestination()

	tests := []struct {
		name  string
		mutot func(Copy) Copy
	}{
		{"the journal has already decided to delete it", func(c Copy) Copy {
			c.Access = Immediate
			c.Placement.Status = state.PlacementDeletePending
			return c
		}},
		{"the journal knows it is gone", func(c Copy) Copy {
			c.Access = Immediate
			c.Placement.Status = state.PlacementGone
			return c
		}},
		{"nothing has ever verified it", func(c Copy) Copy {
			c.Access = Immediate
			c.Placement.VerificationClass = ""
			return c
		}},
		{"only its existence was ever checked", func(c Copy) Copy {
			c.Access = Immediate
			c.Placement.VerificationClass = state.VerificationExistence
			return c
		}},
		{"the medium holding it did not answer", func(c Copy) Copy {
			c.Access = Unreachable
			return c
		}},
		{"a restore of it is still running", func(c Copy) Copy {
			c.Access = Restoring
			return c
		}},
	}

	local := theLocalCopy()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			other := tc.mutot(base)
			if err := CheckSourceDelete(local, []Copy{local, other}); !errors.Is(err, ErrNoRetrievableCopy) {
				t.Fatalf("CheckSourceDelete allowed the delete (err = %v)", err)
			}
		})
	}
}

// TestAnAttestedCopyCanStandIn, because FR-31 makes attested an opt-in
// that IS sufficient to delete a source, and existence explicitly is not.
// Pinning both directions is what stops somebody "simplifying" the rule to
// "anything that was verified".
func TestAnAttestedCopyCanStandIn(t *testing.T) {
	local := theLocalCopy()
	attested := theArchivedDestination()
	attested.Access = Immediate
	attested.Placement.VerificationClass = state.VerificationAttested

	if err := CheckSourceDelete(local, []Copy{local, attested}); err != nil {
		t.Fatalf("CheckSourceDelete refused against an attested, retrievable copy: %v", err)
	}
}

// TestTheOnlyCopyIsNeverDeletable is the degenerate case, and it is worth
// its own test because the loop that looks for another copy is the loop
// that returns "found nothing" when there is nothing to look at.
func TestTheOnlyCopyIsNeverDeletable(t *testing.T) {
	local := theLocalCopy()
	err := CheckSourceDelete(local, []Copy{local})
	if !errors.Is(err, ErrNoRetrievableCopy) {
		t.Fatalf("CheckSourceDelete allowed the deletion of an artifact's only copy: %v", err)
	}
	if !strings.Contains(err.Error(), "no other copy at all") {
		t.Errorf("the refusal should say there is nothing else: %v", err)
	}
}

// TestDeletingAnArchivedCopyIsFineWhenSomethingReadableSurvives is the
// other direction, and it matters: an operator moving artifacts back OFF
// an archive tier must not be stranded by a guard that refuses to delete
// anything archived. The question this guard asks is only ever about what
// survives.
func TestDeletingAnArchivedCopyIsFineWhenSomethingReadableSurvives(t *testing.T) {
	local := theLocalCopy()
	archived := theArchivedDestination()

	if err := CheckSourceDelete(archived, []Copy{local, archived}); err != nil {
		t.Fatalf("CheckSourceDelete refused to delete an unreadable copy while a readable one survives: %v", err)
	}
}
