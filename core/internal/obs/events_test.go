package obs

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestEventNamesAreStable pins every event constant's literal string
// against a value hardcoded independently, right here, rather than against
// itself. If a future edit to events.go changes what EventLifecycleTransition
// equals, this is the test that turns that into a compile-time-adjacent
// failure instead of a silent break for whatever downstream is parsing the
// "event" field. Do not "simplify" this table to compare a constant against
// itself; that would stop testing anything.
func TestEventNamesAreStable(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"EventStartup", EventStartup, "startup"},
		{"EventRcloneVersion", EventRcloneVersion, "rclone_version"},
		{"EventCycleStart", EventCycleStart, "cycle_start"},
		{"EventCycleEnd", EventCycleEnd, "cycle_end"},
		{"EventDiscovery", EventDiscovery, "discovery"},
		{"EventLifecycleTransition", EventLifecycleTransition, "lifecycle_transition"},
		{"EventTransferStats", EventTransferStats, "transfer_stats"},
		{"EventHash", EventHash, "hash"},
		{"EventValidation", EventValidation, "validation"},
		{"EventCommit", EventCommit, "commit"},
		{"EventRemoteDelete", EventRemoteDelete, "remote_delete"},
		{"EventReconciliation", EventReconciliation, "reconciliation"},
		{"EventRetention", EventRetention, "retention"},
		{"EventRetry", EventRetry, "retry"},
		{"EventStaleBackup", EventStaleBackup, "stale_backup"},
		{"EventDiskPressure", EventDiskPressure, "disk_pressure"},
		{"EventError", EventError, "error"},
	}
	seen := make(map[string]string, len(cases))
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q (this is a breaking change for any log consumer; see events.go's package doc)", c.name, c.got, c.want)
		}
		if owner, dup := seen[c.got]; dup {
			t.Errorf("%s and %s share the same event value %q; every event needs a distinct name", c.name, owner, c.got)
		}
		seen[c.got] = c.name
	}
}

func newRecorder(t *testing.T) (*Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return New(&buf, LevelDebug), &buf
}

func TestStartupEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.Startup(context.Background(), "1.2.3", "deadbeef", "go1.27")

	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	got := lines[0]
	if got["event"] != EventStartup {
		t.Errorf("event = %v, want %q", got["event"], EventStartup)
	}
	for k, want := range map[string]string{"version": "1.2.3", "commit": "deadbeef", "go_version": "go1.27"} {
		if got[k] != want {
			t.Errorf("%s = %v, want %q", k, got[k], want)
		}
	}
}

func TestRcloneVersionEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.RcloneVersion(context.Background(), "v1.75.0")

	lines := decodeLines(t, buf)
	if lines[0]["event"] != EventRcloneVersion || lines[0]["rclone_version"] != "v1.75.0" {
		t.Errorf("got %#v", lines[0])
	}
}

func TestCycleStartEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.CycleStart(context.Background(), "cycle-42")

	lines := decodeLines(t, buf)
	if lines[0]["event"] != EventCycleStart || lines[0]["cycle_id"] != "cycle-42" {
		t.Errorf("got %#v", lines[0])
	}
	if lines[0]["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", lines[0]["level"])
	}
}

func TestCycleEndEventSuccess(t *testing.T) {
	l, buf := newRecorder(t)
	l.CycleEnd(context.Background(), "cycle-42", 3*time.Second, nil)

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["event"] != EventCycleEnd {
		t.Errorf("event = %v, want %q", got["event"], EventCycleEnd)
	}
	if got["level"] != "INFO" {
		t.Errorf("a successful cycle end should log at INFO, got %v", got["level"])
	}
	if _, hasErr := got["error"]; hasErr {
		t.Errorf("a successful cycle end should carry no error field, got %v", got["error"])
	}
}

