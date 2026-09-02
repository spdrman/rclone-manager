// Package artifactstore is where an artifact's durable bytes live, and the
// seam that lets that be somewhere other than the local backup root.
//
// Today there is exactly one implementation, Local, and it is the local
// backup root this product has always written to. Nothing in this package
// moves anything, and retention still has exactly one verb, delete. What
// this package adds is the shape that makes a second implementation
// possible later (issue #334) without rewriting the pipeline around it.
//
// # This interface has no production caller yet, and that is deliberate
//
// Everything below reads like a description of a live seam. It is not one,
// so read this first.
//
// The only production use of this package is the package-level function
// LocalLocator, from exactly two places:
//
//   - internal/lifecycle/transfer.go, in finalPath
//   - internal/retention/prune.go, in pruneFinalPath
//
// Both of those hold a local directory string rather than a Store, so both
// bypass the interface entirely. Store, Local, NewLocal, Kind, KindLocal,
// Stat, Open, Put, Remove, ErrNotPresent and ErrAlreadyPresent have no
// production callers at all.
//
// They are a design fixture: the contract a mover and a second backend get
// built against, landed before either exists so the shape is argued once,
// in review, rather than discovered under a deadline. The conversion #334
// actually needs is therefore deferred rather than done, and it is those
// two call sites: resolving a Store for the backup set and asking it,
// instead of composing a path out of LocalPath. The gain from landing this
// first is that the contract they will convert TO is already written down.
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
// The backup set half of that pair arrives through the constructor rather
// than through every method, because a store's configuration is a fact
// about the store, not about each call. See Store's own doc.
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
// "Never zero" is a claim about power loss, not just about a killed
// process, so it rests on Put being durable and not merely visible. That
// obligation is stated on Put, and Local discharges it by fsyncing the
// directory entry it creates.
//
// Nothing here enforces that order yet, because nothing moves yet. What
// this package does is make the wrong order require adding a method
// rather than merely calling two existing ones in the wrong sequence. The
// same reasoning covers Put, which CAN destroy bytes by itself, so Put
// refuses an occupied locator instead of replacing what is there.
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
// states the OBLIGATION and leaves the proof to the caller: Remove
// deletes exactly the artifact it was asked for and nothing else, and the
// proof that this particular artifact is safe to delete is made before
// the call, by internal/retention, with FR-20's checks unchanged.
//
// # Why there is no List
//
// Enumeration is deliberately absent. The catalog, not a scan, is this
// product's answer to "which artifacts exist and where": config describes
// intent, and a scan can only ever see one backend at a time, so an
// enumerate method is how a second, disagreeing inventory gets built by
// accident.
//
// The obvious need is a reconciler hunting for an orphaned destination
// copy after an interrupted move, and that reconciler does not need one:
// the catalog knows which locator the move was writing, so the question
// is a Stat of one locator, which this interface already answers. If a
// real need for enumeration turns up later, it should arrive with the
// argument for why the catalog cannot answer it.
package artifactstore

import (
	"context"
	"errors"
	"io"
	"time"

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

// ErrAlreadyPresent reports that a store already holds something at the
// locator a Put was asked to write, and refused rather than replacing it.
//
// It is distinct from a plain write failure because the two lead to
// opposite next moves. Already-present is the resumable case this
// package's doc calls self-correcting: confirm by Stat, then finish the
// remove a previous run did not reach. A write failure is a refusal, and
// the origin copy stays exactly where it is.
var ErrAlreadyPresent = errors.New("artifactstore: the store already holds an artifact at this locator")

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
//
// Size and ModTime are pointers and Hash and HashAlg are plain strings,
// which is one rule rather than two spellings of it. An artifact can
// genuinely be zero bytes, and a zero time is indistinguishable from a
// backend that does not report mtime, so those two need a level of
// indirection to say "not reported". A hash and an algorithm name are
// never legitimately empty, so "" already says it.
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
// One Store value serves one backup set, and takes whatever that set's
// storage needs at construction: NewLocal takes the set's configured
// local root, and a second backend takes its bucket, prefix and
// credentials the same way. That is why no method here takes a backup
// set, and it is the answer to "should an implementation hold per-set
// state": yes, that is what the value is for. An implementation that
// wants to assert the set on a given call still can, because
// model.ArtifactID carries its own BackupSetID.
//
// A Store value is expected to be cheap enough to hold one per set. A
// backend with an expensive client should share the client between its
// Store values rather than making the Store itself deployment-scoped.
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
	//
	// It takes no configuration and an implementation must not reach for
	// any. Everything it needs arrived through the constructor, which is
	// what keeps a store out of the question of whether a config field
	// has been through Validate yet. config.BackupSet carries fields with
	// two different lifecycles and nothing on the struct distinguishes
	// them: LocalPath is meaningful straight out of YAML, while ID and
	// ReadOnly are the zero value until Validate has run. A store that is
	// never handed the struct cannot read the wrong half of it.
	//
	// The error is for what an implementation can reject on its own: a
	// store built without the configuration it needs, or an artifact id
	// this store cannot address. It is never for the backend being
	// unreachable, because Locator does not talk to the backend.
	Locator(artifact model.ArtifactID) (string, error)

	// Stat reports what the store holds at locator, or ErrNotPresent.
	Stat(ctx context.Context, locator string) (Stat, error)

	// Open reads the artifact's bytes. The caller closes the reader.
	Open(ctx context.Context, locator string) (io.ReadCloser, error)

	// Put writes r's bytes so they become the artifact at locator.
	//
	// Three obligations, and an implementation that meets only the first
	// breaks the failure model this package's doc describes:
	//
	//   - ATOMIC. A Stat must never observe a partial object under
	//     locator. The obvious streaming implementation, open the
	//     destination and copy into it, satisfies "the bytes end up
	//     there" and violates this, and the mover then confirms a
	//     truncated object and removes the origin on the strength of it.
	//   - DURABLE. Once a Stat would succeed, that must survive the
	//     machine losing power, not just the process exiting.
	//     Confirm-then-remove is only safe if the confirmation outlives
	//     the crash.
	//   - REFUSES AN OCCUPIED LOCATOR. Return ErrAlreadyPresent rather
	//     than replacing what is there. This is the only operation here
	//     that can destroy an artifact's bytes, overwriting one is not
	//     recoverable, and a destination that already holds something
	//     different is a case a person decides, not a library.
	//
	// Nothing in the retention path calls this today. It is here because
	// a seam that can only read and delete cannot be the seam a mover is
	// built on, and discovering that after an adapter exists is worse
	// than deciding it now.
	Put(ctx context.Context, locator string, r io.Reader) error

	// Remove deletes exactly the artifact at locator.
	//
	// It performs NO safety proof of its own, and an adapter author
	// should not add one. The proof that these particular bytes are safe
	// to delete belongs to the caller: internal/retention re-derives
	// every one of FR-20's checks from the artifact's own journal record
	// and the backup set's own configured root immediately before it
	// calls anything that deletes, and the reference implementation,
	// Local.Remove, is a bare os.Remove for exactly that reason. See this
	// package's doc for why those checks are not interface methods.
	//
	// What a store IS obliged to do here is narrow and absolute: delete
	// the one object locator names and nothing else, follow no symlink or
	// other indirection to a different object, and never widen the target
	// (no prefix delete, no recursion, no siblings).
	//
	// Removing something already absent is not an error: the caller's
	// intent, that these bytes not be in this store, is satisfied.
	Remove(ctx context.Context, locator string) error
}
