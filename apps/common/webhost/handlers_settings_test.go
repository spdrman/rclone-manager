package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

type settingsTestRouter struct {
	router  http.Handler
	backend *settingsFakeBackend
}

func newSettingsTestRouter(t *testing.T) settingsTestRouter {
	t.Helper()
	backend := newSettingsFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          NotYetImplementedGate{}, // the shipped default: never passed
		BinaryVersion: "test",
		Commit:        "test",
	})
	return settingsTestRouter{router: router, backend: backend}
}

func (r settingsTestRouter) get(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

func (r settingsTestRouter) patch(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

func decodeSettings(t *testing.T, rec *httptest.ResponseRecorder) settingsResponse {
	t.Helper()
	var got settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v, body: %s", err, rec.Body.String())
	}
	return got
}

// ---------------------------------------------------------------- read

func TestGetSettings_ReturnsTheRunningPolicyAndTheSchemaItIsValidatedAgainst(t *testing.T) {
	tr := newSettingsTestRouter(t)

	rec := tr.get(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got := decodeSettings(t, rec)

	if got.Retention.Timezone != "UTC" || got.Retention.WeekStartsOn != "monday" {
		t.Errorf("retention = %+v, want UTC/monday", got.Retention)
	}
	if !got.Retention.ProtectLastKnownGood {
		t.Error("protect_last_known_good = false, want true")
	}
	if len(got.Retention.Tiers) != 3 || got.Retention.Tiers[0].Name != "daily" {
		t.Errorf("tiers = %+v, want the resolved three-tier default chain", got.Retention.Tiers)
	}
	// window_unit is not decoration: the default weekly tier buckets by
	// week but looks back over calendar months, so a wire shape that drops
	// it cannot express the default policy at all.
	if got.Retention.Tiers[1].WindowUnit != service.GranularityMonth {
		t.Errorf("tiers[1].window_unit = %q, want %q", got.Retention.Tiers[1].WindowUnit, service.GranularityMonth)
	}

	schema := got.Schema.Retention
	if len(schema.Granularities) != 7 {
		t.Errorf("schema.granularities = %v, want all seven", schema.Granularities)
	}
	for _, u := range schema.WindowUnits {
		if u == service.GranularityDays {
			t.Errorf("schema.window_units advertises %q, which the validator refuses", u)
		}
	}
	if schema.ReservedTierName != service.TierLastKnownGoodName {
		t.Errorf("schema.reserved_tier_name = %q, want %q", schema.ReservedTierName, service.TierLastKnownGoodName)
	}
	if schema.KeepMax != service.RetentionTierKeepMax || schema.PeriodDaysMax != service.RetentionTierPeriodDaysMax {
		t.Errorf("schema ceilings = %d/%d, want %d/%d", schema.KeepMax, schema.PeriodDaysMax, service.RetentionTierKeepMax, service.RetentionTierPeriodDaysMax)
	}
	if schema.TierNamePattern == "" {
		t.Error("schema.tier_name_pattern is empty; a client has nothing to validate a tier name against")
	}
}

func TestGetSettings_DoesNotRequireCSRF(t *testing.T) {
	// The read carries no CSRF token at all (tr.get never attaches one),
	// exactly like every other GET in this package.
	tr := newSettingsTestRouter(t)
	if rec := tr.get(t); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (a read should never need a CSRF token), body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestGetSettings_BackendFailureIsAGenericInternalError(t *testing.T) {
	tr := newSettingsTestRouter(t)
	tr.backend.errOnRead = errBoom

	rec := tr.get(t)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), errBoom.Error()) {
		t.Errorf("the response leaks an unclassified backend error string: %s", rec.Body.String())
	}
}

// --------------------------------------------------------------- write

func TestPatchSettings_PersistsARetentionChainAndReturnsTheRunningPolicy(t *testing.T) {
	tr := newSettingsTestRouter(t)

	rec := tr.patch(t, `{"retention":{"timezone":"Europe/Berlin","week_starts_on":"sunday","tiers":[
		{"name":"daily","granularity":"day","keep":10},
		{"name":"fortnightly","granularity":"days","period_days":14,"keep":6},
		{"name":"quarterly","granularity":"quarter","keep":4,"window_unit":"year"}
	],"protect_last_known_good":true}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got := decodeSettings(t, rec)
	if len(got.Retention.Tiers) != 3 {
		t.Fatalf("tiers = %+v, want 3", got.Retention.Tiers)
	}
	if got.Retention.Tiers[1].PeriodDays != 14 {
		t.Errorf("tiers[1].period_days = %d, want 14", got.Retention.Tiers[1].PeriodDays)
	}
	if got.Retention.Tiers[2].WindowUnit != "year" {
		t.Errorf("tiers[2].window_unit = %q, want %q", got.Retention.Tiers[2].WindowUnit, "year")
	}

	// What actually crossed the HTTP-to-core seam, not only what came back.
	req := tr.backend.lastUpdate
	if req.Retention == nil {
		t.Fatal("the handler called UpdateSettings with no retention section")
	}
	if req.Retention.Timezone == nil || *req.Retention.Timezone != "Europe/Berlin" {
		t.Errorf("Timezone = %v, want Europe/Berlin", req.Retention.Timezone)
	}
	if req.Retention.ProtectLastKnownGood == nil || !*req.Retention.ProtectLastKnownGood {
		t.Errorf("ProtectLastKnownGood = %v, want an explicit true", req.Retention.ProtectLastKnownGood)
	}
	if len(req.Retention.Tiers) != 3 || req.Retention.Tiers[1].Granularity != service.GranularityDays {
		t.Errorf("Tiers = %+v, want the submitted chain", req.Retention.Tiers)
	}
}

// TestPatchSettings_AnUnnamedFieldIsLeftAlone is the whole point of a
// generic partial-update endpoint: a client that only wants to flip one
// toggle must not have to send back a policy it never read, and the
// handler must not invent values for what the body left out.
func TestPatchSettings_AnUnnamedFieldIsLeftAlone(t *testing.T) {
	tr := newSettingsTestRouter(t)

	rec := tr.patch(t, `{"retention":{"protect_last_known_good":false}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	req := tr.backend.lastUpdate
	if req.Retention == nil {
		t.Fatal("no retention section reached the backend")
	}
	if req.Retention.ProtectLastKnownGood == nil || *req.Retention.ProtectLastKnownGood {
		t.Errorf("ProtectLastKnownGood = %v, want an explicit false", req.Retention.ProtectLastKnownGood)
	}
	// Positive control on the assertion below: the field that WAS named
	// arrived non-nil, so "the others are nil" measures omission rather
	// than a decoder that produced nothing at all.
	if req.Retention.Timezone != nil {
		t.Errorf("Timezone = %q, want nil (the body never named it)", *req.Retention.Timezone)
	}
	if req.Retention.WeekStartsOn != nil {
		t.Errorf("WeekStartsOn = %q, want nil (the body never named it)", *req.Retention.WeekStartsOn)
	}
	if req.Retention.Tiers != nil {
		t.Errorf("Tiers = %+v, want nil (the body never named the chain, so it must keep whatever spelling the file uses)", req.Retention.Tiers)
	}
}

// TestPatchSettings_AnExplicitlyEmptyChainReachesTheBackendAsEmptyNotAbsent
// is what makes core/service's refusal of an emptied chain reachable at
// all: `"tiers": []` and an omitted "tiers" must not collapse to the same
// nil on the way through the HTTP layer.
func TestPatchSettings_AnExplicitlyEmptyChainReachesTheBackendAsEmptyNotAbsent(t *testing.T) {
	tr := newSettingsTestRouter(t)
	tr.backend.errOnUpdate = service.ErrInvalidRequest

	rec := tr.patch(t, `{"retention":{"tiers":[]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	tiers := tr.backend.lastUpdate.Retention.Tiers
	if tiers == nil {
		t.Fatal("an explicitly empty tiers list arrived as nil, which the backend reads as \"leave the chain alone\" — the opposite request")
	}
	if len(tiers) != 0 {
		t.Errorf("Tiers = %+v, want an empty, non-nil slice", tiers)
	}
}

func TestPatchSettings_ErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		backendErr error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "a malformed body",
			body:       `{"retention":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
		{
			name:       "a body that names no setting at all",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
		{
			name:       "an unknown settings section",
			body:       `{"retenton":{"timezone":"UTC"}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
		{
			name:       "a policy the validator refuses",
			body:       `{"retention":{"tiers":[{"name":"Daily","granularity":"day","keep":7}]}}`,
			backendErr: service.ErrInvalidRequest,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
		{
			name:       "a deployment with no config file to persist to",
			body:       `{"retention":{"protect_last_known_good":false}}`,
			backendErr: service.ErrConfigNotFileBacked,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL",
		},
		{
			name:       "an unclassified backend failure",
			body:       `{"retention":{"protect_last_known_good":false}}`,
			backendErr: errBoom,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newSettingsTestRouter(t)
			tr.backend.errOnUpdate = tt.backendErr

			rec := tr.patch(t, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var got errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding error body: %v, body: %s", err, rec.Body.String())
			}
			if got.Error.Code != tt.wantCode {
				t.Errorf("error.code = %q, want %q", got.Error.Code, tt.wantCode)
			}
			if rec.Header().Get("X-Correlation-Id") == "" {
				t.Error("no X-Correlation-Id on an error response")
			}
			if tt.backendErr == errBoom && strings.Contains(rec.Body.String(), errBoom.Error()) {
				t.Errorf("the response leaks an unclassified backend error string: %s", rec.Body.String())
			}
		})
	}
}

// TestPatchSettings_ARefusedRequestNeverReachesTheBackend is the
// counterpart to the table above: the cases the HTTP layer refuses on its
// own must not have called into core at all, so a malformed body can
// never be half-applied by a backend that saw a partially decoded struct.
func TestPatchSettings_ARefusedRequestNeverReachesTheBackend(t *testing.T) {
	// `{"retention":{}}` is mandatory review finding M3: a present but
	// entirely empty section passed the old per-section guard, so a
	// zero-content body rewrote the operator's config file, moved
	// ConfigRevision (invalidating every outstanding retention preview) and
	// answered 200. It is also the cheapest way to reach the hot reload,
	// which is why it is refused here rather than only in core/service.
	for _, body := range []string{`{"retention":`, `{}`, `{"retention":{}}`, `{"retenton":{"timezone":"UTC"}}`} {
		t.Run(body, func(t *testing.T) {
			tr := newSettingsTestRouter(t)
			if rec := tr.patch(t, body); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if tr.backend.updateCalls != 0 {
				t.Errorf("UpdateSettings was called %d times for a request this layer refuses on its own", tr.backend.updateCalls)
			}
		})
	}

	// Positive control: a well-formed body DOES reach the backend, so the
	// zero above is a measurement rather than a fake that is never called.
	tr := newSettingsTestRouter(t)
	if rec := tr.patch(t, `{"retention":{"protect_last_known_good":false}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if tr.backend.updateCalls != 1 {
		t.Errorf("UpdateSettings calls = %d, want 1", tr.backend.updateCalls)
	}
}

func TestPatchSettings_RefusesABodyOverTheSizeLimit(t *testing.T) {
	tr := newSettingsTestRouter(t)

	oversized := `{"retention":{"timezone":"` + strings.Repeat("x", maxSettingsBodyBytes+1) + `"}}`
	rec := tr.patch(t, oversized)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "limit") {
		t.Errorf("the refusal does not mention the size limit: %s", rec.Body.String())
	}
	if tr.backend.updateCalls != 0 {
		t.Error("an oversized body still reached the backend")
	}
}

// ------------------------------------------------------- auth and CSRF

func TestPatchSettings_RequiresAuthentication(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		Backend:       newSettingsFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/settings", strings.NewReader(`{"retention":{"protect_last_known_good":false}}`))
		req.Header.Set("Content-Type", "application/json")
		attachValidCSRF(req)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s /api/v1/settings: status = %d, want %d", method, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestPatchSettings_MissingCSRFTokenReturns403(t *testing.T) {
	tr := newSettingsTestRouter(t)

	tests := []struct {
		name     string
		prepare  func(*http.Request)
		wantCode string
	}{
		{
			name:     "no cookie and no header",
			prepare:  func(*http.Request) {},
			wantCode: "CSRF_TOKEN_MISSING",
		},
		{
			name: "a cookie whose value the header does not echo",
			prepare: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: "bm_csrf", Value: testCSRFToken})
				r.Header.Set("X-CSRF-Token", "not-the-cookie-value")
			},
			wantCode: "CSRF_TOKEN_MISMATCH",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"retention":{"protect_last_known_good":false}}`))
			req.Header.Set("Content-Type", "application/json")
			tt.prepare(req)
			rec := httptest.NewRecorder()
			tr.router.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
			}
			var got errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding error body: %v", err)
			}
			if got.Error.Code != tt.wantCode {
				t.Errorf("error.code = %q, want %q", got.Error.Code, tt.wantCode)
			}
		})
	}

	if tr.backend.updateCalls != 0 {
		t.Error("a request refused for CSRF still reached the backend")
	}
}

// TestPatchSettings_IsNotBehindTheDestructiveGate records the decision
// rather than only relying on destructiveGateExemptRoutes' entry: every
// router in this file is built with NotYetImplementedGate (the shipped
// default, which never passes), so a settings write succeeding here IS
// the claim that the gate does not apply to it.
//
// Editing the retention policy is docs/EPIC-B-multi-nas.md §50's
// "state-changing but non-destructive" bucket, alongside "create/edit
// backup set": it changes configuration and touches no backup data. The
// deletion it can influence still has to go through POST
// /backup-sets/{source}/{set}/retention/apply, which IS gated and IS
// confirmed. Gating this route as well would leave the settings form
// permanently inert until #92 lands, without moving the point at which a
// file can actually be deleted by one inch.
func TestPatchSettings_IsNotBehindTheDestructiveGate(t *testing.T) {
	tr := newSettingsTestRouter(t)

	rec := tr.patch(t, `{"retention":{"protect_last_known_good":false}}`)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("status = 403 with the shipped NotYetImplementedGate; a settings write must not be behind the destructive gate, body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// The control that keeps the assertion above honest: the same router,
	// same gate, refusing the route that IS gated. Without this, a 200
	// here could equally mean the gate never denies anything.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(`{"action":"run_cycle"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "settings-gate-control")
	attachValidCSRF(req)
	gateRec := httptest.NewRecorder()
	tr.router.ServeHTTP(gateRec, req)
	if gateRec.Code != http.StatusForbidden {
		t.Fatalf("POST /api/v1/operations status = %d, want %d; this gate denies nothing, so the settings assertion above proves nothing", gateRec.Code, http.StatusForbidden)
	}
}

// TestPatchSettings_MethodsOtherThanPatchAreNotRegistered pins the write
// verb down: a client that guesses POST or PUT gets a 405, not a silent
// no-op that looks like success.
func TestPatchSettings_MethodsOtherThanPatchAreNotRegistered(t *testing.T) {
	tr := newSettingsTestRouter(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/settings", strings.NewReader(`{"retention":{"protect_last_known_good":false}}`))
		req.Header.Set("Content-Type", "application/json")
		attachValidCSRF(req)
		rec := httptest.NewRecorder()
		tr.router.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/settings: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
	if tr.backend.updateCalls != 0 {
		t.Error("a request with an unregistered method still reached the backend")
	}
}
