// Package mediumcheck is the medium half of what a backup set has had
// since it was written: a way to find out whether the place an operator
// just described actually works, before a cycle carrying a real backup
// finds out for them (EPIC E, FR-28/FR-31/FR-33; issue #443).
//
// # What was missing
//
// A backup set has POST /backup-sets/test-connection. Somebody writing a
// source can prove this manager reaches it before anything depends on it.
// A storage medium had no equivalent, so the first thing in this product
// ever to touch a bucket was a move, in the middle of a cycle, after an
// artifact had already been selected to leave local disk. A wrong region,
// a bucket that is not there, a credentials file the daemon cannot read,
// a policy that denies PutObject: every one of those was discovered by the
// operation that needed it to work.
//
// # It proves what will bite, not that the bucket answers
//
// A reachability ping is the check that feels like enough and is not. What
// a move actually needs is a WRITE, a READ of what it wrote, a DELETE of
// what it wrote, and the verification class the medium's own configuration
// claims. So the preflight does each of those, in that order, against a
// probe object of its own, and rolls the probe back.
//
// The verification step is the one worth reading twice. FR-31's `attested`
// class asks the endpoint for its own full-object digest, and measured
// against rclone v1.75.0 an s3 medium can never produce one:
// backend/s3.Fs.Hashes() returns exactly hash.MD5, that MD5 is the ETag,
// and FR-32 says an ETag is never a content hash. So a medium declared
// `upload_verification: attested` cannot serve one move, ever, on this
// build, and a preflight that reported it green would be lying about the
// only thing it exists to establish. This one asks, and reports the
// refusal.
//
// # What it never says
//
// FR-33: no credential, no signed URL, no key material, and the way that
// is kept true is structural rather than careful. Every sentence in a
// Report is one of this package's OWN strings, chosen from the step and
// the transport category, composed only out of facts this manager already
// publishes about a medium (its id, its bucket, its storage class, its
// verification class). No underlying error text is ever copied into a
// Report, and there is no field one could travel in. That costs a
// diagnostic, deliberately: the classified cause names a path on the host
// or the name of an environment variable, which is a fact about this
// machine that an API caller has no use for and a reader of an exported
// response has every use for. It goes to the log through Deps.Observe
// instead, which is where the operator's own diagnostics already live.
//
// # Where it does not belong
//
// Not in config.Validate. Validation is a decision about a file, taken
// with no network, and it has to stay that way: a check that reaches out
// is a check that fails when the network is down, on a config that is
// fine. This package is the other thing, and it is only ever called
// because somebody asked for it.
package mediumcheck

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Step names one thing the preflight proves. The set is closed and
// ordered: Steps below is the order they run in, and a Report always
// carries one Check per step so a surface can render a fixed list rather
// than discovering which ones happened to run.
type Step string

const (
	// StepCredentials is whether the credential this medium DECLARES can
	// be obtained at all. It is its own step, separate from whether the
	// endpoint accepts it, because they are different jobs for a person:
	// one is a file or a variable or a command on this host, the other is
	// a policy at the provider.
	StepCredentials Step = "credentials"

	// StepReach is whether the endpoint answers and holds the bucket this
	// medium names, with the credential that was obtained.
	StepReach Step = "reach"

	// StepDeliverable is whether an artifact can be DELIVERED to this
	// medium's storage class at all. An archive class cannot take
	// delivery: its objects are durable and unreadable until an explicit
	// restore finishes, so a copy written there can never reach the
	// content class a source delete requires.
	StepDeliverable Step = "deliverable"

	// StepWrite is a real PutObject, with this medium's own configured
	// storage class.
	StepWrite Step = "write"

	// StepReadBack is a real GetObject of what StepWrite just wrote,
	// compared byte for byte. Read access is not what a move needs, and
	// it is exactly what a RESTORE needs, so both halves are proved.
	StepReadBack Step = "read_back"

	// StepStorageClass is whether the object came back in the class the
	// configuration asked for. An S3-compatible endpoint that silently
	// ignores storage_class is a real thing, and the config file goes on
	// claiming a class nothing applied.
	StepStorageClass Step = "storage_class"

	// StepVerification is whether the verification class this medium's
	// own upload_verification declares can actually be ACHIEVED here.
	StepVerification Step = "verification"

	// StepDelete is a real DeleteObject of the probe, confirmed gone. It
	// is the rollback and a check at once: a medium this manager can write
	// to but not delete from is one where FR-30's prune silently does
	// nothing.
	StepDelete Step = "delete"
)

