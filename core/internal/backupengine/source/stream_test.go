package source_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine/source"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// Constructing a stream must not touch the transport, and the WHEN is the
// point rather than the fact: a snapshot that fails before it reaches this
// object should never have opened a connection to it, so there is nothing
// to leak on the failing path.
func TestAnObjectStreamOpensLazilyAndOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	body := patternBytes(4096, 42)
	f.put("runs/2026/db.dump", body, 1_700_000_000)

	when := time.Unix(1_700_000_000, 0).UTC()
	s := source.NewObjectStream(f, fakeTransportSource(), "runs/2026/db.dump", when)

	if f.opens.Load() != 0 {
		t.Fatal("building the stream dialed the source")
	}

	if !s.ModTime().Equal(when) {
		t.Fatalf("ModTime = %s, want %s", s.ModTime(), when)
	}

	rc, err := s.Open(context.Background())
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

	if !bytes.Equal(got, body) {
		t.Fatalf("read %d bytes, want %d", len(got), len(body))
	}

	if f.opens.Load() != 1 {
		t.Fatalf("the object was opened %d times for one read", f.opens.Load())
	}
}

// A source that cannot be opened is an error, not an empty stream. An
// empty stream would snapshot as a perfectly successful backup of
// nothing, which is the worst failure this path has.
func TestAnObjectStreamReportsAnOpenFailure(t *testing.T) {
	t.Parallel()

	want := errors.New("the far host said no")

	s := source.NewObjectStream(
		streamerFunc(func(context.Context, transport.Source, string) (io.ReadCloser, error) {
			return nil, want
		}),
		fakeTransportSource(), "missing.bin", time.Time{})

	rc, err := s.Open(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Open returned %v, want the transport's own error", err)
	}

	if rc != nil {
		t.Fatal("Open returned a reader alongside an error")
	}
}

// The reader is an io.ReadCloser and nothing more. A Seek or a ReadAt on
// it would be an invitation to implement one by reopening the remote,
// which turns one sequential read into an unbounded number of
// connections, and the engine type-switches on exactly these interfaces
// to decide which upload path an entry takes.
func TestAnObjectStreamsReaderIsNotSeekable(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("x.bin", []byte("data"), 1_700_000_000)

	rc, err := source.NewObjectStream(f, fakeTransportSource(), "x.bin", time.Time{}).Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close() //nolint:errcheck // nothing to report here.

	if _, ok := rc.(io.Seeker); ok {
		t.Error("the reader is an io.Seeker")
	}

	if _, ok := rc.(io.ReaderAt); ok {
		t.Error("the reader is an io.ReaderAt")
	}
}
