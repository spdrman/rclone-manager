// Package rclone: this file implements transport.MediumStore over the
// embedded rclone s3 backend (FR-28).
//
// Adapter implements both Transport and MediumStore, which is one value
// serving both directions rather than two objects to wire up. That is the
// right shape because the two interfaces share exactly the thing worth
// sharing: the rule that this package is the only rclone importer, and the
// classification in errors.go that turns whatever rclone says into a
// manager-owned Category. They share no state, because Adapter has none.
package rclone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

var _ transport.MediumStore = (*Adapter)(nil)

// ErrObjectAlreadyPresent is returned by UploadFromLocal when the medium
// already holds something under the key it was asked to write, and it
// refused rather than replacing it.
//
// It is a sentinel for the reason ErrUnsupportedHash is one: errors.go
// classifies by identity rather than by matching this package's own error
// text, and a value defined here costs nothing and cannot drift out of
// sync with a message somebody rewords.
//
// It is this package's counterpart to artifactstore.ErrAlreadyPresent, and
// the two mean the same thing on purpose: the resumable case. A previous
// run got as far as putting the object and did not get as far as recording
// that it had, so the next run finds its own earlier work. What follows is
// confirm-then-continue, never replace.
var ErrObjectAlreadyPresent = errors.New("rclone: the medium already holds an object at this key")

// fsForMedium builds an rclone Fs rooted at the medium's BUCKET, with no
// on-disk rclone config file consulted, exactly as fsFor does for a Source.
//
// Rooted at the bucket and not at the bucket plus prefix, deliberately.
// transport.MediumKey already produces a whole key including the medium's
// prefix, and that function is the single place the layout is decided
// (FR-28). An Fs rooted at bucket/prefix would mean the prefix was applied
// twice for anyone who passed a MediumKey result, or once by two different
// pieces of code for anyone who did not, and "which of these two joins is
// the real key layout" is not a question worth having.
func (a *Adapter) fsForMedium(ctx context.Context, medium transport.Medium) (fs.Fs, error) {
	info, err := fs.Find(string(medium.Type))
	if err != nil {
		return nil, fmt.Errorf("medium %q: backend %q is not registered in this binary: %w", medium.ID, medium.Type, err)
	}
	var cfg configmap.Simple
	switch medium.Type {
	case transport.MediumTypeS3:
		cfg, err = s3Config(medium)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("medium %q: this adapter has no configuration for type %q", medium.ID, medium.Type)
	}

	// The Fs name is what rclone puts in its own log lines and error
	// messages. "medium:<id>" makes those legible without carrying
	// anything sensitive: an id is what config.StorageMedium.ID already
	// is, and a placement record already names it.
	f, err := info.NewFs(ctx, "medium:"+medium.ID, medium.Bucket, cfg)
	if err != nil {
		return nil, fmt.Errorf("medium %q: %w", medium.ID, err)
	}
	return f, nil
}

// objectInfo turns an rclone object into the manager-owned shape. It reads
// the storage class through rclone's own fs.GetTierer, which the s3 backend
// implements, and leaves it empty for a backend that does not: reporting a
// class this product assumed rather than one the medium stated would be
// exactly the FR-34 dishonesty that section exists to forbid.
func objectInfo(ctx context.Context, o fs.Object) transport.ObjectInfo {
	info := transport.ObjectInfo{
		Key:     o.Remote(),
		Size:    o.Size(),
		ModTime: o.ModTime(ctx).Unix(),
	}
	if tierer, ok := o.(fs.GetTierer); ok {
		info.StorageClass = tierer.GetTier()
	}
	return info
}

// errBucketAbsent is what confirmBucket reports when the medium's bucket
// does not exist. It is a Configuration failure rather than a NotFound one
// (FR-28), and it exists as a sentinel so this file states the fact once.
var errBucketAbsent = errors.New("rclone: the medium's bucket does not exist")

