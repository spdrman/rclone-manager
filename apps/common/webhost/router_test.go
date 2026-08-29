package webhost

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestNoAPIRouteBypassesAuthentication is issue #94's REGRESSION
// requirement made concrete: "no route bypasses the auth abstraction
// (write a test proving this, not just an assertion in prose)". Rather
// than hand-listing the routes this package happens to register today (a
// list that silently stops proving anything the day a route is added and
// nobody updates the list), this walks the router's own registered route
// table with chi.Walk and fires an unauthenticated request at literally
// every one of them.
//
// The PlatformAdapter used here (noAuthWiredAdapter) is not a "deny"
// stub built to make this test pass; it is what a provider actually looks
// like today, before #106's reserved apps/common/auth/local (local-auth)
// or #92 (platform-auth) exists. Every route failing closed against it is
// this package's whole fail-closed-by-construction argument, not a
// contrived test double.
func TestNoAPIRouteBypassesAuthentication(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	var checked int
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == "/health/live" || route == "/health/ready" {
			// Deliberately public (§17: "require UGOS authentication on
			// /api/", not on infra health checks); see the dedicated
			// health tests below for that claim.
			return nil
		}

		checked++
		req := httptest.NewRequest(method, strings.ReplaceAll(route, "{id}", "op_1"), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want %d (unauthenticated request must be rejected)", method, route, rec.Code, http.StatusUnauthorized)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if checked == 0 {
		t.Fatal("chi.Walk found no /api/v1 routes to check; this test would pass vacuously")
	}
}

func TestHealthEndpoints_DoNotRequireAuthentication(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	for _, path := range []string{"/health/live", "/health/ready"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d even with no Authenticator wired", path, rec.Code, http.StatusOK)
		}
	}
}

// TestAuthenticatedRequestReachesTheHandler is router_test's positive
// control: without it, a router that rejected every request unconditionally
// (a bug, not "fail closed by construction") would also pass the test
// above for the wrong reason.
func TestAuthenticatedRequestReachesTheHandler(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/version", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/system/version with a valid authenticator: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}
