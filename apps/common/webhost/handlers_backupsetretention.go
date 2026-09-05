// This file is issue #333's per-set retention routes:
//
//	GET    /api/v1/backup-sets/{source}/{set}/retention
//	PUT    /api/v1/backup-sets/{source}/{set}/retention
//	DELETE /api/v1/backup-sets/{source}/{set}/retention
//
// # Why a sub-resource and not a field on the backup set
//
// A backup set's own retention policy is not a field you patch, it is a
// whole policy that either exists or does not. Three things follow, and
// each of them is why these are three methods on a resource of their own
// rather than keys on the backup set's own update request:
//
//   - An override REPLACES the deployment's chain. It is never merged
//     with it field by field, because merging two chains produces a
//     policy nobody wrote and nobody can predict from reading either half
//     (config.BackupSet.RetentionConfig's own doc). PUT is the verb that
//     already means "the body is the whole resource"; PATCH means the
//     opposite, and a whole-policy value living inside a field-by-field
//     request is exactly the shape that invites a client to send half of
//     one.
//   - "Go back to inheriting" cannot be expressed as a value on a sparse
//     update, where an absent field already means "leave this alone".
//     Those are opposite requests. DELETE says it unambiguously, and it
//     is the same discoverable, named operation on every surface.
//   - It matches how this package already exposes a per-set override of a
//     source-level default: /enabled and /read-only beside it are both
//     sub-resources with a fixed source/set arity and a literal tail.
//
// # CSRF yes, destructive gate no
//
// Same tier as /read-only and PATCH /settings. Nothing reachable from
// here moves or deletes a byte of backup data: this writes configuration.
// It does change what a LATER retention apply would delete, in both
// directions (clearing an override that was wider than the deployment's
// chain narrows what is kept), and that is exactly the case the router's
// own comment on PATCH /settings already settles for turning FR-19's
// protection off: the apply is the gated act, it re-reads the policy at
// plan time, and the surface in front of the human is what shows the two
// chains before the change is made. BackupSetRetention.deployment is
// served on every one of these responses so a client can do that.
package webhost

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// maxSetBackupSetRetentionBodyBytes bounds PUT
// .../retention's request body. A retention policy is a short list of
// small objects, so this is the same generous headroom every other write
// route in this package uses rather than a number tuned to a chain length
// that would then be a second, undocumented limit on how many tiers a
// deployment may have.
const maxSetBackupSetRetentionBodyBytes = 1 << 20 // 1 MiB

// backupSetRetentionResponse is the wire shape of
// service.BackupSetRetention: which policy is deciding for this set,
// whether it is the set's own, the deployment's policy beside it, and the
// raw override block as the configuration file carries it.
//
// Override is a pointer because "this set has no policy of its own" is
// the answer for most sets and has to be distinguishable from an empty
// policy object, which is the same distinction
// config.BackupSet.RetentionConfig's pointer exists for one layer down.
type backupSetRetentionResponse struct {
	BackupSetID string                 `json:"backup_set_id"`
	IsOverride  bool                   `json:"is_override"`
	Effective   retentionSettingsBody  `json:"effective"`
	Deployment  retentionSettingsBody  `json:"deployment"`
	Override    *retentionOverrideBody `json:"override"`
}

// retentionOverrideBody is both the PUT request body and the `override`
// half of the response: a client round-trips the identical shape it was
// served, so reading a policy and writing it back cannot change it.
//
// Every field is optional and unresolved, which is the whole point of
// this shape as opposed to retentionSettingsBody beside it. An omitted
// timezone means "inherit the deployment's", and a shape that filled it
// in would turn an inherited field into an explicit one the moment
// somebody re-saved a policy they had not edited.
//
// Both spellings of the chain are here because config.Retention has both,
// and this boundary states no rule of its own about which one is whole:
// core/service hands the submission to the identical config.Validate a
// hand-edited config.yaml goes through, so `{"daily_days":120}` is
// REFUSED, naming the two scalars it is missing, rather than being
// silently completed from the product defaults.
type retentionOverrideBody struct {
	Timezone      string `json:"timezone,omitempty"`
	WeekStartsOn  string `json:"week_starts_on,omitempty"`
	DailyDays     int    `json:"daily_days,omitempty"`
	WeeklyMonths  int    `json:"weekly_months,omitempty"`
	MonthlyMonths int    `json:"monthly_months,omitempty"`

	// Tiers is a slice rather than a pointer to one because nil and an
	// explicitly empty list already mean different things: JSON decodes
	// an absent key to nil and `[]` to a non-nil empty slice, and
	// core/service refuses the second with a message about what emptying
	// a chain would actually do. omitempty on the way out is safe for the
	// same reason: an empty chain is never a policy this server holds.
	Tiers []retentionTierBody `json:"tiers,omitempty"`

	ProtectLastKnownGood *bool `json:"protect_last_known_good"`

	// AcknowledgeMediumDisclosure is FR-27's consent, on this write for
	// the reason service.RetentionOverride gives: an override can send a
	// tier's artifacts off local disk exactly as the deployment's policy
	// can, so the gate stands here too. Request-only in practice: it is a
	// consent and not a setting, so the response's `override` half never
	// carries it (toRetentionOverrideBody leaves it false, and omitempty
	// drops it). Absent and false mean the same thing, which is why it is
	// not a pointer like the field above it.
	AcknowledgeMediumDisclosure bool `json:"acknowledge_medium_disclosure,omitempty"`
}

