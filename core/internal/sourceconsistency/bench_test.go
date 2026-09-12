// The measurements ADR 0009 publishes.
//
// They are benchmarks in the repository rather than numbers somebody once
// saw, because a published cost that cannot be re-run is a claim rather than
// a measurement: nobody can check it, and nobody notices when a change makes
// it wrong. The ADR records the exact commands and the machine; this file is
// what those commands run.
//
//	cd core && go test ./internal/sourceconsistency/ \
//	    -run '^$' -bench Capture -benchtime 1x -count 5
//	cd core && go test ./internal/sourceconsistency/ \
//	    -run '^$' -bench Decide -count 5
//
// -benchtime 1x for the Capture rows because each iteration stages its own
// fixture (a 512 MiB file, a tree of 20,000 files) and a larger iteration
// count would mostly measure the filesystem writing the fixture. Decide
// touches no filesystem and runs at the default benchtime.
//
// -count 5 because wall-clock figures on a laptop vary by a factor of two
// between runs: the comparisons that matter are the RATIOS between
// neighbouring rows and the allocation figures, which are deterministic.
// The large-file rows read a file that has just been written and is
// therefore in the page cache; they price this package's own work (the
// hashing and the window), not a disk.

package sourceconsistency

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// benchLargeFileBytes is the single-file size the throughput rows use. Big
// enough that the hashing dominates the open and the three stats, small
// enough to stage on any machine that can run the test suite.
const benchLargeFileBytes = 512 << 20

// benchSmallFiles is the small-file tree: the shape a real source mostly
// has, and the one the read-buffer sizing was found by.
const (
	benchSmallFiles     = 20_000
	benchSmallFileBytes = 10
)

// BenchmarkCaptureLargeFile is the cost of one proven read: stat, read and
// hash, fstat the descriptor, re-stat the path.
func BenchmarkCaptureLargeFile(b *testing.B) {
	dir := b.TempDir()
	stageFile(b, filepath.Join(dir, "large"), benchLargeFileBytes)

	// ModeExternalSnapshot is how this row asks for a reader with the
	// confirm read OFF; it is the one mode under which a second pass buys
	// nothing, so it is also the only way to price a single pass.
	r := NewReader(OSSource{Root: dir}, model.ModeExternalSnapshot, ReaderOptions{})
	if r.ConfirmsDigest() {
		b.Fatal("this row prices a single pass and the reader is confirming")
	}

	b.SetBytes(benchLargeFileBytes)
	b.ResetTimer()

	for range b.N {
		if got := r.Capture(context.Background(), "large"); !got.Verified() {
			b.Fatalf("outcome = %q (%s)", got.Outcome, got.Reason)
		}
	}
}

// BenchmarkCaptureLargeFileConfirmed is the same read with the confirm read
// armed, which is what every mode except external_snapshot gets. The ratio
// between this row and the one above is the price of catching an in-place
// rewrite whose metadata was restored, and it is the number the ADR quotes
// to an operator.
func BenchmarkCaptureLargeFileConfirmed(b *testing.B) {
	dir := b.TempDir()
	stageFile(b, filepath.Join(dir, "large"), benchLargeFileBytes)

	r := NewReader(OSSource{Root: dir}, model.ModeLiveBestEffort, ReaderOptions{})
	if !r.ConfirmsDigest() {
		b.Fatal("this row prices the confirm read and the reader is not confirming")
	}

	b.SetBytes(benchLargeFileBytes)
	b.ResetTimer()

	for range b.N {
		if got := r.Capture(context.Background(), "large"); !got.Verified() {
			b.Fatalf("outcome = %q (%s)", got.Outcome, got.Reason)
		}
	}
}

