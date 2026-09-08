package webhost

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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

// TestInternalError_ANilLoggerIsSilentAndNotAPanic keeps every existing
// caller of NewRouter working: a RouterConfig with no Logger is a host
// that has not wired one, not a crash on the first 500.
func TestInternalError_ANilLoggerIsSilentAndNotAPanic(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnActivity = errors.New("boom")

	rec := rt.get(t, "/api/v1/activity")
	mustStatus(t, rec, http.StatusInternalServerError)
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
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "writeError" {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Args[1].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "StatusInternalServerError" {
				return true
			}
			offenders = append(offenders, filepath.Base(name)+":"+
				fset.Position(call.Pos()).String())
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("no source file was scanned, so this test would pass against a package that had reintroduced every bare 500")
	}
	if len(offenders) > 0 {
		t.Errorf("these 500s write a refusal and log nothing, so the correlation id they hand an operator matches no line anywhere:\n  %s\n\nUse h.internalError(w, r, code, message, err) instead: it writes the same body and logs the error under the same id the response carries.",
			strings.Join(offenders, "\n  "))
	}
}
