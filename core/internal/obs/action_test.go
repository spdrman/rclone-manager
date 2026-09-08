package obs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// How an operation WENT, and whether it ever finished (issue #625).
//
// The two claims these cases hold to are the two the terminal could not
// make before. A completion has to STATE its outcome, because the only
// alternative on offer was a client re-deriving one from event names and
// lifecycle states across about eight rules, every one of them shaped
// "if this still looks neutral, call it good", which leaves anything the
// rules do not recognise reading as a neutral note. And a start has to be
// paired with its completion by something other than habit, because
// cycle_start and cycle_end were matched by convention alone and nothing
// could notice the case an operator most needs named: the action that
// announced itself and then went quiet.
//
// Both are asserted through the tap AND through the log line, on purpose.
// They are one fact with two readers, and a fact only one of them can see
// is how the two come to disagree.

// TestResultVocabularyIsTheOneTheEpicNames pins the four values against
// literals typed out again here, for the reason events_test.go's own
// table gives: an enum compared against itself passes whatever either
// side becomes, and these strings are on an API surface (the live feed's
// wire contract) and in a log line that alert rules key off.
//
// "warn", not "warning". Every other tone vocabulary in this repository
// says warn, starting with the Level sitting beside this on the same
// line, and one field spelling it differently is a client writing
// `=== "warn"` against the wrong one of two adjacent enums.
func TestResultVocabularyIsTheOneTheEpicNames(t *testing.T) {
	cases := []struct {
		name string
		got  Result
		want string
	}{
		{"ResultSuccess", ResultSuccess, "success"},
		{"ResultWarn", ResultWarn, "warn"},
		{"ResultError", ResultError, "error"},
		{"ResultInfo", ResultInfo, "info"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s = %q, want %q (this is a breaking change for the live feed's contract and for anything filtering the log)", c.name, c.got, c.want)
		}
	}
	if len(Results) != len(cases) {
		t.Errorf("Results lists %d values and there are %d constants; the list is what the wire contract's enum is compared against", len(Results), len(cases))
	}
}

// TestTheResultKeyDoesNotTakeAnEventsOwnFieldName is the reason this is
// called result at all.
//
// The record-level mark used to be written under "outcome", and
// connection_test has carried a field of its own by that name since issue
// #596. Making room for the newcomer by renaming the incumbent is
// backwards: fields are not schema'd, nothing in tests/compat catches it,
// and a log query matching event=connection_test AND outcome=failed goes
// silently empty. So the newcomer takes a free name instead, and the
// incumbent keeps the one it shipped with.
func TestTheResultKeyDoesNotTakeAnEventsOwnFieldName(t *testing.T) {
	if fieldResult != "result" {
		t.Errorf("the record's own mark is written under %q", fieldResult)
	}
	for _, taken := range []string{"outcome", "step", "level", "event"} {
		if fieldResult == taken {
			t.Errorf("the record's own mark is written under %q, which an event already uses for a field of its own", taken)
		}
	}
}

// TestOutcomePicksTheSeverityAStatedOutcomeDeserves is the half that
// keeps level and outcome from drifting into two different stories about
// one line. They answer different questions, and an emitter that states
// an error outcome at info level has said both that it failed and that
// nobody needs to look.
func TestOutcomePicksTheSeverityAStatedOutcomeDeserves(t *testing.T) {
	cases := []struct {
		outcome Outcome
		want    Level
	}{
		{OutcomeSuccess, LevelInfo},
		{OutcomeInfo, LevelInfo},
		{OutcomeWarning, LevelWarn},
		{OutcomeError, LevelError},
	}
	for _, c := range cases {
		if got := c.outcome.Level(); got != c.want {
			t.Errorf("outcome %q emits at %v, want %v", c.outcome, got, c.want)
		}
	}
}

// TestCycleEndStatesHowItWentRatherThanLeavingItToBeInferred is the
// issue's first bullet against the one event that already had both
// halves of a pair. A cycle that finished cleanly says success; one that
// finished with an error says error; neither leaves a client to work it
// out from the absence of an error field.
func TestCycleEndStatesHowItWentRatherThanLeavingItToBeInferred(t *testing.T) {
	sink := &recordingSink{}
	var out bytes.Buffer
	l := New(&out, LevelDebug).WithSink(sink)
	ctx := context.Background()

	l.CycleEnd(ctx, "cycle-42", 0, nil)
	l.CycleEnd(ctx, "cycle-43", 0, errors.New("discovery blew up"))

	got := sink.all()
	if len(got) != 2 {
		t.Fatalf("the sink saw %d records and two cycle ends were logged", len(got))
	}
	if got[0].Outcome != OutcomeSuccess {
		t.Errorf("a clean cycle end states outcome %q, and a completion that does not say it went well reads as a neutral note", got[0].Outcome)
	}
	if got[1].Outcome != OutcomeError {
		t.Errorf("a failed cycle end states outcome %q, want %q", got[1].Outcome, OutcomeError)
	}

	lines := decodeLines(t, &out)
	if lines[0]["outcome"] != string(OutcomeSuccess) {
		t.Errorf("the log line for a clean cycle end carries outcome %v; the tap and the line are one fact with two readers", lines[0]["outcome"])
	}
	if lines[1]["outcome"] != string(OutcomeError) {
		t.Errorf("the log line for a failed cycle end carries outcome %v, want %q", lines[1]["outcome"], OutcomeError)
	}
}

// TestCycleStartAndCycleEndArePairedByMoreThanHabit is the issue's third
// bullet. The two lines carry the same action name and the same action
// id, so a reader can pair them without knowing that "cycle_start" and
// "cycle_end" happen to be spelled that way, and can notice a start that
// never got an end.
func TestCycleStartAndCycleEndArePairedByMoreThanHabit(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)
	ctx := context.Background()

	l.CycleStart(ctx, "cycle-42")
	l.CycleEnd(ctx, "cycle-42", 0, nil)

	got := sink.all()
	if len(got) != 2 {
		t.Fatalf("the sink saw %d records and a cycle start and end were logged", len(got))
	}
	start, end := got[0], got[1]
	if start.Action != ActionCycle || end.Action != ActionCycle {
		t.Errorf("the pair names actions %q and %q, want %q for both", start.Action, end.Action, ActionCycle)
	}
	if start.ActionID == "" {
		t.Fatal("a cycle start carries no action id, so nothing can notice a cycle that started and never reported an outcome")
	}
	if start.ActionID != end.ActionID {
		t.Errorf("the start's action id is %q and its end's is %q; a pair matched by convention is what this replaces", start.ActionID, end.ActionID)
	}
	if start.Outcome != "" {
		t.Errorf("a start states outcome %q; an action that has not finished has not gone any way yet, and that absence is what marks it as a start", start.Outcome)
	}
}

