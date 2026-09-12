package rclone_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/backupdproject/backupd/core/internal/transport"
	"github.com/backupdproject/backupd/core/internal/transport/rclone"
)

// StatSource answers about an object's metadata without reading the
// object, and Stat does not, and the difference is the whole reason
// StatSource exists.
//
// The assertion is on the HASH field rather than on a timing, because the
// hash is what the read is for: rclone's local backend advertises SHA-256
// and produces one by reading every byte of the file. A streaming backup
// stats every object again after it has read it, to see whether it moved,
// so a hashing stat there reads a 100 GB source twice - which is the
// staging anti-pattern arriving through a method nobody looked at.
//
// Stat's behaviour is asserted too, as the positive control. Without it
// this test would keep passing if the local backend silently stopped
// advertising a hash, and would then be proving nothing about StatSource
// at all.
func TestStatSourceReportsMetadataWithoutReadingTheObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	body := make([]byte, 1<<20)
	for i := range body {
		body[i] = byte(i)
	}

	if err := os.WriteFile(filepath.Join(dir, "object.bin"), body, 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	adapter := rclone.New()
	src := transport.Source{ID: "stat-source", Type: "local", Root: dir}
	ctx := context.Background()

	hashing, err := adapter.Stat(ctx, src, "object.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if hashing.Hash == "" {
		t.Fatal("the local backend no longer produces a hash from Stat, so this test's control is gone and it proves nothing about StatSource")
	}

	metadata, err := adapter.StatSource(ctx, src, "object.bin")
	if err != nil {
		t.Fatalf("StatSource: %v", err)
	}

	if metadata.Hash != "" || metadata.HashAlg != "" {
		t.Errorf("StatSource produced the hash %q (%s); producing one means it read the whole object", metadata.Hash, metadata.HashAlg)
	}

	if metadata.Size != int64(len(body)) {
		t.Errorf("StatSource reports %d bytes, want %d", metadata.Size, len(body))
	}

	if metadata.ModTime != hashing.ModTime {
		t.Errorf("StatSource reports the modification time %d and Stat reports %d; they describe the same object", metadata.ModTime, hashing.ModTime)
	}

	// Kind is not answered, on purpose: rclone's object model cannot
	// classify a path (NewObject returns an object for a fifo and
	// follows a symlink), and a consumer that needs a kind must get it
	// from the enumerator rather than from a resolution.
	if metadata.Kind != transport.EntryKindUnknown {
		t.Errorf("StatSource claims the kind %q; rclone's object model does not answer that question", metadata.Kind)
	}
}

// A path that names nothing is a not-found rather than an empty artifact,
// so a caller can tell "the file was deleted between the listing and the
// read" from "the file is here and is empty".
func TestStatSourceRefusesAPathThatNamesNothing(t *testing.T) {
	t.Parallel()

	adapter := rclone.New()
	src := transport.Source{ID: "stat-missing", Type: "local", Root: t.TempDir()}

	if _, err := adapter.StatSource(context.Background(), src, "gone.bin"); err == nil {
		t.Fatal("StatSource invented an artifact for a path that names nothing")
	}
}
