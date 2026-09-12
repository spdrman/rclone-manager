// Package backupengine is the manager-owned boundary around whatever produces
// incremental, deduplicated, content-addressed backups.
//
// It exists for the same reason internal/transport exists: the data-plane
// implementation is somebody else's code on somebody else's release cadence,
// and the only way embedding stays cheaper than a fork is if every import of
// it lives in exactly one adapter package. Every import of the embedded
// engine in this repository lives in this package's single adapter
// subpackage. Nothing else may import it, and no type it defines may appear
// in any signature in this file, which is checkable and therefore checked:
// boundary_test.go greps this file for the vendor's name and fails if it
// finds one, because a convention nothing checks is a convention that lasts
// one busy afternoon.
//
// Note what is absent.
//
// There is no IncrementalSnapshot. Incrementality is not a mode the caller
// selects, it is what Snapshot does when the repository already holds a
// previous snapshot of the same Source: the engine finds the predecessor
// itself and reuses whatever content still matches. A second method, or a
// bool on SnapshotRequest, would advertise a choice the caller does not
// actually have and cannot verify, and the honest report of what happened is
// SnapshotInfo.ReusedFiles after the fact, not a flag before it.
//
// There is no Prune, Forget or ApplyRetention. Deciding which snapshot stops
// being protected is retention policy, which this project owns
// (internal/retention), and DeleteSnapshot takes one identity at a time so
// that the decision is always made on our side of this line. Maintain
// reclaims space for content nothing references any more; it never chooses
// what to stop referencing.
//
// There is no engine-shaped streaming type either. A source whose bytes can
// only be read once, forward, is a real case this product has, and it is
// carried here as a capability -- StreamSource plus StreamingRepository --
// stated in this package's own types and io.ReadCloser. The implementation
// of it lives in the adapter with everything else that knows the vendor's
// name, which is why folding the streaming spike in cost this file four
// declarations and no imports.
package backupengine

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrRepositoryExists is returned by CreateRepository when the location
// already holds a repository. Creating over one would orphan every snapshot
// in it, so this is a refusal, not a warning.
var ErrRepositoryExists = errors.New("backupengine: repository already exists")

// ErrRepositoryNotFound is returned by OpenRepository when the location holds
// no repository, as distinct from holding one we failed to unlock.
var ErrRepositoryNotFound = errors.New("backupengine: repository not found")

// ErrPassphrase is returned when a repository exists and the passphrase does
// not open it. Kept distinct from ErrRepositoryNotFound because the operator
// response differs: one is a configuration mistake, the other is a lost key.
var ErrPassphrase = errors.New("backupengine: incorrect repository passphrase")

// ErrSnapshotNotFound is returned when a SnapshotID names nothing, including
// the case where it named something that a previous DeleteSnapshot removed.
var ErrSnapshotNotFound = errors.New("backupengine: snapshot not found")

// RepositoryLocation says where a repository lives and how to unlock it.
//
// Kind is a closed set (LocationLocal is currently its only member) rather
// than a free-form backend URL, because every additional backend is a
// decision with a dependency and a failure mode attached, the way
// internal/transport/rclone treats registered backends.
type RepositoryLocation struct {
	Kind LocationKind

	// Path is the directory holding repository blobs, for LocationLocal.
	Path string

	// ConfigPath is the file this process writes its connection parameters
	// to. It is ours to place, not the engine's to choose, because a
	// process-wide default config location is shared mutable state between
	// unrelated backupd invocations.
	ConfigPath string

	// CachePath is where the engine may keep its index and metadata caches.
	// Empty means no persistent cache, which is slower and always correct.
	CachePath string

	// Passphrase unlocks the repository. It is carried here and nowhere
	// else; nothing in this package logs, formats or persists it.
	Passphrase string
}

// LocationKind names a repository storage backend this boundary speaks.
type LocationKind string

// LocationLocal is a repository in a directory on a locally reachable
// filesystem, which includes an already-mounted network share. It is the only
// kind Phase 0 proves, and the absence of a second constant is deliberate:
// adding one means proving it, not typing it.
const LocationLocal LocationKind = "local"

// Source identifies what gets backed up, in the engine's own namespace.
//
// Host and User are part of the identity, not decoration: they are how the
// repository distinguishes two different machines backing up the same
// pathname into one shared repository, and getting them wrong makes the two
// look like one source whose contents keep changing completely.
type Source struct {
	Host string
	User string
	Path string
}

// SnapshotID is an opaque handle to one stored snapshot.
//
// Opaque means opaque: it is produced by the engine, compared for equality,
// and handed back. Nothing outside the adapter may parse it, and the catalog
// stores it as a string without interpreting it.
type SnapshotID string

// SnapshotRequest asks for one snapshot of one source.
type SnapshotRequest struct {
	Source Source

	// Description is operator-facing text stored with the snapshot. It has
	// no semantics here and nothing branches on it.
	Description string

	// Tags are stored with the snapshot for later selection. Keys and
	// values are ours; the engine only records them.
	Tags map[string]string
}