// TestBeginPairsAnyActionWithItsOwnCompletion is the general shape, for
// every operator-visible action that is not the cycle. One call opens it,
// one call closes it, and the id that ties them together is minted here
// rather than invented at each call site.
func TestBeginPairsAnyActionWithItsOwnCompletion(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)
	ctx := context.Background()

	action := l.Begin(ctx, "connection_test", "connection_test", "connection test starting")
	action.End(ctx, OutcomeWarning, "connection test finished with skipped steps")

	got := sink.all()
	if len(got) != 2 {
		t.Fatalf("the sink saw %d records and one action was begun and ended", len(got))
	}
	if got[0].ActionID == "" || got[0].ActionID != got[1].ActionID {
		t.Fatalf("the pair carries action ids %q and %q", got[0].ActionID, got[1].ActionID)
	}
	if got[0].Outcome != "" {
		t.Errorf("the start states outcome %q, and a start has not gone any way yet", got[0].Outcome)
	}
	if got[1].Outcome != OutcomeWarning {
		t.Errorf("the completion states outcome %q, want %q", got[1].Outcome, OutcomeWarning)
	}
	if got[1].Level != LevelWarn {
		t.Errorf("a completion stating a warning was emitted at %v; the level follows the outcome so one line does not tell two stories", got[1].Level)
	}
	if _, ok := fieldValue(got[1], "duration"); !ok {
		t.Error("the completion carries no duration, and the handle is holding the moment the action started")
	}
}

