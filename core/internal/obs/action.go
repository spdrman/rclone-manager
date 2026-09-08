package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"
)

// How an operation WENT, and the pairing that makes a start and its
// completion one thing (issue #625).
//
// # Why this is not a fifth level
//
// The obvious change is a "success" severity beside debug, info, warn and
// error, and it is the wrong one. A level is an ORDERING: everything that
// reads one, from `activity --follow --severity warn` to whatever alert
// rule an operator wired up, treats it as a threshold, and "success" has
// no place on a line between info and warn. So the two facts are kept
// apart. The level is how loudly a line was emitted; the outcome is how
// the thing it reports turned out. A retention pass that refused every
// deletion is a warning that succeeded at exactly what it was asked to
// do, and one field cannot say both.
//
// # Why the engine states it at all
//
// service/activity.go argues, correctly, that severity, headline and
// presentation category stay OFF the durable activity log, because a
// display decision must not become a durable domain concept. Nothing
// here reverses that, and the durable log is untouched.
//
// The distinction that makes both true is which of the two facts a line
// carries. How an operation went is a fact the engine OWNS: it ran the
// thing, and it is the only party that knows whether the copy landed.
// How that should look on a screen is not, and is still nobody's business
// but the client's. "This completed successfully" is the first; "draw it
// green" is the second. The live feed already carries the emitter's level,
// so it had already accepted the first half of that argument; what it was
// missing was the value that says success, which is why every client
// wanting to draw a completion as good news was re-deriving one from
// event names and lifecycle state values, and why anything the derivation
// did not recognise stayed a neutral note.
//
// # Why a start and a completion are tied by an id
//
// cycle_start and cycle_end were paired by the habit of spelling them
// that way. Nothing modelled an operation as something that begins and
// then concludes, so nothing could notice an action that announced itself
// and then went quiet, which is the state an operator most needs named.
// A start carries an action name and an action id and no outcome; its
// completion carries the same two and an outcome. That is the whole
// protocol, and it is what lets a reader pair them without knowing either
// event's name, and notice a start that never got its other half.
//
// A line in the MIDDLE of an action deliberately carries neither. A
// correlation id threaded through every line a running action produces is
// a bigger idea (it wants a context value, and every emitter on the path
// to cooperate) and it is not what this needs: the pairing exists so an
// unfinished action is detectable, and the two lines that bracket it are
// the two that answer that. The rule stays simple as a result, and a
// reader can apply it with no lookahead: an action id with no outcome is
// a start, an action id with an outcome is that start's completion.

// Result is how the operation a line reports turned out.
//
// Four values, and the fourth is the one that keeps this honest. Not
// every completion went well or badly: a preview that found nothing to
// do, a check that was skipped because it did not apply, a plan with
// nothing in it are all completions with no verdict to give, and forcing
// one of the other three onto them would make this field the thing an
// operator learns to distrust. An empty Result is different again and
// means the line states none at all, which is what a start and every
// ordinary progress note carry.
type Result string

const (
	// ResultSuccess is the value there was no way to say before this
	// existed. The operation did what it was asked to do.
	ResultSuccess Result = "success"

	// ResultWarn is a completion that finished, and finished with
	// something in it worth an operator's attention: a validation that
	// found the content wrong, a connection test whose steps were
	// skipped, a request refused for something the caller asked for.
	ResultWarn Result = "warn"

	// ResultError is a completion that did not do what it was asked to
	// do.
	ResultError Result = "error"

	// ResultInfo is a completion with no verdict: it ran, it finished,
	// and there is nothing to celebrate or worry about. See the type's
	// own doc for why this is not folded into success.
	ResultInfo Result = "info"
)

// Results lists every value this vocabulary has, in the order
// api/v1/openapi.json declares them.
//
// It exists so the wire contract's enum and this package's vocabulary can
// be compared rather than both being written down and trusted, which is
// the same reason service.LiveActivityOutcomes exists for the other
// enum on that feed.
var Results = []Result{ResultSuccess, ResultWarn, ResultError, ResultInfo}

