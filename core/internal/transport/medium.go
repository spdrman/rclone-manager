// Package transport: this file is EPIC E's FR-28 boundary, the second
// manager-owned interface in this package and the one an alternative
// storage medium is reached through.
//
// It sits beside Transport rather than extending it because the two answer
// different questions. Transport is where backup artifacts come FROM: a
// source this product does not own, listed and read and eventually deleted
// under FR-15's discipline. A medium is where an artifact's durable copy
// GOES: a destination this product does own, writes to, and is responsible
// for. Collapsing them would give lifecycle code one interface whose
// methods are safe in one direction and catastrophic in the other.
//
// # No Move, and this is where that decision is enforced twice
//
// internal/artifactstore's package doc argues at length why a move is put,
// confirm, then remove, composed in one auditable place rather than pushed
// down into each backend. The same argument applies here and for the same
// reason, so the same absence is enforced here too, by its own test. FR-3's
// original wording ("Copy, verify, commit and delete are four separately
// owned steps") and FR-30's move engine are the two ends of that: the
// engine (#238) composes UploadFromLocal, a verification, and an explicit
// DeleteObject, and nothing on this interface lets it skip the middle one.
//
// # Nothing here names an rclone type
//
// That is FR-3, and it is what makes an rclone upgrade a change to
// transport/rclone rather than a change to everything. A test reflects over
// this interface's signatures and fails on any type whose name mentions
// rclone, which catches the accidental version of the mistake; the
// deliberate version needs someone to import rclone into this package,
// which the repository-structure rules already refuse.
package transport

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// MediumType names a storage-medium backend.
//
// The set is closed and mirrors config.StorageMediumTypeS3 exactly, on
// purpose: a type this package accepted that config cannot spell would be a
// medium nobody can configure, and a type config accepts that this package
// does not know would be a config that validates and then cannot be served.
// config does not import transport (it sits under everything), so the
// agreement between the two spellings is pinned by a test rather than by a
// shared constant, the same way config.MediumLocal and
// artifactstore.KindLocal are already pinned to each other.
type MediumType string

// MediumTypeS3 is the one medium type this boundary implements. FR-28 is
// the FR-4 architecture decision that admits rclone's s3 backend; a second
// type is its own decision, and the MediumStore interface is the extension
// point it would arrive through.
const MediumTypeS3 MediumType = "s3"

// MediumCredentials names exactly one way to obtain a medium's
// credentials: File, Env or Command (FR-33).
//
// It is transport's own copy of config.MediumCredentials, which is itself a
// field-for-field copy of config.Key, and the copying is the point rather
// than duplication nobody got around to removing. transport.Source already
// carries KeyFile/KeyEnv/KeyCommand for the identical reason: config
// describes what an operator may write, transport describes what an adapter
// is handed, and the two are allowed to diverge (a resolved value, a
// defaulted one) without either package importing the other.
//
// # There is no field for a literal credential, and that is the enforcement
//
// Exactly as config.MediumCredentials' doc says about the schema half:
// search this type for an access key and you will not find one. On the
// config side the mechanism is Load's KnownFields(true), which makes an
// inline secret a parse error. On this side there is no decoder to lean on,
// so the mechanism is that these three fields are exactly the three
// REFERENCES and a fourth field holding a VALUE would fail
// TestMediumCredentialsNameOnlyWhereASecretComesFrom.
//
// What File, Env and Command produce, and how each is resolved, is
// internal/transport/rclone's business: see credentials.go there, which
// applies keysource.go's discipline unchanged.
type MediumCredentials struct {
	// File is a path to an AWS shared-credentials format file. It is the
	// preferred source for the reason Source.KeyFile is preferred, and the
	// reason is stronger here: rclone opens the file itself, so the secret
	// never enters this process's memory at all, and a secret this process
	// never holds is a secret it cannot log.
	File string

	// Env names an environment variable holding the same
	// shared-credentials text the file would have held.
	Env string

	// Command is an argv array (Command[0] is the executable, the rest are
	// its literal arguments) run to produce that same text on stdout. It
	// is never a shell string: nothing in this program ever hands it to a
	// shell.
	Command []string
}

