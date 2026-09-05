// Prove a declared storage medium works, before a cycle carrying a real
// backup finds out for the operator.
//
// The whole route is a write followed by a delete of the object it just
// wrote, which is what puts it in the CSRF tier without putting it in the
// destructive one: it has a real side effect on somebody's bucket, and the
// only object it can reach is its own probe.
//
// What the response deliberately does not carry is as important as what it
// does. There is no field for key material and there will not be, and the
// endpoint's own text of whatever came back never reaches this struct
// either: a provider's error string can contain a signed URL, a bucket
// listing or an account identifier, so the checks here carry a step, an
// outcome and a category, and the raw sentence goes to the manager's log
// where the operator already has the trust to read it.
package webhost

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// mediumPreflightResponse is POST
// /api/v1/storage-mediums/{id}/preflight's body.
//
// There is no field here for key material of any kind, and there never
// will be (FR-33). What each check carries is a step, an outcome, the
// transport category a failure classified as, and one of the engine's own
// sentences: see core/internal/mediumcheck's package doc for why the text
// of what actually came back never reaches this struct and goes to the
// manager's log instead.
type mediumPreflightResponse struct {
	Medium string                 `json:"medium"`
	OK     bool                   `json:"ok"`
	Checks []mediumPreflightCheck `json:"checks"`
}

type mediumPreflightCheck struct {
	Step     string `json:"step"`
	Outcome  string `json:"outcome"`
	Category string `json:"category,omitempty"`
	Detail   string `json:"detail"`
}

// preflightStorageMedium is POST /api/v1/storage-mediums/{id}/preflight:
// prove one declared storage medium actually works, before a cycle
// carrying a real backup does it for the operator (issue #443).
//
// It carries requireCSRF and NOT requireDestructiveGate, and both halves
// are worth stating.
//
// CSRF, because this is not a read. It writes a small probe object to
// somebody's bucket and deletes it again, which is a real side effect on
// real storage against a caller-supplied id, exactly the reason
// host-key-probe carries CSRF despite being a read of a public key.
//
// Not the destructive gate, because the gate stands in front of operations
// that can destroy BACKUP DATA (docs/EPIC-B-multi-nas.md §50). Nothing
// this can reach touches a backup: the only object it writes is one it
// generated a random key for, under a reserved key segment no configured
// artifact can produce, and the only object it deletes is that same one.
// It moves no journal row, it changes no configuration, and it cannot
// reach a remote source at all.
//
// A medium that does not work is a 200 with ok false, exactly like a
// failed backup-set connection test: a bucket that is not there is what an
// operator did, not what broke. The error path is for a medium this
// configuration does not declare.
func (h *handlers) preflightStorageMedium(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	result, err := h.backend.PreflightStorageMedium(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrMediumNotFound) {
			writeError(w, http.StatusNotFound, "MEDIUM_NOT_FOUND",
				"this configuration declares no storage medium with that id")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to check the storage medium")
		return
	}

	body := mediumPreflightResponse{Medium: result.Medium, OK: result.OK, Checks: make([]mediumPreflightCheck, 0, len(result.Checks))}
	for _, c := range result.Checks {
		body.Checks = append(body.Checks, mediumPreflightCheck{
			Step:     c.Step,
			Outcome:  c.Outcome,
			Category: c.Category,
			Detail:   c.Detail,
		})
	}
	writeJSON(w, http.StatusOK, body)
}
