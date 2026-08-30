package app

import (
	"context"

	"github.com/spdrman/rclone-manager/core/internal/alert"
	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/model"
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
// # Why a pass that cannot see clearly declines to run at all
//
// Dispatcher.Observe treats its argument as the complete picture: a
// condition missing from it has resolved, and is forgotten so the next
// occurrence alerts again. That is the right reading of a pass that
// looked and saw nothing wrong, and the wrong reading of a pass that
// could not look. So this returns without observing anything when the
// health report cannot be built, or when ctx is already done, rather than
// handing over a partial set. Otherwise one transient journal error, or
// an interrupted cycle during shutdown, would silently clear the
// de-duplication state and re-alert every still-unresolved condition on
// the following pass, which is exactly the storm the de-duplication
// exists to prevent.
//
// Nothing here returns an error: there is nothing a cycle could usefully
// do about a failed alerting pass, and an alerting problem must never be
// able to fail a backup run.
func (s *Service) evaluateAlerts(ctx context.Context, report CycleReport) {
	if s.Alerts == nil || ctx.Err() != nil {
		return
	}

	healthReport, err := s.BuildHealthReport(ctx, VersionInfo{})
	if err != nil {
		s.logger().Error(ctx, "alert-evaluation", err)
		return
	}

	conditions := make([]alert.Condition, 0, len(report.Sets))

	// A changed host key comes from the cycle's own outcome rather than
	// from journal state, so it is collected from the cycle report.
	for _, set := range report.Sets {
		if category, ok := transport.CategoryOf(set.Err); ok {
			conditions = append(conditions, alert.HostKeyConditions(set.Set.String(), category)...)
		}
	}

	alertable := s.alertableBackupSets()
	threshold := s.Config.Alerts.RepeatedFailureThreshold

	for _, bs := range healthReport.BackupSets {
		if !alertable[bs.Set] {
			continue
		}
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

	s.Alerts.Observe(ctx, conditions, s.now())
}

// alertableBackupSets is every configured backup set alerting may speak
// about: every one RunCycle actually processes.
//
// A backup set saved disabled (config.BackupSet.Disabled, issue #146's
// "Save disabled" tier) is excluded, and that exclusion is the whole
// reason this exists. BuildHealthReport reports a disabled set like any
// other, correctly: `backup-manager status` should still show it, and
// FR-24 has no notion of a set being switched off. Alerting is a
// different question. A disabled set is never polled, so its newest
// known-good backup ages past stale_after and stays there forever, and
// alerting on that would mean an administrator who deliberately turned a
// backup set off gets told about it on the first pass and then, once the
// condition is dismissed, has a permanently unresolved condition sitting
// in the dispatcher. Nothing is wrong: somebody asked for exactly this.
func (s *Service) alertableBackupSets() map[model.BackupSetID]bool {
	out := map[model.BackupSetID]bool{}
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if bs.Disabled {
				continue
			}
			out[bs.ID] = true
		}
	}
	return out
}
