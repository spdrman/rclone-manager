package webhost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/service"
)

// Creating a backup set, including the branch where creating it also
// starts work.
//
// run_immediately is what makes this route interesting. Persisting a set
// touches no backup data and is not gated; the same call with that flag
// set also starts a run cycle, which is exactly what the gate exists to
// block, so the check is on the branch rather than on the route. The cases
// here pin both directions, including the one where the set is created
// disabled and must never run even though a run was requested.
//
// The partial-failure case is the other one to read: a create that
// succeeds followed by a run that fails is still a successful create, and
// answering it as an error would leave an operator with a set they were
// told did not exist.

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

// TestCreateBackupSet_AnUnprovenConnectionIsRefusedAsDeclared is PR
// #628's review finding on this route: the service proves a create's
// connection itself now, and refuses with the same sentinel an unprovable
// edit already gets, so this handler has to answer the same 409 for it
// and the contract has to declare that 409 for this operation. A refusal
// a client is never told about is one it cannot handle, and this is the
// one a third-party client that never ran the candidate check will meet
// first.
func TestCreateBackupSet_AnUnprovenConnectionIsRefusedAsDeclared(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.errOnCreate = fmt.Errorf("%w: nothing answered TCP on 127.0.0.1:2222: connection refused", service.ErrConnectionNotProven)

	rec := postBackupSet(t, tr.router, validCreateBody, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	code := errorCodeOf(t, rec)
	if code != "BACKUP_SET_CONNECTION_NOT_PROVEN" {
		t.Fatalf("code = %q, want BACKUP_SET_CONNECTION_NOT_PROVEN", code)
	}
	declared := contractEndpoints()["createBackupSet"].ErrorCodes[http.StatusConflict]
	found := false
	for _, c := range declared {
		if string(c) == code {
			found = true
		}
	}
	if !found {
		t.Errorf("the handler returned 409 %q, which api/v1/openapi.json does not declare for createBackupSet at that status (it declares %v)", code, declared)
	}
}

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

// TestCreateBackupSet_RunImmediately_CreateSucceedsButRunFails_Returns201WithRunError
// is the mandatory review's M6 finding (PR #155): a successful create
// with a failed immediate run must not collapse to a bare 500 as if
// creation itself had failed — the backup set IS already durably
// persisted at that point (service.CreateBackupSet's own doc says so
// explicitly). Proven two ways: the response is 201 with run_error set
// and no operation field, AND the set is actually there afterward (a
// follow-up GET finds it) — not merely claimed in a response a retry
// would otherwise contradict.
func TestCreateBackupSet_RunImmediately_CreateSucceedsButRunFails_Returns201WithRunError(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.errOnSubmit = errBoom
	body := strings.TrimSuffix(strings.TrimSpace(validCreateBody), "}") + `,"run_immediately":true}`
	rec := postBackupSet(t, tr.router, body, true)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var respBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if respBody["id"] != "api/postgres-primary" {
		t.Errorf("id = %v, want %q (the set must be reported as created)", respBody["id"], "api/postgres-primary")
	}
	if respBody["run_error"] == "" || respBody["run_error"] == nil {
		t.Error("run_error is missing/empty; the caller must be told the requested run failed to start")
	}
	if _, hasOperation := respBody["operation"]; hasOperation {
		t.Errorf("operation present = %v, want absent (the run never actually started)", respBody["operation"])
	}

	// The set is really persisted, not just claimed in this response: a
	// follow-up GET finds it.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/backup-sets/api/postgres-primary", nil)
	rec2 := httptest.NewRecorder()
	tr.router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET after create-succeeded-run-failed: status = %d, want %d, body: %s", rec2.Code, http.StatusOK, rec2.Body.String())
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

// TestCreateBackupSet_HistoryRepointRefusalIs409WithItsOwnCode: a create
// over an id that already has artifacts on record, pointed somewhere
// other than where they came from, is not a malformed request. It is a
// well-formed one whose consequences the caller has to see first, and it
// conflicts with the state of the id rather than with its own shape, so
// it is a 409 under its own code. Under 400 INVALID_REQUEST the wizard
// could offer an operator nothing better than the same failure again, and
// under the EDIT path's code it could not tell which of the two buttons
// to offer.
func TestCreateBackupSet_HistoryRepointRefusalIs409WithItsOwnCode(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	tr.backend.errOnCreate = fmt.Errorf("%w: 40 artifact(s) are already on record for api/postgres-primary, and this creates it elsewhere: local_path on record as %q, requested as %q",
		service.ErrHistoryRepointNotAcknowledged, "/old", "/new")

	rec := postBackupSet(t, tr.router, validCreateBody, true)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Code != "BACKUP_SET_HISTORY_REPOINT_NOT_ACKNOWLEDGED" {
		t.Errorf("error code = %q, want BACKUP_SET_HISTORY_REPOINT_NOT_ACKNOWLEDGED", body.Error.Code)
	}
	// What the operator reads before deciding has to survive the hop
	// rather than be replaced by a generic sentence.
	for _, want := range []string{"local_path", "/old", "/new", "40"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Errorf("the message does not carry %q: %s", want, body.Error.Message)
		}
	}
}

// TestCreateBackupSet_AcknowledgementCrossesTheSeam: the acknowledgement
// is only worth anything if it reaches core. A handler that decoded it
// and dropped it would produce an identical refusal on the retry, with
// the operator having answered the question and got nowhere.
func TestCreateBackupSet_AcknowledgementCrossesTheSeam(t *testing.T) {
	tr := newBackupSetsTestRouter(t)

	acknowledged := strings.TrimSuffix(strings.TrimSpace(validCreateBody), "}") + `, "acknowledge_repoint": true}`
	if rec := postBackupSet(t, tr.router, acknowledged, true); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if !tr.backend.lastCreate.AcknowledgeRepoint {
		t.Error("AcknowledgeRepoint did not reach the service request")
	}

	// The control: a body that never mentions it must not arrive as an
	// acknowledgement, or every create would be pre-acknowledged and the
	// refusal could never fire at all.
	if rec := postBackupSet(t, tr.router, validCreateBody, true); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if tr.backend.lastCreate.AcknowledgeRepoint {
		t.Error("a body that never mentioned acknowledge_repoint arrived as an acknowledgement")
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

// TestCreateBackupSet_RunImmediately_GateNotPassed_PersistsTheSetAndReportsTheRefusal
// keeps M3's property (PR #155) and drops the half of it that cost an
// operator their whole submission (issue #597).
//
// M3's finding is still enforced: request.run_immediately turns a plain
// create into "also start a run_cycle", the exact destructive action
// requireDestructiveGate exists to block, so with the gate shut NOTHING
// runs. No operation comes back, and the backend is never asked to start
// one.
//
// What changed is what happens to the create. This route is deliberately
// exempt from requireDestructiveGate for a plain create, because creating
// a backup set touches no backup data (destructiveGateExemptRoutes'
// justification, router_test.go). Refusing the whole call therefore
// refused a non-destructive action for a destructive one's reason: the
// operator lost an entire wizard submission and was told "Could not save
// this backup set", which is not what happened. Retrying then hits
// config.Validate's duplicate-id rejection with no way to tell "already
// exists because your last attempt worked" from "your request was wrong
// from the start", which is M6's own argument in the same review, and it
// applies here word for word.
//
// So the answer is the 201-with-run_error shape M6 built and nothing
// could reach: the set exists, the run did not start, and the response
// says both. That path is exercised end to end by the client
// (client.test.ts) and by the wizard (wizard.test.tsx), and until this it
// was unreachable in production.
func TestCreateBackupSet_RunImmediately_GateNotPassed_PersistsTheSetAndReportsTheRefusal(t *testing.T) {
	backend := newBackupSetFakeBackend()
	router := NewRouter(RouterConfig{
		Platform: allowingPlatform("alice"),
		Backend:  backend,
		// Omitted, not NotYetImplementedGate{} spelled out: this is
		// literally how every shipped binary builds its config, and the
		// omission default is what makes the refusal below the answer a
		// real deployment gives rather than one this test arranged.
		Gate:          nil,
		BinaryVersion: "test",
		Commit:        "test",
	})
	body := strings.TrimSuffix(strings.TrimSpace(validCreateBody), "}") + `,"run_immediately":true}`
	rec := postBackupSet(t, router, body, true)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var respBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// The run did not start, and the response says so in the field built
	// for exactly that rather than by leaving the client to infer it.
	runError, _ := respBody["run_error"].(string)
	if runError == "" {
		t.Error("run_error is empty; the operator asked for a run that did not happen and nothing in the response says so")
	}
	if !strings.Contains(runError, "destructive operations are disabled") {
		t.Errorf("run_error = %q, want the gate's own sentence: a client that cannot tell WHY the run did not start cannot say anything useful about it", runError)
	}
	if respBody["operation"] != nil {
		t.Errorf("operation = %v, want absent; the gate is shut, so no run may have been started", respBody["operation"])
	}
	// The set itself IS persisted, which is the half that changed.
	if len(backend.sets) != 1 {
		t.Errorf("backend.sets = %v, want exactly the one that was submitted; a refused RUN must not discard the create", backend.sets)
	}
	// And the backend was never asked to run anything, asserted at the
	// seam rather than off the response: proving the answer is honest is
	// not the same as proving nothing ran, and a handler that started the
	// run and then hid it would produce an identical body.
	if backend.lastCreate.RunImmediately {
		t.Error("RunImmediately crossed the HTTP-to-core seam as true even though the destructive gate is shut, so the run was actually started")
	}
}

// TestCreateBackupSet_PlainCreateSucceedsEvenWhenGateNotPassed pins the
// other half of M3's fix: a create that does NOT set run_immediately
// must stay unaffected by whether #92's destructive gate has been
// verified yet — the whole reason this route is exempt from
// requireDestructiveGate at the route level in the first place
// (destructiveGateExemptRoutes' own justification, router_test.go: a
// plain create never touches remote or local backup data).
func TestCreateBackupSet_PlainCreateSucceedsEvenWhenGateNotPassed(t *testing.T) {
	backend := newBackupSetFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          NotYetImplementedGate{}, // gate NOT passed
		BinaryVersion: "test",
		Commit:        "test",
	})
	rec := postBackupSet(t, router, validCreateBody, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
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

// TestCreateBackupSet_CarriesValidatorIDThroughToTheService is issue
// #162's HTTP-side wiring proof: validator_id off the wire reaches
// service.CreateBackupSetRequest.ValidatorID unchanged, and comes back on
// the 201 so a UI can render what it just saved without a second fetch.
func TestCreateBackupSet_CarriesValidatorIDThroughToTheService(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	body := strings.Replace(validCreateBody,
		`"completion_strategy": "marker"`,
		`"completion_strategy": "marker",
	"validator_id": "trailer-marker"`, 1)

	rec := postBackupSet(t, tr.router, body, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if got := string(tr.backend.lastCreate.ValidatorID); got != "trailer-marker" {
		t.Errorf("service.CreateBackupSetRequest.ValidatorID = %q, want %q", got, "trailer-marker")
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshalling the response: %v", err)
	}
	if resp["validator_id"] != "trailer-marker" {
		t.Errorf("response validator_id = %v, want %q", resp["validator_id"], "trailer-marker")
	}
}

// TestCreateBackupSet_WithoutAValidatorSendsNone is the control for the
// test above: an omitted validator_id must reach the service as the empty
// id (no validator), never as some default this layer invented.
func TestCreateBackupSet_WithoutAValidatorSendsNone(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postBackupSet(t, tr.router, validCreateBody, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if tr.backend.lastCreate.ValidatorID != "" {
		t.Errorf("service.CreateBackupSetRequest.ValidatorID = %q, want empty", tr.backend.lastCreate.ValidatorID)
	}
}

// TestCreateBackupSet_ReadOnly_CarriesThroughToTheServiceAndTheResponse is
// issue #316's RED case at the HTTP layer: before this, backupSetSpec had
// no read_only field at all, so a wizard/API request had no way to ask for
// a read-only backup set — POST /api/v1/backup-sets silently ignored
// anything named "read_only" in the body. Mirrors
// TestCreateBackupSet_CarriesValidatorIDThroughToTheService above.
func TestCreateBackupSet_ReadOnly_CarriesThroughToTheServiceAndTheResponse(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	body := strings.Replace(validCreateBody,
		`"completion_strategy": "marker"`,
		`"completion_strategy": "marker",
	"read_only": true`, 1)

	rec := postBackupSet(t, tr.router, body, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if !tr.backend.lastCreate.ReadOnly {
		t.Error("service.CreateBackupSetRequest.ReadOnly = false, want true")
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshalling the response: %v", err)
	}
	if resp["read_only"] != true {
		t.Errorf("response read_only = %v, want true", resp["read_only"])
	}
}

// TestCreateBackupSet_WithoutReadOnlySendsFalse is the control for the
// test above: an omitted read_only must reach the service as false,
// exactly what every request before this issue already meant, never a
// default this layer invented.
func TestCreateBackupSet_WithoutReadOnlySendsFalse(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postBackupSet(t, tr.router, validCreateBody, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if tr.backend.lastCreate.ReadOnly {
		t.Error("service.CreateBackupSetRequest.ReadOnly = true, want false")
	}
}
