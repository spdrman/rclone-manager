package webhost

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// Issue #419's route out of FAILED. The quarantine retry beside it has
// been tested since #211; this is the same shape one state along, and the
// cases that differ are the note and the 409.

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
