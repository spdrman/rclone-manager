package placement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is FR-20's prune, once the copy being pruned is an object
// rather than a file (EPIC E FR-30, issue #239). It is the second
// irreversible act in this package, and it is written the same way as the
// first: every fact re-derived from the artifact's own journal record and
// from the medium itself, at the moment of the delete, never trusted from
// whatever the caller already decided.
//
// # Why it is not sourcedelete.go
//
// The two look alike and answer different questions. guardSourceDelete
// asks "may this move remove its source, given that a verified
// destination copy exists"; the whole delete is safe BECAUSE another copy
// was just proved. This one asks "may retention remove this artifact's
// only copy, because no tier wants it any more". There is no other copy
// afterwards, and there is not meant to be. So the proof it needs is not
// "somewhere else has these bytes" but "the object at this key is still
// the one this journal is expiring", which is FR-16 exactly:
//
//	Immediately before deletion, compare the current remote object against
//	the stored identity using the strongest practical available
//	attributes. If identity cannot be established with sufficient
//	confidence: preserve the remote object.
//
// # What "the strongest practical available attributes" comes to here
//
// The ladder in ladder.go already answers this, so this file asks it
// rather than writing a third size comparison and a second hash
// comparison of its own. Existence compares the recorded size; Attested
// compares the endpoint's own full-object SHA-256 against the hash
// recorded at ingestion, and returns ErrClassUnavailable where the
// endpoint cannot produce one, which against rclone v1.75.0's s3 backend
// is always. So "where available" is a real branch with a real answer on
// both sides, and both sides are tested.
//
// Content is deliberately NOT asked for. It would download the whole
// object to re-hash it, immediately before deleting it, which is a
// surprise egress bill incurred to check something that is about to stop
// existing. Automatic revalidation already refuses egress for the same
// reason (Class.CostsEgress's own doc), and this is the same trade.
//
// # Nothing here is convergence
//
// sourcedelete.go has errSourceAlreadyGone: a source that is already gone
// mid-move means the delete landed and the journal write did not, and the
// caller's intent is satisfied. This file has no such case, and the
// difference is what that one has: a durably recorded, verified
// destination copy proving the artifact still exists somewhere. Here an
// object that is not at its key means the journal and the medium disagree
// about a backup, and the answer is a refusal an operator can reconcile,
// which is the same answer internal/retention's local path already gives
// for a file that has vanished.

// Reclaimer deletes an artifact's copy from a storage medium, having first
// re-proved the object's identity against the placement record.
//
// It is internal/retention.MediumPruner's implementation. That interface
// is declared over there and satisfied here because internal/retention may
// not read a placement row or a transport.ObjectInfo at all (FR-32, held
// structurally by TestRetentionReadsNoMediumSuppliedValue): the decision
// to delete is retention's, and the evidence for it is this package's.
type Reclaimer struct {
	// Store is the medium boundary. It is the same narrow MediumStore the
	// move engine takes, which is wider than this file needs on its own,
	// and that is the point: a reclaimer built from a different, delete-
	// capable object would be a second answer to "what can destroy an
	// object on a medium".
	Store MediumStore

	// Mediums resolves a medium id into somewhere reachable.
	Mediums MediumResolver

	// Now is the clock the verification results are stamped with. Nil
	// means time.Now.
	Now func() time.Time
}

