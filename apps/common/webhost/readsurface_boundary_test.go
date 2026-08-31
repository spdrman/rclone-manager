package webhost

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This file is issue #211's own reproduction, turned into a test.
//
// The issue was measured by booting a real engine, signing in, and
// driving the shipped pages: four of six failed, because client.ts asked
// for fourteen (method, path) pairs the runtime answered with a 404 or a
// 405. Nothing in this repository could have caught that. The browser
// suite runs against createMockApi, which implements whatever the client
// asks for; the handler tests exercise the routes that exist rather than
// the ones the client calls; and the conformance test pinned the fourteen
// exactly, as an allowlist, which is why it stayed green.
//
// So this drives every one of those pairs against the REAL
// service.BackupService, over the real chi router, and asserts that none
// of them is answered by the router's own "no such route" or "wrong verb"
// reply. It is deliberately not an assertion about status 200: several of
// these legitimately refuse (there is no quarantined artifact to retry in
// a fresh deployment, and the shipped destructive gate refuses a run
// cycle outright). What it asserts is that the refusal comes from a
// HANDLER, in the typed envelope a client can read, rather than from chi
// having nothing registered.
//
// scripts/api/check-client-paths.sh is the static half of the same rule
// and runs on every commit. This is the dynamic half, and neither
// subsumes the other: the static check reads client.ts against the
// contract and knows nothing about whether a route was actually
// registered, while this reads the router and knows nothing about what
// the client asks for.

// routerReply classifies one response as coming from chi's own fallbacks
// or from a handler.
type routerReply struct {
	status int
	body   string
}

func (r routerReply) routerSaidNoSuchRoute() bool {
	return r.status == http.StatusNotFound && strings.Contains(r.body, "404 page not found")
}

func (r routerReply) routerSaidWrongVerb() bool {
	return r.status == http.StatusMethodNotAllowed
}

// routerRefused is either of the above: chi answers "no such route" for a
// path nothing matches, and "wrong verb" for one that matches a pattern
// registered under a different method. Both mean the same thing to a
// browser, which is why the walk above rejects both and the control below
// accepts either.
func (r routerReply) routerRefused() bool {
	return r.routerSaidNoSuchRoute() || r.routerSaidWrongVerb()
}

func driveBoundary(t *testing.T, router http.Handler, method, target string) routerReply {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodPost || method == http.MethodPatch {
		body = strings.NewReader("{}")
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "boundary-"+method+"-"+target)
	if method != http.MethodGet {
		attachValidCSRF(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return routerReply{status: rec.Code, body: rec.Body.String()}
}

// TestEveryPathTheWebUICallsIsServedByARealRuntime is the regression for
// the whole of issue #211.
//
// The table is the issue's own, verbatim in content: every (method, path)
// pair it measured as a 404 or a 405 against a real engine on main, plus
// the four the client used to spell wrongly, expressed here the way the
// router registers them.
func TestEveryPathTheWebUICallsIsServedByARealRuntime(t *testing.T) {
	configPath := writeBoundaryConfig(t)
	router, closeSvc := openBoundaryRouter(t, configPath)
	defer closeSvc()

	// Every path in issue #211's table, at the concrete URL the shared
	// client actually builds for it. "production/postgres-primary" is the
	// backup set writeBoundaryConfig configures, so the two id-shaped
	// routes are driven with an identity that really exists rather than
	// with a placeholder that would 404 for an uninteresting reason.
	cases := []struct {
		what   string
		method string
		target string
	}{
		{"the version banner", http.MethodGet, "/api/v1/system/version"},
		{"the dashboard's health summary", http.MethodGet, "/api/v1/system/health"},
		{"the backups list", http.MethodGet, "/api/v1/backups"},
		{"the backups list, filtered", http.MethodGet, "/api/v1/backups?setId=production%2Fpostgres-primary"},
		{"one backup", http.MethodGet, "/api/v1/backups/production/postgres-primary/backup.dump"},
		{"the activity feed", http.MethodGet, "/api/v1/activity"},
		{"the quarantine list", http.MethodGet, "/api/v1/quarantine"},
		{"revalidating a quarantined backup", http.MethodPost, "/api/v1/quarantine/production/postgres-primary/backup.dump/revalidate"},
		{"retrying a quarantined backup", http.MethodPost, "/api/v1/quarantine/production/postgres-primary/backup.dump/retry"},
		{"the live operations poll", http.MethodGet, "/api/v1/operations"},
		{"submitting a run cycle", http.MethodPost, "/api/v1/operations"},
		{"enabling a backup set", http.MethodPost, "/api/v1/backup-sets/production/postgres-primary/enabled"},
		{"testing a persisted set's connection", http.MethodPost, "/api/v1/backup-sets/test-connection"},
		{"the catalog-recovery scan", http.MethodPost, "/api/v1/catalog/scan"},
		{"the catalog rebuild", http.MethodPost, "/api/v1/catalog/rebuild"},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			got := driveBoundary(t, router, tc.method, tc.target)
			if got.routerSaidNoSuchRoute() {
				t.Fatalf("%s %s: the router has no such route (%d %q). This is exactly what issue #211 measured against a real engine.",
					tc.method, tc.target, got.status, strings.TrimSpace(got.body))
			}
			if got.routerSaidWrongVerb() {
				t.Fatalf("%s %s: the router serves this path under a different verb (405). GET /api/v1/operations was this case.",
					tc.method, tc.target)
			}
			if got.status >= 500 {
				t.Fatalf("%s %s: %d %s", tc.method, tc.target, got.status, strings.TrimSpace(got.body))
			}
		})
	}
}

