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
// *transport.Error (for example, one WrapCtx already produced) returns that
// same Category rather than reclassifying, so wrapping twice by accident
// cannot change the answer.
//
// Classify judges the error and nothing else, so it can never return
// Cancelled for a deadline: it has no way to know whose deadline expired.
// ClassifyCtx is the entry point that does, and every call inside this
// package goes through it.
func Classify(err error) transport.Category {
	if err == nil {
		return transport.Unclassified
	}

	if category, ok := transport.CategoryOf(err); ok {
		return category
	}

	// Cancellation is a program decision, not a judgement about the error's
	// cause, so it takes priority over every category below (FR-22:
	// "Cancellation SHALL propagate through Go contexts"). Only something
	// that was explicitly cancelled counts, and context.DeadlineExceeded is
	// deliberately not checked: an expired deadline says nothing about whose
	// deadline it was, and rclone sets several of its own that no caller
	// asked for. ClassifyCtx is where a deadline gets decided, from the
	// caller's context rather than from the error. See issue #388.
	if errors.Is(err, context.Canceled) {
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

// ClassifyCtx is Classify with the caller's own context in hand, and it is
// what everything inside this package uses. Prefer it anywhere a context is
// available, because the context is the only thing that can tell a deadline
// the caller set apart from a deadline rclone set for itself.
//
// rclone builds both of its dials with --contimeout: fs/fshttp's NewDialer
// sets net.Dialer.Timeout from ci.ConnectTimeout, and backend/sftp sets
// ssh.ClientConfig.Timeout from the same value. When one of those fires, the
// *net.OpError underneath usually carries *net.timeoutError, whose Is method
// answers true for context.DeadlineExceeded. A caller's own
// context.WithTimeout expiring inside the same dial produces a byte-identical
// error, down to the "i/o timeout" text, because net.Dialer applies whichever
// deadline comes first and maps both through the same mapErr. So there is no
// shape to tell them apart by, and issue #388 is what happens when you try:
// rclone's own connect timeout came back as transport.Cancelled, which reads
// everywhere as "the operator decided", and retry.DefaultIsTransient will not
// retry it.
//
// Cancelled therefore means exactly one thing here: the context this call was
// given is done. Everything else is classified on the error alone, which puts
// a connect timeout where it belongs, in Transient.
//
// A nil ctx is treated as a caller that never asked for anything, the same as
// context.Background().
func ClassifyCtx(ctx context.Context, err error) transport.Category {
	if err == nil {
		return transport.Unclassified
	}
	if category, ok := transport.CategoryOf(err); ok {
		return category
	}
	if ctx != nil && ctx.Err() != nil {
		return transport.Cancelled
	}
	return Classify(err)
}

// WrapCtx classifies err with ClassifyCtx and packages it as a
// *transport.Error tagged with op, the transport operation that produced it
// (for example "stat" or "copy_to_local"). It returns nil for a nil err, so
// it is safe to call unconditionally on a call's returned error.
//
// ctx is the context the failed call was given. It takes a context rather
// than classifying the error alone because that is the only way rclone's own
// deadlines stay out of transport.Cancelled; see ClassifyCtx and issue #388.
func WrapCtx(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	return transport.NewError(ClassifyCtx(ctx, err), op, err)
}
