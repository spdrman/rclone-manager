package transport_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/transport"
)

// Issue #792 is one question asked twice: what does this process do when a
// backup source holds one directory with a million entries in it?
//
// Transport.List answers it by materialising every entry: walk.GetAll
// returns fs.Objects, the adapter turns that into []RemoteArtifact, and
// both live at once. Cost is linear in entry count with no ceiling
// anywhere, so the first directory big enough to matter is an OOM in a
// daemon whose whole job is to still be running tomorrow.
//
// transport.LocalEnumerator is the other answer: entries are read in
// chunks and handed to a callback, so the peak is the chunk. The tests
// below are the evidence for that claim, and the one that matters most is
// TestEnumerationMemoryDoesNotScaleWithEntryCount: ten times the entries
// must not cost ten times the memory, or this file is decoration.
//
// Why a synthetic directory: a million real files cost ~two minutes to
// create and as long again to remove, per run, and prove nothing the
// synthetic one does not - the question is whether the ENUMERATOR holds
// entries, and a reader that yields chunks is exactly what the os-backed
// one does (see enumerate.go's osDir, which is *os.File.ReadDir(n)). The
// real filesystem path is covered at honest-but-affordable scale by
// TestEnumeratingARealDirectoryTree here and, against the rclone adapter
// and with a side-by-side List comparison, by
// TestStreamingCostsFarLessThanListAtScale in transport/rclone.

// syntheticDirEntry is one entry of a directory that does not exist,
// built on demand so a million of them are never all in memory at once.
type syntheticDirEntry struct {
	name string
	size int64
	dir  bool
}

func (e syntheticDirEntry) Name() string { return e.name }
func (e syntheticDirEntry) IsDir() bool  { return e.dir }
func (e syntheticDirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e syntheticDirEntry) Info() (fs.FileInfo, error) { return syntheticInfo(e), nil }

type syntheticInfo syntheticDirEntry

func (i syntheticInfo) Name() string { return i.name }
func (i syntheticInfo) Size() int64  { return i.size }
func (i syntheticInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i syntheticInfo) ModTime() time.Time { return time.Unix(1_700_000_000, 0) }
func (i syntheticInfo) IsDir() bool        { return i.dir }
func (i syntheticInfo) Sys() any           { return nil }

// syntheticTree is a directory tree described by counts rather than by
// files: dirs maps a slash path to how many regular files it holds and
// which subdirectories it has.
type syntheticTree struct {
	dirs map[string]syntheticNode

	mu        sync.Mutex
	open      int // readers open right now
	maxOpen   int // the most that were ever open at once
	opened    int
	closed    int
	chunkSize []int // every n LocalEnumerator asked for
}

type syntheticNode struct {
	files  int
	subs   []string
	prefix string // file name prefix, so two directories' entries differ
}

// syntheticRoot is the Source.Root every synthetic fixture uses. It names
// no real directory: the opener below never touches a filesystem.
const syntheticRoot = "/synthetic"

func flatTree(files int) *syntheticTree {
	return &syntheticTree{dirs: map[string]syntheticNode{"": {files: files, prefix: "f"}}}
}

// opener maps an absolute directory path back to a node of the tree. The
// enumerator opens directories by absolute path (it is the os-backed
// reader's own argument), and the tree is described relative to the
// synthetic root, so the prefix is stripped here rather than duplicated
// into every fixture.
func (t *syntheticTree) opener() transport.DirOpener {
	return func(ctx context.Context, dir string) (transport.DirReader, error) {
		rel := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(dir), syntheticRoot), "/")
		node, ok := t.dirs[rel]
		if !ok {
			return nil, fmt.Errorf("synthetic tree has no directory %q (relative %q)", dir, rel)
		}
		t.mu.Lock()
		t.open++
		t.opened++
		if t.open > t.maxOpen {
			t.maxOpen = t.open
		}
		t.mu.Unlock()
		return &syntheticReader{tree: t, node: node}, nil
	}
}

func (t *syntheticTree) stats() (maxOpen, opened, closed int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.maxOpen, t.opened, t.closed
}

type syntheticReader struct {
	tree   *syntheticTree
	node   syntheticNode
	served int
	done   bool
}

