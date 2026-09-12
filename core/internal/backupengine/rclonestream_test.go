package backupengine_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// fakeStreamer is the point of SourceStreamer being an interface: eleven
// lines of test code satisfy the dependency that used to be a concrete
// transport adapter, so this package's own tests need no backend, no
// connection accounting and no temp directories.
type fakeStreamer struct {
	body []byte
	err  error

	source transport.Source
	path   string
	opens  int
}

func (f *fakeStreamer) OpenSourceStream(
	_ context.Context,
	src transport.Source,
	remotePath string,
) (io.ReadCloser, error) {
	f.opens++
	f.source = src
	f.path = remotePath

	if f.err != nil {
		return nil, f.err
	}

	return io.NopCloser(bytes.NewReader(f.body)), nil
}

// TestRcloneSourceOpensLazilyThroughTheInterface pins what the glue does and,
// more usefully, when: constructing a source must not touch the transport,
// because a snapshot that fails before it reaches this object should never
// have opened a connection to it.
func TestRcloneSourceOpensLazilyThroughTheInterface(t *testing.T) {
	t.Parallel()

	streamer := &fakeStreamer{body: []byte("the payload")}
	source := transport.Source{ID: "src-1", Type: "local", Root: "/srv/backups"}

	src := backupengine.NewRcloneSource(streamer, source, "runs/2026/db.dump", transport.RemoteArtifact{
		Path:    "runs/2026/db.dump",
		Size:    11,
		ModTime: 1700000000,
	})

	if streamer.opens != 0 {
		t.Errorf("constructing the source opened the transport %d time(s); opening is GetReader's job", streamer.opens)
	}

	if want := time.Unix(1700000000, 0).UTC(); !src.ModTime().Equal(want) {
		t.Errorf("ModTime() = %v, want %v from the artifact", src.ModTime(), want)
	}

	rc, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if string(got) != "the payload" {
		t.Errorf("read %q, want the streamer's body", got)
	}

	if streamer.opens != 1 || streamer.path != "runs/2026/db.dump" || streamer.source.ID != "src-1" {
		t.Errorf("the streamer saw %d open(s) of %q on source %q; want one open of the requested path and source",
			streamer.opens, streamer.path, streamer.source.ID)
	}
}

// TestRcloneSourceReportsAnOpenFailure keeps the failure honest: a source
// that cannot be opened is an error, not an empty stream that would snapshot
// as a successful backup of nothing.
func TestRcloneSourceReportsAnOpenFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("no such object")
	streamer := &fakeStreamer{err: boom}

	src := backupengine.NewRcloneSource(streamer, transport.Source{ID: "src-1"}, "missing.bin",
		transport.RemoteArtifact{})

	rc, err := src.Open(context.Background())
	if err == nil {
		_ = rc.Close()

		t.Fatal("Open succeeded on a source the transport refused")
	}

	if !errors.Is(err, boom) {
		t.Errorf("Open returned %v; want the transport's own error", err)
	}

	// A zero artifact means the backend reported no modification time, and
	// the zero time is how that is carried: inventing time.Now() here would
	// make every snapshot of that source look freshly modified.
	if !src.ModTime().IsZero() {
		t.Errorf("ModTime() = %v for an artifact that reported none, want the zero time", src.ModTime())
	}
}
