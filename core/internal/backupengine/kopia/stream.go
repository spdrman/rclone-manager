package kopia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kopiafs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/virtualfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/upload"

	"github.com/backupdproject/backupd/core/internal/backupengine"
)

// This file is the streaming half of the adapter: a source whose bytes can
// only be read once, forward, stored without ever existing anywhere else.
// ADR 0007 records what the spike behind it measured and what it costs.
//
// Everything vendor-shaped about it is here, behind
// backupengine.StreamingRepository, for the reason the package doc gives:
// Kopia's streaming entry type, its uploader, its virtual directory and its
// progress sink are all names that may move on any release, and this is the
// one file that should have to notice.

const (
	// defaultStreamAttempts bounds how many times a broken stream is
	// re-read from byte zero. Three is the same shape
	// internal/transport/retry uses: enough to ride out one transport
	// hiccup, few enough that a source that is genuinely gone is reported
	// rather than hammered.
	defaultStreamAttempts = 3

	// streamRootDirName is the virtual directory the streamed object hangs
	// off. Kopia's uploader takes a directory or a file at the root of a
	// snapshot and a bare streaming file is neither, so one has to exist.
	// It holds exactly one entry and never touches a disk.
	streamRootDirName = "."

	// streamPermissions is what a streamed entry records, since a stream
	// has no mode of its own. It matches virtualfs's own choice for the
	// same reason: a snapshot entry needs some permission bits and
	// inventing narrower ones would only look like information.
	streamPermissions os.FileMode = 0o777
)

var _ backupengine.StreamingRepository = (*repository)(nil)

// SnapshotStream implements backupengine.StreamingRepository.
func (r *repository) SnapshotStream(
	ctx context.Context,
	req backupengine.StreamSnapshotRequest,
) (backupengine.StreamSnapshotInfo, error) {
	si, leaf, err := streamIdentity(req.Source)
	if err != nil {
		return backupengine.StreamSnapshotInfo{}, err
	}

	if req.Stream == nil {
		return backupengine.StreamSnapshotInfo{}, errors.New("kopia: stream snapshot request carries no source stream")
	}

	attempts, err := streamAttempts(req.MaxAttempts)
	if err != nil {
		return backupengine.StreamSnapshotInfo{}, err
	}

	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		info, err := r.snapshotStreamOnce(ctx, req, si, leaf, attempt)
		if err == nil {
			return info, nil
		}

		lastErr = err

		// A cancelled context is an instruction, not a transport
		// hiccup, so it ends the run instead of consuming the rest of
		// the attempt budget.
		if ctx.Err() != nil {
			return backupengine.StreamSnapshotInfo{}, err
		}
	}

	return backupengine.StreamSnapshotInfo{}, lastErr
}