func TestCycleEndEventFailure(t *testing.T) {
	l, buf := newRecorder(t)
	l.CycleEnd(context.Background(), "cycle-42", time.Second, errors.New("discovery blew up"))

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["level"] != "ERROR" {
		t.Errorf("a failed cycle end should log at ERROR, got %v", got["level"])
	}
	if got["error"] != "discovery blew up" {
		t.Errorf("error = %v, want %q", got["error"], "discovery blew up")
	}
}

func TestDiscoveryEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.Discovery(context.Background(), "prod/postgres", 3, 1, 2, 0, 1, 0)

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["event"] != EventDiscovery {
		t.Errorf("event = %v, want %q", got["event"], EventDiscovery)
	}
	for k, want := range map[string]float64{
		"discovered":    3,
		"already_known": 1,
		"pending":       2,
		"rejected":      0,
		"conflicts":     1,
		"errored":       0,
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

func TestLifecycleTransitionEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.LifecycleTransition(context.Background(), "prod/postgres/backup.dump", "VERIFIED", "COMMITTING", "")

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["event"] != EventLifecycleTransition {
		t.Errorf("event = %v, want %q", got["event"], EventLifecycleTransition)
	}
	if got["from"] != "VERIFIED" || got["to"] != "COMMITTING" {
		t.Errorf("from/to = %v/%v", got["from"], got["to"])
	}
	if _, has := got["detail"]; has {
		t.Errorf("empty detail should be omitted, got %v", got["detail"])
	}

	buf.Reset()
	l.LifecycleTransition(context.Background(), "prod/postgres/backup.dump", "VERIFYING", "FAILED", "hash mismatch")
	lines = decodeLines(t, buf)
	if lines[0]["detail"] != "hash mismatch" {
		t.Errorf("detail = %v, want %q", lines[0]["detail"], "hash mismatch")
	}
}

func TestTransferStatsEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.TransferStats(context.Background(), "prod/postgres/backup.dump", 12345, 2*time.Second, true)

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["event"] != EventTransferStats {
		t.Errorf("event = %v, want %q", got["event"], EventTransferStats)
	}
	if got["bytes_transferred"] != float64(12345) {
		t.Errorf("bytes_transferred = %v, want 12345", got["bytes_transferred"])
	}
	if got["checksummed"] != true {
		t.Errorf("checksummed = %v, want true", got["checksummed"])
	}
}

func TestHashEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.Hash(context.Background(), "prod/postgres/backup.dump", "sha256", "deadbeef")

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["event"] != EventHash || got["alg"] != "sha256" || got["hash"] != "deadbeef" {
		t.Errorf("got %#v", got)
	}
}

func TestValidationEventPassed(t *testing.T) {
	l, buf := newRecorder(t)
	l.Validation(context.Background(), "prod/postgres/backup.dump", true, "")

	lines := decodeLines(t, buf)
	if lines[0]["level"] != "INFO" {
		t.Errorf("a passing validation should log at INFO, got %v", lines[0]["level"])
	}
}

func TestValidationEventFailed(t *testing.T) {
	l, buf := newRecorder(t)
	l.Validation(context.Background(), "prod/postgres/backup.dump", false, "pg_restore --list failed")

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["level"] != "WARN" {
		t.Errorf("a failing validation should log at WARN, got %v", got["level"])
	}
	if got["detail"] != "pg_restore --list failed" {
		t.Errorf("detail = %v", got["detail"])
	}
}

func TestCommitEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.Commit(context.Background(), "prod/postgres/backup.dump", "/data/backups/backup.dump")

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["event"] != EventCommit || got["local_path"] != "/data/backups/backup.dump" {
		t.Errorf("got %#v", got)
	}
}

func TestRemoteDeleteEventSuccess(t *testing.T) {
	l, buf := newRecorder(t)
	l.RemoteDelete(context.Background(), "prod/postgres/backup.dump", "/incoming/backup.dump", nil)

	lines := decodeLines(t, buf)
	if lines[0]["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", lines[0]["level"])
	}
}

