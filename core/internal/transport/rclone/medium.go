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
// multiply. Measured through this adapter, StatObject against a port
// nothing is listening on (so every attempt fails INSTANTLY, with
// "connection refused"):
//
//	LowLevelRetries=10   3m47.16s
//	LowLevelRetries=2       2.39s
//
// Almost all of that is backoff between attempts rather than time on the
// wire, which is why bounding the count is the whole fix.
//
// It is not a fix for every shape of unreachable. Against an address that
// BLACKHOLES rather than refuses (an unrouted 192.0.2.1, say), one TCP
// connect attempt is itself about two minutes, so two attempts still cost
// four. Nothing reachable from here shortens that; a per-operation
// deadline is what bounds it, and that belongs to the caller.
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
//
// Every method here also releases its Fs on the way out (shutdownFs), the
// discipline #264 established for the Transport half after an Fs per
// operation with nothing ever releasing one turned into a failed backup
// against a host that refuses a third connection. An s3 Fs holds an HTTP
// client and a pacer rather than a pool of SSH sessions, so the failure it
// leaks toward is gentler, but "build one per operation and never release
// it" is the same shape, and one adapter should not have two answers to
// it.
func mediumContext(ctx context.Context) context.Context {
	bounded, config := fs.AddConfig(ctx)
	config.LowLevelRetries = mediumRetries
	return bounded
}

// s3Options is the COMPLETE set of rclone s3 options this adapter will
// ever produce, and it is an allowlist rather than a pass-through, exactly
// as sftpConfig is for a Source.
//
// The point is what is NOT here. rclone's s3 backend takes something over
// a hundred options, and a config surface that forwarded them would make
// assume-role, the SSE-C family, download_url, presigned requests, v2
// signing, versioned views and ACL headers all reachable from a config
// file, each of them a way to change WHERE a backup goes or WHO writes it
// that no reviewer of this repository would ever see.
// TestS3OptionsAreExactlyThisAllowlist pins the producible key set, so
// widening it is a test failure rather than a review miss.
func s3Options(medium transport.Medium) (configmap.Simple, error) {
	cfg := configmap.Simple{}

	auth, err := mediumAuthOptions(medium)
	if err != nil {
		return nil, err
	}
	for k, v := range auth {
		cfg.Set(k, v)
	}

	// "AWS" only when no endpoint was given. rclone uses the provider to
	// decide addressing style and a handful of quirks, and "Other" is its
	// own name for "an S3 API this list does not enumerate", which is what
	// an endpoint override means. There is deliberately no provider field
	// for an operator to set: picking "Minio" or "Ceph" out of an endpoint
	// URL would be guessing, and if a real provider quirk ever demands a
	// specific name that is a config change with its own issue.
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
	return cfg, nil
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
		s3cfg, err := s3Options(medium)
		if err != nil {
			return nil, err
		}
		cfg = s3cfg
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
		// A backend may hand back a LIVE Fs alongside an error, which is
		// the leak "release on the way out of each operation" cannot
		// catch: no caller ever sees this Fs, so no caller ever defers a
		// shutdown for it. newFs learned this from rclone's sftp backend
		// in #264; the s3 backend's own NewFs has the same
		// `return f, err` shape when the root names an object rather
		// than a bucket, so it gets the same treatment rather than an
		// argument about which backends do it.
		if f != nil {
			shutdownFs(ctx, f)
		}
		return nil, Wrap("medium_fs", fmt.Errorf("medium %q: %w", medium.ID, err))
	}
	return f, nil
}

// errBucketAbsent is what confirmBucket reports when the medium's bucket
// is not there at all. It is a Configuration failure rather than a
// NotFound one (FR-28), and it is a sentinel so this file states the fact
// once and a test can recognise which rule fired.
var errBucketAbsent = errors.New("the medium's bucket does not exist")

// confirmBucket answers the question rclone's error translation throws
// away: is this KEY absent, or is the whole BUCKET absent?
//
// # Why it is needed at all
//
// rclone's s3 backend turns a 404 into its own filesystem-shaped sentinels
// before this adapter ever sees it (a 404 from HeadObject becomes
// fs.ErrorObjectNotFound at backend/s3/s3.go, and a 404 from ListObjectsV2
// becomes fs.ErrorDirNotFound at s3.go:2521), and in doing so it discards
// the S3 error code, which is the only thing separating NoSuchKey from
// NoSuchBucket. Same status, completely different problems: one is a fact
// about a single artifact, the other is a typo somebody has to go and fix.
//
// Measured against a real MinIO, before this existed, on the code that is
// now in this file:
//
//   - DeleteObject against a bucket that does not exist returned nil. The
//     delete reported SUCCESS, because NewObject said not-found and
//     DeleteObject correctly treats an already-absent object as success.
//     Under FR-30's medium-aware prune that marks every placement on the
//     medium GONE for artifacts nobody deleted.
//   - ListObjects against one returned an empty listing and no error, so a
//     catalog rebuild concludes the medium holds nothing.
//   - StatObject and OpenObject reported NotFound, which a reconciler reads
//     as the medium having lost the artifact.
//
// # Why a listing is a sound probe
//
// Because the two cases ARE separable at the listing layer even though
// they are not at the object layer. A ListObjectsV2 against an existing
// bucket answers 200 with no contents however empty it is, and only a
// missing bucket makes it 404, which is the single case rclone's list path
// turns into ErrorDirNotFound (s3.go:2521). The one other route to that
// sentinel, the directory-marker HEAD at s3.go:2632, is behind
// f.opt.DirectoryMarkers, which this adapter never sets.
//
// It probes the bucket ROOT, never the medium's prefix, and that is
// load-bearing rather than incidental: a prefix nothing has been written
// under yet is exactly the state of a brand new medium, and probing it
// would turn the first operation against a correctly configured medium
// into "your bucket does not exist".
//
// # What it costs
//
// One extra listing, only on the path that was ABOUT to report not-found,
// so never on a successful stat, read, list or delete. UploadFromLocal
// does not call it: an upload's own failure carries the real NoSuchBucket
// code intact, and probing there would put a round trip on the hot path to
// answer a question the endpoint already answered.
func (a *Adapter) confirmBucket(ctx context.Context, f fs.Fs, medium transport.Medium) error {
	if _, err := f.List(ctx, ""); err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return transport.NewError(transport.Configuration, "confirm_bucket", fmt.Errorf(
				"%w: medium %q names bucket %q and the endpoint does not have it",
				errBucketAbsent, medium.ID, medium.Bucket))
		}
		// The probe itself failed for some other reason (unreachable,
		// unauthorized). That is a better answer than the not-found the
		// caller was about to give, so it is the one that is returned.
		return Wrap("confirm_bucket", err)
	}
	return nil
}

