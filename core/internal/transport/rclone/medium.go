// This file is FR-28: transport.MediumStore, implemented over the embedded
// rclone's own s3 backend, and implemented ONLY here.
//
// The FR-3 rules that govern Adapter's Transport half govern this half
// unchanged: no rclone type appears in a signature crossing the boundary,
// every error leaves classified through Wrap, and there is no Move. What is
// worth stating separately is what this adapter deliberately does NOT do.
//
// # It never creates a bucket
//
// rclone's s3 backend checks for the destination bucket on upload and
// creates it when it is missing, unless no_check_bucket is set. This
// adapter sets it, always. A backup manager that quietly creates the
// bucket it was pointed at turns a typo in an endpoint or a bucket name
// into a silent, empty, second home for artifacts nobody will look in
// again, and it does so with the credentials that were meant to write
// backups, not to provision infrastructure. With the check off, a bucket
// that is not there fails as NoSuchBucket, which classify.go turns into
// transport.Configuration: one line for an operator to fix, rather than a
// bucket for them to discover.
//
// # It offers exactly one hash algorithm, and refuses the rest
//
// ObjectChecksum speaks SHA-256 and nothing else. That is FR-32 made
// structural rather than remembered: the digest an S3 endpoint hands back
// for free is the ETag, the ETag is an MD5 that stops being a whole-object
// MD5 the moment an upload is multipart, and the one thing this product
// must never do is compare it to the SHA-256 it recorded at ingestion.
// There is no way to ask this boundary for that value, so there is no way
// to compare it by accident.
//
// # Against rclone v1.75.0, an s3 medium cannot attest at all
//
// backend/s3's Fs.Hashes() returns exactly hash.MD5, and Object.Hash
// refuses anything else outright. So ObjectChecksum against an s3 medium
// answers with an UnsupportedCapability refusal today, every time, and
// FR-31's `attested` verification class is not reachable on this rclone.
// That is the honest outcome, and it is the outcome FR-13 asks for: an
// explicit capability result rather than a silent downgrade. It also means
// a medium configured with upload_verification: attested cannot be served,
// which is the move engine's problem to refuse loudly (#238) rather than
// this file's to paper over.
package rclone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

var _ transport.MediumStore = (*Adapter)(nil)

// mediumFs builds an rclone Fs rooted at the medium's bucket, without
// touching any on-disk rclone config file: everything comes from the
// manager's own configuration, so there is no ambient rclone state to leak
// in, exactly as fsFor already guarantees for a Source.
//
// The Fs is rooted at the BUCKET, not at the bucket plus prefix, because
// transport.MediumKey already builds a whole key including the prefix.
// One place composes a key, and every method here addresses objects by
// that key verbatim; splitting the prefix between the Fs root and the key
// would give the same object two spellings depending on which side of the
// boundary you asked.
// mediumRetries is how many times rclone, and the AWS SDK underneath it,
// may retry one low-level request before giving the failure back.
//
// rclone's own default is 10, and against an endpoint that is simply not
// there that default costs almost four minutes of silence per call: rclone
// retries the operation, the SDK retries each attempt, and the two
// multiply. That was measured, not guessed (an unreachable endpoint took
// 235 seconds to report through this adapter before this bound existed).
//
// Two is the right number here because this product already has a retry
// policy of its own, one layer up: transport/retry's bounded backoff acts
// on the FR-22 category, which is the level at which "should this be tried
// again" is actually a decision. rclone's low-level retries are for
// smoothing a flaky byte stream, not for waiting out an outage, and a
// medium operation that cannot get through needs to say so while a cycle
// still has time to do something else.
//
// It is applied to the medium path only. fsFor's Transport path keeps
// rclone's default, because changing what an sftp transfer does under a
// flaky link is a real behavioural change with a live integration suite
// pinned to it, and it belongs to its own issue.
const mediumRetries = 2

