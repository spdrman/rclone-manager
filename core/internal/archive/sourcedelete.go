package archive

import (
	"errors"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// ErrNoRetrievableCopy is the refusal that keeps this EPIC's central
// invariant true once an archive class is in play.
//
// FR-30 states the invariant as "every managed-complete artifact has at
// least one ACTIVE placement whose recorded verification class is
// read-back or better, at every instant". Read literally, a placement row
// on DEEP_ARCHIVE with `content` in its verification_class column
// satisfies that sentence, and satisfies it while the bytes it describes
// are hours away from being readable by anybody. That is the gap this
// error exists to close.
var ErrNoRetrievableCopy = errors.New("archive: no other copy of this artifact is both verified and retrievable right now")

// Copy is one durable copy of one artifact, as a decision about deleting
// another copy sees it.
//
// It is a placement row plus the two things the row does not carry: the
// storage class of the medium it lives on, which lives in configuration,
// and what can be done with it right now, which lives at the endpoint.
// Requiring a caller to supply all three is the point. A caller that has
// only the row cannot construct this type, and a caller that cannot
// construct this type cannot ask this package whether it may delete
// something.
type Copy struct {
	// Placement is the journal's row for this copy.
	Placement state.Placement

	// Class is the storage class of the medium this copy is on, empty for
	// the local copy and for a medium that names no class.
	Class string

	// Access is what can be done with this copy right now, from Access.
	Access State
}

// Verified reports whether this copy's RECORDED verification class is
// strong enough to stand in for another copy.
//
// Existence is never enough, and FR-31 says so in those words: an object
// of the right size being present at a key proves nothing whatever about
// its contents, and a source deleted against it is a source deleted
// against a file nobody has ever read.
func (c Copy) Verified() bool {
	class := placement.Class(c.Placement.VerificationClass)
	return class == placement.Content || class == placement.Attested
}

// CanStandIn reports whether this copy is a good enough reason to delete a
// different copy of the same artifact.
//
// Three conditions, and the third is the one this package exists for.
//
// The copy has to be one the journal still believes in (ACTIVE, not
// DELETE_PENDING and not GONE). It has to have been verified at read-back
// strength or better at some point, which is Verified above. And it has to
// be retrievable NOW.
//
// The first two are facts about the past and the third is a fact about the
// present, which is exactly why the third cannot be inferred from the
// other two. A copy is content-verified at the moment it is uploaded, and
// then a bucket lifecycle rule transitions it to DEEP_ARCHIVE a week
// later, or a restore that made it readable expires. Nothing rewrites the
// verification_class column when either of those happens, and nothing
// should: the verification really did happen, and the record of it is
// true. What changed is whether anybody can act on it.
func (c Copy) CanStandIn() bool {
	return c.Placement.Status == state.PlacementActive && c.Verified() && c.Access.Retrievable()
}

// CheckSourceDelete refuses to delete src unless some OTHER copy of the
// same artifact can stand in for it.
//
// This is the function #238's move engine calls before it issues a source
// delete, and the function FR-20's prune calls before it removes a copy
// that retention no longer selects. It is a pure decision over facts the
// caller has already gathered, so it can be tested for the case that
// actually loses data without staging a real move against a real bucket.
//
// # How it composes with the move engine's own guard, and why only one way
//
// #238 has a source-delete guard of its own, and the two are not rivals to
// be reconciled: this one is a precondition of that one, and the direction
// is forced rather than chosen.
//
// That guard asks the journal-shaped question, which is whether a
// destination copy exists and was verified. This one asks whether any
// SURVIVING copy can actually be read right now. The second cannot be
// inferred from the first, because nothing rewrites verification_class
// when a bucket lifecycle rule transitions an object or a restore window
// expires: the recorded verification really did happen, and the record of
// it stays true while the bytes go out of reach. And the composition
// cannot run the other way round, because this package holds no journal
// read, so it has nothing to call. The move engine has already loaded the
// copies; passing them here costs it nothing.
//
// The guard that makes a mover which forgets fail the build rather than
// pass review is TestNothingDeletesACopyWithoutAskingWhetherAnotherOneIsReadable
// (composition_test.go). It lives on this side because the caller does not
// exist in this tree yet, and a rule that only lives in a merged pull
// request description is a rule the next lane never reads.
//
// # The data-loss path it closes
//
// A move to an archive class ends with a destination placement row that
// says ACTIVE, and possibly says content, because the upload really was
// read back and re-hashed at the moment it landed. The source is the last
// copy anybody can actually read. If the delete decision reads only the
// row, it deletes that source and the artifact becomes something that
// exists, is durable, is provably intact, and cannot be got hold of for
// hours by anyone at any price. Restoring it would work, eventually, so
// this is not always unrecoverable; it is unrecoverable exactly when the
// operator needed the backup in the next few hours, which is the only time
// anybody ever needs a backup.
//
// # Why it does not care whether src itself is retrievable
//
// It deliberately says nothing about src's own access state. Deleting a
// copy nobody can read is fine when another copy can be read, and
// refusing it would strand exactly the artifacts an operator is trying to
// get off an archive tier. The question is only ever about what SURVIVES.
func CheckSourceDelete(src Copy, all []Copy) error {
	for _, c := range all {
		if c.Placement.Medium == src.Placement.Medium {
			continue
		}
		if c.CanStandIn() {
			return nil
		}
	}
	return fmt.Errorf("%w: deleting the copy on %q would leave %s",
		ErrNoRetrievableCopy, src.Placement.Medium, describeSurvivors(src, all))
}

// describeSurvivors says what would actually be left, because "refused" on
// its own sends an operator to read code.
func describeSurvivors(src Copy, all []Copy) string {
	var others []string
	for _, c := range all {
		if c.Placement.Medium == src.Placement.Medium {
			continue
		}
		reason := "retrievable and verified"
		switch {
		case c.Placement.Status != state.PlacementActive:
			reason = fmt.Sprintf("recorded as %s", c.Placement.Status)
		case !c.Verified():
			if c.Placement.VerificationClass == "" {
				reason = "never verified"
			} else {
				reason = fmt.Sprintf("verified only at %s, which proves nothing about its bytes", c.Placement.VerificationClass)
			}
		case !c.Access.Retrievable():
			reason = fmt.Sprintf("on %s and %s, so nothing can read it right now", c.Class, c.Access)
		}
		others = append(others, fmt.Sprintf("%s (%s)", c.Placement.Medium, reason))
	}
	if len(others) == 0 {
		return "no other copy at all"
	}
	return fmt.Sprintf("only: %v", others)
}
