// Issue #419's route out of FAILED. The quarantine retry beside it has
// been tested since #211; this is the same shape one state along, and the
// cases that differ are the note and the 409.
package webhost

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

func TestRetryFailedIngestion_Returns204AndNamesTheArtifact(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.post(t, "/api/v1/backups/production/postgres/bad.dump/retry", "")
	mustStatus(t, rec, http.StatusNoContent)
	if got := rt.backend.lastRetriedFailed; got != "production/postgres/bad.dump" {
		t.Errorf("the handler asked about %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 carried a body: %s", rec.Body.String())
	}
}

// TestRetryFailedIngestion_CarriesTheOperatorsNote. The note is the only
// thing a caller can send, and it is what a later failure of the same
// backup carries as context for what was tried last time. A handler that
// dropped it would leave every retry looking identical.
func TestRetryFailedIngestion_CarriesTheOperatorsNote(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.post(t, "/api/v1/backups/production/postgres/bad.dump/retry", `{"note":"the NAS came back"}`)
	mustStatus(t, rec, http.StatusNoContent)
	if got := rt.backend.lastRetryFailedNote; got != "the NAS came back" {
		t.Errorf("note reached the service as %q", got)
	}
}

// TestRetryFailedIngestion_AnArtifactThatIsNotStuckIs409AndNamed. The
// refusal has to be its own code rather than the quarantine one: an
// operator acting from a stale screen needs to be told the backup is not
// stuck, not that it is not quarantined, which is a different fact about a
// different action.
func TestRetryFailedIngestion_AnArtifactThatIsNotStuckIs409AndNamed(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnRetryFailed = fmt.Errorf("%w: production/postgres/bad.dump", service.ErrArtifactNotFailed)

	rec := rt.post(t, "/api/v1/backups/production/postgres/bad.dump/retry", "")
	mustStatus(t, rec, http.StatusConflict)
	if got := responseErrorCode(rec.Body.String()); got != "ARTIFACT_NOT_FAILED" {
		t.Fatalf("error code = %q, want ARTIFACT_NOT_FAILED", got)
	}
}

func TestRetryFailedIngestion_AnUnconfiguredBackupSetIs404AndNamed(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnRetryFailed = fmt.Errorf("%w: production/postgres", service.ErrBackupSetNotFound)

	rec := rt.post(t, "/api/v1/backups/production/postgres/bad.dump/retry", "")
	mustStatus(t, rec, http.StatusNotFound)
	if got := responseErrorCode(rec.Body.String()); got != "BACKUP_SET_NOT_FOUND" {
		t.Fatalf("error code = %q, want BACKUP_SET_NOT_FOUND", got)
	}
}

// TestRetryFailedIngestion_HonoursANoteWhoseLengthIsNotDeclared. A
// chunked request carries a body and a ContentLength of -1, so a handler
// that decoded only when the length was positive would accept the request,
// ignore what it said and report 204. The note is the only thing a caller
// can send; dropping it silently is the worst of the three possible
// behaviours.
func TestRetryFailedIngestion_HonoursANoteWhoseLengthIsNotDeclared(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/backups/production/postgres/bad.dump/retry",
		strings.NewReader(`{"note":"chunked"}`))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	rt.router.ServeHTTP(rec, csrfPaired(req))

	mustStatus(t, rec, http.StatusNoContent)
	if got := rt.backend.lastRetryFailedNote; got != "chunked" {
		t.Fatalf("note reached the service as %q, want %q", got, "chunked")
	}
}

// TestRetryFailedIngestion_AnEmptyBodyIsNotAnError is the other half:
// sending no body at all is what a client with nothing to say does, and it
// must not be a 400.
func TestRetryFailedIngestion_AnEmptyBodyIsNotAnError(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.post(t, "/api/v1/backups/production/postgres/bad.dump/retry", "")
	mustStatus(t, rec, http.StatusNoContent)
	if got := rt.backend.lastRetryFailedNote; got != "" {
		t.Fatalf("note = %q for a request that sent none", got)
	}
}
