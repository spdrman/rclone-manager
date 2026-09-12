package backupengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kopiafs "github.com/kopia/kopia/fs"
)

// This file is the whole of #791's feasibility gate. Every assertion in it
// is one of the acceptance criteria the issue named, and the reason they
// are all in one file is that they are all one claim: an rclone stream can
// be handed to Kopia without ever existing anywhere else.

const spikePassword = "spike-password-not-a-secret"

// newTestEngine opens an engine over a filesystem repository under a fresh
// temp root and returns it together with that root. The root is the thing
// the "no local mirror" assertions walk: if anything staged the source, it
// staged it under here.
func newTestEngine(t *testing.T, opts ...func(*Config)) (*Engine, string) {
	t.Helper()

	root := t.TempDir()
	cfg := Config{
		RepoDir:  filepath.Join(root, "repo"),
		StateDir: filepath.Join(root, "state"),
		Password: spikePassword,
	}
	for _, o := range opts {
		o(&cfg)
	}

	ctx := context.Background()
	e, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := e.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return e, root
}

// --- test sources -----------------------------------------------------

// countingSource is a StreamSource whose body is generated on the fly and
// which records every reader it ever handed out, so a test can assert that
// all of them were closed. Nothing it returns is seekable and nothing it
// returns is backed by a file.
type countingSource struct {
	name    string
	modTime time.Time
	size    int64

	// newBody builds the payload stream for one attempt.
	newBody func(attempt int) io.Reader

	mu       sync.Mutex
	opened   int
	closed   int
	maxAlive int
	alive    int
}

func (s *countingSource) Name() string       { return s.name }
func (s *countingSource) ModTime() time.Time { return s.modTime }

func (s *countingSource) Open(_ context.Context) (io.ReadCloser, error) {
	s.mu.Lock()
	s.opened++
	attempt := s.opened
	s.alive++
	if s.alive > s.maxAlive {
		s.maxAlive = s.alive
	}
	s.mu.Unlock()

	return &countingBody{src: s, r: s.newBody(attempt)}, nil
}

func (s *countingSource) counts() (opened, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.opened, s.closed
}

type countingBody struct {
	src    *countingSource
	r      io.Reader
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *countingBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true

	b.src.mu.Lock()
	b.src.closed++
	b.src.alive--
	b.src.mu.Unlock()

	return nil
}

// patternReader generates n bytes of deterministic pseudo-random data from
// a seed, 64 KiB at a time, and never holds more than one buffer. It is the
// multi-GB fixture: a file this size is generated, not committed, and never
// materialised in full anywhere.
type patternReader struct {
	remaining int64
	state     uint64
	buf       []byte
}

func newPatternReader(n int64, seed uint64) *patternReader {
	if seed == 0 {
		seed = 1
	}

	return &patternReader{remaining: n, state: seed, buf: make([]byte, 64<<10)}
}

func (p *patternReader) Read(dst []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}

	n := len(dst)
	if int64(n) > p.remaining {
		n = int(p.remaining)
	}

	for i := range n {
		// xorshift64*, enough to defeat compression and dedupe.
		p.state ^= p.state >> 12
		p.state ^= p.state << 25
		p.state ^= p.state >> 27
		dst[i] = byte(p.state * 2685821657736338717 >> 33)
	}

	p.remaining -= int64(n)

	return n, nil
}

func patternBytes(n int64, seed uint64) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(newPatternReader(n, seed), b); err != nil {
		panic(err)
	}

	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// --- measurement helpers ----------------------------------------------

// dirSize totals every regular file under root.
func dirSize(t *testing.T, root string) int64 {
	t.Helper()

	var total int64

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	return total
}

