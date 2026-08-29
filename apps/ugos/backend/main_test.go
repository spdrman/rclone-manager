// Command backup-manager-upk-proof is the bare backend half of the B1.2 minimal UPK
// proof (issue #91): it exposes GET /health/live and serves a static frontend bundle,
// and nothing else. It exists so the UPK proof doesn't have to pull in the whole
// apps/common/webhost API surface (auth middleware, the destructive-operation gate,
// core/service.BackupService) just to prove a packaged Docker App can answer a bare
// liveness probe. Real API wiring for UGOS is #83, not this.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthLive_ReturnsOKStatus(t *testing.T) {
	mux := newMux(t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health/live: got status %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /health/live: response body %q is not valid JSON: %v", rec.Body.String(), err)
	}
	if body["status"] != "ok" {
		t.Fatalf(`GET /health/live: got body %v, want {"status":"ok"}`, body)
	}
}

// /health/live is an infrastructure liveness probe, not a product API — it must never
// require credentials (mirrors apps/common/webhost's healthLive, which is deliberately
// registered outside the authenticated /api/v1 route group for the same reason).
func TestHealthLive_RequiresNoAuthentication(t *testing.T) {
	mux := newMux(t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	// Deliberately no Authorization header, no cookie, nothing.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health/live with no credentials: got status %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestStaticRoot_ServesTheBuiltFrontendBundle(t *testing.T) {
	webRoot := t.TempDir()
	const marker = "upk-proof-frontend-marker"
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte(marker), 0o644); err != nil {
		t.Fatalf("writing fixture index.html: %v", err)
	}

	mux := newMux(webRoot)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: got status %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("GET /: body %q does not contain the fixture frontend's marker %q", rec.Body.String(), marker)
	}
}

// A liveness probe answering means "the process can serve a bare HTTP response", not "the
// static frontend directory happens to exist". If webRoot is missing or empty, /health/live
// must still succeed — this is exactly the distinction the acceptance procedure's step 4
// leans on (a probe that only appears to work because it's coupled to unrelated state isn't
// proving what it claims to prove).
func TestHealthLive_SucceedsEvenWithoutAFrontendBundle(t *testing.T) {
	mux := newMux(filepath.Join(t.TempDir(), "does-not-exist"))

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health/live with no web root: got status %d, want %d", rec.Code, http.StatusOK)
	}
}
