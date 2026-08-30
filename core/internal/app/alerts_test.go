package app

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/alert"
	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
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

// TestRunCycle_DisabledBackupSetNeverAlerts proves a backup set an
// administrator deliberately switched off is not reported as a problem.
// It ages past stale_after by definition, since nothing polls it, so
// alerting on it would mean one notification for a state somebody asked
// for, followed by a condition that can never resolve.
func TestRunCycle_DisabledBackupSetNeverAlerts(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.StaleAfter = mustParseDuration(t, "1h")

	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	svc := New(alertingConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	sink := &recordingSink{}
	if !svc.EnableAlerts(sink) {
		t.Fatal("EnableAlerts returned false for a config that opted in")
	}

	// One good cycle while the set is still enabled, so it has history to
	// go stale from, then the administrator disables it.
	svc.RunCycle(context.Background())
	svc.Config.Sources[0].BackupSets[0].Disabled = true

	svc.Now = fixedNow(epoch.Add(48 * time.Hour))
	svc.RunCycle(context.Background())
	svc.Now = fixedNow(epoch.Add(72 * time.Hour))
	svc.RunCycle(context.Background())

	if got := sink.kinds(); len(got) != 0 {
		t.Fatalf("a disabled backup set produced alerts %v, want none", got)
	}
}

// TestRunCycle_CancelledContextDoesNotResetDeduplication proves a pass
// that could not look is not read as a pass that looked and saw nothing.
// A cycle interrupted by shutdown must not clear the dispatcher's memory
// of what is already firing, or every unresolved condition would alert
// again on the next start.
func TestRunCycle_CancelledContextDoesNotResetDeduplication(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.StaleAfter = mustParseDuration(t, "1h")

	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	svc := New(alertingConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	sink := &recordingSink{}
	if !svc.EnableAlerts(sink) {
		t.Fatal("EnableAlerts returned false for a config that opted in")
	}

	svc.RunCycle(context.Background())
	svc.Now = fixedNow(epoch.Add(48 * time.Hour))
	svc.RunCycle(context.Background())
	if got := sink.countOf(alert.StaleBackup); got != 1 {
		t.Fatalf("stale alerts = %d after the first stale pass, want 1", got)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	svc.RunCycle(cancelled)

	// Still stale, and still already reported: the interrupted pass must
	// not have made the dispatcher forget.
	svc.Now = fixedNow(epoch.Add(96 * time.Hour))
	svc.RunCycle(context.Background())

	if got := sink.countOf(alert.StaleBackup); got != 1 {
		t.Fatalf("stale alerts = %d, want still exactly 1: an interrupted pass must not reset de-duplication", got)
	}
}

// listFailingJournal is a Journal whose ListByBackupSet always fails, so
// a test can prove an alerting pass that cannot read journal state
// declines to draw any conclusion from that.
type listFailingJournal struct {
	Journal
	err error
}

func (j listFailingJournal) ListByBackupSet(context.Context, model.BackupSetID) ([]state.Record, error) {
	return nil, j.err
}

// TestEvaluateAlerts_UnreadableJournalDoesNotResetDeduplication is the
// same contract as the cancelled-context case, for the other way a pass
// can fail to see: if the health report cannot be built, nothing is
// observed, so nothing is treated as resolved.
func TestEvaluateAlerts_UnreadableJournalDoesNotResetDeduplication(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.StaleAfter = mustParseDuration(t, "1h")

	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(alertingConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	sink := &recordingSink{}
	if !svc.EnableAlerts(sink) {
		t.Fatal("EnableAlerts returned false for a config that opted in")
	}

	svc.RunCycle(context.Background())
	svc.Now = fixedNow(epoch.Add(48 * time.Hour))
	svc.RunCycle(context.Background())
	if got := sink.countOf(alert.StaleBackup); got != 1 {
		t.Fatalf("stale alerts = %d after the first stale pass, want 1", got)
	}

	svc.Journal = listFailingJournal{Journal: journal, err: errors.New("database is locked")}
	svc.evaluateAlerts(context.Background(), CycleReport{})

	svc.Journal = journal
	svc.evaluateAlerts(context.Background(), CycleReport{})

	if got := sink.countOf(alert.StaleBackup); got != 1 {
		t.Fatalf("stale alerts = %d, want still exactly 1: a pass that could not read the journal must not reset de-duplication", got)
	}
}

// TestEvaluateAlerts_UnreadableFreeSpaceDoesNotResolveTheStorageAlert is
// the storage half of "a pass that could not look is not a pass that
// looked and saw nothing". BuildHealthReport leaves FreeBytes nil exactly
// when capacity.StatPath failed, which is what an unmounted volume or a
// bind mount that disappeared looks like: the one incident an operator
// most needs telling about must not silently resolve the disk-full alert
// and re-fire it when the mount comes back.
func TestEvaluateAlerts_UnreadableFreeSpaceDoesNotResolveTheStorageAlert(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

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

	svc.AlertTick(context.Background())
	if got := sink.countOf(alert.CriticalStoragePressure); got != 1 {
		t.Fatalf("critical-storage alerts = %d after the first pass, want 1", got)
	}

	// The volume goes away, so statfs fails and this pass knows nothing
	// about that backup set's free space.
	if err := os.RemoveAll(localDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	svc.AlertTick(context.Background())

	// And it comes back, still critically low.
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	svc.AlertTick(context.Background())

	if got := sink.countOf(alert.CriticalStoragePressure); got != 1 {
		t.Fatalf("critical-storage alerts = %d, want still exactly 1: a pass that could not read the disk must not resolve the condition", got)
	}

	// Positive control for that 1. The same service, sink and condition
	// DO produce a second alert once a pass that COULD look reports the
	// pressure gone and it later returns, so "still 1" above is the
	// de-duplication holding, not alerting having quietly stopped.
	svc.Capacity = capacity.Thresholds{}
	svc.AlertTick(context.Background())
	svc.Capacity = capacity.Thresholds{WarningFreeBytes: 1 << 62, CriticalFreeBytes: 1 << 62}
	svc.AlertTick(context.Background())

	if got := sink.countOf(alert.CriticalStoragePressure); got != 2 {
		t.Fatalf("critical-storage alerts = %d, want 2: a genuinely resolved condition that recurs is a fresh alert", got)
	}
}

// TestRunCycle_HostKeyAlertSurvivesAPassThatCouldNotCheckIt is the
// host-key half of the same principle, and it is stricter than the
// others: §77 invariant #5 says re-trusting a changed key takes an
// explicit administrator action, so this condition never resolves on its
// own. A cycle where the set failed for an unrelated reason, or never ran
// at all, is not evidence the key verifies again.
func TestRunCycle_HostKeyAlertSurvivesAPassThatCouldNotCheckIt(t *testing.T) {
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

	svc.RunCycle(context.Background())
	if got := sink.countOf(alert.HostKeyChanged); got != 1 {
		t.Fatalf("host-key alerts = %d after the refusal, want 1", got)
	}

	// A cycle that fails for an unrelated reason says nothing about the
	// host key: it never got far enough to check one. Neither does a pass
	// with no cycle behind it at all.
	tr.failErr = transport.NewError(transport.PermissionDenied, "list", errors.New("permission denied"))
	svc.RunCycle(context.Background())
	svc.AlertTick(context.Background())

	// The mismatch is still there, and still the same unresolved one: if
	// either pass above had been read as the key verifying again, this is
	// where the second alert would land.
	tr.failErr = refusal
	svc.RunCycle(context.Background())

	if got := sink.countOf(alert.HostKeyChanged); got != 1 {
		t.Fatalf("host-key alerts = %d, want still exactly 1: absence of the refusal is not evidence the key was re-trusted", got)
	}

	// Positive control: a cycle that completes IS that evidence, so the
	// condition resolves, and a later mismatch is a fresh alert.
	tr.failForSourceID = ""
	tr.failErr = nil
	svc.RunCycle(context.Background())

	tr.failForSourceID = bs.ID.String()
	tr.failErr = refusal
	svc.RunCycle(context.Background())

	if got := sink.countOf(alert.HostKeyChanged); got != 2 {
		t.Fatalf("host-key alerts = %d, want 2: a key that verified and then changed again is a fresh alert", got)
	}
}

// TestRunCycle_ShippedCapacityDefaultsFireNoStorageAlert pins what a
// production deployment actually does today. Service.Capacity is not
// configurable yet (there is no warning_free_bytes / critical_free_bytes
// key in internal/config, FR-21's threshold wiring is still open), so
// every shipped binary runs the zero value, and with all-zero thresholds
// capacity.AssessCurrent reaches Critical only when the filesystem
// reports no available bytes at all. This is that fact under test rather
// than assumed, so nobody reads the storage alert's own test as evidence
// it can fire in production.
func TestRunCycle_ShippedCapacityDefaultsFireNoStorageAlert(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	svc := New(alertingConfig(t, testSource("production", bs)), openJournal(t), newFakeTransport(), nil)
	svc.Now = fixedNow(epoch)

	sink := &recordingSink{}
	if !svc.EnableAlerts(sink) {
		t.Fatal("EnableAlerts returned false for a config that opted in")
	}

	svc.RunCycle(context.Background())
	svc.RunCycle(context.Background())

	if got := sink.countOf(alert.CriticalStoragePressure); got != 0 {
		t.Fatalf("critical-storage alerts = %d on an ordinary filesystem with the shipped thresholds, want 0", got)
	}

	// Positive control: the same service and sink DO alert once the
	// thresholds are set to something no filesystem can satisfy, so the
	// zero above is the thresholds, not a broken harness.
	svc.Capacity = capacity.Thresholds{WarningFreeBytes: 1 << 62, CriticalFreeBytes: 1 << 62}
	svc.RunCycle(context.Background())

	if got := sink.countOf(alert.CriticalStoragePressure); got != 1 {
		t.Fatalf("critical-storage alerts = %d with thresholds no filesystem can satisfy, want 1", got)
	}
}

// TestAlertTick_FiresWithoutACycle is the mechanism behind §76 invariant
// 11, "process liveness is not evidence of backup freshness": the stale
// alert exists for a daemon that is up and not producing backups, so it
// cannot be reachable only from the end of a cycle that completed.
func TestAlertTick_FiresWithoutACycle(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.StaleAfter = mustParseDuration(t, "1h")

	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	svc := New(alertingConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	sink := &recordingSink{}
	if !svc.EnableAlerts(sink) {
		t.Fatal("EnableAlerts returned false for a config that opted in")
	}

	// One good cycle, so the set has history to go stale from, and then
	// no cycle ever completes again.
	svc.RunCycle(context.Background())
	svc.Now = fixedNow(epoch.Add(48 * time.Hour))

	svc.AlertTick(context.Background())

	if got := sink.countOf(alert.StaleBackup); got != 1 {
		t.Fatalf("stale alerts = %d from an out-of-cycle pass, want 1 (all: %v)", got, sink.kinds())
	}

	// And it is still one mechanism: the out-of-cycle pass de-duplicates
	// against the in-cycle one rather than alerting twice about the same
	// unresolved condition.
	svc.RunCycle(context.Background())
	svc.AlertTick(context.Background())

	if got := sink.countOf(alert.StaleBackup); got != 1 {
		t.Fatalf("stale alerts = %d, want still exactly 1 across an in-cycle and an out-of-cycle pass", got)
	}
}
