package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

func previewRetention(t *testing.T, router http.Handler, source, set string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup-sets/"+source+"/"+set+"/retention/preview", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func applyRetention(t *testing.T, router http.Handler, source, set, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backup-sets/"+source+"/"+set+"/retention/apply", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestPreviewRetention_Success_ReturnsSpecSchemaFields is docs/EPIC-B-
// multi-nas.md §15.6's own schema, checked at the actual HTTP boundary:
// every field that section's example JSON names must be present in the
// real response body, in the snake_case it names.
func TestPreviewRetention_Success_ReturnsSpecSchemaFields(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	rec := previewRetention(t, tr.router, "production", "postgres-primary")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, field := range []string{
		"plan_id", "inventory_revision", "config_revision", "expires_at",
		"keep_count", "delete_count", "reclaim_bytes",
	} {
		if _, ok := body[field]; !ok {
			t.Errorf("response is missing %q (docs/EPIC-B-multi-nas.md §15.6's own schema): body=%v", field, body)
		}
	}
	if planID, _ := body["plan_id"].(string); !strings.HasPrefix(planID, "retplan_") {
		t.Errorf("plan_id = %v, want a retplan_ prefix", body["plan_id"])
	}
}

// TestPreviewRetention_DoesNotRequireCSRFOrTheDestructiveGate proves §50's
// own classification (preview retention is read-only/low risk) holds at
// the actual route: a GET with the gate closed and no CSRF token still
// succeeds, unlike applyRetention below.
func TestPreviewRetention_DoesNotRequireCSRFOrTheDestructiveGate(t *testing.T) {
	tr := newOperationsTestRouter(t, NotYetImplementedGate{}) // gate NOT passed
	rec := previewRetention(t, tr.router, "production", "postgres-primary")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (preview must not be behind the destructive gate), body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestPreviewRetention_BackendErrorMapsToBackupSetNotFound(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.errOnPreview = service.ErrBackupSetNotFound

	rec := previewRetention(t, tr.router, "production", "does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "BACKUP_SET_NOT_FOUND" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "BACKUP_SET_NOT_FOUND")
	}
}

// TestApplyRetention_PreviewThenApply_Success is the wizard flow §29.3
// describes end to end at the HTTP boundary: obtain a plan, submit its own
// plan_id back, and get 200 with the same plan_id and a non-empty
// operation_id (the durable operation this apply was recorded under).
func TestApplyRetention_PreviewThenApply_Success(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})

	previewRec := previewRetention(t, tr.router, "production", "postgres-primary")
	var preview map[string]any
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	planID, _ := preview["plan_id"].(string)
	if planID == "" {
		t.Fatal("preview response has no plan_id")
	}

	applyRec := applyRetention(t, tr.router, "production", "postgres-primary", `{"plan_id":"`+planID+`"}`)
	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply status = %d, want %d, body: %s", applyRec.Code, http.StatusOK, applyRec.Body.String())
	}
	var applied map[string]any
	if err := json.Unmarshal(applyRec.Body.Bytes(), &applied); err != nil {
		t.Fatalf("decode apply response: %v", err)
	}
	if applied["plan_id"] != planID {
		t.Errorf("apply response plan_id = %v, want %q (the exact plan reviewed)", applied["plan_id"], planID)
	}
	if op, _ := applied["operation_id"].(string); op == "" {
		t.Error("apply response operation_id is empty, want the durable operation this apply was recorded under")
	}
}

// TestApplyRetention_StalePlanReturns409WithItsOwnCode is this issue's own
// Given/When/Then example (docs/EPIC-B-multi-nas.md §71 WP 3.1, §15.6),
// checked at the actual wire boundary a browser client hits: a stale
// plan_id gets a 409 carrying error.code == "RETENTION_PLAN_STALE",
// distinct from every other error code this route can return, and the
// fake backend behind this router never records the plan as consumed
// successfully.
func TestApplyRetention_StalePlanReturns409WithItsOwnCode(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.errOnApply = service.ErrRetentionPlanStale

	rec := applyRetention(t, tr.router, "production", "postgres-primary", `{"plan_id":"retplan_whatever"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "RETENTION_PLAN_STALE" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "RETENTION_PLAN_STALE")
	}
}

func TestApplyRetention_UnknownPlanIDReturns404WithItsOwnCode(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})

	rec := applyRetention(t, tr.router, "production", "postgres-primary", `{"plan_id":"retplan_never-issued"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "RETENTION_PLAN_NOT_FOUND" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "RETENTION_PLAN_NOT_FOUND")
	}
}