// SnapshotInfo reports one stored snapshot.
//
// ReusedFiles and NewFiles are the incrementality report, and they are
// counts of files the engine did and did not have to re-read, which is the
// only measure of "was this incremental" available without trusting a flag
// nobody set. Bytes is the logical size of the source tree at snapshot time,
// not the physical cost of storing it: after deduplication the second
// snapshot of a mostly-unchanged tree has the same Bytes as the first and
// costs almost nothing new on disk.
type SnapshotInfo struct {
	ID          SnapshotID
	Source      Source
	Start       time.Time
	End         time.Time
	Description string

	Files       int64
	Directories int64
	Bytes       int64

	ReusedFiles int64
	NewFiles    int64

	// Incomplete is empty for a finished snapshot and otherwise carries the
	// engine's reason. A non-empty value means the snapshot exists but does
	// not represent the whole source, which is a thing retention and restore
	// must be able to see rather than infer.
	Incomplete string
}

// VerifyReport reports what a verification actually read.
//
// Errors is a slice of strings rather than errors because these are the
// engine's per-object findings, plural, and the caller's job is to record
// and surface them, not to branch on their identity.
//
// A nil error means one thing only: the verification completed and found
// nothing wrong. Every other outcome is a non-nil error plus whatever the
// report had managed to record, and the two carry different halves of the
// answer -- the error says the verification did not pass, Errors says what
// was found, and errors.Is against context.Canceled or
// context.DeadlineExceeded is how a caller tells a verification that was
// torn down from a repository that is damaged.
//
// Deciding "did it run" from len(Errors) is the mistake this doc exists to
// forbid. A walk torn down by a cancelled context records its findings and
// returns an error, so a caller counting findings alone reads a cancellation
// as "ran, found damage" when nothing was verified at all -- and then
// reports damage to an operator who has a healthy repository, or worse,
// retries later and calls the second cancellation the same thing.
type VerifyReport struct {
	ObjectsVerified int64
	FilesVerified   int64
	BytesVerified   int64
	Errors          []string
}

// RestoreRequest asks for one snapshot to be written to a local directory.
type RestoreRequest struct {
	TargetPath string

	// Overwrite permits replacing files and directories that already exist
	// under TargetPath. False means a collision is an error, which is the
	// right default for a tool whose whole purpose is not destroying data.
	Overwrite bool

	// SkipOwners skips restoring uid/gid, which is what an unprivileged
	// restore has to do.
	SkipOwners bool
}

// RestoreReport reports what a restore actually wrote.
type RestoreReport struct {
	Files       int64
	Directories int64
	Symlinks    int64
	Bytes       int64
}

// MaintenanceMode selects how much work Maintain does.
type MaintenanceMode string

const (
	// MaintenanceQuick does the cheap, frequent housekeeping.
	MaintenanceQuick MaintenanceMode = "quick"

	// MaintenanceFull includes reclaiming space for unreferenced content,
	// which is the part that can actually delete bytes and therefore the
	// part that needs a deliberate caller.
	MaintenanceFull MaintenanceMode = "full"
)

// MaintenanceReport reports what maintenance did.
type MaintenanceReport struct {
	Mode MaintenanceMode

	// Ran is false when maintenance declined to do anything, which is a
	// normal outcome (nothing was due, or another process held the lock)
	// and not an error.
	Ran bool
}

// Engine creates and opens repositories. It holds no repository state.
type Engine interface {
	// CreateRepository initializes a new repository at the location and
	// leaves it closed. It returns ErrRepositoryExists rather than
	// adopting or overwriting an existing one.
	CreateRepository(ctx context.Context, loc RepositoryLocation) error

	// OpenRepository connects to an existing repository. The caller owns
	// the returned Repository and must Close it.
	OpenRepository(ctx context.Context, loc RepositoryLocation) (Repository, error)
}

// Repository is an open repository, and the only surface lifecycle code is
// allowed to depend on.
//
// It is a handle rather than a set of stateless functions taking a
// RepositoryLocation, unlike transport.Transport, and the difference is not
// stylistic: opening a content-addressed repository loads format blobs and
// builds an index cache, so a stateless surface would pay that cost per call
// and a long-running daemon would spend most of a backup window reopening.
type Repository interface {
	// Snapshot stores the current state of a source. If the repository
	// already holds snapshots of the same Source, this reuses their
	// content; see the package doc on why that is not a parameter.
	Snapshot(ctx context.Context, req SnapshotRequest) (SnapshotInfo, error)

	// ListSnapshots returns snapshots of one source, oldest first.
	ListSnapshots(ctx context.Context, src Source) ([]SnapshotInfo, error)

	// Verify reads a snapshot's content back and reports damage. A nil
	// error means it completed and found nothing; anything else is an
	// error plus a report of what it managed to read. See VerifyReport,
	// which is where that contract is argued.
	Verify(ctx context.Context, id SnapshotID) (VerifyReport, error)

	// Restore writes a snapshot to a local directory.
	Restore(ctx context.Context, id SnapshotID, req RestoreRequest) (RestoreReport, error)

	// DeleteSnapshot removes one snapshot's identity. It does not reclaim
	// space; Maintain does. Deleting an already-deleted snapshot returns
	// ErrSnapshotNotFound rather than succeeding quietly, because the
	// caller asked about a specific thing and deserves to know it was not
	// there.
	DeleteSnapshot(ctx context.Context, id SnapshotID) error

	// Maintain performs repository housekeeping, including reclaiming
	// space for content that DeleteSnapshot orphaned.
	Maintain(ctx context.Context, mode MaintenanceMode) (MaintenanceReport, error)

	// Close releases the repository. Safe to call twice.
	Close(ctx context.Context) error
}

