package archive

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

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
// is no path through Verify that runs a class this refuses.
func Ceiling(s State) placement.Class {
	switch s {
	case Immediate:
		return placement.Content
	case RequiresRestore, Restoring:
		return placement.Existence
	default:
		// Unreachable, and anything that is not a valid state at all.
		return ""
	}
}

// CheckClass reports whether want can be attempted against a copy in
// access state s, and refuses with a wrapped placement.ErrClassUnavailable
// when it cannot.
//
// The error type is deliberate and load-bearing. internal/placement draws
// one distinction above all others: "I checked and it is wrong" is a fact
// about the artifact and quarantines it, while "I could not check" is a
// fact about the endpoint and must not. An archived object is the second
// one. A refusal that arrived as a failed Result instead would eventually
// quarantine a perfectly good backup for the crime of being cheap to
// store.
func CheckClass(s State, want placement.Class) error {
	if !want.Valid() {
		return fmt.Errorf("%w: %q is not a verification class", placement.ErrClassUnavailable, want)
	}
	ceiling := Ceiling(s)
	if ceiling == "" {
		return fmt.Errorf("%w: this copy's access state is %q, so no class can be attempted against it at all",
			placement.ErrClassUnavailable, s)
	}
	if want.Stronger(ceiling) {
		return fmt.Errorf(
			"%w: this copy's access state is %q, which supports %s at best (%s), and %s needs %s",
			placement.ErrClassUnavailable, s, ceiling, ceiling.Proves(), want, want.Cost())
	}
	return nil
}

// Verify is internal/placement's verification with the archive gate in
// front of it, and it is the entry point every caller that might be
// looking at a medium copy should use.
//
// It adds exactly one thing to placement.Verify: it decides, from facts it
// already holds, whether the class being asked for could possibly be
// achieved, and refuses BEFORE spending a request on finding out. That
// ordering is the point rather than an optimisation. Asking S3 to GET an
// archived object costs a request and returns InvalidObjectState, and a
// caller that reads that as "the verification failed" has just decided a
// good backup is bad. Refusing first means the caller is told what is
// actually true, which is that nobody has read those bytes and nobody can
// until a restore happens.
//
// It never falls back to a weaker class. That is placement.Verify's rule
// and this function does not get to soften it: a caller that wants the
// existence check against an archived copy asks for existence, in its own
// code, where somebody reviewing it can see the decision.
func Verify(
	ctx context.Context,
	store placement.Store,
	medium transport.Medium,
	p state.Placement,
	want placement.Class,
	obs Observation,
	now time.Time,
) (placement.Result, error) {
	access, err := Access(p.Medium, medium.StorageClass, obs, now)
	if err != nil {
		return placement.Result{}, fmt.Errorf("%w: %w", placement.ErrClassUnavailable, err)
	}
	if err := CheckClass(access, want); err != nil {
		return placement.Result{}, err
	}
	return placement.Verify(ctx, store, medium, p, want, now)
}

// AutomaticClass is the strongest class a scheduled, unattended pass may
// run against a copy in access state s, and it is capped a rung below
// Ceiling for one reason: money.
//
// FR-31 makes anything that costs egress operator-initiated, and
// placement.Class.CostsEgress is the mechanism rather than the promise.
// This function reads it rather than hard-coding "existence", so raising
// the automatic ceiling means changing the class whose CostsEgress is
// consulted, not editing a constant and finding out from a bill.
//
// An archive class makes that worse and not better. A restore is billed
// on top of the egress, and it is billed for a window measured in days, so
// an automatic pass that could trigger one would keep triggering it. There
// is no path here that can: Ceiling already refuses content for an
// archived copy, and this refuses it again for every copy, and neither
// refusal is reachable by configuration.
func AutomaticClass(s State) placement.Class {
	c := Ceiling(s)
	if c == "" {
		return ""
	}
	if c.CostsEgress() {
		return placement.Existence
	}
	return c
}
