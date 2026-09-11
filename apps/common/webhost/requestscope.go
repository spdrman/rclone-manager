package webhost

import (
	"context"
	"net/http"
	"time"
)

// The request-scoped facts every surface in this repository needs and
// none of them could previously agree on: one correlation id, and the
// moment the request arrived.
//
// Issue #730 is why they live together in one context value instead of
// being minted where they are used. The reported deployment produced a
// browser-side "TypeError: Failed to fetch", a proxy hop that had no
// record of the request, and a handler that answered 200 - three
// accounts of one request with nothing in common to join them by. An id
// minted inside the error writer (errors.go, before this) could only
// ever appear on a refusal, which is exactly the request that never
// reached a handler at all.
//
// One value rather than two keys is the whole trick for the timer. A
// reverse proxy's ErrorHandler is handed the OUTBOUND request and
// nothing else, so reporting how long the browser waited needs the clock
// to have started in the request's context; a dedicated
// context.WithValue for it is one allocation per request on a path that
// is overwhelmingly successful. Riding along in the value the
// correlation id already pays for makes it free, which is why
// serve/ui.go can keep reporting elapsed_ms at Warn on a default INFO
// deployment - the level an operator's fault actually occurred at -
// instead of only when diagnostics are on.

// CorrelationHeader is the response header every surface here sets, and
// the one ui/shared/src/api/client.ts reads off every response. Spelled
// once: two hops that disagree about its capitalisation still work over
// HTTP, but a grep for it stops finding one of them.
const CorrelationHeader = "X-Correlation-Id"

// ClientAttemptHeader is the browser's own per-attempt id
// (ui/shared/src/api/client.ts). It is the only identifier that exists
// for a request that got NO response at all: the browser has it, and if
// the request reached this process then this process has it too, so the
// browser's console line and this log can be joined even though there is
// no response to carry a correlation id back on.
const ClientAttemptHeader = "X-Client-Attempt-Id"

// maxDiagnosticToken bounds any peer-supplied identifier this process is
// willing to copy into a log line or a response header - the browser's
// attempt id and an upstream hop's correlation id both. The client sends
// 16 hex characters and this package mints 12; anything near this bound
// is already somebody else's idea, and an unbounded attacker-chosen
// string reaching a log field is how a log file becomes a place to hide
// things.
const maxDiagnosticToken = 64

// requestScope is what one request carries. Unexported, and reachable
// only through the three functions below, so the shape can change
// without being a published contract.
type requestScope struct {
	id    string
	start time.Time
}

// requestScopeKey is the context key. Unexported empty struct: nothing
// outside this package can collide with it or plant one.
type requestScopeKey struct{}

// RequestScope is the middleware that mints both, at the earliest point
// this process can: before authentication, before routing, and before
// any handler can decide not to.
//
// The correlation id goes onto the response IMMEDIATELY rather than at
// the moment something goes wrong, which is the fix for the half of
// #730 that made the report undiagnosable: a 200 whose body the browser
// could not read used to carry no id at all, so "the browser could not
// read this" and "here is what was sent" were two records with nothing
// in common. Setting a header costs one map entry and is not a
// diagnostic-only cost, so it is not behind the debug knob.
//
// # Honouring an id the peer already chose
//
// An inbound CorrelationHeader that survives validation is adopted
// rather than replaced. That is what makes the two containers of one
// deployment produce joinable lines: serve-ui mints an id, forwards it
// to the engine, and the engine's own log then names the same request by
// the same id instead of a second one nobody can match up.
//
// It is safe to adopt because of what the value is never used for. It
// reaches exactly two places - a log attribute and a response header -
// and is never a credential, a session key, a cache key or a lookup of
// any kind, so a client that chooses its own gets to name its own
// request in a log and nothing else. validToken is what keeps it to
// that: a bounded token of safe characters cannot inject a newline into
// a log line or a header into a response.
//
// # Nesting
//
// A request that already carries a scope passes straight through. The
// engine's composition wraps this around its whole handler (auth routes,
// the API router and the mux's own 404s alike) while NewRouter installs
// it for a provider that builds a route table directly, so both run for
// an ordinary /api/v1 request and exactly one of them decides. Minting
// twice would restart the clock a hop out is reading and give one
// request two names, which is the defect this whole middleware exists to
// remove.
func RequestScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(requestScopeKey{}) != nil {
			next.ServeHTTP(w, r)
			return
		}
		id := validToken(r.Header.Get(CorrelationHeader))
		if id == "" {
			id = correlationID()
		}
		w.Header().Set(CorrelationHeader, id)
		scope := &requestScope{id: id, start: time.Now()}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestScopeKey{}, scope)))
	})
}

// CorrelationIDFrom is the id the edge minted for this request, or "" for
// a request that never went through RequestScope (a handler driven
// directly by a test, say). Empty rather than a fresh id on purpose: an
// id nothing else in the process knows about is worse than none, because
// it looks like something an operator could search for.
func CorrelationIDFrom(ctx context.Context) string {
	scope, _ := ctx.Value(requestScopeKey{}).(*requestScope)
	if scope == nil {
		return ""
	}
	return scope.id
}

// RequestElapsed is how long ago this request entered the process, and
// whether that is known at all. The bool is the honest half: a caller
// that reported 0 for "no clock" would be claiming an instant answer for
// exactly the requests nobody timed.
func RequestElapsed(ctx context.Context) (time.Duration, bool) {
	scope, _ := ctx.Value(requestScopeKey{}).(*requestScope)
	if scope == nil {
		return 0, false
	}
	return time.Since(scope.start), true
}

// ClientAttemptIDOf reads the browser's own attempt id off a request,
// bounded and validated, or "" when it is absent or not a token this
// side is willing to write down.
func ClientAttemptIDOf(r *http.Request) string {
	return validToken(r.Header.Get(ClientAttemptHeader))
}

// validToken is the one rule for accepting a client-supplied diagnostic
// identifier: unreserved URL characters only, and short.
//
// Rejecting outright rather than sanitising is deliberate. A value that
// arrives with a space or a control character in it is not this client's
// id with a typo, it is somebody trying to put something in a log line,
// and half of it landing there is worse than none of it: an operator
// searching for what they were told to search for would find a
// truncated match and believe it.
func validToken(v string) string {
	if v == "" || len(v) > maxDiagnosticToken {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == '~':
		default:
			return ""
		}
	}
	return v
}
