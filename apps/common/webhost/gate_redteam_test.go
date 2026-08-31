package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/apps/common/csrf"
)

// gate_redteam_test.go is issue #87 (B5.1)'s attack on the destructive
// gate's own regression test rather than on the gate.
//
// router_test.go used to carry a walk called
// TestNoMutatingAPIRouteBypassesTheDestructiveGate: every non-GET route,
// asserting a 403. Every one of those routes also carries requireCSRF,
// which chi applies FIRST, and which answers a request with no CSRF
// cookie with... 403. So the walk never reached the gate on any route at
// all: it passed unchanged with requireDestructiveGate deleted from every
// route in the table. A negative assertion that does not say WHY the
// request failed is exactly the shape that keeps passing after the
// control it names has gone. That walk is still there, renamed
// TestEveryMutatingAPIRouteRefusesARequestWithNoCSRFPair for the property
// it does prove.
//
// The two tests below close that: one drives the same walk with a valid
// CSRF pair and asserts the gate's own typed code, and one is the
// positive control proving the walk request really does get past the gate
// when the gate passes.

// csrfPaired attaches a matching double-submit cookie/header pair, so a
// request under test is refused by whatever comes AFTER requireCSRF
// rather than by requireCSRF itself.
func csrfPaired(req *http.Request) *http.Request {
	const token = "gate-walk-token"
	req.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	req.Header.Set(csrf.HeaderName, token)
	return req
}

func responseErrorCode(body string) string {
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return ""
	}
	return doc.Error.Code
}

// gateWalk fires one authenticated, CSRF-satisfying request at every
// non-GET /api/v1 route that is not on destructiveGateExemptRoutes, and
// returns each route's response code and typed error code.
func gateWalk(t *testing.T, gate DestructiveGate) map[string]struct {
	status int
	code   string
} {
	t.Helper()

	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		Gate:          gate,
		BinaryVersion: "test",
		Commit:        "test",
	})

	out := map[string]struct {
		status int
		code   string
	}{}
	err := chi.Walk(routableFor(t, router), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet || strings.HasPrefix(route, "/health") {
			return nil
		}
		if destructiveGateExemptRoutes[method+" "+route] {
			return nil
		}
		req := httptest.NewRequest(method, strings.ReplaceAll(route, "{id}", "op_1"), strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "gate-redteam-"+method+"-"+route)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, csrfPaired(req))
		out[method+" "+route] = struct {
			status int
			code   string
		}{rec.Code, responseErrorCode(rec.Body.String())}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("chi.Walk found no gated non-GET /api/v1 routes; this test would pass vacuously")
	}
	return out
}

// TestGatedRoutesAreRefusedByTheGateAndNotByTheCSRFCheck asserts WHICH
// control refused, not merely that something did.
func TestGatedRoutesAreRefusedByTheGateAndNotByTheCSRFCheck(t *testing.T) {
	for route, got := range gateWalk(t, NotYetImplementedGate{}) {
		if got.status != http.StatusForbidden {
			t.Errorf("%s: status = %d, want %d", route, got.status, http.StatusForbidden)
		}
		if got.code != "DESTRUCTIVE_OPERATIONS_DISABLED" {
			t.Errorf("%s: error code = %q, want DESTRUCTIVE_OPERATIONS_DISABLED.\n"+
				"this route is not behind the destructive gate; a bare 403 assertion would not have noticed, because requireCSRF refuses with the same status",
				route, got.code)
		}
	}
}

// TestTheGateWalkReachesTheGate is the positive control. With a gate that
// passes, the identical walk must NOT produce the gate's own refusal on
// any route: if it still did, the assertion above would be about
// something other than the gate.
func TestTheGateWalkReachesTheGate(t *testing.T) {
	for route, got := range gateWalk(t, alwaysPassGate{}) {
		if got.code == "DESTRUCTIVE_OPERATIONS_DISABLED" {
			t.Errorf("%s was refused by the destructive gate even though the gate passes, so the walk never reaches it", route)
		}
		if got.code == "CSRF_TOKEN_MISSING" || got.code == "CSRF_TOKEN_MISMATCH" {
			t.Errorf("%s was refused by the CSRF check (%s), so this walk still does not reach the gate", route, got.code)
		}
		if got.status == http.StatusUnauthorized {
			t.Errorf("%s was refused by authentication, so this walk never reaches the gate", route)
		}
	}
}

// TestTheShippedGateCannotBeMadeToPass. The only DestructiveGate this
// repository ships reports false, and NewRouter's omission default is
// that gate rather than an open door. Both halves matter: a gate that
// could be flipped by a config value, or a router that treated a missing
// gate as "allow", would each defeat the tests above without changing a
// single line either one of them reads.
func TestTheShippedGateCannotBeMadeToPass(t *testing.T) {
	var shipped DestructiveGate = NotYetImplementedGate{}
	if shipped.Passed() {
		t.Error("NotYetImplementedGate reports true")
	}

	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		Gate:          nil, // omitted on purpose
		BinaryVersion: "test",
		Commit:        "test",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(`{"action":"run_cycle"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "omitted-gate")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, csrfPaired(req))

	if rec.Code != http.StatusForbidden || responseErrorCode(rec.Body.String()) != "DESTRUCTIVE_OPERATIONS_DISABLED" {
		t.Fatalf("a RouterConfig with no Gate answered %d %s, want 403 DESTRUCTIVE_OPERATIONS_DISABLED: %s",
			rec.Code, responseErrorCode(rec.Body.String()), rec.Body.String())
	}
}
