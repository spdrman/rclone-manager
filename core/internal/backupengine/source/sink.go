package source

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/sourceconsistency"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// Streamer opens one object on a backup SOURCE for a single forward read.
//
// It is one method wide because that is all this package needs, and it is
// an interface rather than the rclone adapter itself for the reason ADR
// 0001 gives about boundaries: a package naming a concrete adapter
// depends on everything that adapter depends on, so the transport's whole
// backend set, its connection accounting and its dependency graph would
// arrive here through one field. internal/transport/rclone's Adapter
// satisfies this by having the method, and nothing in this file knows
// that.
type Streamer interface {
	OpenSourceStream(ctx context.Context, src transport.Source, remotePath string) (io.ReadCloser, error)
}

// Stater answers what the source says about one object, from its
// METADATA alone. It is the other half of the mutation check: the
// enumeration's answer is the "before", and this is the "after".
//
// The method is StatSource rather than Stat, and the distinct name is
// load-bearing. transport.Transport.Stat computes a content hash where
// the backend advertises one, and rclone's local backend advertises one
// by reading the whole file - so wiring that method here would make
// every post-read check re-read the object it had just streamed, and
// double the I/O of every backup on the quietest possible path. A
// separate name means the wrong one cannot be passed by accident: it
// does not satisfy the interface.
type Stater interface {
	StatSource(ctx context.Context, src transport.Source, remotePath string) (transport.RemoteArtifact, error)
}

// LinkReader is the capability a source has when it can report a symbolic
// link's target without following it. It is optional, and its absence is
// what makes SymlinkPreserve a refusal rather than a guess.
type LinkReader interface {
	ReadSourceLink(ctx context.Context, src transport.Source, remotePath string) (string, error)
}

// Object is one source object, prepared and ready to be stored: a safe
// path, the metadata the backend actually supports, and a stream of its
// bytes that has not been opened yet.
type Object struct {
	// Path is the safe, root-relative, slash-separated source path. It
	// has been through SafeRelPath; nothing downstream needs to check it
	// again.
	Path string

	// Kind is what the source said is here. A Sink only ever sees
	// KindRegular and, under SymlinkPreserve, KindSymlink; the adapter
	// applies its policies before anything reaches a sink.
	Kind sourceconsistency.Kind

	// Size is what the listing reported, and is a hint rather than a
	// promise: it is zero on a backend whose matrix does not declare
	// stable sizes, and it may be stale on any backend, which is the
	// premise of the whole mutation check. Nothing may truncate a read
	// to it.
	Size int64

	// ModTime is the modification time this backend is entitled to
	// claim, already projected through its declared precision. The zero
	// time means the backend keeps none; it is not the epoch.
	ModTime time.Time

	// Stream opens the bytes, once, forward.
	Stream backupengine.StreamSource

	// Attempt is which read of this object this is, from one. A sink
	// that logs or meters can tell a retry from a first pass.
	Attempt int
}

// Stored is what a sink reports after taking one object.
type Stored struct {
	// ID is the sink's handle for what it stored, opaque here. It is
	// what Discard is given when a read turns out to have been torn.
	ID string

	// Bytes is how many bytes the sink actually read from the stream.
	// This is the number the mutation check compares against the
	// source's own post-read size, so a sink that reports what it was
	// told rather than what it read defeats the check.
	Bytes int64

	// UploadedBytes is how much new data reached storage, which on a
	// re-run of unchanged content is near zero while Bytes is the whole
	// object.
	UploadedBytes int64
}

// Sink is where a prepared object's bytes go.
//
// It exists so that this package owns "read the source safely" and
// nothing else. What a stored object becomes - one snapshot per object
// today, an entry in a tree manifest when the snapshot lifecycle lands -
// is the sink's decision, and swapping it must not require re-proving
// path safety, cancellation or mutation detection.
type Sink interface {
	// Store reads obj.Stream to its end and returns what it stored. It
	// must not retry: the retry bound belongs to the adapter, and a sink
	// that retries multiplies it.
	Store(ctx context.Context, obj Object) (Stored, error)

	// Discard removes something Store returned, because the read that
	// produced it turned out to have been torn. A sink that cannot
	// remove what it stored says so with an error, and the adapter
	// reports the object as incomplete AND names the artifact that has
	// to be dealt with, rather than leaving a torn read behind silently.
	Discard(ctx context.Context, id string) error
}

// RepositorySink stores each object as one streamed snapshot in a backup
// repository.
//
// One snapshot per object is what the streaming boundary offers today
// (backupengine.StreamingRepository takes one stream and returns one
// snapshot id), and it is deliberately the sink's business rather than
// the adapter's: gathering a source's objects into a single tree manifest
// is snapshot-lifecycle work, and when it lands it lands here, behind
// this interface, without touching a line of the reading path.
type RepositorySink struct {
	// Repo is the open repository.
	Repo backupengine.StreamingRepository

	// Source is the identity the snapshots are stored under. Its Path is
	// the ROOT of the backup set in the repository's namespace; each
	// object is stored under it, so a source's objects list together and
	// two machines backing up the same pathname stay distinct.
	Source backupengine.Source

	// Description and Tags are recorded with every snapshot this sink
	// writes.
	Description string
	Tags        map[string]string
}

var _ Sink = RepositorySink{}

// Store writes one object as one streamed snapshot.
//
// MaxAttempts is pinned at 1, and that single field is this package's
// retry-boundary contract made structural. The engine will re-open a
// broken stream if asked; the adapter also retries; the transport's own
// backend retries underneath both. Three bounds of three multiply to
// twenty-seven reads of an object that is never going to be readable,
// with a backup window spent on one file and a report that says
// "attempted 3". The adapter is the one authority, so everything below it
// is asked for exactly one attempt.
func (s RepositorySink) Store(ctx context.Context, obj Object) (Stored, error) {
	src := s.Source
	src.Path = repositoryPath(s.Source.Path, obj.Path)

	info, err := s.Repo.SnapshotStream(ctx, backupengine.StreamSnapshotRequest{
		Source:      src,
		Stream:      obj.Stream,
		Description: s.Description,
		Tags:        s.Tags,
		MaxAttempts: 1,
	})
	if err != nil {
		return Stored{}, fmt.Errorf("storing %q: %w", obj.Path, err)
	}

	return Stored{
		ID:            string(info.ID),
		Bytes:         info.Bytes,
		UploadedBytes: info.UploadedBytes,
	}, nil
}

// Discard removes a snapshot written from a read that turned out to be
// torn. It is the half of "never store a torn file as verified" that a
// streaming path needs: a stream is read once, so the tear is only
// visible after the bytes have already been stored, and the honest
// response is to take the restore point away again.
func (s RepositorySink) Discard(ctx context.Context, id string) error {
	if err := s.Repo.DeleteSnapshot(ctx, backupengine.SnapshotID(id)); err != nil {
		return fmt.Errorf("discarding the torn snapshot %q: %w", id, err)
	}

	return nil
}

// repositoryPath joins a source object's path onto the backup set's root
// in the repository's namespace.
//
// It is slash-based and rooted, because that is what the engine requires
// of a streamed object's identity, and because these are remote object
// paths: splitting them with the host platform's separator would make a
// path stored by one build unfindable by another.
func repositoryPath(root, object string) string {
	root = trimSlash(root)
	object = trimSlash(object)

	if root == "" {
		return "/" + object
	}

	return "/" + root + "/" + object
}

func trimSlash(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}

	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}

	return p
}
