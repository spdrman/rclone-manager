package transport

import (
	"errors"
	"fmt"
)

// Category is the manager-owned classification of a transport failure
// (FR-22). Lifecycle code switches on Category, never on the underlying
// error's text or type. That is failure-safety invariant 12 ("Lifecycle
// policy must not depend on parsing rclone log/error strings") and it is why
// this type lives here, in transport, rather than in transport/rclone:
// invariant 13 says rclone API details must not leak outside the adapter,
// and a package lifecycle can import without ever importing rclone is the
// only way to make that leak structurally impossible rather than a
// convention someone has to remember.
//
// The concrete translation from an rclone error to one of these values
// lives in transport/rclone (see ClassifyCtx there). This file only owns the
// vocabulary both sides agree on.
type Category int

// The FR-22 category set. Unclassified is not part of that list; it exists
// so CategoryOf has a zero value to return for an error nobody has
// classified yet, distinct from a real, considered "this is Permanent"
// verdict.
const (
	Unclassified Category = iota
	Transient
	Authentication
	HostVerification
	// KeyPermissions is issue #293's refusal: a locally configured key_file
	// exists but its on-disk mode no longer matches what importSSHKeyInto
	// (core/service/backupsets.go) wrote it with. Like HostVerification and
	// Authentication, this can only ever happen BEFORE a session exists
	// (internal/transport/rclone/ssh.go's sftpConfig decides it while
	// building the connection's own options, before rclone is ever handed
	// them), which is why it belongs beside those two rather than beside
	// PermissionDenied: PermissionDenied is a remote-side failure reachable
	// only with a perfectly good connection (see haltReasonFor's doc in
	// internal/app/halt.go), and this is the opposite of that.
	KeyPermissions
	NotFound
	PermissionDenied
	IntegrityFailure
	Conflict
	UnsupportedCapability
	Permanent
	Cancelled
)

var categoryNames = [...]string{
	Unclassified:          "unclassified",
	Transient:             "transient",
	Authentication:        "authentication",
	HostVerification:      "host_verification",
	KeyPermissions:        "key_permissions",
	NotFound:              "not_found",
	PermissionDenied:      "permission_denied",
	IntegrityFailure:      "integrity_failure",
	Conflict:              "conflict",
	UnsupportedCapability: "unsupported_capability",
	Permanent:             "permanent",
	Cancelled:             "cancelled",
}

// String renders the category name for logs. It is a label, not a contract:
// nothing should parse it back apart, that would just reinvent the string
// inspection this whole type exists to avoid.
func (c Category) String() string {
	if c >= 0 && int(c) < len(categoryNames) {
		return categoryNames[c]
	}
	return fmt.Sprintf("Category(%d)", int(c))
}

// Retryable reports whether FR-22's bounded backoff is ever appropriate for
// this category. Only Transient is: every other category is either a fixed
// property of the request that a retry cannot change (NotFound,
// PermissionDenied, HostVerification, Authentication, KeyPermissions,
// IntegrityFailure, Conflict, UnsupportedCapability), a decision already
// made by the caller (Cancelled), or a case this classifier could not
// place at all (Unclassified, Permanent), which must never be treated as
// safe to retry just because it wasn't recognized.
func (c Category) Retryable() bool {
	return c == Transient
}

// Error is what an adapter returns to lifecycle code once it has classified
// a failure. Category is the only field lifecycle may branch on. Cause is
// kept so logs and diagnostics still see the real underlying error, and so
// errors.Is/errors.As against a caller's own sentinels (context.Canceled,
// for example) keep working through Unwrap, but Cause's text must never
// itself drive a decision outside the adapter that produced it.
type Error struct {
	// Category is the manager-owned classification.
	Category Category
	// Op names the transport operation that failed (e.g. "stat",
	// "copy_to_local"), for logs. It is never meant to be parsed back out.
	Op string
	// Cause is the original error the adapter classified. It may be nil.
	Cause error
}

func (e *Error) Error() string {
	if e.Op != "" {
		return fmt.Sprintf("%s: %s: %v", e.Op, e.Category, e.Cause)
	}
	return fmt.Sprintf("%s: %v", e.Category, e.Cause)
}

// Unwrap exposes Cause to errors.Is and errors.As, so a caller that already
// knows to look for a stdlib sentinel like context.Canceled does not lose
// that ability just because the error passed through classification.
func (e *Error) Unwrap() error { return e.Cause }

// NewError builds a classified Error.
func NewError(category Category, op string, cause error) *Error {
	return &Error{Category: category, Op: op, Cause: cause}
}

// CategoryOf extracts the Category an adapter already assigned to err by
// walking its chain for an *Error. ok is false when err is nil or carries no
// *Error at all, and the Category returned in that case is Unclassified.
//
// A caller that needs to make a go/no-go decision from a bare Category
// should treat Unclassified the same as Permanent: an error the adapter
// never got to classify is not evidence that retrying it is safe.
func CategoryOf(err error) (category Category, ok bool) {
	if err == nil {
		return Unclassified, false
	}
	var te *Error
	if errors.As(err, &te) {
		return te.Category, true
	}
	return Unclassified, false
}
