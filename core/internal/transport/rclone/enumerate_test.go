package rclone

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/hash"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// This file is the adapter half of issue #792. The bounded enumerator
// itself lives in core/internal/transport and is tested there against a
// synthetic million-entry directory; what is tested HERE is the part only
// this package can answer:
//
//   - dispatch: a local source is enumerated in chunks, and a source on a
//     backend whose listing is one slice (sftp) is refused before anything
//     is dialed, opened or allocated, rather than after the memory is
//     spent;
//   - the capability matrix's claims about the rclone backends it
//     describes, pinned against what this binary's rclone actually
//     reports, the same way TestEveryBundledManifestNamesABackendThisBinaryRegisters
//     pins SupportedRcloneBackends;
//   - the measurement #792 asked for: Adapter.List and Adapter.Enumerate
//     over the same real directory, side by side.

// TestAdapterEnumerateStreamsALocalSource is the dispatch, end to end
// through the real adapter and a real directory.
func TestAdapterEnumerateStreamsALocalSource(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"a.dump", "runs/b.dump", "runs/deep/c.dump"} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	err := New().Enumerate(context.Background(),
		transport.Source{Type: "local", Root: root},
		transport.EnumerateOptions{ChunkEntries: 2},
		func(a transport.RemoteArtifact) error {
			got = append(got, a.Path)
			return nil
		})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	want := map[string]bool{"a.dump": true, "runs/b.dump": true, "runs/deep/c.dump": true}
	if len(got) != len(want) {
		t.Fatalf("Enumerate returned %v, want the three artifacts %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
}

// TestEnumeratingAnSftpSourceIsRefusedBeforeAnythingIsDialed is the
// fail-closed half of the decision, and the assertion that matters is
// WHERE the refusal happens. The source below names a host that does not
// resolve and a key file that does not exist, so the order of the
// failures is the evidence: a refusal that reads "this backend cannot
// list a directory in bounded memory" means the capability was checked
// first, and one that reads "key_file is not accessible" means this
// adapter was already resolving credentials on the way to dialing - and a
// refusal that arrives after the dial arrives after the directory has
// been read into memory, which is the failure it exists to prevent.
//
// This is also the regression guard for the ceiling that used to sit in
// front of it: with an operator-configured maximum directory size, sftp
// took the walk instead, and this test's source got as far as its key
// file.
func TestEnumeratingAnSftpSourceIsRefusedBeforeAnythingIsDialed(t *testing.T) {
	src := transport.Source{
		ID:      "unreachable",
		Type:    "sftp",
		Host:    "sftp.invalid.test",
		User:    "nobody",
		KeyFile: filepath.Join(t.TempDir(), "no-such-key"),
		Root:    "/srv/backups",
	}

	start := time.Now()
	err := New().Enumerate(context.Background(), src, transport.EnumerateOptions{}, func(transport.RemoteArtifact) error {
		t.Error("an entry was delivered from a source this engine cannot enumerate safely")
		return nil
	})
	elapsed := time.Since(start)

	// The sentinel is backend's, not this package's: the capability
	// matrix owns the statement "this backend cannot list a directory in
	// bounded memory", and a second sentinel here would be a second
	// place to keep that decision.
	if !errors.Is(err, backend.ErrUnboundedListing) {
		t.Fatalf("Enumerate on an sftp source returned %v, want backend.ErrUnboundedListing", err)
	}
	if strings.Contains(err.Error(), "key_file") {
		t.Errorf("the refusal came out of resolving the source's credentials, so the capability gate did not fire first: %v", err)
	}
	var terr *transport.Error
	if !errors.As(err, &terr) {
		t.Fatalf("the refusal is not a transport.Error: %v", err)
	}
	if terr.Category != transport.Configuration {
		t.Errorf("category = %v, want Configuration: a backend that cannot be read in bounded memory is not a retry, it is a source this engine will not enumerate", terr.Category)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the refusal took %s, which is long enough that it happened after a dial attempt rather than before one", elapsed)
	}
}

// TestTheCapabilityMatrixMatchesWhatRcloneReportsForLocal is the
// cross-package pin backend/doc.go's arrangement asks for: the matrix is
// data in a JSON file, and at least the claims that CAN be checked
// against the live rclone registry are checked here, in the only package
// that may import both sides.
//
// recursive_listing is the interesting one. It means "this backend has a
// NATIVE recursive listing", which is precisely fs.Features().ListR, and
// local does not have one: rclone walks it directory by directory. The
// matrix says false, and if a future rclone gains a local ListR this test
// goes red and the matrix gets updated rather than quietly understating
// what the backend can do.
func TestTheCapabilityMatrixMatchesWhatRcloneReportsForLocal(t *testing.T) {
	reg, err := backend.Bundled()
	if err != nil {
		t.Fatalf("backend.Bundled(): %v", err)
	}
	m, err := reg.Backend("local_volume")
	if err != nil {
		t.Fatalf("Backend(\"local_volume\"): %v", err)
	}
	caps, ok := m.DeclaredCapabilities()
	if !ok {
		t.Fatal("the shipped local_volume manifest declares no capabilities")
	}

	f, _ := localFsWith(t, "probe.dump")
	features := f.Features()

	if got := features.ListR != nil; got != caps.RecursiveListing {
		t.Errorf("local_volume declares recursive_listing=%v and rclone reports ListR!=nil = %v", caps.RecursiveListing, got)
	}
	if !caps.StreamingOpen {
		t.Error("local_volume declares streaming_open=false, and rclone opens a local object as an io.Reader")
	}
	if got := f.Hashes().Contains(hashByName(t, "md5")); got != slices.Contains(caps.HashSupport, backend.HashMD5) {
		t.Errorf("local_volume declares md5 support = %v and rclone reports %v", slices.Contains(caps.HashSupport, backend.HashMD5), got)
	}
}

// TestTheCapabilityMatrixCoversEveryBundledBackend keeps the matrix from
// drifting behind the registry: a fourth manifest without capabilities is
// a backend nothing may enumerate, which is a state this build should not
// be able to ship silently.
func TestTheCapabilityMatrixCoversEveryBundledBackend(t *testing.T) {
	reg, err := backend.Bundled()
	if err != nil {
		t.Fatalf("backend.Bundled(): %v", err)
	}
	for _, id := range reg.IDs() {
		m, err := reg.Backend(id)
		if err != nil {
			t.Fatalf("Backend(%q): %v", id, err)
		}
		if _, ok := m.DeclaredCapabilities(); !ok {
			t.Errorf("bundled backend %q ships no capability matrix", id)
		}
	}
}

// TestStreamingCostsFarLessThanListAtScale is #792's measurement, run as
// an assertion: the same real directory, listed both ways, with the peak
// heap of each. It is skipped under -short because it creates the
// fixture; the entry count comes from BACKUPD_HUGE_DIR_ENTRIES so the
// million-entry run in docs/adr/0008 is reproducible with one variable
// and CI never pays for it.
func TestStreamingCostsFarLessThanListAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a large directory; run without -short")
	}
	entries := 100_000
	if raw := os.Getenv("BACKUPD_HUGE_DIR_ENTRIES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("BACKUPD_HUGE_DIR_ENTRIES=%q: %v", raw, err)
		}
		entries = n
	}

	root := t.TempDir()
	start := time.Now()
	for i := range entries {
		f, err := os.Create(filepath.Join(root, fmt.Sprintf("f%07d.dump", i)))
		if err != nil {
			t.Fatalf("creating fixture entry %d: %v", i, err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("fixture: %d entries created in %s", entries, time.Since(start).Round(time.Millisecond))

	src := transport.Source{Type: "local", Root: root}

	streamed, streamFirst := 0, time.Duration(0)
	streamStart := time.Now()
	streamPeak, streamGoroutines := peakDuring(t, func() {
		if err := New().Enumerate(context.Background(), src,
			transport.EnumerateOptions{ChunkEntries: 4096},
			func(transport.RemoteArtifact) error {
				if streamed == 0 {
					streamFirst = time.Since(streamStart)
				}
				streamed++
				return nil
			}); err != nil {
			t.Fatalf("Enumerate: %v", err)
		}
	})
	streamTotal := time.Since(streamStart)
	if streamed != entries {
		t.Fatalf("streamed %d entries, want %d", streamed, entries)
	}

	listStart := time.Now()
	var listed int
	listPeak, listGoroutines := peakDuring(t, func() {
		out, err := New().List(context.Background(), src)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		listed = len(out)
		// The slice is deliberately still alive when the sampler takes
		// its last reading: List's cost IS that everything is live at
		// once, and dropping it before measuring would hide it.
		if listed == 0 {
			t.Fatal("List returned nothing")
		}
	})
	listTotal := time.Since(listStart)
	if listed != entries {
		t.Fatalf("List returned %d entries, want %d", listed, entries)
	}

	t.Logf("stream: peak_heap=%.1fMiB peak_goroutines=%d total=%s first_entry=%s",
		float64(streamPeak)/(1<<20), streamGoroutines, streamTotal.Round(time.Millisecond), streamFirst.Round(time.Microsecond))
	t.Logf("List:   peak_heap=%.1fMiB peak_goroutines=%d total=%s first_entry=%s (nothing is delivered until the walk finishes)",
		float64(listPeak)/(1<<20), listGoroutines, listTotal.Round(time.Millisecond), listTotal.Round(time.Millisecond))

	if streamPeak >= listPeak {
		t.Errorf("streaming peak heap %.1fMiB is not below List's %.1fMiB; the streaming path is not buying anything",
			float64(streamPeak)/(1<<20), float64(listPeak)/(1<<20))
	}
	if streamPeak > 32<<20 {
		t.Errorf("streaming peak heap is %.1fMiB for %d entries, which is not bounded by the 4096-entry chunk",
			float64(streamPeak)/(1<<20), entries)
	}
}

// peakDuring samples HeapAlloc and the goroutine count while f runs, and
// returns the highest heap reading above the baseline together with the
// highest goroutine count seen.
//
// The goroutine number is half of #792's question about fan-out: rclone's
// walk runs one goroutine per --checkers over a backend with no native
// recursive listing (oneConnectionAtATime pins that at 1 for connection
// reasons, see adapter.go), and the chunked enumerator runs none of its
// own. The sampler itself is one goroutine and is counted in both
// figures, so they are comparable to each other rather than absolute.
func peakDuring(t *testing.T, f func()) (peakHeap uint64, peakGoroutines int) {
	t.Helper()
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	// The sampler writes these and this function reads them, so they are
	// atomics rather than the named returns they used to be: the plain
	// version was a data race the sampler won often enough to be
	// invisible without -race, and it failed the whole package under it.
	var (
		sampledHeap       atomic.Uint64
		sampledGoroutines atomic.Int64
	)

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				for {
					seen := sampledHeap.Load()
					if ms.HeapAlloc <= seen || sampledHeap.CompareAndSwap(seen, ms.HeapAlloc) {
						break
					}
				}
				for n := int64(runtime.NumGoroutine()); ; {
					seen := sampledGoroutines.Load()
					if n <= seen || sampledGoroutines.CompareAndSwap(seen, n) {
						break
					}
				}
			}
		}
	}()
	f()

	// The sampler is joined BEFORE its samples are read. Reading them
	// while it was still running was the other half of the same race.
	close(stop)
	<-done

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	peakHeap = sampledHeap.Load()
	peakGoroutines = int(sampledGoroutines.Load())
	if after.HeapAlloc > peakHeap {
		peakHeap = after.HeapAlloc
	}
	if peakHeap < base.HeapAlloc {
		return 0, peakGoroutines
	}
	return peakHeap - base.HeapAlloc, peakGoroutines
}

// hashByName resolves an rclone hash type by its own name, so this file
// names "md5" the way the capability matrix does rather than importing a
// constant the matrix cannot spell.
func hashByName(t *testing.T, name string) hash.Type {
	t.Helper()
	var ht hash.Type
	if err := ht.Set(name); err != nil {
		t.Fatalf("rclone does not know a hash called %q: %v", name, err)
	}
	return ht
}