func toBackupSetRetentionResponse(r service.BackupSetRetention) backupSetRetentionResponse {
	out := backupSetRetentionResponse{
		BackupSetID: r.BackupSetID,
		IsOverride:  r.IsOverride,
		Effective:   toRetentionSettingsBody(r.Effective),
		Deployment:  toRetentionSettingsBody(r.Deployment),
	}
	if r.Override != nil {
		body := toRetentionOverrideBody(*r.Override)
		out.Override = &body
	}
	return out
}

func toRetentionOverrideBody(o service.RetentionOverride) retentionOverrideBody {
	body := retentionOverrideBody{
		Timezone:      o.Timezone,
		WeekStartsOn:  o.WeekStartsOn,
		DailyDays:     o.DailyDays,
		WeeklyMonths:  o.WeeklyMonths,
		MonthlyMonths: o.MonthlyMonths,
	}
	if o.Tiers != nil {
		body.Tiers = toTierBodies(o.Tiers)
	}
	if o.ProtectLastKnownGood != nil {
		protect := *o.ProtectLastKnownGood
		body.ProtectLastKnownGood = &protect
	}
	return body
}

func (b retentionOverrideBody) toService() service.RetentionOverride {
	o := service.RetentionOverride{
		Timezone:      b.Timezone,
		WeekStartsOn:  b.WeekStartsOn,
		DailyDays:     b.DailyDays,
		WeeklyMonths:  b.WeeklyMonths,
		MonthlyMonths: b.MonthlyMonths,
	}
	if b.Tiers != nil {
		// A non-nil empty slice has to stay non-nil and empty across this
		// projection: it is the difference between "I did not touch the
		// chain" and "I removed every tier", and core/service refuses the
		// second by name. Ranging into a nil slice would collapse them.
		o.Tiers = make([]service.RetentionTier, 0, len(b.Tiers))
		for _, t := range b.Tiers {
			o.Tiers = append(o.Tiers, t.toService())
		}
	}
	if b.ProtectLastKnownGood != nil {
		protect := *b.ProtectLastKnownGood
		o.ProtectLastKnownGood = &protect
	}
	o.AcknowledgeMediumDisclosure = b.AcknowledgeMediumDisclosure
	return o
}

// getBackupSetRetention is GET .../retention: read-only, so no CSRF and
// no destructive gate, exactly like every other GET in this package.
func (h *handlers) getBackupSetRetention(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	got, err := h.backend.BackupSetRetention(r.Context(), id)
	if err != nil {
		writeBackupSetRetentionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetRetentionResponse(got))
}

// setBackupSetRetention is PUT .../retention: give this set a policy of
// its own, replacing whatever it declared before.
//
// The body decoder is deliberately NOT strict about unknown fields, in
// line with every other write route here; what it IS strict about is that
// nothing in this handler decides whether the submitted policy is a whole
// one. That question has exactly one answer in this codebase
// (config.Validate, reached through core/service), and asking it a second
// time here is how the two would eventually disagree.
func (h *handlers) setBackupSetRetention(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSetBackupSetRetentionBodyBytes)

	var body retentionOverrideBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxSetBackupSetRetentionBodyBytes)
		return
	}

	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	got, err := h.backend.SetBackupSetRetention(r.Context(), id, body.toService())
	if err != nil {
		writeBackupSetRetentionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetRetentionResponse(got))
}

// clearBackupSetRetention is DELETE .../retention: this set is retained
// under the deployment's policy again.
//
// It reads no body at all. A DELETE that took one would be a second way
// to say the same thing, and the only thing a body could add here is a
// way to half-clear a policy, which is not a state this schema has.
func (h *handlers) clearBackupSetRetention(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	got, err := h.backend.ClearBackupSetRetention(r.Context(), id)
	if err != nil {
		writeBackupSetRetentionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetRetentionResponse(got))
}

// writeBackupSetRetentionError maps this file's three routes' failures.
//
// ErrInvalidRequest is echoed, and that is the point rather than a
// convenience: the text is config.ValidationError's own, naming the
// missing half of a chain in the same words a hand-edited config.yaml is
// refused with, which is what makes "validated exactly as the global one
// is" something an operator can see rather than something a comment
// claims. It is safe to echo for the reason writeBackupSetError already
// gives for the identical line: that text is built from this project's own
// field descriptions and the caller's own submitted values, never from a
// state or rclone error string.
func writeBackupSetRetentionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrBackupSetNotFound):
		writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
	case errors.Is(err, service.ErrMediumDisclosureRequired):
		// The same code, at the same status, for the same reason as the
		// settings route (writeSettingsError): this is not a field to
		// fix, it is a paragraph to put in front of a human, and the
		// message is that paragraph. Safe to echo for the same reason
		// too: core/service's own words plus tier names and medium ids
		// the caller itself submitted.
		writeError(w, http.StatusBadRequest, "MEDIUM_DISCLOSURE_REQUIRED", err.Error())
	case errors.Is(err, service.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrConfigNotFileBacked):
		writeError(w, http.StatusInternalServerError, "INTERNAL", "this deployment has no configuration file to persist to")
	default:
		// Deliberately not err.Error(): an unclassified error could carry
		// filesystem or rclone-internal text.
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to read or change this backup set's retention policy")
	}
}
