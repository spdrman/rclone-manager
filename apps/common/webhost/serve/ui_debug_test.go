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
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/backupdproject/backupd/apps/common/webhost"
	"github.com/backupdproject/backupd/apps/common/webhost/serve"
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
	t.Setenv("BACKUPD_DEBUG", "")
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
	// Read it to the end: the header-phase event below is emitted before
	// the body moves, but the completion event this test also covers can
	// only exist once the body has been copied and closed.
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatalf("reading the proxied body: %v", err)
	}
	res.Body.Close()

	got := log.named("proxy_upstream_headers")
	if len(got) != 1 {
		t.Fatalf("logged %d proxy_upstream_headers events, want exactly 1: %+v", len(got), log.records)
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
	// The completion half, on a body that arrived whole: declared and
	// observed agree, the read ended at EOF, and nothing failed. This is
	// the baseline the truncated case below is only meaningful against.
	done := log.named("proxy_upstream_complete")
	if len(done) != 1 {
		t.Fatalf("logged %d proxy_upstream_complete events, want exactly 1: %+v", len(done), log.records)
	}
	body := done[0]
	if body.attrs["bytes_read"] != body.attrs["content_length"] {
		t.Errorf("bytes_read = %q for a declared content_length of %q; a clean transfer must report them equal",
			body.attrs["bytes_read"], body.attrs["content_length"])
	}
	if body.attrs["eof"] != "true" {
		t.Errorf("eof = %q on a body that arrived whole, want true", body.attrs["eof"])
	}
	if body.attrs["read_error"] != "" {
		t.Errorf("read_error = %q on a clean transfer, want empty", body.attrs["read_error"])
	}
	if body.level != slog.LevelDebug {
		t.Errorf("level = %v on a clean transfer, want %v", body.level, slog.LevelDebug)
	}
}

