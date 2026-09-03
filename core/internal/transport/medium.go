package transport

import (
	"context"
	"io"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// MediumType names a storage-medium backend. The set is closed, and it
// grows only by an FR-4 architecture decision, never by an import line
// (EPIC E, FR-28).
type MediumType string

const (
	// MediumTypeS3 is the one medium type an operator can configure. It is
	// spelled exactly as config.StorageMediumTypeS3 spells it, and a test
	// in this package pins the two together.
	MediumTypeS3 MediumType = "s3"

	// MediumTypeLocalDir is a medium backed by a directory on this
	// machine, and it exists so the MediumStore contract suite has an
	// in-tree backend to run against, the same way the Transport contract
	// suite already runs against the local backend before it ever sees a
	// real SFTP server.
	//
	// It is NOT configurable. core/internal/config's medium type set is
	// closed to s3 alone, so nothing an operator writes can produce a
	// Medium of this type; the only way to build one is in a test, by
	// hand. That is deliberate: "local" as a MEDIUM would be a second
	// answer to where local artifacts live, and config.MediumLocal (the
	// reserved id for the implicit local placement) is the first one.
	MediumTypeLocalDir MediumType = "local"
)

// MediumCredentials names exactly one place a medium's credentials come
// FROM. It mirrors Source's KeyFile/KeyEnv/KeyCommand trio field for field
// and for the same reason (EPIC E, FR-33): this project already decided
// how a secret is named, and none of the three carries secret material
// itself.
//
// There is no fourth field, and there never will be. A field a literal
// access key fits into is the only way a credential becomes a value this
// process holds straight out of configuration, and medium_test.go pins the
// field set so adding one is a test failure rather than a review miss.
type MediumCredentials struct {
	// File is a path to an AWS shared-credentials file. It is the
	// preferred source for the reason Source.KeyFile is: rclone opens the
	// file itself, so the secret never enters this process's memory, and a
	// secret this process never holds is a secret it cannot log.
	File string

	// Env names an environment variable holding the credentials.
	Env string

	// Command is an argv array run to produce the credentials on stdout.
	// Command[0] is the executable, invoked directly and never through a
	// shell, exactly like Source.KeyCommand.
	Command []string
}

// Medium is the manager-owned description of one configured destination an
// artifact's durable copy can live on.
//
// It is to MediumStore what Source is to Transport: everything the adapter
// needs to reach a place, in this repository's own vocabulary, with no
// rclone type anywhere in it. internal/app builds one from a
// config.StorageMedium; nothing below this line reads config.
//
// A Medium is safe to log. It holds no credential, only the reference to
// where one comes from, and medium_test.go proves the struct has nowhere
// for a value to hide.
type Medium struct {
	// ID is the medium id a retention tier names, and the id a placement
	// record carries. config.MediumLocal ("local") is reserved and never
	// appears here.
	ID string

	// Type names the backend. See MediumType.
	Type MediumType

	// Region is the provider region, passed through unexamined.
	Region string

	// Endpoint overrides the provider endpoint, for an S3-compatible
	// service. Empty means the provider's own endpoint for Region.
	Endpoint string

	// Bucket is the bucket objects are written into. For
	// MediumTypeLocalDir it is the directory that stands in for one.
	Bucket string

	// Prefix is the key namespace inside Bucket. Empty puts the key layout
	// at the root.
	Prefix string

	// StorageClass is the storage class objects are written with, spelled
	// as S3 spells it. Empty means the backend's own default.
	StorageClass string

	// Credentials names where this medium's credentials come from.
	Credentials MediumCredentials
}

// ObjectInfo is what a medium reports about one object it holds.
//
// Note what is absent: there is no ETag, no hash and no checksum field.
// FR-32 says an ETag is never a content hash (multipart and encrypted
// objects make it not one), and the most reliable way to keep a digest a
// medium volunteered out of a comparison against a recorded hash is to
// give a caller nowhere to read one from. Asking for an attestation is a
// deliberate act with its own method, ObjectChecksum, which either answers
// about the whole object or refuses.
type ObjectInfo struct {
	// Key is the object's key, relative to the medium's prefix.
	Key string

	// Size is the object's size in bytes.
	Size int64

	// ModTime is when the medium says the object was last written, in unix
	// seconds, or 0 when the backend reports none.
	//
	// It is upload time, never backup time, and FR-32 makes that a rule
	// rather than a caution: this value is NEVER admissible as a
	// producer timestamp, and nothing in retention may read it. It is here
	// for diagnostics and for reconciliation ordering, and for nothing
	// else.
	ModTime int64

	// StorageClass is the class the medium says this object is stored
	// with, or empty when it reports none.
	StorageClass string
}

// UploadOptions carries the per-upload choices a caller makes.
type UploadOptions struct {
	// StorageClass overrides the medium's own configured class for this
	// one object. Empty means the medium's class.
	StorageClass string
}

// UploadResult reports what an upload actually did.
type UploadResult struct {
	// Key is the key the object was written to, which is the key the
	// caller asked for: an upload never chooses its own destination.
	Key string

	// BytesUploaded is the size of the object as the destination reports
	// it after the write.
	BytesUploaded int64
}

// ChecksumAttestation is a medium's own statement about the digest of a
// whole object (FR-31's `attested` class).
//
// There is no field saying whether this covers the whole object, because
// there is no case in which it does not: a backend that can only produce a
// composite or part-wise digest (S3's own multipart ETag, for example)
// must refuse with an UnsupportedCapability error rather than return one
// of these. A boolean here would be a footgun, since the caller that
// forgets to read it deletes a local copy against a digest of something
// else.
type ChecksumAttestation struct {
	// Algorithm names what was computed.
	Algorithm HashAlgorithm

	// Value is the digest, lower-case hex.
	Value string
}

// MediumStore is the manager-owned boundary around a storage medium, and
// the only surface placement and retention code is allowed to depend on
// (EPIC E, FR-28). It sits beside Transport, and the FR-3 rules carry over
// verbatim: rclone types never appear in a signature here, and lifecycle
// code depends on this interface rather than on an adapter.
//
// # There is no Move, and that is the load-bearing decision
//
// A migration between mediums is UploadFromLocal, then verification, then
// an explicit delete of the source, composed in one auditable place by the
// move engine (#238). Offering Move as one primitive would push the
// ordering of those steps down into each implementation, where every
// adapter author picks it independently and one of those choices loses the
// only copy of a backup. artifactstore's package doc makes the same
// argument about the local seam at length; this is that argument applied
// to the boundary a second machine sits behind.
//
// # Restore is not here yet
//
// FR-28 sketches RestoreStatus and InitiateRestore on this interface. They
// belong to #241, which owns the archive storage classes that give them
// something to mean, and no fixture available in Phase 1 can exercise a
// Glacier restore, so landing them now would land two methods with no
// implementation and no test behind them. #241 adds them here.
type MediumStore interface {
	// StatObject reports what the medium holds at key. A key the medium
	// does not hold is a NotFound-classified error, never a zero
	// ObjectInfo: "the medium answered, and the object is not there" and
	// "the medium could not be reached to ask" must never collapse into
	// one another, because a mover that confuses them deletes a local copy
	// on the strength of a network failure.
	StatObject(ctx context.Context, medium Medium, key string) (ObjectInfo, error)

	// UploadFromLocal writes the file at localPath to key.
	//
	// It is addressed by a local PATH rather than by an io.Reader on
	// purpose: the local file is what exists, rclone can stream it itself
	// with its own retry and multipart handling, and a reader would put
	// this process in the middle of every byte for no gain.
	//
	// # It CONVERGES on an occupied key, it does not refuse one
	//
	// This is the one obligation on this interface that could
	// defensibly have gone the other way, so it is stated rather than
	// left to an implementation to pick. artifactstore.Store.Put refuses
	// an occupied path, and the argument for doing the same here is real:
	// overwriting an artifact's only remaining copy is not recoverable.
	//
	// It goes the other way because of what a MOVE is. A move interrupted
	// between "the upload started" and "the journal recorded that it
	// finished" leaves the engine unable to tell whether the object is
	// there, whole, or half-written, and the recovery it wants is to run
	// the upload again. Under a refusal that restart is a Conflict the
	// engine has to resolve by statting, comparing and possibly DELETING
	// from the medium before retrying, which is a delete on a recovery
	// path: precisely the code FR-3 split copy, verify and delete apart to
	// avoid having.
	//
	// What makes converging safe is the other two rules on this
	// interface. The key is deterministic and derived from the artifact
	// (see MediumKey), so the only thing that can be sitting at it is an
	// earlier attempt at the SAME artifact, never a stranger; and FR-31's
	// verification runs after the upload and before anything deletes a
	// local copy, so a converged upload is still proved before it is
	// trusted. Two artifacts that would collide on one key collide on
	// their basename inside one backup set, which internal/discovery
	// already refuses by name, upstream of here.
	//
	// The contract suite pins this: two uploads to one key leave exactly
	// one object holding the second upload's bytes
	// (contract.RunMedium's upload_converges_on_the_same_key).
	//
	// What it does NOT make safe is two genuinely CONCURRENT uploads to
	// one key, which need a conditional put rclone v1.75 does not expose.
	// The move engine's single-writer journal is what excludes those, and
	// that exclusion lives there rather than here.
	UploadFromLocal(ctx context.Context, medium Medium, localPath, key string, opts UploadOptions) (UploadResult, error)

	// OpenObject reads the object's bytes back, for read-back
	// verification and for restore-to-local. The caller closes the reader.
	OpenObject(ctx context.Context, medium Medium, key string) (io.ReadCloser, error)

	// ObjectChecksum asks the medium for its own stored digest of the
	// whole object at key.
	//
	// Where the endpoint, or the embedded rclone, cannot produce a
	// full-object digest for alg, this returns an UnsupportedCapability
	// error and NEVER a weaker answer wearing this method's name (FR-13,
	// restated by FR-31). Silently degrading here is how a local copy gets
	// deleted against an upload nobody checked.
	//
	// # On rclone v1.75.0 an s3 medium can never answer this
	//
	// Read this before building on FR-31's `attested` class, because it
	// does not exist on the only medium type there is.
	//
	// backend/s3's Fs.Hashes() returns exactly hash.Set(hash.MD5)
	// (s3.go:3294) and Object.Hash refuses every other algorithm outright
	// (s3.go:4036). The MD5 it does serve comes from setMD5FromEtag, so
	// the value IS the ETag, and FR-32 says an ETag is never a content
	// hash. rclone's own X-Amz-Meta-Md5chksum is no better: it is metadata
	// rclone WROTE, so believing it proves this product's earlier upload
	// said something, not that the stored bytes are those bytes.
	//
	// So ObjectChecksum against an s3 medium is an UnsupportedCapability
	// refusal, every time, on this rclone. That is FR-13 working rather
	// than a gap, and it has consequences the phase-2 lanes have to build
	// on rather than discover:
	//
	//   - The move engine (#238) must FAIL LOUDLY when a medium is
	//     configured with upload_verification: attested. It must not fall
	//     back to `content` or `existence`, because a surface that reports
	//     a weaker class than the one configured is the exact lie FR-31's
	//     ladder exists to prevent.
	//   - Retention (#239) and the archive classes (#241) must not assume
	//     an attestation is available for an s3 placement.
	//   - The capability is queried LIVE, not hard-coded, so a future
	//     rclone that surfaces x-amz-checksum-sha256 makes this start
	//     working with no edit to the adapter. What has to change
	//     deliberately at that point is the fixture's AttestsSHA256, and
	//     core/tests/miniointegration's TestMinioAttestationIsRefused is
	//     what will notice.
	ObjectChecksum(ctx context.Context, medium Medium, key string, alg HashAlgorithm) (ChecksumAttestation, error)

	// DeleteObject removes exactly the object at key.
	//
	// It deletes the one object key names and nothing else: no prefix
	// delete, no recursion, no siblings, and no indirection followed to a
	// different object. It performs no safety proof of its own, for
	// artifactstore.Store.Remove's reason: the proof that these particular
	// bytes are safe to delete belongs to the caller, which re-derives it
	// immediately before calling.
	//
	// Deleting something already absent is not an error: the caller's
	// intent, that these bytes not be on this medium, is satisfied.
	DeleteObject(ctx context.Context, medium Medium, key string) error

	// ListObjects enumerates what the medium holds under prefix, for
	// catalog rebuild and for reconciliation.
	//
	// artifactstore deliberately has no List, and this interface has one,
	// which is not a contradiction: that seam serves retention, which has
	// a catalog and must never grow a second, disagreeing inventory, while
	// this one has to serve a rebuild of exactly that catalog after local
	// state is lost, and a rebuild has nothing else to read. FR-32 is what
	// keeps that safe: everything this returns is an untrusted proposal.
	ListObjects(ctx context.Context, medium Medium, prefix string) ([]ObjectInfo, error)
}

// MediumKey builds the key an artifact's object lives at inside a medium:
// <prefix>/<source>/<set>/<artifact-name>, joined with "/" (FR-28).
//
// The layout is deterministic and carries no timestamp and no random
// component, which is the property that makes re-running an interrupted
// upload converge on the same object instead of leaving a second copy
// behind. It mirrors FR-7's backup-set isolation, so two sets can share a
// bucket without sharing a namespace.
//
// It refuses rather than guesses. A key is not only ever a key: restoring
// an artifact writes it to a local path derived from the key, so a segment
// that is empty, or that is "." or "..", is refused here instead of being
// joined into something a later join has to defend against.
func MediumKey(prefix string, artifact model.ArtifactID) (string, error) {
	if artifact.Set.IsZero() {
		return "", &Error{Category: Configuration, Op: "medium_key", Cause: errNoBackupSet}
	}
	segments := make([]string, 0, 4)
	if prefix != "" {
		segments = append(segments, strings.Split(prefix, "/")...)
	}
	segments = append(segments, artifact.Set.Source, artifact.Set.Set, artifact.Name)
	for _, s := range segments {
		if err := validKeySegment(s); err != nil {
			return "", &Error{Category: Configuration, Op: "medium_key", Cause: err}
		}
	}
	return strings.Join(segments, "/"), nil
}

// validKeySegment refuses everything that would make a key ambiguous or
// make the local path a restore derives from it escape.
func validKeySegment(s string) error {
	switch {
	case s == "":
		return errEmptyKeySegment
	case s == "." || s == "..":
		return errTraversingKeySegment
	case strings.Contains(s, "/"):
		// Only a prefix is allowed to be multi-segment, and it was split
		// before it got here. A source, a set or an artifact name that
		// carries a separator would silently become two segments, which is
		// how "../escape" as an artifact name walks out of the namespace.
		return errSeparatorInKeySegment
	case strings.ContainsAny(s, "\\\x00\n\r"):
		return errUnprintableKeySegment
	case strings.TrimSpace(s) != s:
		// Leading and trailing whitespace is legal in an S3 key and
		// invisible in every listing that would show it, so " pg" and
		// "pg" are two different objects that read as one. A restore
		// picking the wrong one of those is not a failure anybody
		// diagnoses from a listing.
		return errPaddedKeySegment
	}
	return nil
}

// The four refusals MediumKey can make. They are values rather than
// formatted strings so a caller (and a test) can recognise which rule
// fired without reading prose, the same discipline errors.go applies to
// everything else in this package.
var (
	errNoBackupSet           = mediumKeyError("an artifact id with no backup set cannot be addressed on a medium")
	errEmptyKeySegment       = mediumKeyError("a key segment is empty, which would produce two spellings of the same object")
	errTraversingKeySegment  = mediumKeyError(`a key segment is "." or "..", and a restore derives a local path from this key`)
	errSeparatorInKeySegment = mediumKeyError("a key segment carries a \"/\", so it would silently become two segments")
	errUnprintableKeySegment = mediumKeyError("a key segment carries a backslash, a NUL or a newline")
	errPaddedKeySegment      = mediumKeyError("a key segment has leading or trailing whitespace, which is legal in S3 and invisible in every listing that would show it")
)

// mediumKeyError is a tiny named error type, rather than errors.New, so
// the four values above cannot be confused with any other error in this
// package by an equality check that meant to compare something else.
type mediumKeyError string

func (e mediumKeyError) Error() string { return string(e) }
