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

// maxApplyRetentionBodyBytes bounds POST .../retention/apply's request
// body (docs/EPIC-B-multi-nas.md §17's request-size limit), mirroring
// maxSubmitOperationBodyBytes (handlers_operations.go): the body carries
// exactly one short string (plan_id), so this is generous headroom over
// anything a legitimate client would send.
const maxApplyRetentionBodyBytes = 1 << 20 // 1 MiB

// retentionVerdictResponse is one artifact's classification within a
// retentionPlanResponse: service.RetentionArtifactVerdict, translated to
// snake_case JSON. This is not part of docs/EPIC-B-multi-nas.md §15.6's
// own required schema (plan_id/inventory_revision/config_revision/
// expires_at/keep_count/delete_count/reclaim_bytes), but §29.2's "the UI
// SHALL expose a retention preview" example (KEEP/DELETE per artifact,
// with a reason) needs exactly this, so it travels alongside the required
// fields rather than needing a second round trip.
type retentionVerdictResponse struct {
	Artifact string   `json:"artifact"`
	Action   string   `json:"action"`
	Reason   string   `json:"reason"`
	Tiers    []string `json:"tiers,omitempty"`
}

// retentionPlanResponse is both GET .../retention/preview's and POST
// .../retention/apply's response shape (docs/EPIC-B-multi-nas.md §15.6's
// own example): a caller never has to reconcile two different shapes for
// "what would happen" versus "what happened" — see
// service.RetentionPlan's own doc for why ApplyRetentionPlan's own return
// value already carries this same shape.
type retentionPlanResponse struct {
	PlanID            string                     `json:"plan_id"`
	BackupSetID       string                     `json:"backup_set_id"`
	InventoryRevision string                     `json:"inventory_revision"`
	ConfigRevision    string                     `json:"config_revision"`
	ExpiresAt         string                     `json:"expires_at"`
	KeepCount         int                        `json:"keep_count"`
	DeleteCount       int                        `json:"delete_count"`
	ReclaimBytes      int64                      `json:"reclaim_bytes"`
	OperationID       string                     `json:"operation_id,omitempty"`
	Verdicts          []retentionVerdictResponse `json:"verdicts"`
}

func toRetentionPlanResponse(p service.RetentionPlan) retentionPlanResponse {
	verdicts := make([]retentionVerdictResponse, len(p.Verdicts))
	for i, v := range p.Verdicts {
		verdicts[i] = retentionVerdictResponse{
			Artifact: v.Artifact,
			Action:   v.Action,
			Reason:   v.Reason,
			Tiers:    v.Tiers,
		}
	}
	return retentionPlanResponse{
		PlanID:            p.PlanID,
		BackupSetID:       p.BackupSetID,
		InventoryRevision: p.InventoryRevision,
		ConfigRevision:    p.ConfigRevision,
		ExpiresAt:         p.ExpiresAt.Format(time.RFC3339Nano),
		KeepCount:         p.KeepCount,
		DeleteCount:       p.DeleteCount,
		ReclaimBytes:      p.ReclaimBytes,
		OperationID:       p.OperationID,
		Verdicts:          verdicts,
	}
}

// previewRetention is GET /api/v1/backup-sets/{source}/{set}/retention/preview
// (docs/EPIC-B-multi-nas.md §15.6). It is read-only end to end: this
// handler makes no mutating call, and it is deliberately not registered
// behind requireCSRF/requireDestructiveGate in router.go, matching §50's
// own classification of "preview retention" as read-only/low risk.
func (h *handlers) previewRetention(w http.ResponseWriter, r *http.Request) {
	source := chi.URLParam(r, "source")
	set := chi.URLParam(r, "set")

	plan, err := h.backend.PreviewRetention(r.Context(), source, set)
	if err != nil {
		writeRetentionServiceError(w, err, "failed to preview retention")
		return
	}

	writeJSON(w, http.StatusOK, toRetentionPlanResponse(plan))
}

