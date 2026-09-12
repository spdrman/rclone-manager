package backupengine

import (
	"context"
	"io"
	"time"

	"github.com/backupdproject/backupd/core/internal/transport"
	"github.com/backupdproject/backupd/core/internal/transport/rclone"
)

// rcloneSource is one file on an rclone-reachable source, seen as a
// StreamSource.
//
// It is four lines of glue and that is the finding, not an accident of
// this spike being small: rclone's Object.Open already returns exactly the
// forward-only io.ReadCloser Kopia's streaming file path already asks for,
// so the two halves meet without an adapter that has to invent anything.
// Nothing here buffers, nothing here seeks, and nothing here keeps a byte
// after the engine has read it.
type rcloneSource struct {
	adapter    *rclone.Adapter
	source     transport.Source
	remotePath string
	name       string
	modTime    time.Time
}

// NewRcloneSource describes one remote file for the engine.
//
// The artifact is what transport.Transport.Stat already returns, so a
// caller that has listed a source has everything needed here and does not
// pay for a second round trip. Its Size is deliberately unused: the engine
// records the bytes it actually read, so a remote that lied about its
// length, or grew while it was being read, produces a correct snapshot
// rather than a truncated one.
func NewRcloneSource(
	adapter *rclone.Adapter,
	source transport.Source,
	remotePath string,
	artifact transport.RemoteArtifact,
) StreamSource {
	name := remotePath
	if artifact.Path != "" {
		name = artifact.Path
	}

	var modTime time.Time
	if artifact.ModTime != 0 {
		modTime = time.Unix(artifact.ModTime, 0).UTC()
	}

	return &rcloneSource{
		adapter:    adapter,
		source:     source,
		remotePath: remotePath,
		name:       name,
		modTime:    modTime,
	}
}

func (s *rcloneSource) Name() string       { return s.name }
func (s *rcloneSource) ModTime() time.Time { return s.modTime }

func (s *rcloneSource) Open(ctx context.Context) (io.ReadCloser, error) {
	//nolint:wrapcheck // the adapter already wraps with its own operation name.
	return s.adapter.OpenSourceStream(ctx, s.source, s.remotePath)
}
