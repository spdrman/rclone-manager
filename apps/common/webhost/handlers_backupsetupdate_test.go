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

// The sparse edit of a backup set, where the whole contract is which
// fields cross the seam.
//
// Two assertions carry that. Only the fields present in the body may reach
// the service, so a UI saving one box cannot silently rewrite the rest of
// the set with whatever it happened to be holding; and a zero in the body
// is not the same as an absent key, which on a timeout or an interval is
// the difference between "no limit" and "leave the limit alone".
//
// The seconds-to-duration case pins the unit conversion at the boundary.
// A number on the wire with no unit is a bug waiting for somebody to read
// it as the other one.

func patchBackupSet(t *testing.T, router http.Handler, id, body string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/backup-sets/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		attachValidCSRF(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// seedSet puts one backup set in the fake so a PATCH has something to
// edit, which is the shape the real service is in too: this route never
// creates.
func seedSet(t *testing.T, tr backupSetsTestRouter, id string) {
	t.Helper()
	rec := postBackupSet(t, tr.router, validCreateBody, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seeding a backup set: status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if _, err := tr.backend.GetBackupSet(t.Context(), id); err != nil {
		t.Fatalf("seeded set %q is not readable back: %v", id, err)
	}
}

// TestUpdateBackupSet_Success_Returns200WithTheUpdatedSet is the route's
// own request/response contract: PATCH the one thing that changed, get
// back the whole persisted set, at 200 rather than 201 (this route edits
// a resource, it never creates one).
func TestUpdateBackupSet_Success_Returns200WithTheUpdatedSet(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"remote_path":"/backups/moved"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["id"] != "api/postgres-primary" {
		t.Errorf("id = %v, want %q", body["id"], "api/postgres-primary")
	}
	if body["remote_path"] != "/backups/moved" {
		t.Errorf("remote_path = %v, want %q", body["remote_path"], "/backups/moved")
	}
}

// TestUpdateBackupSet_OnlyTheFieldsInTheBodyCrossTheSeam is the HTTP half
// of "a per-box Save writes only that box". A key absent from the JSON
// must reach core/service as a nil pointer, not as a zero value, or the
// sparse semantics the service was built with are undone one layer up and
// saving one box would ship every other box's current contents with it.
func TestUpdateBackupSet_OnlyTheFieldsInTheBodyCrossTheSeam(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"remote_path":"/backups/moved"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}

	got := tr.backend.lastUpdate()
	if got.RemotePath == nil || *got.RemotePath != "/backups/moved" {
		t.Errorf("RemotePath = %v, want a pointer to %q", got.RemotePath, "/backups/moved")
	}
	for name, isSet := range map[string]bool{
		"Host":               got.Host != nil,
		"Port":               got.Port != nil,
		"User":               got.User != nil,
		"LocalPath":          got.LocalPath != nil,
		"Include":            got.Include != nil,
		"CompletionStrategy": got.CompletionStrategy != nil,
		"StableFor":          got.StableFor != nil,
		"StaleAfter":         got.StaleAfter != nil,
		"ValidatorID":        got.ValidatorID != nil,
	} {
		if isSet {
			t.Errorf("%s crossed the seam as set, but the request body never mentioned it", name)
		}
	}
}

// TestUpdateBackupSet_AZeroInTheBodyIsNotTheSameAsAnAbsentKey is the
// other half of the same property, and the one a naive value-typed
// request struct gets wrong: port 0 is a real, meaningful value (it
// selects the default port), so "port": 0 has to arrive as a pointer to
// zero and not be indistinguishable from a body that said nothing about
// the port at all.
func TestUpdateBackupSet_AZeroInTheBodyIsNotTheSameAsAnAbsentKey(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	if rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"port":0}`, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	explicit := tr.backend.lastUpdate()
	if explicit.Port == nil {
		t.Fatal(`"port": 0 arrived as an absent field; a zero an operator typed must be distinguishable from a field they never touched`)
	}
	if *explicit.Port != 0 {
		t.Errorf("Port = %d, want 0", *explicit.Port)
	}

	if rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"user":"someone"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if absent := tr.backend.lastUpdate(); absent.Port != nil {
		t.Errorf("Port = %v for a body that never named it, want nil", *absent.Port)
	}
}

// TestUpdateBackupSet_SecondsBecomeDurations pins the same wire
// convention createBackupSet already uses: the JSON carries seconds as
// plain numbers and the HTTP layer is the one place they become a
// time.Duration.
func TestUpdateBackupSet_SecondsBecomeDurations(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := patchBackupSet(t, tr.router, "api/postgres-primary",
		`{"completion_strategy":"stable","stable_for_seconds":90,"stale_after_seconds":3600}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	got := tr.backend.lastUpdate()
	if got.StableFor == nil || got.StableFor.Seconds() != 90 {
		t.Errorf("StableFor = %v, want 90s", got.StableFor)
	}
	if got.StaleAfter == nil || got.StaleAfter.Hours() != 1 {
		t.Errorf("StaleAfter = %v, want 1h", got.StaleAfter)
	}
}

// TestUpdateBackupSet_UnknownSetIs404 keeps the vocabulary the read and
// toggle routes already established, so a client does not have to learn a
// second code for the same condition.
func TestUpdateBackupSet_UnknownSetIs404(t *testing.T) {
	tr := newBackupSetsTestRouter(t)

	rec := patchBackupSet(t, tr.router, "api/nope", `{"remote_path":"/x"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "BACKUP_SET_NOT_FOUND" {
		t.Errorf("code = %q, want %q", code, "BACKUP_SET_NOT_FOUND")
	}
}

// TestUpdateBackupSet_InvalidRequestIs400WithTheReason: a refusal a UI
// has to render beside the box the operator was typing in needs to say
// what was wrong, and service.ErrInvalidRequest's text is safe to echo
// (writeBackupSetError's own doc records why).
func TestUpdateBackupSet_InvalidRequestIs400WithTheReason(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")
	tr.backend.errOnUpdate = service.ErrInvalidRequest

	rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"remote_path":"relative"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want %q", code, "INVALID_REQUEST")
	}
}

// TestUpdateBackupSet_MalformedBodyIs400 uses the shared decode-error
// mapping rather than a third spelling of it.
func TestUpdateBackupSet_MalformedBodyIs400(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"remote_path":`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestUpdateBackupSet_WithoutCSRFIs403: this is a write, so it carries
// requireCSRF like every other write in this package. The gate walk in
// router_test.go covers this generically; this case pins it directly, at
// the route, so the reason a future refactor broke it is legible.
func TestUpdateBackupSet_WithoutCSRFIs403(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"remote_path":"/x"}`, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

// TestUpdateBackupSet_IsNotBehindTheDestructiveGate is the issue's own
// correction, worth a test rather than a comment: POST /backup-sets
// carries only requireCSRF, the destructive gate applies to
// run_immediately, and editing follows creation's precedent. A gated
// edit route would mean an operator who has not turned destructive
// operations on cannot fix a typo'd remote path, which is the opposite of
// what the gate protects.
func TestUpdateBackupSet_IsNotBehindTheDestructiveGate(t *testing.T) {
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

	rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"remote_path":"/backups/moved"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d with the destructive gate closed, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestUpdateBackupSet_RepointRefusalIs409WithItsOwnCode: a client has to
// be able to tell "this edit is malformed" from "this edit is fine but it
// moves the set to different data and needs saying so". Both would be a
// 400 INVALID_REQUEST under one code, and the Web UI could then offer an
// operator nothing better than the same failure again.
func TestUpdateBackupSet_RepointRefusalIs409WithItsOwnCode(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")
	tr.backend.errOnUpdate = fmt.Errorf("%w: this would move local_path from %q to %q while 40 artifact(s) are on record",
		service.ErrRepointNotAcknowledged, "/old", "/new")

	rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"local_path":"/new"}`, true)

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
	if body.Error.Code != "BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED" {
		t.Errorf("error code = %q, want BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED", body.Error.Code)
	}
	// The message is what the operator reads before deciding, so it has
	// to survive the hop rather than be replaced by a generic sentence.
	for _, want := range []string{"local_path", "/old", "/new", "40"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Errorf("the message does not carry %q: %s", want, body.Error.Message)
		}
	}
}

// TestUpdateBackupSet_AcknowledgementCrossesTheSeam: the acknowledgement
// is only worth anything if it actually reaches core, and a handler that
// decoded it and dropped it would produce an identical 200 on the retry
// while core went on refusing.
func TestUpdateBackupSet_AcknowledgementCrossesTheSeam(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"local_path":"/new","acknowledge_repoint":true}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !tr.backend.lastUpdate().AcknowledgeRepoint {
		t.Error("AcknowledgeRepoint did not reach the service request")
	}

	// And the control: a body that does not mention it must not arrive
	// as an acknowledgement, or every edit would be pre-acknowledged.
	if rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"local_path":"/other"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if tr.backend.lastUpdate().AcknowledgeRepoint {
		t.Error("a body that never mentioned acknowledge_repoint arrived as an acknowledgement")
	}
}
