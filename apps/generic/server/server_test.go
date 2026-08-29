// server_test.go is this issue's central RED/GREEN pivot (issue #82/B4.1,
// docs/EPIC-B-multi-nas.md §9.2): it stands up the actual generic Web
// host - a real *service.BackupService opened from a real (temp) config
// file, real local authentication, the real apps/common/webhost router,
// and a real static file server - behind a real net/http/httptest
// listener, and drives it with an ordinary *http.Client exactly as a
// browser or a deployment script would.
package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
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

// testHarness stands up the full generic Web host handler behind an
// httptest.Server, with a cookie-jar-carrying client so login/enroll
// establish a real, usable session exactly as a browser would observe.
type testHarness struct {
	server *httptest.Server
	auth   *local.Service
	client *http.Client
}

func newTestHarness(t *testing.T) *testHarness {
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

	staticFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html><body>generic backup-manager UI shell</body></html>")},
	}

	handler := server.New(server.Config{
		Backend:       backend,
		Auth:          authSvc,
		BinaryVersion: "test",
		Commit:        "testcommit",
		StaticFS:      staticFS,
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	return &testHarness{server: srv, auth: authSvc, client: &http.Client{Jar: jar}}
}

// bootstrapToken extracts the current single-use enrollment token via
// PrintBootstrapNotice - the same string a real deployment's container
// log would show, not a test-only shortcut.
func (h *testHarness) bootstrapToken(t *testing.T) string {
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

// csrfToken seeds the client's cookie jar (a harmless GET, exactly as a
// browser's first page load would) and returns the CSRF token it picked
// up.
func (h *testHarness) csrfToken(t *testing.T) string {
	t.Helper()
	resp, err := h.client.Get(h.server.URL + "/")
	if err != nil {
		t.Fatalf("seed GET /: %v", err)
	}
	resp.Body.Close()

	u, err := resp.Request.URL.Parse("/")
	if err != nil {
		t.Fatalf("URL.Parse: %v", err)
	}
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == local.CSRFCookieName {
			return c.Value
		}
	}
	t.Fatal("no CSRF cookie present after seeding GET /")
	return ""
}

// enrollAndLogIn drives the real POST /api/v1/auth/enroll route end to
// end, leaving h.client holding a real, live session cookie afterward.
func (h *testHarness) enrollAndLogIn(t *testing.T) {
	t.Helper()
	csrf := h.csrfToken(t)
	token := h.bootstrapToken(t)

	body, _ := json.Marshal(map[string]string{"username": "bm-admin", "password": "correct-horse-battery"})
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/auth/enroll", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(local.CSRFHeaderName, csrf)
	req.Header.Set(local.BootstrapTokenHeader, token)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/auth/enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("enroll status = %d, want %d; body=%s", resp.StatusCode, http.StatusNoContent, b)
	}
}

// TestUnauthenticatedDestructiveRequestIsRefused proves the destructive
// POST /api/v1/operations route is unreachable without authentication -
// exactly the "gate it behind local authentication" half of this issue's
// scope, exercised against the REAL composed handler, not just
// apps/common/webhost's own unit tests (which already prove this for the
// router in isolation).
func TestUnauthenticatedDestructiveRequestIsRefused(t *testing.T) {
	h := newTestHarness(t)

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

// TestAuthenticatedRequestSucceedsAgainstSystemVersion proves the other
// half: real local credentials, established through the real
// enroll/login HTTP flow, are enough to reach apps/common/webhost's own
// /api/v1/system/version route through this composed server - the
// local-auth Authenticator this issue wires and apps/common/webhost's
// pre-existing authMiddleware actually agree with each other end to end.
func TestAuthenticatedRequestSucceedsAgainstSystemVersion(t *testing.T) {
	h := newTestHarness(t)
	h.enrollAndLogIn(t)

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

// TestStaticUIServedForNonAPIRoute proves the static handler falls back
// to index.html for a client-side route (React Router's BrowserRouter
// needs the server to answer a hard refresh at any app path with the
// same shell, not a 404), while a route that IS a real static file is
// still served as itself.
func TestStaticUIServedForNonAPIRoute(t *testing.T) {
	h := newTestHarness(t)

	for _, path := range []string{"/", "/sets/some-backup-set", "/settings"} {
		resp, err := h.client.Get(h.server.URL + path)
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