// Medium is one configured non-local destination, as an adapter is handed
// it. It is transport's counterpart to config.StorageMedium, the way Source
// is transport's counterpart to config.Remote.
//
// # This struct is safe to log, because it holds nothing to leak
//
// FR-33 lists what may never contain a credential, and "any log line at any
// level" is on it. The defence is structural rather than disciplinary:
// every field here is either a place (a bucket, a region, an endpoint) or
// the NAME of somewhere a secret lives (a path, a variable name, an argv).
// None of them is a secret, so a debugging operator's reflexive %+v prints
// something useful and nothing dangerous.
//
// TestMediumRendersNoCredentialValue is what keeps that true: it fails on
// any field whose name is the shape a literal credential would fit into.
// The references themselves are echoed on purpose, since a path and a
// variable name are not the secret and hiding them would make a
// misconfigured medium undebuggable.
type Medium struct {
	// ID is the medium's configured id, which is also what a placement
	// record names (FR-29) and what an operator sees when asking where an
	// artifact lives.
	ID string

	// Type is the backend. Only MediumTypeS3 today.
	Type MediumType

	// Region is the provider region, passed to the backend unexamined.
	// config declines to validate it (the legal set belongs to the
	// provider and changes without this product being rebuilt) and so
	// does this package.
	Region string

	// Endpoint overrides the provider endpoint for an S3-compatible
	// service. Empty means the provider's own endpoint for Region.
	//
	// Whether it is set is load-bearing beyond addressing: see
	// internal/transport/rclone's s3.go, which reads an empty Endpoint as
	// AWS and a set one as a generic S3-compatible service, because there
	// is no provider field for an operator to say so with.
	Endpoint string

	// Bucket is the bucket objects are written into.
	Bucket string

	// Prefix is the key namespace inside Bucket. Empty puts the key
	// layout at the root of the bucket. See MediumKey.
	Prefix string

	// StorageClass is the S3 storage class objects are written with,
	// already resolved through config.StorageMedium.EffectiveStorageClass
	// so this field is never empty by the time an adapter sees it.
	StorageClass string

	// Credentials names where this medium's credentials come from.
	Credentials MediumCredentials
}

// ObjectInfo is what a medium reports about one object.
//
// # Why there is no ETag here, and never will be
//
// FR-32 says an ETag is never a content hash, because multipart uploads and
// server-side encryption both make it not one. The safest place to keep an
// ETag out of a comparison against a recorded hash is out of the type: a
// field nobody can read is a field nobody can compare. A full-object
// checksum is a different thing entirely, it costs a deliberate call, and
// ObjectChecksum is where it is asked for on purpose.
//
// TestObjectInfoCarriesNoETag pins that absence.
type ObjectInfo struct {
	// Key is the object's key relative to the medium's bucket, including
	// the medium's prefix: exactly what MediumKey produced.
	Key string

	// Size is the object's size in bytes as the medium reports it.
	Size int64

	// ModTime is the medium's own last-modified time, in unix seconds, or
	// 0 when the medium does not report one.
	//
	// FR-32: this is UPLOAD time, not backup time, and it is never
	// admissible as a producer timestamp. It is here for diagnostics and
	// for reconciliation, never for the retention calendar. A move copies
	// journal truth and never re-derives a timestamp from a destination.
	ModTime int64

	// StorageClass is the class the object is actually stored in, which
	// is not necessarily the class the medium asked for: a lifecycle
	// policy on the bucket can move an object to an archive class behind
	// this product's back, and FR-34 says the surfaces tell the truth
	// about that rather than repeating what was configured. Empty when
	// the medium does not report one.
	StorageClass string
}

// UploadResult reports what an upload actually did. It is deliberately not
// TransferResult: that type reports a copy FROM a source, and reusing it
// would make the two directions indistinguishable at a call site.
type UploadResult struct {
	// BytesUploaded is what the medium acknowledged, read back from the
	// stored object rather than counted on the way out, so a truncated
	// upload that the endpoint accepted is visible here.
	BytesUploaded int64

	// StorageClass is the class the object landed in, reported by the
	// medium.
	StorageClass string
}

// ChecksumAttestation is a medium's own claim about an object's
// full-object content hash, for FR-31's `attested` verification class.
//
// It is a claim, not a proof, and the type name says so. The whole class
// asks the destination to grade its own work, which is why FR-31 makes
// read-back the default and makes `attested` a per-medium opt-in that
// names its trust assumption out loud. What this type is for is carrying
// the claim to the one place allowed to decide whether to believe it.
//
// It is never built from an ETag. See ObjectInfo.
type ChecksumAttestation struct {
	// Algorithm is what the value is a digest under. A caller compares it
	// to the algorithm its recorded hash was computed with and refuses a
	// mismatch rather than comparing digests across algorithms.
	Algorithm HashAlgorithm

	// Value is the digest, lower-case hex, as the medium reports it.
	Value string
}