func TestApplyRetention_MalformedJSONReturns400(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	rec := applyRetention(t, tr.router, "production", "postgres-primary", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestApplyRetention_MissingCSRFCookieReturns403 mirrors
// TestSubmitOperation_MissingCSRFCookieReturns403 exactly (both are the
// one destructive-operations gate + CSRF combination §17 requires): apply
// is destructive, so it must be refused the same way submitOperation
// already is when the CSRF check itself fails, independent of anything
// the destructive gate or the backend would otherwise decide.
func TestApplyRetention_MissingCSRFCookieReturns403(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backup-sets/production/postgres-primary/retention/apply", strings.NewReader(`{"plan_id":"retplan_x"}`))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no attachValidCSRF call.
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

// TestApplyRetention_PlanIDFromAnotherBackupSetIsRefused proves this route
// actually reads the {source}/{set} it is routed by and passes it down to
// be cross-checked, rather than acting on plan_id alone (this issue's own
// review, mandatory finding M5): a valid plan id submitted under a
// different backup set's path is refused, and the artifact behind it is
// never deleted.
func TestApplyRetention_PlanIDFromAnotherBackupSetIsRefused(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})

	preview := previewRetention(t, tr.router, "production", "postgres-primary")
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want %d, body: %s", preview.Code, http.StatusOK, preview.Body.String())
	}
	var plan map[string]any
	if err := json.Unmarshal(preview.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	planID, _ := plan["plan_id"].(string)

	rec := applyRetention(t, tr.router, "production", "billing", `{"plan_id":"`+planID+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "INVALID_REQUEST")
	}

	// Positive control: the same plan id under its own backup set's path
	// succeeds, so the refusal above is the cross-check firing and not a
	// plan id this route could never have applied at all.
	ok := applyRetention(t, tr.router, "production", "postgres-primary", `{"plan_id":"`+planID+`"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("control: status = %d, want %d, body: %s", ok.Code, http.StatusOK, ok.Body.String())
	}
}

// TestApplyRetention_BusyServiceGetsItsOwnCode proves a refusal caused by
// a concurrently executing backup cycle is not reported to the client as a
// stale plan: the plan is intact and the client should retry the same
// plan_id, which is a different instruction from "re-preview and
// re-confirm" (see service.ErrRetentionApplyBusy's own doc).
func TestApplyRetention_BusyServiceGetsItsOwnCode(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.errOnApply = service.ErrRetentionApplyBusy

	rec := applyRetention(t, tr.router, "production", "postgres-primary", `{"plan_id":"retplan_test_1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "RETENTION_APPLY_BUSY" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "RETENTION_APPLY_BUSY")
	}
}

// TestPreviewRetention_VerdictsSayWhichPlacementSelectedEachTier is issue
// #218 at the HTTP boundary. Since #215 a tier can have selected an
// artifact through either of FR-18's two placements, and FR-8 trusts one
// of them and not the other, so the wire has to carry that per tier. A
// single attribution on the verdict would be wrong for exactly the
// artifact this fixture holds, whose DAILY and MONTHLY came from
// different passes.
func TestPreviewRetention_VerdictsSayWhichPlacementSelectedEachTier(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	rec := previewRetention(t, tr.router, "production", "postgres-primary")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Verdicts []struct {
			Artifact       string   `json:"artifact"`
			Action         string   `json:"action"`
			Tiers          []string `json:"tiers"`
			TierSelections []struct {
				Tier       string `json:"tier"`
				SelectedBy string `json:"selected_by"`
			} `json:"tier_selections"`
		} `json:"verdicts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var kept, deleted int
	for _, v := range body.Verdicts {
		switch v.Action {
		case "KEEP":
			kept++
			got := map[string]string{}
			for _, sel := range v.TierSelections {
				got[sel.Tier] = sel.SelectedBy
			}
			want := map[string]string{"DAILY": "DISCOVERY", "MONTHLY": "PRODUCER", "LAST_KNOWN_GOOD": "PROTECTION"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s tier_selections = %v, want %v", v.Artifact, got, want)
			}
			if len(v.Tiers) != len(v.TierSelections) {
				t.Errorf("%s: tiers (%v) and tier_selections (%v) describe different numbers of tiers", v.Artifact, v.Tiers, v.TierSelections)
			}
		case "DELETE":
			deleted++
			if len(v.TierSelections) != 0 {
				t.Errorf("%s is a DELETE candidate but carries tier attribution %v", v.Artifact, v.TierSelections)
			}
		}
	}
	if kept == 0 || deleted == 0 {
		t.Fatalf("the fixture produced %d KEEP and %d DELETE verdicts; this test needs one of each or it proves nothing", kept, deleted)
	}
}

// ------------------------------------------------- EPIC E, issue #430 ---
//
// #239 put FR-27's moves and FR-30's per-deletion medium on the
// preview/apply envelope in core/service and on `backup-manager
// retention`, and stopped at this boundary. Until these pass, an operator
// can see a planned move and the medium a deletion happens on from the
// CLI and not from the API, so the web surface silently under-reports what
// retention is about to do.

// planKeys is one response object's own key set, sorted, which is what
// the two tests below assert against rather than field-by-field presence:
// a key that appears where none used to is exactly as much of a change as
// a key that goes missing, and only a whole-set comparison catches both.
func planKeys(t *testing.T, obj map[string]json.RawMessage) []string {
	t.Helper()
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func decodePlan(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return obj
}

// placementPlan is the fixture EPIC E's own tests below drive: one artifact
// the chain would relocate, one deletion that would happen in somebody
// else's bucket, one deletion that would happen on this machine, and one
// artifact nothing could place.
func placementPlan(p service.RetentionPlan) service.RetentionPlan {
	p.Verdicts = []service.RetentionArtifactVerdict{
		{Artifact: "month-old.dump", Action: "KEEP", Medium: "local", Reason: "kept by the MONTHLY(discovery) tier (test fixture)"},
		{Artifact: "offsite.dump", Action: "DELETE", Medium: "cold_offsite", Reason: "no GFS tier selects this artifact (test fixture)"},
		{Artifact: "here.dump", Action: "DELETE", Medium: "local", Reason: "no GFS tier selects this artifact (test fixture)"},
		{Artifact: "midmove.dump", Action: "REFUSE", Reason: "more than one ACTIVE placement, which is a move in flight (test fixture)"},
	}
	p.Moves = []service.RetentionMove{{Artifact: "month-old.dump", FromMedium: "local", ToMedium: "cold_offsite"}}
	p.UnconfirmedPlacements = []string{"midmove.dump"}
	return p
}

// TestPreviewRetention_CarriesEveryMoveWithBothMediums is FR-27 at the
// HTTP boundary: the response names every artifact this plan would
// relocate and both ends of the move, so an operator confirming a plan is
// shown the bytes it would copy as well as the ones it would delete.
func TestPreviewRetention_CarriesEveryMoveWithBothMediums(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.previewPlan = placementPlan

	body := decodePlan(t, previewRetention(t, tr.router, "production", "postgres-primary"))

	raw, ok := body["moves"]
	if !ok {
		t.Fatalf("the preview response carries no \"moves\" at all, so a UI cannot show what would move before a plan is confirmed: body=%s", mustJSON(t, body))
	}
	var moves []map[string]string
	if err := json.Unmarshal(raw, &moves); err != nil {
		t.Fatalf("decode moves: %v", err)
	}
	want := []map[string]string{{"artifact": "month-old.dump", "from_medium": "local", "to_medium": "cold_offsite"}}
	if !reflect.DeepEqual(moves, want) {
		t.Errorf("moves = %+v, want %+v", moves, want)
	}
}

// TestPreviewRetention_CarriesThePlacementsItCouldNotConfirm is the other
// half of FR-27's plan. "I could not confirm where this is" and "this is
// already where it belongs" produce the same silence and are not the same
// claim, and only one of them is a move already in flight.
func TestPreviewRetention_CarriesThePlacementsItCouldNotConfirm(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.previewPlan = placementPlan

	body := decodePlan(t, previewRetention(t, tr.router, "production", "postgres-primary"))

	raw, ok := body["unconfirmed_placements"]
	if !ok {
		t.Fatalf("the preview response carries no \"unconfirmed_placements\" at all: body=%s", mustJSON(t, body))
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		t.Fatalf("decode unconfirmed_placements: %v", err)
	}
	if !reflect.DeepEqual(names, []string{"midmove.dump"}) {
		t.Errorf("unconfirmed_placements = %v, want [midmove.dump]", names)
	}
}

// TestPreviewRetention_EveryDeletionNamesTheMediumItHappensOn is FR-30's
// own sentence at this boundary: the dry-run explains per-artifact WHERE
// the deletion would happen, not only whether. "Delete 40 artifacts" means
// something very different when half of them are objects in a bucket
// somebody else pays for.
//
// A deletion on the implicit local medium carries no `medium` key, and
// that absence is the answer rather than a gap: it is what keeps a
// deployment that declares no storage medium reading exactly as it did
// before this field existed, and `backup-manager retention` spells the
// same asymmetry the same way (mediumSuffix, core/cmd/backup-manager/
// retention.go).
func TestPreviewRetention_EveryDeletionNamesTheMediumItHappensOn(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.previewPlan = placementPlan

	body := decodePlan(t, previewRetention(t, tr.router, "production", "postgres-primary"))
	verdicts := decodeVerdicts(t, body)

	offsite, ok := verdicts["offsite.dump"]
	if !ok {
		t.Fatalf("no verdict for offsite.dump: %s", mustJSON(t, body))
	}
	var medium string
	if err := json.Unmarshal(offsite["medium"], &medium); err != nil {
		t.Fatalf("offsite.dump's verdict carries no usable \"medium\" (%v); an operator confirming this plan is authorising a delete against a bucket, and the plan has to say so", err)
	}
	if medium != "cold_offsite" {
		t.Errorf("offsite.dump's verdict names medium %q, want %q", medium, "cold_offsite")
	}

	here, ok := verdicts["here.dump"]
	if !ok {
		t.Fatalf("no verdict for here.dump: %s", mustJSON(t, body))
	}
	if _, present := here["medium"]; present {
		t.Errorf("here.dump's verdict carries a \"medium\" key (%s), want none: a deletion on the implicit local medium is spelled by the absence of the field, which is what keeps a medium-free deployment's response unchanged", here["medium"])
	}
}

// TestApplyRetention_CarriesTheSamePlacementFactsThePreviewDid pins the
// half of §15.6 that says a caller never has to reconcile two shapes for
// "what would happen" and "what happened". The apply response is the same
// projection, so an operator reading the outcome sees the moves and the
// mediums they confirmed.
func TestApplyRetention_CarriesTheSamePlacementFactsThePreviewDid(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.previewPlan = placementPlan

	preview := decodePlan(t, previewRetention(t, tr.router, "production", "postgres-primary"))
	var planID string
	if err := json.Unmarshal(preview["plan_id"], &planID); err != nil {
		t.Fatalf("decode plan_id: %v", err)
	}

	applied := decodePlan(t, applyRetention(t, tr.router, "production", "postgres-primary", `{"plan_id":"`+planID+`"}`))
	for _, field := range []string{"moves", "unconfirmed_placements"} {
		// Without this, two absent fields would compare equal and this
		// test would pass hardest on the build that carries neither.
		if _, ok := preview[field]; !ok {
			t.Fatalf("the preview itself carries no %q, so comparing the apply response against it proves nothing", field)
		}
		if !reflect.DeepEqual(applied[field], preview[field]) {
			t.Errorf("apply response %s = %s, want the preview's own %s", field, applied[field], preview[field])
		}
	}
	if got, want := decodeVerdicts(t, applied)["offsite.dump"]["medium"], decodeVerdicts(t, preview)["offsite.dump"]["medium"]; !reflect.DeepEqual(got, want) {
		t.Errorf("apply response's offsite.dump medium = %s, want the preview's own %s", got, want)
	}
}

// TestPreviewRetention_AMediumFreeDeploymentsResponseIsUnchanged is the
// compatibility claim, asserted as a whole key set rather than as a list
// of fields that must be present: a response that grew a key is exactly as
// changed as one that lost a key, and only this comparison sees both.
//
// The fixture backend's plan is what core/service really returns for a
// deployment that declares no storage medium: every verdict names the
// implicit local medium, no move is planned, and nothing is unplaced. None
// of the three may reach the wire, because every deployment written before
// EPIC E is this one.
func TestPreviewRetention_AMediumFreeDeploymentsResponseIsUnchanged(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})

	body := decodePlan(t, previewRetention(t, tr.router, "production", "postgres-primary"))

	wantTop := []string{
		"backup_set_id", "config_revision", "delete_count", "expires_at",
		"inventory_revision", "keep_count", "plan_id", "reclaim_bytes",
		"retention", "retention_is_override", "verdicts",
	}
	if got := planKeys(t, body); !reflect.DeepEqual(got, wantTop) {
		t.Errorf("a medium-free preview's top-level keys are %v, want exactly %v", got, wantTop)
	}

	verdicts := decodeVerdicts(t, body)
	for name, want := range map[string][]string{
		"kept.dump":   {"action", "artifact", "reason", "tier_selections", "tiers"},
		"backup.dump": {"action", "artifact", "reason"},
	} {
		v, ok := verdicts[name]
		if !ok {
			t.Fatalf("no verdict for %s: %s", name, mustJSON(t, body))
		}
		if got := planKeys(t, v); !reflect.DeepEqual(got, want) {
			t.Errorf("a medium-free preview's %s verdict has keys %v, want exactly %v", name, got, want)
		}
	}
}

// TestPreviewRetention_ThePlacementFieldsCarryNoCredentialCostOrETA is
// FR-33's standing absence, checked over the shape this issue adds rather
// than argued from the fact that nobody wrote such a field.
//
// A move names two places and an artifact. It never names the key material
// that reaches either of them, what a provider would charge to run it, or
// how long a provider might take: this product holds none of those three,
// and a field for one would have to be filled with a guess.
func TestPreviewRetention_ThePlacementFieldsCarryNoCredentialCostOrETA(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.previewPlan = placementPlan

	rec := previewRetention(t, tr.router, "production", "postgres-primary")
	body := decodePlan(t, rec)
	if _, ok := body["moves"]; !ok {
		t.Fatalf("this test needs a response that actually carries the placement fields to check them for absences: %s", rec.Body.String())
	}

	var decoded any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	forbidden := []string{
		"credential", "secret", "password", "access_key", "token", "signed_url",
		"cost", "price", "bill", "charge",
		"eta", "estimate", "duration", "seconds_remaining",
	}
	for _, key := range jsonKeyPaths(decoded, "") {
		lower := strings.ToLower(key)
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Errorf("the retention preview carries %q, which names a %s; this product holds no credential, no provider bill and no provider ETA, so a field for one could only be filled with a guess", key, bad)
			}
		}
	}
}

// jsonKeyPaths lists every object key in a decoded JSON document, as a
// dotted path, so the absence check above walks the whole response rather
// than only its top level.
func jsonKeyPaths(node any, prefix string) []string {
	var out []string
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			out = append(out, path)
			out = append(out, jsonKeyPaths(v, path)...)
		}
	case []any:
		for _, v := range n {
			out = append(out, jsonKeyPaths(v, prefix+"[]")...)
		}
	}
	return out
}

// decodeVerdicts indexes a plan response's verdicts by artifact name, each
// still a raw key/value map so a test can ask whether a key is PRESENT
// rather than only what it decodes to.
func decodeVerdicts(t *testing.T, body map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	t.Helper()
	var verdicts []map[string]json.RawMessage
	if err := json.Unmarshal(body["verdicts"], &verdicts); err != nil {
		t.Fatalf("decode verdicts: %v", err)
	}
	out := make(map[string]map[string]json.RawMessage, len(verdicts))
	for _, v := range verdicts {
		var name string
		if err := json.Unmarshal(v["artifact"], &name); err != nil {
			t.Fatalf("decode verdict artifact: %v", err)
		}
		out[name] = v
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
