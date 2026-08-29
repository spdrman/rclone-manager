package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// decodeLines parses buf as a stream of newline-delimited JSON objects, the
// format New produces, and fails the test on the first line that doesn't
// parse. An empty buffer decodes to zero lines, not an error.
func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decoding log line: %v (buffer so far: %s)", err, buf.String())
		}
		out = append(out, m)
	}
	return out
}

func TestNewWritesNDJSON(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo)

	l.Event(context.Background(), LevelInfo, "unit_test", "hello", slog.String("k", "v"))
	l.Event(context.Background(), LevelInfo, "unit_test", "hello again", slog.String("k", "v2"))

	lines := decodeLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("got %d decoded lines, want 2 (raw: %s)", len(lines), buf.String())
	}
	if lines[0]["event"] != "unit_test" {
		t.Errorf("line 0 event = %v, want %q", lines[0]["event"], "unit_test")
	}
	if lines[0]["k"] != "v" {
		t.Errorf("line 0 k = %v, want %q", lines[0]["k"], "v")
	}
	if lines[0]["msg"] != "hello" {
		t.Errorf("line 0 msg = %v, want %q", lines[0]["msg"], "hello")
	}
}

func TestNewNilWriterDoesNotPanic(t *testing.T) {
	l := New(nil, LevelInfo)
	l.Event(context.Background(), LevelInfo, "e", "m")
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelWarn)

	l.Startup(context.Background(), "1.0.0", "abc123", "go1.27")        // LevelInfo, should be dropped
	l.Retry(context.Background(), "copy_to_local", 1, "transient", nil) // LevelWarn, should appear

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines at LevelWarn filtering, want 1 (raw: %s)", len(lines), buf.String())
	}
	if lines[0]["event"] != EventRetry {
		t.Errorf("surviving line's event = %v, want %q", lines[0]["event"], EventRetry)
	}
}

func TestNilLoggerIsSafeNoOp(t *testing.T) {
	var l *Logger

	// None of these may panic. There is nothing to assert about output:
	// the whole point is that a nil Logger produces none, silently.
	l.Event(context.Background(), LevelInfo, "e", "m", slog.String("k", "v"))
	l.Startup(context.Background(), "v", "c", "g")
	l.RcloneVersion(context.Background(), "v1.75.0")
	l.CycleStart(context.Background(), "cycle-1")
	l.CycleEnd(context.Background(), "cycle-1", 0, nil)
	l.Discovery(context.Background(), "prod/set", 0, 0, 0, 0, 0, 0)
	l.LifecycleTransition(context.Background(), "prod/set/artifact", "DISCOVERED", "TRANSFERRING", "")
	l.TransferStats(context.Background(), "prod/set/artifact", 0, 0, false)
	l.Hash(context.Background(), "prod/set/artifact", "sha256", "deadbeef")
	l.Validation(context.Background(), "prod/set/artifact", true, "")
	l.Commit(context.Background(), "prod/set/artifact", "/local/path")
	l.RemoteDelete(context.Background(), "prod/set/artifact", "/remote/path", nil)
	l.Reconciliation(context.Background(), "prod/set/artifact", "remote_absent_local_final", "advance_to_complete")
	l.Retention(context.Background(), "prod/set/artifact", "prod/set", "daily", "keep")
	l.Retry(context.Background(), "copy_to_local", 1, "transient", nil)
	l.StaleBackup(context.Background(), "prod/set", 0, 0)
	l.DiskPressure(context.Background(), "/local", 0, 0, "warning")
	l.Error(context.Background(), "load_config", errTest("boom"))

	// A Logger derived from a nil Logger via With must also stay nil-safe.
	derived := l.With("k", "v")
	derived.Event(context.Background(), LevelInfo, "e", "m")
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestWithAttachesPersistentAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo).With("backup_set", "prod/postgres")

	l.Event(context.Background(), LevelInfo, "unit_test", "hello")

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if lines[0]["backup_set"] != "prod/postgres" {
		t.Errorf("backup_set = %v, want %q", lines[0]["backup_set"], "prod/postgres")
	}
}

func TestEventEscapeHatchNilContext(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo)

	// A caller passing a nil context (a copy-paste mistake, or code that
	// hasn't threaded one through yet) must not crash the logger.
	//nolint:staticcheck // deliberately exercising nil-context safety
	l.Event(nil, LevelInfo, "e", "m")

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
}

func TestEventPrefersNamedHelperEventConstant(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo)

	l.Event(context.Background(), LevelDebug, "custom_event", "m")

	lines := decodeLines(t, &buf)
	if len(lines) != 0 {
		t.Fatalf("LevelDebug line should have been filtered at LevelInfo, got %d lines", len(lines))
	}
}

func TestFieldEventKeyIsEvent(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo)
	l.Event(context.Background(), LevelInfo, "x", "m")

	if !strings.Contains(buf.String(), `"event":"x"`) {
		t.Errorf("expected the event field to be keyed %q, got: %s", fieldEvent, buf.String())
	}
}