func (r *syntheticReader) Next(n int) ([]fs.DirEntry, error) {
	r.tree.mu.Lock()
	r.tree.chunkSize = append(r.tree.chunkSize, n)
	r.tree.mu.Unlock()

	out := make([]fs.DirEntry, 0, n)
	for len(out) < n && r.served < len(r.node.subs) {
		out = append(out, syntheticDirEntry{name: r.node.subs[r.served], dir: true})
		r.served++
	}
	for len(out) < n && r.served < len(r.node.subs)+r.node.files {
		i := r.served - len(r.node.subs)
		out = append(out, syntheticDirEntry{
			name: fmt.Sprintf("%s%08d.dump", r.node.prefix, i),
			size: int64(i % 4096),
		})
		r.served++
	}
	if len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}

func (r *syntheticReader) Close() error {
	if r.done {
		return nil
	}
	r.done = true
	r.tree.mu.Lock()
	r.tree.open--
	r.tree.closed++
	r.tree.mu.Unlock()
	return nil
}

// subdirTree is the directory-HEAVY fixture, and it is a second shape
// rather than a bigger instance of the first because the two bound
// different things. A flat directory of a million entries bounds how the
// enumerator handles ENTRIES; a directory holding a million
// SUBDIRECTORIES bounds the walk's pending work, and an enumerator that
// reads entries in chunks and then keeps one path per child directory is
// bounded on the first fixture and linear on this one - which is exactly
// the deployment shape this product runs against (FR-8: one directory
// per producer run, for years of runs).
//
// It cannot be expressed as a syntheticTree: that one names every
// subdirectory in a fixture map, so a million of them would be a million
// strings allocated before the measurement starts and a million more
// inside it. Here the tree is counts alone - the root holds subdirs
// subdirectories named d%08d, each holding filesPerSubdir regular files
// - so the only per-directory memory in the run is the enumerator's own.
type subdirTree struct {
	subdirs        int
	filesPerSubdir int

	mu      sync.Mutex
	open    int
	maxOpen int
	opened  int
	closed  int
}

func (t *subdirTree) opener() transport.DirOpener {
	return func(_ context.Context, dir string) (transport.DirReader, error) {
		rel := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(dir), syntheticRoot), "/")
		reader := &countedReader{tree: t}
		switch {
		case rel == "":
			reader.dirs = t.subdirs
		case strings.HasPrefix(rel, "d") && !strings.Contains(rel, "/"):
			reader.files = t.filesPerSubdir
		default:
			return nil, fmt.Errorf("subdir tree has no directory %q (relative %q)", dir, rel)
		}
		t.mu.Lock()
		t.open++
		t.opened++
		if t.open > t.maxOpen {
			t.maxOpen = t.open
		}
		t.mu.Unlock()
		return reader, nil
	}
}

func (t *subdirTree) stats() (maxOpen, opened, closed int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.maxOpen, t.opened, t.closed
}

type countedReader struct {
	tree   *subdirTree
	dirs   int
	files  int
	served int
	done   bool
}

func (r *countedReader) Next(n int) ([]fs.DirEntry, error) {
	// Sized to what is left, not to what was asked for. *os.File.ReadDir
	// allocates what it returns, and a fixture that allocated 4096 slots
	// to hand back one entry would be measuring itself: a million
	// single-file subdirectories is a million near-empty chunks.
	remaining := min(n, r.dirs+r.files-r.served)
	if remaining <= 0 {
		return nil, io.EOF
	}
	out := make([]fs.DirEntry, 0, remaining)
	for len(out) < n && r.served < r.dirs {
		out = append(out, syntheticDirEntry{name: fmt.Sprintf("d%08d", r.served), dir: true})
		r.served++
	}
	for len(out) < n && r.served < r.dirs+r.files {
		out = append(out, syntheticDirEntry{
			name: fmt.Sprintf("f%08d.dump", r.served-r.dirs),
			size: int64(r.served - r.dirs),
		})
		r.served++
	}
	if len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}

