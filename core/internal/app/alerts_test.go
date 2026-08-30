package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/alert"
	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// recordingSink stands in for whatever platform notifier a provider app
// wires in at the apps/ layer: core/ cannot import apps/ (§7.1), so the
// delivery mechanism always arrives through this seam.
type recordingSink struct {
	mu        sync.Mutex
	delivered []alert.Alert
}

func (s *recordingSink) Deliver(_ context.Context, a alert.Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, a)
	return nil
}

func (s *recordingSink) kinds() []alert.Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]alert.Kind, 0, len(s.delivered))
	for _, a := range s.delivered {
		out = append(out, a.Kind)
	}
	return out
}

func (s *recordingSink) countOf(kind alert.Kind) int {
	n := 0
	for _, k := range s.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

// alertingConfig is testConfig plus this work package's opt-in, so no
// test here can pass by accident on a config that never enabled alerting.
func alertingConfig(t *testing.T, sources ...config.Source) *config.Config {
	t.Helper()
	cfg := testConfig(t, sources...)
	cfg.Alerts = config.Alerts{Enabled: true, RepeatedFailureThreshold: 3}
	return cfg
}

// TestRunCycle_StaleBackupSetAlertsOnceWhileUnresolved is this work
// package's boundary test: a real internal/health.Report, computed from
// journal state this cycle itself produced, drives an actual alert
// dispatch end to end, and the same unresolved staleness does not
// re-alert on the next pass.
func TestRunCycle_StaleBackupSetAlertsOnceWhileUnresolved(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.StaleAfter = mustParseDuration(t, "1h")

	tr := newFakeTransport()
	tr.put("backup.dump", "stale-alert payload", epoch.Unix())

	svc := New(alertingConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	sink := &recordingSink{}
	if !svc.EnableAlerts(sink) {
		t.Fatal("EnableAlerts returned false for a config that opted in")
	}

	// Pass 1: the artifact lands, so the set is HEALTHY and nothing fires.
	svc.RunCycle(context.Background())
	if got := sink.countOf(alert.StaleBackup); got != 0 {
		t.Fatalf("a healthy backup set fired %d stale alerts, want 0 (all: %v)", got, sink.kinds())
	}

	// Pass 2 and 3: nothing new arrives and the clock moves well past
	// stale_after, so internal/health reports STALE on both.
	svc.Now = fixedNow(epoch.Add(48 * time.Hour))
	svc.RunCycle(context.Background())
	svc.Now = fixedNow(epoch.Add(72 * time.Hour))
	svc.RunCycle(context.Background())

	if got := sink.countOf(alert.StaleBackup); got != 1 {
		t.Fatalf("stale alerts = %d, want exactly 1 across two stale passes (all: %v)", got, sink.kinds())
	}
}

// TestRunCycle_HostKeyChangeAlertsOnTopOfTheConnectionRefusal covers
// §77 invariant #5: the transport layer still refuses the connection, and
// the alert is added on top of that refusal rather than replacing or
// resolving it.
func TestRunCycle_HostKeyChangeAlertsOnTopOfTheConnectionRefusal(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	refusal := transport.NewError(transport.HostVerification, "list",
		errors.New("knownhosts: key mismatch"))
	tr := newFakeTransport()
	tr.failForSourceID = bs.ID.String()
	tr.failErr = refusal

	svc := New(alertingConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	sink := &recordingSink{}
	if !svc.EnableAlerts(sink) {
		t.Fatal("EnableAlerts returned false for a config that opted in")
	}

	report := svc.RunCycle(context.Background())

	if len(report.Sets) != 1 {
		t.Fatalf("CycleReport.Sets = %+v, want exactly one backup set", report.Sets)
	}
	if category, ok := transport.CategoryOf(report.Sets[0].Err); !ok || category != transport.HostVerification {
		t.Fatalf("cycle error = %v (category %v, classified %v), want the connection layer's own HostVerification refusal to survive", report.Sets[0].Err, category, ok)
	}
	if got := sink.countOf(alert.HostKeyChanged); got != 1 {
		t.Fatalf("host-key alerts = %d, want exactly 1 (all: %v)", got, sink.kinds())
	}
}

// TestRunCycle_CriticalStoragePressureAlerts drives internal/capacity's
// own Critical assessment (thresholds no real filesystem can satisfy) and
// proves the alert fires from that verdict, with no capacity arithmetic
// re-implemented here.
func TestRunCycle_CriticalStoragePressureAlerts(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	svc := New(alertingConfig(t, testSource("production", bs)), openJournal(t), newFakeTransport(), nil)
	svc.Now = fixedNow(epoch)
	svc.Capacity = capacity.Thresholds{
		WarningFreeBytes:  1 << 62,
		CriticalFreeBytes: 1 << 62,
	}

	sink := &recordingSink{}
	if !svc.EnableAlerts(sink) {
		t.Fatal("EnableAlerts returned false for a config that opted in")
	}

	svc.RunCycle(context.Background())
	svc.RunCycle(context.Background())

	if got := sink.countOf(alert.CriticalStoragePressure); got != 1 {
		t.Fatalf("critical-storage alerts = %d, want exactly 1 across two passes (all: %v)", got, sink.kinds())
	}
}

// TestEnableAlerts_RequiresTheConfiguredOptIn proves the delivery
// mechanism stays off unless an administrator turned it on: §71's
// "explicit opt-in".
func TestEnableAlerts_RequiresTheConfiguredOptIn(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.StaleAfter = mustParseDuration(t, "1h")

	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	cfg := testConfig(t, testSource("production", bs)) // alerts block left at its zero value
	svc := New(cfg, openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	sink := &recordingSink{}
	if svc.EnableAlerts(sink) {
		t.Fatal("EnableAlerts returned true for a config that never opted in")
	}
	if svc.Alerts != nil {
		t.Fatal("Service.Alerts is set for a config that never opted in")
	}

	svc.RunCycle(context.Background())
	svc.Now = fixedNow(epoch.Add(48 * time.Hour))
	svc.RunCycle(context.Background())

	if got := sink.kinds(); len(got) != 0 {
		t.Fatalf("alerts delivered without an opt-in: %v", got)
	}
}

// TestRunCycle_WithoutASinkStillRunsNormally proves alerting is additive:
// a Service with no dispatcher at all processes a cycle exactly as it did
// before this work package existed.
func TestRunCycle_WithoutASinkStillRunsNormally(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	svc := New(alertingConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(context.Background())
	if len(report.Sets) != 1 || report.Sets[0].Err != nil {
		t.Fatalf("CycleReport = %+v, want one clean backup set result", report)
	}
	if tr.copyToLocalCalls() != 1 {
		t.Fatalf("CopyToLocal calls = %d, want 1", tr.copyToLocalCalls())
	}
}
