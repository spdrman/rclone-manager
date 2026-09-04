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
		return fmt.Errorf("%w: %q is not a verification class", ErrClassUnavailable, want)
	}
	ceiling := Ceiling(s)
	if ceiling == "" {
		return fmt.Errorf("%w: this copy's access state is %q, so no class can be attempted against it at all",
			ErrClassUnavailable, s)
	}
	if want.Stronger(ceiling) {
		return fmt.Errorf(
			"%w: this copy's access state is %q, which supports %s at best (%s), and %s needs %s",
			ErrClassUnavailable, s, ceiling, ceiling.Proves(), want, want.Cost())
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
// run against a copy in access state s, and it is capped a rung below
// Ceiling for one reason: money.
//
// FR-31 makes anything that costs egress operator-initiated, and
// Class.CostsEgress is the mechanism rather than the promise. This
// function reads it rather than hard-coding "existence", so raising the
// automatic ceiling means changing the class whose CostsEgress is
// consulted, not editing a constant and finding out from a bill.
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
