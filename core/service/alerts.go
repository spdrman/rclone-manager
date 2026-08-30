package service

import (
	"context"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/alert"
)

// Alert is the plain, provider-agnostic shape of one proactive
// notification (docs/EPIC-B-multi-nas.md §71, Work Package 3.5), as an
// AlertSink outside core/ receives it.
//
// Title and Message are what a platform notifier actually renders. Kind
// and Scope are carried alongside as plain strings so a sink can log or
// route on them without parsing the rendered text back apart, and as
// strings rather than internal/alert's own Kind type because §7.2 says
// nothing internal crosses this boundary: an apps/ package must be able
// to name every field of this struct, and it cannot name a type from
// core/internal.
type Alert struct {
	// Kind is one of STALE_BACKUP, REPEATED_FAILURE, HOST_KEY_CHANGED or
	// CRITICAL_STORAGE_PRESSURE: §71's four conditions, and only those.
	Kind string

	// Scope is the backup set the alert is about, as its
	// source/backup-set identifier.
	Scope string

	// Title is the short headline for the notification.
	Title string

	// Message is the operator-facing explanation.
	Message string

	// ObservedAt is when the evaluation pass that fired this alert ran.
	ObservedAt time.Time
}

// AlertSink is the single delivery seam for proactive alerts. A provider
// app implements it over whatever local notification capability its
// platform actually offers (see apps/common/platform/notify for the
// adapter over capabilities.Notifier), and a platform with none simply
// never installs one.
//
// There is exactly one sink per BackupService, and no way to add a
// second: §71 rules out a broad notification framework in v1, and
// internal/alert's own gate test keeps that structurally true on the
// other side of this boundary.
//
// A sink must return an error rather than silently dropping a
// notification it could not deliver. The dispatcher logs that failure and
// does not retry it, so a swallowed error would leave nobody, operator or
// log, any the wiser.
type AlertSink interface {
	DeliverAlert(ctx context.Context, a Alert) error
}

// EnableAlerts installs sink as this BackupService's proactive-alert
// delivery mechanism and reports whether alerting is now on. It returns
// false, changing nothing, when sink is nil or when the loaded
// configuration has not opted in (alerts.enabled: true).
//
// Call it once, during wiring, before the process starts serving requests
// or ticking the scheduler: this is a dependency, like the transport and
// the logger New already takes, not a runtime control. It is a separate
// method rather than another New parameter for the same reason
// configPath is set after New in Open: the sink comes from the provider
// app's platform adapter, which core/ cannot name, so only a caller above
// this package can supply one, and most callers (every core/ test, the
// CLI) have none at all.
func (b *BackupService) EnableAlerts(sink AlertSink) bool {
	if sink == nil {
		return false
	}
	b.alertSink = sink
	return b.state.Load().inner.EnableAlerts(sinkAdapter{sink: sink})
}

// sinkAdapter translates internal/alert's Sink into this package's
// provider-agnostic AlertSink, which is the only direction that
// translation can go: internal/alert is unreachable from apps/, and this
// package is the seam §7.2 puts between them.
type sinkAdapter struct {
	sink AlertSink
}

func (a sinkAdapter) Deliver(ctx context.Context, in alert.Alert) error {
	return a.sink.DeliverAlert(ctx, Alert{
		Kind:       in.Kind.String(),
		Scope:      in.Scope,
		Title:      in.Title,
		Message:    in.Message,
		ObservedAt: in.ObservedAt,
	})
}

var _ alert.Sink = sinkAdapter{}