// TestAnActionsCompletionInheritsWhatItsStartWasGiven is the bug three
// reviewers of #627 found independently, and it is about where a line
// LANDS rather than about what it says.
//
// service/liveactivity.go routes each record into a bucket by reading
// backup_set off it, and it looks for an unfinished action per bucket. So
// a start carrying a backup set whose completion does not is a start in
// one ring and a completion in another, and the start is reported as an
// action that announced itself and went quiet for as long as the buffer
// holds it. Nothing about that is visible from the call site: it is two
// correct-looking calls, and the second one is Succeeded, which takes no
// attributes at all and is the one this package's own doc calls the case
// worth having a name for.
//
// So the handle carries what Begin was given and the completion inherits
// it. A caller that wants to say something else still can, and what it
// passes wins, which is the same rule emit already applies to what With
// bound.
func TestAnActionsCompletionInheritsWhatItsStartWasGiven(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)
	ctx := context.Background()

	l.Begin(ctx, "connection_test", "connection_test", "connection test starting",
		slog.String("backup_set", "alpha/nightly")).
		Succeeded(ctx, "connection test finished")

	got := sink.all()
	if len(got) != 2 {
		t.Fatalf("the sink saw %d records and one action was begun and ended", len(got))
	}
	for i, r := range got {
		v, ok := fieldValue(r, "backup_set")
		if !ok || v != "alpha/nightly" {
			t.Errorf("record %d carries backup_set=%q (present=%v); a start and a completion that disagree about their set land in two different rings, and the start is then unfinished forever", i, v, ok)
		}
	}
}

// TestACompletionsOwnAttributeWinsOverTheStarts is the other half of that
// rule. The inherited attributes are a default, not a floor: a completion
// that has something newer to say about a key says it, and there is one
// value under that key rather than two, for the reason emit's own
// duplicate-key note gives.
func TestACompletionsOwnAttributeWinsOverTheStarts(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)
	ctx := context.Background()

	l.Begin(ctx, "medium_verify", "medium_verify", "verifying", slog.String("medium", "offsite_s3"), slog.String("phase", "start")).
		End(ctx, OutcomeSuccess, "verified", slog.String("phase", "end"))

	end := sink.all()[1]
	seen := 0
	for _, f := range end.Fields {
		if f.Key == "phase" {
			seen++
			if f.Value != "end" {
				t.Errorf("the completion carries phase=%q, and it passed \"end\" itself", f.Value)
			}
		}
	}
	if seen != 1 {
		t.Errorf("the completion carries %d phase fields; a duplicate key is worse than either answer", seen)
	}
	if v, ok := fieldValue(end, "medium"); !ok || v != "offsite_s3" {
		t.Errorf("the completion carries medium=%q (present=%v); a key the completion said nothing about is inherited", v, ok)
	}
}

// TestAnActionClosesOnce is about the adoption pattern that reads best
// and breaks today. `defer action.Failed(ctx, err, ...)` beside an
// explicit Succeeded on the happy path is the obvious way to write a
// function that can leave several ways, and it writes two completions for
// one action: the feed then holds a success and an error for one id, and
// an operator reading a terminal sees the action fail after it succeeded.
func TestAnActionClosesOnce(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)
	ctx := context.Background()

	func() {
		action := l.Begin(ctx, "cycle", ActionCycle, "cycle starting")
		defer action.Failed(ctx, errors.New("left without finishing"), "cycle did not finish")
		action.Succeeded(ctx, "cycle finished")
	}()

	got := sink.all()
	if len(got) != 2 {
		t.Fatalf("the sink saw %d records; one action begun and ended is two lines, and a second completion is a feed holding two verdicts for one id", len(got))
	}
	if got[1].Outcome != OutcomeSuccess {
		t.Errorf("the completion states outcome %q; the first close is the one that happened", got[1].Outcome)
	}
}

// TestFailedStatesTheErrorAndTheOutcomeTogether is the shorthand every
// call site would otherwise write by hand, and getting it wrong in one
// place is how an error ends up logged at info.
func TestFailedStatesTheErrorAndTheOutcomeTogether(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)
	ctx := context.Background()

	l.Begin(ctx, "medium_verify", "medium_verify", "verifying offsite_s3").
		Failed(ctx, errors.New("no credentials for offsite_s3"), "verification failed")

	got := sink.all()
	end := got[len(got)-1]
	if end.Outcome != OutcomeError {
		t.Errorf("a failed completion states outcome %q, want %q", end.Outcome, OutcomeError)
	}
	if end.Level != LevelError {
		t.Errorf("a failed completion was emitted at %v, want %v", end.Level, LevelError)
	}
	if v, ok := fieldValue(end, "error"); !ok || v != "no credentials for offsite_s3" {
		t.Errorf("a failed completion carries error=%q (present=%v)", v, ok)
	}
}

