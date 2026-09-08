package obs

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// This file is the sink itself: the type other packages hold, and the one
// funnel every line this package writes goes through.
//
// Two things about it carry weight that the signatures do not show. A nil
// *Logger is silent on every method rather than a panic on one, and that is
// what let this package arrive without touching every construction site in
// the repository in the same change: a Deps struct that grew a *Logger
// field keeps working for callers that never set it, the way a nil map
// keeps working for a reader.
//
// And emit is the only place a string is actually written down. Redaction
// (#295) hooks in there and nowhere else, so a rule applied once covers
// every event events.go declares today and every one it declares later,
// without a single helper or caller needing to know redaction exists.

// fieldEvent is the attribute key every line this package emits carries.
// Its value is one of the Event* constants declared in events.go: the
// couple between "which method was called" and "what string shows up in
// the event field" is exactly the stability contract this package exists
// to hold onto (see events.go's package-level doc for why that string is
// never allowed to drift).
const fieldEvent = "event"

// Level is log/slog's own severity type, re-exported so a caller wiring up
// a Logger does not need its own "log/slog" import just to pick a
// verbosity for New.
type Level = slog.Level

// The four severities log/slog defines. Re-exported for the same reason as
// Level.
const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// Logger is the FR-23 sink: one small type other packages hold (typically
// as a field on their own Deps struct, alongside a Journal or a Transport)
// and call into, instead of each package building its own log lines by
// hand. Centralizing it here is what makes "every event has a stable name"
// and "secrets never render" properties of one package instead of
// properties every call site has to individually get right.
//
// A nil *Logger is a valid, silent no-op for every method below. That
// matters because nothing else in this repository has been wired up to
// hold a Logger yet (see this package's introduction in the pull request
// that added it): a struct that embeds a *Logger field left at its zero
// value must not need a special case to stay safe, it should simply not
// log anything, the same way a nil map is safe to read from.
type Logger struct {
	base *slog.Logger

	// redact, when non-nil, is run over msg and every string-valued attr
	// in emit before either reaches base. See WithRedaction and
	// Redactor's own doc (redact.go); a nil value here is "redact
	// nothing", exactly the default a Logger built by New starts at.
	redact *Redactor

	// sink, when non-nil, is handed a flattened copy of every event this
	// Logger emits, after redaction. See WithSink and Sink's own doc
	// (sink.go); a nil value is "nobody is following this stream", which
	// is what a Logger built by New starts at.
	sink Sink

	// bound is what With has attached, held as attrs rather than pushed
	// onto base, so emit is still the only place a string is written
	// down. That is what puts these through redaction and through the
	// duplicate-key rule with everything else: attaching them to base
	// instead would put them ahead of both, which is how a Logger built
	// by With(...).WithRedaction(...) used to ship an endpoint past the
	// redactor. See boundAttrs (sink.go).
	bound []slog.Attr
}

// New builds a Logger that writes newline-delimited JSON to w, one JSON
// object per event, at minLevel and above.
//
// JSON, not a human-readable line format, is the deliberate choice here:
// FR-23 asks for structured logs, and "structured" only pays off if
// whatever ends up reading these lines (a log aggregator, a jq pipeline, an
// on-call engineer grepping a container's stdout for an event field) can
// parse every line the same way, rather than needing a format-specific
// scanner for the common case and falling back to regex for the rest.
//
// w is typically os.Stdout in production, so the process's own supervisor
// (systemd, a container runtime) owns log rotation and shipping; this
// package has no opinion on either. A nil w is treated as io.Discard
// rather than panicking, matching this type's general policy of failing
// safe (silently) rather than loud when logging infrastructure itself is
// misconfigured: a backup manager should not go down because its own
// logger was constructed oddly.
func New(w io.Writer, minLevel Level) *Logger {
	if w == nil {
		w = io.Discard
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: minLevel})
	return &Logger{base: slog.New(h)}
}