// MediumStore is the only medium surface lifecycle, retention and the move
// engine are allowed to depend on (FR-28).
//
// # Why every method takes a Medium, when artifactstore.Store takes none
//
// The two seams are at different altitudes and the difference is
// deliberate. artifactstore.Store is per-backup-set: one value serves one
// set, its configuration arrives at construction, and no method takes any.
// MediumStore is the layer below that, and it mirrors Transport instead:
// one adapter value serves every configured medium, and the medium travels
// with the call, exactly as a Source travels with every Transport call.
//
// That keeps the adapter stateless and keeps "which medium" auditable at
// every call site, which matters most for DeleteObject: a delete whose
// destination is implicit in a value built somewhere else is a delete
// nobody reviewing the call can check. The per-set store shape lives one
// layer up, in artifactstore, where a Medium store binds one medium and one
// backup set together and answers Locator for them.
//
// # There is no Move
//
// See this file's package comment, and internal/artifactstore's, which
// makes the argument in full. TestMediumStoreOffersNoMoveMethod enforces
// it by method NAME, so a signature nobody predicted does not walk past.
//
// # There is no restore, yet
//
// FR-28 sketches RestoreStatus and InitiateRestore here. They belong to
// #241, which owns the archive storage classes that give them meaning.
// Landing them now would land two methods that every implementation has to
// stub, and a stubbed capability reads as satisfied, which is the specific
// hazard artifactstore's own doc names.
type MediumStore interface {
	// StatObject reports what the medium holds at key.
	//
	// It answers about the object, never about the bytes: a successful
	// StatObject says an object of that size exists under that key, which
	// is FR-31's `existence` class and explicitly not enough to delete a
	// source copy.
	//
	// An object the medium does not hold is an *Error with category
	// NotFound, never a zero ObjectInfo: "the medium answered and the
	// object is not there" and "the medium could not be reached to ask"
	// must never collapse into one another, for the reason
	// artifactstore.ErrNotPresent's own doc gives.
	StatObject(ctx context.Context, medium Medium, key string) (ObjectInfo, error)

	// UploadFromLocal puts the file at localPath under key.
	//
	// It takes a path rather than an io.Reader because the medium may need
	// to know the size before it starts (multipart threshold, content
	// length) and because a reader that fails halfway is a class of
	// partial upload a file does not have. The caller's file is opened
	// read-only and never modified.
	//
	// Three obligations, the same three artifactstore.Store.Put carries,
	// and an implementation's doc must say how it discharges each:
	//
	//   - ATOMIC. A StatObject must never observe a partial object under
	//     key.
	//   - DURABLE. Once a StatObject would succeed, that survives the
	//     endpoint restarting, not merely this process exiting.
	//   - REFUSES AN OCCUPIED KEY. An upload onto a key that already holds
	//     something returns an *Error with category Conflict rather than
	//     replacing it. Overwriting an artifact's only remaining copy is
	//     not recoverable, and a destination that already holds something
	//     different is a case a person decides.
	//
	// The key layout is deterministic (see MediumKey) precisely so that
	// re-running an interrupted upload targets the same key rather than
	// making a second copy, which is what makes the refusal above a
	// resumable case rather than a dead end.
	UploadFromLocal(ctx context.Context, medium Medium, localPath, key string) (UploadResult, error)

	// OpenObject reads the object's bytes, for FR-31's `content` class
	// read-back verification and for restore-to-local. The caller closes
	// the reader.
	//
	// This is the expensive one: it is a full download, it costs egress,
	// and FR-31 is explicit that nothing which costs egress may happen
	// silently on a schedule.
	OpenObject(ctx context.Context, medium Medium, key string) (io.ReadCloser, error)

	// ObjectChecksum asks the medium for its own full-object checksum
	// under alg, for FR-31's `attested` class.
	//
	// Where the medium or the embedded rclone cannot produce a
	// FULL-OBJECT checksum under alg, this returns an *Error with
	// category UnsupportedCapability. It never falls back to something
	// weaker and never returns an ETag dressed as a digest: FR-13's
	// "explicit capability result rather than silently weakening
	// configured verification" applies verbatim, and FR-32 forecloses the
	// ETag specifically.
	ObjectChecksum(ctx context.Context, medium Medium, key string, alg HashAlgorithm) (ChecksumAttestation, error)

	// DeleteObject deletes exactly the object at key.
	//
	// It performs no safety proof of its own, exactly as
	// artifactstore.Store.Remove performs none: the proof that these
	// particular bytes are safe to delete belongs to the caller, and for
	// a medium that is FR-30's prune half (re-check the object's identity
	// against the placement record, refuse on mismatch) composed in
	// internal/retention. An adapter author must not add a check here,
	// because a check that lives in two places is a check reviewers stop
	// reading in either.
	//
	// What an implementation IS obliged to do is narrow and absolute:
	// delete the one object key names and nothing else, never widen the
	// target (no prefix delete, no recursion, no versions), and treat an
	// already-absent object as success, since the caller's intent is
	// already true.
	DeleteObject(ctx context.Context, medium Medium, key string) error

	// ListObjects enumerates objects under prefix, for catalog rebuild and
	// for reconciliation after an interrupted move.
	//
	// artifactstore deliberately has no List, and that is not in tension
	// with this. The argument there is that the CATALOG, not a scan,
	// answers "which artifacts exist and where", and this method does not
	// answer that question: FR-32 says everything a medium reports is
	// untrusted input, so what comes back here is a set of PROPOSALS a
	// rebuild reports and a reconciler cross-checks against the journal,
	// never an inventory anything trusts on its own.
	//
	// prefix is a full key prefix, already including the medium's own
	// Prefix. A caller enumerating a backup set passes what MediumKey
	// would produce for it minus the artifact name.
	ListObjects(ctx context.Context, medium Medium, prefix string) ([]ObjectInfo, error)
}