// Level is the severity a line stating this outcome is emitted at, unless
// its emitter has a reason to say otherwise.
//
// It is the default for every completion in this file, so the ordinary
// call site does not pick a level by hand and cannot pick one that
// contradicts what it just said: an error stated at info has claimed both
// that the operation failed and that nobody needs to look, and whichever
// of the two a reader believes, the other one is a lie.
//
// The two are still different questions, and CompletedAt and Action.EndAt
// exist for the emitters that have to answer them differently. The clear
// case is this package's own standing convention, which Alert and
// RetentionHold both state: LevelError is reserved for the manager
// failing at something, and a check that correctly reports the far side
// as unreachable is the manager working exactly as designed. That line's
// outcome is an error and its level is a warning, and neither is wrong.
func (o Result) Level() Level {
	switch o {
	case ResultError:
		return LevelError
	case ResultWarn:
		return LevelWarn
	default:
		return LevelInfo
	}
}

// The action names this package brackets work under.
//
// Named constants for the same reason the Event* names are: an action
// name reaches the log line, the live feed and anything filtering either
// one, so it is an API surface rather than a string a refactor may
// reword. A caller outside this package passing its own name is fine and
// expected (Begin takes one); these are the ones obs itself emits.
const (
	// ActionCycle brackets one discovery-through-retention processing
	// cycle, which is CycleStart and CycleEnd's pair.
	ActionCycle = "cycle"
)

// The three attribute keys the record's own marks are written into the
// log line under.
//
// They are reserved: no event may log a field of its own under one of
// them. A duplicate key in the JSON object is worse than either answer,
// for the reason DiskPressure's doc records one field along, and worse
// here than there: the tap takes the first and encoding/json keeps the
// last, so a line whose event supplied its own "result" would tell the
// terminal one thing and the log another about how the very same
// operation went.
//
// "result" and not "outcome", and that is the whole reason this is not
// called an outcome on the wire. connection_test has carried a field
// called outcome since issue #596, and taking that name for the mark
// would have meant renaming a shipped field to make room: fields are not
// schema'd, nothing in tests/compat catches it, and a query matching
// event=connection_test AND outcome=failed goes silently empty rather
// than failing. The newcomer takes a free name and the incumbent keeps
// the one it shipped with. It also keeps the contract's four properties
// called outcome from becoming five with a fifth enum, two of which are
// already nested in one response.
//
// emit will not produce that object even if an event tries: a mark whose
// key an event has already claimed is not written a second time, and the
// event's own field stands, exactly as contextBackupSet leaves an event's
// own backup_set alone. See TestAMarkedLineNeverCarriesADuplicateKey.
const (
	fieldResult   = "result"
	fieldAction   = "action"
	fieldActionID = "action_id"
)

// mark is the set of record-level facts a line carries beside its
// severity: how the operation went, and which action it opens or closes.
//
// It is a struct rather than three parameters so emit's signature does
// not grow three arguments that are empty at almost every call site, and
// so the zero value means exactly what it should: an ordinary line
// stating no outcome and belonging to no bracketed action.
type mark struct {
	result   Result
	action   string
	actionID string
}

// attrs renders the mark as the attributes it contributes to the log
// line, in the order a reader wants them.
//
// Empty values contribute nothing rather than an empty string. An
// "outcome": "" in a log line is a claim that something stated an
// outcome, which is precisely what an ordinary note has not done, and a
// client reading it back cannot tell the two apart.
func (m mark) attrs() []slog.Attr {
	var out []slog.Attr
	if m.result != "" {
		out = append(out, slog.String(fieldResult, string(m.result)))
	}
	if m.action != "" {
		out = append(out, slog.String(fieldAction, m.action))
	}
	if m.actionID != "" {
		out = append(out, slog.String(fieldActionID, m.actionID))
	}
	return out
}

