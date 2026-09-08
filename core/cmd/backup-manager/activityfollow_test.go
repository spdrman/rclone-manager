package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/apicontract"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// `backup-manager activity --follow`, driven against a real route.
//
// The three claims worth testing here are all about the CLIENT half of a
// polling contract, and none of them is visible from a single request.
// Does the cursor advance so a second reading does not repeat the first?
// Does it get DROPPED when the process that issued those sequence numbers
// goes away? And does a follow with no route to the engine refuse rather
// than quietly reading the journal, which would put a different feed on
// screen under the flag that asked for this one?
//
// followActivity takes a context and its two writers rather than reaching
// for os.Stdout, which is what makes any of this drivable: a loop that
// only ends on a signal cannot be ended by a test, and a test that could
// not end it would be a test of the flag parsing in front of it.

// followFor runs the follow loop with a real client against e, ending it
// after the loop has had time to make several readings.
func followFor(t *testing.T, e *fakeEngine, opts followOptions, d time.Duration) (string, string, error) {
	t.Helper()
	e.attach(t)

	cfg, err := config.LoadAndValidate(e.configPath)
	if err != nil {
		t.Fatalf("loading the engine's configuration: %v", err)
	}
	var out, errOut bytes.Buffer
	mode, err := enterReadMode(t.Context(), e.configPath, cfg, &errOut)
	if err != nil {
		t.Fatalf("entering read mode: %v", err)
	}
	if !mode.attached() {
		t.Fatalf("the fixture did not reach the engine, so this test would be about the refusal instead: %s", errOut.String())
	}

	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	err = followActivity(ctx, mode, opts, &out, &errOut)
	return out.String(), errOut.String(), err
}

// oneLiveReading is the shape the engine answers with, stated rather than
// produced. See the fake engine's own liveActivity for why.
func oneLiveReading(epoch string, latest int64, events ...apicontract.LiveActivityEvent) apicontract.LiveActivityResponse {
	return apicontract.LiveActivityResponse{
		Epoch:       epoch,
		ObservedAt:  time.Now().Format(time.RFC3339Nano),
		PollAfterMs: 1,
		Sets: []apicontract.LiveActivitySet{{
			BackupSetID:    "production/postgres-primary",
			LatestSequence: latest,
			ProgressBasis:  "unknown",
			Events:         events,
		}},
	}
}

func liveEvent(seq int64, level, event, message string, fields ...apicontract.LiveActivityField) apicontract.LiveActivityEvent {
	return apicontract.LiveActivityEvent{
		At:       time.Now().Format(time.RFC3339Nano),
		Event:    event,
		Fields:   fields,
		Level:    level,
		Message:  message,
		Scope:    "set",
		Sequence: seq,
	}
}