// snapshotStreamOnce is one attempt: one open of the source, one pass over
// its bytes, and a manifest saved only at the end of a clean pass.
//
// The whole attempt runs inside one Kopia write session. That is what makes
// "no torn snapshot marked good" structural rather than careful: if the
// callback returns an error the session closes without flushing, so nothing
// a partial read produced is committed and no manifest exists to find it by.
func (r *repository) snapshotStreamOnce(
	ctx context.Context,
	req backupengine.StreamSnapshotRequest,
	si snapshot.SourceInfo,
	leaf string,
	attempt int,
) (backupengine.StreamSnapshotInfo, error) {
	streams := &openStreams{}
	prog := &captureProgress{}

	root := virtualfs.NewStaticDirectory(streamRootDirName, []kopiafs.Entry{
		newStreamingEntry(leaf, req.Stream, streams),
	})

	// Unblock a read that is already in flight when the caller cancels.
	// Kopia's copy loop only checks its cancellation flag between reads, so
	// a reader parked on a remote socket is not reachable from inside the
	// uploader. Closing it from out here is.
	attemptDone := make(chan struct{})
	defer close(attemptDone)

	go func() {
		select {
		case <-ctx.Done():
			streams.closeAll()
		case <-attemptDone:
		}
	}()

	// Belt and braces for the paths Kopia's own defer does not cover (a
	// source opened but never handed to the copy loop). guardedReader.Close
	// is idempotent, so this costs nothing when Kopia got there first.
	defer streams.closeAll()

	var (
		uploaded atomic.Int64
		man      *snapshot.Manifest
		id       manifest.ID
	)

	err := repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{
		Purpose:  "backupd:snapshot-stream",
		OnUpload: func(n int64) { uploaded.Add(n) },
	}, func(ctx context.Context, w repo.RepositoryWriter) error {
		u := upload.NewUploader(w)
		u.Progress = prog
		u.FailFast = true
		u.DisableIgnoreRules = true
		u.ParallelUploads = 1

		// No previous manifests are passed, and that is the safety
		// property rather than a simplification. A streaming entry has
		// no size, so Kopia's findCachedEntry falls back to comparing
		// mode and modification time alone and would reuse the previous
		// entry without reading a byte: a source rewritten in place with
		// its mtime preserved would have its new content skipped,
		// silently, and the snapshot would claim to hold it. Every run
		// reads every byte; reuse has to earn itself at the content
		// level, which the measurements in ADR 0007 show it does.
		m, uerr := u.Upload(ctx, root, policy.BuildTree(nil, policy.DefaultPolicy), si)
		if uerr != nil {
			return uerr
		}

		if bad := prog.uploadFailure(m); bad != nil {
			return bad
		}

		m.Description = req.Description
		m.Tags = req.Tags

		sid, serr := snapshot.SaveSnapshot(ctx, w, m)
		if serr != nil {
			return fmt.Errorf("saving snapshot of %s: %w", si.Path, serr)
		}

		man, id = m, sid

		return nil
	})
	if err != nil {
		return backupengine.StreamSnapshotInfo{}, fmt.Errorf("streaming %s (attempt %d): %w", si.Path, attempt, err)
	}

	man.ID = id

	return backupengine.StreamSnapshotInfo{
		SnapshotInfo:  snapshotInfo(man),
		UploadedBytes: uploaded.Load(),
		Attempts:      attempt,
	}, nil
}

// OpenSnapshotStream implements backupengine.StreamingRepository.
func (r *repository) OpenSnapshotStream(ctx context.Context, id backupengine.SnapshotID) (io.ReadCloser, error) {
	man, err := r.load(ctx, id)
	if err != nil {
		return nil, err
	}

	// The name used here is the name streamIdentity derived when the
	// snapshot was written, computed by the same function from the same
	// stored path. The two used to be computed differently -- the entry
	// took the whole slash path, the restore took its base -- and an object
	// called runs/2026/db.dump was then unrestorable: it was in the
	// repository under a name nothing looked for.
	leaf, err := leafName(man.Source.Path)
	if err != nil {
		return nil, err
	}

	root, err := snapshotfs.SnapshotRoot(r.rep, man)
	if err != nil {
		return nil, fmt.Errorf("resolving snapshot root: %w", err)
	}

	dir, ok := root.(kopiafs.Directory)
	if !ok {
		return nil, fmt.Errorf("kopia: snapshot root is %T, not a directory", root)
	}

	child, err := dir.Child(ctx, leaf)
	if err != nil {
		return nil, fmt.Errorf("finding %q in snapshot %s: %w", leaf, id, err)
	}

	file, ok := child.(kopiafs.File)
	if !ok {
		return nil, fmt.Errorf("kopia: %q is stored as %T, not a file", leaf, child)
	}

	rc, err := file.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening %q from snapshot %s: %w", leaf, id, err)
	}

	return rc, nil
}