// With returns a Logger that attaches args to every event it logs from
// here on, alongside whatever that event's own helper adds. args follows
// slog's own convention (alternating key/value pairs, or slog.Attr
// values); see log/slog's package doc for the exact rules.
//
// This exists for the common case of a caller that knows a piece of
// context for its whole lifetime (a backup_set id, a cycle id) and would
// otherwise have to thread it through every event call by hand. It is not
// itself a new event and does not appear in this package's event-name
// contract.
func (l *Logger) With(args ...any) *Logger {
	if l == nil || l.base == nil {
		return l
	}
	cp := *l
	cp.bound = append(append([]slog.Attr(nil), l.bound...), boundAttrs(args)...)
	return &cp
}

// WithRedaction returns a Logger that runs every event's message and every
// string-valued attribute through r before it reaches base, in addition to
// whatever With has already attached. r is typically built by
// obs.NewRedactor from the endpoints a deployment has opted into
// (config.Remote.Sensitive, issue #295); passing nil is how a caller turns
// redaction back off, not merely how one declines to turn it on, which
// matters for internal/app.New calling this again on every hot config
// reload with whatever the newly loaded configuration's own redactor is.
//
// Like With, this returns a new Logger rather than mutating l, and is
// nil-receiver-safe: WithRedaction on a nil *Logger returns nil, exactly
// as every other method on this type does (see the type's own doc for why
// that has to hold).
func (l *Logger) WithRedaction(r *Redactor) *Logger {
	if l == nil {
		return l
	}
	cp := *l
	cp.redact = r
	return &cp
}

// Event is the escape hatch for anything FR-23 asks for that does not
// already have a named helper below: it logs event at level with msg as
// the human-readable summary and attrs as its structured fields.
//
// Prefer a named helper (Startup, LifecycleTransition, Retry, ...) whenever
// one exists. Those exist specifically so the string identifying an event
// lives in exactly one place in this package, as a named constant a test
// can pin down, rather than as a string literal copy-pasted at every call
// site that happens to want it. Reach for Event directly only when no named
// helper fits; if that happens often for the same event, that is a sign
// this package is missing a helper, not that Event is the right permanent
// home for it.
func (l *Logger) Event(ctx context.Context, level Level, event, msg string, attrs ...slog.Attr) {
	l.emit(ctx, level, event, msg, attrs...)
}

// emit is every exported method's shared plumbing: it stamps the event
// name and hands everything to slog's attribute-based logging path
// (LogAttrs, not the reflect-based Log/Printf-style variadic path), which
// avoids the boxing that passing []any would otherwise cost on every single
// call.
//
// It is also issue #295's one seam for the log line: msg and every
// string-valued attr are run through l.redact.Filter here, and nowhere
// else, before either reaches base. Every named helper in events.go funnels
// through this one function, so a redaction rule applied here covers every
// event this package can emit, present or future, without any of those
// helpers, or their callers throughout this repository, needing to know
// redaction exists at all. Filter is nil-receiver-safe (redact.go), so this
// runs unconditionally rather than branching on whether l.redact is set:
// a deployment that never configured one pays a nil check per string attr,
// not a different code path.
// It is also issue #573's one seam for the in-process tap, for the same
// reason and in the same place: a Sink is handed the finished record here,
// after redaction, so the tap and the line are built from one set of
// bytes and every event events.go declares later is followed for free.
//
// It takes no marks of its own and delegates to emitMarked, so a caller
// that has nothing to say about how an operation went writes exactly what
// it always wrote.
func (l *Logger) emit(ctx context.Context, level Level, event, msg string, attrs ...slog.Attr) {
	l.emitMarked(ctx, level, mark{}, event, msg, attrs...)
}

