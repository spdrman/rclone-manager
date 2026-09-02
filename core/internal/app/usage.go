package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
)

// LocalUsage measures how much space this manager is currently occupying,
// for FR-21's storage cap (issue #286).
//
// It is the join between two packages that do not know about each other:
// internal/lifecycle owns which artifact states hold a local copy, and
// internal/state owns the sum over the rows in those states. Neither can
// import the other (lifecycle imports state), so this is where the
// vocabulary is handed over.
//
// A failure comes back as an error rather than as a zero. A zero usage
// reads as "the whole cap is free", which is the one wrong answer that lets
// a ceiling be breached; callers turn a failure into a Usage with Known
// false, and internal/capacity refuses to assess a configured cap against
// one of those (see capacity.Usage).
func (s *Service) LocalUsage(ctx context.Context) (capacity.Usage, error) {
	bytes, err := s.Journal.LocalBytesInUse(ctx, lifecycle.StatesHoldingLocalCopyStrings())
	if err != nil {
		return capacity.Usage{}, fmt.Errorf("app: measuring this manager's own storage usage: %w", err)
	}
	return capacity.Usage{Bytes: bytes, Known: true}, nil
}

// usageForAssessment is LocalUsage for the callers that must not fail
// outright when the catalog cannot be read: a health report and an alerting
// pass are both meant to say what they do not know rather than produce no
// report at all.
//
// It returns an unmeasured Usage on failure, which internal/capacity
// refuses to assess a configured cap against, so the result is a reading
// marked unavailable rather than an assessment computed from a guess. With
// no cap configured the unmeasured value is not consulted at all and the
// disk reading stands on its own, which is exactly the behaviour every
// deployment had before this existed.
func (s *Service) usageForAssessment(ctx context.Context, event string) capacity.Usage {
	usage, err := s.LocalUsage(ctx)
	if err != nil {
		s.logger().Error(ctx, event, err)
		return capacity.Usage{}
	}
	return usage
}
