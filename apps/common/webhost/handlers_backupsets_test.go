package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

type backupSetsTestRouter struct {
	router  http.Handler
	backend *backupSetFakeBackend
}

func newBackupSetsTestRouter(t *testing.T) backupSetsTestRouter {
	t.Helper()
	backend := newBackupSetFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	return backupSetsTestRouter{router: router, backend: backend}
}

func postBackupSet(t *testing.T, router http.Handler, body string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backup-sets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		attachValidCSRF(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

const validCreateBody = `{
	"name": "postgres-primary",
	"host": "prod-db-01.internal",
	"port": 22,
	"user": "backup-agent",
	"ssh_key_id": "key_test_1",
	"known_hosts_line": "prod-db-01.internal ssh-ed25519 AAAAfaketest",
	"remote_path": "/backups/postgresql",
	"local_path": "/data/backups/production/postgres",
	"include": ["*.dump.zst"],
	"completion_strategy": "marker"
}`

// TestCreateBackupSet_Success_Returns201WithBackupSetJSON is the RED
// plan's request/response contract case: a well-formed create request
// returns 201 with the persisted backup set's shape, not merely 2xx.
func TestCreateBackupSet_Success_Returns201WithBackupSetJSON(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postBackupSet(t, tr.router, validCreateBody, true)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["id"] != "api/postgres-primary" {
		t.Errorf("id = %v, want %q", body["id"], "api/postgres-primary")
	}
	if body["name"] != "postgres-primary" {
		t.Errorf("name = %v, want %q", body["name"], "postgres-primary")
	}
	if body["host"] != "prod-db-01.internal" {
		t.Errorf("host = %v, want %q", body["host"], "prod-db-01.internal")
	}
	if _, hasOperation := body["operation"]; hasOperation {
		t.Errorf("operation present = %v, want absent (run_immediately was not set)", body["operation"])
	}
}

// TestCreateBackupSet_RunImmediately_IncludesOperation is "Save, enable &
// run": the response carries the run_cycle operation it kicked off.
func TestCreateBackupSet_RunImmediately_IncludesOperation(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	withRun := strings.TrimSuffix(strings.TrimSpace(validCreateBody), "}") + `,"run_immediately":true}`
	rec := postBackupSet(t, tr.router, withRun, true)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	op, ok := body["operation"].(map[string]any)
	if !ok {
		t.Fatalf("operation missing or wrong shape in response: %v", body)
	}
	if op["operation_id"] == "" || op["operation_id"] == nil {
		t.Error("operation.operation_id is missing/empty")
	}
}

// TestCreateBackupSet_Disabled_NeverRunsEvenIfRequested is "Save
// disabled": run_immediately is ignored when disabled is true, matching
// service.CreateBackupSetRequest.RunImmediately's own doc.
func TestCreateBackupSet_Disabled_NeverRunsEvenIfRequested(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	body := strings.TrimSuffix(strings.TrimSpace(validCreateBody), "}") + `,"disabled":true,"run_immediately":true}`
	rec := postBackupSet(t, tr.router, body, true)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["disabled"] != true {
		t.Errorf("disabled = %v, want true", resp["disabled"])
	}
	if _, hasOperation := resp["operation"]; hasOperation {
		t.Errorf("operation present = %v, want absent (disabled sets never auto-run)", resp["operation"])
	}
}

func TestCreateBackupSet_MalformedJSONReturns400(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postBackupSet(t, tr.router, `{not json`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestCreateBackupSet_InvalidRequestFromBackendReturns400 proves
// service.ErrInvalidRequest (a missing field, a bad completion strategy,
// config.Validate's own rejection, ...) maps to 400 INVALID_REQUEST, with
// the backend's own message echoed (service's own contract: that message
// is always safe to show).
func TestCreateBackupSet_InvalidRequestFromBackendReturns400(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.errOnCreate = service.ErrInvalidRequest
	rec := postBackupSet(t, tr.router, validCreateBody, true)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "INVALID_REQUEST")
	}
}

func TestCreateBackupSet_SSHKeyNotFoundReturns400WithItsOwnCode(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.errOnCreate = service.ErrSSHKeyNotFound
	rec := postBackupSet(t, tr.router, validCreateBody, true)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "SSH_KEY_NOT_FOUND" {
		t.Errorf("error.code = %v, want %q", errObj["code"], "SSH_KEY_NOT_FOUND")
	}
}

func TestCreateBackupSet_UnclassifiedBackendErrorReturns500WithoutLeakingDetails(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.errOnCreate = errBoom
	rec := postBackupSet(t, tr.router, validCreateBody, true)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), errBoom.Error()) {
		t.Errorf("response leaked the unclassified backend error verbatim: %s", rec.Body.String())
	}
}

func TestCreateBackupSet_MissingCSRFCookieReturns403(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postBackupSet(t, tr.router, validCreateBody, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

// TestCreateBackupSet_RequiresAuthentication is this endpoint's own
// positive control alongside router_test.go's structural walk: proves a
// genuinely unauthenticated request is rejected, not merely that SOME
// route is (the walk already proves that broadly; this pins it to this
// specific one, the way every other handler test file in this package
// pins its own route too).
func TestCreateBackupSet_RequiresAuthentication(t *testing.T) {
	backend := newBackupSetFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	rec := postBackupSet(t, router, validCreateBody, true)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestListBackupSets_Success_ReturnsArray(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	postBackupSet(t, tr.router, validCreateBody, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup-sets", nil)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		BackupSets []map[string]any `json:"backup_sets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.BackupSets) != 1 {
		t.Fatalf("len(backup_sets) = %d, want 1, body: %s", len(body.BackupSets), rec.Body.String())
	}
	if body.BackupSets[0]["id"] != "api/postgres-primary" {
		t.Errorf("backup_sets[0].id = %v, want %q", body.BackupSets[0]["id"], "api/postgres-primary")
	}
}

func TestListBackupSets_DoesNotRequireCSRF(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup-sets", nil)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (a read should never need a CSRF token), body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestGetBackupSet_Success_ReturnsBackupSetJSON(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	postBackupSet(t, tr.router, validCreateBody, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup-sets/api/postgres-primary", nil)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["name"] != "postgres-primary" {
		t.Errorf("name = %v, want %q", body["name"], "postgres-primary")
	}
}

func TestGetBackupSet_UnknownIDReturns404(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup-sets/api/does-not-exist", nil)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}
