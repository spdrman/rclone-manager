package placement

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is the archive gate in front of the ladder: what may be
// ATTEMPTED against a copy, given what can be done with it right now.
//
// # Why it lives here and not in internal/archive
//
// It was written in internal/archive first, beside the access vocabulary
// it reads, and that put the package edge the wrong way round. Every
// function here consumes an access state, which is archive's fact, and
// produces or gates a verification Class, which is this package's
// vocabulary. So it acts in this package's domain and merely asks archive
// a question first. Keeping it in archive made archive import placement,
// and that made it impossible for the move engine, which lives here and is
// the code in this project that deletes a copy, to ask archive anything
// before it did. The engine has to ask (archive.CheckSourceDelete, from
// guardSourceDelete), so the edge runs placement -> archive, and the gate
// moved to the side of the edge where its answers are used.
//
// archive keeps the facts: the class table, the access derivation, the
// restore operation, and the one refusal that is stated purely in its own
// vocabulary (a copy nobody can read is not a reason to delete one that
// somebody can). This file holds the other refusal those facts imply,
// which is stated in a verification class: a copy nobody can read cannot
// earn a class that needs reading it.

// Ceiling is the strongest verification class that can honestly be
// attempted against a copy in access state s.
//
// The empty class means nothing can be attempted at all, which is the
// answer for a medium that did not answer: a check that could not run is
// not a weak pass, it is not a check.
//
// # Why an archived copy stops at existence
//
// Both classes above existence need something an archived object will not
// give. Content needs the bytes, and a GET of an archived object fails.
// Attested needs the provider's own full-object digest, and the archive
// tiers are exactly where S3 is least willing to compute one; more to the
// point, this build cannot reach an attestation on ANY class, because
// rclone v1.75.0's s3 backend reports hash.MD5 and nothing else from
// Fs.Hashes(), so a full-object SHA-256 attestation is unavailable before
// the storage class is even considered. FR-31's own words for that
// situation are an explicit capability result rather than a silent
// weakening, and that is what CheckClass returns.
//
// So an archived copy tops out at "an object of the right size is at that
// key", and every surface has to say so in those words. This is the
// function that makes it impossible to say anything else, because there
// is no path through VerifyWithAccess that runs a class this refuses.
func Ceiling(s archive.State) Class {
	switch s {
	case archive.Immediate:
		return Content
	case archive.RequiresRestore, archive.Restoring:
		return Existence
	default:
		// Unreachable, and anything that is not a valid state at all.
		return ""
	}
}

// CheckClass reports whether want can be attempted against a copy in
// access state s, and refuses with a wrapped ErrClassUnavailable when it
// cannot.
//
// The error type is deliberate and load-bearing. This package draws one
// distinction above all others: "I checked and it is wrong" is a fact
// about the artifact and quarantines it, while "I could not check" is a
// fact about the endpoint and must not. An archived object is the second
// one. A refusal that arrived as a failed Result instead would eventually
// quarantine a perfectly good backup for the crime of being cheap to
// store.
func CheckClass(s archive.State, want Class) error {
	if !want.Valid() {
		return fmt.Errorf("%w: %q is not a verification class", ErrClassRefused, want)
	}
	ceiling := Ceiling(s)
	if ceiling == "" {
		return fmt.Errorf("%w: this copy's access state is %q, so no class can be attempted against it at all",
			ErrClassRefused, s)
	}
	if want.Stronger(ceiling) {
		return fmt.Errorf(
			"%w: this copy's access state is %q, which supports %s at best (%s), and %s needs %s",
			ErrClassRefused, s, ceiling, ceiling.Proves(), want, want.Cost())
	}
	return nil
}

// ErrClassRefused is the half of ErrClassUnavailable that a retry cannot
// change, and it exists because the move engine has to tell the two apart
// before it decides what to do next.
//
// Both halves mean "I could not check". A read that timed out is the first
// half: the endpoint might answer next time, so throwing the destination
// away and copying it again is a reasonable thing to do. A copy on
// DEEP_ARCHIVE that nobody has asked to restore is the second half: the
// next attempt is identical to this one, and so is the thousandth, and the
// engine's copy-verify-recopy loop turns that into an upload and a delete
// per cycle, for ever. On a class with a minimum billable duration each of
// those uploads is charged for months after it has been deleted, so the
// difference between the two halves is a bill rather than a nicety.
//
// It wraps ErrClassUnavailable, so every existing caller that asks
// errors.Is(err, ErrClassUnavailable) keeps the answer it had.
var ErrClassRefused = fmt.Errorf("%w, and no retry can change that", ErrClassUnavailable)

