package obs

import (
	"bytes"
	"context"
	"errors"
	"testing"
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

// TestOutcomeVocabularyIsTheOneTheEpicNames pins the four values against
// literals typed out again here, for the reason events_test.go's own
// table gives: an enum compared against itself passes whatever either
// side becomes, and these strings are on an API surface (the live feed's
// wire contract) and in a log line that alert rules key off.
func TestOutcomeVocabularyIsTheOneTheEpicNames(t *testing.T) {
	cases := []struct {
		name string
		got  Outcome
		want string
	}{
		{"OutcomeSuccess", OutcomeSuccess, "success"},
		{"OutcomeWarning", OutcomeWarning, "warning"},
		{"OutcomeError", OutcomeError, "error"},
		{"OutcomeInfo", OutcomeInfo, "info"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s = %q, want %q (this is a breaking change for the live feed's contract and for anything filtering the log)", c.name, c.got, c.want)
		}
	}
	if len(Outcomes) != len(cases) {
		t.Errorf("Outcomes lists %d values and there are %d constants; the list is what the wire contract's enum is compared against", len(Outcomes), len(cases))
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

// TestNoEventLogsItsOwnFieldUnderAReservedKey is the shadowing rule
// DiskPressure's own doc records, one field along. The record's outcome,
// action and action id are written into the log line under those three
// keys, so an event that also logged an attribute called one of them
// would produce a JSON object with a duplicate key: the tap takes the
// first and encoding/json keeps the last, and two readers of one event
// would disagree about how it went.
func TestNoEventLogsItsOwnFieldUnderAReservedKey(t *testing.T) {
	for _, key := range []string{fieldOutcome, fieldAction, fieldActionID} {
		if !reservedFieldKey(key) {
			t.Errorf("%q is written into every marked line and is not reserved", key)
		}
	}
	if reservedFieldKey("step") {
		t.Error("an ordinary event field is being treated as reserved")
	}
}
