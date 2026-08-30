package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
