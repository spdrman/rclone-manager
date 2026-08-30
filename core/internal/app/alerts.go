package app

import (
	"context"

	"github.com/spdrman/rclone-manager/core/internal/alert"
	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// EnableAlerts installs sink as this Service's proactive-alert delivery
// mechanism (docs/EPIC-B-multi-nas.md §71, Work Package 3.5) and reports
// whether alerting is now on.
//
// It returns false, and changes nothing, when the loaded configuration
// has not opted in (alerts.enabled, see internal/config's Alerts block)
// or when sink is nil. Both are ordinary states rather than errors:
// alerting is off by default, and a provider whose platform offers no
// notification capability has no sink to hand over in the first place.
//
// The sink itself always comes from outside core: core/ imports nothing
// from apps/ (§7.1), so the platform's own notifier reaches this package
// as a plain internal/alert.Sink, never as a provider type.
//
// Call this once, while wiring the Service up, before any cycle runs.
// Service is not built to have its dependencies swapped underneath a
// running cycle, and this is a dependency like Transport or Logger, not a
// runtime control.
func (s *Service) EnableAlerts(sink alert.Sink) bool {
	if sink == nil || s.Config == nil || !s.Config.Alerts.Enabled {
		return false
	}
	s.Alerts = alert.NewDispatcher(sink, s.Logger)
	return s.Alerts != nil
}

// evaluateAlerts is Work Package 3.5's evaluation pass, run at the end of
// every RunCycle once the cycle's own work has landed. It computes
// nothing about backups, disks or host keys: it collects verdicts the
// cycle and internal/health have already reached and hands them to the
// dispatcher, which decides which of them are new.
//
// Three sources, all of them existing:
//
//   - BuildHealthReport, exactly as `backup-manager status` calls it, for
//     the FR-24 state and failure count of every configured backup set.
//     Reusing it is the point: there is no second freshness rule here to
//     drift away from internal/health's decideState, and the report is
//     computed from the journal rows this cycle just finished writing.
//
//   - Each set's FreeBytes, already read by that same report, weighed by
//     internal/capacity.AssessCurrent against the Service's own FR-21
//     thresholds. That reuses the one statfs reading the health report
//     took rather than taking a second one, and reuses capacity's own
//     level arithmetic rather than restating it.
//
//   - Each backup set's cycle error, classified through
//     internal/transport.CategoryOf. A changed SSH host key surfaces here
//     as the HostVerification category the rclone adapter already
//     assigned when it refused the connection, so the alert rides on the
//     refusal instead of second-guessing it (§77 invariant #5).
//
// A failure building the health report is logged and drops only the
// health-derived conditions for this pass; the host-key conditions the
// cycle itself produced still go through. Nothing here returns an error,
// because there is nothing a cycle could usefully do about a failed
// alerting pass, and an alerting problem must never be able to fail a
// backup run.
func (s *Service) evaluateAlerts(ctx context.Context, report CycleReport) {
	if s.Alerts == nil {
		return
	}

	conditions := make([]alert.Condition, 0, len(report.Sets))

	// A changed host key is the one condition that comes from the cycle's
	// own outcome rather than from journal state, so it is collected even
	// when the health report below cannot be built.
	for _, set := range report.Sets {
		if category, ok := transport.CategoryOf(set.Err); ok {
			conditions = append(conditions, alert.HostKeyConditions(set.Set.String(), category)...)
		}
	}

	if health, err := s.BuildHealthReport(ctx, VersionInfo{}); err != nil {
		s.logger().Error(ctx, "alert-evaluation", err)
	} else {
		threshold := s.Config.Alerts.RepeatedFailureThreshold
		for _, bs := range health.BackupSets {
			conditions = append(conditions, alert.BackupSetConditions(bs, threshold)...)

			if bs.FreeBytes == nil {
				continue
			}
			assessment, err := capacity.AssessCurrent(capacity.Stat{AvailableBytes: *bs.FreeBytes}, s.Capacity)
			if err != nil {
				s.logger().Error(ctx, "alert-evaluation", err)
				continue
			}
			conditions = append(conditions, alert.StorageConditions(bs.Set.String(), assessment)...)
		}
	}

	s.Alerts.Observe(ctx, conditions, s.now())
}