func (r *countedReader) Close() error {
	if r.done {
		return nil
	}
	r.done = true
	r.tree.mu.Lock()
	r.tree.open--
	r.tree.closed++
	r.tree.mu.Unlock()
	return nil
}

// measurement is what #792 asked to be captured: peak memory, total
// latency, time to the FIRST entry (the number that says whether a caller
// can start working before the listing finishes), goroutines and open
// directory handles.
type measurement struct {
	entries        int
	peakHeap       uint64
	rssDelta       int64
	total          time.Duration
	firstEntry     time.Duration
	goroutineDelta int
	maxOpenDirs    int
}

func (m measurement) String() string {
	return fmt.Sprintf("entries=%d peak_heap=%s max_rss_delta=%s total=%s first_entry=%s goroutine_delta=%d max_open_dirs=%d",
		m.entries, mib(m.peakHeap), mib(uint64(max64(m.rssDelta, 0))), m.total.Round(time.Millisecond),
		m.firstEntry.Round(time.Microsecond), m.goroutineDelta, m.maxOpenDirs)
}

func mib(b uint64) string { return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20)) }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// maxRSS reads this process's high-water resident set. It is monotonic
// per process, so only a DELTA measured around one run means anything,
// and only when that run is the biggest so far; the tests below order
// their runs smallest-first for exactly that reason.
func maxRSS(t *testing.T) int64 {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) // bytes
	}
	return int64(ru.Maxrss) * 1024 // kilobytes everywhere else
}

// measureEnumeration runs one enumeration with a heap sampler alongside
// it and returns what it cost.
func measureEnumeration(t *testing.T, run func(yield func(transport.RemoteArtifact) error) error) measurement {
	t.Helper()

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	goroutinesBefore := runtime.NumGoroutine()
	rssBefore := maxRSS(t)

	var peak uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > peak {
					peak = ms.HeapAlloc
				}
			}
		}
	}()

	var m measurement
	start := time.Now()
	err := run(func(transport.RemoteArtifact) error {
		if m.entries == 0 {
			m.firstEntry = time.Since(start)
		}
		m.entries++
		return nil
	})
	m.total = time.Since(start)
	close(stop)
	<-done

	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > peak {
		peak = after.HeapAlloc
	}
	if peak > base.HeapAlloc {
		m.peakHeap = peak - base.HeapAlloc
	}
	m.rssDelta = maxRSS(t) - rssBefore
	m.goroutineDelta = runtime.NumGoroutine() - goroutinesBefore
	return m
}

// TestEnumerationMemoryDoesNotScaleWithEntryCount is #792's acceptance
// criterion and the whole point of the exercise: the same flat directory
// at 100,000 and at 1,000,000 entries must cost about the same peak, not
// ten times as much.
//
// Two assertions, because either one alone is cheatable. The RATIO catches
// a per-entry allocation that survives the callback (the exact shape
// List has), and the ABSOLUTE ceiling catches an enumerator whose fixed
// overhead is already so large that a ratio near 1 means nothing.
func TestEnumerationMemoryDoesNotScaleWithEntryCount(t *testing.T) {
	const chunk = 4096
	sizes := []int{100_000, 1_000_000}
	got := make([]measurement, 0, len(sizes))

	for _, n := range sizes {
		tree := flatTree(n)
		enum := transport.LocalEnumerator{OpenDir: tree.opener()}
		m := measureEnumeration(t, func(yield func(transport.RemoteArtifact) error) error {
			return enum.Enumerate(context.Background(), transport.Source{Type: "local", Root: "/synthetic"},
				transport.EnumerateOptions{ChunkEntries: chunk}, yield)
		})
		maxOpen, opened, closed := tree.stats()
		m.maxOpenDirs = maxOpen
		if m.entries != n {
			t.Fatalf("enumerated %d entries, want %d", m.entries, n)
		}
		if opened != closed {
			t.Errorf("%d directory readers opened and %d closed", opened, closed)
		}
		t.Logf("bounded enumeration: %s", m)
		got = append(got, m)
	}

	small, large := got[0], got[1]

	// A chunk of 4096 entries is ~4096 * (fs.DirEntry + RemoteArtifact +
	// a name), a few megabytes at the outside. 32 MiB is a deliberately
	// generous ceiling: the failure this guards is 1,000,000 live
	// entries, which is hundreds of megabytes, not a factor of two.
	const ceiling = 32 << 20
	if large.peakHeap > ceiling {
		t.Errorf("peak heap enumerating 1,000,000 entries is %s, and a chunked enumeration may not exceed %s: memory is scaling with the directory, not with the buffer",
			mib(large.peakHeap), mib(ceiling))
	}
	if large.rssDelta > ceiling {
		t.Errorf("max RSS grew by %s while enumerating 1,000,000 entries, ceiling %s", mib(uint64(large.rssDelta)), mib(ceiling))
	}

	// Ten times the entries, at most twice the peak. Linear would be 10x.
	if small.peakHeap > 0 && float64(large.peakHeap) > 2*float64(small.peakHeap)+float64(4<<20) {
		t.Errorf("peak heap went from %s at 100,000 entries to %s at 1,000,000: ten times the entries must not cost ten times the memory",
			mib(small.peakHeap), mib(large.peakHeap))
	}
	if large.goroutineDelta != 0 {
		t.Errorf("enumeration left %d goroutine(s) behind", large.goroutineDelta)
	}
	if large.maxOpenDirs != 1 {
		t.Errorf("enumeration held %d directory handles at once over a single flat directory; the walk holds one per level of depth", large.maxOpenDirs)
	}
}

