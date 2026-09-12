package backupengine

import (
	"context"
	"io"
	"time"

	"github.com/backupdproject/backupd/core/internal/transport"
)

// SourceStreamer opens one file on a backup source for a single forward
// read.
//
// It is one method wide because that is all this package needs, and it is an
// interface rather than the rclone adapter itself for the reason ADR 0001
// gives about boundaries generally: a package that names a concrete adapter
// depends on everything that adapter depends on, so the transport's whole
// backend set, its connection accounting and its dependency graph would
// arrive here through one field. internal/transport/rclone's Adapter
// satisfies this by having the method; nothing in this file knows that, and
// a test proves it by satisfying it with eleven lines of its own.
//
// Wiring is the app layer's job: it holds the adapter and passes it in.
type SourceStreamer interface {
	OpenSourceStream(ctx context.Context, src transport.Source, remotePath string) (io.ReadCloser, error)
}

// rcloneSource is one file on a transport-reachable source, seen as a
// StreamSource.
//
// It is four lines of glue and that is the finding, not an accident of the
// spike being small: the transport's OpenSourceStream already returns
// exactly the forward-only io.ReadCloser the engine's streaming path already
// asks for, so the two halves meet without an adapter that has to invent
// anything. Nothing here buffers, nothing here seeks, and nothing here keeps
// a byte after the engine has read it.
type rcloneSource struct {
	streamer   SourceStreamer
	source     transport.Source
	remotePath string
	modTime    time.Time
}

// NewRcloneSource describes one remote file's bytes for the engine.
//
// The artifact is what transport.Transport.Stat already returns, so a caller
// that has listed a source has everything needed here and does not pay for a
// second round trip. Only its ModTime is read: Size is deliberately unused,
// because the engine records the bytes it actually read, so a remote that
// lied about its length, or grew while it was being read, produces a correct
// snapshot rather than a truncated one.
//
// What identifies the object in the repository is not here. That is
// StreamSnapshotRequest.Source, which the caller owns, because a backup
// set's identity is a policy decision and a remote path is not one.
func NewRcloneSource(
	streamer SourceStreamer,
	source transport.Source,
	remotePath string,
	artifact transport.RemoteArtifact,
) StreamSource {
	var modTime time.Time
	if artifact.ModTime != 0 {
		modTime = time.Unix(artifact.ModTime, 0).UTC()
	}

	return &rcloneSource{
		streamer:   streamer,
		source:     source,
		remotePath: remotePath,
		modTime:    modTime,
	}
}

func (s *rcloneSource) ModTime() time.Time { return s.modTime }

func (s *rcloneSource) Open(ctx context.Context) (io.ReadCloser, error) {
	//nolint:wrapcheck // the transport already wraps with its own operation name.
	return s.streamer.OpenSourceStream(ctx, s.source, s.remotePath)
}
