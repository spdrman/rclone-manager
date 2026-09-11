package webhost

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Issue #598. Thirty 500s that knew exactly what had gone wrong and told
// nobody.
//
// The reported symptom was on the browser's side, but the half that made
// it undiagnosable is here: every `writeError(w, 500, "INTERNAL", ...)` in
// this package bound `err`, tested it and dropped it, and there was
// nowhere to write it anyway, because `handlers` held no logger and
// NewRouter installed no logging middleware. So the frontend's own
// describeFailure was telling operators "its own log holds the detail,
// under this correlation id" about a log that held nothing, under any id,
// for any of those routes.
//
// The tests here pin the three things that makes true: a 500 logs, it logs
// the SAME correlation id the response carries (an id that matches nothing
// is worse than none), and no 500 site is left outside the helper that
// does it.

// captureLogger is the Logger seam, recording instead of writing. It is
// safe for concurrent use because chi hands each request its own goroutine
// and one test drives several.
type captureLogger struct {
	mu      sync.Mutex
	records []captured
}

type captured struct {
	level slog.Level
	event string
	msg   string
	attrs map[string]string
}

func (c *captureLogger) Event(_ context.Context, level slog.Level, event, msg string, attrs ...slog.Attr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := captured{level: level, event: event, msg: msg, attrs: map[string]string{}}
	for _, a := range attrs {
		rec.attrs[a.Key] = a.Value.String()
	}
	c.records = append(c.records, rec)
}

func (c *captureLogger) only(t *testing.T) captured {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) != 1 {
		t.Fatalf("logged %d events, want exactly 1: %+v", len(c.records), c.records)
	}
	return c.records[0]
}

func newLoggedRouter(t *testing.T) (readSurfaceRouter, *captureLogger) {
	t.Helper()
	backend := newSyncFakeBackend()
	log := &captureLogger{}
	return readSurfaceRouter{
		router: NewRouter(RouterConfig{
			Platform:      allowingPlatform("alice"),
			Backend:       backend,
			Gate:          alwaysPassGate{},
			Logger:        log,
			BinaryVersion: "test",
			Commit:        "test",
		}),
		backend: backend,
	}, log
}

// TestInternalError_LogsTheErrorItRefusedOver is the route the issue was
// reported against, and the one an operator opens when something is
// already wrong.
func TestInternalError_LogsTheErrorItRefusedOver(t *testing.T) {
	rt, log := newLoggedRouter(t)
	rt.backend.errOnActivity = errors.New("journal is unreadable: /var/lib/backup-manager/state.db")

	rec := rt.get(t, "/api/v1/activity")
	mustStatus(t, rec, http.StatusInternalServerError)

	entry := log.only(t)
	if entry.level != slog.LevelError {
		t.Errorf("level = %v, want %v", entry.level, slog.LevelError)
	}
	// The underlying error is the whole point: without it the log line
	// says no more than the response already did.
	if !strings.Contains(entry.attrs["error"], "journal is unreadable") {
		t.Errorf("the logged error does not carry what went wrong: %+v", entry.attrs)
	}
	if entry.attrs["route"] != "/api/v1/activity" {
		t.Errorf("route = %q, want /api/v1/activity", entry.attrs["route"])
	}
	if entry.attrs["code"] != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL", entry.attrs["code"])
	}
	if entry.attrs["status"] != "500" {
		t.Errorf("status = %q, want 500", entry.attrs["status"])
	}
}

// TestInternalError_LogsTheSameCorrelationIdTheResponseCarried is the
// clause the frontend's INTERNAL branch promises. An id an operator can
// quote is only worth anything if it matches something.
func TestInternalError_LogsTheSameCorrelationIdTheResponseCarried(t *testing.T) {
	rt, log := newLoggedRouter(t)
	rt.backend.errOnActivity = errors.New("boom")

	rec := rt.get(t, "/api/v1/activity")
	mustStatus(t, rec, http.StatusInternalServerError)

	served := rec.Header().Get("X-Correlation-Id")
	if served == "" {
		t.Fatal("the refusal carried no X-Correlation-Id, so there is nothing for a log line to match")
	}
	if logged := log.only(t).attrs["correlation_id"]; logged != served {
		t.Errorf("logged correlation id %q, response carried %q; an id that matches nothing sends whoever quotes it grepping for a string that was never written", logged, served)
	}
}