// TestEnumerationMemoryDoesNotScaleWithDirectoryCount is the same
// acceptance criterion asked of the other axis, and it is a separate test
// because an enumerator can pass the one above and fail this one
// completely: reading ENTRIES in chunks says nothing about what the walk
// retains about the directories it has not descended into yet. A walk
// that collects one path per child directory before descending is linear
// in the number of directories, and a source laid out one directory per
// producer run (FR-8) is precisely the tree that has a lot of them.
//
// Ten times the subdirectories, at most twice the peak - the same bound
// as the flat case, because a bounded walk is bounded in its pending work
// too, not only in its buffer.
func TestEnumerationMemoryDoesNotScaleWithDirectoryCount(t *testing.T) {
	const chunk = 4096
	sizes := []int{100_000, 1_000_000}
	got := make([]measurement, 0, len(sizes))

	for _, n := range sizes {
		tree := &subdirTree{subdirs: n, filesPerSubdir: 1}
		enum := transport.LocalEnumerator{OpenDir: tree.opener()}
		m := measureEnumeration(t, func(yield func(transport.RemoteArtifact) error) error {
			return enum.Enumerate(context.Background(), transport.Source{Type: "local", Root: syntheticRoot},
				transport.EnumerateOptions{ChunkEntries: chunk}, yield)
		})
		maxOpen, opened, closed := tree.stats()
		m.maxOpenDirs = maxOpen
		if m.entries != n {
			t.Fatalf("enumerated %d entries from %d subdirectories, want %d", m.entries, n, n)
		}
		if opened != n+1 {
			t.Errorf("opened %d directories, want %d: the root and every subdirectory under it", opened, n+1)
		}
		if opened != closed {
			t.Errorf("%d directory readers opened and %d closed", opened, closed)
		}
		t.Logf("directory-heavy enumeration: %s", m)
		got = append(got, m)
	}

	small, large := got[0], got[1]

	const ceiling = 32 << 20
	if large.peakHeap > ceiling {
		t.Errorf("peak heap enumerating 1,000,000 subdirectories is %s, ceiling %s: the walk is retaining the tree, not walking it",
			mib(large.peakHeap), mib(ceiling))
	}
	if large.rssDelta > ceiling {
		t.Errorf("max RSS grew by %s while enumerating 1,000,000 subdirectories, ceiling %s", mib(uint64(large.rssDelta)), mib(ceiling))
	}
	if small.peakHeap > 0 && float64(large.peakHeap) > 2*float64(small.peakHeap)+float64(4<<20) {
		t.Errorf("peak heap went from %s at 100,000 subdirectories to %s at 1,000,000: ten times the directories must not cost ten times the memory",
			mib(small.peakHeap), mib(large.peakHeap))
	}
	if large.goroutineDelta != 0 {
		t.Errorf("enumeration left %d goroutine(s) behind", large.goroutineDelta)
	}
	// Two levels of tree, so two open handles at the deepest point: the
	// root being read and the child being descended into. The handle is
	// what makes a directory frame RESUMABLE, which is what lets the walk
	// descend without first collecting every child path, so this number
	// is the shape of the tree and never the size of it.
	if large.maxOpenDirs > 2 {
		t.Errorf("enumeration held %d directory handles at once over a tree two levels deep; the bound is one per level", large.maxOpenDirs)
	}
}

