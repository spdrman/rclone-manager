package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

type operationsTestRouter struct {
	router  http.Handler
	backend *syncFakeBackend
}

func newOperationsTestRouter(t *testing.T, gate DestructiveGate) operationsTestRouter {
	t.Helper()
	backend := newSyncFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          gate,
		BinaryVersion: "test",
		Commit:        "test",
	})
	return operationsTestRouter{router: router, backend: backend}
}

func submitOperation(t *testing.T, router http.Handler, idempotencyKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestSubmitOperation_Success_Returns202WithOperationIDAndStatus(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	rec := submitOperation(t, tr.router, "idem-1", `{"action":"run_cycle","config_revision":"rev-1"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["operation_id"] == "" || body["operation_id"] == nil {
		t.Error("operation_id is missing/empty")
	}
	if body["status"] == "" || body["status"] == nil {
		t.Error("status is missing/empty")
	}
	if body["actor"] != "alice" {
		t.Errorf("actor = %v, want %q (from the authenticated caller, not the request body)", body["actor"], "alice")
	}
}

func TestSubmitOperation_MissingIdempotencyKeyReturns400(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	rec := submitOperation(t, tr.router, "", `{"action":"run_cycle","config_revision":"rev-1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestSubmitOperation_UnsupportedActionReturns400(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	rec := submitOperation(t, tr.router, "idem-1", `{"action":"delete_everything","config_revision":"rev-1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestSubmitOperation_MalformedJSONReturns400(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	rec := submitOperation(t, tr.router, "idem-1", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestSubmitOperation_ConfigRevisionStaleReturns409 proves the HTTP layer
// maps service.ErrConfigRevisionStale to a 409 Conflict, matching §15.6's
// RETENTION_PLAN_STALE precedent applied to a stale configuration revision
// generally.
func TestSubmitOperation_ConfigRevisionStaleReturns409(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.errOnSubmit = service.ErrConfigRevisionStale

	rec := submitOperation(t, tr.router, "idem-1", `{"action":"run_cycle","config_revision":"stale-rev"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "CONFIG_REVISION_STALE" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "CONFIG_REVISION_STALE")
	}
}

// TestSubmitOperation_GateNotPassedReturns403 is the destructive-gate half
// of issue #94's INTEGRATION requirement: even a fully authenticated
// request must be refused, with a distinct status/code from
// "unauthenticated", while the gate has not passed.
func TestSubmitOperation_GateNotPassedReturns403(t *testing.T) {
	tr := newOperationsTestRouter(t, NotYetImplementedGate{})
	rec := submitOperation(t, tr.router, "idem-1", `{"action":"run_cycle","config_revision":"rev-1"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "DESTRUCTIVE_OPERATIONS_DISABLED" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "DESTRUCTIVE_OPERATIONS_DISABLED")
	}
}

// TestSubmitOperation_GateIsCheckedAfterAuthentication proves ordering: an
// unauthenticated request against a disabled gate must be reported as
// unauthenticated (401), not "gate disabled" (403) — a caller must never
// learn anything about server-side authorization state before proving who
// they are.
func TestSubmitOperation_GateIsCheckedAfterAuthentication(t *testing.T) {
	backend := newSyncFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		Backend:       backend,
		Gate:          NotYetImplementedGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	rec := submitOperation(t, router, "idem-1", `{"action":"run_cycle","config_revision":"rev-1"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (auth must be checked first)", rec.Code, http.StatusUnauthorized)
	}
}

func TestGetOperation_UnknownIDReturns404(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operations/op_does_not_exist", nil)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestGetOperation_Success_ReturnsOperationJSON(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	submitRec := submitOperation(t, tr.router, "idem-1", `{"action":"run_cycle","config_revision":"rev-1"}`)
	var submitted map[string]any
	if err := json.Unmarshal(submitRec.Body.Bytes(), &submitted); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	id, _ := submitted["operation_id"].(string)
	if id == "" {
		t.Fatal("submitted operation_id is empty")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/operations/"+id, nil)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if body["operation_id"] != id {
		t.Errorf("operation_id = %v, want %q", body["operation_id"], id)
	}
}

func TestGetOperation_DoesNotRequireTheDestructiveGate(t *testing.T) {
	backend := newSyncFakeBackend()
	backend.ops["op_1"] = service.Operation{ID: "op_1", Status: "completed"}
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          NotYetImplementedGate{}, // gate NOT passed
		BinaryVersion: "test",
		Commit:        "test",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/operations/op_1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reading an operation's status must not require the destructive gate: status = %d, body: %s", rec.Code, rec.Body.String())
	}
}
