package rclone

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/tests/bwlimit"
)

// TestCopyToLocal_ReportsIntermediateProgressForARealTransfer is the
// evidence issue #221 asks for, and it is deliberately not a test that can
// pass by only ever seeing 0 and 100.
//
// A progress test whose only readings are "nothing yet" and "all done"
// proves nothing at all: those two are exactly what the operations table
// could already answer before any of this existed. So this drives a real
// rclone copy through the real Adapter and requires at least one reading
// STRICTLY BETWEEN the two, plus monotonicity across the whole run.
//
// # Why throttled rather than enormous
//
// The intermediate reading has to be observed reliably on every machine
// this gate runs on, and "make the file big enough" is a race against
// whatever disk is underneath: on a fast NVMe a file large enough to
// guarantee several sampling windows is also large enough to be unfriendly
// in a test suite. So this turns rclone's own bandwidth limiter down and
// uses a small payload instead. A slow, small, real transfer exercises
// exactly the same accounting path a fast huge one would, and it makes the
// timing a fact rather than a hope.
//
// --bwlimit is the right lever HERE and the wrong one for a cancellation
// test, which is worth knowing before copying this file's shape into one.
// It throttles by parking inside rclone (Account.accountRead and fshttp's
// dialer both wait with WaitN(context.Background(), n)), so a copy under it
// is slow AND partly uninterruptible. That is fine for sampling progress
// and fatal for proving an interruption; gate_test.go's
// MidTransferCancellation used to use it and did not prove what it said
// (#414). That row runs over a slow link now instead.
func TestCopyToLocal_ReportsIntermediateProgressForARealTransfer(t *testing.T) {
	// "1M" is rclone's own spelling for 1 MiB/s. It is written as a
	// suffixed string on purpose: a bare number in an rclone bandwidth
	// limit is KiB, not bytes, so "1048576" would set a gigabyte-a-second
	// limit that throttles nothing and would leave this test passing on
	// timing luck alone.
	const bwLimit = "1M"
	const size = 1024 * 1024 // 1 MiB, so ~1s of transfer at the limit above
	const sampleInterval = 20 * time.Millisecond

	// ~50 sampling windows inside the transfer. The assertion below needs
	// one; this margin is what keeps a slow or loaded machine from turning
	// a real failure into a coin flip in either direction.
	restore := progressSampleInterval
	progressSampleInterval = sampleInterval
	t.Cleanup(func() { progressSampleInterval = restore })

	// bwlimit.Throttle rather than StartTokenBucket-and-put-it-back, because
	// putting it back that way does not work and this test's own 1MiB/s
	// limit was outliving it. See that package's doc.
	bwCtx := bwlimit.Throttle(t, context.Background(), bwLimit)

	srcRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcRoot, "big.bin"), make([]byte, size), 0o644); err != nil {
		t.Fatalf("writing the source artifact: %v", err)
	}

	var mu sync.Mutex
	var samples []transport.ByteProgress
	ctx := transport.WithProgressReporter(bwCtx, transport.ProgressReporterFunc(func(p transport.ByteProgress) {
		mu.Lock()
		samples = append(samples, p)
		mu.Unlock()
	}))

	dst := filepath.Join(t.TempDir(), "big.bin.partial")
	started := time.Now()
	result, err := New().CopyToLocal(ctx, transport.Source{ID: "progress-local", Type: "local", Root: srcRoot}, "big.bin", dst)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("CopyToLocal: %v", err)
	}
	if result.BytesTransferred != size {
		t.Fatalf("BytesTransferred = %d, want %d", result.BytesTransferred, size)
	}

	mu.Lock()
	defer mu.Unlock()
	t.Logf("throttled to %s/s, %d B payload, copied in %v, %d progress samples", bwLimit, size, elapsed, len(samples))

	// A throttle that did not actually throttle would make every timing
	// assumption below meaningless, and an intermediate reading observed
	// under it would be luck rather than evidence. Fail on that directly
	// rather than letting it show up as a mysterious flake later.
	if minimum := 500 * time.Millisecond; elapsed < minimum {
		t.Fatalf("the copy finished in %v, faster than the %v the %s/s limit implies for %d bytes; "+
			"the bandwidth limit did not take effect, so this test would prove nothing about sampling",
			elapsed, minimum, bwLimit, size)
	}

	if len(samples) == 0 {
		t.Fatal("the copy reported no progress at all")
	}

	// Monotonic: a byte count that goes backwards is a bar that jumps
	// backwards, and it would mean the sampler is reading something other
	// than this copy.
	for i := 1; i < len(samples); i++ {
		if samples[i].BytesTransferred < samples[i-1].BytesTransferred {
			t.Fatalf("sample %d reports %d bytes after sample %d reported %d; progress went backwards",
				i, samples[i].BytesTransferred, i-1, samples[i-1].BytesTransferred)
		}
	}

	// The total is the artifact's real size on every sample, so a client
	// can compute a fraction from any one of them on its own.
	for i, s := range samples {
		if s.BytesTotal != size {
			t.Fatalf("sample %d reports BytesTotal = %d, want %d", i, s.BytesTotal, size)
		}
	}

	// The reading this whole test exists for.
	intermediate := 0
	for _, s := range samples {
		if s.BytesTransferred > 0 && s.BytesTransferred < size {
			intermediate++
		}
	}
	if intermediate == 0 {
		t.Fatalf("no sample fell strictly between 0 and %d bytes; the samples were %v. "+
			"A progress feed that only ever reads 0 or 100%% is exactly what issue #221 reports as useless.",
			size, byteCounts(samples))
	}
	t.Logf("%d of %d samples were strictly intermediate", intermediate, len(samples))

	// The last reading is the finished one, so a client that polls once
	// more after the copy ends does not see a permanently unfinished bar.
	last := samples[len(samples)-1]
	if last.BytesTransferred != size {
		t.Errorf("final sample reports %d of %d bytes; the last reading of a completed copy must be the whole artifact", last.BytesTransferred, size)
	}

	// A rate has to have been measured at some point: it is one of the
	// readings §52 asks a poll response to carry.
	measuredRate := false
	for _, s := range samples {
		if s.BytesPerSecond > 0 {
			measuredRate = true
			break
		}
	}
	if !measuredRate {
		t.Errorf("no sample reported a transfer rate at all, over %v of throttled transfer", elapsed)
	}
}

// TestCopyToLocal_WithNoReporterIsUnchanged pins the other half: with no
// reporter on the context, the copy still copies. Every caller in this
// repository outside issue #221's own wiring is in that position.
func TestCopyToLocal_WithNoReporterIsUnchanged(t *testing.T) {
	srcRoot := t.TempDir()
	const payload = "unreported payload"
	if err := os.WriteFile(filepath.Join(srcRoot, "small.bin"), []byte(payload), 0o644); err != nil {
		t.Fatalf("writing the source artifact: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "small.bin.partial")
	result, err := New().CopyToLocal(context.Background(),
		transport.Source{ID: "unreported-local", Type: "local", Root: srcRoot}, "small.bin", dst)
	if err != nil {
		t.Fatalf("CopyToLocal: %v", err)
	}
	if result.BytesTransferred != int64(len(payload)) {
		t.Fatalf("BytesTransferred = %d, want %d", result.BytesTransferred, len(payload))
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading the copy: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("copied content = %q, want %q", got, payload)
	}
}

func byteCounts(samples []transport.ByteProgress) []int64 {
	out := make([]int64, 0, len(samples))
	for _, s := range samples {
		out = append(out, s.BytesTransferred)
	}
	return out
}
