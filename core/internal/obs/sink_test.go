package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// The tap this package grows so something in this process can follow the
// event stream as it happens, rather than only whatever is shipping
// stdout.
//
// These tests are about the tap, not about the events: events_test.go
// already pins every event name and every field. What has to hold here is
// that the tap sees exactly what the log line sees, including redaction,
// that it cannot make the process slower or less safe by being absent,
// and that a record can be attributed to the backup set the work was
// being done for.

// recordingSink is a Sink that keeps every record it was handed. It takes
// a lock because a real one is written from whichever goroutine reached
// the event, and a sink that were only safe under a test's single
// goroutine would prove nothing about the type it stands in for.
type recordingSink struct {
	mu      sync.Mutex
	records []Record
}

func (s *recordingSink) RecordEvent(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

func (s *recordingSink) all() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

func fieldValue(r Record, key string) (string, bool) {
	for _, f := range r.Fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return "", false
}

func TestSinkSeesEveryEventTheLogLineDoes(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	logger := New(&out, LevelInfo).WithSink(sink)

	ctx := context.Background()
	logger.CycleStart(ctx, "2026-09-07T00:16:29Z")
	logger.Discovery(ctx, "api-server/var-backups", 41, 15, 26, 0, 0, 0)
	logger.LifecycleTransition(ctx, "api-server/var-backups/dpkg.status.2.gz", "DISCOVERED", "TRANSFERRING", "", false)
	logger.TransferStats(ctx, "api-server/var-backups/dpkg.status.2.gz", 4194304, 0, false)

	got := sink.all()
	if len(got) != 4 {
		t.Fatalf("the sink saw %d records, and four events were logged", len(got))
	}

	wantEvents := []string{EventCycleStart, EventDiscovery, EventLifecycleTransition, EventTransferStats}
	for i, want := range wantEvents {
		if got[i].Event != want {
			t.Errorf("record %d is event %q, and the %dth event logged was %q", i, got[i].Event, i, want)
		}
	}
	if got[0].Level != LevelInfo {
		t.Errorf("cycle_start reached the sink at level %v, and it is logged at info", got[0].Level)
	}
	if got[0].Message == "" {
		t.Error("cycle_start reached the sink with no message, so nothing could render it as a line")
	}
	if got[0].At.IsZero() {
		t.Error("a record reached the sink with no timestamp, so nothing could order it")
	}
	if v, ok := fieldValue(got[1], "discovered"); !ok || v != "41" {
		t.Errorf("discovery reached the sink with discovered=%q (present=%v), and it was logged as 41", v, ok)
	}
	if v, ok := fieldValue(got[1], "backup_set"); !ok || v != "api-server/var-backups" {
		t.Errorf("discovery reached the sink with backup_set=%q (present=%v)", v, ok)
	}
	if v, ok := fieldValue(got[3], "bytes_transferred"); !ok || v != "4194304" {
		t.Errorf("transfer_stats reached the sink with bytes_transferred=%q (present=%v)", v, ok)
	}

	// The tap is in addition to the log line, never instead of it.
	lines := strings.Count(strings.TrimSpace(out.String()), "\n") + 1
	if lines != 4 {
		t.Fatalf("the log wrote %d lines while the sink saw 4; a tap that swallows the line it taps is a regression", lines)
	}
}

func TestSinkSeesRedactedValuesAndNeverTheRawOnes(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	logger := New(&out, LevelInfo).
		WithRedaction(NewRedactor(Endpoint{Host: "nas.internal", Port: 1209, User: "backups"})).
		WithSink(sink)

	logger.Event(context.Background(), LevelInfo, "connection", "dialling backups@nas.internal:1209",
		slog.String("endpoint", "backups@nas.internal:1209"))

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("the sink saw %d records, and one event was logged", len(got))
	}
	if strings.Contains(got[0].Message, "nas.internal") {
		t.Errorf("the sink was handed the unredacted message %q; a second reader of the event stream must not be a way around redaction", got[0].Message)
	}
	if v, _ := fieldValue(got[0], "endpoint"); strings.Contains(v, "nas.internal") {
		t.Errorf("the sink was handed the unredacted field %q", v)
	}
}

