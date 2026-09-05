package app

import (
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// FR-34's answer to "where is my backup and can I have it", computed over no
// network at all.
//
// Every field on ArtifactCopy comes from a journal row and a configuration
// block. Nothing here asks a medium anything, which is not a limitation
// being worked around: FR-34's rule is that a read never initiates a restore
// as a side effect, and a status page that quietly probed a provider on every
// render is exactly how that rule gets broken by accident. So an archived
// copy reads as requires_restore, because being on an archive class IS the
// artifact's state until somebody does something about it, and the state that
// would need a round trip to establish is the temporary one.
//
// The two things this file decides are what counts as a copy and what may be
// said about one. A GONE placement is not a copy and is dropped, because the
// struct's shape is a copy's shape and a renderer would faithfully print five
// true-looking fields about a file that is not there. And no price, no
// percentage, no estimate: this product has no price list and S3 reports a
// restore as running or finished, so each of those three would be a number
// invented at the last layer before an operator reads it.

// ArtifactCopy is one durable copy of one artifact, as an operator asking
// "where is my backup and can I have it" reads it (EPIC E, FR-34).
//
// It is a placement row plus the two things the row cannot know on its
// own: what storage class the medium holding it writes with, which lives
// in configuration, and what that means for getting the bytes back, which
// lives in internal/archive.
//
// # What is not here
//
// No price, no percentage, no estimate of when anything will finish.
// FR-34 is direct about why: this product has no price list and S3 reports
// a restore as running or finished and nothing else, so any of those three
// would be invented. RetrievalBilled says a bill exists, which is a fact
// this product does hold, and Detail says in words what the operator is
// looking at.
type ArtifactCopy struct {
	// Medium is state.MediumLocal or a configured medium id.
	Medium string

	// Location is an absolute path for the local copy and an object key
	// for a copy on a medium.
	Location string

	// Status is "ACTIVE" or "DELETE_PENDING". A placement the journal
	// knows is GONE never becomes one of these at all; see
	// artifactCopies.
	//
	// DELETE_PENDING is here and GONE is not, and the line between them
	// is the whole rule: a DELETE_PENDING copy is one this manager has
	// written down that it intends to delete and has not deleted yet, so
	// the bytes are there and every other field on this struct is true
	// about them. A GONE copy is one that is not there.
	Status string

	// VerificationClass is the strongest class of verification this copy
	// has ACHIEVED, empty when nothing has verified it, and never the
	// strongest class configured (FR-31).
	VerificationClass string

	// VerifiedAt is when that class was last achieved, or nil.
	VerifiedAt *time.Time

	// SizeBytes is what this copy measures, or nil when nobody recorded
	// it.
	SizeBytes *int64

	// StorageClass is the class the medium writes with, empty for the
	// local copy.
	StorageClass string

	// Access is what can be done with this copy right now, from
	// archive.Access.
	Access archive.State

	// RetrievalBilled is whether the provider charges to read this copy
	// back. No amount, ever; see this struct's own doc.
	RetrievalBilled bool

	// Detail is the plain-words sentence explaining Access, empty for a
	// copy that needs no explaining.
	Detail string

	// CheckableAs is the strongest verification class that can be
	// attempted against this copy right now, and empty when none can.
	//
	// FR-31 ends its archive rule with "the status surfaces say exactly
	// that", and this is the field that does it. An archived copy is
	// existence-checkable and nothing more until a restore, and an
	// operator reading a status page is exactly the person who needs to
	// know that the strongest thing anybody could do to reassure
	// themselves about this backup today is confirm an object of the
	// right size is at that key.
	//
	// It is what could be attempted, never what has been achieved.
	// VerificationClass above is the achieved one, and the two being
	// different fields is the whole point: they disagree all the time,
	// and each of them is true.
	CheckableAs string
}

// Retrievable reports whether this copy's bytes can be read right now.
func (c ArtifactCopy) Retrievable() bool { return c.Access.Retrievable() }

// artifactCopies turns a record's placements into the operator-facing view
// above.
//
// # It asks no medium anything, and that is the point
//
// FR-34's rule is that a read never initiates a restore as a side effect,
// and this is a read: `backup-manager artifacts <id>` prints what the
// journal and the configuration say, over no network at all. So every
// copy is derived with archive.Observation's zero value, which means "I
// have not looked", and an archived copy therefore reads as
// requires_restore rather than as anything more encouraging.
//
// That is not a limitation being papered over, it is the honest answer.
// Being on an archive class IS the artifact's state until somebody does
// something about it, and the state that needs a network round trip to
// establish is the temporary one, a restore having been asked for. The
// restore operation is what establishes that, and it says so from the
// provider's own answer rather than from here.
// # A GONE placement is not a copy, and is dropped here
//
// state.PlacementGone means "the copy is no longer there and the journal
// knows it", which is what a completed move leaves behind on the source
// and what a prune leaves behind on the medium. The row is kept in the
// journal forever, deliberately: a deleted copy is recorded, never
// removed, so reconciliation and the recovery manifest can still account
// for it. None of that makes it a copy.
//
// Serving one here would put it in front of an operator wearing this
// struct's shape, and this struct's shape is a copy: a location, an
// access state, what verified it, what could still be checked against it,
// whether reading it is billed. Every one of those is derived from the
// row's own recorded hash and class, which a GONE row still carries, so
// they all compute cleanly and all describe a file that is not there.
// `backup-manager artifacts` prints them one under the other, so the
// output says GONE once and contradicts itself five times.
//
// core/service drops GONE rows at the API boundary for exactly this
// reason and states it in one line: a row for it would read as a copy in
// every layout anyone would write for one. FR-34 requires the CLI and the
// UI to read the same truth about the same artifact, so the drop belongs
// to the view rather than to either renderer, and a second renderer of
// ArtifactCopy inherits it instead of rediscovering it.
//
// What is left is absence of a copy as absence of a row, which is the same
// thing an artifact that never had one reports, and it is what makes an
// empty list mean exactly one thing.
func (s *Service) artifactCopies(rec state.Record, now time.Time) []ArtifactCopy {
	if len(rec.Placements) == 0 {
		return nil
	}
	out := make([]ArtifactCopy, 0, len(rec.Placements))
	for _, p := range rec.Placements {
		if p.Status == state.PlacementGone {
			continue
		}
		out = append(out, s.artifactCopy(p, now))
	}
	return out
}

func (s *Service) artifactCopy(p state.Placement, now time.Time) ArtifactCopy {
	c := ArtifactCopy{
		Medium:            p.Medium,
		Location:          p.Location,
		Status:            p.Status,
		VerificationClass: p.VerificationClass,
		VerifiedAt:        p.VerifiedAt,
		SizeBytes:         p.Size,
	}

	class, known := s.storageClassOf(p.Medium)
	c.StorageClass = class

	if !known {
		// The journal names a medium this configuration no longer
		// declares. Nothing here can say what class it wrote with, and
		// nothing here can reach it either, which is exactly what
		// unreachable means: a fact about the endpoint, deliberately not
		// a claim that the copy is gone.
		c.Access = archive.Unreachable
		c.Detail = "this copy is on a storage medium this configuration no longer declares, so nothing here can reach it or say what class it is on; the copy itself may well still be there"
		return c
	}

	access, err := archive.Access(p.Medium, class, archive.Observation{}, now)
	if err != nil {
		c.Access = archive.Unreachable
		c.Detail = "this copy is on a storage class this build does not recognise, so nothing here will claim its bytes can be read"
		return c
	}
	c.Access = access
	c.Detail = archive.Describe(access, class, nil)
	c.CheckableAs = string(placement.Ceiling(access))
	if b, err := archive.Of(class); err == nil {
		c.RetrievalBilled = b.RetrievalBilled
	}
	return c
}

// storageClassOf resolves a placement's medium id to the storage class
// that medium writes with, and reports whether the configuration declares
// that medium at all.
//
// The local medium is declared by every deployment implicitly and has no
// storage class, so it answers ("", true): known, and classless.
func (s *Service) storageClassOf(medium string) (string, bool) {
	if medium == state.MediumLocal {
		return "", true
	}
	if s.Config == nil {
		return "", false
	}
	for _, m := range s.Config.StorageMediums {
		if m.ID == medium {
			return m.EffectiveStorageClass(), true
		}
	}
	return "", false
}
