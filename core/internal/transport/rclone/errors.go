// Package rclone: this file is the FR-22 translation. It is the one place
// that is allowed to know what an rclone-shaped error looks like, so that
// nowhere else in this repository has to.
//
// The reason this matters more than a typical internal-error-handling
// concern: failure-safety invariant 12 says lifecycle policy must not depend
// on parsing rclone log/error strings, and invariant 13 says rclone API
// details must not leak outside the transport adapter. Together they mean an
// rclone upgrade is allowed to reword, restructure or entirely replace any
// error message it produces, on any release, without that being a breaking
// change for this codebase, as long as this file is the only place that
// would need to change in response. Classify is that seam.
//
// Wherever rclone (or a library it embeds) exposes a typed error or an
// exported sentinel value, this file matches on that, by identity, not by
// text: errors.As/errors.Is against a real Go value survives an upstream
// wording change even if the file were never touched again. Adapter.go's own
// ErrUnsupportedHash (RemoteHash's error for an algorithm this adapter
// cannot translate, or the backend cannot compute) is exactly this kind of
// value: it costs nothing to define here since this package authors it, so
// it is matched by errors.Is like every other sentinel below rather than by
// the text RemoteHash happens to format around it. One case has no such
// value to reach for:
//
//   - authentication failure: golang.org/x/crypto/ssh has a typed
//     ServerAuthError for the server side of an SSH exchange, but nothing
//     equivalent for a client that failed to authenticate outward, which is
//     the only direction this adapter ever drives. client_auth.go's
//     "ssh: unable to authenticate, ..." is the only signal available, and
//     it comes from x/crypto/ssh (an rclone dependency), not from rclone
//     itself, so it changes on an x/crypto/ssh upgrade, not an rclone one,
//     but the same caution applies.
//
// errors_test.go proves every category this function can reach against a
// real error from a real rclone call wherever that is achievable (a
// disposable Docker SFTP server for authentication, host verification,
// permission and capability; a real closed TCP port for the transient
// network case; a real cancelled context). Conflict and IntegrityFailure are
// the two exceptions: this adapter's surface (List, Stat, CopyToLocal,
// RemoteHash, DeleteRemote against local or sftp) has no reachable path that
// provokes rclone's own fs.ErrorDirExists or its "corrupted on transfer"
// transfer-verification failure without a flaky timing race against a live
// transfer, so those two are proved against the real, cited rclone values
// instead of a live-triggered scenario. See the PR description.
package rclone

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"

	rclonefs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	rclonehash "github.com/rclone/rclone/fs/hash"

	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// integrityFailurePrefix is rclone's own wording for a transfer that failed
// its post-copy size/hash verification (fs/operations/copy.go and
// fs/operations/operations.go in rclone v1.75.0, both literally
// `fmt.Errorf("corrupted on transfer...")`). rclone's own manual documents
// this exact phrase as what a caller should look for ("...give an error
// 'corrupted on transfer' if they don't match"), which is as close to a
// stable public contract as an error string gets without being a typed
// value.
const integrityFailurePrefix = "corrupted on transfer"

// apiError is the part of the S3 error model this file matches on: a code
// naming what the service said went wrong.
//
// # Why this is a locally-declared interface and not an SDK import
//
// The real types are github.com/aws/smithy-go's APIError and the s3 service
// package's own error types, and matching them by identity is exactly what
// this file's package doc says to do wherever a library rclone embeds
// exposes a typed error. But importing them would put a provider SDK in
// this repository's own import graph, which is what FR-28 forbids and what
// backends_test.go's TestNoCloudProviderSDKIsImportedDirectly enforces with
// no exception for this package.
//
// Go interfaces are structural, so there is no conflict to resolve: an
// interface declared here matches any error in the chain that has the
// method, whoever wrote it, and errors.As does the walking. That is
// strictly BETTER than importing smithy, not a compromise: it survives
// rclone swapping its SDK, it costs no dependency, and it cannot drift out
// of sync with an upstream type this project does not control. What it
// gives up is compile-time proof that the real errors satisfy it, so the
// MinIO integration suite provides that proof at runtime instead, against a
// real endpoint returning real NoSuchBucket, NoSuchKey and AccessDenied.
//
// The risk of a false match is an unrelated error type with an
// ErrorCode() string method landing in a chain this classifier sees.
// Nothing in this repository has one, and a code this table does not
// recognise falls through to the rest of Classify unchanged, so the cost of
// one would be nil.
type apiError interface {
	error
	ErrorCode() string
}

