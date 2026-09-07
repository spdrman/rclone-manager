package obs

import (
	"context"
	"log/slog"
	"time"
)

// The in-process tap on the event stream (issue #573).
//
// events.go already writes every FR-23 moment as a structured line, and
// that line is the contract. What it is not is readable by anything except
// whatever is shipping the process's stdout, so a browser asking "what is
// this backup set doing right now" had no way to find out: the events
// existed and nothing in this process could follow them.
//
// A Sink closes that without a second event vocabulary. It is handed the
// same record the log line is built from, at the same instant, after the
// same redaction, so a reader of the tap and a reader of the log can never
// disagree about what happened. Adding an event to events.go therefore
// costs nothing here: the tap sees it for free, which is the whole reason
// the seam is at emit rather than at each call site.
//
// # Why a Record rather than a slog.Handler
//
// A slog.Handler would also work and would be more general, and that is
// the problem: an implementation would then have to know slog's Value
// kinds, its group semantics and its enabled/clone protocol to read a
// field. Everything downstream of this tap wants a flat list of already
// rendered strings, because it is going to put them on a screen. So the
// record is flattened here, once, rather than by every reader.
//
// # What a Sink may not do
//
// It is called synchronously, on the goroutine that reached the event,
// while a backup cycle is running. So an implementation must not block,
// must not log through the same Logger (which would recurse), and must be
// safe for concurrent use. Nothing reads a return value, and there is
// none: observing the event stream may never change what the cycle does.

// Field is one attribute of a Record, already rendered and already
// redacted. Both halves are deliberate: a reader is putting this on a
// screen or into JSON, and asking each one to re-run redaction would be
// asking each one to remember to.
type Field struct {
	Key   string
	Value string
}

// Record is one emitted event, flattened.
//
// It carries what a line needs to be rendered and ordered and nothing
// else: when it happened, how severe its emitter said it was, which event
// it is, the human-readable message, and the event's own fields in the
// order they were logged.
type Record struct {
	At      time.Time
	Level   Level
	Event   string
	Message string
	Fields  []Field
}

// Sink receives every event a Logger emits.
//
// See this file's doc for the three rules an implementation has to hold
// to: do not block, do not log, be safe under concurrency.
//
// # What a Sink changed about who can read a log line
//
// There is a fourth thing to know, and it is about the caller rather than
// the implementation. Before this tap existed, an event's fields reached
// exactly one place: the process's own stdout, read by whoever can read
// the container's log. GET /api/v1/activity/live (issue #573) serves the
// same fields to any authenticated session, so anything logged is now on
// an API surface, and "it only goes in the log" has stopped being a
// reason a value is safe to put in a field. Redaction is applied on the
// way here for exactly that reason (emit, logger.go, including the
// attributes With bound), and a new field carrying a credential, a
// remote's address or a customer path is now a disclosure rather than a
// line in a file.
type Sink interface {
	RecordEvent(Record)
}

// WithSink returns a Logger that hands every event it emits to s in
// addition to writing it, exactly as WithRedaction returns one that
// filters. A nil s turns the tap off, which is how a caller declines one
// rather than merely how one is omitted, and matters for the same reason
// WithRedaction says so: a hot reload calls this again with whatever the
// new configuration decided.
//
// Nil-receiver-safe, like every other method on this type.
func (l *Logger) WithSink(s Sink) *Logger {
	if l == nil {
		return l
	}
	cp := *l
	cp.sink = s
	return &cp
}

type backupSetKey struct{}

// WithBackupSet returns a context that attributes every event emitted
// under it to the backup set id.
//
// It exists because most of the events a per-set view wants already name
// their set or their artifact, and a handful of the most interesting ones
// do not: an error from reconcile, discovery or the artifact listing is
// logged with an op and an error and nothing that says which set was being
// worked on. Those are exactly the lines an operator needs attributed,
// because they are the ones that explain why a set stopped.
//
// The context, rather than a set-scoped Logger, because internal/app
// already runs each backup set's pass on its own context and threads it
// everywhere; a scoped logger would have to be threaded through every
// function that currently reaches for the Service's own.
//
// An empty id declines to scope rather than scoping to "": a caller that
// does not know the set must not be able to erase one an outer scope
// already established.
func WithBackupSet(ctx context.Context, id string) context.Context {
	if ctx == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, backupSetKey{}, id)
}

// BackupSetFrom returns the backup set id WithBackupSet put on ctx, or ""
// when there is none. Exported for the same reason
// ProgressObserverFrom is: the package that installs a value has to be
// able to prove it installed one.
func BackupSetFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(backupSetKey{}).(string)
	return id
}

// fieldBackupSet is the attribute key an event's own backup set is logged
// under. Discovery, Retention, StaleBackup and Alert already set it
// themselves, which is why contextBackupSet below only ever adds one when
// the event did not.
const fieldBackupSet = "backup_set"

// contextBackupSet returns the attr the context's backup set should
// contribute, and whether there is one to contribute.
//
// It returns nothing when the event already named a set. An event that
// names its own is the authority on it: Discovery is called with the set
// it actually discovered, and a cycle-scoped context value overwriting
// that would turn a correct field into a wrong one. A duplicate key would
// be worse still, since encoding/json keeps the last of two and the
// severity-shadowing bug DiskPressure's own doc records is the same
// mistake one field along.
//
// "The event named a set" covers what With bound as well as what this
// call passed, and it did not used to. A Logger built by
// With("backup_set", ...) got a second copy appended from the context,
// and the two readers of one event then disagreed about which set it
// belonged to: the tap files a record under the first backup_set it sees
// and encoding/json keeps the last, so a line could be filed under one
// strip and printed under another.
func contextBackupSet(ctx context.Context, attrs ...[]slog.Attr) (slog.Attr, bool) {
	id := BackupSetFrom(ctx)
	if id == "" {
		return slog.Attr{}, false
	}
	for _, list := range attrs {
		for _, a := range list {
			if a.Key == fieldBackupSet {
				return slog.Attr{}, false
			}
		}
	}
	return slog.String(fieldBackupSet, id), true
}

// boundAttrs flattens the alternating-key/value and slog.Attr forms With
// accepts into the one form emit can carry.
//
// Holding them as attrs rather than as an already-rendered list is what
// lets emit put them through the same redaction, the same duplicate-key
// rule and the same flattening every other attribute goes through. They
// used to be rendered at With time, which put them ahead of redaction:
// a Logger built by With(...).WithRedaction(...) shipped an endpoint
// straight past the redactor, into the log line and into the tap.
//
// Malformed trailing arguments are dropped rather than rendered as slog's
// own !BADKEY: this list ends up on a screen, and a placeholder key is
// noise on it.
func boundAttrs(args []any) []slog.Attr {
	var out []slog.Attr
	for i := 0; i < len(args); {
		switch v := args[i].(type) {
		case slog.Attr:
			out = append(out, v)
			i++
		case string:
			if i+1 >= len(args) {
				return out
			}
			out = append(out, slog.Any(v, args[i+1]))
			i += 2
		default:
			return out
		}
	}
	return out
}
