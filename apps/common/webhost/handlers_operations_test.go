package webhost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/csrf"
	"github.com/spdrman/rclone-manager/core/service"
)

// POST /api/v1/operations and the reads beside it.
//
// The idempotency cases are the ones carrying real weight. A duplicate key
// has to return the same operation rather than a second one, and a missing
// key has to be refused rather than defaulted, because the alternative to
// both is a retried submit that starts a second run cycle over the same
// data. The stale-revision case is the same idea for configuration: a
// client acting on an old picture is refused, and the refusal carries the
// current revision as a field so the client has something to retry
// against without parsing prose.
//
// Every case goes through the real router rather than calling the handler
// directly, so the CSRF and gate middleware in front of the route are part
// of what is being tested.

// testCSRFToken is an arbitrary, fixed value every test in this file uses
// for both the CSRF cookie and header on a submitted request: requireCSRF
// (csrf.go) only ever checks that the two match each other, never any
// specific value, so any fixed string both sides agree on exercises the
// real check exactly as a real client's own randomly-issued token would.
const testCSRFToken = "test-csrf-token"

// attachValidCSRF adds a matching CSRF cookie/header pair to req, as if a
// real client had already loaded a page from this origin and echoed back
// the cookie EnsureCookie/EnsureCSRFCookie issued it. Every test in this
// file that submits a mutating request goes through this (via
// submitOperation) now that POST /api/v1/operations enforces CSRF - see
// TestSubmitOperation_MissingCSRFCookieReturns403 for the dedicated proof
// that the check itself actually rejects a request without this.
func attachValidCSRF(req *http.Request) {
	req.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: testCSRFToken})
	req.Header.Set(csrf.HeaderName, testCSRFToken)
}

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
	attachValidCSRF(req)
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

// TestSubmitOperation_DuplicateIdempotencyKeyOverHTTPReturnsTheSameOperation
// is the RED plan's "two requests with the same idempotency key produce
// one operation record, not two" made concrete at the actual HTTP
// endpoint: two real POST requests through the router (not a single call
// into the backend), same Idempotency-Key header, must resolve to the
// same operation_id.
func TestSubmitOperation_DuplicateIdempotencyKeyOverHTTPReturnsTheSameOperation(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	body := `{"action":"run_cycle","config_revision":"rev-1"}`

	first := submitOperation(t, tr.router, "idem-shared", body)
	second := submitOperation(t, tr.router, "idem-shared", body)

	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("status codes = %d, %d, want both %d", first.Code, second.Code, http.StatusAccepted)
	}

	var firstBody, secondBody map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatalf("decode second response: %v", err)
	}

	if firstBody["operation_id"] != secondBody["operation_id"] {
		t.Errorf("operation_id = %v then %v, want the same id for a resubmitted idempotency key", firstBody["operation_id"], secondBody["operation_id"])
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
	// issue #118 item 5: the current config_revision must also be a
	// structured, top-level field on this exact response, not only
	// embedded in error.message's prose.
	if body["config_revision"] != tr.backend.ConfigRevision() {
		t.Errorf("config_revision = %v, want %q", body["config_revision"], tr.backend.ConfigRevision())
	}
}

// TestSubmitOperation_IdempotencyKeyConflictReturns409WithItsOwnCode is
// issue #118 item 10: reusing an idempotency key for a different logical
// request must map to its own machine-readable code, distinct from
// INVALID_REQUEST, so a client can tell "fix your JSON" apart from "you
// need a different idempotency key".
func TestSubmitOperation_IdempotencyKeyConflictReturns409WithItsOwnCode(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.errOnSubmit = fmt.Errorf("%w: idempotency key already used for a different request", service.ErrIdempotencyKeyConflict)

	rec := submitOperation(t, tr.router, "idem-1", `{"action":"run_cycle","config_revision":"rev-1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "IDEMPOTENCY_KEY_CONFLICT" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "IDEMPOTENCY_KEY_CONFLICT")
	}
}

// TestSubmitOperation_OperationAlreadyRunningReturns409WithItsOwnCode is
// issue #118 item 1's HTTP-layer half: a rejected "another run_cycle is
// already in progress" submission maps to its own code, distinct from
// both CONFIG_REVISION_STALE and IDEMPOTENCY_KEY_CONFLICT, so a client
// that understands this code specifically knows to retry later with a
// fresh idempotency key rather than assuming its request itself was
// wrong.
func TestSubmitOperation_OperationAlreadyRunningReturns409WithItsOwnCode(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.errOnSubmit = fmt.Errorf("%w: rejected: another run_cycle operation is already in progress", service.ErrOperationAlreadyRunning)

	rec := submitOperation(t, tr.router, "idem-1", `{"action":"run_cycle","config_revision":"rev-1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "OPERATION_ALREADY_RUNNING" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "OPERATION_ALREADY_RUNNING")
	}
}

// TestSubmitOperation_BodyExceedingSizeLimitReturns400 is issue #118 item
// 14: §17 requires enforcing request-size limits, and this proves it is
// actually wired up, not just declared in a constant nothing reads.
func TestSubmitOperation_BodyExceedingSizeLimitReturns400(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})

	oversized := `{"action":"run_cycle","config_revision":"` + strings.Repeat("x", maxSubmitOperationBodyBytes+1) + `"}`
	rec := submitOperation(t, tr.router, "idem-1", oversized)
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
}