// Steps is every step, in the order Run performs them.
var Steps = []Step{
	StepCredentials, StepReach, StepDeliverable, StepWrite,
	StepReadBack, StepStorageClass, StepVerification, StepDelete,
}

// Outcome is what one step produced.
type Outcome string

const (
	// Passed: the step ran and proved what it is for.
	Passed Outcome = "passed"

	// Failed: the step ran and did not.
	Failed Outcome = "failed"

	// Skipped: the step never ran, because an earlier one failed in a way
	// that makes this one meaningless. It is a first-class outcome and not
	// a quiet pass: a surface that renders a skipped write as anything but
	// "this was never tried" has told an operator their bucket is writable
	// on the strength of a credential that was never obtained.
	Skipped Outcome = "skipped"
)

// Check is one step's result.
type Check struct {
	Step    Step
	Outcome Outcome

	// Category is the transport category the failure classified as, as a
	// name ("configuration", "authentication", ...), or empty when the
	// step passed, was skipped, or failed for a reason no transport
	// produced. It is the machine-readable half: a surface branches on
	// this, never on Detail.
	Category string

	// Detail is one of this package's own sentences. It never carries an
	// underlying error's text: see the package doc on FR-33.
	Detail string
}

// Report is one preflight.
type Report struct {
	// Medium is the medium id this ran against.
	Medium string

	// OK is true only when every step either passed or was skipped for a
	// reason that is not a failure. In practice: no Check has Outcome
	// Failed.
	OK bool

	// Checks is one entry per Step, in Steps order.
	Checks []Check
}

// Failures returns the checks that failed, in order. It exists so a
// surface, or a test, states the question once rather than filtering by
// hand at each call site.
func (r Report) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Outcome == Failed {
			out = append(out, c)
		}
	}
	return out
}

// Deps is what Run is handed.
type Deps struct {
	// Store is the FR-28 boundary this whole check runs through. It is
	// the same interface the move engine uses, deliberately: a preflight
	// that reached the endpoint by some other route would be proving
	// something about a code path no backup ever takes.
	Store transport.MediumStore

	// Observe, when set, is called once per failed step with the
	// CLASSIFIED cause, so an operator's log keeps the diagnostic the
	// Report deliberately does not carry. Nil discards it.
	Observe func(step Step, err error)
}

func (d Deps) observe(step Step, err error) {
	if d.Observe != nil && err != nil {
		d.Observe(step, err)
	}
}

// probePrefix is the key segment every probe object this package writes
// lives under, inside the medium's own prefix.
//
// A segment of its own, rather than a suffix on a name, for the reason
// internal/placement's staging area is a subdirectory: an artifact may
// legitimately be called anything, and a shared namespace is how a probe
// and a backup come to have one spelling. transport.MediumKey composes an
// artifact's key out of the backup set's source, its name and the
// artifact's name, none of which config lets contain a "/", so nothing an
// operator can configure produces a key under this segment.
const probePrefix = ".rclone-manager-preflight"

// probeBody is what a probe object contains. It is fixed, small, and says
// what it is, so an operator who finds one left behind by a preflight this
// process was killed in the middle of knows immediately what it is and
// that deleting it is safe.
var probeBody = []byte("rclone-manager medium preflight probe. This object is written and deleted by a preflight check and is safe to remove.\n")