// TestInternalError_NeverEchoesTheUnderlyingErrorToTheClient keeps the two
// halves apart: the operator gets an id, the log gets the detail. A change
// that started putting err in the response would pass every assertion
// above and leak a filesystem path to an unauthenticated-adjacent surface.
func TestInternalError_NeverEchoesTheUnderlyingErrorToTheClient(t *testing.T) {
	rt, _ := newLoggedRouter(t)
	rt.backend.errOnActivity = errors.New("journal is unreadable: /var/lib/secret.db")

	rec := rt.get(t, "/api/v1/activity")
	mustStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), "/var/lib") {
		t.Errorf("the response echoed the underlying error: %s", rec.Body.String())
	}
}

// TestInternalError_AHostThatNamedNoLoggerStillLogs is the direction the
// default has to fall.
//
// A nil Logger could mean "silent", and that is the wrong way round: a
// provider wiring up NewRouter for the first time would get a route table
// whose 500s hand out correlation ids matching nothing, which is the
// defect this issue closes, reintroduced by omission. So nil means the
// package default (stdout, JSON, the shape core/internal/obs writes) and a
// host has to opt out on purpose.
func TestInternalError_AHostThatNamedNoLoggerStillLogs(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnActivity = errors.New("boom")

	// Answers, does not panic, and carries an id. What it writes goes to
	// this process's stdout, which is exactly what `docker logs` on the
	// engine container shows.
	rec := rt.get(t, "/api/v1/activity")
	mustStatus(t, rec, http.StatusInternalServerError)
	if rec.Header().Get("X-Correlation-Id") == "" {
		t.Error("the refusal carried no correlation id")
	}
}

// TestEveryInternalServerErrorGoesThroughTheLoggingHelper is the "all
// thirty sites" clause, held structurally rather than by anybody
// remembering.
//
// A diagnostic that exists on one route is one route's worth of luck: the
// next 500 an operator meets is not the one that was fixed. So a bare
// writeError with a 500 in it is a compile-clean way to reintroduce
// exactly the hole this issue closes, and this is what notices.
func TestEveryInternalServerErrorGoesThroughTheLoggingHelper(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading this package: %v", err)
	}

	var offenders []string
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		scanned++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			offenders = append(offenders, refusalsWithNoRecord(fset, fn)...)
		}
	}

	if scanned == 0 {
		t.Fatal("no source file was scanned, so this test would pass against a package that had reintroduced every bare 500")
	}
	if len(offenders) > 0 {
		t.Errorf("these 500s write a refusal and record nothing, so the correlation id they hand an operator matches no line anywhere:\n  %s\n\nUse h.internalError(w, r, code, message, err), which writes the same body and logs the error under the same id. A site that has to write its own body keeps the id writeError returns and calls h.logRefusal with it.",
			strings.Join(offenders, "\n  "))
	}
}

// refusalsWithNoRecord finds the 500s in one function that cannot possibly
// have been logged.
//
// Two shapes are legal and the difference between them is the whole rule.
// A `writeError(w, http.StatusInternalServerError, ...)` written as a bare
// statement THROWS AWAY the correlation id it just minted, so nothing
// downstream could quote it even if it wanted to; that is the shape all
// thirty sites had. A site that keeps the id (`id := writeError(...)`) can
// hand it to logRefusal, and this asks that the same function does.
func refusalsWithNoRecord(fset *token.FileSet, fn *ast.FuncDecl) []string {
	var discarded []string
	kept := 0
	records := false

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calls(call, "logRefusal") {
			records = true
		}
		stmt, ok := n.(*ast.ExprStmt)
		if !ok {
			return true
		}
		if call, ok := stmt.X.(*ast.CallExpr); ok && isInternalServerErrorWrite(call) {
			discarded = append(discarded, fset.Position(call.Pos()).String())
		}
		return true
	})

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, rhs := range assign.Rhs {
			if call, ok := rhs.(*ast.CallExpr); ok && isInternalServerErrorWrite(call) {
				kept++
			}
		}
		return true
	})

	if kept > 0 && !records {
		discarded = append(discarded, fset.Position(fn.Pos()).String()+" (keeps the id and never calls logRefusal with it)")
	}
	return discarded
}

// isInternalServerErrorWrite recognises writeError(w, 500, ...) by the
// constant, not by the number: http.StatusInternalServerError is how every
// site in this package spells it, and a literal 500 would be a style break
// a reviewer catches long before this test would.
func isInternalServerErrorWrite(call *ast.CallExpr) bool {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok || ident.Name != "writeError" || len(call.Args) < 2 {
		return false
	}
	sel, ok := call.Args[1].(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "StatusInternalServerError"
}

// calls reports whether call names `name`, as a bare function or as a
// method on anything (h.logRefusal).
func calls(call *ast.CallExpr, name string) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == name
	case *ast.SelectorExpr:
		return fn.Sel.Name == name
	}
	return false
}