// TestUI_DebugReportsABodyThatDidNotArriveWhole is the finding this
// event exists for, and the shape #730 looks like from JavaScript.
//
// The upstream declares a length and then sends less of the body than it
// promised. Every earlier line in this process still reads as a success:
// the round trip returned, the status was 200, the declared framing was
// well formed. The browser's HTTP stack refuses the response outright
// and fetch() rejects with a bare TypeError, and without this event
// nothing in this container ever recorded that the transfer came apart.
func TestUI_DebugReportsABodyThatDidNotArriveWhole(t *testing.T) {
	const declared = 4096
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Content-Length set by hand, then a short write: net/http's own
		// server notices, stops writing and drops the connection, which
		// is exactly what a proxy or a kernel dropping a transfer mid
		// body looks like from this side.
		w.Header().Set("Content-Length", strconv.Itoa(declared))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[`))
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

	// The client's own outcome is deliberately not asserted, because
	// this is the shape that has no single one: the transfer comes apart
	// mid body, so depending on timing a caller either gets a response
	// whose read then fails or no response at all. That is precisely the
	// reported symptom ("TypeError: Failed to fetch" against a route
	// curl reads), and it is the reason the record below has to exist
	// here rather than being inferred from what the caller saw.
	if res, err := http.Get(ui.URL + "/api/v1/activity"); err == nil {
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()
	}

	var done []loggedEvent
	// The body is copied and closed on the proxy's own goroutine, which
	// can outlive the client's read by a moment.
	for i := 0; i < 100; i++ {
		if done = log.named("proxy_upstream_complete"); len(done) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(done) != 1 {
		t.Fatalf("logged %d proxy_upstream_complete events, want exactly 1: %+v", len(done), log.records)
	}
	entry := done[0]

	if entry.attrs["content_length"] != strconv.Itoa(declared) {
		t.Errorf("content_length = %q, want the declared %d", entry.attrs["content_length"], declared)
	}
	read, err := strconv.ParseInt(entry.attrs["bytes_read"], 10, 64)
	if err != nil {
		t.Fatalf("bytes_read = %q, which is not a number", entry.attrs["bytes_read"])
	}
	if read <= 0 || read >= declared {
		t.Errorf("bytes_read = %d, want more than nothing and less than the declared %d: a truncation this event cannot see is a truncation nobody can see", read, declared)
	}
	// An operator reading a default-level log has to be able to find
	// this without also reading every debug line around it: the transfer
	// coming apart is the fault, not a detail of one.
	if entry.level != slog.LevelWarn {
		t.Errorf("level = %v for a body that did not arrive whole, want %v", entry.level, slog.LevelWarn)
	}
}

// TestUI_CorrelatesTheRequestAcrossBothHops is issue #730's review,
// medium finding 3. Three accounts of one request - the browser's, this
// hop's and the engine's - are only worth anything if they name it the
// same way.
func TestUI_CorrelatesTheRequestAcrossBothHops(t *testing.T) {
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(webhost.CorrelationHeader)
		// The engine echoes the id it was handed, exactly as
		// webhost.RequestScope does on the other side of this proxy.
		w.Header().Set(webhost.CorrelationHeader, r.Header.Get(webhost.CorrelationHeader))
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
		Debug:    true,
	}))
	t.Cleanup(ui.Close)

	req, err := http.NewRequest(http.MethodGet, ui.URL+"/api/v1/activity", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set(webhost.ClientAttemptHeader, "attempt-0123456789")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	_, _ = io.ReadAll(res.Body)
	res.Body.Close()

	served := res.Header.Values(webhost.CorrelationHeader)
	if len(served) != 1 || served[0] == "" {
		t.Fatalf("%s = %v, want exactly one non-empty value: the engine echoes the id this hop forwarded, and both copies reaching the browser is a response with two answers to one question",
			webhost.CorrelationHeader, served)
	}

	forwarded := <-seen
	if forwarded != served[0] {
		t.Errorf("forwarded %q upstream but answered the browser with %q; the two hops' log lines cannot be joined", forwarded, served[0])
	}

	traced := log.named("proxy_upstream_headers")
	if len(traced) != 1 {
		t.Fatalf("logged %d proxy_upstream_headers events, want exactly 1: %+v", len(traced), log.records)
	}
	if traced[0].attrs["correlation_id"] != served[0] {
		t.Errorf("the trace names correlation_id %q, the response carried %q", traced[0].attrs["correlation_id"], served[0])
	}
	// The browser's own attempt id, which is the only identifier that
	// survives a request that gets no response at all.
	if traced[0].attrs["client_attempt_id"] != "attempt-0123456789" {
		t.Errorf("client_attempt_id = %q, want the one the browser sent", traced[0].attrs["client_attempt_id"])
	}
}

// TestUI_StaticResponsesCarryACorrelationIdToo. The app shell is served
// by this container itself, and a browser that got HTML where it
// expected JSON is one of #730's live hypotheses - so that response is
// exactly one an operator needs to be able to name.
func TestUI_StaticResponsesCarryACorrelationId(t *testing.T) {
	handler := serve.NewUI(serve.UIConfig{Upstream: deadUpstream(t), StaticFS: uiShell()})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sets/abc", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the app shell)", rec.Code)
	}
	if got := rec.Header().Get(webhost.CorrelationHeader); got == "" {
		t.Errorf("the app shell carried no %s", webhost.CorrelationHeader)
	}
}

// TestUI_UnreachableUpstreamStillReportsHowLongTheBrowserWaited is the
// clause the off-path cost review could have cost. The start time moved
// out of this file's own per-request context value and into the one the
// edge middleware already allocates, and the whole point of that value
// was telling "the engine refused instantly" apart from "we sat on
// ResponseHeaderTimeout" - at DEFAULT level, since a browser that gets
// no answer is not a debug-only event.
func TestUI_UnreachableUpstreamStillReportsHowLongTheBrowserWaited(t *testing.T) {
	t.Setenv("BACKUPD_DEBUG", "")
	t.Setenv("RM_DEBUG", "")
	t.Setenv("LOG_LEVEL", "")

	log := &recordingLogger{}
	handler := serve.NewUI(serve.UIConfig{Upstream: deadUpstream(t), StaticFS: uiShell(), Logger: log})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/activity", nil))

	got := log.named("proxy_error")
	if len(got) != 1 {
		t.Fatalf("logged %d proxy_error events, want exactly 1: %+v", len(got), log.records)
	}
	if _, ok := got[0].attrs["elapsed_ms"]; !ok {
		t.Error("no elapsed_ms on a default deployment's proxy_error: how long the browser waited is the fact that separates a refused connection from a timeout")
	}
	if got[0].attrs["correlation_id"] == "" {
		t.Error("no correlation_id on proxy_error: the id the browser was handed is what joins its report to this line")
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