// confirmBucket answers the question rclone's own error handling throws
// away: is this key absent, or is the whole BUCKET absent?
//
// # Why this is needed at all
//
// rclone's s3 backend translates a 404 into its own filesystem-shaped
// sentinels before this adapter ever sees it (backend/s3/s3.go turns a
// 404 from HeadObject into fs.ErrorObjectNotFound and a 404 from
// ListObjects into fs.ErrorDirNotFound), and in doing so it discards the
// S3 error code, which is the only thing that separates NoSuchKey from
// NoSuchBucket. Those are the same status and completely different
// problems: one is a fact about a single artifact, the other is a typo
// somebody has to go and fix.
//
// Measured against a real MinIO, before this existed:
//
//   - DeleteObject against a nonexistent bucket returned nil. The delete
//     reported SUCCESS, because NewObject said not-found and DeleteObject
//     correctly treats an already-absent object as success. Under FR-30's
//     medium-aware prune that would mark every placement GONE on a medium
//     whose bucket name had been mistyped in a config edit.
//   - StatObject and OpenObject against a nonexistent bucket both reported
//     NotFound, which a reconciler reads as "the medium lost the artifact".
//   - ListObjects against a nonexistent bucket returned an empty listing
//     and no error at all.
//
// # Why a list is a sound probe
//
// Because the two cases are distinguishable at THIS layer even though they
// are not distinguishable at the object layer, which the same measurement
// showed: walk over a prefix with nothing under it in a real bucket comes
// back with a nil error and no entries, and walk over any prefix in a
// missing bucket comes back with fs.ErrorDirNotFound. That holds because a
// ListObjectsV2 against an existing bucket answers 200 with no contents
// however empty the prefix is, and only a missing bucket makes it 404,
// which is the single case rclone's list path turns into ErrorDirNotFound.
//
// # What it costs
//
// One extra list, only on the path that was ABOUT to report not-found, so
// never on a successful stat, read or delete. UploadFromLocal deliberately
// does not call it: its own pre-flight stat returns not-found on every
// healthy upload, so probing there would add a round trip to the hot path
// to answer a question PutObject answers by itself, with the real
// NoSuchBucket code intact.
func (a *Adapter) confirmBucket(ctx context.Context, f fs.Fs, medium transport.Medium) error {
	if _, err := f.List(ctx, ""); err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return fmt.Errorf("%w: medium %q names bucket %q, and the endpoint does not have it", errBucketAbsent, medium.ID, medium.Bucket)
		}
		return err
	}
	return nil
}

// StatObject reports what the medium holds at key, or a NotFound error.
//
// A not-found is confirmed against the bucket before it is reported, so
// "this artifact is not on the medium" and "this medium's bucket does not
// exist" never reach a caller as the same answer. See confirmBucket.
func (a *Adapter) StatObject(ctx context.Context, medium transport.Medium, key string) (transport.ObjectInfo, error) {
	f, err := a.fsForMedium(ctx, medium)
	if err != nil {
		return transport.ObjectInfo{}, Wrap("stat_object", err)
	}
	o, err := f.NewObject(ctx, key)
	if err != nil {
		if isObjectAbsent(err) {
			if berr := a.confirmBucket(ctx, f, medium); berr != nil {
				return transport.ObjectInfo{}, Wrap("stat_object", berr)
			}
		}
		return transport.ObjectInfo{}, Wrap("stat_object", err)
	}
	return objectInfo(ctx, o), nil
}

// isObjectAbsent reports whether err is rclone's way of saying "there is
// nothing here", in either of the two shapes its s3 backend produces for a
// 404. It is a helper rather than an inline pair of checks because getting
// only one of the two is a silent half-fix.
func isObjectAbsent(err error) bool {
	return errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound)
}

