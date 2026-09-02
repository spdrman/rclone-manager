// Package placement owns what it means for a copy of an artifact to be
// verified, and what each way of finding out costs (EPIC E, FR-31).
//
// # Why a ladder and not a boolean
//
// Everything this product did before EPIC E assumed an artifact's durable
// copy is a local file it can re-read: verification opened it and hashed
// it, and "verified" meant that hash matched. Off local disk that stops
// being one question with one answer. Downloading a hundred gigabytes from
// an object store to re-hash it is a real bill, and asking the endpoint
// what it thinks the checksum is costs nothing and proves less, and asking
// whether the object exists at all costs even less and proves less still.
//
// So there are three answers, they are ordered, and every surface reports
// the one that was actually ACHIEVED rather than the one that was
// configured or hoped for. That last part is the whole point of this
// package existing at all: an existence check reported as "verified" is
// worse than no check, because it converts "nobody has looked at this
// backup in a year" into a green tick.
//
// # The rule this package exists to make structural
//
// FR-13's rule, restated by FR-31: where an endpoint cannot produce what a
// class needs, the answer is an explicit capability refusal, never a
// weaker class wearing a stronger name. Verify returns the class it ran,
// always, and it has no path that returns a class it did not run.
package placement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Class is one rung of FR-31's verification ladder.
//
// The strings are the ones internal/state stores and 0007_placements.sql
// constrains, and ladder_test.go pins the two lists together, so a class
// added here without a migration cannot be written and a class the schema
// admits without a rung here cannot be produced.
type Class string

const (
	// Content is the strongest: the bytes were read back and hashed, and
	// they match the hash the journal recorded at ingestion.
	Content Class = Class(state.VerificationContent)

	// Attested is the provider's own word: its stored full-object
	// checksum equals the recorded hash. One metadata call, no egress,
	// and it trusts the endpoint to implement checksum semantics
	// honestly. An endpoint that lies here can cause a local copy to be
	// deleted against a bad upload, which is why it is opt-in per medium
	// and why the configuration documentation says so in plain words.
	Attested Class = Class(state.VerificationAttested)

	// Existence is the weakest: the object is there, at the recorded
	// size. One HEAD request. It proves nothing whatever about the bytes,
	// and it is never sufficient to delete a source copy.
	Existence Class = Class(state.VerificationExistence)
)

// Classes is the ladder, strongest first. Order is meaningful: it is what
// "stronger than" means, and Stronger reads it rather than hard-coding a
// comparison somewhere it could drift from this list.
var Classes = []Class{Content, Attested, Existence}

// Valid reports whether c is one of the three rungs.
//
// The empty class is not one. It is what a placement nothing has verified
// carries, and it is deliberately not a Class value: a caller reaching for
// a name for "unverified" is usually about to record it as a weak pass,
// and the point of a ladder is that a rung is something achieved.
func (c Class) Valid() bool {
	for _, known := range Classes {
		if c == known {
			return true
		}
	}
	return false
}

// Stronger reports whether c proves strictly more than other.
func (c Class) Stronger(other Class) bool {
	ci, oi := -1, -1
	for i, known := range Classes {
		if c == known {
			ci = i
		}
		if other == known {
			oi = i
		}
	}
	if ci < 0 {
		return false
	}
	if oi < 0 {
		// Anything on the ladder is stronger than something that is not
		// on it, which is what an unverified placement is.
		return true
	}
	return ci < oi
}

// CostsEgress reports whether achieving c means downloading the object's
// bytes.
//
// It is the machine-checkable half of FR-31's cost column, and it is not
// documentation: the automatic revalidation path refuses any class where
// this is true, so "automatic medium revalidation never downloads" is a
// property of the code rather than a promise in a comment. Silent egress
// is a surprise bill, and a surprise bill is how an operator learns to
// turn a safety feature off.
func (c Class) CostsEgress() bool { return c == Content }

// Cost describes, in words an operator reads, what achieving c takes.
func (c Class) Cost() string {
	switch c {
	case Content:
		return "a full download of the object: time plus egress, and for an archive storage class a restore first"
	case Attested:
		return "one metadata call, no egress, trusting the endpoint's own checksum"
	case Existence:
		return "one HEAD request, which says nothing about the bytes"
	default:
		return "unknown"
	}
}