// mediumContext bounds rclone's own retrying for one medium operation. It
// has to wrap the context the Fs is BUILT with as well as the one the
// operation runs under: the s3 backend reads LowLevelRetries once, at
// construction, to size the AWS SDK's retryer.
func mediumContext(ctx context.Context) context.Context {
	bounded, config := fs.AddConfig(ctx)
	config.LowLevelRetries = mediumRetries
	return bounded
}

func (a *Adapter) mediumFs(ctx context.Context, medium transport.Medium) (fs.Fs, error) {
	if medium.ID == "" {
		return nil, transport.NewError(transport.Configuration, "medium_fs", errors.New("medium has no id"))
	}
	if medium.Bucket == "" {
		return nil, transport.NewError(transport.Configuration, "medium_fs", fmt.Errorf("medium %q names no bucket", medium.ID))
	}

	var backend string
	cfg := configmap.Simple{}

	switch medium.Type {
	case transport.MediumTypeS3:
		backend = "s3"
		auth, err := mediumAuthOptions(medium)
		if err != nil {
			return nil, err
		}
		for k, v := range auth {
			cfg.Set(k, v)
		}
		// "AWS" only when no endpoint was given. rclone uses the provider
		// to decide addressing style and a handful of quirks, and "Other"
		// is its own name for "an S3 API this list does not enumerate",
		// which is what an endpoint override means.
		if medium.Endpoint == "" {
			cfg.Set("provider", "AWS")
		} else {
			cfg.Set("provider", "Other")
			cfg.Set("endpoint", medium.Endpoint)
		}
		if medium.Region != "" {
			cfg.Set("region", medium.Region)
		}
		if medium.StorageClass != "" {
			cfg.Set("storage_class", medium.StorageClass)
		}
		// See this file's package comment: never provision, never guess.
		cfg.Set("no_check_bucket", "true")
	case transport.MediumTypeLocalDir:
		backend = "local"
	default:
		return nil, transport.NewError(transport.Configuration, "medium_fs",
			fmt.Errorf("medium %q has type %q, which this adapter does not implement", medium.ID, medium.Type))
	}

	info, err := fs.Find(backend)
	if err != nil {
		return nil, transport.NewError(transport.Configuration, "medium_fs",
			fmt.Errorf("backend %q is not registered in this binary: %w", backend, err))
	}

	f, err := info.NewFs(ctx, medium.ID, medium.Bucket, withBackendDefaults(cfg, info))
	if err != nil {
		return nil, Wrap("medium_fs", fmt.Errorf("medium %q: %w", medium.ID, err))
	}
	return f, nil
}

// toObjectInfo carries exactly what FR-32 allows a medium to tell this
// product: a key, a size, an upload time that is never a producer
// timestamp, and a storage class where the backend reports one. No ETag,
// no digest, nothing to mistake for a content hash.
func toObjectInfo(ctx context.Context, o fs.Object) transport.ObjectInfo {
	info := transport.ObjectInfo{
		Key:  o.Remote(),
		Size: o.Size(),
	}
	if t := o.ModTime(ctx); !t.IsZero() {
		info.ModTime = t.Unix()
	}
	if tierer, ok := o.(fs.GetTierer); ok {
		info.StorageClass = tierer.GetTier()
	}
	return info
}

func (a *Adapter) StatObject(ctx context.Context, medium transport.Medium, key string) (transport.ObjectInfo, error) {
	ctx = mediumContext(ctx)
	f, err := a.mediumFs(ctx, medium)
	if err != nil {
		return transport.ObjectInfo{}, err
	}
	o, err := f.NewObject(ctx, key)
	if err != nil {
		return transport.ObjectInfo{}, Wrap("stat_object", err)
	}
	return toObjectInfo(ctx, o), nil
}

