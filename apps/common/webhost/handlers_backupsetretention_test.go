package webhost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

const retentionRoute = "/api/v1/backup-sets/production/postgres-primary/retention"

func retentionRouterWith(t *testing.T, backend BackupServiceClient) http.Handler {
	t.Helper()
	return NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          NotYetImplementedGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
}

func doRetention(t *testing.T, router http.Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, retentionRoute, nil)
	} else {
		r = httptest.NewRequest(method, retentionRoute, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		attachValidCSRF(r)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

func decodeRetention(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, rec.Body.String())
	}
	return body
}

func tierNamesOf(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("tiers is %T, not a list", v)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("a tier is %T, not an object", e)
		}
		out = append(out, m["name"].(string))
	}
	return out
}

// TestGetBackupSetRetention_SaysWhichPolicyIsInForce is the read half of
// the route group. An inheriting set has to say so, and it has to serve
// the deployment's chain even while inheriting it, because a form about
// to CREATE an override pre-fills from a whole resolved chain and that is
// what stops the first submission being half a policy.
func TestGetBackupSetRetention_SaysWhichPolicyIsInForce(t *testing.T) {
	router := retentionRouterWith(t, newSyncFakeBackend())

	rec := doRetention(t, router, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	body := decodeRetention(t, rec)
	if body["is_override"] != false {
		t.Errorf("is_override = %v, want false", body["is_override"])
	}
	if _, present := body["override"]; present && body["override"] != nil {
		t.Errorf("override = %v, want null for an inheriting set", body["override"])
	}
	effective, _ := body["effective"].(map[string]any)
	deployment, _ := body["deployment"].(map[string]any)
	if effective == nil || deployment == nil {
		t.Fatalf("effective/deployment missing from %s", rec.Body.String())
	}
	if got := tierNamesOf(t, effective["tiers"]); len(got) != 2 {
		t.Errorf("effective tiers = %v, want the deployment's two", got)
	}
	if deployment["timezone"] != "America/Vancouver" {
		t.Errorf("deployment.timezone = %v", deployment["timezone"])
	}
}

// TestGetBackupSetRetention_CarriesATiersMedium is the round-trip
// property that makes PUT safe. A chain write replaces the whole chain,
// so a field the wire cannot carry is a field the save deletes from the
// operator's configuration file: reading a chain whose monthly tier lives
// on a storage medium and writing it straight back has to leave the
// medium where it was.
func TestGetBackupSetRetention_CarriesATiersMedium(t *testing.T) {
	router := retentionRouterWith(t, newSyncFakeBackend())
	body := decodeRetention(t, doRetention(t, router, http.MethodGet, ""))

	effective, _ := body["effective"].(map[string]any)
	tiers, _ := effective["tiers"].([]any)
	monthly, _ := tiers[1].(map[string]any)
	if monthly["medium"] != "cold" {
		t.Fatalf("the monthly tier's medium did not reach the wire: %v", monthly)
	}
}

// TestSetAndClearBackupSetRetention_AreTheTwoDirectionsOfOneState drives
// the whole cycle through the router: inherit, override, inherit again.
//
// The clear half is the one that cannot be expressed as a value on a
// sparse update, and it is asserted on the resulting STATE rather than on
// the response to the DELETE, because "the set is inheriting again" is
// the claim and a handler could return a convincing body while writing
// nothing.
func TestSetAndClearBackupSetRetention_AreTheTwoDirectionsOfOneState(t *testing.T) {
	backend := newSyncFakeBackend()
	router := retentionRouterWith(t, backend)

	rec := doRetention(t, router, http.MethodPut, `{"tiers":[{"name":"daily","granularity":"day","keep":4}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	body := decodeRetention(t, rec)
	if body["is_override"] != true {
		t.Errorf("is_override after PUT = %v, want true", body["is_override"])
	}
	override, _ := body["override"].(map[string]any)
	if override == nil {
		t.Fatalf("override missing from the PUT response: %s", rec.Body.String())
	}
	if got := tierNamesOf(t, override["tiers"]); len(got) != 1 || got[0] != "daily" {
		t.Errorf("override tiers = %v", got)
	}

	if backend.lastSetRetention == nil {
		t.Fatal("nothing reached the service seam")
	}
	if backend.lastSetRetention.id != "production/postgres-primary" {
		t.Errorf("the handler built the id %q", backend.lastSetRetention.id)
	}

	// The GET in between: the state is real, not an echo of the PUT.
	body = decodeRetention(t, doRetention(t, router, http.MethodGet, ""))
	if body["is_override"] != true {
		t.Errorf("a GET after the PUT reports is_override = %v", body["is_override"])
	}

	rec = doRetention(t, router, http.MethodDelete, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if backend.lastClearedRetention != "production/postgres-primary" {
		t.Errorf("the clear reached the seam as %q", backend.lastClearedRetention)
	}
	body = decodeRetention(t, doRetention(t, router, http.MethodGet, ""))
	if body["is_override"] != false {
		t.Errorf("is_override after the DELETE = %v, want false", body["is_override"])
	}
	if body["override"] != nil {
		t.Errorf("override after the DELETE = %v, want null", body["override"])
	}
}

// TestSetBackupSetRetention_PassesTheSubmissionThroughUnresolved is the
// property the whole route group rests on: this layer decides nothing
// about what a whole chain is.
//
// A submission naming one of the three scalars reaches core/service
// EXACTLY as sent, so the refusal comes from the one place that owns the
// rule. A handler that "helped" by filling in the other two would produce
// a policy nobody wrote, which is the failure #362 was written to stop,
// reintroduced one layer up.
func TestSetBackupSetRetention_PassesTheSubmissionThroughUnresolved(t *testing.T) {
	backend := newSyncFakeBackend()
	router := retentionRouterWith(t, backend)

	if rec := doRetention(t, router, http.MethodPut, `{"daily_days":120}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body: %s", rec.Code, rec.Body.String())
	}
	got := backend.lastSetRetention.override
	want := service.RetentionOverride{DailyDays: 120}
	if got.DailyDays != want.DailyDays || got.WeeklyMonths != 0 || got.MonthlyMonths != 0 {
		t.Fatalf("the seam received %+v, want the submission unresolved (%+v)", got, want)
	}
	if got.Timezone != "" || got.WeekStartsOn != "" || got.ProtectLastKnownGood != nil {
		t.Fatalf("the handler filled in fields the caller omitted: %+v", got)
	}
	if got.Tiers != nil {
		t.Fatalf("the handler invented a tiers list: %v", got.Tiers)
	}
}

// TestSetBackupSetRetention_KeepsEmptyAndAbsentTiersApart is the
// distinction that survives the wire or nothing downstream can act on it:
// an absent `tiers` key is "I did not name a chain", and `"tiers": []` is
// "I removed every tier", which core/service refuses by name because
// emptying a chain widens the policy rather than disabling it.
func TestSetBackupSetRetention_KeepsEmptyAndAbsentTiersApart(t *testing.T) {
	backend := newSyncFakeBackend()
	router := retentionRouterWith(t, backend)

	doRetention(t, router, http.MethodPut, `{"daily_days":1,"weekly_months":1,"monthly_months":1}`)
	if backend.lastSetRetention.override.Tiers != nil {
		t.Fatalf("an absent tiers key arrived as %v, want nil", backend.lastSetRetention.override.Tiers)
	}

	doRetention(t, router, http.MethodPut, `{"tiers":[]}`)
	tiers := backend.lastSetRetention.override.Tiers
	if tiers == nil {
		t.Fatal("an explicitly empty tiers list arrived as nil, so core/service cannot refuse it by name")
	}
	if len(tiers) != 0 {
		t.Fatalf("an explicitly empty tiers list arrived as %v", tiers)
	}
}

// TestSetBackupSetRetention_EchoesTheConfigLayersOwnRefusal is what makes
// "a per-set chain is validated exactly as the global one is" visible to
// an operator rather than only true in a comment. core/service refuses
// with config.Validate's own sentence, and this route has to carry that
// sentence through rather than replacing it with a form-validation
// paraphrase.
func TestSetBackupSetRetention_EchoesTheConfigLayersOwnRefusal(t *testing.T) {
	backend := newSyncFakeBackend()
	backend.errOnSetRetention = errInvalidPerSetChain
	router := retentionRouterWith(t, backend)

	rec := doRetention(t, router, http.MethodPut, `{"daily_days":120}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "INVALID_REQUEST" {
		t.Fatalf("error code = %q, want INVALID_REQUEST", code)
	}
	if !strings.Contains(rec.Body.String(), "weekly_months") {
		t.Fatalf("the refusal lost the missing-field detail: %s", rec.Body.String())
	}
}

// TestBackupSetRetention_UnknownSetIs404 covers all three methods at
// once: a route keyed by an identity has to answer for the identity it
// was given rather than for the deployment.
func TestBackupSetRetention_UnknownSetIs404(t *testing.T) {
	backend := newSyncFakeBackend()
	backend.errOnSetRetention = errNoSuchBackupSet
	router := retentionRouterWith(t, backend)

	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		body := ""
		if m == http.MethodPut {
			body = `{"daily_days":1,"weekly_months":1,"monthly_months":1}`
		}
		rec := doRetention(t, router, m, body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404, body: %s", m, rec.Code, rec.Body.String())
		}
		if code := errorCodeOf(t, rec); code != "BACKUP_SET_NOT_FOUND" {
			t.Errorf("%s error code = %q, want BACKUP_SET_NOT_FOUND", m, code)
		}
	}
}

// TestBackupSetRetention_WritesAreNotBehindTheDestructiveGate backs the
// two entries this route group has in destructiveGateExemptRoutes with a
// real request through a CLOSED gate, rather than leaving the exemption
// justified only by its own comment.
//
// The direction that needs saying out loud: these routes CAN change what
// a later retention apply deletes, in the dangerous direction, and they
// are still exempt because PATCH /settings already is for exactly the
// same hazard. The apply is the gated act.
func TestBackupSetRetention_WritesAreNotBehindTheDestructiveGate(t *testing.T) {
	router := retentionRouterWith(t, newSyncFakeBackend()) // NotYetImplementedGate: closed

	if rec := doRetention(t, router, http.MethodPut, `{"daily_days":1,"weekly_months":1,"monthly_months":1}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT through a closed gate = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if rec := doRetention(t, router, http.MethodDelete, ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE through a closed gate = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
}

// TestPreviewRetention_ReportsThePolicyItDecidedUnder is the API half of
// the issue's "the preview says which policy it applied" criterion.
func TestPreviewRetention_ReportsThePolicyItDecidedUnder(t *testing.T) {
	backend := newSyncFakeBackend()
	router := retentionRouterWith(t, backend)

	preview := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, retentionRoute+"/preview", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}
		return decodeRetention(t, rec)
	}

	body := preview()
	if body["retention_is_override"] != false {
		t.Errorf("an inheriting set's plan reports retention_is_override = %v", body["retention_is_override"])
	}
	policy, _ := body["retention"].(map[string]any)
	if policy == nil {
		t.Fatalf("the plan carries no retention policy: %v", body)
	}
	if got := tierNamesOf(t, policy["tiers"]); len(got) != 2 {
		t.Errorf("plan chain = %v, want the deployment's two tiers", got)
	}

	doRetention(t, router, http.MethodPut, `{"tiers":[{"name":"daily","granularity":"day","keep":4}]}`)

	body = preview()
	if body["retention_is_override"] != true {
		t.Errorf("a plan computed under the set's own policy reports retention_is_override = %v", body["retention_is_override"])
	}
	policy, _ = body["retention"].(map[string]any)
	if got := tierNamesOf(t, policy["tiers"]); len(got) != 1 || got[0] != "daily" {
		t.Errorf("plan chain = %v, want the set's own one tier", got)
	}
}

// errInvalidPerSetChain and errNoSuchBackupSet stand in for the two
// refusals core/service actually produces, with the config layer's own
// wording preserved: what these tests are about is that this route maps
// and carries them, not that it re-derives them.
var errInvalidPerSetChain = fmt.Errorf("%w: invalid config: sources[0].backup_sets[0].retention: a backup set's own policy replaces "+
	"the deployment's whole chain, so it has to name a whole one: either a tiers list, or all three of daily_days, weekly_months and "+
	"monthly_months (missing weekly_months, monthly_months)", service.ErrInvalidRequest)

var errNoSuchBackupSet = fmt.Errorf("%w: production/postgres-primary", service.ErrBackupSetNotFound)