// largestFile returns the size and path of the biggest regular file under
// root. Inside a repository this is a pack blob, which Kopia caps well
// below the size of any file worth streaming; a staged mirror would blow
// straight through that cap.
func largestFile(t *testing.T, root string) (int64, string) {
	t.Helper()

	var (
		biggest int64
		where   string
	)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Size() > biggest {
			biggest, where = info.Size(), path
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	return biggest, where
}

// assertNothingStaged is the "no complete local mirror" criterion.
//
// It walks everything under root EXCEPT the repository, because the
// repository is where the bytes are supposed to end up. Anywhere else --
// a temp file, a spool directory, a partial -- is a mirror, and Kopia packs
// small objects into blobs of their own so the repository's file sizes say
// nothing about staging either way.
func assertNothingStaged(t *testing.T, root, repoDir string) {
	t.Helper()

	// Anything this side of the repository should be configuration. The
	// Kopia connection config is a few hundred bytes.
	const configBudget = 64 << 10

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == repoDir {
				return filepath.SkipDir
			}

			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Size() > configBudget {
			t.Errorf("a %d-byte file exists outside the repository at %s; the stream was staged", info.Size(), path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// peakHeap samples HeapAlloc while fn runs and reports the highest reading.
// It is deliberately a sampler rather than a hook: the claim under test is
// "peak live heap stays far below the file size", and that is exactly what
// HeapAlloc measures.
func peakHeap(fn func()) uint64 {
	var peak atomic.Uint64

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		var ms runtime.MemStats

		for {
			select {
			case <-stop:
				return
			default:
			}

			runtime.ReadMemStats(&ms)
			for {
				old := peak.Load()
				if ms.HeapAlloc <= old || peak.CompareAndSwap(old, ms.HeapAlloc) {
					break
				}
			}

			time.Sleep(20 * time.Millisecond)
		}
	}()

	fn()
	close(stop)
	<-done

	return peak.Load()
}

// --- the gate ---------------------------------------------------------

// TestStreamingEntryIsNotSeekable is the acceptance criterion that no fake
// Seek was needed. The entry this package hands Kopia must satisfy
// fs.StreamingFile, whose only reader accessor is GetReader() io.ReadCloser,
// and it must NOT satisfy fs.File, whose Open() returns an fs.Reader that
// embeds io.Seeker. Kopia's own newDirEntry type-switches `case fs.File,
// fs.StreamingFile` with fs.File first, so an entry that accidentally grew
// an Open method would silently take the seekable path.
func TestStreamingEntryIsNotSeekable(t *testing.T) {
	src := &countingSource{
		name:    "payload.bin",
		modTime: time.Unix(1700000000, 0).UTC(),
		newBody: func(int) io.Reader { return bytes.NewReader(nil) },
	}

	entry := newStreamingEntry(src, nil)

	if _, ok := any(entry).(kopiafs.StreamingFile); !ok {
		t.Fatalf("entry %T does not satisfy fs.StreamingFile; it is not on Kopia's streaming path", entry)
	}
	if _, ok := any(entry).(kopiafs.File); ok {
		t.Errorf("entry %T satisfies fs.File, so Kopia will take the seekable path and demand a Reader with Seek", entry)
	}

	rc, err := entry.GetReader(context.Background())
	if err != nil {
		t.Fatalf("GetReader: %v", err)
	}
	defer rc.Close()

	if _, ok := rc.(io.Seeker); ok {
		t.Errorf("the reader handed to Kopia (%T) implements io.Seeker; this spike must not fake seekability", rc)
	}
	if _, ok := rc.(io.ReaderAt); ok {
		t.Errorf("the reader handed to Kopia (%T) implements io.ReaderAt; this spike must not fake random access", rc)
	}
}

// TestSnapshotAndRestoreSmallStream is the round trip: a stream in, a
// byte-identical stream out.
func TestSnapshotAndRestoreSmallStream(t *testing.T) {
	e, root := newTestEngine(t)
	ctx := context.Background()

	want := patternBytes(3<<20, 0xC0FFEE)
	wantHash := sha256Hex(want)

	src := &countingSource{
		name:    "small.bin",
		modTime: time.Unix(1700000000, 0).UTC(),
		size:    int64(len(want)),
		newBody: func(int) io.Reader { return bytes.NewReader(want) },
	}

	snap, err := e.SnapshotStream(ctx, src)
	if err != nil {
		t.Fatalf("SnapshotStream: %v", err)
	}

	if snap.Bytes != int64(len(want)) {
		t.Errorf("snapshot recorded %d bytes, source produced %d", snap.Bytes, len(want))
	}
	if opened, closed := src.counts(); opened != 1 || closed != 1 {
		t.Errorf("source opened %d times and closed %d; want exactly one of each", opened, closed)
	}

	rc, err := e.OpenSnapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading restored stream: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("closing restored stream: %v", err)
	}

	if gotHash := sha256Hex(got); gotHash != wantHash {
		t.Fatalf("restored %d bytes sha256=%s; want %d bytes sha256=%s",
			len(got), gotHash, len(want), wantHash)
	}

	assertNothingStaged(t, root, filepath.Join(root, "repo"))
}

// TestLargeStreamIsBoundedInMemory is the multi-GB criterion. The stream is
// generated, never stored, and the peak live heap while it is snapshotted
// must be nowhere near its size.
func TestLargeStreamIsBoundedInMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-GB stream; skipped under -short")
	}

	streamBytes := int64(2) << 30
	if s := os.Getenv("BACKUPD_SPIKE_STREAM_BYTES"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("BACKUPD_SPIKE_STREAM_BYTES=%q: %v", s, err)
		}
		streamBytes = n
	}

	e, root := newTestEngine(t)
	ctx := context.Background()

	src := &countingSource{
		name:    "large.bin",
		modTime: time.Unix(1700000000, 0).UTC(),
		size:    streamBytes,
		newBody: func(int) io.Reader { return newPatternReader(streamBytes, 0x5EED) },
	}

	var (
		snap Snapshot
		err  error
	)

	runtime.GC()

	peak := peakHeap(func() {
		snap, err = e.SnapshotStream(ctx, src)
	})
	if err != nil {
		t.Fatalf("SnapshotStream: %v", err)
	}

	if snap.Bytes != streamBytes {
		t.Fatalf("snapshot recorded %d bytes, stream was %d", snap.Bytes, streamBytes)
	}

	// The whole point: peak live heap is a bounded working set, not the
	// file. A 2 GiB stream through an implementation that buffered it
	// would read at least 2 GiB of heap here.
	const memoryBudget = 256 << 20
	if peak >= memoryBudget {
		t.Errorf("peak heap %d bytes (%.1f MiB) exceeded the %d MiB budget while streaming %d bytes",
			peak, float64(peak)/(1<<20), memoryBudget>>20, streamBytes)
	}
	if peak >= uint64(streamBytes)/8 {
		t.Errorf("peak heap %d bytes is not far below the %d-byte stream; memory is tracking file size",
			peak, streamBytes)
	}

	assertNothingStaged(t, root, filepath.Join(root, "repo"))

	// Not even the repository holds the file as a file: Kopia caps a pack
	// blob well below this, so a chunked stream stays under it and a
	// mirror would blow straight through.
	const perFileBudget = 64 << 20
	if biggest, where := largestFile(t, root); biggest >= perFileBudget {
		t.Errorf("largest file under the test root is %d bytes at %s; nothing here should approach the %d-byte source",
			biggest, where, streamBytes)
	}

	biggest, _ := largestFile(t, filepath.Join(root, "repo"))
	t.Logf("streamed %d bytes; peak heap %d bytes (%.1f MiB, %.2f%% of stream); repository %d bytes; largest blob %d bytes",
		streamBytes, peak, float64(peak)/(1<<20), 100*float64(peak)/float64(streamBytes),
		dirSize(t, filepath.Join(root, "repo")), biggest)
}

