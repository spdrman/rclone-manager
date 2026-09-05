// The edit hold, and mostly the null.
//
// Three of the cases here assert that nothing running is reported as an
// absent object rather than an empty one, on both the read and the take.
// That is not a serialisation nicety: a client renders the warning when
// the field is present, so a zero-valued object warns an operator about
// interrupting work that is not happening, and an operator warned about
// nothing stops reading warnings.
//
// The naming case covers the other half. When something IS running, the
// warning has to say which artifact and which stage, because discarding a
// partial transfer and cancelling a cycle that has not picked a file yet
// cost very different amounts.
package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

func editHoldRequest(t *testing.T, router http.Handler, method, path string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		attachValidCSRF(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

const editHoldPath = "/api/v1/backup-sets/api/postgres-primary/edit-hold"

func decodeEditHold(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return body
}

// TestGetBackupSetEditHold_ReportsNothingRunningWhenNothingIs is the
// route behind "entering edit mode is silent when nothing is running":
// the client asks this first, and a null "running" is what tells it to
// open edit mode with no prompt.
func TestGetBackupSetEditHold_ReportsNothingRunningWhenNothingIs(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := editHoldRequest(t, tr.router, http.MethodGet, editHoldPath, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeEditHold(t, rec)
	if body["running"] != nil {
		t.Errorf("running = %v with nothing in flight, want null", body["running"])
	}
	if body["held"] != false {
		t.Errorf("held = %v before anything took a hold, want false", body["held"])
	}
}

// TestGetBackupSetEditHold_NamesTheArtifactAndStage: the warning's whole
// content has to reach the client, or the prompt is the bare "are you
// sure" the issue rules out.
func TestGetBackupSetEditHold_NamesTheArtifactAndStage(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")
	tr.backend.running = &service.RunningWork{Artifact: "backup.dump", Stage: "transferring"}

	rec := editHoldRequest(t, tr.router, http.MethodGet, editHoldPath, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	running, ok := decodeEditHold(t, rec)["running"].(map[string]any)
	if !ok {
		t.Fatalf("running missing or wrong shape: %s", rec.Body.String())
	}
	if running["artifact"] != "backup.dump" {
		t.Errorf("running.artifact = %v, want %q", running["artifact"], "backup.dump")
	}
	if running["stage"] != "transferring" {
		t.Errorf("running.stage = %v, want %q", running["stage"], "transferring")
	}
}

// TestGetBackupSetEditHold_IsReadOnly: it takes no hold and needs no
// CSRF, exactly like every other GET in this package. A read that
// silently took a hold would pause backups for a set an operator only
// looked at.
func TestGetBackupSetEditHold_IsReadOnly(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	if rec := editHoldRequest(t, tr.router, http.MethodGet, editHoldPath, false); rec.Code != http.StatusOK {
		t.Fatalf("a GET without CSRF got %d, want 200: this route is read-only", rec.Code)
	}
	if tr.backend.beginCalls() != 0 {
		t.Errorf("the read took %d hold(s); it must take none", tr.backend.beginCalls())
	}
}

// TestPostBackupSetEditHold_TakesTheHoldAndReportsWhatItStopped is the
// confirm step: an operator who has seen the warning and accepted it
// posts here, and the answer says what was actually interrupted.
func TestPostBackupSetEditHold_TakesTheHoldAndReportsWhatItStopped(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")
	tr.backend.running = &service.RunningWork{Artifact: "backup.dump", Stage: "transferring"}

	rec := editHoldRequest(t, tr.router, http.MethodPost, editHoldPath, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeEditHold(t, rec)
	if body["expires_at"] == nil || body["expires_at"] == "" {
		t.Errorf("expires_at is missing; a client that cannot see the lease cannot renew it: %s", rec.Body.String())
	}
	stopped, ok := body["stopped"].(map[string]any)
	if !ok {
		t.Fatalf("stopped missing or wrong shape: %s", rec.Body.String())
	}
	if stopped["artifact"] != "backup.dump" {
		t.Errorf("stopped.artifact = %v, want %q", stopped["artifact"], "backup.dump")
	}
	if tr.backend.beginCalls() != 1 {
		t.Errorf("BeginBackupSetEdit was called %d time(s), want 1", tr.backend.beginCalls())
	}
}

// TestPostBackupSetEditHold_OmitsStoppedWhenNothingWasRunning: claiming
// to have stopped something on every Edit press is how an operator stops
// reading the message.
func TestPostBackupSetEditHold_OmitsStoppedWhenNothingWasRunning(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := editHoldRequest(t, tr.router, http.MethodPost, editHoldPath, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if got := decodeEditHold(t, rec)["stopped"]; got != nil {
		t.Errorf("stopped = %v with nothing running, want null", got)
	}
}

// TestPostBackupSetEditHoldRelease_ReleasesIt: leaving edit mode has to
// actually let backups resume, and this is the route every exit path
// calls.
func TestPostBackupSetEditHoldRelease_ReleasesIt(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	rec := editHoldRequest(t, tr.router, http.MethodPost, editHoldPath+"/release", true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if tr.backend.endCalls() != 1 {
		t.Errorf("EndBackupSetEdit was called %d time(s), want 1", tr.backend.endCalls())
	}
}

// TestBackupSetEditHoldWrites_RequireCSRF: both writes are writes.
func TestBackupSetEditHoldWrites_RequireCSRF(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	for _, path := range []string{editHoldPath, editHoldPath + "/release"} {
		if rec := editHoldRequest(t, tr.router, http.MethodPost, path, false); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF got %d, want 403", path, rec.Code)
		}
	}
}

// TestBackupSetEditHold_UnknownSetIs404 across all three routes, with the
// same code the rest of the backup-set surface uses.
func TestBackupSetEditHold_UnknownSetIs404(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	const missing = "/api/v1/backup-sets/api/nope/edit-hold"

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, missing},
		{http.MethodPost, missing},
		{http.MethodPost, missing + "/release"},
	} {
		rec := editHoldRequest(t, tr.router, tc.method, tc.path, true)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404, body: %s", tc.method, tc.path, rec.Code, rec.Body.String())
			continue
		}
		if code := errorCodeOf(t, rec); code != "BACKUP_SET_NOT_FOUND" {
			t.Errorf("%s %s: code = %q, want BACKUP_SET_NOT_FOUND", tc.method, tc.path, code)
		}
	}
}

// TestBackupSetEditHold_IsNotBehindTheDestructiveGate: holding a set
// stops work rather than starting or deleting any, and gating it would
// mean an operator who has not turned destructive operations on cannot
// safely edit a set at all.
func TestBackupSetEditHold_IsNotBehindTheDestructiveGate(t *testing.T) {
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

	if rec := editHoldRequest(t, tr.router, http.MethodPost, editHoldPath, true); rec.Code != http.StatusOK {
		t.Fatalf("taking a hold with the destructive gate closed got %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
}