// Issue #730's handler half. The reported deployment's Activity page
// gets "TypeError: Failed to fetch" - no HTTP response reaches
// JavaScript at all - while curl to the same route answers cleanly, and
// a rig built from the shipped image did not reproduce it. So the only
// remaining move is to record, on the real deployment, what this handler
// produced for the request the browser could not read.
//
// Both halves of that are tested: the record exists when an operator
// asks for it, and a deployment that asked for nothing behaves exactly
// as it did before.

// TestListActivity_ServesNoDebugRecordByDefault is the constraint the
// whole feature is worth nothing without. Diagnostics that cost a
// default deployment anything get turned off and are then unavailable
// when they are needed.
//
// The correlation id is deliberately NOT part of that claim anymore. It
// used to be a debug-only addition on a success, and this test used to
// require its absence; issue #730's review moved it to the edge, on
// every response, because the browser reads it off whatever response it
// got and an id that exists only while diagnostics are on is an id that
// never exists when the fault is first reported. What a default
// deployment must still not pay is the extra LINE.
func TestListActivity_ServesNoDebugRecordByDefault(t *testing.T) {
	t.Setenv("RM_DEBUG", "")
	t.Setenv("LOG_LEVEL", "")

	rt, log := newLoggedRouter(t)
	rec := rt.get(t, "/api/v1/activity")
	mustStatus(t, rec, http.StatusOK)

	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.records) != 0 {
		t.Errorf("a default deployment logged %d events serving the activity feed, want none: %+v", len(log.records), log.records)
	}
}

// TestListActivity_DebugRecordsWhatWasServedUnderAQuotableId is what an
// operator turns on. The correlation id is the load-bearing part: the
// browser-side debug log reads X-Correlation-Id off the response it got,
// so without one on the success path there is no way to join "the
// browser could not read this" to "here is what was sent".
func TestListActivity_DebugRecordsWhatWasServedUnderAQuotableId(t *testing.T) {
	t.Setenv("RM_DEBUG", "1")

	rt, log := newLoggedRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity?limit=25", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "nas.example")
	req.Header.Set(ClientAttemptHeader, "attempt0123456789")
	rec := httptest.NewRecorder()
	rt.router.ServeHTTP(rec, req)
	mustStatus(t, rec, http.StatusOK)

	entry := log.only(t)
	if entry.event != "activity_debug" {
		t.Fatalf("event = %q, want activity_debug", entry.event)
	}
	if entry.level != slog.LevelDebug {
		t.Errorf("level = %v, want %v", entry.level, slog.LevelDebug)
	}

	served := rec.Header().Get("X-Correlation-Id")
	if served == "" {
		t.Fatal("the debug response carried no X-Correlation-Id, so a browser-side record of a failed read matches no line here")
	}
	if entry.attrs["correlation_id"] != served {
		t.Errorf("logged correlation id %q, response carried %q", entry.attrs["correlation_id"], served)
	}
	if entry.attrs["limit"] != "25" {
		t.Errorf("limit = %q, want 25", entry.attrs["limit"])
	}
	// The EXACT size, not merely a non-zero one: this number used to come
	// from a second json.Marshal of the same value, and the whole reason
	// it now comes from a counting writer around the one real encode is
	// that a client's problem is the bytes that actually left this
	// process.
	if got, want := entry.attrs["bytes"], strconv.Itoa(rec.Body.Len()); got != want {
		t.Errorf("bytes = %q, want %q (the response body's real length)", got, want)
	}
	// The browser's own attempt id: the only identifier that still
	// exists for a request the browser got no response to at all.
	if got := entry.attrs["client_attempt_id"]; got != "attempt0123456789" {
		t.Errorf("client_attempt_id = %q, want the one the browser sent", got)
	}
	if _, ok := entry.attrs["event_count"]; !ok {
		t.Error("no event_count attribute")
	}
	// The forwarded headers are here because the operator's own front
	// end is the other live suspect: a proto or host that disagrees with
	// what the browser asked for means the hop in front rewrote it.
	for key, want := range map[string]string{
		"x_forwarded_for":   "203.0.113.7",
		"x_forwarded_proto": "https",
		"x_forwarded_host":  "nas.example",
	} {
		if entry.attrs[key] != want {
			t.Errorf("%s = %q, want %q", key, entry.attrs[key], want)
		}
	}
}
