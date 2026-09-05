package transport

import (
	"errors"
	"fmt"
)

// This file is the vocabulary half of FR-22's error classification, and it
// is deliberately the half that knows nothing about rclone.
//
// Classifying a failure is two jobs: deciding what an error IS, in words
// this project owns, and working out which of those words an
// rclone-shaped error deserves. Only the first is here. The second lives
// in transport/rclone, beside the sentinels it matches on, and the split
// is what turns failure-safety invariant 13 from a convention somebody has
// to remember into a fact about the import graph: internal/lifecycle
// imports this package to read a Category and has no path from here to the
// adapter that produced it, so there is no line through which an rclone
// type could arrive.
//
// The thing to know before adding anything below: a Category is not a
// label, it is a branch. Retryable is written against this set, so is
// internal/app/halt.go's haltReasonFor, so is every consumer that decides
// whether a failure stops a cycle. A new value is a decision about what
// lifecycle policy DOES, and every one of those switches has to be asked
// about it. That is why ErrCredentialsUnavailable further down is a
// sentinel wrapped into a cause chain rather than a fourteenth category:
// it needed to be distinguishable, not decided upon.

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
	// Configuration is EPIC E's addition (FR-28): the request cannot
	// succeed as configured, and nothing about credentials, the network or
	// the object itself is the reason. A bucket that does not exist, and an
	// endpoint that cannot be resolved to a service at all, are the two
	// shapes the s3 backend produces.
	//
	// It sits apart from Authentication and from Permanent on purpose.
	// Authentication says the caller is not who the endpoint wants;
	// Configuration says the caller named a place that is not there, which
	// a different credential does not fix and a retry does not either. And
	// Permanent is this classifier's "I could not place this at all", which
	// is exactly what an operator cannot act on: NoSuchBucket landing there
	// would tell someone their backup failed permanently without telling
	// them the one line of config to change.
	Configuration
	IntegrityFailure
	Conflict
	UnsupportedCapability
	Permanent
	Cancelled
)

// categoryNames is written as an indexed literal, keyed BY the constants
// above rather than listed in their declaration order. That is not style:
// a positional list silently renames every category below the point where
// somebody inserts one, and the categories are inserted in the middle
// (Configuration went in between PermissionDenied and IntegrityFailure
// when EPIC E added it). Keying by the constant makes an insertion a
// no-op for every other name, and leaves a new one rendering as "" rather
// than as its neighbour's word, which is the failure that is at least
// visible.
//
// The names are snake_case because they are read by machines as well as
// people: they land in log lines and in operator-facing surfaces beside
// the rest of this project's machine-readable values.
var categoryNames = [...]string{
	Unclassified:          "unclassified",
	Transient:             "transient",
	Authentication:        "authentication",
	HostVerification:      "host_verification",
	KeyPermissions:        "key_permissions",
	NotFound:              "not_found",
	PermissionDenied:      "permission_denied",
	Configuration:         "configuration",
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
// made by the caller (Cancelled), a fact about the configuration that
// only a person can change (Configuration), or a case this classifier
// could not place at all (Unclassified, Permanent), which must never be
// treated as safe to retry just because it wasn't recognized.
func (c Category) Retryable() bool {
	return c == Transient
}

// ErrCredentialsUnavailable marks a Configuration failure that happened
// while OBTAINING a medium's credentials, before anything was sent
// anywhere (EPIC E, FR-33; issue #443).
//
// It exists because the medium preflight has to tell two questions apart
// that share the Configuration category and cannot otherwise be separated:
// "this manager could not get hold of the credential this medium declares"
// and "this manager got the credential and the endpoint says the bucket
// named here is not there". Both are a person's job to fix and they are
// different jobs, so an operator has to be told which one.
//
// It is a sentinel wrapped into the Cause chain rather than a new
// Category, and rather than something read off Error.Op, because Op is
// documented as being for logs and never meant to be parsed back out, and
// a new Category would have to answer Retryable, halt classification and
// every switch that already covers the set. errors.Is against this is a
// structural question with one answer.
//
// It never carries the credential, or the name of where the credential
// came from: it marks the SHAPE of a failure and nothing else, which is
// the same discipline the adapter that produces it already follows.
var ErrCredentialsUnavailable = errors.New("the medium's credentials could not be obtained")

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

// Error renders the operation, the category and the cause. Op is dropped
// when it is empty rather than printed as an empty field, because a
// classified error built without one (retry.Do's cancellation, for
// instance) still has to read as a sentence.
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