// UploadFromLocal copies the file at localPath to key on the medium.
//
// Copy, never Move, and never a delete of the source: the local copy is
// the one guaranteed intact until the move engine has durably recorded
// that the destination verified (FR-30). This method has no opinion about
// when that is; it just never takes the decision away.
func (a *Adapter) UploadFromLocal(ctx context.Context, medium transport.Medium, localPath, key string, opts transport.UploadOptions) (transport.UploadResult, error) {
	ctx = mediumContext(ctx)
	if key == "" {
		return transport.UploadResult{}, transport.NewError(transport.Configuration, "upload_from_local",
			errors.New("an upload needs a destination key"))
	}

	dstMedium := medium
	if opts.StorageClass != "" {
		dstMedium.StorageClass = opts.StorageClass
	}
	dstFs, err := a.mediumFs(ctx, dstMedium)
	if err != nil {
		return transport.UploadResult{}, err
	}

	srcDir, srcName := splitPath(localPath)
	srcFs, err := fs.NewFs(ctx, srcDir)
	if err != nil {
		return transport.UploadResult{}, Wrap("upload_from_local", err)
	}
	srcObj, err := srcFs.NewObject(ctx, srcName)
	if err != nil {
		return transport.UploadResult{}, Wrap("upload_from_local", err)
	}

	var dst fs.Object
	if err := copyWithProgress(ctx, srcObj.Size(), func(ctx context.Context) error {
		var copyErr error
		dst, copyErr = operations.Copy(ctx, dstFs, nil, key, srcObj)
		return copyErr
	}); err != nil {
		return transport.UploadResult{}, Wrap("upload_from_local", err)
	}
	return transport.UploadResult{Key: key, BytesUploaded: dst.Size()}, nil
}

func (a *Adapter) OpenObject(ctx context.Context, medium transport.Medium, key string) (io.ReadCloser, error) {
	ctx = mediumContext(ctx)
	f, err := a.mediumFs(ctx, medium)
	if err != nil {
		return nil, err
	}
	o, err := f.NewObject(ctx, key)
	if err != nil {
		return nil, Wrap("open_object", err)
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return nil, Wrap("open_object", err)
	}
	return rc, nil
}

// ObjectChecksum asks the medium for its own digest of the WHOLE object.
//
// Both refusals below are the same refusal FR-13 asks for and FR-31
// restates: an explicit capability result, never a weaker answer under a
// stronger name. See this file's package comment for why an s3 medium
// always takes the second one on rclone v1.75.0.
func (a *Adapter) ObjectChecksum(ctx context.Context, medium transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	ctx = mediumContext(ctx)
	if alg != transport.SHA256 {
		return transport.ChecksumAttestation{}, Wrap("object_checksum", fmt.Errorf(
			"%w: this boundary attests %s and nothing else, so an ETag's MD5 can never be compared to a recorded hash: %q",
			ErrUnsupportedHash, transport.SHA256, alg))
	}

	f, err := a.mediumFs(ctx, medium)
	if err != nil {
		return transport.ChecksumAttestation{}, err
	}
	if !f.Hashes().Contains(hash.SHA256) {
		return transport.ChecksumAttestation{}, Wrap("object_checksum", fmt.Errorf(
			"%w: medium %q (type %s) cannot attest a full-object %s",
			ErrUnsupportedHash, medium.ID, medium.Type, transport.SHA256))
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
		// rclone's own convention for "this object has no such hash
		// recorded" is an empty string with a nil error, and an empty
		// digest compared against a recorded one would compare equal to
		// nothing and unequal to everything, which is a verdict this
		// product must not produce by accident.
		return transport.ChecksumAttestation{}, Wrap("object_checksum", fmt.Errorf(
			"%w: medium %q holds no %s for %q", ErrUnsupportedHash, medium.ID, transport.SHA256, key))
	}
	return transport.ChecksumAttestation{Algorithm: transport.SHA256, Value: sum}, nil
}

// DeleteObject removes exactly the object at key.
//
// An object that is already gone is not an error: the caller's intent,
// that these bytes not be on this medium, is satisfied, and a mover
// resuming after a crash must be able to finish a delete it may already
// have completed.
func (a *Adapter) DeleteObject(ctx context.Context, medium transport.Medium, key string) error {
	ctx = mediumContext(ctx)
	f, err := a.mediumFs(ctx, medium)
	if err != nil {
		return err
	}
	o, err := f.NewObject(ctx, key)
	if err != nil {
		if wrapped := Wrap("delete_object", err); isNotFound(wrapped) {
			return nil
		}
		return Wrap("delete_object", err)
	}
	return Wrap("delete_object", o.Remove(ctx))
}