// Action is one operator-visible operation in flight: something that has
// announced itself and owes a completion.
//
// The handle exists so the two halves cannot drift apart. A caller that
// logged a start and then hand-wrote an end line would be free to spell
// the action differently, mint a different id, or never write the second
// line at all, which is the state this issue found the codebase in. Here
// the id is minted once, held, and reused, and the only way to close the
// action is a method that requires an outcome.
//
// Nil-receiver-safe on every method, exactly as *Logger is, so a
// component holding a Logger it was never given can still be written as
// begin-then-end rather than as a branch around whether logging is on.
type Action struct {
	logger  *Logger
	event   string
	name    string
	id      string
	started time.Time

	// attrs is what Begin was given, held so the completion carries it
	// too.
	//
	// This is not a convenience. service/liveactivity.go routes a record
	// into a per-set ring or the deployment ring by reading backup_set
	// off it, and it looks for unfinished actions PER BUCKET, so a start
	// naming a set whose completion names none is a start in one ring and
	// a completion in another: the completion clears nothing, and the
	// set's strip reports an action that announced itself and went quiet
	// for one that finished cleanly. Nothing at the call site shows that,
	// because it is two correct-looking calls and the second one is
	// Succeeded, which takes no attributes at all.
	//
	// So the scope an action was opened with is a property of the action
	// rather than something each completion has to remember to repeat.
	attrs []slog.Attr

	// closed is what makes a second completion a no-op rather than a
	// second verdict.
	//
	// `defer action.Failed(ctx, err, ...)` beside an explicit Succeeded on
	// the happy path is the obvious way to write a function that can leave
	// several ways, and without this it writes both: the feed then holds a
	// success and an error for one action id, and a terminal shows the
	// action failing after it succeeded. The first close is the one that
	// happened, and every later one is dropped.
	//
	// Atomic because an action can be closed from a different goroutine
	// than the one that opened it (a cancel path, a deferred close running
	// while a worker reports), and this type is otherwise safe to hand
	// around.
	closed atomic.Bool
}

// Begin logs the start of one operator-visible action and returns the
// handle that finishes it.
//
// event is the event name BOTH lines are emitted under, and that is the
// point rather than an omission. cycle_start and cycle_end are two names
// for one action's two ends and this file exists because pairing them was
// somebody's job to remember; a caller adopting Begin does not get to
// make that mistake, because the start and the completion of one action
// are told apart by whether they state an outcome, not by their names.
// (CycleStart and CycleEnd keep their two names, since those are a
// published contract, and gain the pairing underneath.)
//
// action is the stable name of what is being done ("cycle",
// "connection_test", "medium_verify"), which is what an operator reads in
// "this started and never finished".
// The attrs are carried onto the completion as well as onto this line.
// See the Action type's own field for why that is a correctness property
// rather than a convenience.
func (l *Logger) Begin(ctx context.Context, event, action, msg string, attrs ...slog.Attr) *Action {
	a := &Action{
		logger:  l,
		event:   event,
		name:    action,
		id:      newActionID(),
		started: time.Now(),
		attrs:   append([]slog.Attr(nil), attrs...),
	}
	l.emitMarked(ctx, LevelInfo, mark{action: action, actionID: a.id}, event, msg, attrs...)
	return a
}

// Completed logs one line that is a completion in its own right, with no
// start to pair it with.
//
// Some operator-visible work is over by the time anything in this process
// can report it. An /api/v1 request is the clearest case: the status code
// IS the outcome, and the moment the recorder is reached the request has
// already been served, so a start line emitted then would be a line
// claiming to announce something that had already happened. A pair would
// be a lie, and this is the honest half of one.
//
// It is deliberately not the default shape. Anything that takes real time
// (a cycle, a connection test, a retention apply) owes a start as well,
// because the whole reason the terminal exists is that an operator should
// not have to wonder whether a button did anything; use Begin for those.
func (l *Logger) Completed(ctx context.Context, result Result, event, msg string, attrs ...slog.Attr) {
	l.emitMarked(ctx, result.Level(), mark{result: result}, event, msg, attrs...)
}

