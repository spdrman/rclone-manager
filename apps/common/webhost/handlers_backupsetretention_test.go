package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getRetention(t *testing.T, router http.Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup-sets/"+id+"/retention", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func postRetention(t *testing.T, router http.Handler, id, tail, body string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backup-sets/"+id+"/retention"+tail, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		attachValidCSRF(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

type retentionBody struct {
	BackupSetID string `json:"backup_set_id"`
	IsOverride  bool   `json:"is_override"`
	Policy      struct {
		Timezone             string `json:"timezone"`
		WeekStartsOn         string `json:"week_starts_on"`
		ProtectLastKnownGood bool   `json:"protect_last_known_good"`
		Tiers                []struct {
			Name   string `json:"name"`
			Keep   int    `json:"keep"`
			Medium string `json:"medium"`
		} `json:"tiers"`
	} `json:"policy"`
	DeploymentPolicy struct {
		Tiers []struct {
			Name string `json:"name"`
			Keep int    `json:"keep"`
		} `json:"tiers"`
	} `json:"deployment_policy"`
}

func decodeRetention(t *testing.T, rec *httptest.ResponseRecorder) retentionBody {
	t.Helper()
	var out retentionBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return out
}

// TestBackupSetRetention_ShowSetClearRoundTrip is issue #333's API half,
// through the router: an inheriting set says so, setting an override
// makes the set decide for itself, and clearing puts it back. Driven as
// one sequence for the same reason the CLI's is: what the issue asks for
// is a round trip an operator can make and unmake, and three independent
// cases would each prove a step while leaving that unchecked.
func TestBackupSetRetention_ShowSetClearRoundTrip(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	inherited := decodeRetention(t, getRetention(t, tr.router, "api/postgres-primary"))
	if inherited.IsOverride {
		t.Error("is_override is true for a set that declares nothing")
	}
	if len(inherited.Policy.Tiers) == 0 {
		t.Fatal("the reported policy names no tier; it has to be the resolved chain")
	}

	rec := postRetention(t, tr.router, "api/postgres-primary", "",
		`{"tiers":[{"name":"daily","granularity":"day","keep":30}]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	set := decodeRetention(t, rec)
	if !set.IsOverride {
		t.Error("is_override is false immediately after setting an override")
	}
	if len(set.Policy.Tiers) != 1 || set.Policy.Tiers[0].Keep != 30 {
		t.Errorf("policy.tiers = %+v, want the single 30-day tier just set", set.Policy.Tiers)
	}
	// What clearing would return to, without a second request.
	if len(set.DeploymentPolicy.Tiers) == 0 {
		t.Error("deployment_policy names no tier, so a client cannot show what clearing would restore")
	}

	cleared := decodeRetention(t, postRetention(t, tr.router, "api/postgres-primary", "/clear", "", true))
	if cleared.IsOverride {
		t.Error("is_override is still true after clearing")
	}
	if len(cleared.Policy.Tiers) != len(inherited.Policy.Tiers) {
		t.Errorf("after clearing the set decides under %+v, want the deployment chain %+v", cleared.Policy.Tiers, inherited.Policy.Tiers)
	}
}

// TestSetBackupSetRetention_CarriesEveryTierFieldAcrossTheSeam. A whole
// chain is REPLACED by this write, so a field the body cannot hold is a
// field the write deletes from the operator's file. The medium is the one
// that costs real money to get wrong: dropping it moves that tier's
// artifacts back onto local disk with nothing reported.
func TestSetBackupSetRetention_CarriesEveryTierFieldAcrossTheSeam(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := postRetention(t, tr.router, "api/postgres-primary", "", `{
	  "tiers": [
	    {"name":"daily","granularity":"day","keep":14},
	    {"name":"fortnightly","granularity":"days","period_days":14,"keep":6,"window_unit":"month","medium":"offsite"}
	  ],
	  "timezone": "America/Vancouver",
	  "week_starts_on": "sunday",
	  "protect_last_known_good": false
	}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	got := tr.backend.lastRetentionOverride()
	if len(got.Tiers) != 2 {
		t.Fatalf("the service saw %d tier(s), want 2", len(got.Tiers))
	}
	second := got.Tiers[1]
	if second.Name != "fortnightly" || second.Granularity != "days" || second.PeriodDays != 14 ||
		second.Keep != 6 || second.WindowUnit != "month" || second.Medium != "offsite" {
		t.Errorf("the second tier crossed the seam as %+v; one of its fields was dropped", second)
	}
	if got.Timezone != "America/Vancouver" || got.WeekStartsOn != "sunday" {
		t.Errorf("the calendar did not cross the seam: %+v", got)
	}
	if got.ProtectLastKnownGood == nil || *got.ProtectLastKnownGood {
		t.Errorf("an explicit protect_last_known_good: false did not cross the seam as one: %v", got.ProtectLastKnownGood)
	}

	// And the read side carries it back, so a client round-trips exactly
	// what it sent rather than a chain quietly missing a field.
	back := decodeRetention(t, getRetention(t, tr.router, "api/postgres-primary"))
	if len(back.Policy.Tiers) != 2 || back.Policy.Tiers[1].Medium != "offsite" {
		t.Errorf("the read side lost the medium: %+v", back.Policy.Tiers)
	}
	if back.Policy.ProtectLastKnownGood {
		t.Error("the read side reports protection on for a set that explicitly turned it off")
	}
}

// TestSetBackupSetRetention_OmittedCalendarArrivesEmptyRatherThanGuessed.
// "" is what "inherit the deployment's" is spelled as, so a handler that
// filled in a default here would silently move which day every restore
// point in this one set belongs to.
func TestSetBackupSetRetention_OmittedCalendarArrivesEmptyRatherThanGuessed(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	if rec := postRetention(t, tr.router, "api/postgres-primary", "",
		`{"tiers":[{"name":"daily","granularity":"day","keep":30}]}`, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	got := tr.backend.lastRetentionOverride()
	if got.Timezone != "" || got.WeekStartsOn != "" {
		t.Errorf("an omitted calendar arrived as %q/%q rather than empty", got.Timezone, got.WeekStartsOn)
	}
	if got.ProtectLastKnownGood != nil {
		t.Errorf("an omitted protect_last_known_good arrived as %v rather than nil", *got.ProtectLastKnownGood)
	}
}

// TestBackupSetRetention_UnknownSetIs404 on all three operations. A 200
// for a name this deployment does not configure would read as "that set
// now has this policy".
func TestBackupSetRetention_UnknownSetIs404(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	if rec := getRetention(t, tr.router, "api/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404", rec.Code)
	}
	if rec := postRetention(t, tr.router, "api/nope", "", `{"tiers":[{"name":"daily","granularity":"day","keep":7}]}`, true); rec.Code != http.StatusNotFound {
		t.Errorf("POST status = %d, want 404", rec.Code)
	}
	if rec := postRetention(t, tr.router, "api/nope", "/clear", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("POST clear status = %d, want 404", rec.Code)
	}
}

// TestBackupSetRetention_WritesRequireCSRFAndTheReadDoesNot. The two
// writes are ordinary state-changing routes; the read changes nothing and
// must not be made to carry a token, or a client cannot show a policy
// without first arranging to write one.
func TestBackupSetRetention_WritesRequireCSRFAndTheReadDoesNot(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	if rec := getRetention(t, tr.router, "api/postgres-primary"); rec.Code != http.StatusOK {
		t.Errorf("GET without CSRF status = %d, want 200", rec.Code)
	}
	if rec := postRetention(t, tr.router, "api/postgres-primary", "", `{"tiers":[{"name":"daily","granularity":"day","keep":7}]}`, false); rec.Code != http.StatusForbidden {
		t.Errorf("POST without CSRF status = %d, want 403", rec.Code)
	}
	if rec := postRetention(t, tr.router, "api/postgres-primary", "/clear", "", false); rec.Code != http.StatusForbidden {
		t.Errorf("POST clear without CSRF status = %d, want 403", rec.Code)
	}
}

// TestBackupSetRetention_IsNotBehindTheDestructiveGate backs this pair's
// entry in destructiveGateExemptRoutes with a real request through a
// CLOSED gate, so the exemption's justification is a test rather than its
// own comment. Writing a policy deletes nothing; a retention APPLY is
// what the gate is for, and it stays gated.
func TestBackupSetRetention_IsNotBehindTheDestructiveGate(t *testing.T) {
	backend := newBackupSetFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          NotYetImplementedGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	tr := backupSetsTestRouter{router: router, backend: backend}
	seedSet(t, tr, "api/postgres-primary")

	if rec := postRetention(t, tr.router, "api/postgres-primary", "", `{"tiers":[{"name":"daily","granularity":"day","keep":7}]}`, true); rec.Code != http.StatusOK {
		t.Errorf("set status = %d with the gate closed, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if rec := postRetention(t, tr.router, "api/postgres-primary", "/clear", "", true); rec.Code != http.StatusOK {
		t.Errorf("clear status = %d with the gate closed, want 200, body: %s", rec.Code, rec.Body.String())
	}
}
