// server_test.go proves both halves of the two-container split
// (project-owner requirement folded into issue #82/B4.1 before merge):
// NewEngine's HTTP surface standing alone (a real *service.BackupService
// opened from a real temp config file, real local authentication, the
// real apps/common/webhost router - no static UI), and NewUI's reverse
// proxy actually forwarding a real request through to a real engine
// httptest.Server, end to end, the same shape the two real containers
// have in production (engine has no published port; the UI host is the
// only thing a browser ever talks to directly).
package server_test

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

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/generic/server"
	"github.com/spdrman/rclone-manager/core/service"
)

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

	handler := server.NewEngine(server.EngineConfig{
		Backend:       backend,
		Auth:          authSvc,
		BinaryVersion: "test",
		Commit:        "testcommit",
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

// enrollAndLogIn drives the real POST .../auth/enroll route, against
// base (either the engine directly or the UI host's proxy - both must
// behave identically), leaving client holding a real, live session
// cookie afterward.
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
// authentication - exactly the "gate it behind local authentication"
// half of this issue's scope, exercised against the real engine handler
// directly (see TestUI_ProxiedDestructiveRequestIsRefusedWithoutAuth for
// the same proof through the UI host's reverse proxy).
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

// TestEngine_AuthenticatedRequestSucceedsAgainstSystemVersion proves the
// other half: real local credentials, established through the real
// enroll/login HTTP flow, are enough to reach apps/common/webhost's own
// /api/v1/system/version route directly against the engine - the
// local-auth Authenticator this issue wires and apps/common/webhost's
// pre-existing authMiddleware actually agree with each other end to end.
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

// TestEngine_ServesNoStaticUI proves the engine's own handler is
// API-only now (the two-container split's whole point): a browser-shaped
// GET for a non-API route must NOT get the static shell back from the
// engine - that is the UI host's job, and the engine has no published
// port for a browser to reach anyway.
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

// uiHarness wraps an engineHarness with a real NewUI httptest.Server
// proxying to it, modelling the real two-container topology: engine has
// no published port and the UI host proxies through - see this package's
// own doc comment for the routing shape.
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

	ui := httptest.NewServer(server.NewUI(server.UIConfig{Upstream: upstream, StaticFS: staticFS}))
	t.Cleanup(ui.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	return &uiHarness{engineHarness: engine, ui: ui, client: &http.Client{Jar: jar}}
}

// TestUI_ProxiesAuthenticatedRequestsToTheEngine is this split's central
// end-to-end proof: enroll and log in THROUGH the UI host's reverse
// proxy (never touching the engine's httptest.Server directly), then
// confirm an authenticated GET /api/v1/system/version, also through the
// proxy, reaches the real engine and succeeds - the UI host forwards the
// session cookie, the CSRF header, and the response body/status
// unchanged, exactly as a browser talking only to the UI host in
// production would experience it.
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
// host's proxy, not just directly against the engine
// (TestEngine_UnauthenticatedDestructiveRequestIsRefused already proves
// the latter): the proxy must not strip or otherwise defeat the engine's
// own 401.
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

// TestUI_StaticUIServedForNonAPIRoute proves the UI host's static
// handler falls back to index.html for a client-side route (React
// Router's BrowserRouter needs the server to answer a hard refresh at
// any app path with the same shell, not a 404), and that the reverse
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
