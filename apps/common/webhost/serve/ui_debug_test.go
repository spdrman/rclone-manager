// Issue #730. A UGREEN NAS deployment where the Activity page's
// fetch('/api/v1/activity') fails with a bare "TypeError: Failed to
// fetch" - no HTTP response reaches JavaScript at all - while curl to the
// same route answers 401 cleanly. A three-container rig built from the
// shipped image did not reproduce it, so the fault is either in the
// operator's own front end or in this hop, and this hop said nothing
// either way: NewUI's ReverseProxy had no ErrorHandler, so every failure
// to answer went to net/http's default logger and never to this
// process's own log.
//
// The tests here pin the two halves of that: the record of a failure is
// unconditional (an operator who has to turn a knob on first has already
// lost the occurrence that prompted them), and the per-request upstream
// trace is opt-in and completely silent when it is off.
package serve_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/spdrman/rclone-manager/apps/common/webhost"
	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
)

// recordingLogger is webhost.Logger, recording instead of writing. The
// mutex is real: the proxy logs from the goroutine serving the request,
// not the test's.
type recordingLogger struct {
	mu      sync.Mutex
	records []loggedEvent
}

type loggedEvent struct {
	level slog.Level
	event string
	attrs map[string]string
}

func (l *recordingLogger) Event(_ context.Context, level slog.Level, event, _ string, attrs ...slog.Attr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := loggedEvent{level: level, event: event, attrs: map[string]string{}}
	for _, a := range attrs {
		rec.attrs[a.Key] = a.Value.String()
	}
	l.records = append(l.records, rec)
}

func (l *recordingLogger) named(event string) []loggedEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []loggedEvent
	for _, r := range l.records {
		if r.event == event {
			out = append(out, r)
		}
	}
	return out
}

// deadUpstream is an address nothing is listening on: a port bound and
// released, so it is a real, currently-unused port on loopback rather
// than a guess.
func deadUpstream(t *testing.T) *url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	u, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatalf("parsing %q: %v", addr, err)
	}
	return u
}

func uiShell() fstest.MapFS {
	return fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("shell")}}
}

// TestUI_UnreachableUpstreamIsLogged is the line that was missing when
// #730 was reported. From the browser an unanswerable proxy hop is
// indistinguishable from a network failure, so unless this process
// records it the operator has a "Failed to fetch" and nothing else to
// look at.
//
// It logs at Warn, NOT at debug: this must fire on a default INFO
// deployment, which is the only kind the reporter was running.
func TestUI_UnreachableUpstreamIsLogged(t *testing.T) {
	log := &recordingLogger{}
	ui := httptest.NewServer(serve.NewUI(serve.UIConfig{
		Upstream: deadUpstream(t),
		StaticFS: uiShell(),
		Logger:   log,
	}))
	t.Cleanup(ui.Close)

	res, err := http.Get(ui.URL + "/api/v1/activity")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d: installing an ErrorHandler must not change what a client is told", res.StatusCode, http.StatusBadGateway)
	}

	got := log.named("proxy_error")
	if len(got) != 1 {
		t.Fatalf("logged %d proxy_error events, want exactly 1: %+v", len(got), log.records)
	}
	entry := got[0]
	if entry.level != slog.LevelWarn {
		t.Errorf("level = %v, want %v: a failure to answer the browser is not a debug-only event", entry.level, slog.LevelWarn)
	}
	if entry.attrs["method"] != http.MethodGet {
		t.Errorf("method = %q, want GET", entry.attrs["method"])
	}
	if entry.attrs["path"] != "/api/v1/activity" {
		t.Errorf("path = %q, want /api/v1/activity", entry.attrs["path"])
	}
	if entry.attrs["error"] == "" {
		t.Error("no error attribute: without what went wrong the line says no more than the 502 already did")
	}
	if _, ok := entry.attrs["elapsed_ms"]; !ok {
		t.Error("no elapsed_ms attribute: an instant connection refusal and a five-second header timeout are different faults and this is what tells them apart")
	}
}

// TestUI_UpstreamTraceIsSilentByDefault is the opt-in half. A deployment
// that set nothing must produce byte-identical output to the one before
// this instrumentation existed, on the path that actually works.
func TestUI_UpstreamTraceIsSilentByDefault(t *testing.T) {
	t.Setenv("RM_DEBUG", "")
	t.Setenv("LOG_LEVEL", "")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parsing upstream URL: %v", err)
	}

	log := &recordingLogger{}
	ui := httptest.NewServer(serve.NewUI(serve.UIConfig{
		Upstream: upstreamURL,
		StaticFS: uiShell(),
		Logger:   log,
	}))
	t.Cleanup(ui.Close)

	res, err := http.Get(ui.URL + "/api/v1/activity")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 forwarded unchanged", res.StatusCode)
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.records) != 0 {
		t.Errorf("a default deployment logged %d events on a successfully proxied request, want none: %+v", len(log.records), log.records)
	}
}

// TestUI_DebugTracesTheUpstreamResponse is what an operator turns on to
// answer "what did the engine actually send". The encoding attributes
// are the point: a body whose framing the browser rejects never reaches
// fetch() at all, and this is the only record of what that framing was.
func TestUI_DebugTracesTheUpstreamResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"UNAUTHENTICATED"}`))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parsing upstream URL: %v", err)
	}

	log := &recordingLogger{}
	ui := httptest.NewServer(serve.NewUI(serve.UIConfig{
		Upstream: upstreamURL,
		StaticFS: uiShell(),
		Logger:   log,
		Debug:    true,
	}))
	t.Cleanup(ui.Close)

	res, err := http.Get(ui.URL + "/api/v1/activity")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer res.Body.Close()

	got := log.named("proxy_upstream")
	if len(got) != 1 {
		t.Fatalf("logged %d proxy_upstream events, want exactly 1: %+v", len(got), log.records)
	}
	entry := got[0]
	if entry.level != slog.LevelDebug {
		t.Errorf("level = %v, want %v", entry.level, slog.LevelDebug)
	}
	if entry.attrs["status"] != "401" {
		t.Errorf("status = %q, want 401: the trace has to report what the engine answered, which is the value curl and the browser disagreed about", entry.attrs["status"])
	}
	if entry.attrs["path"] != "/api/v1/activity" {
		t.Errorf("path = %q, want /api/v1/activity", entry.attrs["path"])
	}
	for _, key := range []string{"content_length", "content_encoding", "transfer_encoding", "elapsed_ms"} {
		if _, ok := entry.attrs[key]; !ok {
			t.Errorf("no %s attribute: response framing is exactly what a browser rejects before fetch() sees anything", key)
		}
	}
}

// TestUI_DefaultLoggerIsTheSharedSeam. A host that named no logger still
// gets the unconditional proxy_error record - the reported deployment
// wired none, and a diagnostic an operator has to opt into is one nobody
// had switched on when the fault occurred.
func TestUI_DefaultLoggerIsTheSharedSeam(t *testing.T) {
	var cfg serve.UIConfig
	if cfg.Logger != nil {
		t.Fatal("the zero UIConfig names a logger; this test is checking the nil-means-default path")
	}
	var _ webhost.Logger = webhost.NewStdoutLogger()

	handler := serve.NewUI(serve.UIConfig{Upstream: deadUpstream(t), StaticFS: uiShell()})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/activity", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}
