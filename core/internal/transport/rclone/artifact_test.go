package rclone

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// modTimeOnlyObject is an fs.Object that exists to answer exactly one
// question: what does toArtifact do with the modification time a backend
// hands it.
//
// It is a fake rather than a real object on a real backend because the
// case under test is one no backend reachable through a Source produces.
// Both local and sftp always report a time, which is precisely why the
// missing guard sat here unnoticed; the third backend that does not is the
// one this test stands in for. Writing a year-one timestamp onto a real
// file and reading it back would test the filesystem's willingness to
// store it, not this function's handling of it, and would be a different
// answer on every platform the gate runs on.
//
// TestToArtifactCarriesARealModTime below is the positive control that
// keeps this from being the only evidence: it runs the same field through
// a real local backend and a real file.
type modTimeOnlyObject struct {
	remote  string
	size    int64
	modTime time.Time
}

func (o modTimeOnlyObject) Fs() fs.Info                       { return nil }
func (o modTimeOnlyObject) String() string                    { return o.remote }
func (o modTimeOnlyObject) Remote() string                    { return o.remote }
func (o modTimeOnlyObject) ModTime(context.Context) time.Time { return o.modTime }
func (o modTimeOnlyObject) Size() int64                       { return o.size }
func (o modTimeOnlyObject) Storable() bool                    { return true }
func (o modTimeOnlyObject) Hash(context.Context, hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}
func (o modTimeOnlyObject) SetModTime(context.Context, time.Time) error { return nil }
func (o modTimeOnlyObject) Open(context.Context, ...fs.OpenOption) (io.ReadCloser, error) {
	return nil, fs.ErrorNotImplemented
}
func (o modTimeOnlyObject) Update(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) error {
	return fs.ErrorNotImplemented
}
func (o modTimeOnlyObject) Remove(context.Context) error { return fs.ErrorNotImplemented }

var _ fs.Object = modTimeOnlyObject{}

// TestToArtifactReportsNoModTimeAsZero holds RemoteArtifact.ModTime's own
// documented contract: "0 when the backend does not report one".
//
// Before #492 toArtifact called .Unix() unconditionally, so a backend
// reporting no time produced -62135596800, which is not zero, is not a
// time anybody wrote, and is a value retention would read as an artifact
// from the year one. medium.go's toObjectInfo already guarded this on the
// medium side with !t.IsZero(); this is the same guard on the source side.
func TestToArtifactReportsNoModTimeAsZero(t *testing.T) {
	got := toArtifact(modTimeOnlyObject{remote: "exports/nightly.dump", size: 4096})

	if got.ModTime != 0 {
		t.Errorf("toArtifact ModTime = %d, want 0: a backend that reports no modification time must leave the field at the zero RemoteArtifact.ModTime documents, not at time.Time{}.Unix()", got.ModTime)
	}
	// The rest of the artifact still has to arrive. A guard that dropped
	// the object on the floor would satisfy the assertion above and lose
	// the identity the caller actually came for.
	if got.Path != "exports/nightly.dump" {
		t.Errorf("toArtifact Path = %q, want %q", got.Path, "exports/nightly.dump")
	}
	if got.Size != 4096 {
		t.Errorf("toArtifact Size = %d, want 4096", got.Size)
	}
}

// TestToArtifactCarriesARealModTime is the positive control for the guard
// above, and it deliberately does not use the fake.
//
// An absence assertion written against a hand-built object proves nothing
// on its own: a toArtifact that returned a zero ModTime for every object
// would pass it and would quietly destroy FR-16's identity comparison for
// every real backend. So this half runs a real file through the real local
// backend and insists the time survives.
func TestToArtifactCarriesARealModTime(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nightly.dump")
	if err := os.WriteFile(path, []byte("dump-bytes"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	want := time.Date(2026, 3, 15, 8, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, want, want); err != nil {
		t.Fatalf("setting the fixture's mod time: %v", err)
	}

	got, err := New().Stat(context.Background(), transport.Source{ID: "p", Type: "local", Root: root}, "nightly.dump")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got.ModTime != want.Unix() {
		t.Errorf("toArtifact ModTime = %d, want %d: the zero-time guard must not swallow a time the backend did report", got.ModTime, want.Unix())
	}
}