// CompletedAt is Completed for an emitter whose own severity is not the
// one its outcome would pick. See Result.Level for when that is
// legitimate; reach for Completed unless it is.
func (l *Logger) CompletedAt(ctx context.Context, level Level, result Result, event, msg string, attrs ...slog.Attr) {
	l.emitMarked(ctx, level, mark{result: result}, event, msg, attrs...)
}

// End closes the action with the outcome it reached.
//
// The outcome is a required argument and not a defaulted one, because the
// whole failure this file addresses is a completion that did not say how
// it went. The level comes from the outcome (see Result.Level), and the
// wall-clock duration since Begin rides along as a field: "it finished"
// and "it took eleven minutes" are both things an operator reads off a
// completion, and the handle is already holding the only moment from
// which the second one can be worked out.
func (a *Action) End(ctx context.Context, result Result, msg string, attrs ...slog.Attr) {
	if a == nil {
		return
	}
	a.EndAt(ctx, result.Level(), result, msg, attrs...)
}

// EndAt is End for an action whose own severity is not the one its
// outcome would pick. See Result.Level for when that is legitimate.
func (a *Action) EndAt(ctx context.Context, level Level, result Result, msg string, attrs ...slog.Attr) {
	if a == nil {
		return
	}
	// The first close is the one that happened. See the closed field.
	if !a.closed.CompareAndSwap(false, true) {
		return
	}
	all := make([]slog.Attr, 0, len(attrs)+len(a.attrs)+1)
	all = append(all, slog.Duration("duration", time.Since(a.started)))
	all = append(all, attrs...)
	// What the start was given, and only where this completion did not
	// say the same thing itself. The completion is the authority on its
	// own fields, exactly as an event is against what With bound (see
	// emit), and a duplicate key is worse than either answer.
	for _, b := range a.attrs {
		if attrNamed(all, b.Key) {
			continue
		}
		all = append(all, b)
	}
	a.logger.emitMarked(ctx, level, mark{result: result, action: a.name, actionID: a.id}, a.event, msg, all...)
}

// Succeeded is End with ResultSuccess, which is the case worth having a
// name for: it is the one the codebase had no way to say at all.
func (a *Action) Succeeded(ctx context.Context, msg string, attrs ...slog.Attr) {
	a.End(ctx, ResultSuccess, msg, attrs...)
}

// Failed is End with ResultError and err attached under the same "error"
// key every other failure in this package uses.
//
// It is a method rather than a note in End's doc telling callers to do
// both because doing one without the other is the bug: an action that
// attaches its error and states no outcome is invisible to a client
// reading outcomes, and one that states an error with no error field
// says something went wrong and refuses to say what.
//
// Whatever produced err is responsible for not having built it out of a
// Secret's raw value, exactly as Error's own doc says: wrapping after the
// fact, here, is too late.
func (a *Action) Failed(ctx context.Context, err error, msg string, attrs ...slog.Attr) {
	if a == nil {
		return
	}
	all := make([]slog.Attr, 0, len(attrs)+1)
	if err != nil {
		all = append(all, slog.String("error", err.Error()))
	}
	all = append(all, attrs...)
	a.End(ctx, ResultError, msg, all...)
}

// ID is the id both of this action's lines carry, for a caller that wants
// to put it on something else as well (an operation record, a response
// header). Empty on a nil handle.
func (a *Action) ID() string {
	if a == nil {
		return ""
	}
	return a.id
}

// newActionID mints the name one action's two lines are paired by.
//
// Random rather than a counter or a clock, and for the same reason
// service.newLiveActivityEpoch is: the only property that matters is that
// two actions in flight at once never collide, a counter would restart
// with the process and start colliding with whatever a client still holds
// from before it, and a clock read twice inside one tick collides
// quietly. The clock is the fallback if the system source refuses, which
// keeps this consistent with the rest of the observability path: a line
// that cannot mint an id should still be logged, and the worst a repeated
// id costs is the pairing this exists for.
func newActionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 16)
}
