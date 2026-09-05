package webhost

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// removeRouterWith builds a router over one sync fake, authenticated and
// with the destructive gate open, so what a test observes is this route's
// own behaviour and not a refusal from something in front of it.
func removeRouterWith(backend *syncFakeBackend) http.Handler {
	return NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
}

func removeRequest(t *testing.T, router http.Handler, path string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if csrf {
		attachValidCSRF(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestRemoveBackupSet_AnswersNoContentAndAsksForTheWholeID covers the two
// things only the HTTP layer can get wrong: the status, and the id.
//
// The id in particular. This route keys on two named path segments and a
// backup set's identity is the pair joined with "/", so a handler that
// passed only one of them along would still answer 204 and would have
// removed the wrong thing, or nothing. Asserting what the backend was
// ASKED for is the only way to see that.
func TestRemoveBackupSet_AnswersNoContentAndAsksForTheWholeID(t *testing.T) {
	backend := newSyncFakeBackend()
	rec := removeRequest(t, removeRouterWith(backend), "/api/v1/backup-sets/production/postgres-primary", true)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("a 204 carried a body: %q", body)
	}
	if got := backend.lastRemoved; got != "production/postgres-primary" {
		t.Errorf("the backend was asked to remove %q, want %q", got, "production/postgres-primary")
	}
}

// TestRemoveBackupSet_UnknownSetIs404 pins the decision rather than
// leaving it to fall out of a verb table. The EFFECT of removing an
// already-removed set is idempotent and the STATUS is deliberately not:
// on a destructive control, a mistyped name reporting success is the
// exact defect issue #391 is about.
func TestRemoveBackupSet_UnknownSetIs404(t *testing.T) {
	backend := newSyncFakeBackend()
	backend.errOnRemove = fmt.Errorf("%w: production/gone", service.ErrBackupSetNotFound)

	rec := removeRequest(t, removeRouterWith(backend), "/api/v1/backup-sets/production/gone", true)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if got := errorCodeOf(t, rec); got != "BACKUP_SET_NOT_FOUND" {
		t.Errorf("error code = %q, want BACKUP_SET_NOT_FOUND", got)
	}
}

// TestRemoveBackupSet_RefusalIsNotSwallowedAsSuccess is the control the
// two above cannot provide between them. A handler that ignored the
// backend's error and answered 204 anyway would pass the first test and
// would be the same "the dialog closed, so it worked" lie in a new place.
func TestRemoveBackupSet_RefusalIsNotSwallowedAsSuccess(t *testing.T) {
	backend := newSyncFakeBackend()
	backend.errOnRemove = errors.New("the configuration file is not writable")

	rec := removeRequest(t, removeRouterWith(backend), "/api/v1/backup-sets/production/postgres-primary", true)

	if rec.Code == http.StatusNoContent {
		t.Fatal("the backend refused and the route still answered 204")
	}
	if rec.Code < 400 {
		t.Errorf("status = %d for a refused removal, want a 4xx or 5xx", rec.Code)
	}
}

// TestRemoveBackupSet_WithoutCSRFIsRefused is the same claim
// router_test.go's walk makes for every mutating route, made here as
// well because this one is destructive-sounding and worth reading in
// place: a cross-site DELETE must not remove an operator's backup set.
func TestRemoveBackupSet_WithoutCSRFIsRefused(t *testing.T) {
	backend := newSyncFakeBackend()
	rec := removeRequest(t, removeRouterWith(backend), "/api/v1/backup-sets/production/postgres-primary", false)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d without a CSRF pair, want %d", rec.Code, http.StatusForbidden)
	}
	if backend.lastRemoved != "" {
		t.Errorf("the request was refused but the backend was still asked to remove %q", backend.lastRemoved)
	}
}
