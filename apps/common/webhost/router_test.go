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
// just this comment) why. A future route added to this map without a
// genuine justification is defeating the point of the gate walk in
// gate_redteam_test.go, not satisfying it. (The walk below, which shares
// this list, is the CSRF one: see its own doc for why the two are
// separate tests.)
var destructiveGateExemptRoutes = map[string]bool{
	// Issue #146 (B2.7): every one of these is "state-changing but
	// non-destructive" or outright read-only under
	// docs/EPIC-B-multi-nas.md §50 ("create/edit backup set", "generate
	// SSH key" are the non-destructive bucket; "test SSH", "probe host
	// key" are read-only), never "destructive" — none of them touch,
	// let alone delete, remote or local backup data. See router.go's own
	// comment on this route group for the full reasoning.
	"POST /api/v1/backup-sets":                 true,
	"POST /api/v1/backup-sets/test-connection": true,
	"POST /api/v1/ssh-keys":                    true,
	"POST /api/v1/ssh/host-key-probe":          true,

	// Issue #140 (B3.7): editing server-side configuration is §50's
	// "state-changing but non-destructive" bucket, alongside "create/edit
	// backup set" — nothing reachable from this route touches, moves or
	// deletes backup data. The one setting worth naming here,
	// protect_last_known_good, is dangerous because it widens what a
	// LATER retention apply may delete, and that apply
	// (POST /backup-sets/{source}/{set}/retention/apply) is itself gated
	// and is NOT on this list. See router.go's own comment on the route
	// for the full argument.
	"PATCH /api/v1/settings": true,

	// Issue #176 (B3.x): the setup submission of an instance that has no
	// configuration yet. Gating it would be self-defeating in the literal
	// sense: requireDestructiveGate refuses until an operator turns
	// destructive operations on, and on a fresh app-store install the only
	// way to turn anything on is the very flow this route completes, so a
	// gated first-run route is an instance that can never be configured
	// through its own UI. That is the exact failure #176 exists to remove.
	//
	// It is safe to exempt for a reason that does not depend on the gate:
	// this route can only ever write where nothing was. completeFirstRun
	// refuses with 409 ALREADY_CONFIGURED the moment a configuration
	// exists (firstrun.go, and again underneath at
	// service.ErrAlreadyConfigured), so it cannot replace, damage or even
	// see a live deployment's configuration - which is what the gate
	// protects. TestCompleteFirstRun_RefusesOnceConfigured pins that
	// refusal by its typed code, so this entry's justification is a test
	// and not just this comment.
	"POST /api/v1/system/first-run": true,

	// Issue #211. Each of these is state-changing but non-destructive
	// under the same §50 reading, and each handler's own doc carries the
	// argument in full; the short version:
	//
	//   - enabled: a disabled backup set is excluded from every run
	//     cycle, and everything already backed up stays exactly where it
	//     is. core/service pins that directly
	//     (TestSetBackupSetEnabled_DisablingDeletesNothing).
	//   - revalidate: re-reads a quarantined backup's local copy and
	//     reports a verdict. It writes nothing at all, and cannot: the
	//     lifecycle graph has no edge out of quarantine except back into
	//     the pipeline, which is the next route.
	//   - retry: moves a journal row from QUARANTINED to DISCOVERED. No
	//     local file and no remote object is touched.
	//   - catalog scan and rebuild: rebuild only ADDS journal rows whose
	//     recovery manifests are already on disk and whose rows are
	//     missing. It never removes or overwrites an existing row, never
	//     contacts a remote, and is a no-op against a healthy journal.
	//     Scan is that same pass with nothing written.
	//
	// None of them can reach a deletion, which is what this list is for.
	"POST /api/v1/backup-sets/{source}/{set}/enabled":          true,
	"POST /api/v1/quarantine/{source}/{set}/{name}/revalidate": true,
	"POST /api/v1/quarantine/{source}/{set}/{name}/retry":      true,
	"POST /api/v1/catalog/scan":                                true,
	"POST /api/v1/catalog/rebuild":                             true,
}

// TestEveryMutatingAPIRouteRefusesARequestWithNoCSRFPair walks the route
// table and fires a real request at every non-GET /api/v1 route, and
// what it proves is the CSRF walk in its name and nothing more.
//
// It was called TestNoMutatingAPIRouteBypassesTheDestructiveGate, and its
// doc comment said "a 403 from an authenticated request proves the GATE
// rejected it, not auth". Issue #87 (B5.1) disproved that by mutation:
// deleting requireDestructiveGate from BOTH gated routes leaves this test
// green, because the requests below carry no CSRF cookie/header pair and
// requireCSRF refuses first, with the same 403. A green test whose name
// promises a control it does not exercise is worse than no test, so this
// one is named for what it actually walks.
//
// The destructive gate's own structural proof is gate_redteam_test.go,
// which satisfies CSRF so the request reaches the gate, asserts the
// gate's own typed error code rather than a bare status, and refuses to
// run vacuously. Do not re-add a gate claim here without making these
// requests satisfy CSRF first.
//
// What remains is still worth having: every mutating route is reachable,
// is behind requireCSRF, and answers 403 to a request that does not carry
// the double-submit pair. The platform is fully authenticated
// (allowingPlatform), so the 403 is not authentication answering either.
func TestEveryMutatingAPIRouteRefusesARequestWithNoCSRFPair(t *testing.T) {
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
			t.Errorf("%s %s: status = %d, want %d (a mutating route must refuse a request carrying no CSRF pair)", method, route, rec.Code, http.StatusForbidden)
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