// TestCancellationClosesTheRemoteReader is the leak criterion. Cancelling
// mid-snapshot must stop the upload, close the reader taken from the
// remote, leave no snapshot behind, and leave no goroutine running.
func TestCancellationClosesTheRemoteReader(t *testing.T) {
	e, _ := newTestEngine(t)

	started := make(chan struct{})

	var once sync.Once

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := &countingSource{
		name:    "endless.bin",
		modTime: time.Unix(1700000000, 0).UTC(),
		newBody: func(int) io.Reader {
			return readerFunc(func(p []byte) (int, error) {
				once.Do(func() { close(started) })
				for i := range p {
					p[i] = byte(i)
				}

				return len(p), nil
			})
		},
	}

	// Let the engine settle before taking the goroutine baseline, so the
	// number below is about this upload and not about repo.Open.
	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)

	baseline := runtime.NumGoroutine()

	done := make(chan error, 1)

	go func() {
		_, err := e.SnapshotStream(ctx, src)
		done <- err
	}()

	<-started
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("SnapshotStream did not return within 60s of cancellation")
	}

	if err == nil {
		t.Fatal("SnapshotStream succeeded after cancellation; a torn upload must not be reported as a snapshot")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("SnapshotStream returned %v; want an error wrapping context.Canceled", err)
	}

	opened, closed := src.counts()
	if opened == 0 {
		t.Fatal("the source was never opened, so this test proved nothing about closing it")
	}
	if closed != opened {
		t.Errorf("source opened %d readers and closed %d; the remote reader leaked on cancellation", opened, closed)
	}

	// A cancelled run must save nothing.
	snaps, err := e.Snapshots(context.Background(), src.Name())
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("cancelled run left %d snapshot(s) in the repository", len(snaps))
	}

	// Goroutines settle back. Poll rather than sampling once: the
	// uploader's worker pool shuts down asynchronously.
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("goroutine count settled at %d, above the %d before the cancelled upload", n, baseline)

			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestReadErrorIsBoundedAndSavesNothing is the transport-interruption