// BenchmarkCaptureSmallFileTree prices the per-file cost of the window on
// the tree shape that has the most files, against two hand-rolled baselines
// that do the same hashing with less care. Each ratio isolates one thing:
//
//   - "window" over "no window" is what the two extra stats and the
//     descriptor fstat cost, which is the overhead this design was most
//     worried about;
//   - "no window, fixed buffer" over "no window" is what a read buffer
//     sized by the constant rather than by the stat costs, which is the
//     finding that changed Reader.bufferSize.
func BenchmarkCaptureSmallFileTree(b *testing.B) {
	dir := b.TempDir()
	names := make([]string, benchSmallFiles)
	for i := range names {
		names[i] = fmt.Sprintf("f%05d", i)
		stageFile(b, filepath.Join(dir, names[i]), benchSmallFileBytes)
	}

	b.Run("window", func(b *testing.B) {
		r := NewReader(OSSource{Root: dir}, model.ModeExternalSnapshot, ReaderOptions{})

		b.ReportAllocs()
		b.ResetTimer()

		for range b.N {
			for _, name := range names {
				if got := r.Capture(context.Background(), name); !got.Verified() {
					b.Fatalf("outcome = %q (%s)", got.Outcome, got.Reason)
				}
			}
		}

		reportPerFile(b)
	})

	// The same tree with the confirm read armed, which is what every mode
	// except external_snapshot gets. Two reads per file, one read buffer:
	// the buffer belongs to the capture rather than to the read, so the
	// allocation figure here is the evidence for that.
	b.Run("window, confirmed", func(b *testing.B) {
		r := NewReader(OSSource{Root: dir}, model.ModeLiveBestEffort, ReaderOptions{})

		b.ReportAllocs()
		b.ResetTimer()

		for range b.N {
			for _, name := range names {
				if got := r.Capture(context.Background(), name); !got.Verified() {
					b.Fatalf("outcome = %q (%s)", got.Outcome, got.Reason)
				}
			}
		}

		reportPerFile(b)
	})

	// The same work without the window: one stat, an open, a hash. This is
	// what a straightforward implementation of "read and hash a tree" does,
	// and it is the baseline the window's cost is measured against.
	for _, tc := range []struct {
		name  string
		sized bool
	}{
		{"no window", true},
		{"no window, fixed buffer", false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				for _, name := range names {
					handRolledHash(b, filepath.Join(dir, name), tc.sized)
				}
			}

			reportPerFile(b)
		})
	}
}

// BenchmarkDecide prices the re-read decision itself, which happens once per
// path per run and must therefore cost nothing beside the I/O it avoids.
func BenchmarkDecide(b *testing.B) {
	pol := model.VerificationPolicyFor(model.TrustWeak, model.PresetTrustMetadata)
	now := time.Now()

	prev := Entry{
		Path:         "some/deep/path/report.csv",
		Size:         4096,
		ModTimeNanos: now.Add(-24 * time.Hour).UnixNano(),
		Mode:         0o600,
		Owner:        "1000:1000",
		Digest:       "aaaa",
		DigestAlg:    DigestAlgorithm,
		VerifiedAt:   now.Add(-time.Hour),
	}
	cur := prev
	cur.Digest = ""

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if got := Decide(prev, cur, pol, now); got.Action == "" {
			b.Fatal("Decide returned no action")
		}
	}
}

// handRolledHash is the baseline: stat, open, hash to EOF, no comparison
// window at all. With sized false it allocates the fixed defaultChunkSize
// buffer this package used to allocate per read, whatever the file's length.
func handRolledHash(b *testing.B, path string, sized bool) {
	b.Helper()

	fi, err := os.Stat(path)
	if err != nil {
		b.Fatalf("stat: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer f.Close() //nolint:errcheck // a read-only handle in a benchmark

	size := defaultChunkSize
	if sized {
		size = int(fi.Size()) + 1
		if size < minChunkSize {
			size = minChunkSize
		}
	}

	h := sha256.New()
	buf := make([]byte, size)

	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}

			b.Fatalf("read: %v", readErr)
		}
	}

	if h.Sum(nil) == nil {
		b.Fatal("no digest")
	}
}

// reportPerFile turns an iteration over the whole tree into the per-file
// figure the ADR's table quotes.
func reportPerFile(b *testing.B) {
	b.Helper()

	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*benchSmallFiles)/1000, "us/file")
}

// stageFile writes a file of the requested length with content that is not
// all one byte, so a hash of it cannot be accidentally right.
func stageFile(b *testing.B, path string, size int) {
	b.Helper()

	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i % 251)
	}

	if err := os.WriteFile(path, buf, 0o600); err != nil {
		b.Fatalf("stage %s: %v", path, err)
	}
}
