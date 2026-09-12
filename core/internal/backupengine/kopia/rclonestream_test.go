package kopia_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	src "github.com/backupdproject/backupd/core/internal/backupengine/source"
	"github.com/backupdproject/backupd/core/internal/transport"
	"github.com/backupdproject/backupd/core/internal/transport/rclone"
)

// TestRcloneObjectStreamsStraightIntoTheRepository is the whole wire, with no
// stand-ins anywhere on it: a real rclone Fs, a real rclone Object, the
// io.ReadCloser its Open returns, the boundary's streaming source, the
// adapter's streaming entry, and a filesystem repository. The bytes exist
// twice at the end -- on the "remote" and in the repository -- and nowhere in
// between.
//
// The backend is local, which is one of the three this binary registers (see
// internal/transport/rclone/backends.go). It is a loopback in the sense that
// matters here: rclone opens it, rclone reads it, and nothing above the
// transport ever learns which backend it was.
func TestRcloneObjectStreamsStraightIntoTheRepository(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	remoteDir := t.TempDir()
	payload := patternBytes(8<<20, 0xDEADBEEF)

	const remoteName = "archive/2026/photo-archive.tar"

	remoteFile := filepath.Join(remoteDir, filepath.FromSlash(remoteName))

	if err := os.MkdirAll(filepath.Dir(remoteFile), 0o750); err != nil {
		t.Fatalf("seeding the remote: %v", err)
	}

	if err := os.WriteFile(remoteFile, payload, 0o600); err != nil {
		t.Fatalf("seeding the remote: %v", err)
	}

	adapter := rclone.New()
	transportSource := transport.Source{ID: "spike-local", Type: "local", Root: remoteDir}

	art, err := adapter.StatSource(ctx, transportSource, remoteName)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if art.Size != int64(len(payload)) {
		t.Fatalf("rclone reports %d bytes; wrote %d", art.Size, len(payload))
	}

	rep, root := newStreamingRepository(t)

	// The production source adapter's stream, not a spike's glue: the
	// modification time is the one the backend's capability matrix
	// entitles it to claim, which is what Profile.ModTime answers.
	profile, err := src.ProfileFor(transportSource.Type)
	if err != nil {
		t.Fatalf("ProfileFor: %v", err)
	}

	req := streamRequest("/"+remoteName, src.NewObjectStream(adapter, transportSource, remoteName, profile.ModTime(art.ModTime)))

	snap, err := rep.SnapshotStream(ctx, req)
	if err != nil {
		t.Fatalf("SnapshotStream over rclone: %v", err)
	}

	if snap.Bytes != int64(len(payload)) {
		t.Fatalf("snapshot recorded %d bytes; the object is %d", snap.Bytes, len(payload))
	}

	if got, n := readStream(t, rep, snap.ID); got != sha256Hex(payload) || n != int64(len(payload)) {
		t.Fatalf("restored %d bytes sha256=%s; the rclone object is %d bytes sha256=%s",
			n, got, len(payload), sha256Hex(payload))
	}

	// Nothing staged the object: the only place its bytes exist locally
	// besides the remote itself is inside the repository, in pack blobs
	// written as they streamed past.
	assertNothingStaged(t, root, filepath.Join(root, "repo"))
}
