// Package perfbaseline_test captures the transfer half of the Phase 6
// pre-refactor performance baseline (issue #165, EPIC B #81's performance
// contract): representative backup transfer throughput.
//
// It lives in core/ rather than next to the runtime harness in
// apps/generic because the thing being measured is core's, not any app's:
// the manager-owned transport boundary and the embedded rclone adapter
// behind it (core/internal/transport, core/internal/transport/rclone).
// Measuring it from an app would measure an app's ability to reach it.
//
// The transfer is local-backend, disk to disk, through the exact
// Adapter.CopyToLocal call the lifecycle engine uses. That is deliberate,
// and it is a narrower claim than "how fast does this NAS back up": an
// sftp transfer's number would describe a fixture container's network
// stack and would not be reproducible on another machine, while this one
// is reproducible and still walks every layer the refactor could add a
// hop to. The metric exists to catch a data-path regression introduced by
// moving code, which is what EPIC B #81's performance contract asks of
// it, not to characterise real hardware.
//
// Skipped unless PERF_BASELINE=1, so `go test ./...` (including
// scripts/architecture/verify-core-without-apps.sh's own run) never pays
// for it. It imports nothing from apps/ and nothing outside core/, so the
// dependency rule is unaffected.
package perfbaseline_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

const (
	// workloadID must match the runtime harness's own, since the two
	// halves are recorded as one baseline.
	workloadID = "phase6-baseline-v1"

	// artifactBytes is the size of each copied artifact: large enough
	// that per-call overhead is not what is being timed, small enough
	// that a capture stays quick and fits comfortably in page cache on
	// any machine likely to run it.
	artifactBytes = 256 << 20

	// repetitions is how many copies are timed. The recorded number is
	// the median, so a single unlucky copy (a background process, a
	// checkpoint) cannot set the baseline.
	repetitions = 5
)

type transferRecord struct {
	Workload            string  `json:"workload"`
	Backend             string  `json:"backend"`
	ArtifactBytes       int64   `json:"artifact_bytes"`
	Repetitions         int     `json:"repetitions"`
	MedianMBPerSecond   float64 `json:"median_mb_per_second"`
	SlowestMBPerSecond  float64 `json:"slowest_mb_per_second"`
	FastestMBPerSecond  float64 `json:"fastest_mb_per_second"`
	MedianDurationMs    float64 `json:"median_duration_ms"`
	SpreadPercentOfMedn float64 `json:"spread_percent_of_median"`
}

func TestCaptureTransferBaseline(t *testing.T) {
	if os.Getenv("PERF_BASELINE") != "1" {
		t.Skip("perf baseline harness: set PERF_BASELINE=1 to run it (scripts/perf/capture-baseline.sh does)")
	}

	ctx := context.Background()
	adapter := rclone.New()

	remoteRoot := t.TempDir()
	localRoot := t.TempDir()
	src := transport.Source{ID: "perf-baseline-local", Type: "local", Root: remoteRoot}

	// Incompressible content: a run of zeroes would let any layer that
	// ever learns to compress or sparse-fill report a throughput the real
	// data path could never reach.
	payload := make([]byte, artifactBytes)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generate payload: %v", err)
	}
	remotePath := "artifact.dump"
	if err := os.WriteFile(filepath.Join(remoteRoot, remotePath), payload, 0o644); err != nil {
		t.Fatalf("write source artifact: %v", err)
	}

	rates := make([]float64, 0, repetitions)
	durations := make([]float64, 0, repetitions)
	for i := 0; i < repetitions; i++ {
		dst := filepath.Join(localRoot, fmt.Sprintf("artifact-%02d.partial", i))
		start := time.Now()
		res, err := adapter.CopyToLocal(ctx, src, remotePath, dst)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("CopyToLocal: %v", err)
		}
		if res.BytesTransferred != artifactBytes {
			t.Fatalf("CopyToLocal copied %d bytes, want %d", res.BytesTransferred, int64(artifactBytes))
		}
		if err := os.Remove(dst); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		rates = append(rates, float64(artifactBytes)/(1<<20)/elapsed.Seconds())
		durations = append(durations, elapsed.Seconds()*1000)
	}

	sort.Float64s(rates)
	sort.Float64s(durations)
	median := rates[len(rates)/2]

	rec := transferRecord{
		Workload:            workloadID,
		Backend:             "rclone local backend, disk to disk, warm page cache",
		ArtifactBytes:       artifactBytes,
		Repetitions:         repetitions,
		MedianMBPerSecond:   round3(median),
		SlowestMBPerSecond:  round3(rates[0]),
		FastestMBPerSecond:  round3(rates[len(rates)-1]),
		MedianDurationMs:    round3(durations[len(durations)/2]),
		SpreadPercentOfMedn: round3((rates[len(rates)-1] - rates[0]) / median * 100),
	}

	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	t.Logf("transfer baseline:\n%s", out)
	if path := os.Getenv("PERF_BASELINE_OUT"); path != "" {
		if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}