func (r *Reclaimer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// DeleteFromMedium removes rec's copy on medium, or refuses and removes
// nothing.
//
// Every path out of it that is not the last line preserves the object.
func (r *Reclaimer) DeleteFromMedium(ctx context.Context, rec state.Record, medium string) error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("placement: refusing to delete %s's copy on %q: "+format,
			append([]any{rec.Artifact, medium}, args...)...)
	}

	// internal/retention turns a nil MediumPruner into a REFUSE, and that
	// check is on the INTERFACE: a caller handing over a (*Reclaimer)(nil)
	// satisfies it, the refusal never fires, and the first field access
	// below panics in the middle of a retention apply. Refusing here costs
	// one comparison and makes the fail-safe hold for the value as well as
	// for the interface. `refuse` reads nothing off r, so it is safe on a
	// nil receiver.
	if r == nil {
		return refuse("there is no reclaimer to ask; something handed this delete path a nil value that still satisfied its interface, and a delete decided by nothing at all is not a delete")
	}
	if medium == config.MediumLocal {
		// A local copy is FR-20's, and FR-20's proof is a canonicalized
		// path proven beneath the backup set's configured root. Nothing
		// here has a root, a path or a symlink check, so answering about
		// one would be answering with no proof at all.
		return refuse("that is the implicit local medium, whose deletions go through FR-20's path discipline in internal/retention and never through here")
	}
	if r.Store == nil {
		return refuse("no medium store is configured")
	}
	if r.Mediums == nil {
		return refuse("no medium resolver is configured, so there is nowhere to reach")
	}

	// The placement, re-derived from the record rather than taken from
	// whatever decided this delete. Exactly one ACTIVE placement is a
	// location: none is "I cannot confirm where this is", and more than
	// one is a move in flight, whose source or destination this must not
	// remove from under it. internal/app.ActiveMediumFromRecords reads it
	// the same way one layer up, and it is re-read here for the reason
	// every other check in this package is re-run at the point of the
	// dangerous action.
	active := activePlacements(rec)
	switch {
	case len(active) == 0:
		return refuse("the journal records no ACTIVE placement at all, so nothing confirms this artifact has a durable copy anywhere")
	case len(active) > 1:
		return refuse("the journal records %d ACTIVE placements (%s), which is a move in flight; deleting one of them is the race FR-30's journal exists to make unrepresentable",
			len(active), placementMediumList(active))
	}
	p := active[0]
	switch {
	case p.Medium != medium:
		return refuse("the artifact's one ACTIVE placement is on %q, not here", p.Medium)
	case p.Location == "":
		return refuse("the placement records no location, so there is no key to address")
	}

	target, _, err := r.Mediums.Resolve(medium)
	if err != nil {
		return refuse("%v", err)
	}

	now := r.now()

	// FR-16, first attribute: the object is there, at the size recorded
	// for this placement. A medium that could not be asked is an error
	// rather than a failed check, and it refuses either way.
	existence, err := Verify(ctx, r.Store, target, p, Existence, now)
	if err != nil {
		return refuse("the medium could not be asked about %q: %v", p.Location, err)
	}
	if !existence.Passed {
		return refuse("%s", existence.Detail)
	}
	proved := []string{}
	if p.Size != nil {
		proved = append(proved, "the recorded size")
	}

	// FR-16, second attribute, "where available": the endpoint's own
	// full-object checksum against the hash recorded at ingestion. An
	// endpoint that cannot produce one leaves the size as the whole proof,
	// and says so; a mismatch refuses.
	attested, err := Verify(ctx, r.Store, target, p, Attested, now)
	switch {
	case errors.Is(err, ErrClassUnavailable):
		// No checksum is obtainable for this placement, which is every s3
		// medium reachable through this build. Not a refusal on its own:
		// see the file comment.
	case err != nil:
		return refuse("asking %q for its own checksum of %q failed: %v", medium, p.Location, err)
	case !attested.Passed:
		return refuse("%s", attested.Detail)
	default:
		proved = append(proved, "the endpoint's own checksum")
	}

	// FR-16's closing line. An object that exists at a key, with nothing
	// recorded to compare it against, has had its identity established to
	// exactly no confidence, and the instruction for that is to preserve
	// it.
	if len(proved) == 0 {
		return refuse("the placement records neither a size nor a checksum this endpoint can attest, so the only thing proved about %q is that an object is there; that is not identity, and FR-16 says to preserve rather than guess",
			p.Location)
	}

	if err := r.Store.DeleteObject(ctx, target, p.Location); err != nil {
		return refuse("the delete itself failed: %v", err)
	}
	return nil
}

// activePlacements is the ACTIVE subset of a record's placements, which is
// the only status that means "a durable copy is here".
func activePlacements(rec state.Record) []state.Placement {
	var out []state.Placement
	for _, p := range rec.Placements {
		if p.Status == state.PlacementActive {
			out = append(out, p)
		}
	}
	return out
}

// placementMediumList renders the mediums of a set of placements for the
// refusal above, so the message says what the ambiguity actually was.
func placementMediumList(ps []state.Placement) string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Medium)
	}
	return strings.Join(names, ", ")
}
