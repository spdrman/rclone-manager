package app

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/internal/config"
	"github.com/spdrman/rclone-manager/internal/health"
)

// TestBuildHealthReport_NoHistoryIsDegradedNotStale proves BuildHealthReport
// reaches into internal/health correctly for a backup set with no journal
// history at all: FR-24's decideState treats that as DEGRADED (nothing has
// "stopped" if nothing ever started), never HEALTHY (silence is never
// healthy) and never STALE (that would require history to go stale from).
func TestBuildHealthReport_NoHistoryIsDegradedNotStale(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, newFakeTransport(), nil)
	svc.Now = fixedNow(epoch)

	report, err := svc.BuildHealthReport(context.Background(), VersionInfo{BinaryVersion: "1.2.3", RcloneVersion: "v1.75.0"})
	if err != nil {
		t.Fatalf("BuildHealthReport: %v", err)
	}
	if report.Process.BinaryVersion != "1.2.3" || report.Process.RcloneVersion != "v1.75.0" {
		t.Errorf("Process = %+v, want the supplied VersionInfo reflected", report.Process)
	}
	if len(report.BackupSets) != 1 {
		t.Fatalf("BackupSets = %+v, want exactly one", report.BackupSets)
	}
	if got := report.BackupSets[0].State; got != health.Degraded {
		t.Errorf("State = %s, want %s", got, health.Degraded)
	}
}

// TestBuildHealthReport_HealthyAfterACompleteCycle proves a backup set
// that just produced a fresh, complete backup reports HEALTHY.
func TestBuildHealthReport_HealthyAfterACompleteCycle(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.StaleAfter = mustParseDuration(t, "24h")

	tr := newFakeTransport()
	tr.put("backup.dump", "health payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	svc.RunCycle(context.Background())

	report, err := svc.BuildHealthReport(context.Background(), VersionInfo{})
	if err != nil {
		t.Fatalf("BuildHealthReport: %v", err)
	}
	if len(report.BackupSets) != 1 {
		t.Fatalf("BackupSets = %+v, want exactly one", report.BackupSets)
	}
	if got := report.BackupSets[0].State; got != health.Healthy {
		t.Errorf("State = %s, want %s (reason: %s)", got, health.Healthy, report.BackupSets[0].Reason)
	}
	if report.BackupSets[0].LastSuccessfulPollAt == nil {
		t.Error("LastSuccessfulPollAt = nil, want set: RunCycle just ran against this backup set")
	}
}

func mustParseDuration(t *testing.T, s string) config.Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("ParseDuration(%q): %v", s, err)
	}
	return config.Duration(d)
}