// TestALifecycleTransitionIntoAFailureIsLoudEnoughToFind is the half of
// this event the engine does own (issue #625).
//
// The catalog's note says a transition states no outcome, because WHICH
// resting states read as good news is a decision about a screen. That
// argument covers the good news and not the bad: an artifact that ended
// an attempt in one of the three states this machine calls exceptional is
// a backup that did not happen, which is the manager failing at the one
// thing it is for, and no screen has to be consulted to know it.
//
// It arrived at info, so the browser painted it red off its own list of
// state names and `activity --follow --severity error` showed nothing at
// all. The two surfaces disagreed about the most important line either of
// them carries.
func TestALifecycleTransitionIntoAFailureIsLoudEnoughToFind(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	l := New(&out, LevelDebug).WithSink(sink)
	ctx := context.Background()

	l.LifecycleTransition(ctx, "alpha/nightly/one.dump", "VERIFYING", "QUARANTINED", "md5 differs", true)
	l.LifecycleTransition(ctx, "alpha/nightly/two.dump", "DISCOVERED", "TRANSFERRING", "", false)

	got := sink.all()
	if got[0].Level != LevelError {
		t.Errorf("a transition into a failure state was emitted at %v; `activity --follow --severity error` is where an operator goes to find a backup that did not happen", got[0].Level)
	}
	if got[0].Outcome != OutcomeError {
		t.Errorf("a transition into a failure state states outcome %q, want %q", got[0].Outcome, OutcomeError)
	}

	// The ordinary transition is untouched, which is the whole of the
	// catalog note's argument: this states the failure and still declines
	// to decide which of the other states are good news.
	if got[1].Level != LevelInfo {
		t.Errorf("an ordinary transition was emitted at %v, want %v", got[1].Level, LevelInfo)
	}
	if got[1].Outcome != "" {
		t.Errorf("an ordinary transition states outcome %q, and which resting states read as good news is a decision about a screen", got[1].Outcome)
	}

	lines := decodeLines(t, &out)
	if lines[0]["level"] != "ERROR" || lines[0]["outcome"] != string(OutcomeError) {
		t.Errorf("the log line for a failed transition is level=%v outcome=%v; the tap and the line are one fact with two readers", lines[0]["level"], lines[0]["outcome"])
	}
	if _, stated := lines[1]["outcome"]; stated {
		t.Errorf("the log line for an ordinary transition carries an outcome: %v", lines[1]["outcome"])
	}
}

// TestAConditionStatesNoOutcomeBecauseNothingRan is the boundary of this
// vocabulary, asserted rather than only written down. A filesystem
// crossing a threshold is a fact about the world this process noticed,
// not an operation that went one way or another, and a field that
// stretched to cover it would be a second vocabulary for severity with
// the same four values as the first.
func TestAConditionStatesNoOutcomeBecauseNothingRan(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)
	ctx := context.Background()

	l.StaleBackup(ctx, "alpha/nightly", 48*time.Hour, 24*time.Hour)
	l.DiskPressure(ctx, "/data", 1, 100, "critical")

	for _, got := range sink.all() {
		if got.Outcome != "" {
			t.Errorf("%s states outcome %q, and it reports a condition rather than an operation", got.Event, got.Outcome)
		}
	}
	// The level is how these say how much attention they want, and that
	// half was never the problem.
	if sink.all()[0].Level != LevelWarn || sink.all()[1].Level != LevelError {
		t.Errorf("the two conditions were emitted at %v and %v, want warn and error", sink.all()[0].Level, sink.all()[1].Level)
	}
}

// TestCompletedStatesAnOutcomeWithNoStartToPairItWith is the honest half
// of a pair, for work that was over before anything could report it. An
// /api/v1 request is the case: the status IS the outcome, and a start
// line emitted at the recorder would be announcing something that had
// already happened.
func TestCompletedStatesAnOutcomeWithNoStartToPairItWith(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)

	l.Completed(context.Background(), OutcomeSuccess, "api_action", "post /operations")

	got := sink.all()[0]
	if got.Outcome != OutcomeSuccess {
		t.Errorf("the line states outcome %q, want %q", got.Outcome, OutcomeSuccess)
	}
	if got.ActionID != "" {
		t.Errorf("an unpaired completion carries action id %q, which would report as a start nothing will ever close", got.ActionID)
	}
}