func TestALoggerWithNoSinkAndANilLoggerAreBothSafe(t *testing.T) {
	var out bytes.Buffer
	New(&out, LevelInfo).CycleStart(context.Background(), "c1")
	if out.Len() == 0 {
		t.Fatal("a logger with no sink stopped logging, so this test would prove nothing about the sink being optional")
	}

	var nilLogger *Logger
	if got := nilLogger.WithSink(&recordingSink{}); got != nil {
		t.Errorf("WithSink on a nil *Logger returned %v; every other method on this type is nil-safe and returns nil", got)
	}
	nilLogger.WithSink(&recordingSink{}).CycleStart(context.Background(), "c1")

	// A nil Sink is "no tap", not a panic waiting for the first event.
	New(&out, LevelInfo).WithSink(nil).CycleStart(context.Background(), "c1")
}

func TestABackupSetOnTheContextAttributesTheEventToIt(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	logger := New(&out, LevelInfo).WithSink(sink)

	ctx := WithBackupSet(context.Background(), "api-server/var-backups")
	logger.Error(ctx, "discover", errStub{"the source refused the connection"})

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("the sink saw %d records, and one event was logged", len(got))
	}
	if v, ok := fieldValue(got[0], "backup_set"); !ok || v != "api-server/var-backups" {
		t.Fatalf("an error logged while a backup set was on the context reached the sink with backup_set=%q (present=%v). Without it nothing can tell an operator WHICH set failed to be discovered.", v, ok)
	}

	// The same fact reaches the log line, because a line that cannot say
	// which set it is about is the gap this closes.
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &line); err != nil {
		t.Fatalf("parsing the log line: %v", err)
	}
	if line["backup_set"] != "api-server/var-backups" {
		t.Errorf("the log line says backup_set=%v", line["backup_set"])
	}
}

func TestAnEventThatNamesItsOwnBackupSetKeepsTheOneItNamed(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	logger := New(&out, LevelInfo).WithSink(sink)

	// The context says one set and the event says another. The event
	// wins: Discovery is called with the set it actually discovered, and
	// a context value silently overwriting it would make the field lie.
	ctx := WithBackupSet(context.Background(), "api-server/var-backups")
	logger.Discovery(ctx, "nas-media/photos", 18, 18, 0, 0, 0, 0)

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("the sink saw %d records, and one event was logged", len(got))
	}
	count := 0
	for _, f := range got[0].Fields {
		if f.Key == "backup_set" {
			count++
			if f.Value != "nas-media/photos" {
				t.Errorf("backup_set is %q, and Discovery was called with nas-media/photos", f.Value)
			}
		}
	}
	if count != 1 {
		t.Errorf("the record carries %d backup_set fields; a duplicate key is a JSON object where one value silently wins", count)
	}
}

func TestBackupSetFromReadsBackWhatWithBackupSetPutThere(t *testing.T) {
	if got := BackupSetFrom(context.Background()); got != "" {
		t.Errorf("a bare context reports backup set %q", got)
	}
	ctx := WithBackupSet(context.Background(), "api-server/var-backups")
	if got := BackupSetFrom(ctx); got != "api-server/var-backups" {
		t.Errorf("BackupSetFrom reports %q", got)
	}
	// An empty id is declining to scope, not scoping to "".
	if got := BackupSetFrom(WithBackupSet(ctx, "")); got != "api-server/var-backups" {
		t.Errorf("scoping to an empty id changed the answer to %q", got)
	}
}

type errStub struct{ msg string }

func (e errStub) Error() string { return e.msg }

func TestAttributesAttachedWithWithReachTheSinkToo(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	logger := New(&out, LevelInfo).WithSink(sink).With("cycle_id", "c1", slog.String("host", "nas"))

	logger.CycleStart(context.Background(), "c1")

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("the sink saw %d records, and one event was logged", len(got))
	}
	if v, ok := fieldValue(got[0], "cycle_id"); !ok || v != "c1" {
		t.Errorf("cycle_id reached the sink as %q (present=%v); an attribute on the log line the tap cannot see is two readers disagreeing about one event", v, ok)
	}
	if v, ok := fieldValue(got[0], "host"); !ok || v != "nas" {
		t.Errorf("host reached the sink as %q (present=%v)", v, ok)
	}
}

