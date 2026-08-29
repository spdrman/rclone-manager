// Package obs is the FR-23 structured-observability layer.
//
// Nothing else in this repository logs anything yet. This package exists
// so that when the rest of the manager starts logging, it does so through
// one small, shared surface with a settled event vocabulary and a
// structural guard against leaking secrets, rather than as scattered
// log.Printf calls that each package invents its own field names and its
// own (missing) secret handling for.
//
// # Shape
//
// Logger (logger.go) wraps the standard library's log/slog and writes
// newline-delimited JSON. There is no third-party logging dependency here,
// deliberately: log/slog already covers everything this package needs
// (leveled, structured, context-aware logging with a JSON handler built
// in), and this project's own conventions call for reaching for the
// standard library over a framework whenever it suffices.
//
// A caller that owns a long-lived component (the daemon loop, a lifecycle
// step's Deps, a discovery pass) holds a *Logger the same way it already
// holds a Journal or a Transport, and calls one of the typed event methods
// in events.go at the appropriate point. A nil *Logger is a safe no-op
// everywhere, so adding a Logger field to an existing Deps struct is
// backward compatible: an existing caller that never sets it keeps
// building and keeps working, just without logs, until it's updated to
// pass one.
//
// # The event catalog is the contract
//
// events.go declares one exported Event* constant and one typed method per
// FR-23 bullet. The constant's string value, not the Go method name, is
// this package's real API surface: it is what ends up in a log line's
// "event" field, and it is what anything downstream (a dashboard query, an
// alert rule, a jq filter, a future FR-24 health endpoint reading these
// same logs) actually depends on. Treat renaming one exactly like renaming
// a database column or a wire-protocol field. events_test.go pins every
// value down for this reason.
//
// # Secrets
//
// FR-23 requires that secrets never be logged. This package treats that as
// a structural property of a type, not a reviewer's checklist item: Secret
// (secret.go) wraps a sensitive string (an SSH key path, a known_hosts
// path, anything from internal/config's Remote that may carry credentials)
// and renders as a fixed placeholder through every rendering path Go's
// standard library offers a value: fmt's Stringer, Formatter and
// GoStringer interfaces (Formatter alone already forecloses every fmt verb,
// including %#v and %+v), encoding.TextMarshaler, json.Marshaler, and
// log/slog's own LogValuer hook. The wrapped value comes back out only
// through Reveal, a name chosen to be easy to grep for and to look
// deliberate in a diff. secret_test.go proves this holds by actually
// formatting, marshaling and logging a wrapped value and asserting the raw
// bytes never appear anywhere in the output, for every one of those paths.
//
// This package cannot force every future caller to wrap every sensitive
// field; that work happens where each field is defined (for example,
// internal/config would need its own change to expose Remote.KeyFile and
// Remote.KnownHosts as obs.Secret, which is outside this package's own
// scope). What this package guarantees is that once a value is wrapped,
// accidentally printing it (the %+v reflex, a debug dump of a config
// struct, handing it straight to slog.Any) cannot recover the plaintext.
// Logging a secret becomes a deliberate act (calling Reveal and then
// choosing to print the result) instead of a one-line accident.
package obs