// Run performs one preflight against medium and reports what it found.
//
// requiredClass is the verification class this medium's own
// upload_verification resolves to (internal/app's MediumResolver is where
// that mapping lives, and it stays there: this package must not be a
// second answer to what a configured word means).
//
// The returned error is non-nil only for something that stopped the
// preflight from running at all, such as being handed no Store or a
// cancelled context. A medium that does not work is a successful call with
// a Report saying so, exactly like BackupService.TestConnection reports a
// bad host through its result rather than through an error: a bucket that
// is not there is what an operator did, not what broke.
func Run(ctx context.Context, deps Deps, medium transport.Medium, requiredClass placement.Class) (Report, error) {
	if deps.Store == nil {
		return Report{}, fmt.Errorf("mediumcheck: preflight needs a MediumStore")
	}
	if medium.ID == "" {
		return Report{}, fmt.Errorf("mediumcheck: preflight needs a medium with an id")
	}
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("mediumcheck: preflight: %w", err)
	}

	key, err := probeKey(medium.Prefix)
	if err != nil {
		return Report{}, fmt.Errorf("mediumcheck: preflight: %w", err)
	}

	r := &run{deps: deps, medium: medium, required: requiredClass, key: key}
	r.reachable(ctx)
	r.deliverable(ctx)
	r.written(ctx)
	return r.report(), nil
}

// run holds one preflight's state while the steps walk forward. Each step
// method records its own Check and, where a later step depends on it, sets
// a flag the later one reads, so the "skipped because an earlier step
// failed" answer is produced in one place rather than re-derived.
type run struct {
	deps     Deps
	medium   transport.Medium
	required placement.Class
	key      string

	checks map[Step]Check

	// reached is true once the endpoint has answered with the bucket
	// there. Everything that touches an object depends on it.
	reached bool

	// deliverable is true when an artifact can be written here at all.
	// An archive class cannot take delivery, and a probe written to one
	// is billed for a minimum duration measured in months, so this gates
	// the write rather than merely annotating it.
	canDeliver bool

	// wrote is true once the probe object exists, which is what makes the
	// delete a rollback rather than an extra call.
	wrote bool
}

func (r *run) record(step Step, outcome Outcome, category, detail string) {
	if r.checks == nil {
		r.checks = make(map[Step]Check, len(Steps))
	}
	r.checks[step] = Check{Step: step, Outcome: outcome, Category: category, Detail: detail}
}

func (r *run) pass(step Step, detail string) { r.record(step, Passed, "", detail) }
func (r *run) skip(step Step, detail string) { r.record(step, Skipped, "", detail) }
func (r *run) fail(step Step, err error, detail string) {
	r.deps.observe(step, err)
	r.record(step, Failed, categoryName(err), detail)
}

func (r *run) report() Report {
	out := Report{Medium: r.medium.ID, OK: true, Checks: make([]Check, 0, len(Steps))}
	for _, step := range Steps {
		c, ok := r.checks[step]
		if !ok {
			// Unreachable while every step records itself, and here so
			// that a step added to Steps and not to the walk shows up as
			// a hole rather than as silence.
			c = Check{Step: step, Outcome: Skipped, Detail: "this check did not run"}
		}
		if c.Outcome == Failed {
			out.OK = false
		}
		out.Checks = append(out.Checks, c)
	}
	return out
}

// reachable is the credentials and reach pair, taken from one probe.
//
// One call answers both because they are one call: the credential is
// resolved on the way to the endpoint, so a failure is either "the
// credential never got made" or "everything past that". The two are told
// apart by transport.ErrCredentialsUnavailable, a sentinel rather than a
// category, because a bucket that is not there is Configuration too.
//
// The probe is a HEAD of a key nothing has ever written, so its SUCCESS
// case is a NotFound: the medium answered, with the credential accepted,
// and the object is not there. That is the strongest thing one free call
// can establish, and it is deliberately not a listing: a listing needs
// s3:ListBucket, a move does not, and a preflight that demanded a
// permission the product never uses would refuse a correctly scoped
// policy.
func (r *run) reachable(ctx context.Context) {
	_, err := r.deps.Store.StatObject(ctx, r.medium, r.key)

	category, classified := transport.CategoryOf(err)
	switch {
	case err == nil:
		// A probe key that already exists. Only a preflight writes one,
		// so this is a leftover from a run that was killed between its
		// write and its delete. It answers both questions anyway.
		r.pass(StepCredentials, r.credentialsPassed())
		r.pass(StepReach, r.reachPassed())
		r.reached = true

	case errors.Is(err, transport.ErrCredentialsUnavailable):
		r.fail(StepCredentials, err, fmt.Sprintf(
			"the credential storage medium %q declares could not be obtained, so nothing was sent anywhere. "+
				"This is a question for this host rather than for the provider: check the credentials block for this medium in the configuration. "+
				"What exactly went wrong names a path or an environment variable on this machine, so it is written to this manager's log rather than returned here (FR-33)",
			r.medium.ID))
		r.skip(StepReach, "the endpoint was never contacted, because there was no credential to contact it with")

	case classified && category == transport.NotFound:
		r.pass(StepCredentials, r.credentialsPassed())
		r.pass(StepReach, r.reachPassed())
		r.reached = true

	default:
		r.pass(StepCredentials, r.credentialsPassed())
		r.fail(StepReach, err, r.reachFailed(category, classified))
	}
}

