package webhost

// This file is issue #333's API half: the three operations that let an
// operator give one backup set its own retention chain, or put it back on
// the deployment's, without opening config.yaml on the NAS.
//
// # Why POST twice rather than PUT and DELETE
//
// A whole-policy write is a PUT and a removal is a DELETE, in the
// abstract. Neither method appears anywhere in this API, and introducing
// two of them for one feature would mean this contract's readers, its
// generated bindings and its middleware all learn two new shapes to serve
// one route each. The precedent already in the router is closer and
// cheaper: /enabled and /read-only are POSTs that set a value, and
// /edit-hold/release is a POST tail that removes one. These follow it.
//
// # CSRF, and not the destructive gate
//
// Writing a retention policy deletes nothing. What deletes is a retention
// APPLY, which is behind the gate already and which an administrator
// reviews as a plan first. Gating the policy itself would mean an
// operator who has not turned destructive operations on cannot correct a
// retention policy that is about to delete the wrong thing, which is
// exactly backwards.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// maxSetBackupSetRetentionBodyBytes bounds the override body. A chain is
// a short list of small records, so this is generous by orders of
// magnitude; it exists so an unbounded read cannot be aimed at this route
// rather than to express a real limit.
const maxSetBackupSetRetentionBodyBytes = 64 << 10

// setBackupSetRetentionRequest is POST /backup-sets/{source}/{set}/retention's
// body.
//
// Tiers is the whole chain and is required. The three legacy
// daily_days/weekly_months/monthly_months scalars are deliberately absent:
// see core/service/backupsetretention.go for why a shape that can express
// half a chain is the one thing this feature must not have.
type setBackupSetRetentionRequest struct {
	Tiers []retentionTierBody `json:"tiers"`

	// Timezone and WeekStartsOn are absent (or "") to inherit the
	// deployment's.
	Timezone     string `json:"timezone,omitempty"`
	WeekStartsOn string `json:"week_starts_on,omitempty"`

	// ProtectLastKnownGood is a pointer because absent means "inherit
	// the deployment's posture" and an explicit false is a different,
	// materially more dangerous request.
	ProtectLastKnownGood *bool `json:"protect_last_known_good"`
}

// backupSetRetentionResponse is what all three operations answer with,
// including the two writes: a caller that just wrote a policy should see
// what is now in force rather than an echo of what it sent, since
// inheritance means those can legitimately differ.
type backupSetRetentionResponse struct {
	BackupSetID string `json:"backup_set_id"`
	IsOverride  bool   `json:"is_override"`
	// Policy is the RESOLVED chain in force for this set.
	Policy retentionSettingsBody `json:"policy"`
	// DeploymentPolicy is what clearing the override would return to.
	DeploymentPolicy retentionSettingsBody `json:"deployment_policy"`
}

func toBackupSetRetentionResponse(r service.BackupSetRetention) backupSetRetentionResponse {
	return backupSetRetentionResponse{
		BackupSetID:      r.BackupSetID,
		IsOverride:       r.IsOverride,
		Policy:           toRetentionSettingsBody(r.Policy),
		DeploymentPolicy: toRetentionSettingsBody(r.DeploymentPolicy),
	}
}

// getBackupSetRetention is GET /backup-sets/{source}/{set}/retention.
func (h *handlers) getBackupSetRetention(w http.ResponseWriter, r *http.Request) {
	got, err := h.backend.GetBackupSetRetention(r.Context(), backupSetIDFromRoute(r))
	if err != nil {
		writeBackupSetRetentionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetRetentionResponse(got))
}

// setBackupSetRetention is POST /backup-sets/{source}/{set}/retention.
func (h *handlers) setBackupSetRetention(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSetBackupSetRetentionBodyBytes)

	var body setBackupSetRetentionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxSetBackupSetRetentionBodyBytes)
		return
	}

	override := service.BackupSetRetentionOverride{
		Timezone:             body.Timezone,
		WeekStartsOn:         body.WeekStartsOn,
		ProtectLastKnownGood: body.ProtectLastKnownGood,
		Tiers:                fromTierBodies(body.Tiers),
	}
	got, err := h.backend.SetBackupSetRetention(r.Context(), backupSetIDFromRoute(r), override)
	if err != nil {
		writeBackupSetRetentionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetRetentionResponse(got))
}

// clearBackupSetRetention is POST /backup-sets/{source}/{set}/retention/clear.
func (h *handlers) clearBackupSetRetention(w http.ResponseWriter, r *http.Request) {
	got, err := h.backend.ClearBackupSetRetention(r.Context(), backupSetIDFromRoute(r))
	if err != nil {
		writeBackupSetRetentionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetRetentionResponse(got))
}

// backupSetIDFromRoute rebuilds the backup set id from the two named path
// segments, the same join model.BackupSetID.String() performs. Safe
// because neither half may contain a slash, so there is exactly one way
// to read the pair back as one id (issue #285).
func backupSetIDFromRoute(r *http.Request) string {
	return chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
}

// writeBackupSetRetentionError maps this file's three operations'
// failures. It is its own function rather than writeBackupSetError,
// because that one's default sentence is about writing a backup set and
// an operator whose retention write failed should not be sent looking for
// a set that was never being written.
func writeBackupSetRetentionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrBackupSetNotFound):
		writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
	case errors.Is(err, service.ErrInvalidRequest):
		// Safe to echo, on the same terms as every other ErrInvalidRequest
		// this package echoes: core/service builds it from config's own
		// field descriptions and the caller's own values.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrConfigNotFileBacked):
		writeError(w, http.StatusInternalServerError, "INTERNAL", "this deployment has no configuration file to persist to")
	default:
		// Deliberately not err.Error(): an unclassified error could carry
		// filesystem or rclone-internal text.
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to write this backup set's retention policy")
	}
}
