// engine_test.go pins the behavioral contract issue #129 moves here from
// apps/generic/server (PR #119's own server_test.go): NewEngine's HTTP
// surface standing alone - a real *service.BackupService opened from a
// real temp config file, real local authentication, the real
// apps/common/webhost router, no static UI - proven from ITS NEW location
// under apps/common/webhost/serve, decoupled from any concrete provider.
//
// The one thing that changed on purpose, not just moved: this file never
// imports apps/generic/platform. testPlatformAdapter below builds a
// capabilities.PlatformAdapter using nothing but
// apps/common/auth/local + apps/common/platform/capabilities - both
// already siblings of this package under apps/common - which is the
// actual proof this composition no longer lives inside a specific
// provider's module (docs/EPIC-B-multi-nas.md §9.2, issue #129's own
// scope: "parameterized by PlatformAdapter/Authenticator/auth-routes-
// handler the way NewRouter already is").
package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
	"github.com/spdrman/rclone-manager/core/service"
)

// testPlatformAdapter is a minimal capabilities.PlatformAdapter built only
// from an apps/common/auth/local.Service - the same information
// apps/generic/platform.Adapter wraps, but constructed right here instead
// of importing that provider-specific package, which apps/common must
// never do (docs/EPIC-B-multi-nas.md §7.1 - the dependency direction is
// core -> nothing, apps/<provider> -> apps/common, never the reverse).
type testPlatformAdapter struct {
	capabilities.BasePlatformAdapter
	auth *local.Service
}

func (a testPlatformAdapter) ID() capabilities.PlatformID { return capabilities.PlatformGeneric }

func (a testPlatformAdapter) Capabilities() capabilities.PlatformCapabilities {
	return capabilities.PlatformCapabilities{}
}

func (a testPlatformAdapter) Authenticator() capabilities.Authenticator {
	return a.auth.Authenticator()
}

func (a testPlatformAdapter) PlatformInfo(_ context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: capabilities.PlatformGeneric, Name: "test"}, nil
}

// writeTestConfig mirrors core/cmd/backup-manager/main_test.go's own
// writeTestConfig: a minimal, valid config against real temp directories,
// needing no network and no Docker.
func writeTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "state.db")
	content := "poll_interval: 1h\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// engineHarness stands up NewEngine's own HTTP surface behind an
// httptest.Server, with a cookie-jar-carrying client so login/enroll
// establish a real, usable session exactly as a browser would observe.
type engineHarness struct {
	server *httptest.Server
	auth   *local.Service
	client *http.Client
}

func newEngineHarness(t *testing.T) *engineHarness {
	t.Helper()

	configPath := writeTestConfig(t)
	backend, cleanup, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("BackupService cleanup: %v", err)
		}
	})

	authSvc, err := local.New(local.Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	handler := serve.NewEngine(serve.EngineConfig{
		Platform:              testPlatformAdapter{auth: authSvc},
		AuthRoutes:            authSvc.Handler(),
		TrustForwardedHeaders: authSvc.TrustForwardedHeaders(),
		Backend:               backend,
		BinaryVersion:         "test",
		Commit:                "testcommit",
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	return &engineHarness{server: srv, auth: authSvc, client: &http.Client{Jar: jar}}
}

// bootstrapToken extracts the current single-use enrollment token via
// PrintBootstrapNotice - the same string a real deployment's container
// log would show, not a test-only shortcut.
func (h *engineHarness) bootstrapToken(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := h.auth.PrintBootstrapNotice(&buf, ""); err != nil {
		t.Fatalf("PrintBootstrapNotice: %v", err)
	}
	const marker = "token: "
	s := buf.String()
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("no bootstrap token in notice: %q", s)
	}
	fields := strings.Fields(s[i+len(marker):])
	if len(fields) == 0 {
		t.Fatalf("could not parse token out of notice: %q", s)
	}
	return fields[0]
}

// csrfToken seeds the client's cookie jar (a harmless GET against base,
// exactly as a browser's first page load would) and returns the CSRF
// token it picked up.
func csrfToken(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("seed GET /: %v", err)
	}
	resp.Body.Close()

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == local.CSRFCookieName {
			return c.Value
		}
	}
	t.Fatal("no CSRF cookie present after seeding GET /")
	return ""
}

// enrollAndLogIn drives the real POST .../auth/enroll route, against base
// (either the engine directly or the UI host's proxy - both must behave
// identically), leaving client holding a real, live session cookie
// afterward.
func enrollAndLogIn(t *testing.T, h *engineHarness, client *http.Client, base string) {
	t.Helper()
	csrf := csrfToken(t, client, base)
	token := h.bootstrapToken(t)

	body, _ := json.Marshal(map[string]string{"username": "bm-admin", "password": "correct-horse-battery"})
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/enroll", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(local.CSRFHeaderName, csrf)
	req.Header.Set(local.BootstrapTokenHeader, token)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/auth/enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("enroll status = %d, want %d; body=%s", resp.StatusCode, http.StatusNoContent, b)
	}
}