// CheckDestinationClass reports whether a copy this product is about to
// CREATE on a medium writing with storage class class could ever be
// verified at want, and refuses when it could not.
//
// It is CheckClass asked one step earlier, about an object that does not
// exist yet, and that is why it takes a storage class rather than an
// access state: there is exactly one access state a freshly written object
// on an archive class can be in. Nobody has asked to restore an object
// that was not there a second ago, so it is RequiresRestore, by
// definition, and no request has to be spent to find that out.
//
// # Why this is worth having as well as the gate at verification time
//
// The gate in front of Verify saves the request. This saves the upload,
// which is the part that costs real money: AWS bills DEEP_ARCHIVE for a
// 180-day minimum whether or not the object survives the afternoon. A move
// that uploads, discovers it cannot verify, deletes and gives up has still
// bought six months of storage, and the cycle after it buys six more.
//
// The check is answerable entirely from configuration, so an operator who
// writes an incompatible pair gets told about it in the first cycle rather
// than in the first invoice.
func CheckDestinationClass(class string, want Class) error {
	b, err := archive.Of(class)
	if err != nil {
		// A class the table does not recognise. config.Validate refuses
		// one at load, so this is drift between two lists rather than
		// something an operator can write, and the safe direction for
		// "nothing here knows what this class does to readability" is to
		// refuse rather than to upload into it.
		return fmt.Errorf("%w: %w", ErrClassRefused, err)
	}
	if !b.Archive {
		return nil
	}
	if err := CheckClass(archive.RequiresRestore, want); err != nil {
		return fmt.Errorf("%w; a copy written to %s is archived the instant it lands, and nothing has asked to restore an object that did not exist a second ago", err, class)
	}
	return nil
}

// VerifyWithAccess is Verify with the archive gate in front of it, and it
// is the entry point every caller that might be looking at a medium copy
// should use.
//
// It adds exactly one thing to Verify: it decides, from facts it already
// holds, whether the class being asked for could possibly be achieved, and
// refuses BEFORE spending a request on finding out. That ordering is the
// point rather than an optimisation. Asking S3 to GET an archived object
// costs a request and returns InvalidObjectState, and a caller that reads
// that as "the verification failed" has just decided a good backup is bad.
// Refusing first means the caller is told what is actually true, which is
// that nobody has read those bytes and nobody can until a restore happens.
//
// It never falls back to a weaker class. That is Verify's rule and this
// function does not get to soften it: a caller that wants the existence
// check against an archived copy asks for existence, in its own code,
// where somebody reviewing it can see the decision.
func VerifyWithAccess(
	ctx context.Context,
	store Store,
	medium transport.Medium,
	p state.Placement,
	want Class,
	obs archive.Observation,
	now time.Time,
) (Result, error) {
	access, err := archive.Access(p.Medium, medium.StorageClass, obs, now)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrClassUnavailable, err)
	}
	if err := CheckClass(access, want); err != nil {
		return Result{}, err
	}
	return Verify(ctx, store, medium, p, want, now)
}

// AutomaticClass is the strongest class a scheduled, unattended pass may
// run against a copy in access state s, and it is capped below Ceiling for
// one reason: money.
//
// FR-31 makes anything that costs egress operator-initiated, and
// Class.CostsEgress is the mechanism rather than the promise. This
// function reads it rather than hard-coding "existence", so raising the
// automatic ceiling means changing the class whose CostsEgress is
// consulted, not editing a constant and finding out from a bill.
//
// # It lands two rungs down, not one, and that is not this rule's doing
//
// Ceiling(Immediate) is Content, which costs egress, so this answers
// Existence and steps straight past Attested, which costs none. The cap
// written here is not what skips that rung: nothing on the ladder sits
// between "costs egress" and "does not", so a rule stated in CostsEgress
// alone lands on the weakest free class rather than the strongest. FR-31
// makes attested operator-initiated too, so the answer happens to be the
// one FR-31 wants, and it is worth saying that it is a coincidence of
// where the rungs fall rather than something this function decided.
//
// The operator-initiated door does make the distinction, in its own code
// where the choice is visible: internal/app's checkMediumCopies (issue
// #435) attempts Attested first and steps down to Existence explicitly,
// naming the step-down, because measured against rclone v1.75.0 no s3
// medium can attest at all.
//
// An archive class makes that worse and not better. A restore is billed
// on top of the egress, and it is billed for a window measured in days, so
// an automatic pass that could trigger one would keep triggering it. There
// is no path here that can: Ceiling already refuses content for an
// archived copy, and this refuses it again for every copy, and neither
// refusal is reachable by configuration.
func AutomaticClass(s archive.State) Class {
	c := Ceiling(s)
	if c == "" {
		return ""
	}
	if c.CostsEgress() {
		return Existence
	}
	return c
}
