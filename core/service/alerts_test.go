// This file covers the one moment alerting can change its mind: a
// configuration reload in a running process.
//
// Installing a sink is wiring and would barely need a test. What needed
// one is that alerts.enabled is read at two different times, once when a
// sink is installed and again on every configuration write, and the
// dispatcher holding which conditions are currently firing has to survive
// the second without either going silent or re-announcing everything. All
// three directions are here (stays on, turns off, turns on) because the
// bug this file was written after was directional: the opt-in was ignored
// on reload while a sibling field from the same block hot-reloaded
// correctly.
//
// The last test is the odd one out and the most important. It proves
// alerting still reaches a person while a cycle is wedged, which is the
// only situation where a stale-backup alert is the thing an operator
// needs, and precisely the situation a design sharing one loop or one
// lock would have silenced.
package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
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

func (s *countingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.delivered)
}

func (s *countingSink) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.delivered))
	for _, a := range s.delivered {
		out = append(out, a.Kind)
	}
	return out
}

// writeAlertingConfigFile is writeTestConfigFile (open_test.go) with this
// work package's opt-in and a stale_after short enough that one completed
// cycle is stale by the time the next pass looks at it, so a test can
// drive a real condition through a real Open'd BackupService without
// injecting a clock the production wiring has no way to inject.
func writeAlertingConfigFile(t *testing.T, enabled bool) string {
	t.Helper()
	configPath := writeTestConfigFile(t)

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	updated := strings.Replace(string(content), "stale_after: 24h", "stale_after: 1ms", 1) +
		"alerts:\n  enabled: " + strconv.FormatBool(enabled) + "\n  repeated_failure_threshold: 3\n"
	if err := os.WriteFile(configPath, []byte(updated), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The backup set's local_path, which the fixture only names and a
	// cycle would otherwise create on its first run. It has to exist
	// before any alerting pass, because capacity.StatPath is what gives
	// the health report its FreeBytes and a pass that cannot stat a
	// directory correctly declines to say anything about that set's
	// storage at all.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(configPath), "local"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return configPath
}

// setAlertsEnabled edits alerts.enabled in the config file on disk, the
// way an administrator would, so the next CreateBackupSet re-reads it.
func setAlertsEnabled(t *testing.T, configPath string, enabled bool) {
	t.Helper()
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	from := "enabled: " + strconv.FormatBool(!enabled)
	to := "enabled: " + strconv.FormatBool(enabled)
	updated := strings.Replace(string(content), from, to, 1)
	if updated == string(content) {
		t.Fatalf("config file has no %q line to flip", from)
	}
	if err := os.WriteFile(configPath, []byte(updated), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// openAlertingService opens a BackupService against a config file written
// by writeAlertingConfigFile, with a sink already installed.
func openAlertingService(t *testing.T, enabled bool) (*BackupService, string, *countingSink) {
	t.Helper()
	configPath := writeAlertingConfigFile(t, enabled)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	sink := &countingSink{}
	if on := svc.EnableAlerts(sink); on != enabled {
		t.Fatalf("EnableAlerts = %v for a config with alerts.enabled: %v", on, enabled)
	}
	return svc, configPath, sink
}

// criticalThresholds are thresholds no real filesystem can satisfy, which
// is the only way to reach internal/capacity's Critical level today
// (Service.Capacity has no configuration wiring yet, FR-21). A test uses
// them to produce one alertable condition on demand, with no journal
// history and no clock to wind forward.
var criticalThresholds = capacity.Thresholds{WarningFreeBytes: 1 << 62, CriticalFreeBytes: 1 << 62}

// TestCreateBackupSet_KeepsAlertingAcrossTheHotReload is the test the
// carry-over on the hot-reload path never had. Adding a backup set swaps
// the wrapped Service for one built from the freshly re-read config, and
// a dispatcher rebuilt from scratch there would re-alert every
// still-unresolved condition on the next pass, purely because somebody
// used the wizard.
func TestCreateBackupSet_KeepsAlertingAcrossTheHotReload(t *testing.T) {
	svc, _, sink := openAlertingService(t, true)
	ctx := context.Background()

	// One cycle gives the configured backup set a known-good artifact,
	// and its 1ms stale_after has outrun it by the time the next pass
	// looks. Staleness only ever grows from here, so the wait is a floor
	// rather than a race.
	svc.state.Load().inner.RunCycle(ctx)
	time.Sleep(20 * time.Millisecond)
	svc.state.Load().inner.AlertTick(ctx)
	if got := sink.count(); got != 1 {
		t.Fatalf("alerts = %d after the first stale pass, want 1 (all: %v)", got, sink.kinds())
	}

	if _, err := svc.CreateBackupSet(ctx, validCreateReq(t, svc, "new-set")); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}

	reloaded := svc.state.Load().inner
	reloaded.AlertTick(ctx)
	reloaded.AlertTick(ctx)

	if got := sink.count(); got != 1 {
		t.Fatalf("alerts = %d, want still exactly 1: adding a backup set must not re-alert an unresolved condition (all: %v)", got, sink.kinds())
	}

	// Positive control for that 1: the reloaded Service is genuinely
	// still alerting, so "still 1" above is the de-duplication state
	// surviving the swap rather than alerting having been dropped by it.
	reloaded.Capacity = criticalThresholds
	reloaded.AlertTick(ctx)

	if got := sink.count(); got != 2 {
		t.Fatalf("alerts = %d, want 2: a new condition after the hot reload must still be delivered (all: %v)", got, sink.kinds())
	}
}

// TestCreateBackupSet_HonoursAlertsTurnedOffInTheReReadConfig proves the
// one moment an edited alerts block can take effect in a running process
// is not the one moment it is ignored. CreateBackupSet re-reads and
// re-validates the whole config file; an administrator who set
// alerts.enabled: false and then added a backup set used to keep being
// notified until the process restarted.
func TestCreateBackupSet_HonoursAlertsTurnedOffInTheReReadConfig(t *testing.T) {
	svc, configPath, sink := openAlertingService(t, true)
	ctx := context.Background()

	setAlertsEnabled(t, configPath, false)
	if _, err := svc.CreateBackupSet(ctx, validCreateReq(t, svc, "new-set")); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}

	reloaded := svc.state.Load().inner
	if reloaded.Alerts != nil {
		t.Error("the hot-reloaded Service still has a dispatcher after alerts.enabled was set to false")
	}

	reloaded.Capacity = criticalThresholds
	reloaded.AlertTick(ctx)

	if got := sink.count(); got != 0 {
		t.Fatalf("alerts = %d after the operator opted out, want 0 (all: %v)", got, sink.kinds())
	}
}

// TestCreateBackupSet_TurnsAlertingOnFromTheReReadConfig is the same
// contract in the other direction, and it is why the sink is remembered
// even while alerting is off: opting in was equally stuck behind a
// restart.
func TestCreateBackupSet_TurnsAlertingOnFromTheReReadConfig(t *testing.T) {
	svc, configPath, sink := openAlertingService(t, false)
	ctx := context.Background()

	setAlertsEnabled(t, configPath, true)
	if _, err := svc.CreateBackupSet(ctx, validCreateReq(t, svc, "new-set")); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}

	reloaded := svc.state.Load().inner
	if reloaded.Alerts == nil {
		t.Fatal("the hot-reloaded Service has no dispatcher after alerts.enabled was set to true")
	}

	reloaded.Capacity = criticalThresholds
	reloaded.AlertTick(ctx)

	if got := sink.count(); got != 1 {
		t.Fatalf("alerts = %d after the operator opted in, want 1 (all: %v)", got, sink.kinds())
	}
}

// TestRunOnSchedule_AlertsWhileACycleIsStuck is §76 invariant 11 at this
// layer: "process liveness is not evidence of backup freshness". An
// API-submitted operation that never finishes holds the single-flight
// lock, so every scheduled tick skips its cycle, which is exactly the
// "the manager is up and producing nothing" situation the alerting exists
// to report. It must still report it.
func TestRunOnSchedule_AlertsWhileACycleIsStuck(t *testing.T) {
	svc, _, sink := openAlertingService(t, true)
	svc.state.Load().inner.Capacity = criticalThresholds

	// A run that started and never finishes: runScheduledCycle's TryLock
	// fails on every tick from here on.
	svc.runOnce.Lock()
	defer svc.runOnce.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if err := svc.RunOnSchedule(ctx, 10*time.Millisecond); err != nil {
			t.Errorf("RunOnSchedule: %v", err)
		}
	}()

	deadline := time.After(10 * time.Second)
	for sink.count() == 0 {
		select {
		case <-deadline:
			cancel()
			<-stopped
			t.Fatal("no alert was delivered while every scheduled cycle was blocked: alerting only fires when a cycle completes")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-stopped
}
