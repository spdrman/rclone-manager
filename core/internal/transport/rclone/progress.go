package rclone

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/fs/accounting"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// progressSampleInterval is how often a copy in flight is sampled for
// transport.ByteProgress.
//
// It is a var, not a const, so this package's own tests can shorten it;
// nothing outside this package can reach it, and nothing in production
// overrides it. 250ms is chosen against what consumes these samples: the
// browser polls operation state on a much slower cadence, so sampling
// faster buys nothing a client could see, and sampling slower would leave
// a short transfer with no reading at all except its final one.
var progressSampleInterval = 250 * time.Millisecond

// copyGroupCounter names each copy's own rclone statistics group.
var copyGroupCounter uint64

// copyWithProgress runs copy, reporting byte progress to whatever
// transport.ProgressReporter is on ctx while it does.
//
// # Where the numbers come from
//
// rclone already counts these: fs/accounting wraps the source reader, so
// every chunk the copy consumes lands in a StatsInfo before the copy
// returns. Nothing here re-implements that; it reads it and translates it
// into transport.ByteProgress, which is the manager's own vocabulary. That
// translation is the whole point of doing it in this file: this package is
// the only one in the repository allowed to name an rclone type (FR-3,
// failure-safety invariant 13), so a *accounting.StatsInfo must not, and
// does not, travel any further than this function.
//
// # A statistics group per copy
//
// The group is unique per call rather than shared, so the counters read
// below belong to this copy and nothing else. rclone offers no exported
// way to delete a group again, so ResetCounters is what releases the
// per-transfer state this one accumulated; the map it stays in is bounded
// by rclone's own --max-stats-groups (1000 by default, oldest evicted), so
// the residue is a bounded number of empty entries rather than a leak. A
// single shared group would avoid even that, at the price of two
// concurrent copies reading each other's bytes, and correctness under
// concurrency is worth more here than a thousand empty map entries.
//
// # A sampler rather than a callback
//
// rclone exposes no per-chunk progress hook, so this polls. The sampler
// runs on its own goroutine for the length of the copy, and one final
// sample is taken after the copy returns successfully: without it a small
// artifact that finished between two ticks would leave its last reported
// reading short of its real size, which is a bar that never fills.
//
// With no reporter on ctx this does nothing at all beyond calling copy,
// which is the case for every caller in this repository outside issue
// #221's own wiring.
func copyWithProgress(ctx context.Context, size int64, copy func(context.Context) error) error {
	reporter := transport.ProgressReporterFrom(ctx)
	if reporter == nil {
		return copy(ctx)
	}

	group := fmt.Sprintf("backup-manager-copy-%d", atomic.AddUint64(&copyGroupCounter, 1))
	ctx = accounting.WithStatsGroup(ctx, group)
	stats := accounting.StatsGroup(ctx, group)
	defer stats.ResetCounters()

	started := time.Now()
	sample := func() transport.ByteProgress {
		done := stats.GetBytes()
		p := transport.ByteProgress{BytesTransferred: done, BytesTotal: size}
		// The average rate over this copy so far. Computed here rather
		// than read off rclone's own moving average because that one is
		// maintained by a background loop whose lifetime is tied to the
		// transfer's, and a rate this function cannot explain is a number
		// nobody can defend. Left at zero until there is something to
		// divide: a rate of zero reported before any bytes have moved
		// would read as "stalled", and transport.ByteProgress documents
		// zero as "no rate yet" for exactly that reason.
		if elapsed := time.Since(started); done > 0 && elapsed > 0 {
			p.BytesPerSecond = int64(float64(done) / elapsed.Seconds())
		}
		return p
	}

	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		ticker := time.NewTicker(progressSampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				reporter.CopyProgress(sample())
			}
		}
	}()

	err := copy(ctx)

	// Stop and JOIN before the final sample, so the last reading a
	// reporter sees is genuinely the last one: a sampler still running
	// could otherwise deliver an older, smaller count after it.
	close(stop)
	sampler.Wait()

	if err == nil {
		reporter.CopyProgress(sample())
	}
	return err
}