func (r *run) credentialsPassed() string {
	return fmt.Sprintf("the credential storage medium %q declares was obtained and the endpoint accepted it", r.medium.ID)
}

func (r *run) reachPassed() string {
	return fmt.Sprintf("the endpoint answered and holds bucket %q", r.medium.Bucket)
}

// reachFailed is where the FR-22 categories earn their keep: each one is a
// different person's job, and saying which is the whole point of keeping
// them apart at the transport boundary.
func (r *run) reachFailed(category transport.Category, classified bool) string {
	if !classified {
		return fmt.Sprintf(
			"the endpoint did not answer for bucket %q, and the failure could not be classified; this manager's log has what came back",
			r.medium.Bucket)
	}
	switch category {
	case transport.Configuration:
		return fmt.Sprintf(
			"the endpoint answered and does not have bucket %q. This manager never creates a bucket, deliberately, so a typo in a bucket name or a region stays a typo instead of becoming a second empty home for backups nobody looks in. Check the bucket and region for this medium",
			r.medium.Bucket)
	case transport.Authentication:
		return "the endpoint rejected the credential this medium declares. The credential was obtained, so this is a question for the provider rather than for this host: check that the key is active and that it belongs to the account this bucket is in"
	case transport.PermissionDenied:
		return fmt.Sprintf(
			"the credential is accepted but is not allowed to read objects in bucket %q. A move stats an object after it writes it, and a restore reads one back, so a policy that denies this cannot serve either",
			r.medium.Bucket)
	case transport.Transient:
		return "the endpoint could not be reached. Nothing here is a statement about the configuration: this is the one outcome worth simply trying again"
	default:
		return fmt.Sprintf("the endpoint could not be asked about bucket %q; this manager's log has what came back", r.medium.Bucket)
	}
}

// deliverable answers, without spending anything, whether an artifact can
// be written here at all.
//
// It is local knowledge: internal/archive's table says which storage
// classes hold objects that cannot be read until a restore finishes, and
// FR-30's standing invariant needs an ACTIVE placement at content class,
// which an archived copy cannot hold. So a retention tier can never
// deliver to one, and a probe written to one would be billed for a minimum
// duration measured in months for an answer already known.
//
// It refuses delivery without refusing the medium. A declared archive-class
// medium holding pre-existing objects to RESTORE is a legitimate thing to
// configure, and the restore path is what it is for; what cannot work is
// delivering a new artifact to it. The config layer draws the same line in
// the same place, at the tier-to-medium pairing rather than at the medium
// declaration.
func (r *run) deliverable(ctx context.Context) {
	if !r.reached {
		r.skip(StepDeliverable, "the medium's storage class was not examined, because the endpoint could not be reached to act on it")
		return
	}
	behaviour, err := archive.Of(r.medium.StorageClass)
	if err != nil {
		r.fail(StepDeliverable, err, fmt.Sprintf(
			"storage class %q is not one this build knows the behaviour of, so this manager cannot say whether an artifact written here could ever be read back",
			r.medium.StorageClass))
		return
	}
	if behaviour.Archive {
		r.fail(StepDeliverable, nil, fmt.Sprintf(
			"storage class %s holds objects that cannot be read until an explicit restore has been asked for and has finished, so an artifact delivered here could never reach the content verification a source delete requires. "+
				"A retention tier cannot deliver to this medium. Declaring it to RESTORE objects that are already there is a different thing and stays legal",
			behaviour.Class))
		return
	}
	r.canDeliver = true
	r.pass(StepDeliverable, fmt.Sprintf("storage class %s reads on demand, so an artifact delivered here can be verified and later restored", behaviour.Class))
}

