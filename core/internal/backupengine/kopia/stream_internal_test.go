package kopia

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	kopiafs "github.com/kopia/kopia/fs"
)

// nilStream is the least a StreamSource can be: a modtime and an empty body.
type nilStream struct{}

func (nilStream) ModTime() time.Time { return time.Unix(1700000000, 0).UTC() }

func (nilStream) Open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

// TestStreamingEntryIsNotSeekable is the acceptance criterion that no fake
// Seek was needed, and it has to live inside this package because the thing
// it asserts about is the entry type itself rather than anything the boundary
// exposes.
//
// The entry handed to the engine must satisfy fs.StreamingFile, whose only
// reader accessor is GetReader() io.ReadCloser, and it must NOT satisfy
// fs.File, whose Open returns a reader embedding io.Seeker. Kopia's own
// newDirEntry type-switches `case fs.File, fs.StreamingFile` with fs.File
// FIRST, so an entry that accidentally grew an Open method would silently
// leave the streaming path and be asked for seekability, and nothing would
// say so.
func TestStreamingEntryIsNotSeekable(t *testing.T) {
	t.Parallel()

	entry := newStreamingEntry("payload.bin", nilStream{}, nil)

	if _, ok := any(entry).(kopiafs.StreamingFile); !ok {
		t.Fatalf("entry %T does not satisfy fs.StreamingFile; it is not on the streaming path", entry)
	}

	if _, ok := any(entry).(kopiafs.File); ok {
		t.Errorf("entry %T satisfies fs.File, so the uploader will take the seekable path and demand a Seek", entry)
	}

	if entry.Size() != 0 {
		t.Errorf("entry reports Size()=%d; a stream's length is not known before it is read", entry.Size())
	}

	if entry.LocalFilesystemPath() != "" {
		t.Errorf("entry reports a local path %q; nothing about a stream exists on a local filesystem",
			entry.LocalFilesystemPath())
	}

	rc, err := entry.GetReader(context.Background())
	if err != nil {
		t.Fatalf("GetReader: %v", err)
	}

	defer rc.Close() //nolint:errcheck // nothing to report in a test teardown here.

	if _, ok := rc.(io.Seeker); ok {
		t.Errorf("the reader handed to the uploader (%T) implements io.Seeker; this boundary must not fake seekability", rc)
	}

	if _, ok := rc.(io.ReaderAt); ok {
		t.Errorf("the reader handed to the uploader (%T) implements io.ReaderAt; this boundary must not fake random access", rc)
	}
}

// TestLeafNameIsDerivedOnce pins the one function both halves of a streamed
// snapshot use to name the stored entry. The snapshot writes it and the
// restore looks it up, and the bug this replaces was those two derivations
// disagreeing.
func TestLeafNameIsDerivedOnce(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/db.dump", want: "db.dump"},
		{path: "/runs/2026/db.dump", want: "db.dump"},
		{path: "/runs//2026///db.dump", want: "db.dump"},
		{path: "/runs/2026/", want: "2026"},
	} {
		got, err := leafName(tc.path)
		if err != nil {
			t.Errorf("leafName(%q): %v", tc.path, err)

			continue
		}

		if got != tc.want {
			t.Errorf("leafName(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}

	for _, path := range []string{"/", "//", "/.", "/runs/.."} {
		if got, err := leafName(path); err == nil {
			t.Errorf("leafName(%q) = %q, want a refusal: it names no object", path, got)
		}
	}
}
