package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/apps/common/platform/notify"
	"github.com/spdrman/rclone-manager/core/service"
)

// These tests cover the two things that make alerting a capability rather
// than a feature: the refusal, and the pass-through.
//
// The refusal is the one worth protecting. A sink that accepts alerts from
// a platform with no way to show them would pass every test anybody would
// think to write, because the alerts go in and nothing errors; the only
// observable difference is that the operator is never told. So the first
// test asserts on the failure to build a sink at all, at wiring time,
// which is the moment a person is still watching.
//
// The two adapters model the only two shapes that exist today:
// notifyingAdapter is what a provider with a native notification channel
// looks like, and silentAdapter is what every profile in the table
// actually is right now.

type recordingNotifier struct {
	titles   []string
	messages []string
	err      error
}

func (n *recordingNotifier) Notify(_ context.Context, title, message string) error {
	n.titles = append(n.titles, title)
	n.messages = append(n.messages, message)
	return n.err
}

// notifyingAdapter is a provider that DOES declare the native local
// notification capability, the shape a UGOS adapter takes once its own
// work package lands.
type notifyingAdapter struct {
	capabilities.BasePlatformAdapter
	notifier capabilities.Notifier
}

func (a notifyingAdapter) ID() capabilities.PlatformID { return capabilities.PlatformUGOS }
func (a notifyingAdapter) Capabilities() capabilities.PlatformCapabilities {
	return capabilities.PlatformCapabilities{NativeNotifications: true}
}
func (a notifyingAdapter) Notifier() capabilities.Notifier { return a.notifier }
func (a notifyingAdapter) PlatformInfo(context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: capabilities.PlatformUGOS}, nil
}

// silentAdapter is a provider with no native notification capability at
// all, exactly like apps/generic's own adapter.
type silentAdapter struct {
	capabilities.BasePlatformAdapter
}

func (a silentAdapter) ID() capabilities.PlatformID { return capabilities.PlatformGeneric }
func (a silentAdapter) Capabilities() capabilities.PlatformCapabilities {
	return capabilities.PlatformCapabilities{}
}
func (a silentAdapter) PlatformInfo(context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: capabilities.PlatformGeneric}, nil
}

// TestNewPlatformSink_RefusesAPlatformWithoutTheCapability proves the one
// selected mechanism is never emulated (§22): a provider that has not
// declared NativeNotifications gets a typed refusal at wiring time, not a
// sink that silently drops every alert.
func TestNewPlatformSink_RefusesAPlatformWithoutTheCapability(t *testing.T) {
	_, err := notify.NewPlatformSink(silentAdapter{})
	if !errors.Is(err, capabilities.ErrCapabilityUnsupported) {
		t.Fatalf("NewPlatformSink error = %v, want ErrCapabilityUnsupported", err)
	}
}

// TestPlatformSink_DeliversThroughTheNativeNotifier proves the alert's
// own Title and Message reach the platform notifier unchanged.
func TestPlatformSink_DeliversThroughTheNativeNotifier(t *testing.T) {
	notifier := &recordingNotifier{}
	sink, err := notify.NewPlatformSink(notifyingAdapter{notifier: notifier})
	if err != nil {
		t.Fatalf("NewPlatformSink: %v", err)
	}

	err = sink.DeliverAlert(context.Background(), service.Alert{
		Kind:       "STALE_BACKUP",
		Scope:      "production/postgres-primary",
		Title:      "Backup is stale",
		Message:    "production/postgres-primary has no known-good backup within its stale threshold",
		ObservedAt: time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("DeliverAlert: %v", err)
	}

	if len(notifier.titles) != 1 {
		t.Fatalf("notifier saw %d notifications, want 1", len(notifier.titles))
	}
	if notifier.titles[0] != "Backup is stale" {
		t.Errorf("title = %q, want the alert's own title", notifier.titles[0])
	}
	if notifier.messages[0] != "production/postgres-primary has no known-good backup within its stale threshold" {
		t.Errorf("message = %q, want the alert's own message", notifier.messages[0])
	}
}

// TestPlatformSink_SurfacesADeliveryFailure proves a failed notification
// is reported back to the dispatcher rather than swallowed.
func TestPlatformSink_SurfacesADeliveryFailure(t *testing.T) {
	wantErr := errors.New("notification daemon unavailable")
	sink, err := notify.NewPlatformSink(notifyingAdapter{notifier: &recordingNotifier{err: wantErr}})
	if err != nil {
		t.Fatalf("NewPlatformSink: %v", err)
	}

	if err := sink.DeliverAlert(context.Background(), service.Alert{Title: "t", Message: "m"}); !errors.Is(err, wantErr) {
		t.Fatalf("DeliverAlert error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestPlatformSinkIsTheAlertSink is the compile-time proof that this
// adapter is exactly what core/service's alerting seam accepts, so the
// two halves cannot drift apart silently.
func TestPlatformSinkIsTheAlertSink(t *testing.T) {
	var _ service.AlertSink = notify.PlatformSink{}
}