// written is the write / read-back / storage-class / verification / delete
// sequence, plus the rollback that has to happen however far down that
// list the run got.
func (r *run) written(ctx context.Context) {
	if !r.reached || !r.canDeliver {
		reason := "nothing was written, because the endpoint could not be reached"
		if r.reached {
			reason = "nothing was written, because an artifact cannot be delivered to this medium's storage class"
		}
		for _, step := range []Step{StepWrite, StepReadBack, StepStorageClass, StepVerification, StepDelete} {
			r.skip(step, reason)
		}
		return
	}

	local, err := writeProbeFile()
	if err != nil {
		r.fail(StepWrite, err, "this manager could not stage the probe object on local disk, so nothing was sent; the endpoint was never asked")
		for _, step := range []Step{StepReadBack, StepStorageClass, StepVerification, StepDelete} {
			r.skip(step, "nothing was written, so there was nothing to check")
		}
		return
	}
	defer func() { _ = os.Remove(local) }()

	if _, err := r.deps.Store.UploadFromLocal(ctx, r.medium, local, r.key, transport.UploadOptions{}); err != nil {
		r.fail(StepWrite, err, r.writeFailed(err))
		for _, step := range []Step{StepReadBack, StepStorageClass, StepVerification} {
			r.skip(step, "nothing was written, so there was nothing to check")
		}
		// The delete still runs. An upload that reported a failure may
		// still have left an object behind (a multipart that completed
		// and then failed its own confirmation, say), and leaving one
		// behind is exactly what "roll it back" is meant to prevent.
		r.deleted(ctx)
		return
	}
	r.wrote = true
	r.pass(StepWrite, fmt.Sprintf("an object was written to bucket %q with storage class %s", r.medium.Bucket, r.classAsked()))

	r.readBack(ctx)
	r.observedClass(ctx)
	r.verification(ctx)
	r.deleted(ctx)
}

func (r *run) classAsked() string {
	if r.medium.StorageClass == "" {
		return "the endpoint's own default"
	}
	return r.medium.StorageClass
}

func (r *run) writeFailed(err error) string {
	category, classified := transport.CategoryOf(err)
	if classified && category == transport.PermissionDenied {
		return fmt.Sprintf(
			"the credential is accepted but is not allowed to write to bucket %q. This is the failure a cycle would have found for you, in the middle of a move, after an artifact had already been chosen to leave local disk",
			r.medium.Bucket)
	}
	if classified && category == transport.Configuration && r.medium.StorageClass != "" {
		return fmt.Sprintf(
			"the endpoint refused the write to bucket %q as configured; storage class %s is the part of this configuration an endpoint most often does not accept",
			r.medium.Bucket, r.medium.StorageClass)
	}
	return fmt.Sprintf("the object could not be written to bucket %q; this manager's log has what came back", r.medium.Bucket)
}

