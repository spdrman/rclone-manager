// Package notify is the delivery half of Work Package 3.5's proactive
// alerting (docs/EPIC-B-multi-nas.md §71): it adapts a provider's own
// local notification capability into the single alert sink core/service
// accepts.
//
// # Which of §71's two mechanisms this is
//
// §71 states a preference: "official UGOS local notification capability
// if available/documented", otherwise "one explicit opt-in generic alert
// mechanism". This package implements the first. The seam a UGOS (or any
// other) adapter declares that capability through already exists in this
// repository as capabilities.Notifier, so alerting rides on the same
// PlatformAdapter contract every other platform-specific behaviour in
// this product goes through, rather than growing a second, parallel one.
//
// It is deliberately the whole mechanism. There is no webhook, no
// command hook, no channel selection and no fallback chain: §71 says "do
// not add a broad notification framework in v1", and the version of that
// rule that actually holds is the one where a second mechanism has
// nowhere to be plugged in.
//
// # Never emulated
//
// §22 requires an unsupported capability to be explicit rather than
// faked. A provider that has not declared NativeNotifications gets a
// typed refusal from NewPlatformSink at wiring time, so the process
// starts with alerting visibly off instead of with a sink that accepts
// every alert and drops it. That mirrors capabilities.BasePlatformAdapter's
// own null-object Notifier, which fails with ErrCapabilityUnsupported
// rather than returning nil.
package notify

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/core/service"
)

// PlatformSink delivers a core alert through one platform's native local
// notification capability. It is the one delivery mechanism Work Package
// 3.5 selected; see the package doc.
type PlatformSink struct {
	notifier capabilities.Notifier
}

// NewPlatformSink returns the alert sink for adapter's platform.
//
// It returns capabilities.ErrCapabilityUnsupported when adapter has not
// declared NativeNotifications, or when its Notifier() is nil. A caller
// wiring a provider up should treat that as "this platform cannot deliver
// proactive alerts", report it once at startup, and carry on: alerting is
// an addition to the product, never a precondition for running backups.
func NewPlatformSink(adapter capabilities.PlatformAdapter) (PlatformSink, error) {
	if adapter == nil {
		return PlatformSink{}, fmt.Errorf("notify: a platform adapter is required: %w", capabilities.ErrCapabilityUnsupported)
	}
	if !adapter.Capabilities().NativeNotifications {
		return PlatformSink{}, fmt.Errorf("notify: %s does not provide native notifications: %w", adapter.ID(), capabilities.ErrCapabilityUnsupported)
	}
	notifier := adapter.Notifier()
	if notifier == nil {
		return PlatformSink{}, fmt.Errorf("notify: %s declares native notifications but returned no notifier: %w", adapter.ID(), capabilities.ErrCapabilityUnsupported)
	}
	return PlatformSink{notifier: notifier}, nil
}

// DeliverAlert hands one alert's Title and Message to the platform
// notifier unchanged. A notifier failure is returned rather than
// swallowed, so the dispatcher can log that nobody was actually told.
//
// The zero PlatformSink (one nobody built through NewPlatformSink) has no
// notifier and refuses rather than pretending to deliver, for the same
// no-emulation reason NewPlatformSink refuses a platform without the
// capability.
func (s PlatformSink) DeliverAlert(ctx context.Context, a service.Alert) error {
	if s.notifier == nil {
		return fmt.Errorf("notify: no platform notifier is wired: %w", capabilities.ErrCapabilityUnsupported)
	}
	if err := s.notifier.Notify(ctx, a.Title, a.Message); err != nil {
		return fmt.Errorf("notify: delivering the %s alert for %s: %w", a.Kind, a.Scope, err)
	}
	return nil
}

var _ service.AlertSink = PlatformSink{}
