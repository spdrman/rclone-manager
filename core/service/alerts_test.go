package service

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

type countingSink struct {
	mu        sync.Mutex
	delivered []Alert
}

func (s *countingSink) DeliverAlert(_ context.Context, a Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, a)
	return nil
}

func alertingTestConfig(sources ...config.Source) *config.Config {
	cfg := testConfig(sources...)
	cfg.Alerts = config.Alerts{Enabled: true, RepeatedFailureThreshold: 3}
	return cfg
}

// TestEnableAlerts_HonoursTheConfiguredOptIn proves apps/ can only turn
// proactive alerting on for a configuration that actually asked for it.
func TestEnableAlerts_HonoursTheConfiguredOptIn(t *testing.T) {
	off := New(testConfig(config.Source{Name: "alpha"}), openTestJournal(t), nil, nil)
	if off.EnableAlerts(&countingSink{}) {
		t.Error("EnableAlerts returned true for a config with alerts.enabled unset")
	}

	on := New(alertingTestConfig(config.Source{Name: "alpha"}), openTestJournal(t), nil, nil)
	if !on.EnableAlerts(&countingSink{}) {
		t.Error("EnableAlerts returned false for a config with alerts.enabled: true")
	}
}

// TestEnableAlerts_RefusesANilSink proves there is no way to end up with
// alerting "on" and no mechanism behind it.
func TestEnableAlerts_RefusesANilSink(t *testing.T) {
	svc := New(alertingTestConfig(config.Source{Name: "alpha"}), openTestJournal(t), nil, nil)
	if svc.EnableAlerts(nil) {
		t.Error("EnableAlerts(nil) returned true")
	}
}

// TestAlertIsProviderAgnostic proves the alert shape apps/ sees carries
// nothing from core/internal: §7.2 says this package exposes only plain,
// provider-agnostic types across that boundary.
func TestAlertIsProviderAgnostic(t *testing.T) {
	alertType := reflect.TypeOf(Alert{})
	for i := 0; i < alertType.NumField(); i++ {
		f := alertType.Field(i)
		if pkg := f.Type.PkgPath(); strings.Contains(pkg, "core/internal") {
			t.Errorf("Alert.%s is %s, from %s: internal types must never cross this boundary", f.Name, f.Type, pkg)
		}
	}
}