// streamIdentity converts a streamed object's identity into Kopia's and
// returns the single leaf name the snapshot and the restore both use.
//
// The path is required to be rooted and is only cleaned, never resolved
// against the filesystem: it names an object on a remote source, so
// filepath.Abs would silently mix the process working directory into a
// snapshot's identity and make the same object list differently depending on
// where backupd was started from.
func streamIdentity(src backupengine.Source) (snapshot.SourceInfo, string, error) {
	if src.Host == "" || src.User == "" {
		return snapshot.SourceInfo{}, "", errors.New(
			"kopia: source must carry both host and user; they are part of its identity in the repository")
	}

	if src.Path == "" {
		return snapshot.SourceInfo{}, "", errors.New("kopia: streaming source has no path")
	}

	if !strings.HasPrefix(src.Path, "/") {
		return snapshot.SourceInfo{}, "", fmt.Errorf(
			"kopia: streaming source path %q is not rooted; it must start with / so that a snapshot's identity "+
				"does not depend on the process working directory", src.Path)
	}

	clean := path.Clean(src.Path)

	leaf, err := leafName(clean)
	if err != nil {
		return snapshot.SourceInfo{}, "", err
	}

	return snapshot.SourceInfo{Host: src.Host, UserName: src.User, Path: clean}, leaf, nil
}

// leafName is the one place a streamed object's entry name is derived.
//
// It is slash-based, not filepath-based, because the paths it sees are remote
// object paths: on a platform where filepath.Base splits on something else, a
// path stored by one build would be looked up by another under a different
// name.
func leafName(objectPath string) (string, error) {
	leaf := path.Base(path.Clean(objectPath))

	switch leaf {
	case "/", ".", "..":
		return "", fmt.Errorf("kopia: streaming source path %q names no object", objectPath)
	}

	return leaf, nil
}

// streamAttempts resolves the request's attempt budget.
func streamAttempts(maxAttempts int) (int, error) {
	switch {
	case maxAttempts < 0:
		return 0, fmt.Errorf(
			"kopia: MaxAttempts is %d; a negative attempt budget is a caller bug, not an instruction to give up",
			maxAttempts)
	case maxAttempts == 0:
		return defaultStreamAttempts, nil
	default:
		return maxAttempts, nil
	}
}

// streamingEntry is the adapter: a backupengine.StreamSource seen as a Kopia
// fs.StreamingFile.
//
// It implements fs.Entry (os.FileInfo plus Owner/Device/LocalFilesystemPath/
// Close) and GetReader, and it implements nothing else. In particular it has
// no Open method, so it does not satisfy fs.File and cannot be routed down
// Kopia's seekable path by accident -- newDirEntry type-switches
// `case fs.File, fs.StreamingFile` with fs.File first, so an entry that grew
// an Open would silently be asked for a seekable reader.
//
// Size() reports 0 and that is honest rather than lazy: the size of a stream
// is not known before it is read. Kopia agrees -- its
// uploadStreamingFileInternal overwrites DirEntry.FileSize with the number of
// bytes it actually copied.
type streamingEntry struct {
	name  string
	src   backupengine.StreamSource
	track *openStreams
}

var _ kopiafs.StreamingFile = (*streamingEntry)(nil)

func newStreamingEntry(name string, src backupengine.StreamSource, track *openStreams) *streamingEntry {
	return &streamingEntry{name: name, src: src, track: track}
}

func (e *streamingEntry) Name() string               { return e.name }
func (e *streamingEntry) Size() int64                { return 0 }
func (e *streamingEntry) Mode() os.FileMode          { return streamPermissions }
func (e *streamingEntry) ModTime() time.Time         { return e.src.ModTime() }
func (e *streamingEntry) IsDir() bool                { return false }
func (e *streamingEntry) Sys() any                   { return nil }
func (e *streamingEntry) Owner() kopiafs.OwnerInfo   { return kopiafs.OwnerInfo{} }
func (e *streamingEntry) Device() kopiafs.DeviceInfo { return kopiafs.DeviceInfo{} }

// LocalFilesystemPath returns "" because there is no local path. This is the
// method a staging implementation would have to fill in, and its emptiness is
// what says none exists.
func (e *streamingEntry) LocalFilesystemPath() string { return "" }