// readBack proves the bytes come back. Byte-for-byte rather than by size,
// because a size comparison is the check that passes against a truncated
// or re-encoded object, and a restore is the moment nobody wants to find
// that out.
func (r *run) readBack(ctx context.Context) {
	rc, err := r.deps.Store.OpenObject(ctx, r.medium, r.key)
	if err != nil {
		r.fail(StepReadBack, err, fmt.Sprintf(
			"the object this preflight just wrote to bucket %q could not be read back. A write nobody can read is not a backup, and a restore is exactly this call",
			r.medium.Bucket))
		return
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(io.LimitReader(rc, int64(len(probeBody))+1))
	if err != nil {
		r.fail(StepReadBack, err, fmt.Sprintf("the object this preflight just wrote to bucket %q could not be read to the end", r.medium.Bucket))
		return
	}
	if string(got) != string(probeBody) {
		r.fail(StepReadBack, nil, fmt.Sprintf(
			"the object read back from bucket %q is not the object that was written: %d bytes came back where %d went out. Nothing this manager does after an upload can be trusted against an endpoint that does this",
			r.medium.Bucket, len(got), len(probeBody)))
		return
	}
	r.pass(StepReadBack, "the object was read back and is byte for byte what was written")
}

// observedClass compares what the configuration asked for against what the
// endpoint says it did.
//
// An S3-compatible endpoint that accepts a storage_class and ignores it is
// a real thing, and the cost of not noticing is a config file that goes on
// claiming a class nothing applied: an operator budgeting for STANDARD_IA
// and paying for STANDARD, or, worse, believing a class carries a
// retrieval characteristic it does not have.
//
// An endpoint that reports NO class is a different answer and gets a
// different one back. It is not evidence that the class was ignored, so it
// is not a failure; it is the absence of the evidence, and saying so is
// the honest thing this product does everywhere else it cannot confirm
// something.
func (r *run) observedClass(ctx context.Context) {
	info, err := r.deps.Store.StatObject(ctx, r.medium, r.key)
	if err != nil {
		r.fail(StepStorageClass, err, fmt.Sprintf(
			"the object this preflight just wrote to bucket %q could not be stat'd, so the class it landed in is unknown", r.medium.Bucket))
		return
	}
	switch {
	case info.StorageClass == "":
		r.pass(StepStorageClass, fmt.Sprintf(
			"the endpoint reports no storage class for the object it just stored, so this manager cannot confirm that %s was applied. That is an absence of evidence rather than evidence of a problem, and it is what an S3-compatible endpoint with no tiering does",
			r.classAsked()))
	case r.medium.StorageClass == "":
		r.pass(StepStorageClass, fmt.Sprintf("this medium names no storage class, and the endpoint stored the object as %s", info.StorageClass))
	case strings.EqualFold(info.StorageClass, r.medium.StorageClass):
		r.pass(StepStorageClass, fmt.Sprintf("the endpoint stored the object as %s, which is the class this medium declares", info.StorageClass))
	default:
		r.fail(StepStorageClass, nil, fmt.Sprintf(
			"this medium declares storage class %s and the endpoint stored the object as %s. The endpoint accepted the request and did something else with it, so the configuration is claiming a class nothing applied",
			r.medium.StorageClass, info.StorageClass))
	}
}

// verification asks whether the class this medium's own configuration
// requires can actually be achieved here, and it asks the endpoint rather
// than a table.
//
// This is the step that exists because of what would otherwise be a lie.
// FR-31's `attested` class means the endpoint's own full-object digest,
// and against rclone v1.75.0 an s3 medium cannot produce one at all
// (backend/s3.Fs.Hashes() returns exactly hash.MD5, and that MD5 is the
// ETag, which FR-32 says is never a content hash). A medium declared
// `upload_verification: attested` therefore cannot serve a single move on
// this build, and the move engine refuses it at the point a local copy
// would otherwise have been deleted. The whole purpose of a preflight is
// that an operator learns that here instead.
//
// The capability is asked LIVE, not looked up, so a future rclone that
// surfaces x-amz-checksum-sha256 turns this green with no edit.
func (r *run) verification(ctx context.Context) {
	switch r.required {
	case placement.Content:
		// Already proved, by the step above: content IS reading the bytes
		// back and comparing them. Saying which step proved it, rather
		// than re-reading the object, keeps the preflight's egress to one
		// copy of a probe.
		if c, ok := r.checks[StepReadBack]; ok && c.Outcome == Passed {
			r.pass(StepVerification, fmt.Sprintf(
				"this medium requires the %s class, which is reading the bytes back and comparing them, and that is exactly what the read-back step just did", placement.Content))
			return
		}
		r.fail(StepVerification, nil, fmt.Sprintf(
			"this medium requires the %s class, which means reading every uploaded object back and comparing it, and the read-back above did not succeed", placement.Content))

	case placement.Attested:
		_, err := r.deps.Store.ObjectChecksum(ctx, r.medium, r.key, transport.SHA256)
		if err == nil {
			r.pass(StepVerification, fmt.Sprintf(
				"this medium requires the %s class and the endpoint produced a full-object %s digest, so an upload here can be verified without downloading it",
				placement.Attested, transport.SHA256))
			return
		}
		category, classified := transport.CategoryOf(err)
		if classified && category == transport.UnsupportedCapability {
			r.fail(StepVerification, err, fmt.Sprintf(
				"this medium declares upload_verification: attested, and this endpoint cannot produce a full-object %s digest, so that class can never be achieved here and every move to this medium will refuse rather than fall back to a weaker check. "+
					"Measured against the rclone this build embeds, no s3 endpoint can: the only digest it serves is the ETag, and an ETag is not a content hash. Declare readback instead",
				transport.SHA256))
			return
		}
		r.fail(StepVerification, err, fmt.Sprintf(
			"this medium declares upload_verification: attested, and the endpoint could not be asked for a full-object %s digest; this manager's log has what came back", transport.SHA256))

	default:
		r.fail(StepVerification, nil, fmt.Sprintf(
			"this medium resolves to the %q verification class, which this preflight has no way to prove. It refuses rather than reporting a class it did not run, because a weaker check wearing a stronger name is what deletes a local copy against an upload nobody verified",
			r.required))
	}
}

// deleted removes the probe and confirms it is gone.
//
// Confirming is the point. DeleteObject treats an already-absent object as
// success, which is right for its caller and useless as a proof, so this
// stats afterwards and requires a NotFound. A medium this manager can
// write to but cannot delete from is one where FR-30's prune runs every
// cycle and silently changes nothing, and the operator's bill is the only
// place that shows up.
func (r *run) deleted(ctx context.Context) {
	if !r.wrote {
		// Best effort anyway: a failed upload can still leave an object
		// behind. Nothing is reported about it, because this run has no
		// claim to make about a delete it only attempted defensively.
		_ = r.deps.Store.DeleteObject(ctx, r.medium, r.key)
		r.skip(StepDelete, "nothing was written, so there was nothing to delete")
		return
	}

	if err := r.deps.Store.DeleteObject(ctx, r.medium, r.key); err != nil {
		r.fail(StepDelete, err, fmt.Sprintf(
			"the object this preflight wrote to bucket %q could not be deleted, so it is still there and this manager could not roll its own probe back. A medium that cannot be deleted from is one where retention silently reclaims nothing",
			r.medium.Bucket))
		return
	}

	_, err := r.deps.Store.StatObject(ctx, r.medium, r.key)
	category, classified := transport.CategoryOf(err)
	switch {
	case err == nil:
		r.fail(StepDelete, nil, fmt.Sprintf(
			"the delete reported success and the object is still in bucket %q. Retention would report space reclaimed that was never reclaimed", r.medium.Bucket))
	case classified && category == transport.NotFound:
		r.pass(StepDelete, "the probe object was deleted, and the endpoint confirms it is gone")
	default:
		r.fail(StepDelete, err, fmt.Sprintf(
			"the delete reported success and the endpoint could not be asked to confirm it, so this preflight cannot say whether the probe object it wrote to bucket %q is still there", r.medium.Bucket))
	}
}

// probeKey builds the key this run's probe object lives at.
//
// Random rather than derived from a clock or a fixed name, and 16 bytes of
// it: two preflights running at once must not share an object, and a probe
// left behind by a run that was killed must never be mistaken for this
// one's, because the delete step's whole verdict is "the thing I wrote is
// gone".
func probeKey(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating a probe key: %w", err)
	}
	segments := make([]string, 0, 4)
	if prefix != "" {
		segments = append(segments, strings.Split(prefix, "/")...)
	}
	segments = append(segments, probePrefix, hex.EncodeToString(raw[:])+".probe")
	return strings.Join(segments, "/"), nil
}

// writeProbeFile stages the probe's bytes on local disk, because
// MediumStore.UploadFromLocal is addressed by a path rather than by a
// reader (see its own doc for why).
func writeProbeFile() (string, error) {
	dir, err := os.MkdirTemp("", "rclone-manager-preflight-*")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "probe")
	if err := os.WriteFile(path, probeBody, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}

// categoryName renders the FR-22 category a failure classified as, for the
// one machine-readable field a Check carries. An unclassified failure gets
// an empty string rather than "unclassified": a surface asking "which
// category was this" and getting a name back for an error nobody
// classified would be told something that was never decided.
func categoryName(err error) string {
	if err == nil {
		return ""
	}
	category, ok := transport.CategoryOf(err)
	if !ok {
		return ""
	}
	return category.String()
}
