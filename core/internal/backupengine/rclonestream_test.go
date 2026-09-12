package backupengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/backupdproject/backupd/core/internal/transport"
	"github.com/backupdproject/backupd/core/internal/transport/rclone"
)

// TestRcloneObjectStreamsStraightIntoKopia is the end of the wire #791 asked
// for, with no stand-ins anywhere on it: a real rclone Fs, a real rclone
// Object, the io.ReadCloser its Open returns, this package's streaming
// source, Kopia's streaming file path, and a filesystem repository. The
// bytes exist twice at the end -- on the "remote" and in the repository --
// and nowhere in between.
//
// The backend is local, which is one of the three this binary registers
// (see internal/transport/rclone/backends.go). It is a loopback in the
// sense that matters here: rclone opens it, rclone reads it, and this
// package never learns which backend it was.
func TestRcloneObjectStreamsStraightIntoKopia(t *testing.T) {
	ctx := context.Background()

	remoteDir := t.TempDir()
	payload := patternBytes(8<<20, 0xDEADBEEF)
	wantHash := sha256Hex(payload)

	const remoteName = "photo-archive.tar"
	if err := os.WriteFile(filepath.Join(remoteDir, remoteName), payload, 0o600); err != nil {
		t.Fatalf("seeding the remote: %v", err)
	}

	adapter := rclone.New()
	source := transport.Source{ID: "spike-local", Type: "local", Root: remoteDir}

	art, err := adapter.Stat(ctx, source, remoteName)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if art.Size != int64(len(payload)) {
		t.Fatalf("rclone reports %d bytes; wrote %d", art.Size, len(payload))
	}

	e, root := newTestEngine(t)

	snap, err := e.SnapshotStream(ctx, NewRcloneSource(adapter, source, remoteName, art))
	if err != nil {
		t.Fatalf("SnapshotStream over rclone: %v", err)
	}
	if snap.Bytes != int64(len(payload)) {
		t.Fatalf("snapshot recorded %d bytes; the object is %d", snap.Bytes, len(payload))
	}

	rc, err := e.OpenSnapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("closing restored stream: %v", err)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != wantHash {
		t.Fatalf("restored sha256=%s; the rclone object hashes to %s", got, wantHash)
	}

	// Nothing staged the object. The repository under the test root holds
	// pack blobs; a mirror would be one file of the object's size.
	if biggest, where := largestFile(t, root); biggest >= int64(len(payload)) {
		t.Errorf("a %d-byte file exists at %s, at least the size of the %d-byte object: the stream was staged",
			biggest, where, len(payload))
	}
}

// TestRcloneSourceRefusesAMissingObject keeps the failure honest: a source
// that cannot be opened is an error from SnapshotStream, not an empty
// snapshot that looks like a successful backup of nothing.
func TestRcloneSourceRefusesAMissingObject(t *testing.T) {
	ctx := context.Background()

	e, _ := newTestEngine(t, func(c *Config) { c.MaxAttempts = 1 })

	adapter := rclone.New()
	source := transport.Source{ID: "spike-local", Type: "local", Root: t.TempDir()}

	src := NewRcloneSource(adapter, source, "not-there.bin", transport.RemoteArtifact{Path: "not-there.bin"})

	if _, err := e.SnapshotStream(ctx, src); err == nil {
		t.Fatal("SnapshotStream succeeded for an object that does not exist")
	}

	snaps, err := e.Snapshots(ctx, "not-there.bin")
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("a failed open left %d snapshot(s) behind", len(snaps))
	}
}
