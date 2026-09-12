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

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
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

// ErrStorageUnsupported is returned when a storage target is reachable and
// authenticated but does not provide the semantics a content-addressed
// repository needs -- read-after-write on both reads and listings, atomic
// whole-blob writes, range reads, and timestamps that do not run
// backwards.
//
// It exists because "S3-compatible" is a marketing claim, not a
// specification, and the ways a partial implementation fails are exactly
// the ways that cannot be recovered from later: an index blob that is not
// visible in a listing right after it was written does not produce an
// error, it produces a repository that has forgotten some of its content.
// So the gap is found by probing at create time and reported as a
// refusal, rather than discovered as corruption during the first restore
// somebody needed.
var ErrStorageUnsupported = errors.New("backupengine: storage target does not provide the semantics a repository requires")

// RepositoryLocation says which repository is wanted, where its bytes
// live, and where the two secrets that reach it come from.
//
// Kind is a closed set rather than a free-form backend URL, because every
// additional backend is a decision with a dependency and a failure mode
// attached, the way internal/transport/rclone treats registered backends.
//
// # Why a Domain rather than a path
//
// A repository's identity is its Repository Domain
// (model.RepositoryDomain), not the directory or bucket it happens to sit
// in. That is what makes "reopen the same repository after a restart" a
// question with an answer: the process that comes back up has the config
// file, the cache and possibly the mount point in a different state, and
// the only stable thing is the id an operator declared. Deriving the
// storage location FROM the id, rather than carrying both and hoping they
// agree, also removes the failure this package's checkStorageIdentity
// exists to catch at its source.
//
// # Why the secrets are references
//
// Passphrase and S3.Credentials name where a secret comes from and never
// carry one, so a RepositoryLocation is safe to log, to render into an
// error and to keep in a struct for the life of a daemon. The material is
// resolved by the adapter at the moment it opens storage and dropped
// immediately afterwards, which is the shortest lifetime available to a
// library that has to hand a passphrase to somebody else's Open call.
type RepositoryLocation struct {
	Kind LocationKind

	// Domain is the repository's stable identity. It names the encryption,
	// credential, maintenance, deduplication, corruption and
	// administrative boundary every backup set stored here shares; see
	// model.RepositoryDomain, which is where that argument lives and where
	// co-tenancy is decided.
	Domain model.RepositoryDomainID

	// Root is the backup root for LocationLocal: the directory this
	// manager already owns on the machine it runs on. The repository is
	// NOT placed directly in it. It goes under the reserved namespace
	// ReservedLocalDir computes, because a backup root is the directory a
	// NAS deployment exports and an artifact catalog walks, and pack files
	// sitting next to artifacts are pack files something eventually
	// treats as artifacts.
	Root string

	// S3 is the bucket for LocationS3, and is ignored for every other
	// kind.
	S3 S3Storage

	// StateDir is where this process keeps the repository's connection
	// config, its index cache and its maintenance-ownership record: local
	// state about a repository, never repository content.
	//
	// It is ours to place, not the engine's to choose, because a
	// process-wide default config location is shared mutable state
	// between unrelated backupd invocations. It belongs under this
	// manager's private state directory (/var/lib/backupd), never under
	// Root: #298 was filed over exactly that exposure for the SSH key,
	// and a cache directory under an exported backup root is the same
	// mistake with more bytes in it.
	//
	// Empty is accepted only for LocationLocal, where it resolves to the
	// reserved namespace under Root. An S3 repository has nowhere to put
	// local state implicitly, so it must be told.
	StateDir string

	// Passphrase names where the repository passphrase comes from. It is
	// required for every kind: a repository this product creates is
	// always encrypted.
	Passphrase secretref.Ref
}

// LocationKind names a repository storage backend this boundary speaks.
type LocationKind string

const (
	// LocationLocal is a repository under the backup root on a locally
	// reachable filesystem, which includes an already-mounted network
	// share.
	LocationLocal LocationKind = "local"

	// LocationS3 is a repository in an S3 bucket, reached natively.
	//
	// Natively is the load-bearing word. The embedded engine also has a
	// provider that proxies through rclone, and using it would have made
	// every backend rclone speaks available for free; it is refused
	// because a repository is not a file copy. It needs read-after-write
	// listings, atomic writes and stable timestamps from the thing
	// underneath it, an rclone remote provides whatever its own backend
	// provides, and the failure mode of getting that wrong is a
	// repository that silently forgets content. rclone stays on the
	// source side, where a wrong answer is a failed read.
	LocationS3 LocationKind = "s3"
)