// applyRetentionRequest is POST .../retention/apply's request body
// (docs/EPIC-B-multi-nas.md §15.6: "MUST require the plan_id the
// administrator actually reviewed").
//
// plan_id is what ApplyRetentionPlan looks the plan up by (an opaque,
// per-preview token — see service.RetentionPlan's own doc), which is what
// makes the backup set this actually acts on impossible to spoof by
// editing the URL without also holding a plan_id that backup set's own
// PreviewRetention call issued. The {source}/{set} the URL names is passed
// down alongside it all the same, and the service refuses when the two
// disagree: it costs four lines, it keeps this route acting on the
// resource its path names like every other route in this package, and it
// catches the far likelier failure the URL check is actually for — a
// client bug or stale component state submitting the wrong plan id (see
// service.ApplyRetentionRequest.Source's own doc).
type applyRetentionRequest struct {
	PlanID string `json:"plan_id"`
}

// applyRetention is POST /api/v1/backup-sets/{source}/{set}/retention/apply
// (docs/EPIC-B-multi-nas.md §15.6, §29.3). This is the one route this
// package's own retention surface can delete local restore points
// through, so — unlike previewRetention above — router.go wraps it in both
// requireCSRF and requireDestructiveGate, exactly like POST
// /api/v1/operations.
func (h *handlers) applyRetention(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxApplyRetentionBodyBytes)

	var body applyRetentionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("request body exceeds the %d byte limit", maxApplyRetentionBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON body")
		return
	}

	plan, err := h.backend.ApplyRetentionPlan(r.Context(), service.ApplyRetentionRequest{
		PlanID: body.PlanID,
		Source: chi.URLParam(r, "source"),
		Set:    chi.URLParam(r, "set"),
		Actor:  actorFromContext(r.Context()),
	})
	if err != nil {
		writeRetentionServiceError(w, err, "failed to apply retention")
		return
	}

	writeJSON(w, http.StatusOK, toRetentionPlanResponse(plan))
}

// writeRetentionServiceError maps a core/service retention error to this
// package's usual JSON error shape, shared between previewRetention and
// applyRetention since both call into the same core/service sentinels.
//
// RETENTION_PLAN_STALE is deliberately its own code, distinct from
// INVALID_REQUEST or a bare 500: docs/EPIC-B-multi-nas.md §15.6 names it
// explicitly, and a client handling this response is expected to react
// differently to it (re-preview and re-confirm) than to a validation
// failure or an unclassified server error — the same reasoning
// writeConfigRevisionStale/CONFIG_REVISION_STALE and submitOperation's own
// error switch (handlers_operations.go) already apply to their own
// sentinels.
func writeRetentionServiceError(w http.ResponseWriter, err error, fallbackMessage string) {
	switch {
	case errors.Is(err, service.ErrRetentionPlanStale):
		writeError(w, http.StatusConflict, "RETENTION_PLAN_STALE", err.Error())
	case errors.Is(err, service.ErrRetentionApplyBusy):
		// Its own code, not RETENTION_PLAN_STALE: the plan is fine and was
		// not consumed, the server is busy, and the client should retry
		// the same plan_id rather than tell the operator to re-preview
		// (see that sentinel's own doc, core/service/retention.go).
		writeError(w, http.StatusConflict, "RETENTION_APPLY_BUSY", err.Error())
	case errors.Is(err, service.ErrRetentionPlanNotFound):
		writeError(w, http.StatusNotFound, "RETENTION_PLAN_NOT_FOUND", err.Error())
	case errors.Is(err, service.ErrBackupSetNotFound):
		writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", err.Error())
	case errors.Is(err, service.ErrInvalidRequest):
		// Safe to echo: ErrInvalidRequest's message is always one of
		// core/service's own deliberately generic strings (see that
		// sentinel's doc, operations.go).
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		// Deliberately NOT err.Error() here: an unclassified error might
		// carry state-layer/SQLite text across this boundary (see
		// submitOperation's identical reasoning, handlers_operations.go).
		writeError(w, http.StatusInternalServerError, "INTERNAL", fallbackMessage)
	}
}