// s3ErrorCategories maps an S3 error code to the FR-22 category it means,
// per FR-28: "throttling and 5xx are Transient, NoSuchBucket and endpoint
// resolution failures are Configuration, AccessDenied and signature
// failures are Auth, NoSuchKey is NotFound".
//
// Codes rather than HTTP status, because a code says what happened and a
// status says only how the service felt about it: 404 covers both "this key
// is not here", which is a fact about one artifact, and "this bucket does
// not exist", which is a fact about the configuration and needs a person.
// Those two must not collapse, and the code is the only thing that
// separates them.
//
// A code absent from this table is not classified here at all; Classify
// falls through to its existing sentinel checks and, failing those, to
// Permanent. That is the safe direction: an unrecognised failure treated as
// retryable would spend a backoff budget on something that cannot succeed,
// and treated as a capability absence would read as "nothing to worry
// about".
var s3ErrorCategories = map[string]transport.Category{
	// Throttling and service-side failures. Retrying these is the whole
	// reason FR-22 has a Transient category.
	"SlowDown":                               transport.Transient,
	"RequestLimitExceeded":                   transport.Transient,
	"RequestThrottled":                       transport.Transient,
	"RequestThrottledException":              transport.Transient,
	"Throttling":                             transport.Transient,
	"ThrottlingException":                    transport.Transient,
	"TooManyRequestsException":               transport.Transient,
	"ProvisionedThroughputExceededException": transport.Transient,
	"InternalError":                          transport.Transient,
	"InternalFailure":                        transport.Transient,
	"ServiceUnavailable":                     transport.Transient,
	"RequestTimeout":                         transport.Transient,
	"RequestTimeoutException":                transport.Transient,

	// Facts about the configuration. None of these starts being untrue on
	// a second attempt; every one of them needs somebody to edit a config
	// file.
	//
	// PermanentRedirect and AuthorizationHeaderMalformed are both what a
	// wrong REGION looks like from the outside, which is worth having here
	// because "your region is wrong" reads nothing like either code.
	"NoSuchBucket":                       transport.Configuration,
	"InvalidBucketName":                  transport.Configuration,
	"PermanentRedirect":                  transport.Configuration,
	"AuthorizationHeaderMalformed":       transport.Configuration,
	"IllegalLocationConstraintException": transport.Configuration,
	"InvalidLocationConstraint":          transport.Configuration,

	// Credentials. FR-28 puts AccessDenied here rather than under
	// PermissionDenied on purpose: against an object store, "denied" is
	// overwhelmingly a key or a policy attached to a key, which is the
	// same thing an operator has to go and fix, and PermissionDenied in
	// this vocabulary means a remote-side refusal reached over a
	// perfectly good session (see internal/app/halt.go's haltReasonFor).
	"AccessDenied":                transport.Authentication,
	"InvalidAccessKeyId":          transport.Authentication,
	"SignatureDoesNotMatch":       transport.Authentication,
	"InvalidSecurity":             transport.Authentication,
	"InvalidToken":                transport.Authentication,
	"ExpiredToken":                transport.Authentication,
	"TokenRefreshRequired":        transport.Authentication,
	"AuthFailure":                 transport.Authentication,
	"UnrecognizedClientException": transport.Authentication,
	"MissingAuthenticationToken":  transport.Authentication,
	"AccountProblem":              transport.Authentication,

	// One object, absent.
	"NoSuchKey":    transport.NotFound,
	"NotFound":     transport.NotFound,
	"NoSuchUpload": transport.NotFound,

	// The bytes arrived and did not match what was declared. This is the
	// one category where a retry is actively wrong: it would re-upload the
	// same corrupt payload.
	"BadDigest":                 transport.IntegrityFailure,
	"InvalidDigest":             transport.IntegrityFailure,
	"XAmzContentSHA256Mismatch": transport.IntegrityFailure,

	// InvalidObjectState is an object in an archive class that has to be
	// restored before it can be read (FR-34). It is a capability absence
	// at this moment rather than a permanent one, and #241 owns turning it
	// into an explicit restore; until then UnsupportedCapability is the
	// honest label, because no retry changes it and nothing is wrong.
	"InvalidObjectState": transport.UnsupportedCapability,
	"NotImplemented":     transport.UnsupportedCapability,

	"BucketAlreadyExists":     transport.Conflict,
	"BucketAlreadyOwnedByYou": transport.Conflict,
}

