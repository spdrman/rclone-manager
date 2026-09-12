package backupengine

import (
	"context"
	"io"
	"os"
	"sync"
	"time"

	kopiafs "github.com/kopia/kopia/fs"
)

// StreamSource is one remote file to be backed up, described by the little
// a streaming transport can honestly say about it.
//
// There is no Size and no Seek, and both absences are the point. A remote
// stream's length is not known until it ends, and its bytes arrive once, in
// order. Kopia's streaming file path asks for exactly this much, so this is
// exactly as much as the boundary carries.
type StreamSource interface {
	// Name is the file name recorded in the snapshot.
	Name() string

	// ModTime is the modification time recorded in the snapshot.
	ModTime() time.Time

	// Open starts a new sequential read of the whole file, from byte zero.
	//
	// It is called once per attempt: a retry re-opens rather than resumes,
	// because resuming would require the remote to be seekable and this
	// engine refuses to pretend that it is. The returned reader is closed
	// exactly once, by the engine or by Kopia, whichever gets there first.
	Open(ctx context.Context) (io.ReadCloser, error)
}

// defaultPermissions is what a streamed entry records, since a stream has
// no mode of its own. It matches virtualfs's own choice for the same
// reason: a snapshot entry needs some permission bits and inventing
// narrower ones would only look like information.
const defaultPermissions os.FileMode = 0o777

// streamingEntry is the adapter: a StreamSource seen as a Kopia
// fs.StreamingFile.
//
// It implements fs.Entry (os.FileInfo plus Owner/Device/LocalFilesystemPath/
// Close) and GetReader, and it implements nothing else. In particular it
// has no Open method, so it does not satisfy fs.File and cannot be routed
// down Kopia's seekable path by accident.
//
// Size() reports 0 and that is honest rather than lazy: the size of a
// stream is not known before it is read. Kopia agrees -- its
// uploadStreamingFileInternal overwrites DirEntry.FileSize with the number
// of bytes it actually copied.
type streamingEntry struct {
	src   StreamSource
	track *openStreams
}

var _ kopiafs.StreamingFile = (*streamingEntry)(nil)

func newStreamingEntry(src StreamSource, track *openStreams) *streamingEntry {
	return &streamingEntry{src: src, track: track}
}

func (e *streamingEntry) Name() string               { return e.src.Name() }
func (e *streamingEntry) Size() int64                { return 0 }
func (e *streamingEntry) Mode() os.FileMode          { return defaultPermissions }
func (e *streamingEntry) ModTime() time.Time         { return e.src.ModTime() }
func (e *streamingEntry) IsDir() bool                { return false }
func (e *streamingEntry) Sys() any                   { return nil }
func (e *streamingEntry) Owner() kopiafs.OwnerInfo   { return kopiafs.OwnerInfo{} }
func (e *streamingEntry) Device() kopiafs.DeviceInfo { return kopiafs.DeviceInfo{} }

// LocalFilesystemPath returns "" because there is no local path. This is
// the method a staging implementation would have to fill in, and its
// emptiness is what says none exists.
func (e *streamingEntry) LocalFilesystemPath() string { return "" }

// Close is required by fs.Entry and must be idempotent. The entry itself
// holds nothing; the reader it handed out is closed through its own Close.
func (e *streamingEntry) Close() {}

// GetReader opens the remote, once, when Kopia asks for it.
//
// Opening lazily rather than in the constructor is deliberate: if the
// upload fails before it reaches this file, no connection to the remote was
// ever made, so there is nothing to leak.
func (e *streamingEntry) GetReader(ctx context.Context) (io.ReadCloser, error) {
	rc, err := e.src.Open(ctx)
	if err != nil {
		return nil, err
	}

	g := &guardedReader{ctx: ctx, rc: rc}
	if e.track != nil {
		e.track.add(g)
	}

	return g, nil
}

// guardedReader is the cancellation half of the contract.
//
// Kopia's copy loop checks its own cancellation flag between reads, not the
// context, and a Read already blocked on a remote socket is not reachable
// by a flag. So two things happen here: every Read is refused once ctx is
// done, and openStreams.closeAll closes the underlying reader from the
// outside to unblock one that is already in flight.
//
// Because a read torn down that way reports whatever the transport felt
// like reporting, an error raised while ctx is done is reported as ctx's
// error. A caller that cancelled must see context.Canceled, not "use of
// closed network connection".
//
// It implements io.ReadCloser and nothing more. It is not an io.Seeker and
// it is not an io.ReaderAt, and a test asserts both.
type guardedReader struct {
	ctx context.Context
	rc  io.ReadCloser

	once     sync.Once
	closeErr error
}

func (g *guardedReader) Read(p []byte) (int, error) {
	if err := g.ctx.Err(); err != nil {
		return 0, err
	}

	n, err := g.rc.Read(p)
	if err != nil {
		if cerr := g.ctx.Err(); cerr != nil {
			return n, cerr
		}
	}

	return n, err
}

func (g *guardedReader) Close() error {
	g.once.Do(func() { g.closeErr = g.rc.Close() })

	return g.closeErr
}

// openStreams tracks the readers handed to Kopia during one attempt.
//
// Kopia closes the streaming reader itself, in a defer, and in the ordinary
// case that is the only Close that happens -- guardedReader.Close is
// idempotent precisely so the engine's own belt-and-braces closeAll is
// free. What the tracker buys is the case Kopia cannot cover: a reader
// blocked in Read when the context is cancelled, which nothing inside the
// uploader can reach.
type openStreams struct {
	mu      sync.Mutex
	readers []io.Closer
}

func (o *openStreams) add(c io.Closer) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.readers = append(o.readers, c)
}

func (o *openStreams) closeAll() {
	o.mu.Lock()
	readers := o.readers
	o.mu.Unlock()

	for _, r := range readers {
		_ = r.Close()
	}
}