func TestActivityFollow_PrintsTheLinesTheEngineHolds(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	e.holdLiveActivity(oneLiveReading("epoch-1", 2,
		liveEvent(1, "info", "cycle_start", "cycle starting"),
		liveEvent(2, "error", "error", "the remote refused", apicontract.LiveActivityField{Key: "op", Value: "transfer"}),
	))

	out, _, err := followFor(t, e, followOptions{}, 120*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	for _, want := range []string{"cycle_start", "cycle starting", "the remote refused", "op=transfer", "ERROR"} {
		if !strings.Contains(out, want) {
			t.Errorf("the follow output does not carry %q:\n%s", want, out)
		}
	}
}

func TestActivityFollow_TheCursorAdvancesSoNothingIsPrintedTwice(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	// Two readings and then silence: the second is repeated for every
	// later poll, which is what an idle engine looks like.
	e.holdLiveActivity(
		oneLiveReading("epoch-1", 2, liveEvent(1, "info", "cycle_start", "cycle starting"), liveEvent(2, "info", "discovery", "discovery pass complete")),
		oneLiveReading("epoch-1", 2),
	)

	out, _, err := followFor(t, e, followOptions{}, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	if got := strings.Count(out, "cycle starting"); got != 1 {
		t.Errorf("the first line was printed %d times; a cursor that did not advance reprints the whole window on every tick:\n%s", got, out)
	}

	cursors := e.cursorsSeen()
	if len(cursors) < 2 {
		t.Fatalf("the loop made %d reading(s), so it cannot be shown to carry a cursor at all", len(cursors))
	}
	if cursors[0] != 0 {
		t.Errorf("the first reading asked since=%d, want 0: a follow starts from whatever the engine still holds", cursors[0])
	}
	if cursors[1] != 2 {
		t.Errorf("the second reading asked since=%d, want 2, which is the highest sequence the engine said it holds", cursors[1])
	}
}

func TestActivityFollow_DropsTheCursorWhenTheEngineRestarts(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	// A restart: a new epoch, and sequence numbers that start again. A
	// client that carried its cursor across would ask this process for
	// everything after 7, and this process has never emitted a 7, so the
	// operator would watch an empty feed for ever.
	e.holdLiveActivity(
		oneLiveReading("epoch-1", 7, liveEvent(7, "info", "cycle_start", "the old process")),
		oneLiveReading("epoch-2", 1, liveEvent(1, "info", "startup", "the new process")),
		oneLiveReading("epoch-2", 1, liveEvent(1, "info", "startup", "the new process")),
	)

	out, errOut, err := followFor(t, e, followOptions{}, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}

	if !strings.Contains(errOut, "restarted") {
		t.Errorf("the restart was not said out loud, so the gap in the feed is invisible:\n%s", errOut)
	}
	if !strings.Contains(out, "the new process") {
		t.Errorf("nothing from the new process reached the operator:\n%s", out)
	}

	cursors := e.cursorsSeen()
	if len(cursors) < 3 {
		t.Fatalf("the loop made %d reading(s), too few to show a cursor being dropped", len(cursors))
	}
	// 0 to start, 7 carried from the old process, then 0 again because
	// the epoch changed. Anything else here is #573's own bug.
	if cursors[1] != 7 {
		t.Errorf("the second reading asked since=%d, want 7; without that this test cannot show a cursor being dropped, because there was none to drop", cursors[1])
	}
	if cursors[2] != 0 {
		t.Errorf("the reading after the restart asked since=%d, want 0. A cursor from a process that is gone names sequence numbers the new one has never issued, so carrying it across shows an empty feed for ever (#573)", cursors[2])
	}
}

func TestActivityFollow_NeverReprintsADeploymentWideEventOncePerSet(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	// A cycle starting is reported to EVERY set's strip by design, so a
	// nested walk over sets prints it once per configured set.
	shared := liveEvent(1, "info", "cycle_start", "cycle starting")
	shared.Scope = "deployment"
	reading := oneLiveReading("epoch-1", 1, shared)
	reading.Sets = append(reading.Sets, apicontract.LiveActivitySet{
		BackupSetID:    "production/billing-mysql",
		LatestSequence: 1,
		ProgressBasis:  "unknown",
		Events:         []apicontract.LiveActivityEvent{shared},
	})
	e.holdLiveActivity(reading, oneLiveReading("epoch-1", 1))

	out, _, err := followFor(t, e, followOptions{}, 120*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	if got := strings.Count(out, "cycle starting"); got != 1 {
		t.Errorf("a deployment-wide event was printed %d times, once per set that carries it:\n%s", got, out)
	}
}

func TestActivityFollow_SeverityNarrowsWithoutStallingTheCursor(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	e.holdLiveActivity(
		oneLiveReading("epoch-1", 3,
			liveEvent(1, "info", "cycle_start", "cycle starting"),
			liveEvent(2, "warn", "stale_backup", "this set is stale"),
			liveEvent(3, "info", "commit", "durable commit complete"),
		),
		oneLiveReading("epoch-1", 3),
	)

	out, _, err := followFor(t, e, followOptions{minSeverity: severityWarn}, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	if strings.Contains(out, "cycle starting") || strings.Contains(out, "durable commit") {
		t.Errorf("--severity warn let ordinary notes through:\n%s", out)
	}
	if !strings.Contains(out, "this set is stale") {
		t.Errorf("--severity warn dropped the warning it was asked for:\n%s", out)
	}

	// The cursor comes off what the engine HOLDS, not off what was
	// printed. Taking the printed one would leave a filtered follow
	// re-requesting the same window for ever.
	cursors := e.cursorsSeen()
	if len(cursors) >= 2 && cursors[1] != 3 {
		t.Errorf("the second reading asked since=%d, want 3: a filter must not stall the cursor at the last line that happened to pass it", cursors[1])
	}
}

func TestActivityFollow_JSONEmitsOneWireEventPerLine(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	e.holdLiveActivity(
		oneLiveReading("epoch-1", 1, liveEvent(1, "info", "cycle_start", "cycle starting")),
		oneLiveReading("epoch-1", 1),
	)

	out, _, err := followFor(t, e, followOptions{asJSON: true}, 120*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	line := strings.TrimSpace(out)
	if line == "" {
		t.Fatal("--json printed nothing at all")
	}
	var event apicontract.LiveActivityEvent
	if err := json.Unmarshal([]byte(strings.Split(line, "\n")[0]), &event); err != nil {
		t.Fatalf("--json emitted something that is not a LiveActivityEvent (%v): %s", err, line)
	}
	if event.Event != "cycle_start" || event.Sequence != 1 {
		t.Errorf("the emitted event is not the one the engine held: %+v", event)
	}
}

// TestActivityFollow_RefusesWithNoRouteRatherThanReadingTheJournal is the
// claim that keeps the two feeds apart.
//
// A fallback would put the durable transition log on screen under the flag
// that asked for the live one, and both would look right. That is this
// repository's own recurring defect, and it is the reason --follow refuses
// instead.
func TestActivityFollow_RefusesWithNoRouteRatherThanReadingTheJournal(t *testing.T) {
	configPath := writeTestConfig(t)
	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	var out, errOut bytes.Buffer
	mode, err := enterReadMode(t.Context(), configPath, cfg, &errOut)
	if err != nil {
		t.Fatalf("entering read mode: %v", err)
	}
	if mode.attached() {
		t.Fatal("something is serving this fixture, so this test is not about a read with no route")
	}

	err = followActivity(t.Context(), mode, followOptions{}, &out, &errOut)
	if err == nil {
		t.Fatal("--follow with no route to the engine returned no error, so it answered from somewhere")
	}
	if out.Len() != 0 {
		t.Errorf("--follow with no route printed a feed anyway:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "--follow") {
		t.Errorf("the refusal does not name the flag it is about: %v", err)
	}
}

func TestActivity_FollowIsPinnedInUsage(t *testing.T) {
	text := captureStderr(t, usage)
	if !strings.Contains(text, "  activity --follow ") {
		t.Errorf("usage() has no entry for `activity --follow`, so the live half is undiscoverable:\n%s", text)
	}
}