// S3Storage is the operator-configured description of an S3 bucket a
// repository lives in.
//
// It is deliberately the same set of facts config.StorageMedium already
// collects for an artifact destination -- endpoint, region, bucket,
// prefix, and a credential REFERENCE -- because an operator who has
// already told this product how to reach a bucket should not have to
// describe it a second time in a second vocabulary for the repository
// that lives in it.
//
// There is no field for "do not verify TLS", and there will not be. A
// knob that disables authentication of the endpoint, on the connection
// carrying every backup this product holds, is not a convenience; a
// private CA is a real situation and RootCA is the answer to it.
type S3Storage struct {
	// Endpoint is the service endpoint as a URL, e.g.
	// https://minio.example:9000, spelled exactly as
	// config.StorageMedium.Endpoint spells it. Empty means AWS's own
	// endpoint for Region.
	//
	// An http:// endpoint is accepted and means what it says: no TLS.
	// That is a real deployment (a MinIO on a trusted LAN segment, a test
	// fixture) and pretending otherwise would only push operators towards
	// disabling verification instead, which is worse.
	Endpoint string

	// Region is the provider region, passed through unexamined for
	// config.StorageMedium.Region's reason: the set of legal regions
	// belongs to the provider and changes without this product being
	// rebuilt.
	Region string

	// Bucket holds the repository. This product never creates a bucket:
	// bucket creation is an account-level act with billing and policy
	// consequences, and an operator who mistyped a name deserves a
	// refusal rather than a second empty bucket.
	Bucket string

	// Prefix is the key namespace inside Bucket, so one bucket can hold
	// more than one repository, or a repository beside something else
	// entirely. Empty puts the repository at the root of the bucket.
	Prefix string

	// Credentials names where this bucket's credentials come from. The
	// material behind it is AWS shared-credentials text, which is what
	// config.MediumCredentials already points at.
	Credentials secretref.Ref

	// RootCA is a PEM certificate bundle to trust in addition to the
	// system roots, for an endpoint behind a private CA. Empty means the
	// system roots, which is the ordinary case.
	RootCA []byte
}

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

// HealthWarningKind names a condition a repository can be in that is not
// a failure and is not fine either.
//
// The set is closed so that a caller can route on one -- raise an alert,
// refuse to start a maintenance window -- without matching prose, and so
// that adding a condition is a deliberate act with a name an operator will
// read.
type HealthWarningKind string

const (
	// HealthWarningClockSkew means this machine's clock and the storage's
	// own idea of the time disagree by more than a repository can safely
	// tolerate.
	//
	// It matters because almost everything a repository does about
	// concurrency and reclamation is expressed in timestamps: a
	// maintenance lock is held until a time, a blob is too recent to
	// garbage-collect until a time, and a snapshot's own start time is
	// what retention later reasons about. A clock an hour fast can make
	// one process believe another's lock has expired while it is still
	// held; a clock an hour slow can make freshly written content look old
	// enough to reclaim. Neither produces an error at the time.
	//
	// This is a WARNING and never a refusal, deliberately. The
	// alternative is a product that stops backing up because an NTP
	// server was unreachable, which trades a risk for a certainty. Fixing
	// it is time synchronisation, which is the operating system's job and
	// not this product's.
	HealthWarningClockSkew HealthWarningKind = "clock_skew"
)

// HealthWarning is one thing worth an operator's attention about a
// repository that is nonetheless working.
type HealthWarning struct {
	// Kind is what was found, from the closed set above.
	Kind HealthWarningKind

	// Detail is the operator-facing sentence: what was measured, what was
	// expected, and what to do. It never carries a credential, a
	// passphrase or any part of either.
	Detail string
}

// HealthReport is what a health check found.
//
// Reachable and Warnings answer two different questions and a caller needs
// both: a repository can be perfectly reachable and have a clock that will
// corrupt its next maintenance window, and it can be unreachable for a
// reason that has nothing wrong with it (a NAS that is asleep).
type HealthReport struct {
	// Reachable is whether the storage answered a read and a write.
	//
	// It is proved rather than assumed: a repository handle stays open
	// across a network partition and every method on it would fail, so
	// "we have a handle" is not evidence and this check does not treat it
	// as any.
	Reachable bool

	// Warnings is everything found that is not a failure, in the order it
	// was checked. Empty means nothing was found, which is the answer an
	// operator wants and the only one they should get when it is true.
	Warnings []HealthWarning
}

// RepositoryStats reports what a repository holds.
//
// The numbers here are the PHYSICAL ones, which is the distinction that
// makes this type worth having beside SnapshotInfo. A snapshot's Bytes is
// the logical size of what was backed up and is the same for the tenth
// snapshot of an unchanged tree as for the first; what an operator needs
// to know is how much storage the repository is actually occupying, which
// only the storage can answer.
type RepositoryStats struct {
	// Sources is how many distinct sources have snapshots here. It is the
	// co-tenancy number: a Repository Domain declared isolated whose
	// repository reports three sources is a boundary that has already
	// been crossed.
	Sources int

	// Snapshots is how many snapshots the repository holds across every
	// source.
	Snapshots int

	// Blobs is how many storage objects the repository occupies, and
	// PhysicalBytes is their total size.
	//
	// Both are read from the storage's own listing rather than from the
	// repository's index, because the question they answer is "what is
	// this costing" and the answer to that is whatever is really there,
	// including blobs an interrupted maintenance left behind.
	Blobs         int
	PhysicalBytes int64
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

	// LookupSnapshot returns one snapshot by its identity, or
	// ErrSnapshotNotFound.
	//
	// It exists beside ListSnapshots because the catalog stores a
	// SnapshotID and later has to ask what became of it, and answering
	// that by listing a source's snapshots and scanning for a match costs
	// a manifest load per snapshot and cannot answer at all for a source
	// whose identity has since changed.
	LookupSnapshot(ctx context.Context, id SnapshotID) (SnapshotInfo, error)

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

	// Health reports whether this repository is usable right now, and
	// what is worth an operator's attention even though it still works.
	//
	// A nil error means the check completed; it does NOT mean everything
	// is fine, because the interesting answers are warnings rather than
	// failures. Read HealthReport.
	Health(ctx context.Context) (HealthReport, error)

	// Stats reports what the repository holds and what it costs.
	//
	// It is separate from Health because the two have different costs and
	// different callers: health is a cheap preflight something runs before
	// every backup, and stats walks the storage's blob listing, which on a
	// bucket with a large repository in it is a real number of requests.
	Stats(ctx context.Context) (RepositoryStats, error)

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
