package webhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// submitOperationRequest is POST /api/v1/operations' request body. The
// idempotency key travels as a header (Idempotency-Key), not a body
// field: it is a property of the HTTP request/retry, not of the
// operation's own business parameters, matching the common REST
// convention for this exact concern.
type submitOperationRequest struct {
	Action         string `json:"action"`
	ConfigRevision string `json:"config_revision"`
}

// operationResponse is the wire shape of one operation, used both for the
// 202 response POST /api/v1/operations returns and for
// GET /api/v1/operations/{id} (docs/EPIC-B-multi-nas.md §14's example,
// §15.7's polling surface). Timestamp fields are omitted (not
// zero-valued) until the corresponding event has actually happened.
type operationResponse struct {
	OperationID    string `json:"operation_id"`
	Status         string `json:"status"`
	Actor          string `json:"actor,omitempty"`
	BackupSetID    string `json:"backup_set_id,omitempty"`
	ConfigRevision string `json:"config_revision,omitempty"`
	Action         string `json:"action,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	Result         string `json:"result,omitempty"`
	Error          string `json:"error,omitempty"`
}

func toOperationResponse(op service.Operation) operationResponse {
	resp := operationResponse{
		OperationID:    op.ID,
		Status:         op.Status,
		Actor:          op.Actor,
		BackupSetID:    op.BackupSetID,
		ConfigRevision: op.ConfigRevision,
		Action:         op.Action,
		Result:         op.Result,
		Error:          op.Error,
	}
	if !op.CreatedAt.IsZero() {
		resp.CreatedAt = op.CreatedAt.Format(time.RFC3339Nano)
	}
	if !op.StartedAt.IsZero() {
		resp.StartedAt = op.StartedAt.Format(time.RFC3339Nano)
	}
	if !op.FinishedAt.IsZero() {
		resp.FinishedAt = op.FinishedAt.Format(time.RFC3339Nano)
	}
	return resp
}

// submitOperation is POST /api/v1/operations. It is the one mutating
// route this skeleton exposes, and the one route wrapped in
// requireDestructiveGate (see router.go): a run_cycle can advance backup
// sets all the way through remote deletion (FR-15), so it is treated as
// destructive, not merely "mutating", for gating purposes.
//
// It never blocks on the operation actually finishing: it returns as soon
// as core/service.BackupServiceClient.SubmitRunCycle has durably persisted
// the row (see that method's own doc for the decoupling guarantee this
// handler relies on rather than re-implements).
func (h *handlers) submitOperation(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "the Idempotency-Key header is required")
		return
	}

	var body submitOperationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON body")
		return
	}
	if body.Action != service.ActionRunCycle {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("unsupported action %q; only %q is supported in this release", body.Action, service.ActionRunCycle))
		return
	}

	op, err := h.backend.SubmitRunCycle(r.Context(), service.RunCycleRequest{
		IdempotencyKey: idempotencyKey,
		Actor:          actorFromContext(r.Context()),
		ConfigRevision: body.ConfigRevision,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrConfigRevisionStale):
			writeError(w, http.StatusConflict, "CONFIG_REVISION_STALE", err.Error())
		case errors.Is(err, service.ErrInvalidRequest):
			// Safe to echo: ErrInvalidRequest's message is always one of
			// core/service's own deliberately generic strings (see that
			// sentinel's doc), never anything from core/internal.
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		default:
			// Deliberately NOT err.Error() here: an unclassified error is
			// exactly the case core/service.ErrInvalidRequest's doc warns
			// about, one that might otherwise carry state-layer/SQLite
			// text across this boundary.
			writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to submit operation")
		}
		return
	}

	writeJSON(w, http.StatusAccepted, toOperationResponse(op))
}

// getOperation is GET /api/v1/operations/{id}: authenticated polling
// (§15.7, §14's "the v1 frontend SHALL observe long-running operation
// state through authenticated polling"). It does not require the
// destructive gate — reading status is not a destructive action, and
// gating reads on #92 landing would make an already-submitted operation
// unobservable, not safer.
func (h *handlers) getOperation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	op, err := h.backend.GetOperation(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrOperationNotFound) {
			writeError(w, http.StatusNotFound, "OPERATION_NOT_FOUND", "no such operation")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load operation")
		return
	}
	writeJSON(w, http.StatusOK, toOperationResponse(op))
}
