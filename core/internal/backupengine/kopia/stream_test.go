package kopia_test

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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
)

// This file is the feasibility gate for streaming sources, folded into this
// package from the spike that proved them. Every assertion is one of the
// acceptance criteria that spike named, and they are all one claim: a source
// that can only be read once, forward, can be stored without ever existing
// anywhere else. ADR 0007 records the measurements.
//
// The tests drive backupengine.StreamingRepository, not this package's
// internals, because the capability is the thing that has to hold: a future
// engine behind the same port has to pass this file unchanged.

// streamHost and streamUser are the identity half of a streamed source. They
// are constants here because a test is one machine; what they are NOT is a
// backup-set identity, which is the Phase 1 requirement ADR 0006 records.
const (
	streamHost = "spike-host"
	streamUser = "spike-user"
)

// newStreamingRepository opens a repository under a fresh temp root and
// returns it as the streaming capability plus that root. The root is what the
// "no local mirror" assertions walk: if anything staged the source, it staged
// it under here.
func newStreamingRepository(t *testing.T) (backupengine.StreamingRepository, string) {
	t.Helper()

	ctx := context.Background()
	root := t.TempDir()

	loc := backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Path:       filepath.Join(root, "repo"),
		ConfigPath: filepath.Join(root, "state", "repository.config"),
		Passphrase: testPassphrase,
	}

	if err := os.MkdirAll(filepath.Dir(loc.ConfigPath), 0o750); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}

	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	// Content caching is left switched off (an empty CachePath, which is
	// how "no cache" is spelled here). That is not tuning: a local cache is
	// a second place the source's bytes could land, and the claim these
	// tests make is that there is no such place.
	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	streaming, ok := rep.(backupengine.StreamingRepository)
	if !ok {
		t.Fatalf("%T does not implement backupengine.StreamingRepository; the streaming capability is not wired up", rep)
	}

	return streaming, root
}

// streamRequest is the request under test: one object path, one stream.
func streamRequest(objectPath string, src backupengine.StreamSource) backupengine.StreamSnapshotRequest {
	return backupengine.StreamSnapshotRequest{
		Source: backupengine.Source{Host: streamHost, User: streamUser, Path: objectPath},
		Stream: src,
	}
}

// --- test sources -----------------------------------------------------

// countingSource is a StreamSource whose body is generated on the fly and
// which records every reader it ever handed out, so a test can assert that
// all of them were closed. Nothing it returns is seekable and nothing it
// returns is backed by a file.
type countingSource struct {
	modTime time.Time

	// newBody builds the payload stream for one attempt.
	newBody func(attempt int) io.Reader

	mu     sync.Mutex
	opened int
	closed int
}

func (s *countingSource) ModTime() time.Time { return s.modTime }

func (s *countingSource) Open(_ context.Context) (io.ReadCloser, error) {
	s.mu.Lock()
	s.opened++
	attempt := s.opened
	s.mu.Unlock()

	return &countingBody{src: s, r: s.newBody(attempt)}, nil
}

func (s *countingSource) counts() (opened, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.opened, s.closed
}

type countingBody struct {
	src *countingSource
	r   io.Reader

	once sync.Once
}

func (b *countingBody) Read(p []byte) (int, error) { return b.r.Read(p) } //nolint:wrapcheck // test fixture