// TestSubmitOperation_InvalidRequestFromBackendReturns400 proves the
// second half of the error-mapping switch in handlers_operations.go: a
// core/service.ErrInvalidRequest is safe to report back to the client
// (its message is always one of core/service's own generic strings), so
// it maps to 400, not 500.
func TestSubmitOperation_InvalidRequestFromBackendReturns400(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.errOnSubmit = fmt.Errorf("%w: run_cycle request requires a configuration revision", service.ErrInvalidRequest)

	rec := submitOperation(t, tr.router, "idem-1", `{"action":"run_cycle","config_revision":"rev-1"}`)
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
}

// TestSubmitOperation_UnclassifiedBackendErrorReturns500WithoutLeakingDetails
// is the acceptance criterion "API exposes no rclone/SQLite implementation
// types" applied to an ERROR path, not just a success response: an
// unclassified error from the backend (in production, potentially
// wrapping a raw state-layer/SQLite failure) must never have its message
// echoed back to the client.
func TestSubmitOperation_UnclassifiedBackendErrorReturns500WithoutLeakingDetails(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	tr.backend.errOnSubmit = errBoom // deliberately NOT wrapped in a recognised sentinel

	rec := submitOperation(t, tr.router, "idem-1", `{"action":"run_cycle","config_revision":"rev-1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), errBoom.Error()) {
		t.Errorf("response leaked the raw backend error text: %s", rec.Body.String())
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

// TestSubmitOperation_MissingCSRFCookieReturns403 is issue #119's review
// finding that POST /api/v1/operations had no CSRF check of its own at
// all, made concrete: a real, authenticated, gate-passed request with no
// CSRF cookie must still be refused. Every other test in this file goes
// through submitOperation, which attaches a valid CSRF cookie/header pair
// via attachValidCSRF - this one deliberately builds its own request
// without that, to prove the check is actually wired in, not merely
// present in the source and unreachable.
func TestSubmitOperation_MissingCSRFCookieReturns403(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(`{"action":"run_cycle","config_revision":"rev-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "idem-no-csrf")
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "CSRF_TOKEN_MISSING" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "CSRF_TOKEN_MISSING")
	}
}

// TestSubmitOperation_MismatchedCSRFHeaderReturns403 covers the other
// half of requireCSRF: a cookie is present, but the echoed header doesn't
// match it (a cross-site attacker can make a victim's browser send the
// cookie, but cannot read its value to construct a matching header - see
// apps/common/csrf's own doc for why that's the entire defense).
func TestSubmitOperation_MismatchedCSRFHeaderReturns403(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(`{"action":"run_cycle","config_revision":"rev-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "idem-bad-csrf")
	req.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: testCSRFToken})
	req.Header.Set(csrf.HeaderName, "a-completely-different-value")
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "CSRF_TOKEN_MISMATCH" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "CSRF_TOKEN_MISMATCH")
	}
}

// TestSubmitOperation_CSRFIsCheckedEvenWhenTheGateWouldAlsoRefuse proves
// CSRF verification isn't accidentally short-circuited by the destructive
// gate: a request that would fail BOTH checks must be reported as the
// CSRF failure, matching requireCSRF's position first in the middleware
// chain (router.go) - a caller must learn its request is fundamentally
// forgeable-looking before learning anything about server-side
// authorization state, the same ordering principle
// TestSubmitOperation_GateIsCheckedAfterAuthentication already pins down
// for auth vs. the gate.
func TestSubmitOperation_CSRFIsCheckedEvenWhenTheGateWouldAlsoRefuse(t *testing.T) {
	tr := newOperationsTestRouter(t, NotYetImplementedGate{}) // gate NOT passed

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(`{"action":"run_cycle","config_revision":"rev-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "idem-no-csrf-no-gate")
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "CSRF_TOKEN_MISSING" {
		t.Errorf("error.code = %v, want %q (CSRF must be checked before the destructive gate)", errObj["code"], "CSRF_TOKEN_MISSING")
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
