package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/backupd/core/apicontract"
	"github.com/spdrman/backupd/core/internal/config"
	"github.com/spdrman/backupd/core/service"
)

// `backupd activity --follow`, driven against a real route.
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

// oneDeploymentReading is what a deployment-scoped read answers with:
// the bucket that belongs to no backup set, and no sets at all. It is
// also the shape a fresh install answers EVERY read with, which is the
// case the deployment bucket was added for (#593).
func oneDeploymentReading(epoch string, latest int64, events ...apicontract.LiveActivityEvent) apicontract.LiveActivityResponse {
	return apicontract.LiveActivityResponse{
		Epoch:       epoch,
		ObservedAt:  time.Now().Format(time.RFC3339Nano),
		PollAfterMs: 1,
		Sets:        []apicontract.LiveActivitySet{},
		Deployment: &apicontract.LiveActivityDeployment{
			LatestSequence: latest,
			Events:         events,
		},
	}
}

func deploymentEvent(seq int64, level, event, message string) apicontract.LiveActivityEvent {
	e := liveEvent(seq, level, event, message)
	e.Scope = "deployment"
	return e
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

// TestActivityFollow_PrintsHowEachLineWent is CLI parity for issue #625.
//
// The terminal in the browser echoes the command that produced an action
// and colours the line by the outcome the engine stated, so a command
// line that printed everything BUT that outcome would be a second, worse
// account of the same moment: the two surfaces are meant to tell one
// story, and this is the field the story is now told in.
func TestActivityFollow_PrintsHowEachLineWent(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	start := liveEvent(1, "info", "cycle_start", "cycle starting")
	start.Action = "cycle"
	start.ActionID = "cycle-42"
	done := liveEvent(2, "info", "cycle_end", "cycle finished")
	done.Action = "cycle"
	done.ActionID = "cycle-42"
	done.Result = "success"
	e.holdLiveActivity(oneLiveReading("epoch-1", 2, start, done))

	out, _, err := followFor(t, e, followOptions{}, 120*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	if !strings.Contains(out, "success") {
		t.Errorf("the follow output does not say how the cycle went; a completion that only says it finished is the gap #625 is about:\n%s", out)
	}
	// The start does not claim one. An action that has not finished has
	// not gone any way yet, and printing a value there would make the
	// column meaningless on exactly the lines a reader is waiting on.
	first, _, ok := strings.Cut(out, "\n")
	if !ok {
		t.Fatalf("the follow output is a single line:\n%s", out)
	}
	if strings.Contains(first, "success") {
		t.Errorf("the start line carries a result:\n%s", first)
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

func TestActivityFollow_PrintsADeploymentWideLineOnceAndWhereItHappened(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	// The shape the engine actually answers with since #593: a set's own
	// lines in that set's bucket, the deployment's own in its own bucket,
	// and ONE sequence counter across both. The old version of this case
	// hand-built a reading that copied a deployment-wide event into every
	// set, which is a reading the engine can no longer produce, so it
	// passed without exercising the follow at all.
	//
	// Two claims, and the second is why the sort exists: every line
	// reaches the operator exactly once, and they arrive in the order
	// they happened rather than grouped by whichever bucket the response
	// listed first.
	reading := oneLiveReading("epoch-1", 4,
		liveEvent(2, "info", "discovery", "discovery pass complete"),
		liveEvent(4, "info", "commit", "durable commit complete"),
	)
	reading.Deployment = &apicontract.LiveActivityDeployment{
		LatestSequence: 4,
		Events: []apicontract.LiveActivityEvent{
			deploymentEvent(1, "info", "cycle_start", "cycle starting"),
			deploymentEvent(3, "warn", "disk_pressure", "the backup volume is filling up"),
		},
	}
	e.holdLiveActivity(reading, oneLiveReading("epoch-1", 4))

	out, _, err := followFor(t, e, followOptions{}, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	for _, want := range []string{"cycle starting", "discovery pass complete", "the backup volume is filling up", "durable commit complete"} {
		if got := strings.Count(out, want); got != 1 {
			t.Errorf("%q was printed %d time(s), want exactly one:\n%s", want, got, out)
		}
	}
	var order []int
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		_ = i
		switch {
		case strings.Contains(line, "cycle starting"):
			order = append(order, 1)
		case strings.Contains(line, "discovery pass complete"):
			order = append(order, 2)
		case strings.Contains(line, "the backup volume is filling up"):
			order = append(order, 3)
		case strings.Contains(line, "durable commit complete"):
			order = append(order, 4)
		}
	}
	if len(order) != 4 || order[0] != 1 || order[1] != 2 || order[2] != 3 || order[3] != 4 {
		t.Errorf("the lines reached the operator in sequence order %v, want 1 2 3 4: the two buckets interleave in time, and a follow that printed one bucket and then the other would put the line before an error somewhere else on the screen:\n%s", order, out)
	}
}

// TestActivityFollow_ReadsTheDeploymentBucketOnADeploymentWithNoSets is
// the case --follow could not answer at all.
//
// The loop walked reading.Sets and nothing else, so on a fresh install,
// where every reading has an empty set list and everything that happens
// lands in the deployment bucket, it printed nothing, for ever, and its
// cursor never moved. That is exactly the moment the bucket was added
// for: a new operator pressing buttons in a wizard with no configured
// set anywhere to read.
func TestActivityFollow_ReadsTheDeploymentBucketOnADeploymentWithNoSets(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	e.holdLiveActivity(
		oneDeploymentReading("epoch-1", 2,
			deploymentEvent(1, "info", "startup", "backupd starting"),
			deploymentEvent(2, "info", "api_action", "patch /settings"),
		),
		oneDeploymentReading("epoch-1", 2),
	)

	out, _, err := followFor(t, e, followOptions{}, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	for _, want := range []string{"backupd starting", "patch /settings"} {
		if got := strings.Count(out, want); got != 1 {
			t.Errorf("%q was printed %d time(s) on a deployment whose only bucket is the deployment's own; a follow that reads Sets alone prints nothing here for ever:\n%s", want, got, out)
		}
	}

	cursors := e.cursorsSeen()
	if len(cursors) < 2 {
		t.Fatalf("the loop made %d reading(s), so it cannot be shown to carry a cursor at all", len(cursors))
	}
	if cursors[1] != 2 {
		t.Errorf("the second reading asked since=%d, want 2, which is the highest sequence the deployment bucket says it holds. A cursor that never advances re-requests the same window for ever", cursors[1])
	}
}

// TestActivityFollow_ScopeDeploymentAsksForTheDeploymentBucketAlone is
// the request half of the same distinction. Naming no set already means
// "every set", so the deployment's own log is the one reading a cursor
// cannot express without this.
func TestActivityFollow_ScopeDeploymentAsksForTheDeploymentBucketAlone(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	e.holdLiveActivity(oneDeploymentReading("epoch-1", 1, deploymentEvent(1, "info", "cycle_start", "cycle starting")))

	out, _, err := followFor(t, e, followOptions{scope: service.LiveActivityScopeDeployment}, 120*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	if !strings.Contains(out, "cycle starting") {
		t.Errorf("the deployment-scoped follow printed nothing:\n%s", out)
	}
	scopes := e.scopesSeen()
	if len(scopes) == 0 || scopes[0] != service.LiveActivityScopeDeployment {
		t.Errorf("the request carried scope=%q, want %q: without it a terminal following the deployment's own log has to ask for every set as well", scopes, service.LiveActivityScopeDeployment)
	}
}

// TestActivityFollow_HoldsTheCursorAtATruncatedBucket is the paging
// claim, and the reason the engine hands back the OLDEST slice above a
// cursor rather than the newest.
//
// The engine says "there is more of this bucket than the limit let me
// give you" and expects to be asked again from where it stopped. A
// client that advances to the highest sequence anywhere in the reading
// instead steps clean over the rest of that bucket, and nothing in the
// next response mentions it: the events are still held, still inside the
// buffer, and simply never requested again. One busy set beside one
// quiet one is all it takes.
func TestActivityFollow_HoldsTheCursorAtATruncatedBucket(t *testing.T) {
	e := startFakeEngine(t, writeTestConfig(t))
	first := oneLiveReading("epoch-1", 300,
		liveEvent(1, "info", "discovery", "discovery pass complete"),
		liveEvent(2, "info", "transfer_stats", "transferring"),
		liveEvent(3, "info", "commit", "durable commit complete"),
	)
	// One page of a bucket that holds up to 300, beside a second set that
	// is quiet and far ahead. 4..300 are still held and are what the next
	// reading has to ask for.
	first.Sets[0].Truncated = true
	first.Sets = append(first.Sets, apicontract.LiveActivitySet{
		BackupSetID:    "production/billing-mysql",
		LatestSequence: 400,
		ProgressBasis:  "unknown",
		Events:         []apicontract.LiveActivityEvent{liveEvent(400, "info", "commit", "the other set committed")},
	})
	e.holdLiveActivity(first, oneLiveReading("epoch-1", 400,
		liveEvent(4, "info", "commit", "the line after the page"),
	))

	_, _, err := followFor(t, e, followOptions{}, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("followActivity: %v", err)
	}
	cursors := e.cursorsSeen()
	if len(cursors) < 2 {
		t.Fatalf("the loop made %d reading(s), too few to show where the cursor landed", len(cursors))
	}
	if cursors[1] != 3 {
		t.Errorf("the reading after a truncated bucket asked since=%d, want 3: the bucket said it had handed over one page and stopped at 3, so anything above that skips 4..300 permanently, with nothing in the next response saying they were skipped", cursors[1])
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

// TestActivity_ScopeIsAFollowFlagAndTakesOnlyTheDeploymentScope pins the
// three ways --scope can be typed wrong, all of which are wrong on every
// deployment rather than on this one, so all three are settled on the
// command line before anything is opened.
func TestActivity_ScopeIsAFollowFlagAndTakesOnlyTheDeploymentScope(t *testing.T) {
	configPath := writeTestConfig(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			"a scope this contract does not have",
			[]string{"--follow", "--scope", "sets"},
			"is not a scope",
		},
		{
			// The durable log is one table of transitions and has no
			// deployment bucket to narrow to, so this is a flag asking
			// the wrong feed a question only the other one has.
			"the durable log has no deployment bucket",
			[]string{"--scope", "deployment"},
			"--follow",
		},
		{
			// The engine would answer this one, and answer it about the
			// set, because a named set is the narrower question. A
			// command line is where an operator can be told instead of
			// quietly given the other half of what they typed.
			"a set and the deployment are two different questions",
			[]string{"--follow", "--scope", "deployment", "--backup-set", "alpha/nightly"},
			"--backup-set",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() {
				captureStdout(t, func() {
					code = run(append([]string{"activity", "--config", configPath}, tc.args...))
				})
			})
			if code != exitUsage {
				t.Errorf("`activity %s` exited %d, want %d\nstderr: %s", strings.Join(tc.args, " "), code, exitUsage, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("the refusal does not mention %q, which is what the operator has to change\nstderr: %s", tc.want, stderr)
			}
			if strings.Contains(stderr, "mode: ") {
				t.Errorf("the command announced a read mode, so it opened the configuration and the journal before refusing a command line that is wrong whatever they hold\nstderr: %s", stderr)
			}
		})
	}
}

func TestActivity_FollowIsPinnedInUsage(t *testing.T) {
	text := captureStderr(t, usage)
	if !strings.Contains(text, "  activity --follow ") {
		t.Errorf("usage() has no entry for `activity --follow`, so the live half is undiscoverable:\n%s", text)
	}
}