// UploadFromLocal puts the file at localPath under key.
//
// # How this discharges Store.Put's three obligations
//
// transport.MediumStore.UploadFromLocal states the same three obligations
// artifactstore.Store.Put states, and Local.Put discharges them with a temp
// file, an fsync, a hard link and a directory fsync. S3's answers are
// different in kind, and two of the three are not this code's work at all,
// which is worth saying plainly rather than letting the doc imply effort
// where there is none.
//
// ATOMIC is the protocol's, not this implementation's. An S3 PUT makes an
// object visible only on success, and a multipart upload makes it visible
// only on CompleteMultipartUpload; there is no window in which a GET or a
// HEAD returns a partially written object under the key. So the obligation
// here is negative: do not defeat it. This code writes through
// operations.Copy to the final key and never to some other key it then
// renames (S3 has no rename, only a server-side copy plus a delete, which
// would reintroduce exactly the multi-step failure window the single PUT
// does not have).
//
// DURABLE is the endpoint's. Local.Put has to fsync because a POSIX
// filesystem makes no promise at all until it is asked to; S3 makes the
// promise as part of the response, and has offered read-after-write
// consistency for new objects since December 2020. What this product does
// NOT do is believe that promise on its own: FR-31 makes read-back the
// default verification precisely because "the endpoint said it stored it"
// is a claim by the party being checked, and the reward for believing a
// false one is a deleted local copy. So the durability argument here is
// "the protocol says so, and the product verifies anyway", which is the
// only honest version of it.
//
// REFUSES AN OCCUPIED KEY is the one the protocol does not give, and
// therefore the only one this function actually implements. An S3 PUT
// overwrites silently. So this stats the key first and refuses with
// ErrObjectAlreadyPresent (Category Conflict) if anything is there.
//
// That is a check-then-act, and it is worth being exact about what it
// closes and what it does not. It closes the case that actually happens: a
// move or an upload re-run after an interruption, finding its own previous
// attempt's object under the deterministic key MediumKey produced, which is
// the resumable case artifactstore's doc calls self-correcting. It does NOT
// close two genuinely concurrent uploads to one key, because between the
// stat and the put there is a window. Closing that properly needs a
// conditional put (If-None-Match: *), which both AWS and MinIO now support
// and which rclone v1.75's Put path does not expose, so it is not reachable
// from behind this boundary today. What excludes concurrency instead is
// upstream: the move engine (#238) journals one move per artifact and is
// single-writer per key by construction. If that ever stops being true, a
// conditional put is the fix and it belongs in the issue that makes it
// stop being true.
func (a *Adapter) UploadFromLocal(ctx context.Context, medium transport.Medium, localPath, key string) (transport.UploadResult, error) {
	local, err := os.Stat(localPath)
	if err != nil {
		return transport.UploadResult{}, Wrap("upload_from_local", err)
	}
	if !local.Mode().IsRegular() {
		return transport.UploadResult{}, Wrap("upload_from_local", fmt.Errorf("%s is not a regular file (mode %s)", localPath, local.Mode()))
	}

	f, err := a.fsForMedium(ctx, medium)
	if err != nil {
		return transport.UploadResult{}, Wrap("upload_from_local", err)
	}

	// The refusal. A NotFound here is the good case and the only one that
	// continues; anything else is a failure to ASK, which must not be read
	// as an answer of "nothing is there" (artifactstore.ErrNotPresent's own
	// doc makes this distinction, and it is the one that decides whether an
	// origin copy gets deleted later).
	switch _, statErr := f.NewObject(ctx, key); {
	case statErr == nil:
		return transport.UploadResult{}, Wrap("upload_from_local", fmt.Errorf("%w: %s", ErrObjectAlreadyPresent, key))
	case isObjectAbsent(statErr):
		// Nothing there. Proceed, WITHOUT a confirmBucket probe: this
		// branch is the healthy path of every upload, and PutObject
		// answers the bucket question by itself a moment later with the
		// real NoSuchBucket code intact (measured). Probing here would
		// buy nothing and cost a round trip per artifact.
	default:
		return transport.UploadResult{}, Wrap("upload_from_local", statErr)
	}

	srcFs, err := fs.NewFs(ctx, path.Dir(localPath))
	if err != nil {
		return transport.UploadResult{}, Wrap("upload_from_local", err)
	}
	srcObj, err := srcFs.NewObject(ctx, path.Base(localPath))
	if err != nil {
		return transport.UploadResult{}, Wrap("upload_from_local", err)
	}

	// copyWithProgress is the same wrapper CopyToLocal uses, and with no
	// transport.ProgressReporter on ctx it calls the closure and does
	// nothing else.
	var dst fs.Object
	if err := copyWithProgress(ctx, srcObj.Size(), func(ctx context.Context) error {
		var copyErr error
		dst, copyErr = operations.Copy(ctx, f, nil, key, srcObj)
		return copyErr
	}); err != nil {
		return transport.UploadResult{}, Wrap("upload_from_local", err)
	}

	// The size is read back off the STORED object rather than counted on
	// the way out, so an endpoint that accepted a truncated upload is
	// visible here rather than three verification steps later. rclone's own
	// no_head option would disable the read this leans on; s3Config never
	// sets it, on purpose.
	result := transport.UploadResult{BytesUploaded: dst.Size()}
	if tierer, ok := dst.(fs.GetTierer); ok {
		result.StorageClass = tierer.GetTier()
	}
	return result, nil
}

