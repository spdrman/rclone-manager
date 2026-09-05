package webhost

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// runningWorkResponse names what a run cycle is doing for one backup set
// right now. It is the content of the warning an operator sees before
// entering edit mode, and the reason that warning is worth showing at
// all: discarding a partial transfer of a named artifact is a materially
// different cost from cancelling a scheduler tick that has not started
// work, and only a message that says which one it is lets an operator
// decide (issue #350).
type runningWorkResponse struct {
	// Artifact is the artifact being worked on, or "" during discovery,
	// when the cycle has reached this set but not yet picked one.
	Artifact string `json:"artifact"`
	// Stage is one of OperationProgress.stage's values.
	Stage string `json:"stage"`
}

// editHoldStateResponse is GET /api/v1/backup-sets/{source}/{set}/edit-hold's
// body: what entering edit mode would interrupt, and whether a hold is
// already in force.
//
// Running is a pointer so it can be null, and that is the whole shape of
// "the warning appears only when something is actually running": a client
// that gets null opens edit mode with no prompt at all. A zero-valued
// object would be rendered as a prompt for a risk that does not exist.
type editHoldStateResponse struct {
	BackupSetID string               `json:"backup_set_id"`
	Held        bool                 `json:"held"`
	ExpiresAt   string               `json:"expires_at,omitempty"`
	Running     *runningWorkResponse `json:"running"`
}

// editHoldResponse is POST /edit-hold's body: the lease, plus what taking
// it interrupted (null when nothing was running, so a client never claims
// to have stopped something it did not).
type editHoldResponse struct {
	BackupSetID string               `json:"backup_set_id"`
	ExpiresAt   string               `json:"expires_at"`
	Stopped     *runningWorkResponse `json:"stopped"`
}

func toRunningWorkResponse(w *service.RunningWork) *runningWorkResponse {
	if w == nil {
		return nil
	}
	return &runningWorkResponse{Artifact: w.Artifact, Stage: w.Stage}
}

// getBackupSetEditHold is GET
// /api/v1/backup-sets/{source}/{set}/edit-hold: what a cycle is currently
// doing for this set, and whether it is already held.
//
// Read-only (§50), so no CSRF and no destructive gate, exactly like the
// other GETs in this package. It deliberately takes no hold: a read that
// silently held would pause backups for a set an operator merely looked
// at, and the whole point of splitting the read from the write is that an
// operator can be shown what they are about to stop and then decline.
func (h *handlers) getBackupSetEditHold(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	st, err := h.backend.BackupSetEditState(r.Context(), id)
	if err != nil {
		writeEditHoldError(w, err)
		return
	}
	resp := editHoldStateResponse{
		BackupSetID: st.BackupSetID,
		Held:        st.Held,
		Running:     toRunningWorkResponse(st.Running),
	}
	if st.Held {
		resp.ExpiresAt = st.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

// takeBackupSetEditHold is POST
// /api/v1/backup-sets/{source}/{set}/edit-hold: take the hold, or renew
// one already held. The cycle currently processing this set, if any, is
// stopped at its next safe boundary, and no new pass starts for it until
// the hold is released or lapses.
//
// One route for both taking and renewing, rather than two: a renewal IS
// "hold this set for another lease", and a client whose heartbeat arrives
// a second after the lease lapsed still has its form open with an
// operator mid-edit, so refusing it would leave the set unheld while the
// form stayed on screen (see core/service.RenewBackupSetEdit's doc).
//
// requireCSRF and NOT requireDestructiveGate. Nothing reachable from here
// starts work or deletes anything: it STOPS work. Gating it would mean an
// operator who has not turned destructive operations on cannot safely
// edit a backup set at all, which is the opposite of what the gate is
// for.
func (h *handlers) takeBackupSetEditHold(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	hold, err := h.backend.BeginBackupSetEdit(r.Context(), id)
	if err != nil {
		writeEditHoldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, editHoldResponse{
		BackupSetID: hold.BackupSetID,
		ExpiresAt:   hold.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Stopped:     toRunningWorkResponse(hold.Stopped),
	})
}

// releaseBackupSetEditHold is POST
// /api/v1/backup-sets/{source}/{set}/edit-hold/release: leave edit mode.
//
// A POST with a "/release" tail rather than a DELETE on the path above,
// for two reasons. This package's whole write surface is POST and one
// PATCH, and adding a fourth method for one route is a wider change than
// it looks (every CSRF and gate walk, every provider proxy, every
// client). And the release has to be callable from a page that is going
// away, where the browser APIs that survive an unload are POST-shaped.
//
// 204, not 200: there is nothing to say. Releasing a hold that is not
// held is a success for the same reason core/service.EndBackupSetEdit
// says so: several routes out of edit mode fire for one edit, and a
// duplicate release must not be something a client has to avoid.
func (h *handlers) releaseBackupSetEditHold(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	if err := h.backend.EndBackupSetEdit(r.Context(), id); err != nil {
		writeEditHoldError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeEditHoldError maps the one refusal these three routes can produce
// to the vocabulary the rest of the backup-set surface already uses, so a
// client does not learn a second code for the same condition.
func writeEditHoldError(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrBackupSetNotFound) {
		writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
		return
	}
	// Deliberately not err.Error(): an unclassified error here could
	// carry filesystem or transport-internal text, the same reason
	// writeBackupSetError's own default case gives.
	writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to read or change this backup set's edit hold")
}
