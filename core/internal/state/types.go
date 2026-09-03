package state

import (
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// RemoteIdentity is the remote object identity captured at discovery (FR-16),
// so it can be compared against the remote object's identity again
// immediately before deletion elsewhere. Backends do not all report every
// attribute (a local filesystem backend, for example, may have no
// backend-specific stable id at all), so every field here is optional: a nil
// Size/ModTime or an empty Hash/HashAlg/BackendID means the backend did not
// supply that attribute, not that the value is actually zero or empty.
type RemoteIdentity struct {
	Size      *int64
	ModTime   *time.Time
	Hash      string
	HashAlg   string
	BackendID string
}

// TransferResult is what the copy step actually did (FR-11, FR-13).
type TransferResult struct {
	BytesTransferred int64
	Checksummed      bool
}

// HashUpdate carries a locally computed hash, typically attached to the
// VERIFIED transition (FR-13).
type HashUpdate struct {
	Hash string
	Alg  string
}

// ValidationUpdate carries the result of an optional application validator,
// typically attached to the VERIFIED transition (FR-13).
type ValidationUpdate struct {
	Passed bool
	Detail string
}

// RetryUpdate carries retry bookkeeping. FR-22 owns retry policy (backoff,
// what counts as retryable); this only records what that policy decided.
type RetryUpdate struct {
	Count       int
	LastError   string
	NextAttempt *time.Time
}

// DeletionUpdate carries the outcome of the explicit, manager-controlled
// remote delete (FR-15), typically attached to the COMPLETE transition.
type DeletionUpdate struct {
	DeletedAt *time.Time
	Error     string
}

// RetentionUpdate carries a GFS retention classification. FR-18 owns the
// policy that computes it; this only records the verdict.
type RetentionUpdate struct {
	Tier      string
	ExpiresAt *time.Time
}

// Record is one artifact's full journal row: everything FR-9 requires this
// journal to persist for a single artifact.
type Record struct {
	Artifact   model.ArtifactID
	RemotePath string
	LocalPath  string

	// State holds whatever FR-10 lifecycle state string the state-machine
	// package last recorded. It is a plain string, not a named type owned
	// by this package: the FR-10 state machine (a separate component) owns
	// that vocabulary, and this package only stores what it produces.
	State string

	DiscoveredAt time.Time
	UpdatedAt    time.Time

	Remote RemoteIdentity

	Transfer *TransferResult

	LocalHash        string
	LocalHashAlg     string
	ValidationPassed *bool
	ValidationDetail string

	RetryCount  int
	LastError   string
	NextRetryAt *time.Time

	RemoteDeletedAt   *time.Time
	RemoteDeleteError string

	RetentionTier      string
	RetentionExpiresAt *time.Time

	// Placements is where this artifact's durable copies actually are
	// (EPIC E, FR-29), one entry per copy, ordered by medium.
	//
	// It is empty for an artifact that has no durable copy yet, which is a
	// real state and not a gap: a DISCOVERED artifact has zero copies. It
	// is also empty on a Record built by hand rather than read from the
	// journal, which is how most of this repository's tests build one, and
	// ReadableLocalPath's fallback is what keeps those honest.
	//
	// LocalPath keeps meaning exactly what it always meant, the ingestion
	// landing path. What changed is that the callers asking "can I read
	// this artifact off disk" ask ReadableLocalPath instead of assuming
	// that field names a readable file.
	Placements []Placement
}