func TestWithDoesNotShareItsBoundFieldsBackwards(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	parent := New(&out, LevelInfo).WithSink(sink)
	parent.With("only_on_the_child", "yes")

	parent.CycleStart(context.Background(), "c1")

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("the sink saw %d records, and one event was logged", len(got))
	}
	if _, ok := fieldValue(got[0], "only_on_the_child"); ok {
		t.Error("a field bound on a derived Logger appeared on the parent's own event, so With is mutating rather than deriving")
	}
}

// TestWithBoundFieldsAreRedactedEverywhereTheyLand is #295's rule applied
// to the one place it was not.
//
// With recorded its attributes at the moment it was called and the tap
// prepended them to every record unfiltered, so a Logger built by
// With(...).WithRedaction(...) shipped an endpoint straight past the
// redactor into both the JSON line and the tap. That was latent while the
// tap fed a container log; it stopped being latent when GET
// /api/v1/activity/live began serving the same fields to a browser.
func TestWithBoundFieldsAreRedactedEverywhereTheyLand(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	logger := New(&out, LevelInfo).
		With("endpoint", "backup-agent@nas.internal:2222").
		WithRedaction(NewRedactor(Endpoint{Host: "nas.internal", Port: 2222, User: "backup-agent"}))

	logger.CycleStart(context.Background(), "c1")

	got := sink.all()
	if len(got) != 0 {
		t.Fatalf("this logger has no sink yet and the recorder saw %d records", len(got))
	}
	if strings.Contains(out.String(), "nas.internal") {
		t.Errorf("an endpoint bound by With reached the log line unredacted:\n  %s", strings.TrimSpace(out.String()))
	}

	out.Reset()
	tapped := New(&out, LevelInfo).
		WithSink(sink).
		With("endpoint", "backup-agent@nas.internal:2222").
		WithRedaction(NewRedactor(Endpoint{Host: "nas.internal", Port: 2222, User: "backup-agent"}))
	tapped.CycleStart(context.Background(), "c1")

	got = sink.all()
	if len(got) != 1 {
		t.Fatalf("the sink saw %d records, and one event was logged", len(got))
	}
	v, ok := fieldValue(got[0], "endpoint")
	if !ok {
		t.Fatal("the bound field never reached the tap at all")
	}
	if strings.Contains(v, "nas.internal") {
		t.Errorf("the tap was handed %q; every field here is now served to a browser by GET /api/v1/activity/live, so an unredacted one is an endpoint on a screen", v)
	}
}

// TestABoundBackupSetIsTheOneEveryReaderAgreesOn closes the other half of
// the same gap.
//
// contextBackupSet looked for an existing backup_set among the event's own
// attrs and never among the ones With had bound, so a Logger carrying one
// got a second copy appended from the context. The tap files a record
// under the FIRST backup_set it sees and encoding/json keeps the LAST of
// two identical keys, so the strip an event landed on and the set the log
// line named could be different sets.
func TestABoundBackupSetIsTheOneEveryReaderAgreesOn(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	logger := New(&out, LevelInfo).WithSink(sink).With("backup_set", "api-server/var-backups")

	// A context scoped to a DIFFERENT set, which is the shape that makes
	// the disagreement visible.
	logger.CycleStart(WithBackupSet(context.Background(), "production/postgres"), "c1")

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("the sink saw %d records, and one event was logged", len(got))
	}
	seen := 0
	for _, f := range got[0].Fields {
		if f.Key == "backup_set" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the record carries %d backup_set fields. A reader taking the first and a reader taking the last then disagree about which set the event belongs to, which is a line filed under one strip and printed under another",
			seen)
	}

	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &line); err != nil {
		t.Fatalf("decoding the log line: %v", err)
	}
	tapped, _ := fieldValue(got[0], "backup_set")
	if line["backup_set"] != tapped {
		t.Errorf("the log line says backup_set=%v and the tap was told %q; one event cannot belong to two sets", line["backup_set"], tapped)
	}
	// The Logger's own binding wins, for the reason contextBackupSet
	// already gives: a caller that named its set is the authority on it,
	// and a cycle-scoped context value overwriting that turns a correct
	// field into a wrong one.
	if tapped != "api-server/var-backups" {
		t.Errorf("the bound set was overwritten by the context's: %q", tapped)
	}
}