// emitMarked is emit with the record-level marks issue #625 added: how
// the operation this line reports went, and which bracketed action it
// opens or closes (see action.go for what those mean and why they are
// not a fifth level).
//
// They go through here rather than being attached by each caller for the
// same reason redaction and the tap do: this is the one place a line is
// written down, so a mark applied here reaches the log line AND the tap
// from one list of attributes, and the two readers of one event cannot
// come to disagree about how it went. Nothing about an unmarked line
// changes, which is what lets every existing call site keep calling emit.
func (l *Logger) emitMarked(ctx context.Context, level Level, m mark, event, msg string, attrs ...slog.Attr) {
	if l == nil || l.base == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if extra, ok := contextBackupSet(ctx, attrs, l.bound); ok {
		attrs = append(attrs, extra)
	}
	marks := m.attrs()
	all := make([]slog.Attr, 0, len(attrs)+len(l.bound)+len(marks)+1)
	all = append(all, slog.String(fieldEvent, event))
	// The marks come before everything else, and only where neither the
	// event nor what With bound already claimed the key. Those three keys
	// are reserved (action.go) precisely so that never happens, and the
	// check is here anyway: a duplicate key is worse than either answer,
	// the tap takes the first and encoding/json keeps the last, and a
	// line disagreeing with itself about its own outcome is the one shape
	// this field must never take.
	added := 0
	for _, k := range marks {
		if attrNamed(attrs, k.Key) || attrNamed(l.bound, k.Key) {
			continue
		}
		all = append(all, k)
		added++
	}
	// What With bound comes next, and only where the event did not say
	// the same thing itself. The event is the authority on its own
	// fields, and a duplicate key is worse than either answer: the tap
	// takes the first and encoding/json keeps the last, so two readers of
	// one event would disagree.
	for _, b := range l.bound {
		if attrNamed(attrs, b.Key) {
			continue
		}
		all = append(all, redactAttr(l.redact, b))
	}
	for _, a := range attrs {
		all = append(all, redactAttr(l.redact, a))
	}
	msg = l.redact.Filter(msg)
	l.base.LogAttrs(ctx, level, msg, all...)
	// The event attr and whichever marks were actually added are sliced
	// off: the tap names all four in fields of its own, and repeating
	// them in the flat list would leave every reader to filter them back
	// out. Sliced by COUNT rather than dropped by key, so an event that
	// claimed one of those keys for a field of its own keeps that field
	// in the tap exactly as it keeps it in the line.
	l.tap(level, m, event, msg, all[1+added:])
}

// attrNamed reports whether attrs already carries key.
func attrNamed(attrs []slog.Attr, key string) bool {
	for _, a := range attrs {
		if a.Key == key {
			return true
		}
	}
	return false
}

// redactAttr runs a string-valued attr through r. Filter is
// nil-receiver-safe (redact.go), so a deployment that configured no
// redactor pays a nil check per string attr rather than taking a
// different code path.
func redactAttr(r *Redactor, a slog.Attr) slog.Attr {
	if a.Value.Kind() != slog.KindString {
		return a
	}
	return slog.String(a.Key, r.Filter(a.Value.String()))
}

// tap hands one already-redacted event to the Sink, if there is one. attrs
// excludes the event attribute emit prepends and the marks emitMarked
// added, because Record names all four in fields of its own and repeating
// them in the list would leave every reader to filter them back out. It
// already carries whatever With bound, redacted and de-duplicated with the
// rest, which is what keeps the tap and the log line built from one list
// rather than two.
func (l *Logger) tap(level Level, m mark, event, msg string, attrs []slog.Attr) {
	if l.sink == nil {
		return
	}
	fields := make([]Field, 0, len(attrs))
	for _, a := range attrs {
		fields = append(fields, Field{Key: a.Key, Value: a.Value.String()})
	}
	l.sink.RecordEvent(Record{
		At:       time.Now(),
		Level:    level,
		Event:    event,
		Message:  msg,
		Fields:   fields,
		Outcome:  m.outcome,
		Action:   m.action,
		ActionID: m.actionID,
	})
}