func TestRemoteDeleteEventFailure(t *testing.T) {
	l, buf := newRecorder(t)
	l.RemoteDelete(context.Background(), "prod/postgres/backup.dump", "/incoming/backup.dump", errors.New("sftp: permission denied"))

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", got["level"])
	}
	if got["error"] != "sftp: permission denied" {
		t.Errorf("error = %v", got["error"])
	}
}

func TestReconciliationEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.Reconciliation(context.Background(), "prod/postgres/backup.dump", "remote_absent_local_final", "advance_to_complete")

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["event"] != EventReconciliation || got["scenario"] != "remote_absent_local_final" || got["action"] != "advance_to_complete" {
		t.Errorf("got %#v", got)
	}
}

func TestRetentionEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.Retention(context.Background(), "prod/postgres/backup.dump", "prod/postgres", "daily", "keep")

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["event"] != EventRetention || got["tier"] != "daily" || got["decision"] != "keep" {
		t.Errorf("got %#v", got)
	}
}

func TestRetryEventAlwaysWarn(t *testing.T) {
	l, buf := newRecorder(t)
	l.Retry(context.Background(), "copy_to_local", 2, "transient", errors.New("connection reset"))

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", got["level"])
	}
	if got["attempt"] != float64(2) || got["category"] != "transient" || got["error"] != "connection reset" {
		t.Errorf("got %#v", got)
	}
}

func TestStaleBackupEventAlwaysWarn(t *testing.T) {
	l, buf := newRecorder(t)
	l.StaleBackup(context.Background(), "prod/postgres", 40*time.Hour, 30*time.Hour)

	lines := decodeLines(t, buf)
	if lines[0]["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", lines[0]["level"])
	}
}

func TestDiskPressureEventWarning(t *testing.T) {
	l, buf := newRecorder(t)
	l.DiskPressure(context.Background(), "/data", 1<<30, 100<<30, "warning")

	lines := decodeLines(t, buf)
	if lines[0]["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", lines[0]["level"])
	}
	if lines[0]["threshold_level"] != "warning" {
		t.Errorf("threshold_level = %v, want %q", lines[0]["threshold_level"], "warning")
	}
}

func TestDiskPressureEventCritical(t *testing.T) {
	l, buf := newRecorder(t)
	l.DiskPressure(context.Background(), "/data", 1<<20, 100<<30, "critical")

	lines := decodeLines(t, buf)
	if lines[0]["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", lines[0]["level"])
	}
	if lines[0]["threshold_level"] != "critical" {
		t.Errorf("threshold_level = %v, want %q", lines[0]["threshold_level"], "critical")
	}
}

// TestDiskPressureEventDoesNotShadowSeverityLevel is a regression test for
// a real bug caught while writing this package: DiskPressure's threshold
// field was originally also named "level", which collided with slog's own
// top-level severity field of the same name. encoding/json keeps the last
// of two duplicate JSON object keys rather than erroring, so the collision
// silently replaced the record's actual severity (INFO/WARN/ERROR) with
// whatever threshold string DiskPressure logged, in the decoded map. This
// asserts both fields survive, under their own names, at once.
func TestDiskPressureEventDoesNotShadowSeverityLevel(t *testing.T) {
	l, buf := newRecorder(t)
	l.DiskPressure(context.Background(), "/data", 1<<20, 100<<30, "critical")

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["level"] != "ERROR" {
		t.Errorf("severity level = %v, want ERROR (it may have been shadowed by threshold_level)", got["level"])
	}
	if got["threshold_level"] != "critical" {
		t.Errorf("threshold_level = %v, want %q", got["threshold_level"], "critical")
	}
}

func TestErrorEvent(t *testing.T) {
	l, buf := newRecorder(t)
	l.Error(context.Background(), "load_config", errors.New("parsing config: yaml: line 3: mapping values are not allowed"))

	lines := decodeLines(t, buf)
	got := lines[0]
	if got["event"] != EventError || got["level"] != "ERROR" || got["op"] != "load_config" {
		t.Errorf("got %#v", got)
	}
}