func (b *countingBody) Close() error {
	b.once.Do(func() {
		b.src.mu.Lock()
		b.src.closed++
		b.src.mu.Unlock()
	})

	return nil
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// patternReader generates n bytes of deterministic pseudo-random data from a
// seed and never holds more than one buffer. It is the multi-GB fixture: a
// file this size is generated, not committed, and never materialised in full
// anywhere.
type patternReader struct {
	remaining int64
	state     uint64
}

func newPatternReader(n int64, seed uint64) *patternReader {
	if seed == 0 {
		seed = 1
	}

	return &patternReader{remaining: n, state: seed}
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

// largestFile returns the size and path of the biggest regular file under
// root. Inside a repository this is a pack blob, which Kopia caps well below
// the size of any file worth streaming; a staged mirror would blow straight
// through that cap.
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
// repository is where the bytes are supposed to end up. Anywhere else -- a
// temp file, a spool directory, a partial -- is a mirror, and small objects
// are packed into blobs of their own so the repository's file sizes say
// nothing about staging either way.
func assertNothingStaged(t *testing.T, root, repoDir string) {
	t.Helper()

	// Anything this side of the repository should be configuration. The
	// connection config is a few hundred bytes.
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

// readStream reads a stored snapshot back and returns its SHA-256.
func readStream(t *testing.T, rep backupengine.StreamingRepository, id backupengine.SnapshotID) (string, int64) {
	t.Helper()

	rc, err := rep.OpenSnapshotStream(context.Background(), id)
	if err != nil {
		t.Fatalf("OpenSnapshotStream(%s): %v", id, err)
	}

	h := sha256.New()

	n, err := io.Copy(h, rc)
	if err != nil {
		t.Fatalf("reading back snapshot %s: %v", id, err)
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("closing restored stream: %v", err)
	}

	return hex.EncodeToString(h.Sum(nil)), n
}

// --- the gate ---------------------------------------------------------

// TestSnapshotAndRestoreSmallStream is the round trip: a stream in, a
// byte-identical stream out.
func TestSnapshotAndRestoreSmallStream(t *testing.T) {
	t.Parallel()

	rep, root := newStreamingRepository(t)
	ctx := context.Background()

	want := patternBytes(3<<20, 0xC0FFEE)

	src := &countingSource{
		modTime: time.Unix(1700000000, 0).UTC(),
		newBody: func(int) io.Reader { return bytes.NewReader(want) },
	}

	snap, err := rep.SnapshotStream(ctx, streamRequest("/small.bin", src))
	if err != nil {
		t.Fatalf("SnapshotStream: %v", err)
	}

	if snap.Bytes != int64(len(want)) {
		t.Errorf("snapshot recorded %d bytes, source produced %d", snap.Bytes, len(want))
	}

	if snap.Attempts != 1 {
		t.Errorf("a clean stream took %d attempts, want 1", snap.Attempts)
	}

	if opened, closed := src.counts(); opened != 1 || closed != 1 {
		t.Errorf("source opened %d times and closed %d; want exactly one of each", opened, closed)
	}

	if got, n := readStream(t, rep, snap.ID); got != sha256Hex(want) || n != int64(len(want)) {
		t.Fatalf("restored %d bytes sha256=%s; want %d bytes sha256=%s", n, got, len(want), sha256Hex(want))
	}

	assertNothingStaged(t, root, filepath.Join(root, "repo"))
}

// TestNestedObjectNameRoundTrips is the regression for an object stored under
// a name nothing looks for.
//
// A remote path has segments in it -- runs/2026/db.dump is an ordinary object
// name on every backend this product speaks -- and the snapshot entry's name
// and the name the restore looks up used to be derived two different ways
// from it. The snapshot then succeeded, reported bytes, and could not be
// read back: an unrestorable backup that every statistic called healthy,
// which is the exact failure this product exists to prevent.
func TestNestedObjectNameRoundTrips(t *testing.T) {
	t.Parallel()

	rep, _ := newStreamingRepository(t)
	ctx := context.Background()

	want := patternBytes(1<<20, 0x2026)

	const objectPath = "/runs/2026/db.dump"

	src := &countingSource{
		modTime: time.Unix(1700000000, 0).UTC(),
		newBody: func(int) io.Reader { return bytes.NewReader(want) },
	}

	snap, err := rep.SnapshotStream(ctx, streamRequest(objectPath, src))
	if err != nil {
		t.Fatalf("SnapshotStream of %s: %v", objectPath, err)
	}

	if got, n := readStream(t, rep, snap.ID); got != sha256Hex(want) || n != int64(len(want)) {
		t.Fatalf("restored %d bytes sha256=%s; want %d bytes sha256=%s", n, got, len(want), sha256Hex(want))
	}

	// The identity the repository recorded has to be the one a caller can
	// list by, or the snapshot is only findable by remembering its ID.
	snaps, err := rep.ListSnapshots(ctx, backupengine.Source{Host: streamHost, User: streamUser, Path: objectPath})
	if err != nil {
		t.Fatalf("ListSnapshots(%s): %v", objectPath, err)
	}

	if len(snaps) != 1 || snaps[0].ID != snap.ID {
		t.Fatalf("listing %s returned %d snapshot(s) %v; want just %s", objectPath, len(snaps), snaps, snap.ID)
	}
}

// TestChangedContentWithPreservedModTimeIsReRead is the no-previous-manifests
// discipline, stated as the data loss it prevents.
//
// A streamed entry has no size, so a metadata comparison against a previous
// snapshot degrades to modification time and mode alone. A source that
// rewrites an object in place while preserving its mtime -- a restored
// database file, a dump written by a tool that copies timestamps, a remote
// with coarse clock granularity -- would then have its new content skipped
// without reading a byte, and the snapshot would claim to hold content it has
// never seen. Nothing downstream can detect that: the manifest is
// well-formed, the bytes it points at are intact, they are just the wrong
// bytes.
func TestChangedContentWithPreservedModTimeIsReRead(t *testing.T) {
	t.Parallel()

	rep, _ := newStreamingRepository(t)
	ctx := context.Background()

	const objectPath = "/rewritten-in-place.bin"

	frozen := time.Unix(1700000000, 0).UTC()

	before := patternBytes(4<<20, 0xAAAA)
	after := patternBytes(4<<20, 0xBBBB)

	first, err := rep.SnapshotStream(ctx, streamRequest(objectPath, &countingSource{
		modTime: frozen,
		newBody: func(int) io.Reader { return bytes.NewReader(before) },
	}))
	if err != nil {
		t.Fatalf("first SnapshotStream: %v", err)
	}

	changed := &countingSource{
		modTime: frozen,
		newBody: func(int) io.Reader { return bytes.NewReader(after) },
	}

	second, err := rep.SnapshotStream(ctx, streamRequest(objectPath, changed))
	if err != nil {
		t.Fatalf("second SnapshotStream: %v", err)
	}

	if opened, _ := changed.counts(); opened != 1 {
		t.Fatalf("the second run opened the source %d times; a reused entry never opens it at all", opened)
	}

	if second.Bytes != int64(len(after)) {
		t.Errorf("the second snapshot read %d bytes of a %d-byte object; it was not re-read in full",
			second.Bytes, len(after))
	}

	// The proof that matters is what comes back out, not how many bytes the
	// engine says it read: the second snapshot must restore the NEW content.
	if got, _ := readStream(t, rep, second.ID); got != sha256Hex(after) {
		t.Fatalf("the second snapshot restores sha256=%s; the source now holds %s (and used to hold %s). "+
			"Changed content was skipped on matching mtime", got, sha256Hex(after), sha256Hex(before))
	}

	// And the first snapshot is still the old content, so this is two
	// distinct snapshots rather than one overwritten in place.
	if got, _ := readStream(t, rep, first.ID); got != sha256Hex(before) {
		t.Errorf("the first snapshot now restores sha256=%s, want the original %s", got, sha256Hex(before))
	}

	if first.ID == second.ID {
		t.Error("both runs returned the same snapshot ID")
	}
}

// TestStreamRequestRefusesNonsense pins the refusals. Each of these used to
// be either a silent success or a panic-adjacent surprise, and all of them
// are caller bugs that cost nothing to name.
func TestStreamRequestRefusesNonsense(t *testing.T) {
	t.Parallel()

	rep, _ := newStreamingRepository(t)
	ctx := context.Background()

	body := func() backupengine.StreamSource {
		return &countingSource{newBody: func(int) io.Reader { return bytes.NewReader([]byte("payload")) }}
	}

	cases := []struct {
		name string
		req  backupengine.StreamSnapshotRequest
		want string
	}{
		{
			name: "negative attempt budget",
			req: backupengine.StreamSnapshotRequest{
				Source:      backupengine.Source{Host: streamHost, User: streamUser, Path: "/o.bin"},
				Stream:      body(),
				MaxAttempts: -1,
			},
			want: "MaxAttempts",
		},
		{
			name: "no source path",
			req: backupengine.StreamSnapshotRequest{
				Source: backupengine.Source{Host: streamHost, User: streamUser},
				Stream: body(),
			},
			want: "no path",
		},
		{
			name: "relative source path",
			req: backupengine.StreamSnapshotRequest{
				Source: backupengine.Source{Host: streamHost, User: streamUser, Path: "runs/2026/db.dump"},
				Stream: body(),
			},
			want: "not rooted",
		},
		{
			name: "path names no object",
			req: backupengine.StreamSnapshotRequest{
				Source: backupengine.Source{Host: streamHost, User: streamUser, Path: "/"},
				Stream: body(),
			},
			want: "names no object",
		},
		{
			name: "no stream",
			req: backupengine.StreamSnapshotRequest{
				Source: backupengine.Source{Host: streamHost, User: streamUser, Path: "/o.bin"},
			},
			want: "no source stream",
		},
		{
			name: "no host or user",
			req: backupengine.StreamSnapshotRequest{
				Source: backupengine.Source{Path: "/o.bin"},
				Stream: body(),
			},
			want: "host and user",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			info, err := rep.SnapshotStream(ctx, tc.req)
			if err == nil {
				t.Fatalf("SnapshotStream accepted it and returned snapshot %s", info.ID)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("SnapshotStream said %q; want something mentioning %q", err, tc.want)
			}
		})
	}
}

// TestLargeStreamIsBoundedInMemory is the multi-GB criterion. The stream is
// generated, never stored, and the peak live heap while it is snapshotted
// must be nowhere near its size.
//
// It deliberately does NOT call t.Parallel(). peakHeap samples process-wide
// HeapAlloc, so a sibling test allocating a 64 MiB payload next to it is
// measured as this test's memory: running it alongside the others read
// 865 MiB and said nothing true about streaming.
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

	rep, root := newStreamingRepository(t)
	ctx := context.Background()

	src := &countingSource{
		modTime: time.Unix(1700000000, 0).UTC(),
		newBody: func(int) io.Reader { return newPatternReader(streamBytes, 0x5EED) },
	}

	var (
		snap backupengine.StreamSnapshotInfo
		err  error
	)

	runtime.GC()

	peak := peakHeap(func() {
		snap, err = rep.SnapshotStream(ctx, streamRequest("/large.bin", src))
	})

	if err != nil {
		t.Fatalf("SnapshotStream: %v", err)
	}

	if snap.Bytes != streamBytes {
		t.Fatalf("snapshot recorded %d bytes, stream was %d", snap.Bytes, streamBytes)
	}

	// The whole point: peak live heap is a bounded working set, not the
	// file. A 2 GiB stream through an implementation that buffered it would
	// read at least 2 GiB of heap here.
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

	// Not even the repository holds the file as a file: a pack blob is
	// capped well below this, so a chunked stream stays under it and a
	// mirror would blow straight through.
	const perFileBudget = 64 << 20

	if biggest, where := largestFile(t, root); biggest >= perFileBudget {
		t.Errorf("largest file under the test root is %d bytes at %s; nothing here should approach the %d-byte source",
			biggest, where, streamBytes)
	}

	biggest, _ := largestFile(t, filepath.Join(root, "repo"))
	t.Logf("streamed %d bytes; peak heap %d bytes (%.1f MiB, %.2f%% of stream); repository %d bytes; largest blob %d bytes",
		streamBytes, peak, float64(peak)/(1<<20), 100*float64(peak)/float64(streamBytes),
		dirBytes(t, filepath.Join(root, "repo")), biggest)
}

// TestCancellationClosesTheRemoteReader is the leak criterion. Cancelling
// mid-snapshot must stop the upload, close the reader taken from the source,
// leave no snapshot behind, and leave no goroutine running.
func TestCancellationClosesTheRemoteReader(t *testing.T) {
	rep, _ := newStreamingRepository(t)

	started := make(chan struct{})

	var once sync.Once

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := &countingSource{
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
	// number below is about this upload and not about opening a repository.
	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)

	baseline := runtime.NumGoroutine()

	done := make(chan error, 1)

	go func() {
		_, err := rep.SnapshotStream(ctx, streamRequest("/endless.bin", src))
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
		t.Errorf("source opened %d readers and closed %d; the reader leaked on cancellation", opened, closed)
	}

	// A cancelled run must save nothing.
	snaps, err := rep.ListSnapshots(context.Background(),
		backupengine.Source{Host: streamHost, User: streamUser, Path: "/endless.bin"})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 0 {
		t.Errorf("cancelled run left %d snapshot(s) in the repository", len(snaps))
	}

	// Goroutines settle back. Poll rather than sampling once: the uploader's
	// worker pool shuts down asynchronously.
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

// TestReadErrorIsBoundedAndSavesNothing is the transport-interruption
// criterion, failing half. A stream that keeps breaking must be retried a
// bounded number of times and then reported as a failure, with no snapshot
// saved: a torn upload must never be marked good.
func TestReadErrorIsBoundedAndSavesNothing(t *testing.T) {
	t.Parallel()

	const attempts = 3

	rep, _ := newStreamingRepository(t)
	ctx := context.Background()

	boom := errors.New("simulated transport interruption")

	src := &countingSource{
		modTime: time.Unix(1700000000, 0).UTC(),
		newBody: func(int) io.Reader {
			return io.MultiReader(
				bytes.NewReader(patternBytes(512<<10, 7)),
				readerFunc(func([]byte) (int, error) { return 0, boom }),
			)
		},
	}

	req := streamRequest("/flaky.bin", src)
	req.MaxAttempts = attempts

	if _, err := rep.SnapshotStream(ctx, req); err == nil {
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

	snaps, err := rep.ListSnapshots(ctx, req.Source)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 0 {
		t.Errorf("a run that never completed left %d snapshot(s) in the repository", len(snaps))
	}
}

// TestReadErrorRetriesFromTheStartAndSucceeds is the other half. A retry
// re-opens the source and reads it from byte zero -- it does not seek, and it
// does not resume -- and the snapshot that results is byte-exact.
func TestReadErrorRetriesFromTheStartAndSucceeds(t *testing.T) {
	t.Parallel()

	rep, _ := newStreamingRepository(t)
	ctx := context.Background()

	want := patternBytes(4<<20, 0xABCDEF)

	src := &countingSource{
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

	req := streamRequest("/recovers.bin", src)
	req.MaxAttempts = 3

	snap, err := rep.SnapshotStream(ctx, req)
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

	if got, n := readStream(t, rep, snap.ID); got != sha256Hex(want) || n != int64(len(want)) {
		t.Fatalf("restored %d bytes sha256=%s; want %d bytes sha256=%s", n, got, len(want), sha256Hex(want))
	}
}

// TestSecondUnchangedSnapshotReusesContent is the incrementality criterion.
// Snapshotting the same bytes again must not store them again: the repository
// grows by metadata, not by another copy.
func TestSecondUnchangedSnapshotReusesContent(t *testing.T) {
	t.Parallel()

	rep, root := newStreamingRepository(t)
	ctx := context.Background()

	repoDir := filepath.Join(root, "repo")
	payload := patternBytes(64<<20, 0x1234567)

	newSrc := func() *countingSource {
		return &countingSource{
			modTime: time.Unix(1700000000, 0).UTC(),
			newBody: func(int) io.Reader { return bytes.NewReader(payload) },
		}
	}

	first, err := rep.SnapshotStream(ctx, streamRequest("/stable.bin", newSrc()))
	if err != nil {
		t.Fatalf("first SnapshotStream: %v", err)
	}

	afterFirst := dirBytes(t, repoDir)
	if afterFirst == 0 {
		t.Fatal("repository is empty after the first snapshot")
	}

	second, err := rep.SnapshotStream(ctx, streamRequest("/stable.bin", newSrc()))
	if err != nil {
		t.Fatalf("second SnapshotStream: %v", err)
	}

	growth := dirBytes(t, repoDir) - afterFirst

	// A second physical copy would roughly double the repository. Reuse
	// means the growth is manifest and index overhead only.
	budget := int64(len(payload)) / 100

	if growth > budget {
		t.Errorf("repository grew %d bytes on an unchanged re-snapshot of %d bytes (budget %d); content was not reused",
			growth, len(payload), budget)
	}

	// The second run still READ every byte -- streaming files carry no
	// usable size, so there is no metadata short-circuit to hide behind, and
	// the port forbids one on purpose. What it did not do is store them
	// again.
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

	t.Logf("payload %d bytes; repo after first %d, growth on the second %d (%.4f%% of payload); uploaded first=%d second=%d",
		len(payload), afterFirst, growth, 100*float64(growth)/float64(len(payload)),
		first.UploadedBytes, second.UploadedBytes)

	snaps, err := rep.ListSnapshots(ctx, backupengine.Source{Host: streamHost, User: streamUser, Path: "/stable.bin"})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(snaps))
	}

	// Both snapshots still restore.
	for i, s := range snaps {
		if got, _ := readStream(t, rep, s.ID); got != sha256Hex(payload) {
			t.Errorf("snapshot %d restored sha256=%s; want %s", i, got, sha256Hex(payload))
		}
	}
}
