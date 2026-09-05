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
	Artifact string `json:"artifact"`
	Action   string `json:"action"`

	// Medium is WHERE this verdict's copy lives, and it is set only when
	// that is a configured storage medium (EPIC E FR-30, issue #430).
	//
	// A copy on the implicit local medium is spelled by the ABSENCE of
	// the field, which is the one decision on this projection that is not
	// a straight translation. It is what keeps a deployment that declares
	// no storage medium serving exactly the bytes it served before this
	// field existed, and `backup-manager retention` already states the
	// same asymmetry the same way (mediumSuffix, core/cmd/backup-manager/
	// retention.go), so the two operator surfaces read alike.
	//
	// The service-side value is not tested against the literal "local"
	// here: that id belongs to core/service, which answers the question
	// through RetentionArtifactVerdict.OnStorageMedium so this package
	// never acquires a second copy of a reserved identifier.
	Medium string `json:"medium,omitempty"`

	Reason string   `json:"reason"`
	Tiers  []string `json:"tiers,omitempty"`

	// TierSelections carries the same tiers, in the same order, each
	// paired with which of FR-18's two placements selected it (issue
	// #218). It is a second field rather than a richer `tiers` because
	// the two answer different questions and a client that only wants
	// the names should not have to walk objects for them; both are
	// projected from one service-side list in toRetentionPlanResponse,
	// so they cannot come to disagree.
	TierSelections []retentionTierSelectionResponse `json:"tier_selections,omitempty"`
}

// retentionTierSelectionResponse is one (tier, placement) pair. See the
// contract's own RetentionTierSelection description for why the placement
// belongs to the tier and not to the verdict: an artifact really can be
// selected by DAILY through the discovery placement and by MONTHLY
// through the producer's own timestamp, and this is the dialog that asks
// an operator to authorise deleting what is not on the list.
type retentionTierSelectionResponse struct {
	Tier       string `json:"tier"`
	SelectedBy string `json:"selected_by"`
}

// retentionMoveResponse is one artifact a plan would relocate, and both
// ends of the move: service.RetentionMove, translated to snake_case JSON
// (EPIC E FR-27, issue #430).
//
// Both mediums are carried verbatim, "local" included, because a move is
// about the DIFFERENCE between two places and naming only one of them
// would leave a client to infer the other. That is the opposite choice
// from retentionVerdictResponse.Medium above, and deliberately: a verdict
// answers "where would this happen", which has an implicit default, and a
// move answers "from where to where", which has none.
type retentionMoveResponse struct {
	Artifact   string `json:"artifact"`
	FromMedium string `json:"from_medium"`
	ToMedium   string `json:"to_medium"`
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

	// Moves is every artifact this plan would relocate, in verdict order,
	// and UnconfirmedPlacements names every kept artifact whose current
	// location could not be established (EPIC E FR-27, issue #430).
	//
	// Both are absent in a deployment that declares no storage medium.
	// That is not this projection's doing: core/service leaves them out
	// for such a deployment (summarizeRetentionPlan, and
	// omitPlacementInAMediumFreeDeployment for why), and omitempty here
	// carries the same answer onto the wire rather than serving an empty
	// array that a client would have to know to read as "nothing to say"
	// instead of "nothing found".
	//
	// They travel with the plan for Retention's own reason: an apply is
	// confirmed against a plan_id, and what that plan_id commits to has
	// to be the whole of what was shown.
	Moves                 []retentionMoveResponse `json:"moves,omitempty"`
	UnconfirmedPlacements []string                `json:"unconfirmed_placements,omitempty"`

	// Retention is the policy these verdicts were decided under, and
	// RetentionIsOverride says whether it is this backup set's own or the
	// deployment's (issue #333).
	//
	// A preview is read to answer "why is this artifact about to be
	// deleted", and that question has a different answer, and a different
	// place to go and fix it, depending on which policy was in force.
	// Both travel with the plan rather than being fetched separately,
	// because a plan is pinned to the configuration revision it was
	// computed against and a second read is not: a client that fetched
	// the attribution on its own could render a chain beside the wrong
	// source.
	Retention           retentionSettingsBody `json:"retention"`
	RetentionIsOverride bool                  `json:"retention_is_override"`
}

func toRetentionPlanResponse(p service.RetentionPlan) retentionPlanResponse {
	verdicts := make([]retentionVerdictResponse, len(p.Verdicts))
	for i, v := range p.Verdicts {
		var tiers []string
		var selections []retentionTierSelectionResponse
		for _, sel := range v.Tiers {
			tiers = append(tiers, sel.Tier)
			selections = append(selections, retentionTierSelectionResponse{Tier: sel.Tier, SelectedBy: sel.SelectedBy})
		}
		medium := ""
		if v.OnStorageMedium() {
			medium = v.Medium
		}
		verdicts[i] = retentionVerdictResponse{
			Artifact:       v.Artifact,
			Action:         v.Action,
			Medium:         medium,
			Reason:         v.Reason,
			Tiers:          tiers,
			TierSelections: selections,
		}
	}

	var moves []retentionMoveResponse
	for _, m := range p.Moves {
		moves = append(moves, retentionMoveResponse{
			Artifact:   m.Artifact,
			FromMedium: m.FromMedium,
			ToMedium:   m.ToMedium,
		})
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

		Moves:                 moves,
		UnconfirmedPlacements: p.UnconfirmedPlacements,

		Retention:           toRetentionSettingsBody(p.Retention),
		RetentionIsOverride: p.RetentionIsOverride,
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