// ListObjects enumerates what the medium holds under prefix.
//
// prefix is a key prefix at a "/" boundary, which is what FR-28's layout
// produces and all it ever produces. A prefix holding nothing is an empty
// result and not an error: "there is nothing here" is an answer, and a
// caller rebuilding a catalog has to be able to tell it apart from "the
// medium could not be reached".
func (a *Adapter) ListObjects(ctx context.Context, medium transport.Medium, prefix string) ([]transport.ObjectInfo, error) {
	ctx = mediumContext(ctx)
	f, err := a.mediumFs(ctx, medium)
	if err != nil {
		return nil, err
	}
	objs, _, err := walk.GetAll(ctx, f, prefix, true, -1)
	if err != nil {
		if wrapped := Wrap("list_objects", err); isNotFound(wrapped) {
			return nil, nil
		}
		return nil, Wrap("list_objects", err)
	}
	out := make([]transport.ObjectInfo, 0, len(objs))
	for _, o := range objs {
		out = append(out, toObjectInfo(ctx, o))
	}
	// Sorted for List's reason: an unordered listing makes a rebuild's
	// conflict reporting depend on whatever order the backend happened to
	// yield, which is the difference between a conflict an operator can
	// reason about and one they cannot reproduce.
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// isNotFound reports whether err carries this project's own NotFound
// classification. It reads the manager-owned category rather than the
// underlying rclone sentinel on purpose: which sentinel a backend produces
// for an absent object is exactly the detail errors.go exists to absorb.
func isNotFound(err error) bool {
	category, ok := transport.CategoryOf(err)
	return ok && category == transport.NotFound
}

// withBackendDefaults layers a backend's own option defaults underneath
// the options this adapter set, and layers nothing else underneath those.
//
// # Why this is needed at all
//
// Handing info.NewFs a bare configmap.Simple, the way fsFor has always
// done for sftp, means every option this adapter did not set explicitly
// arrives at the backend as its ZERO value rather than as the default
// rclone documents. sftp survives that because its zero values are
// workable; s3 does not, and it says so immediately: an unset chunk_size
// arrives as 0 and the backend refuses to build at all with "chunk size:
// 0 is less than 5Mi". Multipart thresholds, concurrency, the list
// chunk, the copy cutoff and a dozen other knobs are in the same
// position, and the ones that do not refuse outright would silently run
// at zero.
//
// # Why not rclone's own fs.ConfigMap
//
// Because it layers in three things this adapter refuses to have: the
// rclone config FILE for this remote name, remote-specific environment
// variables (RCLONE_CONFIG_<NAME>_*), and backend-wide ones
// (RCLONE_S3_*). Every one of those is ambient state outside this
// product's configuration that could change where a backup is written or
// which credentials write it, which is exactly what fsFor's own doc
// promises does not happen. So this composes the one layer that is wanted,
// defaults, and none of the ones that are not.
//
// It is deliberately not retrofitted onto fsFor's sftp path here. That
// path has worked this way since the adapter was written and its behaviour
// is pinned by a live SFTP integration suite; changing what options an
// sftp Fs is built with is a real behavioural change and belongs to its
// own issue, not to a drive-by in a change about mediums.
func withBackendDefaults(overrides configmap.Simple, info *fs.RegInfo) *configmap.Map {
	m := configmap.New()
	m.AddGetter(overrides, configmap.PriorityNormal)
	m.AddGetter(backendDefaults{options: info.Options}, configmap.PriorityDefault)
	return m
}

// backendDefaults answers with a backend option's documented default, and
// only ever with that.
type backendDefaults struct {
	options fs.Options
}

func (d backendDefaults) Get(key string) (string, bool) {
	for i := range d.options {
		if d.options[i].Name == key {
			return d.options[i].String(), true
		}
	}
	return "", false
}