// criterion, failing half. A stream that keeps breaking must be retried a
// bounded number of times and then reported as a failure, with no snapshot
// saved: a torn upload must never be marked good.
func TestReadErrorIsBoundedAndSavesNothing(t *testing.T) {
	const attempts = 3

	e, _ := newTestEngine(t, func(c *Config) { c.MaxAttempts = attempts })
	ctx := context.Background()

	boom := errors.New("simulated transport interruption")

	src := &countingSource{
		name:    "flaky.bin",
		modTime: time.Unix(1700000000, 0).UTC(),
		newBody: func(int) io.Reader {
			return io.MultiReader(
				bytes.NewReader(patternBytes(512<<10, 7)),
				readerFunc(func([]byte) (int, error) { return 0, boom }),
			)
		},
	}

	if _, err := e.SnapshotStream(ctx, src); err == nil {
		t.Fatal("SnapshotStream succeeded on a stream that always breaks")
	} else if !errors.Is(err, boom) {
		t.Errorf("SnapshotStream returned %v; want the underlying read error", err)
	}

	opened, closed := src.counts()
	if opened != attempts {
		t.Errorf("source was opened %d times; MaxAttempts is %d, so retries are not bounded by it", opened, attempts)
	}
	if closed != opened {
		t.Errorf("source opened %d readers and closed %d; a failed attempt leaked its reader", opened, closed)
	}

	snaps, err := e.Snapshots(ctx, src.Name())
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("a run that never completed left %d snapshot(s) in the repository", len(snaps))
	}
}

// TestReadErrorRetriesFromTheStartAndSucceeds is the other half. A retry
// re-opens the remote and reads it from byte zero -- it does not seek, and
// it does not resume -- and the snapshot that results is byte-exact.
func TestReadErrorRetriesFromTheStartAndSucceeds(t *testing.T) {
	e, _ := newTestEngine(t, func(c *Config) { c.MaxAttempts = 3 })
	ctx := context.Background()

	want := patternBytes(4<<20, 0xABCDEF)
	wantHash := sha256Hex(want)

	src := &countingSource{
		name:    "recovers.bin",
		modTime: time.Unix(1700000000, 0).UTC(),
		newBody: func(attempt int) io.Reader {
			if attempt == 1 {
				return io.MultiReader(
					bytes.NewReader(want[:1<<20]),
					readerFunc(func([]byte) (int, error) { return 0, errors.New("connection reset by peer") }),
				)
			}

			return bytes.NewReader(want)
		},
	}

	snap, err := e.SnapshotStream(ctx, src)
	if err != nil {
		t.Fatalf("SnapshotStream: %v", err)
	}
	if snap.Attempts != 2 {
		t.Errorf("snapshot took %d attempts; want 2 (one failure, one success)", snap.Attempts)
	}

	opened, closed := src.counts()
	if opened != 2 {
		t.Errorf("source was opened %d times; want 2", opened)
	}
	if closed != opened {
		t.Errorf("source opened %d readers and closed %d", opened, closed)
	}

	rc, err := e.OpenSnapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading restored stream: %v", err)
	}
	if gotHash := sha256Hex(got); gotHash != wantHash {
		t.Fatalf("restored sha256=%s (%d bytes); want %s (%d bytes)", gotHash, len(got), wantHash, len(want))
	}
}

