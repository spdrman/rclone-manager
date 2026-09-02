package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	// Same for WithRedaction: a nil *Logger stays nil, and the result is
	// still safe to call.
	withRedaction := l.WithRedaction(NewRedactor(Endpoint{Host: "example.internal"}))
	withRedaction.Event(context.Background(), LevelInfo, "e", "m")
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

// TestWithRedactionFiltersStringAttrsAndMsg is issue #295's unit-level
// proof for emit's own seam: every string-valued attr, and msg itself, is
// run through the configured Redactor, while a non-string attr (an int
// here) is left completely alone, since a Redactor only ever matches
// substrings of a string.
func TestWithRedactionFiltersStringAttrsAndMsg(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo).WithRedaction(NewRedactor(Endpoint{Host: "127.0.0.1", Port: 55570, User: "backupuser"}))

	l.Event(context.Background(), LevelError, "unit_test",
		"dial tcp 127.0.0.1:55570: connect: connection refused",
		slog.String("error", `source "prod/set": dial tcp 127.0.0.1:55570: connect: connection refused`),
		slog.Int("attempt", 55570), // an int that happens to equal the port must survive untouched
	)

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	line := lines[0]

	if strings.Contains(line["msg"].(string), "55570") {
		t.Errorf("msg still contains the port: %v", line["msg"])
	}
	if !strings.Contains(line["msg"].(string), redacted) {
		t.Errorf("msg was not redacted at all: %v", line["msg"])
	}
	if strings.Contains(line["error"].(string), "55570") || strings.Contains(line["error"].(string), "127.0.0.1") {
		t.Errorf("error attr still contains the endpoint: %v", line["error"])
	}
	if !strings.Contains(line["error"].(string), "prod/set") {
		t.Errorf("error attr lost the source id, want it preserved: %v", line["error"])
	}
	if attempt, ok := line["attempt"].(float64); !ok || attempt != 55570 {
		t.Errorf("attempt (a non-string attr) = %v, want the untouched int 55570", line["attempt"])
	}
}

// TestWithRedactionNilRedactorLeavesOutputUnchanged is the regression
// control: WithRedaction(nil), which is what a Logger for a deployment
// with no sensitive remote gets (internal/app.New always calls
// WithRedaction, with whatever obs.NewRedactor(sensitiveEndpoints(cfg)...)
// returns, and that is nil when nothing opted in), must produce the exact
// same fields New alone would, field for field (excluding "time", which
// New's own real-clock timestamp makes non-deterministic independent of
// anything this test is about).
func TestWithRedactionNilRedactorLeavesOutputUnchanged(t *testing.T) {
	var plain, withNilRedaction bytes.Buffer
	msg := "dial tcp 127.0.0.1:55570: connect: connection refused"
	attrs := []slog.Attr{slog.String("error", msg)}

	New(&plain, LevelInfo).Event(context.Background(), LevelError, "unit_test", msg, attrs...)
	New(&withNilRedaction, LevelInfo).WithRedaction(nil).Event(context.Background(), LevelError, "unit_test", msg, attrs...)

	plainLines := decodeLines(t, &plain)
	withNilLines := decodeLines(t, &withNilRedaction)
	if len(plainLines) != 1 || len(withNilLines) != 1 {
		t.Fatalf("got %d and %d lines, want 1 and 1", len(plainLines), len(withNilLines))
	}
	delete(plainLines[0], "time")
	delete(withNilLines[0], "time")
	if fmt.Sprint(plainLines[0]) != fmt.Sprint(withNilLines[0]) {
		t.Fatalf("WithRedaction(nil) changed the output:\nplain:              %v\nwithRedaction(nil): %v", plainLines[0], withNilLines[0])
	}
}

// TestWithPreservesRedaction proves With, called after WithRedaction (or
// the other way around), never drops the redactor: a caller that attaches
// a persistent attribute via With to a Logger already configured for
// redaction must keep redacting, since nothing about attaching a
// persistent field is a request to also turn redaction off.
func TestWithPreservesRedaction(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo).
		WithRedaction(NewRedactor(Endpoint{Host: "127.0.0.1", Port: 55570})).
		With("backup_set", "prod/postgres")

	l.Event(context.Background(), LevelError, "unit_test", "dial tcp 127.0.0.1:55570: connect: connection refused")

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if strings.Contains(lines[0]["msg"].(string), "55570") {
		t.Errorf("With dropped the redactor WithRedaction had already configured: msg = %v", lines[0]["msg"])
	}
	if lines[0]["backup_set"] != "prod/postgres" {
		t.Errorf("With's own attached attr was lost: backup_set = %v", lines[0]["backup_set"])
	}
}
