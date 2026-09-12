package source

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// objectStream is one remote object seen as a stream the backup engine
// can chunk: the production replacement for the Phase 0 glue, with the
// three things the prototype did not have.
//
// It projects its modification time through the backend's declared
// precision instead of passing the transport's number along, so a backend
// that keeps no timestamp reports none rather than reporting the epoch.
//
// It tracks the readers it hands out, so a cancelled run closes a read
// that is blocked on a remote socket. A context is not something a
// blocked syscall consults; closing the descriptor from the outside is
// the only thing that unblocks one, which is why the tracking exists at
// all rather than a ctx.Done() check being called cancellation.
//
// And it counts the bytes that actually went past, because "how much did
// this read produce" is one half of the mutation check and the other half
// is the source's own post-read size. A number taken from the listing
// would compare the source with itself.
type objectStream struct {
	streamer   Streamer
	source     transport.Source
	remotePath string
	modTime    time.Time

	mu      sync.Mutex
	read    int64
	opens   int
	readers []io.Closer
	closed  bool
}

var _ backupengine.StreamSource = (*objectStream)(nil)

// NewObjectStream describes one remote object's bytes for the backup
// engine: the production replacement for the Phase 0 glue that lived in
// backupengine as NewRcloneSource.
//
// modTime is the time this backend is entitled to claim, which means it
// has already been through Profile.ModTime. It is a parameter rather
// than a field read off an artifact for exactly that reason: taking a
// transport's raw unix seconds here would let a caller record a
// modification time on a backend whose matrix says it keeps none, which
// is the fabrication the profile exists to prevent. The zero time is a
// legitimate value and means "this backend reports none".
//
// Nothing is opened until the engine asks. A snapshot that fails before
// it reaches this object never dialed it, so there is nothing to leak.
func NewObjectStream(streamer Streamer, src transport.Source, remotePath string, modTime time.Time) backupengine.StreamSource {
	return newObjectStream(streamer, src, remotePath, modTime)
}

func newObjectStream(streamer Streamer, src transport.Source, remotePath string, modTime time.Time) *objectStream {
	return &objectStream{
		streamer:   streamer,
		source:     src,
		remotePath: remotePath,
		modTime:    modTime,
	}
}

// ModTime implements backupengine.StreamSource.
func (s *objectStream) ModTime() time.Time { return s.modTime }

// Open starts a new sequential read of the whole object.
//
// A stream this adapter has already closed refuses to open another
// reader. That is not defensive noise: closeAll runs when a run is torn
// down, and an engine goroutine that had not yet reached its Open would
// otherwise dial the source after the run it belonged to was over.
func (s *objectStream) Open(ctx context.Context) (io.ReadCloser, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return nil, errStreamClosed
	}
	s.opens++
	s.mu.Unlock()

	rc, err := s.streamer.OpenSourceStream(ctx, s.source, s.remotePath)
	if err != nil {
		//nolint:wrapcheck // the transport already wraps with its own operation name.
		return nil, err
	}

	counted := &countingReader{stream: s, inner: rc}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = rc.Close()

		return nil, errStreamClosed
	}
	s.readers = append(s.readers, counted)
	s.mu.Unlock()

	return counted, nil
}

// errStreamClosed is returned by Open after the run that owned this
// stream has been torn down.
var errStreamClosed = errors.New("source: the stream was closed by the run that owned it")

// BytesRead is how many bytes have been read out of this object across
// every reader it handed out.
func (s *objectStream) BytesRead() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.read
}

// Opens is how many times the object was actually dialed. It is what
// makes "one attempt means one open" a measured fact rather than a
// comment: nothing below the adapter may reopen a source behind its back.
func (s *objectStream) Opens() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.opens
}

// closeAll closes every reader this stream handed out and refuses to hand
// out another.
//
// It is idempotent and it is called on every path out of an attempt, not
// only the failing ones. In the ordinary case the engine has already
// closed the reader it was given and this does nothing; what it buys is
// the case nothing inside the engine can reach, which is a Read blocked
// on a socket when the context is cancelled.
func (s *objectStream) closeAll() {
	s.mu.Lock()
	readers := s.readers
	s.readers = nil
	s.closed = true
	s.mu.Unlock()

	for _, r := range readers {
		_ = r.Close()
	}
}

// countingReader is the byte count and the idempotent close.
//
// It is an io.ReadCloser and nothing else - no Seek, no ReaderAt - so the
// engine cannot route it down a seekable path by type assertion. The
// streaming boundary's whole argument is that a remote object is read
// once, forward; a reader that advertised a Seek would be an invitation
// to implement one by reopening the remote, which turns one sequential
// read into an unbounded number of connections.
type countingReader struct {
	stream *objectStream
	inner  io.ReadCloser

	mu     sync.Mutex
	closed bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	if n > 0 {
		c.stream.mu.Lock()
		c.stream.read += int64(n)
		c.stream.mu.Unlock()
	}

	//nolint:wrapcheck // this is a pass-through of the transport's own error, including io.EOF.
	return n, err
}

// Close is idempotent because two owners close it: the engine, in a
// defer, on the ordinary path, and closeAll from the outside when a run
// is cancelled. Whichever gets there first wins and the other is free.
func (c *countingReader) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return nil
	}
	c.closed = true
	c.mu.Unlock()

	//nolint:wrapcheck // the transport's close error is the answer here.
	return c.inner.Close()
}