// classifyS3Error reads an S3 error code out of err's chain and maps it.
// ok is false when there is no code, or the code is one this table does not
// place, so the caller keeps looking.
func classifyS3Error(err error) (transport.Category, bool) {
	var api apiError
	if !errors.As(err, &api) {
		return transport.Unclassified, false
	}
	category, ok := s3ErrorCategories[api.ErrorCode()]
	return category, ok
}

// Classify translates err, as returned by this package's Adapter, into the
// manager-owned transport.Category lifecycle code is allowed to switch on.
// It never returns an error and never fails: a case it does not recognize
// classifies as transport.Permanent, on purpose, because treating an
// unrecognized failure as safe-to-retry (transport.Transient) or as
// safe-to-ignore (transport.UnsupportedCapability) would be the actual
// failure-safety violation, not classifying it as narrowly as possible.
//
// Classify is idempotent: calling it on an error that already carries a
// *transport.Error (for example, one Wrap already produced) returns that
// same Category rather than reclassifying, so wrapping twice by accident
// cannot change the answer.
func Classify(err error) transport.Category {
	if err == nil {
		return transport.Unclassified
	}

	if category, ok := transport.CategoryOf(err); ok {
		return category
	}

	// Cancellation is a program decision, not a judgement about the error's
	// cause, so it takes priority over every category below (FR-22:
	// "Cancellation SHALL propagate through Go contexts").
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return transport.Cancelled
	}

	// The S3 code comes BEFORE every sentinel check below, and the order is
	// load-bearing rather than incidental. rclone's s3 backend translates
	// some failures into its own filesystem-shaped sentinels on the way
	// out (a 404 becomes fs.ErrorObjectNotFound or fs.ErrorDirNotFound),
	// and where it does, the code is gone. Where it does NOT, the code is
	// the only thing that can tell "this key is absent" apart from "this
	// bucket does not exist", which are the same status and completely
	// different problems. So the code is read first, wherever one survived.
	if category, ok := classifyS3Error(err); ok {
		return category
	}

	// An endpoint that does not resolve is a fact about the configuration,
	// and FR-28 says so explicitly. This has to come before the
	// fserrors.ShouldRetry fallback at the bottom, which treats a DNS
	// failure as a network condition worth retrying: NXDOMAIN is not a
	// network condition, it is a hostname nobody has ever registered, and
	// a backup job retrying it for its whole backoff budget every cycle
	// tells the operator nothing.
	//
	// Only IsNotFound. A DNS server that is temporarily unreachable is a
	// genuine transient and is left to ShouldRetry, which is better at
	// that question than this file would be.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return transport.Configuration
	}

	// golang.org/x/crypto/ssh/knownhosts is the exact library rclone's sftp
	// backend calls into for known_hosts_file checking (see ssh.go's package
	// comment). It returns a typed error for both shapes of host-key
	// failure: KeyError.Want is empty for an unknown host, non-empty for a
	// mismatch, but both are the same manager-owned category, since either
	// way this adapter refuses to talk to whatever answered.
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		return transport.HostVerification
	}
	var revokedErr *knownhosts.RevokedError
	if errors.As(err, &revokedErr) {
		return transport.HostVerification
	}

	// See the package comment: no typed client-side auth error exists in
	// golang.org/x/crypto/ssh, so this is the one case that has to match
	// dependency-authored text rather than a Go value.
	if strings.Contains(err.Error(), "unable to authenticate") {
		return transport.Authentication
	}

	// NotFound: rclone's own sentinels for the sftp/local backends
	// (fs.ErrorObjectNotFound, fs.ErrorDirNotFound) and, since both backends
	// sometimes let the standard library's own os.ErrNotExist survive
	// unwrapped instead of translating it, that too.
	if errors.Is(err, rclonefs.ErrorObjectNotFound) ||
		errors.Is(err, rclonefs.ErrorDirNotFound) ||
		errors.Is(err, rclonefs.ErrorIsDir) ||
		errors.Is(err, rclonefs.ErrorIsFile) ||
		errors.Is(err, os.ErrNotExist) {
		return transport.NotFound
	}

	// PermissionDenied: same story as NotFound. rclone's local backend
	// translates a permission failure it catches during NewObject/Stat into
	// its own fs.ErrorPermissionDenied, but a permission failure caught
	// later, while actually opening/reading a file for a hash or a copy (the
	// case that actually matters for FR-22, since Stat itself often
	// succeeds against an unreadable object on a POSIX filesystem), comes
	// through as the standard library's os.ErrPermission instead, wrapped
	// but not translated. github.com/pkg/sftp's client does the equivalent
	// normalisation for the sftp backend: an SSH_FX_PERMISSION_DENIED status
	// becomes os.ErrPermission too. Both need checking; relying on rclone's
	// own sentinel alone misses the exact case this category exists for.
	if errors.Is(err, rclonefs.ErrorPermissionDenied) || errors.Is(err, os.ErrPermission) {
		return transport.PermissionDenied
	}

	// UnsupportedCapability: rclone's own "optional feature not implemented"
	// and "this hash is not computable" sentinels, plus adapter.go's own
	// ErrUnsupportedHash for the same fact (see the package comment for why
	// that one is a sentinel rather than a string match).
	if errors.Is(err, rclonefs.ErrorNotImplemented) ||
		errors.Is(err, rclonehash.ErrUnsupported) ||
		errors.Is(err, ErrUnsupportedHash) {
		return transport.UnsupportedCapability
	}

	// Conflict: rclone's own sentinel for "the destination already exists in
	// a way that makes this operation ambiguous". See the package comment:
	// this adapter's current surface has no reachable path to it, but the
	// sentinel is real and cheap to recognize regardless.
	if errors.Is(err, rclonefs.ErrorDirExists) || errors.Is(err, ErrObjectAlreadyPresent) {
		return transport.Conflict
	}

	// IntegrityFailure: see integrityFailurePrefix's doc comment and the
	// package comment for why this is a prefix match rather than a typed
	// value.
	if strings.HasPrefix(err.Error(), integrityFailurePrefix) {
		return transport.IntegrityFailure
	}

	// Transient: deferred entirely to rclone's own fs/fserrors.ShouldRetry,
	// rather than this package maintaining a second, competing opinion
	// about which network conditions are worth retrying. rclone already
	// curates this list (Timeout()/Temporary() checks, io.EOF, a set of
	// known-retriable connection-loss substrings) and keeps it current as
	// part of its own maintenance, which is exactly the kind of judgement an
	// rclone upgrade should be free to improve without this file changing.
	if fserrors.ShouldRetry(err) {
		return transport.Transient
	}

	return transport.Permanent
}

// Wrap classifies err and packages it as a *transport.Error tagged with op,
// the transport operation that produced it (for example "stat" or
// "copy_to_local"). It returns nil for a nil err, so it is safe to call
// unconditionally on a call's returned error.
func Wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return transport.NewError(Classify(err), op, err)
}