// StreamSource is one object whose bytes arrive once, in order, from
// somewhere this process cannot seek: the source data plane, stated in the
// least a streaming transport can honestly offer.
//
// There is no Size and no Seek, and both absences are a finding rather than
// an omission. A remote stream's length is not known until it ends, its
// bytes arrive once, and the spike behind ADR 0007 proved the engine needs
// neither: an io.ReadCloser is enough. An interface with a Seek on it would
// be an invitation to implement one by reopening the remote, which turns one
// sequential read into an unbounded number of connections and hides that
// behind a method name.
//
// The identity of what is being backed up is not here either. It is
// StreamSnapshotRequest.Source, because a source's name in the repository
// and the bytes of one of its objects are two different concerns, and the
// mismatch between them is exactly the bug that made nested object names
// unrestorable in the spike.
type StreamSource interface {
	// ModTime is the modification time recorded with the snapshot. The
	// zero time means the transport does not report one, and nothing here
	// treats it as content: see StreamSnapshotRequest on why a streamed
	// source is never reused on the strength of it.
	ModTime() time.Time

	// Open starts a new sequential read of the whole object, from byte
	// zero.
	//
	// It is called once per attempt: a retry re-opens rather than resumes,
	// because resuming would require the source to be seekable and this
	// boundary refuses to pretend that it is. The returned reader is
	// closed exactly once, by the engine or by the implementation behind
	// it, whichever gets there first.
	Open(ctx context.Context) (io.ReadCloser, error)
}

// StreamSnapshotRequest asks for one snapshot of one streamed object.
type StreamSnapshotRequest struct {
	// Source identifies the object in the repository. Path is a rooted,
	// slash-separated path in the source's own namespace
	// ("/runs/2026/db.dump"), not a local filesystem path: nothing opens
	// it, and requiring it rooted is what keeps a snapshot's identity from
	// depending on the process working directory.
	Source Source

	// Stream is where the bytes come from. It is required; a request
	// without one is refused rather than producing a snapshot of nothing.
	Stream StreamSource

	// Description is operator-facing text stored with the snapshot.
	Description string

	// Tags are stored with the snapshot for later selection.
	Tags map[string]string

	// MaxAttempts bounds how many times a broken stream is re-read from
	// byte zero. Zero means the engine's default; a negative value is
	// refused rather than quietly meaning "never try", because a caller
	// that computed a negative attempt count has a bug and deserves to
	// hear about it.
	MaxAttempts int
}

// StreamSnapshotInfo reports one streamed snapshot.
//
// It embeds SnapshotInfo and adds the two numbers only a streaming run can
// report: what it actually pushed into storage, and how many times it had to
// open the source to get there.
type StreamSnapshotInfo struct {
	SnapshotInfo

	// UploadedBytes is how many bytes this run pushed into the
	// repository's storage. On a re-snapshot of unchanged content it is
	// near zero while SnapshotInfo.Bytes is the whole object, and that
	// difference is the measurement that says content was reused rather
	// than stored again.
	UploadedBytes int64

	// Attempts is how many times the source had to be opened, so a caller
	// can tell a clean run from one that survived an interruption.
	Attempts int
}

// StreamingRepository is the capability a Repository advertises when it can
// store an object it cannot stat, seek or re-read.
//
// It is a separate interface rather than two more methods on Repository
// because it is a capability and not every engine has to have it: a caller
// type-asserts for it, and an engine that cannot stream says so by not
// satisfying it, which is a compile-time answer instead of a runtime
// ErrUnsupported. Everything it adds is expressed in this package's own
// types plus io.ReadCloser.
type StreamingRepository interface {
	Repository

	// SnapshotStream reads the request's Stream once, straight into the
	// repository, and stores a snapshot only if the read completed. A
	// broken stream is retried by re-opening from byte zero; a cancelled
	// context is not retried, because it is an instruction rather than a
	// failure.
	//
	// A streamed object is never reused on metadata. It has no size to
	// compare, so "unchanged" would mean "same modification time", and a
	// source that rewrites a file while preserving its mtime would have
	// its new content skipped silently and unrecoverably. Every run reads
	// every byte; deduplication happens below, on content.
	SnapshotStream(ctx context.Context, req StreamSnapshotRequest) (StreamSnapshotInfo, error)

	// OpenSnapshotStream reads back what a streamed snapshot stored. The
	// reader is over the repository, not over the original source, and the
	// caller closes it.
	OpenSnapshotStream(ctx context.Context, id SnapshotID) (io.ReadCloser, error)
}