// Proves describes, in words an operator reads, what achieving c actually
// establishes. It is deliberately separate from Cost: the pair is what
// makes a choice between rungs an informed one, and a surface that shows
// only the cost invites picking the cheapest.
func (c Class) Proves() string {
	switch c {
	case Content:
		return "the bytes on the medium hash to the hash this product recorded when it ingested the artifact"
	case Attested:
		return "the provider's stored full-object checksum equals the recorded hash"
	case Existence:
		return "an object exists at the recorded key, at the recorded size"
	default:
		return "nothing"
	}
}

// Result is what one verification attempt actually achieved.
//
// Class is the class that RAN, never the class that was asked for, and
// there is no path in this package that sets it to anything else. A failed
// attempt still carries the class it ran, because "the content check
// failed" and "the existence check failed" are different facts about an
// artifact and the surfaces have to be able to tell them apart.
type Result struct {
	// Class is the rung this attempt ran.
	Class Class

	// Passed is whether the placement satisfied that rung.
	Passed bool

	// At is when the attempt ran.
	At time.Time

	// Detail is a short, human-readable explanation, suitable for a log
	// line or an audit trail. It never contains a credential, because
	// nothing in this package has one.
	Detail string
}

// ErrClassUnavailable is returned when a class cannot be attempted at all
// against this placement, as opposed to being attempted and failing.
//
// The two must never collapse into one another, for the reason
// artifactstore.ErrNotPresent exists: "we checked and it is wrong" is a
// fact about the artifact, and "we could not check" is a fact about the
// endpoint or the record, and a caller that confuses them either
// quarantines a perfectly good backup or believes an unverified one.
var ErrClassUnavailable = errors.New("placement: that verification class cannot be achieved for this placement")

// Store is the slice of transport.MediumStore this package needs. Stating
// it here rather than taking the whole interface is what lets a test
// substitute a double without implementing upload or delete, and it is
// also a statement of intent: verification reads, and it has no business
// holding a method that can destroy an object.
type Store interface {
	StatObject(ctx context.Context, medium transport.Medium, key string) (transport.ObjectInfo, error)
	OpenObject(ctx context.Context, medium transport.Medium, key string) (io.ReadCloser, error)
	ObjectChecksum(ctx context.Context, medium transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error)
}

// Verify attempts exactly the class it is asked for against p, on medium,
// and reports what it achieved.
//
// It NEVER falls back. A caller that asks for Attested against an endpoint
// that cannot attest gets ErrClassUnavailable, not an Existence result
// with an Attested label; a caller that wants the fallback asks for the
// weaker class itself, in its own code, where the decision is visible.
//
// A non-nil error means the class could not be attempted. A Result with
// Passed false means it was attempted and the placement failed it. Those
// are the only two shapes.
func Verify(ctx context.Context, store Store, medium transport.Medium, p state.Placement, want Class, now time.Time) (Result, error) {
	if !want.Valid() {
		return Result{}, fmt.Errorf("%w: %q is not one of %v", ErrClassUnavailable, want, Classes)
	}
	if store == nil {
		return Result{}, fmt.Errorf("%w: no medium store is configured", ErrClassUnavailable)
	}
	if p.Location == "" {
		return Result{}, fmt.Errorf("%w: the placement on %q records no location", ErrClassUnavailable, p.Medium)
	}

	switch want {
	case Content:
		return verifyContent(ctx, store, medium, p, now)
	case Attested:
		return verifyAttested(ctx, store, medium, p, now)
	default:
		return verifyExistence(ctx, store, medium, p, now)
	}
}

// verifyContent downloads the object and re-hashes it against the hash the
// journal recorded.
//
// A placement with no recorded hash cannot be content-verified, and that
// is a capability refusal rather than a pass: there is nothing to compare
// against, and reporting "content verified" for a comparison that never
// happened is the exact dishonesty this package exists to prevent.
func verifyContent(ctx context.Context, store Store, medium transport.Medium, p state.Placement, now time.Time) (Result, error) {
	if p.Hash == "" || !strings.EqualFold(p.HashAlg, string(transport.SHA256)) {
		return Result{}, fmt.Errorf("%w: content verification compares against a recorded %s, and this placement records %q",
			ErrClassUnavailable, transport.SHA256, p.HashAlg)
	}

	rc, err := store.OpenObject(ctx, medium, p.Location)
	if err != nil {
		return Result{}, fmt.Errorf("%w: reading %q back from %q: %w", ErrClassUnavailable, p.Location, p.Medium, err)
	}
	defer rc.Close()

	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return Result{}, fmt.Errorf("%w: reading %q back from %q: %w", ErrClassUnavailable, p.Location, p.Medium, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))

	if !strings.EqualFold(sum, p.Hash) {
		return Result{
			Class: Content, Passed: false, At: now,
			Detail: fmt.Sprintf("%s on %s hashes to %s, but the hash recorded at ingestion was %s", p.Location, p.Medium, sum, p.Hash),
		}, nil
	}
	return Result{
		Class: Content, Passed: true, At: now,
		Detail: fmt.Sprintf("%s on %s was read back and still hashes to the %s recorded at ingestion", p.Location, p.Medium, p.HashAlg),
	}, nil
}

