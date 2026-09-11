package webhost

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #730's review, medium finding 3. The reported deployment
// produced three accounts of one request - a browser-side "Failed to
// fetch", a proxy hop, and a handler that answered 200 - and nothing
// they had in common. The correlation id existed only on refusals,
// minted inside the error writer, which is exactly the response the
// reported request never got.
//
// So the id moved to the edge, and these are the clauses that makes it
// worth anything: it is on EVERY response, there is exactly ONE per
// request, and a value a peer chose is either safe to write down or not
// used at all.

// TestEveryResponseCarriesACorrelationId walks one response of each kind
// this router produces. A header on the refusals alone is what the
// previous arrangement had.
func TestEveryResponseCarriesACorrelationId(t *testing.T) {
	t.Setenv("RM_DEBUG", "")
	t.Setenv("LOG_LEVEL", "")

	rt := newReadSurfaceRouter(t)

	for _, tc := range []struct {
		name   string
		target string
		status int
	}{
		{name: "a 200 on the route the issue was reported against", target: "/api/v1/activity", status: http.StatusOK},
		{name: "an unauthenticated liveness probe", target: "/health/live", status: http.StatusOK},
		{name: "a 404 for a route that does not exist", target: "/api/v1/nothing-here", status: http.StatusNotFound},
		{name: "a refusal that names a backup set nobody has", target: "/api/v1/backup-sets/nope/nope", status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rt.get(t, tc.target)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if got := rec.Header().Get(CorrelationHeader); got == "" {
				t.Errorf("%s carried no %s; a browser that cannot read this response has nothing to quote", tc.target, CorrelationHeader)
			}
		})
	}
}

// TestARefusalCarriesExactlyOneCorrelationId is the half that made the
// move a fix rather than a second id. The edge sets the header before any
// handler runs and writeError sets it again; two ids for one request is
// worse than one, because the operator quotes whichever they were shown
// and the log holds the other.
func TestARefusalCarriesExactlyOneCorrelationId(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.get(t, "/api/v1/backup-sets/nope/nope")

	values := rec.Header().Values(CorrelationHeader)
	if len(values) != 1 {
		t.Fatalf("%s = %v, want exactly one value", CorrelationHeader, values)
	}
}

// TestCorrelationIdAdoptsTheUpstreamHopsOwnId is what makes a
// two-container deployment diagnosable: serve-ui mints an id and
// forwards it, and the engine's line for the same request has to name
// the same id rather than a second one nobody can match up.
func TestCorrelationIdAdoptsTheUpstreamHopsOwnId(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity", nil)
	req.Header.Set(CorrelationHeader, "cid_from-the-ui-hop")
	rec := httptest.NewRecorder()
	rt.router.ServeHTTP(rec, req)

	if got := rec.Header().Get(CorrelationHeader); got != "cid_from-the-ui-hop" {
		t.Errorf("%s = %q, want the id the hop in front already used", CorrelationHeader, got)
	}
}

// TestCorrelationIdRefusesAnUnsafePeerValue is the bound on that trust.
// The value reaches a log line and a response header, so a peer that
// could put a newline or a kilobyte of text in it could put a line in
// the log or a header in the response.
func TestCorrelationIdRefusesAnUnsafePeerValue(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	for _, name := range []string{
		"has a space",
		"has\nа newline",
		"has:a:colon",
		strings.Repeat("x", maxDiagnosticToken+1),
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/activity", nil)
		// Set directly on the map: http.Header.Set would be free to
		// reject a control character before this code ever sees it, and
		// the point is what THIS code does with it.
		req.Header[CorrelationHeader] = []string{name}
		rec := httptest.NewRecorder()
		rt.router.ServeHTTP(rec, req)

		got := rec.Header().Get(CorrelationHeader)
		if got == "" {
			t.Errorf("a request carrying %q got no id at all; an unusable peer value means mint one, not go without", name)
		}
		if got == name {
			t.Errorf("%s = %q: a peer-chosen value that is not a bounded safe token was written straight back", CorrelationHeader, got)
		}
	}
}

// TestClientAttemptIdIsBoundedBeforeItIsWrittenDown is the same rule for
// the browser's own per-attempt id, which is the only identifier that
// survives a request that got NO response (client.ts mints it; there is
// no response to carry a correlation id back on).
func TestClientAttemptIdIsBoundedBeforeItIsWrittenDown(t *testing.T) {
	for _, tc := range []struct {
		sent string
		want string
	}{
		{sent: "a1b2c3d4e5f60718", want: "a1b2c3d4e5f60718"},
		{sent: "", want: ""},
		{sent: "not a token", want: ""},
		{sent: strings.Repeat("9", maxDiagnosticToken+1), want: ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/activity", nil)
		r.Header[ClientAttemptHeader] = []string{tc.sent}
		if got := ClientAttemptIDOf(r); got != tc.want {
			t.Errorf("ClientAttemptIDOf(%q) = %q, want %q", tc.sent, got, tc.want)
		}
	}
}

// TestRequestElapsedIsAbsentWithoutTheEdge keeps the clock honest. A
// handler reached outside the middleware (a test, a future route table
// built without it) has no start time, and reporting zero would claim an
// instant answer for exactly the requests nobody timed.
func TestRequestElapsedIsAbsentWithoutTheEdge(t *testing.T) {
	bare := httptest.NewRequest(http.MethodGet, "/api/v1/activity", nil)
	if _, ok := RequestElapsed(bare.Context()); ok {
		t.Error("a request that never went through RequestScope reported an elapsed time")
	}
	if id := CorrelationIDFrom(bare.Context()); id != "" {
		t.Errorf("CorrelationIDFrom = %q on a request with no scope, want empty", id)
	}

	var (
		sawID      string
		sawElapsed bool
	)
	RequestScope(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawID = CorrelationIDFrom(r.Context())
		_, sawElapsed = RequestElapsed(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), bare)

	if sawID == "" {
		t.Error("the middleware put no correlation id on the request context")
	}
	if !sawElapsed {
		t.Error("the middleware started no clock, so a proxy hop cannot report how long the browser waited")
	}
}
