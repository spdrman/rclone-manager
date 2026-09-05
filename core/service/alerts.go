package service

import (
	"context"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/alert"
)

// This file is the whole of proactive alerting as anything outside core/
// can see it (docs/EPIC-B-multi-nas.md §71, issue #159): one plain Alert
// shape, one sink interface, one way to install a sink, and the adapter
// that carries internal/alert's own Alert across the boundary.
//
// Alerting is the one part of this product that reaches a person who is
// not looking at it, so the design here is mostly about what it refuses
// to become. There is one sink and no registry, no fan-out and no
// per-condition routing, because §71 rules out a notification framework
// in v1 and the cheapest way to keep a framework from growing is to give
// it nowhere to start. Every field crossing the seam is a plain string or
// a time, so a platform notifier can be written against this file alone
// without ever naming a type from core/internal.
//
// The direction of the adapter at the bottom is forced rather than
// chosen. internal/alert cannot import this package (that is the seam
// running the wrong way), and nothing in apps/ can import internal/alert
// at all, so the translation has to live on this side, once, in the one
// package that is allowed to name both vocabularies.

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
// notification it could not deliver. That error is the only thing
// standing between a notification channel that is down and permanent
// silence: the dispatcher logs it and retries the condition on a later
// pass, rate-limited, so a swallowed error would instead mark the
// condition delivered and leave nobody, operator or log, any the wiser.
//
// A sink does not need a timeout of its own. The dispatcher gives every
// delivery a deadline and calls it holding no lock, so a slow notifier
// costs one slow alerting pass rather than a stalled backup cycle.
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
// CLI) have none at all. It is not safe to call once cycles are running:
// it writes the wrapped Service's dispatcher, which a running cycle's
// alerting pass reads.
//
// The sink is remembered even when this returns false for a
// configuration that has not opted in. That is deliberate rather than
// sloppy: CreateBackupSet re-reads the config file, so an operator who
// sets alerts.enabled: true and then adds a backup set gets alerting on
// from that moment, and it can only be turned on from a sink that was
// kept. "Off" here means no dispatcher exists, never that the mechanism
// was thrown away.
func (b *BackupService) EnableAlerts(sink AlertSink) bool {
	if sink == nil {
		return false
	}

	// configMu, because CreateBackupSet reads b.alertSink under it while
	// deciding what the hot-reloaded Service's alerting should be.
	b.configMu.Lock()
	defer b.configMu.Unlock()

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
