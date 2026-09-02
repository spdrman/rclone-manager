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
	"os"
	"strings"

	"net"

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

	// The S3 verdicts (EPIC E, FR-28). They come first among the
	// backend-specific checks because an S3 API error is a precise,
	// documented statement about what went wrong, and every check below
	// this one is a broader guess that would happily swallow it: a 403 on
	// a HEAD looks like nothing in particular to fserrors.ShouldRetry, and
	// "the bucket does not exist" would otherwise land in Permanent, which
	// is this classifier's label for "I could not place this at all".
	if code, ok := apiErrorCode(err); ok {
		if category, known := s3Categories[code]; known {
			return category
		}
	}

	// An endpoint that does not resolve is a fact about the configuration,
	// not about the network, when DNS says the name does not exist at all.
	// A lookup that timed out or failed for any other reason is left to
	// fserrors.ShouldRetry below, because that one genuinely can be a blip.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return transport.Configuration
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
	if errors.Is(err, rclonefs.ErrorDirExists) {
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

// apiErrorCode reports the S3 API error code err carries, if any.
//
// # Why this declares its own interface instead of importing the SDK
//
// FR-28 is explicit that no AWS SDK enters this repository, in Go or in
// TypeScript: rclone's s3 backend is the entire S3 implementation, and the
// aws-sdk-go-v2 packages under it are rclone's dependency, upgraded when
// rclone is. Importing smithy-go here to reach its APIError type would put
// an SDK import line in this repository's own go.mod, which is the thing
// that FR forbids, for a value this file can obtain without it.
//
// So it matches on the SHAPE the SDK already exposes. Every S3 API error
// the AWS SDK produces implements ErrorCode() string, and errors.As
// against a locally-declared single-method interface finds it wherever it
// sits in the chain. That is a match by Go type identity, not by parsed
// text, which is exactly what this file's package comment asks for: it
// survives a reworded message, and it breaks visibly (this function stops
// matching, and errors_test.go's table fails) rather than silently if the
// SDK ever renames the method.
//
// The CODES themselves are S3's own documented, wire-level API contract,
// not an implementation detail of any SDK. "NoSuchBucket" is what the
// service returns in the XML body, and every S3-compatible endpoint this
// product can talk to returns the same string, which is why it is safe to
// switch on where an error MESSAGE would not be.
func apiErrorCode(err error) (string, bool) {
	var coder interface{ ErrorCode() string }
	if errors.As(err, &coder) {
		if code := coder.ErrorCode(); code != "" {
			return code, true
		}
	}
	return "", false
}

// s3Categories is FR-28's error-classification table: S3's own API error
// codes mapped into the manager-owned FR-22 vocabulary lifecycle code is
// allowed to switch on.
//
// A code that is not in this table is not classified here at all; it falls
// through to the generic checks below apiErrorCode's call site, and
// ultimately to Permanent. That is deliberate. Guessing at an unlisted
// code would produce a confident wrong answer, and the two answers that
// matter most (is this retryable, is this safe to treat as absence) are
// exactly the two that must never be guessed.
var s3Categories = map[string]transport.Category{
	// The request cannot succeed until a person edits the configuration.
	// The bucket is named in config, and it is not there.
	"NoSuchBucket":      transport.Configuration,
	"InvalidBucketName": transport.Configuration,

	// The caller is not who the endpoint will accept, or is not allowed to
	// do this. S3 has no pre-session handshake, so a wrong key and a
	// too-narrow policy are the same first-request failure; they share a
	// category because there is no earlier moment at which they could have
	// been told apart.
	"AccessDenied":          transport.Authentication,
	"InvalidAccessKeyId":    transport.Authentication,
	"SignatureDoesNotMatch": transport.Authentication,
	"ExpiredToken":          transport.Authentication,
	"InvalidToken":          transport.Authentication,
	"TokenRefreshRequired":  transport.Authentication,
	// Forbidden and Unauthorized are not S3 codes at all, they are what
	// the SDK synthesizes from the HTTP status when the response has no
	// body to read a code out of. A HEAD is exactly that response, and a
	// HEAD is how this adapter stats an object, so this is the code a
	// wrong credential actually produces on the most common call: proved
	// against MinIO in core/tests/miniointegration, and missing from an
	// earlier version of this table that was written from the S3
	// documentation alone.
	"Forbidden":    transport.Authentication,
	"Unauthorized": transport.Authentication,

	// The object is not there. NotFound is what a HEAD returns; NoSuchKey
	// is what a GET returns; both mean the same thing to this product.
	"NoSuchKey": transport.NotFound,
	"NotFound":  transport.NotFound,

	// Throttling and the service's own 5xx family: the request may well
	// succeed if it is made again, which is the whole meaning of
	// Transient and the only category FR-22 lets retry.Do act on.
	"SlowDown":             transport.Transient,
	"Throttling":           transport.Transient,
	"ThrottlingException":  transport.Transient,
	"TooManyRequests":      transport.Transient,
	"RequestLimitExceeded": transport.Transient,
	"RequestTimeout":       transport.Transient,
	"InternalError":        transport.Transient,
	"ServiceUnavailable":   transport.Transient,
}
