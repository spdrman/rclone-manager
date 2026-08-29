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
// routableFor type-asserts router (NewRouter's return type is http.Handler,
// deliberately narrowed — see that function's own doc) back to chi.Routes
// so a test can still walk its registered route table with chi.Walk. Every
// router this package's own tests build is a *chi.Mux underneath, so this
// assertion is never expected to fail; it exists so that fact lives in one
// place instead of an unchecked type assertion at every call site.
func routableFor(t *testing.T, router http.Handler) chi.Routes {
	t.Helper()
	routes, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("router %T does not implement chi.Routes; chi.Walk needs it", router)
	}
	return routes
}

func TestNoAPIRouteBypassesAuthentication(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	var checked int
	err := chi.Walk(routableFor(t, router), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
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

// destructiveGateExemptRoutes names every non-GET /api/v1 route that is
// deliberately NOT behind requireDestructiveGate, and (in a real entry, not
// just this comment) why. It is empty today: the only mutating route this
// skeleton has, POST /api/v1/operations, is gated. A future route added to
// this map without a genuine justification is defeating the point of
// TestNoMutatingAPIRouteBypassesTheDestructiveGate below, not satisfying
// it.
var destructiveGateExemptRoutes = map[string]bool{}

// TestNoMutatingAPIRouteBypassesTheDestructiveGate is issue #118 item 3's
// structural regression test, mirroring
// TestNoAPIRouteBypassesAuthentication above exactly the way the review
// that asked for it did: `r.With(requireDestructiveGate(gate)).Post(...)`
// compiles fine and passes every other test in this package even if a
// future mutating route forgets to chain requireDestructiveGate onto
// itself, since nothing else in this package's route table walks every
// route and checks. This does, using the same chi.Walk-driven,
// fire-a-real-request approach as the auth test, rather than the auth
// test's neighbour asserting a specific route list by name (that list
// would silently stop proving anything the day a route is added and
// nobody updates it).
//
// The platform here is fully authenticated (allowingPlatform), unlike
// TestNoAPIRouteBypassesAuthentication's noAuthWiredAdapter: a 403 from an
// authenticated request proves the GATE rejected it, not auth (auth is
// already proven not to bypass anything, and ordering between the two is
// TestSubmitOperation_GateIsCheckedAfterAuthentication's own job, not
// this test's).
func TestNoMutatingAPIRouteBypassesTheDestructiveGate(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		Gate:          NotYetImplementedGate{}, // gate NOT passed
		BinaryVersion: "test",
		Commit:        "test",
	})

	var checked int
	err := chi.Walk(routableFor(t, router), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet {
			// Reads are never destructive; see getOperation's own doc for
			// why GET /api/v1/operations/{id} in particular is
			// deliberately exempt from this gate.
			return nil
		}
		if route == "/health/live" || route == "/health/ready" {
			return nil
		}
		if destructiveGateExemptRoutes[method+" "+route] {
			return nil
		}

		checked++
		req := httptest.NewRequest(method, strings.ReplaceAll(route, "{id}", "op_1"), strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "gate-walk-"+method+"-"+route)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want %d (a non-GET route must be behind the destructive gate)", method, route, rec.Code, http.StatusForbidden)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if checked == 0 {
		t.Fatal("chi.Walk found no non-GET /api/v1 routes to check; this test would pass vacuously")
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
