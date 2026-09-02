// Package artifactstore is where an artifact's durable bytes live, and the
// seam that lets that be somewhere other than the local backup root.
//
// Today there is exactly one implementation, Local, and it is the local
// backup root this product has always written to. Nothing in this package
// moves anything, and retention still has exactly one verb, delete. What
// this package adds is the shape that makes a second implementation
// possible later (issue #334) without rewriting the pipeline around it.
//
// # Why the unit is an artifact and not a file
//
// Every operation here is addressed by (backup set, artifact), never by a
// path a caller composed. A path is a local-filesystem detail: an object
// store has keys, a tape has positions, and neither has a parent
// directory or a symlink. Handing this package a path would mean every
// caller had already decided the artifact lives on a filesystem, which is
// the assumption #334 exists to remove. Locator goes the other way: the
// store is asked where the artifact is, and returns a string only that
// store knows how to interpret.
//
// # Why there is no Move
//
// This is the load-bearing decision in this package, and it is a decision
// about failure rather than about convenience.
//
// A move is a put followed by a remove. Offering Move as one primitive
// would push the ordering of those two steps down into each
// implementation, where it is invisible to review and where every
// adapter author gets to choose it independently. One of those choices,
// remove-then-put, loses the artifact outright if the process dies in
// between, and a backup product that loses the backup has failed at the
// only thing it does. The ordering is too important to be an
// implementation detail repeated per backend.
//
// So this package deliberately offers only the pieces: Put, Stat, Open
// and Remove. A mover, when one is written, composes them in one place,
// in one auditable order: put the bytes at the destination, confirm by
// Stat that the destination holds them, and only then Remove at the
// origin. That order is what makes the failure model answerable, and the
// answer is:
//
//   - The ORIGIN copy is the one guaranteed intact. It is not removed
//     until the destination copy has been independently confirmed.
//   - A move interrupted at any point therefore leaves either one copy
//     (at the origin) or two copies (origin and destination). It never
//     leaves zero, and it never leaves only an unconfirmed one.
//   - Two copies is a wasteful outcome, not a dangerous one, and it is
//     self-correcting: the next run finds the destination copy already
//     present and proceeds to the remove it did not reach.
//
// Nothing here enforces that order yet, because nothing moves yet. What
// this package does is make the wrong order require adding a method
// rather than merely calling two existing ones in the wrong sequence.
//
// # Why safety proofs are not in the interface
//
// FR-20's six checks before a local delete (canonicalize the path, prove
// containment under the configured root, refuse a symlink, refuse a
// .partial, confirm no tier selects it, confirm it is not
// last-known-good) are local-filesystem semantics. Containment and
// symlinks mean nothing to an object store, and a path-traversal check
// against a key namespace is not the same question wearing different
// clothes.
//
// Hoisting them into this interface would therefore produce methods a
// non-local adapter could only stub, and a stubbed safety check is worse
// than an absent one because it reads as satisfied. So the interface
// states the OBLIGATION and leaves the proof to the implementation:
// Remove deletes exactly the artifact it was asked for and nothing else,
// or it refuses and says which check refused it. Local discharges that
// obligation with FR-20's checks, unchanged, in internal/retention.
package artifactstore

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// ErrNotPresent reports that a store does not hold the artifact asked
// about. It is a distinct error rather than a zero Stat because "the
// store answered, and the artifact is not there" and "the store could not
// be reached to ask" must never collapse into one another: the first is a
// fact about the artifact, the second is a fact about the backend, and a
// mover that confuses them can delete an origin copy on the strength of a
// network failure.
var ErrNotPresent = errors.New("artifactstore: the store does not hold this artifact")

// Kind names a storage backend. It is recorded alongside a locator so a
// locator is never interpreted by the wrong store, and it is what an
// operator sees when asking where an artifact lives.
type Kind string

// KindLocal is the local backup root: the only implementation today, and
// the one every existing configuration uses without naming it.
const KindLocal Kind = "local"

// Stat is what a store reports about bytes it holds. Every field is
// optional for the same reason state.RemoteIdentity's are: backends do
// not all report every attribute, and a zero value must not be read as an
// authoritative zero.
type Stat struct {
	Size    *int64
	ModTime *time.Time

	// Hash and HashAlg are populated only when the store can report a
	// content hash it did not have to read the whole object to compute.
	// A store that would have to stream the bytes to answer leaves these
	// empty rather than doing so behind a caller's back.
	Hash    string
	HashAlg string
}

// Store holds artifacts' durable bytes for one backup set.
//
// Implementations are responsible for their own safety invariants; see
// this package's doc for why those are not interface methods.
type Store interface {
	// Kind names this backend.
	Kind() Kind

	// Locator returns the backend-specific string identifying where this
	// artifact's bytes belong in this store. It is a pure computation: it
	// does not consult the backend and does not report whether anything
	// is actually there. Stat answers that.
	Locator(bs config.BackupSet, artifact model.ArtifactID) (string, error)

	// Stat reports what the store holds at locator, or ErrNotPresent.
	Stat(ctx context.Context, locator string) (Stat, error)

	// Open reads the artifact's bytes. The caller closes the reader.
	Open(ctx context.Context, locator string) (io.ReadCloser, error)

	// Put writes bytes to locator durably enough that a subsequent Stat
	// from a different process would find them.
	//
	// Nothing in the retention path calls this today. It is here because
	// a seam that can only read and delete cannot be the seam a mover is
	// built on, and discovering that after an adapter exists is worse
	// than deciding it now.
	Put(ctx context.Context, locator string, r io.Reader) error

	// Remove deletes exactly the artifact at locator, or refuses and
	// returns an error naming the check that refused it. Removing
	// something that is already absent is not an error: the caller's
	// intent, that these bytes not be in this store, is satisfied.
	Remove(ctx context.Context, locator string) error
}