// TestTheChunkCeilingIsWhatTheCallerAsked keeps the bound honest rather
// than incidental: an enumerator that ignored ChunkEntries and read the
// whole directory would pass the ratio test above on a synthetic reader
// that happens to be cheap, and would still OOM against a real one.
func TestTheChunkCeilingIsWhatTheCallerAsked(t *testing.T) {
	tree := flatTree(10_000)
	enum := transport.LocalEnumerator{OpenDir: tree.opener()}
	if err := enum.Enumerate(context.Background(),
		transport.Source{Type: "local", Root: "/synthetic"},
		transport.EnumerateOptions{ChunkEntries: 512},
		func(transport.RemoteArtifact) error { return nil }); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	tree.mu.Lock()
	asked := append([]int(nil), tree.chunkSize...)
	tree.mu.Unlock()
	if len(asked) == 0 {
		t.Fatal("the enumerator never asked the directory for a chunk")
	}
	for i, n := range asked {
		if n != 512 {
			t.Fatalf("chunk %d asked for %d entries, want the configured 512", i, n)
		}
	}
	if len(asked) < 10_000/512 {
		t.Errorf("10,000 entries arrived in %d chunks of 512; that is fewer reads than the entries require", len(asked))
	}
}

// TestEnumerationRecursesDepthFirstAndPrunesExcludedSubtrees pins the two
// properties that make a bounded enumerator usable as a replacement for
// List: it sees the whole tree, and it honours Source.ExcludePaths by
// never opening the excluded directory at all (issue #737's rule, which
// is about the walk and not about the answer).
func TestEnumerationRecursesDepthFirstAndPrunesExcludedSubtrees(t *testing.T) {
	tree := &syntheticTree{dirs: map[string]syntheticNode{
		"":            {files: 2, subs: []string{"runs", "cache"}, prefix: "top"},
		"runs":        {files: 3, subs: []string{"deep"}, prefix: "run"},
		"runs/deep":   {files: 1, prefix: "deep"},
		"cache":       {files: 5, subs: []string{"tiles"}, prefix: "cache"},
		"cache/tiles": {files: 7, prefix: "tile"},
	}}
	enum := transport.LocalEnumerator{OpenDir: tree.opener()}

	var paths []string
	err := enum.Enumerate(context.Background(),
		transport.Source{Type: "local", Root: "/synthetic", ExcludePaths: []string{"cache"}},
		transport.EnumerateOptions{ChunkEntries: 2},
		func(a transport.RemoteArtifact) error {
			paths = append(paths, a.Path)
			return nil
		})
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	want := map[string]bool{
		"top00000000.dump": true, "top00000001.dump": true,
		"runs/run00000000.dump": true, "runs/run00000001.dump": true, "runs/run00000002.dump": true,
		"runs/deep/deep00000000.dump": true,
	}
	if len(paths) != len(want) {
		t.Fatalf("enumerated %d entries (%v), want %d", len(paths), paths, len(want))
	}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unexpected entry %q; the excluded subtree must not appear", p)
		}
	}
	if _, opened, _ := tree.stats(); opened != 3 {
		t.Errorf("opened %d directories, want 3: an excluded subtree is never opened, so cache/ and cache/tiles/ are not reads", opened)
	}
}

