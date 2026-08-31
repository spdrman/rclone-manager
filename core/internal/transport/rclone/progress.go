package rclone

import (
	"context"
	"time"

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

// copyWithProgress runs copy, reporting byte progress to whatever
// transport.ProgressReporter is on ctx while it does.
//
// STUB: reports nothing yet. The sampler lands with the implementation
// commit; this exists so the call site and the tests that drive it are
// written first.
func copyWithProgress(ctx context.Context, size int64, copy func(context.Context) error) error {
	_ = transport.ProgressReporterFrom(ctx)
	_ = size
	return copy(ctx)
}