// TestEngine_UnauthenticatedDestructiveRequestIsRefused proves the
// destructive POST /api/v1/operations route is unreachable without
// authentication, from NewEngine's new home under apps/common/webhost/serve.
func TestEngine_UnauthenticatedDestructiveRequestIsRefused(t *testing.T) {
	h := newEngineHarness(t)

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/operations", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Idempotency-Key", "test-key-1")
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/operations: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST /api/v1/operations status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestEngine_AuthenticatedRequestSucceedsAgainstSystemVersion proves real
// local credentials, established through the real enroll/login HTTP flow,
// are enough to reach apps/common/webhost's own /api/v1/system/version
// route directly against the engine - the local-auth Authenticator this
// test's own testPlatformAdapter wires (not apps/generic/platform's) and
// apps/common/webhost's pre-existing authMiddleware actually agree with
// each other end to end.
func TestEngine_AuthenticatedRequestSucceedsAgainstSystemVersion(t *testing.T) {
	h := newEngineHarness(t)
	enrollAndLogIn(t, h, h.client, h.server.URL)

	resp, err := h.client.Get(h.server.URL + "/api/v1/system/version")
	if err != nil {
		t.Fatalf("GET /api/v1/system/version: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("authenticated GET /api/v1/system/version status = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, b)
	}
}

// TestEngine_ServesNoStaticUI proves the engine's own handler is API-only
// (the two-container split's whole point): a browser-shaped GET for a
// non-API route must NOT get a static shell back from NewEngine - that is
// NewUI's job.
func TestEngine_ServesNoStaticUI(t *testing.T) {
	h := newEngineHarness(t)

	resp, err := h.client.Get(h.server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Errorf("GET / on the engine directly status = %d, want NOT 200 (the engine must not serve the static UI)", resp.StatusCode)
	}
}