// TestTheRouterStillRefusesAPathNobodyRegistered is the positive control
// for the walk above. Without it, "nothing answered 404" would be equally
// consistent with a router that matched everything, and the table would
// prove nothing at all.
func TestTheRouterStillRefusesAPathNobodyRegistered(t *testing.T) {
	configPath := writeBoundaryConfig(t)
	router, closeSvc := openBoundaryRouter(t, configPath)
	defer closeSvc()

	for _, tc := range []struct {
		what   string
		method string
		target string
	}{
		// The shapes issue #211 measured. The first two are paths that
		// exist nowhere at all. The third is the one the client used to
		// call and which must STAY unserved, because the run cycle it
		// stands for is deployment-wide and has no per-set form; it comes
		// back as a 405 rather than a 404, because "/backup-sets/*" is
		// registered for GET and chi answers a matched pattern under an
		// unregistered method that way. To a browser the two are the same
		// refusal, which is why the walk above rejects both.
		{"a path nothing registers", http.MethodGet, "/api/v1/nothing-here"},
		{"a write to a path nothing registers", http.MethodPost, "/api/v1/catalog/nothing-here"},
		{"the per-set run route that never existed", http.MethodPost, "/api/v1/backup-sets/production/postgres-primary/run"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got := driveBoundary(t, router, tc.method, tc.target)
			if !got.routerRefused() {
				t.Fatalf("%s %s answered %d %q; the walk above can only mean something while an unregistered path really is refused",
					tc.method, tc.target, got.status, strings.TrimSpace(got.body))
			}
		})
	}
}

// TestTheShippedGateStillRefusesARunCycle is the second reason a UI action
// cannot reach the backend, and issue #211 notes it precisely because it
// is easy to confuse with the first when reading a 403 next to a 404.
//
// POST /api/v1/operations is now reachable (the walk above proves the
// route exists), and with the shipped default gate it is refused by name.
// That is deliberate and tracked separately (#92); what changed here is
// that the gate finally stands in front of something a browser can
// actually ask for, having previously guarded a route no client called.
func TestTheShippedGateStillRefusesARunCycle(t *testing.T) {
	configPath := writeBoundaryConfig(t)
	router, closeSvc := openBoundaryRouter(t, configPath)
	defer closeSvc()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations",
		strings.NewReader(`{"action":"run_cycle","config_revision":"whatever"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "boundary-gate")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from the shipped NotYetImplementedGate; body: %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "DESTRUCTIVE_OPERATIONS_DISABLED" {
		t.Errorf("code = %q, want DESTRUCTIVE_OPERATIONS_DISABLED; a bare 403 would also be what a missing CSRF pair looks like", code)
	}
}