// verifyAttested asks the medium for its own full-object digest.
//
// The capability refusal is the interesting path and the one FR-13 is
// about. Against the rclone this product embeds today, an s3 medium takes
// it every single time: rclone v1.75.0's s3 backend exposes MD5 from the
// ETag and refuses every other algorithm, so no S3 endpoint reachable
// through this build can attest a full-object SHA-256. That is not a gap
// in this function; it is this function reporting the truth.
func verifyAttested(ctx context.Context, store Store, medium transport.Medium, p state.Placement, now time.Time) (Result, error) {
	if p.Hash == "" || !strings.EqualFold(p.HashAlg, string(transport.SHA256)) {
		return Result{}, fmt.Errorf("%w: an attestation is compared against a recorded %s, and this placement records %q",
			ErrClassUnavailable, transport.SHA256, p.HashAlg)
	}

	attestation, err := store.ObjectChecksum(ctx, medium, p.Location, transport.SHA256)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %q cannot attest a full-object %s for %q: %w",
			ErrClassUnavailable, p.Medium, transport.SHA256, p.Location, err)
	}
	if attestation.Value == "" {
		return Result{}, fmt.Errorf("%w: %q returned an empty attestation for %q",
			ErrClassUnavailable, p.Medium, p.Location)
	}

	if !strings.EqualFold(attestation.Value, p.Hash) {
		return Result{
			Class: Attested, Passed: false, At: now,
			Detail: fmt.Sprintf("%s on %s is attested as %s, but the hash recorded at ingestion was %s", p.Location, p.Medium, attestation.Value, p.Hash),
		}, nil
	}
	return Result{
		Class: Attested, Passed: true, At: now,
		Detail: fmt.Sprintf("%s on %s is attested by the endpoint as the %s recorded at ingestion", p.Location, p.Medium, p.HashAlg),
	}, nil
}

// verifyExistence HEADs the object and compares its size.
//
// A placement with no recorded size still passes on existence alone, and
// says so: an object being there is genuinely all this rung claims, and
// refusing for want of a size would make the cheapest check unavailable
// exactly where the record is thinnest. What it must never do is imply
// more, which is why the detail says which of the two it managed.
func verifyExistence(ctx context.Context, store Store, medium transport.Medium, p state.Placement, now time.Time) (Result, error) {
	info, err := store.StatObject(ctx, medium, p.Location)
	if err != nil {
		category, _ := transport.CategoryOf(err)
		if category == transport.NotFound {
			return Result{
				Class: Existence, Passed: false, At: now,
				Detail: fmt.Sprintf("%s is not present on %s", p.Location, p.Medium),
			}, nil
		}
		// Anything else is the medium failing to answer, which is a fact
		// about the medium and not about the artifact.
		return Result{}, fmt.Errorf("%w: %q could not be asked about %q: %w", ErrClassUnavailable, p.Medium, p.Location, err)
	}

	if p.Size == nil {
		return Result{
			Class: Existence, Passed: true, At: now,
			Detail: fmt.Sprintf("%s is present on %s at %d bytes; no size was recorded for this placement, so only its presence was confirmed", p.Location, p.Medium, info.Size),
		}, nil
	}
	if info.Size != *p.Size {
		return Result{
			Class: Existence, Passed: false, At: now,
			Detail: fmt.Sprintf("%s on %s is %d bytes, but %d was recorded for this placement", p.Location, p.Medium, info.Size, *p.Size),
		}, nil
	}
	return Result{
		Class: Existence, Passed: true, At: now,
		Detail: fmt.Sprintf("%s is present on %s at the recorded %d bytes", p.Location, p.Medium, info.Size),
	}, nil
}