// TestEngine_NoAuthRoutesMeansNoAuthEndpoint proves AuthRoutes is truly
// optional: a provider with a native session Authenticator and no
// login/enroll/logout HTTP surface of its own gets no /api/v1/auth/*
// route registered at all, rather than NewEngine panicking on a nil
// http.Handler.
func TestEngine_NoAuthRoutesMeansNoAuthEndpoint(t *testing.T) {
	backend, cleanup, err := service.Open(context.Background(), writeTestConfig(t))
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	authSvc, err := local.New(local.Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	handler := serve.NewEngine(serve.EngineConfig{
		Platform:      testPlatformAdapter{auth: authSvc},
		Backend:       backend,
		BinaryVersion: "test",
		Commit:        "testcommit",
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/auth/login")
	if err != nil {
		t.Fatalf("GET /api/v1/auth/login: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/v1/auth/login with AuthRoutes unset status = %d, want %d (no route registered)", resp.StatusCode, http.StatusNotFound)
	}
}

// uiHarness wraps an engineHarness with a real NewUI httptest.Server
// proxying to it, modelling the real two-container topology: engine has
// no published port and the UI host proxies through.
type uiHarness struct {
	*engineHarness
	ui     *httptest.Server
	client *http.Client
}

func newUIHarness(t *testing.T) *uiHarness {
	t.Helper()
	engine := newEngineHarness(t)

	upstream, err := url.Parse(engine.server.URL)
	if err != nil {
		t.Fatalf("url.Parse(engine.server.URL): %v", err)
	}

	staticFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html><body>generic backup-manager UI shell</body></html>")},
	}

	ui := httptest.NewServer(serve.NewUI(serve.UIConfig{Upstream: upstream, StaticFS: staticFS}))
	t.Cleanup(ui.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	return &uiHarness{engineHarness: engine, ui: ui, client: &http.Client{Jar: jar}}
}

// TestUI_ProxiesAuthenticatedRequestsToTheEngine is this split's central
// end-to-end proof, exercised from NewUI/NewEngine's new shared home:
// enroll and log in THROUGH the UI host's reverse proxy, then confirm an
// authenticated GET /api/v1/system/version, also through the proxy,
// reaches the real engine and succeeds.
func TestUI_ProxiesAuthenticatedRequestsToTheEngine(t *testing.T) {
	h := newUIHarness(t)
	enrollAndLogIn(t, h.engineHarness, h.client, h.ui.URL)

	resp, err := h.client.Get(h.ui.URL + "/api/v1/system/version")
	if err != nil {
		t.Fatalf("GET /api/v1/system/version (via UI proxy): %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /api/v1/system/version via UI proxy status = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, body)
	}
	if !strings.Contains(string(body), "api_version") {
		t.Errorf("proxied response body = %q, want the engine's real /api/v1/system/version JSON", body)
	}
}

// TestUI_ProxiedDestructiveRequestIsRefusedWithoutAuthentication proves
// the same authentication boundary holds when reached through the UI
// host's proxy, not just directly against the engine.
func TestUI_ProxiedDestructiveRequestIsRefusedWithoutAuthentication(t *testing.T) {
	h := newUIHarness(t)

	req, err := http.NewRequest(http.MethodPost, h.ui.URL+"/api/v1/operations", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Idempotency-Key", "test-key-1")
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/operations (via UI proxy): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST /api/v1/operations via UI proxy status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestUI_StaticUIServedForNonAPIRoute proves the UI host's static handler
// falls back to index.html for a client-side route, and that the reverse
// proxy never intercepts a route it doesn't own.
func TestUI_StaticUIServedForNonAPIRoute(t *testing.T) {
	h := newUIHarness(t)

	for _, path := range []string{"/", "/sets/some-backup-set", "/settings"} {
		resp, err := h.client.Get(h.ui.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusOK)
		}
		if !strings.Contains(string(body), "generic backup-manager UI shell") {
			t.Errorf("GET %s body = %q, want it to contain the static index.html content (SPA fallback)", path, body)
		}
	}
}

// fakeUpstream stands up a minimal httptest.Server for testing NewUI's
// reverse-proxy behavior in isolation from a real engine.
func fakeUpstream(t *testing.T, handler http.HandlerFunc) *url.URL {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", srv.URL, err)
	}
	return u
}

func newUIProxyingTo(t *testing.T, upstream *url.URL, timeout time.Duration) *httptest.Server {
	t.Helper()
	staticFS := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("shell")}}
	ui := httptest.NewServer(serve.NewUI(serve.UIConfig{
		Upstream:                   upstream,
		StaticFS:                   staticFS,
		ProxyResponseHeaderTimeout: timeout,
	}))
	t.Cleanup(ui.Close)
	return ui
}

// TestUI_DiscardsAClientSuppliedXForwardedForHeader is issue #119's
// review's central regression test for the anti-spoofing half of the
// rate-limit-collapse fix, re-proven from NewUI's new location.
func TestUI_DiscardsAClientSuppliedXForwardedForHeader(t *testing.T) {
	var gotForwardedFor string
	upstream := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	})
	ui := newUIProxyingTo(t, upstream, 0)

	req, err := http.NewRequest(http.MethodGet, ui.URL+"/api/v1/system/version", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Forwarded-For", "6.6.6.6")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET via UI proxy: %v", err)
	}
	resp.Body.Close()

	if strings.Contains(gotForwardedFor, "6.6.6.6") {
		t.Errorf("upstream received X-Forwarded-For = %q, want it to NOT contain the client-forged value %q", gotForwardedFor, "6.6.6.6")
	}
	if gotForwardedFor == "" {
		t.Error("upstream received an empty X-Forwarded-For, want the proxy's own observed client address")
	}
}

// TestUI_SetsXForwardedProtoFromItsOwnRealConnection proves the proxy
// sets X-Forwarded-Proto from the connection it ACTUALLY has with the
// client, re-proven from NewUI's new location.
func TestUI_SetsXForwardedProtoFromItsOwnRealConnection(t *testing.T) {
	var gotProto string
	upstream := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.WriteHeader(http.StatusOK)
	})
	ui := newUIProxyingTo(t, upstream, 0)

	req, err := http.NewRequest(http.MethodGet, ui.URL+"/api/v1/system/version", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Forwarded-Proto", "https")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET via UI proxy: %v", err)
	}
	resp.Body.Close()

	if gotProto != "http" {
		t.Errorf("upstream received X-Forwarded-Proto = %q, want %q", gotProto, "http")
	}
}

// TestUI_ProxyTimesOutAgainstAHungUpstream is issue #119's review's
// empirically-demonstrated finding 3, re-proven from NewUI's new
// location: UIConfig.ProxyResponseHeaderTimeout actually bounds the wait
// for a connection that accepts but never responds.
func TestUI_ProxyTimesOutAgainstAHungUpstream(t *testing.T) {
	release := make(chan struct{})
	upstream := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	t.Cleanup(func() { close(release) })
	ui := newUIProxyingTo(t, upstream, 50*time.Millisecond)

	start := time.Now()
	resp, err := http.Get(ui.URL + "/api/v1/system/version")
	if err != nil {
		t.Fatalf("GET via UI proxy: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	if elapsed > 2*time.Second {
		t.Errorf("request took %s to fail, want it bounded by the configured 50ms ResponseHeaderTimeout", elapsed)
	}
}