// TestSecondUnchangedSnapshotReusesContent is the incrementality criterion.
// Snapshotting the same bytes again must not store them again: the
// repository grows by metadata, not by another copy.
func TestSecondUnchangedSnapshotReusesContent(t *testing.T) {
	e, root := newTestEngine(t)
	ctx := context.Background()

	repoDir := filepath.Join(root, "repo")
	payload := patternBytes(64<<20, 0x1234567)

	newSrc := func() *countingSource {
		return &countingSource{
			name:    "stable.bin",
			modTime: time.Unix(1700000000, 0).UTC(),
			size:    int64(len(payload)),
			newBody: func(int) io.Reader { return bytes.NewReader(payload) },
		}
	}

	first, err := e.SnapshotStream(ctx, newSrc())
	if err != nil {
		t.Fatalf("first SnapshotStream: %v", err)
	}

	afterFirst := dirSize(t, repoDir)
	if afterFirst == 0 {
		t.Fatal("repository is empty after the first snapshot")
	}

	second, err := e.SnapshotStream(ctx, newSrc())
	if err != nil {
		t.Fatalf("second SnapshotStream: %v", err)
	}

	afterSecond := dirSize(t, repoDir)
	growth := afterSecond - afterFirst

	// A second physical copy would roughly double the repository. Reuse
	// means the growth is manifest and index overhead only.
	budget := int64(len(payload)) / 100
	if growth > budget {
		t.Errorf("repository grew %d bytes on an unchanged re-snapshot of %d bytes (budget %d); content was not reused",
			growth, len(payload), budget)
	}

	if first.RootObjectID != second.RootObjectID {
		t.Errorf("the same bytes produced different object ids %q and %q; content addressing is not holding",
			first.RootObjectID, second.RootObjectID)
	}

	// The second run still READ every byte -- streaming files carry no
	// usable mtime/size cache, so there is no metadata short-circuit to
	// hide behind. What it did not do is store them again.
	if second.Bytes != int64(len(payload)) {
		t.Errorf("second snapshot read %d bytes; want the whole %d-byte stream re-read", second.Bytes, len(payload))
	}
	if first.UploadedBytes <= 0 {
		t.Fatalf("first snapshot uploaded %d bytes, so the measurement below distinguishes nothing", first.UploadedBytes)
	}
	if second.UploadedBytes > budget {
		t.Errorf("second snapshot pushed %d bytes to the repository (budget %d); the payload was stored twice",
			second.UploadedBytes, budget)
	}

	t.Logf("payload %d bytes; repo after first %d, after second %d, growth %d (%.4f%% of payload); uploaded first=%d second=%d",
		len(payload), afterFirst, afterSecond, growth, 100*float64(growth)/float64(len(payload)),
		first.UploadedBytes, second.UploadedBytes)

	snaps, err := e.Snapshots(ctx, "stable.bin")
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(snaps))
	}

	// Both snapshots still restore.
	for i, s := range snaps {
		rc, err := e.OpenSnapshot(ctx, s.ID)
		if err != nil {
			t.Fatalf("OpenSnapshot(%d): %v", i, err)
		}

		h := sha256.New()
		if _, err := io.Copy(h, rc); err != nil {
			t.Fatalf("restoring snapshot %d: %v", i, err)
		}
		rc.Close()

		if got := hex.EncodeToString(h.Sum(nil)); got != sha256Hex(payload) {
			t.Errorf("snapshot %d restored sha256=%s; want %s", i, got, sha256Hex(payload))
		}
	}
}