// TestCancellingMidEnumerationStopsPromptlyAndLeaksNothing is the second
// acceptance criterion. A million-entry directory is exactly the case
// where a shutdown has to be able to interrupt a listing, so cancellation
// has to be observed DURING the walk and every directory handle has to be
// closed on the way out.
func TestCancellingMidEnumerationStopsPromptlyAndLeaksNothing(t *testing.T) {
	tree := &syntheticTree{dirs: map[string]syntheticNode{
		"":     {files: 500_000, subs: []string{"more"}, prefix: "top"},
		"more": {files: 500_000, prefix: "more"},
	}}
	enum := transport.LocalEnumerator{OpenDir: tree.opener()}

	goroutinesBefore := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := 0
	err := enum.Enumerate(ctx, transport.Source{Type: "local", Root: "/synthetic"},
		transport.EnumerateOptions{ChunkEntries: 1024},
		func(transport.RemoteArtifact) error {
			seen++
			if seen == 2048 {
				cancel()
			}
			return nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("enumerate after cancellation returned %v, want context.Canceled", err)
	}
	// One chunk of latency is the contract: the check is per chunk, not
	// per entry, because ctx.Err() on every one of a million entries is a
	// cost paid for nothing.
	if seen > 2048+1024 {
		t.Errorf("enumeration delivered %d entries after cancellation at 2048; the ceiling is one chunk (1024) more", seen)
	}
	_, opened, closed := tree.stats()
	if opened != closed {
		t.Errorf("cancellation left %d of %d directory readers open", opened-closed, opened)
	}
	// Let any goroutine that was going to leak get scheduled first.
	time.Sleep(50 * time.Millisecond)
	if delta := runtime.NumGoroutine() - goroutinesBefore; delta != 0 {
		t.Errorf("cancellation left %d goroutine(s) behind", delta)
	}
}

// TestAnErrorFromTheCallbackStopsTheWalkAndClosesEverything is the same
// property for the other way out: the consumer of a stream is what
// decides it has seen enough, and a listing that kept walking after its
// consumer failed would be a million syscalls nobody is waiting for.
func TestAnErrorFromTheCallbackStopsTheWalkAndClosesEverything(t *testing.T) {
	tree := flatTree(100_000)
	enum := transport.LocalEnumerator{OpenDir: tree.opener()}
	sentinel := errors.New("the consumer gave up")

	seen := 0
	err := enum.Enumerate(context.Background(), transport.Source{Type: "local", Root: "/synthetic"},
		transport.EnumerateOptions{ChunkEntries: 256},
		func(transport.RemoteArtifact) error {
			seen++
			if seen == 300 {
				return sentinel
			}
			return nil
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("enumerate returned %v, want the callback's own error", err)
	}
	if seen != 300 {
		t.Errorf("the walk delivered %d entries; it must stop at the entry the callback refused", seen)
	}
	if _, opened, closed := tree.stats(); opened != closed {
		t.Errorf("%d of %d directory readers left open", opened-closed, opened)
	}
}

// TestEnumeratingARealDirectoryTree is the os-backed reader against a
// real filesystem: the synthetic tests above prove the driver is bounded,
// and this proves the driver is driving the right thing - relative paths,
// sizes, modification times, recursion, and a symlink treated the way the
// capability matrix says local_volume treats one (skipped).
func TestEnumeratingARealDirectoryTree(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string, size int) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.dump", 11)
	mustWrite("runs/b.dump", 22)
	mustWrite("runs/deep/c.dump", 33)
	mustWrite("cache/d.dump", 44)
	if err := os.Symlink(filepath.Join(root, "a.dump"), filepath.Join(root, "link.dump")); err != nil {
		t.Fatal(err)
	}

	sizes := map[string]int64{}
	var m measurement
	start := time.Now()
	err := transport.LocalEnumerator{}.Enumerate(context.Background(),
		transport.Source{Type: "local", Root: root, ExcludePaths: []string{"cache"}},
		transport.EnumerateOptions{ChunkEntries: 2},
		func(a transport.RemoteArtifact) error {
			if m.entries == 0 {
				m.firstEntry = time.Since(start)
			}
			m.entries++
			sizes[a.Path] = a.Size
			if a.ModTime == 0 {
				t.Errorf("entry %q carries no modification time", a.Path)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	want := map[string]int64{"a.dump": 11, "runs/b.dump": 22, "runs/deep/c.dump": 33}
	if len(sizes) != len(want) {
		t.Fatalf("enumerated %v, want exactly %v (the symlink is skipped and cache/ is excluded)", sizes, want)
	}
	for path, size := range want {
		if sizes[path] != size {
			t.Errorf("%s size = %d, want %d", path, sizes[path], size)
		}
	}
}

// ReportNonRegular is what a caller with a symlink policy and a
// special-file policy needs: the entries it has to make a decision about,
// each SAYING what it is.
//
// Two claims, and both of them matter to the caller that asked for this.
// The entries arrive, with a Kind that is not a guess - a fifo is
// "other", a symlink is "symlink", and neither is "regular". And nothing
// here opens or follows either one: the sizes come from the lstat the
// directory read already performed, so the symlink's size is the length
// of its target and not the size of the file it points at.
func TestReportNonRegularNamesWhatItYieldsAndFollowsNothing(t *testing.T) {
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "regular.dump"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("regular.dump", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo is unavailable here: %v", err)
	}

	collect := func(report bool) map[string]transport.EntryKind {
		t.Helper()

		got := map[string]transport.EntryKind{}
		err := transport.LocalEnumerator{}.Enumerate(context.Background(),
			transport.Source{Type: "local", Root: root},
			transport.EnumerateOptions{ReportNonRegular: report},
			func(a transport.RemoteArtifact) error {
				got[a.Path] = a.Kind
				if a.Path == "link" && a.Size == 4096 {
					t.Error("the symlink was described with its target's size, which means something followed it")
				}
				return nil
			})
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}

		return got
	}

	// The default is unchanged: a socket is not an artifact this
	// product copies, and every caller predating this option expects
	// not to hear about one.
	if got := collect(false); len(got) != 1 || got["regular.dump"] != transport.EntryKindRegular {
		t.Fatalf("without ReportNonRegular the walk yielded %v, want only the regular file", got)
	}

	want := map[string]transport.EntryKind{
		"regular.dump": transport.EntryKindRegular,
		"link":         transport.EntryKindSymlink,
		"pipe":         transport.EntryKindOther,
	}

	got := collect(true)
	if len(got) != len(want) {
		t.Fatalf("the walk yielded %v, want %v", got, want)
	}

	for path, kind := range want {
		if got[path] != kind {
			t.Errorf("%s is reported as %q, want %q", path, got[path], kind)
		}
	}
}

// TestEnumeratingAMissingRootIsAClassifiedRefusal keeps the boundary's own
// promise: a failure out of this enumerator is a transport.Error with a
// category, the same as every other failure lifecycle code switches on
// (FR-22), and not a bare os.PathError from whatever syscall noticed.
func TestEnumeratingAMissingRootIsAClassifiedRefusal(t *testing.T) {
	err := transport.LocalEnumerator{}.Enumerate(context.Background(),
		transport.Source{Type: "local", Root: filepath.Join(t.TempDir(), "not-there")},
		transport.EnumerateOptions{}, func(transport.RemoteArtifact) error { return nil })
	if err == nil {
		t.Fatal("enumerating a root that does not exist succeeded")
	}
	var terr *transport.Error
	if !errors.As(err, &terr) {
		t.Fatalf("error is not a transport.Error: %v", err)
	}
	if terr.Category != transport.NotFound {
		t.Errorf("category = %v, want NotFound", terr.Category)
	}
}

// TestTheDefaultChunkIsUsedWhenTheCallerAsksForNothing: zero has to mean
// the default rather than "read everything", because zero is what every
// caller that has not thought about this will pass.
func TestTheDefaultChunkIsUsedWhenTheCallerAsksForNothing(t *testing.T) {
	tree := flatTree(3)
	enum := transport.LocalEnumerator{OpenDir: tree.opener()}
	if err := enum.Enumerate(context.Background(), transport.Source{Type: "local", Root: "/synthetic"},
		transport.EnumerateOptions{}, func(transport.RemoteArtifact) error { return nil }); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if len(tree.chunkSize) == 0 || tree.chunkSize[0] != transport.DefaultChunkEntries {
		t.Fatalf("first chunk asked for %v, want the default %d", tree.chunkSize, transport.DefaultChunkEntries)
	}
}