// TestCompletedAtLetsAnEmitterBeQuieterThanItsOutcome is the exception
// this package's own convention needs. It reserves LevelError for the
// manager failing at something, and a check that correctly reports the
// far side as unreachable is the manager working exactly as designed: an
// error outcome logged as a warning is two right answers to two
// questions, not one line contradicting itself.
func TestCompletedAtLetsAnEmitterBeQuieterThanItsOutcome(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)

	l.CompletedAt(context.Background(), LevelWarn, OutcomeError, "connection_test", "connection test: host_key failed")

	got := sink.all()[0]
	if got.Level != LevelWarn {
		t.Errorf("the line was emitted at %v, and its emitter asked for %v", got.Level, LevelWarn)
	}
	if got.Outcome != OutcomeError {
		t.Errorf("the line states outcome %q, want %q; the whole point of the pair is that the two say different things", got.Outcome, OutcomeError)
	}
}

// TestAnActionOnASilentLoggerIsSilentRatherThanAPanic holds this package's
// standing rule at the new surface: a nil *Logger is a safe no-op on
// every method, so a Deps struct that never got one keeps working. A
// handle that panicked when it was ended would make that false for every
// caller that adopted the pair.
func TestAnActionOnASilentLoggerIsSilentRatherThanAPanic(t *testing.T) {
	var l *Logger
	ctx := context.Background()
	action := l.Begin(ctx, "cycle", ActionCycle, "cycle starting")
	action.End(ctx, OutcomeSuccess, "cycle finished")
	action.Succeeded(ctx, "again")
	action.Failed(ctx, errors.New("boom"), "and again")

	var nilAction *Action
	nilAction.End(ctx, OutcomeSuccess, "on nothing at all")
	if nilAction.ID() != "" {
		t.Errorf("a nil action has id %q", nilAction.ID())
	}
}

// TestAMarkedLineNeverCarriesADuplicateKey is the shadowing rule
// DiskPressure's own doc records, one field along. The record's outcome,
// action and action id are written into the log line under three reserved
// keys, so an event that also logged an attribute called one of them
// would produce a JSON object with two: the tap takes the first and
// encoding/json keeps the last, and two readers of one event would
// disagree about how it went.
//
// No event in the catalog does that, which is what "reserved" means, and
// this is what happens if one ever tries. The event's own field stands,
// for the same reason contextBackupSet leaves an event's own backup_set
// alone: the event is the authority on its own fields. The typed outcome
// still reaches the tap, because it is a field of the Record rather than
// one of the flat list.
func TestAMarkedLineNeverCarriesADuplicateKey(t *testing.T) {
	var out bytes.Buffer
	sink := &recordingSink{}
	l := New(&out, LevelDebug).WithSink(sink)

	l.Completed(context.Background(), OutcomeSuccess, "unit_test", "a made-up event with a field of its own",
		slog.String(fieldOutcome, "passed"))

	if got := strings.Count(out.String(), `"outcome"`); got != 1 {
		t.Errorf("the log line carries %d outcome keys:\n%s", got, out.String())
	}
	lines := decodeLines(t, &out)
	if lines[0][fieldOutcome] != "passed" {
		t.Errorf("the log line's outcome is %v, and the event named its own; the event is the authority on its own fields", lines[0][fieldOutcome])
	}

	got := sink.all()[0]
	if got.Outcome != OutcomeSuccess {
		t.Errorf("the record's stated outcome is %q; it is a field of the Record and cannot be shadowed by one of the event's own", got.Outcome)
	}
	if v, ok := fieldValue(got, fieldOutcome); !ok || v != "passed" {
		t.Errorf("the event's own outcome field reached the tap as %q (present=%v); a field an event logged must not be dropped on the way", v, ok)
	}
}

// TestAMarksOwnAttributesDoNotAlsoArriveAsFields keeps the tap's list and
// the Record's typed fields from saying the same thing twice, which would
// leave every reader filtering three keys back out of the fields it
// prints beside a line.
func TestAMarksOwnAttributesDoNotAlsoArriveAsFields(t *testing.T) {
	sink := &recordingSink{}
	l := New(nil, LevelDebug).WithSink(sink)

	l.CycleEnd(context.Background(), "cycle-42", 0, nil)

	got := sink.all()[0]
	for _, key := range []string{fieldOutcome, fieldAction, fieldActionID} {
		if v, ok := fieldValue(got, key); ok {
			t.Errorf("the record carries %q=%q in its fields as well as in its own, so every reader has to filter it back out", key, v)
		}
	}
}