// MediumKey is FR-28's deterministic key layout:
// <prefix>/<source>/<set>/<artifact-name>, joined with "/".
//
// No timestamp and no random component, which is the whole point: an
// interrupted upload re-run targets the same key rather than leaving a
// second copy behind, so a resumed move finds either nothing or its own
// previous attempt, and never a stranger.
//
// The <source>/<set> pair mirrors FR-7's backup-set isolation exactly, so
// one bucket can hold several backup sets without any of them being able to
// address another's keys through this function.
//
// # Why this refuses rather than sanitises
//
// A key is not only ever a key. Restoring an artifact writes it to a local
// path derived from the key, so a segment that is empty, or "..", or that
// carries a leading slash, is a local path problem wearing S3 clothes.
// Every one of those is refused here, and the refusal returns an empty
// string alongside its error so that a caller which ignores the error
// cannot accidentally use a half-built key. Sanitising instead would mean
// two different artifacts could silently map onto one key, which is the
// failure this product least wants to discover from a restore.
//
// prefix is validated here as well as in config, deliberately. config's
// Validate refuses a bad prefix for a config built through that package;
// this is the backstop for anything that builds a Medium directly, tests
// included, and it is the same "both ends check" ssh.go's sftpConfig
// applies to a Source's key sources.
func MediumKey(prefix string, artifact model.ArtifactID) (string, error) {
	if artifact.Set.IsZero() {
		return "", fmt.Errorf("transport: artifact %q has no backup set, so no medium key can be built for it", artifact.Name)
	}
	segments := []string{artifact.Set.Source, artifact.Set.Set, artifact.Name}
	if prefix != "" {
		if strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
			return "", fmt.Errorf("transport: medium prefix %q has a leading or trailing %q, which produces an empty key segment", prefix, "/")
		}
		segments = append(strings.Split(prefix, "/"), segments...)
	}
	for _, segment := range segments {
		if err := validKeySegment(segment); err != nil {
			return "", fmt.Errorf("transport: building a medium key for %s: %w", artifact, err)
		}
	}
	// path.Join, never filepath.Join: a key separator is "/" on every
	// platform, and filepath.Join would spell it "\" on Windows, which is
	// a different object under a name nothing else in this product would
	// ever compute.
	return path.Join(segments...), nil
}

// validKeySegment is MediumKey's per-segment rule, applied to the prefix's
// own segments as well as to the artifact's, because a bad segment is
// equally bad wherever it came from.
func validKeySegment(segment string) error {
	switch {
	case segment == "":
		return fmt.Errorf("a key segment is empty")
	case segment == "." || segment == "..":
		return fmt.Errorf("key segment %q is a directory reference, and a key is not only ever a key: a restore turns it back into a local path", segment)
	case strings.ContainsAny(segment, "/\\\x00\n\r"):
		return fmt.Errorf("key segment %q contains a separator or a control character", segment)
	case strings.TrimSpace(segment) != segment:
		return fmt.Errorf("key segment %q has leading or trailing whitespace, which is legal in S3 and invisible in every listing that would show it", segment)
	}
	return nil
}