// absenceOrMissingBucket is what every method that is about to report
// "there is nothing at this key" calls first.
//
// It returns the error the caller should report: either the bucket-absent
// verdict, or the original not-found once the bucket has been confirmed to
// exist. Keeping it in one function rather than inline at four call sites
// is deliberate: getting this at three of the four is a silent half-fix,
// and the one that was missed would be the one that reports success.
func (a *Adapter) absenceOrMissingBucket(ctx context.Context, f fs.Fs, medium transport.Medium, op string, absence error) error {
	if err := a.confirmBucket(ctx, f, medium); err != nil {
		return err
	}
	return Wrap(op, absence)
}

// isAbsence reports whether err is rclone's way of saying "there is
// nothing here", in either of the two shapes its s3 backend produces for a
// 404. Both are checked because catching only one is a silent half-fix.
func isAbsence(err error) bool {
	return errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound)
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
	defer shutdownFs(ctx, f)
	o, err := f.NewObject(ctx, key)
	if err != nil {
		if isAbsence(err) {
			// "this artifact is not on the medium" and "this medium's
			// bucket does not exist" must not reach a caller as the same
			// answer. See confirmBucket.
			return transport.ObjectInfo{}, a.absenceOrMissingBucket(ctx, f, medium, "stat_object", err)
		}
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
	defer shutdownFs(ctx, dstFs)

	srcDir, srcName := splitPath(localPath)
	srcFs, err := fs.NewFs(ctx, srcDir)
	if err != nil {
		return transport.UploadResult{}, Wrap("upload_from_local", err)
	}
	defer shutdownFs(ctx, srcFs)
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
		if isAbsence(err) {
			reported := a.absenceOrMissingBucket(ctx, f, medium, "open_object", err)
			shutdownFs(ctx, f)
			return nil, reported
		}
		shutdownFs(ctx, f)
		return nil, Wrap("open_object", err)
	}
	rc, err := o.Open(ctx)
	if err != nil {
		shutdownFs(ctx, f)
		return nil, Wrap("open_object", err)
	}
	// This is the one method whose Fs cannot be released on the way out:
	// the reader it returns is still reading through it. So the release
	// rides on the reader's own Close, which the caller is already
	// obliged to call, rather than being skipped because this method has
	// no convenient place for a defer.
	return &fsBoundReadCloser{ReadCloser: rc, fs: f, ctx: ctx}, nil
}

// fsBoundReadCloser releases an Fs when the reader taken from it is
// closed.
//
// Closing the Fs happens whatever the reader's own Close reports, and that
// error is the one returned: a failure to hang up cleanly must not mask a
// failure to finish reading, and it must not turn a completed read into a
// reported failure either.
type fsBoundReadCloser struct {
	io.ReadCloser
	fs  fs.Fs
	ctx context.Context
}

func (r *fsBoundReadCloser) Close() error {
	err := r.ReadCloser.Close()
	shutdownFs(r.ctx, r.fs)
	return err
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
	defer shutdownFs(ctx, f)
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
	defer shutdownFs(ctx, f)
	o, err := f.NewObject(ctx, key)
	if err != nil {
		if wrapped := Wrap("delete_object", err); isNotFound(wrapped) {
			// An already-absent object is success, but only once the
			// bucket it would have been in is known to exist. Without
			// that check a delete against a mistyped bucket reported
			// success and the prune marked the placement GONE.
			if berr := a.confirmBucket(ctx, f, medium); berr != nil {
				return berr
			}
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
	defer shutdownFs(ctx, f)
	objs, _, err := walk.GetAll(ctx, f, prefix, true, -1)
	if err != nil {
		if wrapped := Wrap("list_objects", err); isNotFound(wrapped) {
			// A prefix holding nothing is an empty listing, but a bucket
			// that is not there must never be reported as one: a catalog
			// rebuild reading that concludes the medium holds nothing.
			if berr := a.confirmBucket(ctx, f, medium); berr != nil {
				return nil, berr
			}
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