// OpenObject reads the object's bytes. The caller closes the reader.
func (a *Adapter) OpenObject(ctx context.Context, medium transport.Medium, key string) (io.ReadCloser, error) {
	f, err := a.fsForMedium(ctx, medium)
	if err != nil {
		return nil, Wrap("open_object", err)
	}
	o, err := f.NewObject(ctx, key)
	if err != nil {
		if isObjectAbsent(err) {
			if berr := a.confirmBucket(ctx, f, medium); berr != nil {
				return nil, Wrap("open_object", berr)
			}
		}
		return nil, Wrap("open_object", err)
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return nil, Wrap("open_object", err)
	}
	return rc, nil
}

// ObjectChecksum asks the medium for its own full-object checksum, for
// FR-31's `attested` class.
//
// # What this returns against S3 today, and why that is the right answer
//
// UnsupportedCapability, always, and that is not a stub. It is the live
// capability query giving its honest answer, and FR-31 asks for exactly
// this rather than a silent downgrade.
//
// rclone v1.75.0's s3 backend reports hash.Set(hash.MD5) from Hashes() and
// nothing else, and its Object.Hash serves that MD5 from setMD5FromEtag:
// the value IS the ETag. FR-32's first bullet says an ETag is never a
// content hash, because multipart uploads and server-side encryption both
// make it not one, so returning it here would be handing a caller the exact
// value the spec forbids comparing against a recorded hash, wearing the
// name of an attestation.
//
// There is a second value that looks like a candidate and is not: rclone
// writes an X-Amz-Meta-Md5chksum user-metadata key on objects it uploads
// and will serve that as the MD5 when present. It is still MD5 rather than
// the SHA-256 the journal records, and more to the point it is metadata
// RCLONE wrote, not a checksum the PROVIDER computed and stores, so
// believing it proves that this product's own earlier upload said something,
// not that the bytes on the medium are those bytes. FR-31's `attested`
// class is specifically the provider's stored full-object checksum.
//
// So the capability is genuinely absent from this backend at this version,
// and a medium configured with upload_verification: attested gets a clear
// refusal rather than a weaker guarantee under a stronger name. If a future
// rclone surfaces x-amz-checksum-sha256 through Hashes(), this function
// starts working with no change here, which is why it queries rather than
// simply returning the error.
func (a *Adapter) ObjectChecksum(ctx context.Context, medium transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	if alg != transport.SHA256 {
		return transport.ChecksumAttestation{}, Wrap("object_checksum", fmt.Errorf(
			"%w: medium %q: this product attests against %s, the algorithm its journal records, and %q is not it "+
				"(an MD5 from an S3 endpoint is its ETag, and FR-32 says an ETag is never a content hash)",
			ErrUnsupportedHash, medium.ID, transport.SHA256, alg))
	}

	f, err := a.fsForMedium(ctx, medium)
	if err != nil {
		return transport.ChecksumAttestation{}, Wrap("object_checksum", err)
	}
	if !f.Hashes().Contains(hash.SHA256) {
		return transport.ChecksumAttestation{}, Wrap("object_checksum", fmt.Errorf(
			"%w: medium %q cannot produce a full-object %s checksum: the embedded rclone's %s backend reports only MD5, "+
				"and that MD5 is the object's ETag, which is never a content hash. Use upload_verification: readback",
			ErrUnsupportedHash, medium.ID, transport.SHA256, medium.Type))
	}

	o, err := f.NewObject(ctx, key)
	if err != nil {
		return transport.ChecksumAttestation{}, Wrap("object_checksum", err)
	}
	sum, err := o.Hash(ctx, hash.SHA256)
	if err != nil {
		return transport.ChecksumAttestation{}, Wrap("object_checksum", err)
	}
	if sum == "" {
		// rclone returns an empty string, with no error, for an object it
		// cannot hash after all. An empty digest that a caller then
		// compared against a recorded one would compare unequal and read
		// as corruption, which is the wrong verdict for an absent
		// capability.
		return transport.ChecksumAttestation{}, Wrap("object_checksum", fmt.Errorf(
			"%w: medium %q returned an empty %s digest for %s", ErrUnsupportedHash, medium.ID, transport.SHA256, key))
	}
	return transport.ChecksumAttestation{Algorithm: transport.SHA256, Value: sum}, nil
}