// Close is required by fs.Entry and must be idempotent. The entry itself
// holds nothing; the reader it handed out is closed through its own Close.
func (e *streamingEntry) Close() {}

// GetReader opens the source, once, when Kopia asks for it.
//
// Opening lazily rather than in the constructor is deliberate: if the upload
// fails before it reaches this file, no connection to the source was ever
// made, so there is nothing to leak.
func (e *streamingEntry) GetReader(ctx context.Context) (io.ReadCloser, error) {
	rc, err := e.src.Open(ctx)
	if err != nil {
		//nolint:wrapcheck // the source already wraps with its own operation name.
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
// context, and a Read already blocked on a remote socket is not reachable by
// a flag. So two things happen here: every Read is refused once ctx is done,
// and openStreams.closeAll closes the underlying reader from the outside to
// unblock one that is already in flight.
//
// Because a read torn down that way reports whatever the transport felt like
// reporting, an error raised while ctx is done is reported as ctx's error. A
// caller that cancelled must see context.Canceled, not "use of closed network
// connection".
//
// It implements io.ReadCloser and nothing more. It is not an io.Seeker and it
// is not an io.ReaderAt, and a test asserts both.
type guardedReader struct {
	ctx context.Context
	rc  io.ReadCloser

	once     sync.Once
	closeErr error
}

func (g *guardedReader) Read(p []byte) (int, error) {
	if err := g.ctx.Err(); err != nil {
		//nolint:wrapcheck // the context's own error is the answer here.
		return 0, err
	}

	n, err := g.rc.Read(p)
	if err != nil {
		if cerr := g.ctx.Err(); cerr != nil {
			//nolint:wrapcheck // see the type doc: a torn-down read reports as cancellation.
			return n, cerr
		}
	}

	//nolint:wrapcheck // the reader below is the source's; its errors are its own.
	return n, err
}

func (g *guardedReader) Close() error {
	g.once.Do(func() { g.closeErr = g.rc.Close() })

	//nolint:wrapcheck // the reader below is the source's; its errors are its own.
	return g.closeErr
}

// openStreams tracks the readers handed to Kopia during one attempt.
//
// Kopia closes the streaming reader itself, in a defer, and in the ordinary
// case that is the only Close that happens -- guardedReader.Close is
// idempotent precisely so the adapter's own belt-and-braces closeAll is free.
// What the tracker buys is the case Kopia cannot cover: a reader blocked in
// Read when the context is cancelled, which nothing inside the uploader can
// reach.
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

// captureProgress is a Kopia upload progress sink that exists to catch the
// one thing Upload does not return.
//
// Kopia treats a failure to read ONE entry as a property of that entry rather
// than of the run: processEntryUploadResult records it against the directory
// and returns nil, so Upload hands back a perfectly well-formed manifest with
// ErrorCount set. Saving that manifest is exactly the torn snapshot marked
// good that must never happen, and the underlying error is only ever offered
// here, through Progress.Error.
type captureProgress struct {
	upload.NullUploadProgress

	mu    sync.Mutex
	first error
}

func (p *captureProgress) Error(_ string, err error, isIgnored bool) {
	if isIgnored {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.first == nil {
		p.first = err
	}
}

// uploadFailure reports why a manifest must not be saved, or nil if it may.
func (p *captureProgress) uploadFailure(m *snapshot.Manifest) error {
	p.mu.Lock()
	first := p.first
	p.mu.Unlock()

	if first != nil {
		//nolint:wrapcheck // this is the upload's own error, reported where it was offered.
		return first
	}

	if m.Stats.ErrorCount > 0 {
		return fmt.Errorf("upload reported %d error(s) but named none", m.Stats.ErrorCount)
	}

	if m.IncompleteReason != "" {
		return fmt.Errorf("upload is incomplete: %s", m.IncompleteReason)
	}

	if m.RootEntry == nil {
		return errors.New("upload produced no root entry")
	}

	return nil
}
