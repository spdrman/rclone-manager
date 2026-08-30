package app

import (
	"context"
	"time"

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

// AdoptAlerts carries an already-wired dispatcher onto this Service and
// reports whether alerting is now on. It exists for exactly one caller:
// core/service's in-process configuration hot reload, which builds a
// replacement Service around a freshly re-read config file and must not
// throw away what the previous one already knows.
//
// What it carries is the de-duplication state. A dispatcher remembers
// which conditions are currently firing, so rebuilding one from scratch
// would re-alert every still-unresolved condition the next time a cycle
// ran, purely because somebody added a backup set.
//
// What it does NOT carry is the decision to alert at all. That is
// re-read from this Service's own configuration, every time, exactly like
// EnableAlerts reads it: the hot reload is the one moment an edited
// alerts.enabled can take effect, so an administrator who turned alerting
// off gets it off here rather than at the next process restart. Carrying
// the dispatcher across regardless is what made half the alerts block
// hot-reload and the other half not.
//
// A nil dispatcher (alerting was off before the reload) returns false and
// changes nothing, which is the caller's signal to decide the question
// from its own sink instead.
func (s *Service) AdoptAlerts(d *alert.Dispatcher) bool {
	if d == nil || s.Config == nil || !s.Config.Alerts.Enabled {
		return false
	}
	s.Alerts = d
	return true
}

// AlertTick is one alerting pass driven by something other than a cycle.
//
// Alerting cannot only run at the end of RunCycle. §76 invariant 11 is
// "process liveness is not evidence of backup freshness", and the stale
// alert exists precisely for the daemon that is up and not producing
// backups: one hung transfer blocks the rest of that cycle's sequential
// loop, so the pass at the end of it never runs, and the one condition
// most worth reporting is the one guaranteed to be silent. A shutting-down
// or repeatedly-interrupted daemon has the same problem, since the pass
// returns early on a cancelled context.
//
// So a caller that owns a timer (Daemon here, RunOnSchedule in
// core/service) also ticks this, independently of whether any cycle
// finishes. It weighs the same health report the in-cycle pass does, and
// nothing else: an empty CycleReport carries no per-set cycle outcome, so
// this pass says nothing about host keys either way, and evaluateAlerts
// marks them unevaluated rather than resolving them (see there).
//
// The cost is that a health-derived condition can be observed while a
// cycle is mid-flight, so the report may be slightly behind the journal.
// Staleness and storage pressure are both measured in hours, so being one
// artifact behind changes no verdict.
func (s *Service) AlertTick(ctx context.Context) {
	s.evaluateAlerts(ctx, CycleReport{})
}

// runAlertTicks repeats AlertTick every interval until ctx is done. It is
// the same timer shape Daemon's own loop uses, deliberately: one timer
// per tick, reset only after the pass returns, so a slow alerting pass
// makes the next one late rather than overlapping it.
func (s *Service) runAlertTicks(ctx context.Context, interval time.Duration) {
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.AlertTick(ctx)
	}
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
// # Why a pass that cannot see clearly says so
//
// Dispatcher.Observe treats its conditions argument as the complete
// picture: a condition missing from it has resolved, and is forgotten so
// the next occurrence alerts again. That is the right reading of a pass
// that looked and saw nothing wrong, and the wrong reading of a pass that
// could not look. There are four ways this pass cannot look, and each is
// answered honestly rather than as "nothing is wrong":
//
//   - the health report cannot be built, or ctx is already done: nothing
//     at all is observed, because nothing at all was seen. One transient
//     journal error, or an interrupted cycle during shutdown, would
//     otherwise clear the whole de-duplication state and re-alert every
//     still-unresolved condition on the following pass.
//
//   - a backup set's free space could not be read (an unmounted volume, a
//     bind mount that disappeared, which is exactly the incident an
//     operator most needs telling about): that one set's storage
//     condition is reported as unevaluated, so a still-true
//     CRITICAL_STORAGE_PRESSURE is neither forgotten nor re-alerted when
//     the mount returns. The other sets are still evaluated normally; one
//     broken mount must not suppress alerting for everything else.
//
//   - this pass has no positive evidence about a backup set's host key.
//     Absence is not resolution here, unlike the health-derived
//     conditions: §77 invariant 5 says re-trusting a changed key takes an
//     explicit administrator action, so HOST_KEY_CHANGED resolves only on
//     evidence that the connection now verifies, which is a cycle that
//     ran that set to completion with no error at all. A set that failed
//     for some other reason, was skipped, or was not part of this pass
//     (every set, on an out-of-cycle AlertTick) is reported unevaluated.
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

	alertable := s.alertableBackupSets()
	conditions := make([]alert.Condition, 0, len(report.Sets))
	var unevaluated []alert.Subject

	// A changed host key comes from the cycle's own outcome rather than
	// from journal state, so it is collected from the cycle report. A set
	// that finished the cycle with no error connected, which is the
	// positive evidence its key verifies again; anything else says
	// nothing either way.
	verified := map[model.BackupSetID]bool{}
	for _, set := range report.Sets {
		if set.Err == nil {
			verified[set.Set] = true
			continue
		}
		if category, ok := transport.CategoryOf(set.Err); ok {
			conditions = append(conditions, alert.HostKeyConditions(set.Set.String(), category)...)
		}
	}
	for id := range alertable {
		if !verified[id] {
			unevaluated = append(unevaluated, alert.Subject{Kind: alert.HostKeyChanged, Scope: id.String()})
		}
	}

	threshold := s.Config.Alerts.RepeatedFailureThreshold

	for _, bs := range healthReport.BackupSets {
		if !alertable[bs.Set] {
			continue
		}
		conditions = append(conditions, alert.BackupSetConditions(bs, threshold)...)

		storage := alert.Subject{Kind: alert.CriticalStoragePressure, Scope: bs.Set.String()}
		if bs.FreeBytes == nil {
			// BuildHealthReport leaves this nil exactly when
			// capacity.StatPath failed, so the disk was not read at all
			// this pass. Saying nothing about it is the whole point.
			unevaluated = append(unevaluated, storage)
			continue
		}
		// health's FreeBytes is app/health.go's own copy of
		// capacity.Stat.AvailableBytes (the space this account may
		// actually use), not the filesystem's raw free figure, which is
		// why it is safe to weigh as AvailableBytes here. The two are
		// different numbers on any filesystem with reserved blocks, and
		// capacity.Stat's doc says so: if health ever starts carrying the
		// raw one, this line has to change with it.
		assessment, err := capacity.AssessCurrent(capacity.Stat{AvailableBytes: *bs.FreeBytes}, s.Capacity)
		if err != nil {
			s.logger().Error(ctx, "alert-evaluation", err)
			unevaluated = append(unevaluated, storage)
			continue
		}
		conditions = append(conditions, alert.StorageConditions(bs.Set.String(), assessment)...)
	}

	s.Alerts.Observe(ctx, conditions, unevaluated, s.now())
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