// DeleteObject deletes exactly the object at key.
//
// An object that is already absent is success, not an error: the caller's
// intent, that these bytes not be on this medium, is already true. That
// matches artifactstore.Store.Remove and Local.Remove's treatment of
// os.ErrNotExist, and it is what makes a re-run of an interrupted delete
// safe.
//
// No safety proof happens here, and none should be added. See the
// interface's own doc: the proof belongs to internal/retention, which
// re-derives it immediately before calling anything that deletes, and a
// check that lives in two places is a check reviewers stop reading in
// either.
func (a *Adapter) DeleteObject(ctx context.Context, medium transport.Medium, key string) error {
	f, err := a.fsForMedium(ctx, medium)
	if err != nil {
		return Wrap("delete_object", err)
	}
	o, err := f.NewObject(ctx, key)
	if err != nil {
		if isObjectAbsent(err) {
			// An absent object is success ONLY once the bucket it would
			// have been in is known to exist. Without this, a delete
			// against a mistyped bucket reports success and FR-30's prune
			// marks the placement GONE for an artifact nobody deleted.
			// Measured: this returned nil against a real MinIO before
			// confirmBucket existed.
			if berr := a.confirmBucket(ctx, f, medium); berr != nil {
				return Wrap("delete_object", berr)
			}
			return nil
		}
		return Wrap("delete_object", err)
	}
	return Wrap("delete_object", o.Remove(ctx))
}

// ListObjects enumerates objects under prefix.
//
// An empty result is not an error. A prefix with nothing under it is an
// ordinary answer on the first upload to a new backup set, and rclone
// reports it as a nil error with no entries, so nothing has to be
// translated for that case to work.
//
// A missing BUCKET is a completely different fact and does arrive here as
// fs.ErrorDirNotFound, which is why the branch below refuses instead of
// returning an empty slice. See confirmBucket for the measurement behind
// that distinction.
//
// Recursive, with the same walk.GetAll call and the same reasoning
// Adapter.List uses: a partial listing that silently omits anything below
// the top level is the protection-dies-quietly failure this product exists
// to prevent, and here it would additionally make a reconciler conclude an
// uploaded artifact was missing.
func (a *Adapter) ListObjects(ctx context.Context, medium transport.Medium, prefix string) ([]transport.ObjectInfo, error) {
	f, err := a.fsForMedium(ctx, medium)
	if err != nil {
		return nil, Wrap("list_objects", err)
	}
	objs, _, err := walk.GetAll(ctx, f, prefix, true, -1)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			// NOT an empty answer. An earlier version of this returned
			// (nil, nil) here on the theory that S3 has no directories to
			// be missing, and measuring it against a real MinIO showed
			// what that actually did: a listing against a NONEXISTENT
			// BUCKET came back as "this medium holds nothing", silently,
			// which is the worst possible answer for a catalog rebuild or
			// a reconciler.
			//
			// The empty-prefix case does not need the mapping anyway: a
			// prefix with nothing under it in a real bucket comes back
			// with a nil error and no entries, because ListObjectsV2
			// answers 200 however empty the prefix is. So ErrorDirNotFound
			// from an s3 listing means the bucket, and it is reported as
			// the configuration problem it is.
			return nil, Wrap("list_objects", fmt.Errorf("%w: medium %q names bucket %q, and the endpoint does not have it", errBucketAbsent, medium.ID, medium.Bucket))
		}
		return nil, Wrap("list_objects", err)
	}
	out := make([]transport.ObjectInfo, 0, len(objs))
	for _, o := range objs {
		out = append(out, objectInfo(ctx, o))
	}
	// Sorted for Adapter.List's reason: the backend returns whatever order
	// it produced, and a caller comparing two listings, or reporting one,
	// should not see a different order each time for the same content.
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
