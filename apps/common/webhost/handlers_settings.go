package webhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/spdrman/rclone-manager/core/service"
)

// maxSettingsBodyBytes bounds PATCH /api/v1/settings' request body, the
// same rationale as maxCreateBackupSetBodyBytes
// (handlers_backupsets.go). A retention chain is a short list of small
// objects, so 64 KiB is already orders of magnitude more headroom than
// any legitimate request needs while still bounding how much of a
// malformed or hostile body this handler reads before giving up. It is
// deliberately smaller than the backup-set limit: there is no field here
// that can legitimately carry bulk content (no key material, no include
// list), so the generous 1 MiB that route needs would only be a larger
// target.
const maxSettingsBodyBytes = 64 << 10

// settingsRequest is PATCH /api/v1/settings' body: one optional object
// per settings section.
//
// # Why this is generic, and where the generality stops
//
// One route serves every administrable setting, so adding the next one is
// a field on this struct and a line in the mapping below, never a new
// route with its own auth, CSRF and error-mapping wiring to get subtly
// wrong. What it deliberately is NOT is a passthrough: there is no field
// here that accepts an arbitrary config key or an arbitrary YAML
// fragment, so nothing a caller can put in this body reaches
// state.database, a backup set's remote, or a validator command — those
// have no spelling in this type. Every field is enumerated, and the whole
// resulting config still goes through core/service's own
// config.Validate before anything is written (see
// service.BackupService.UpdateSettings).
//
// Every field is a pointer (or, for the chain, a nil-able slice) because
// a PATCH's whole contract is "change what I named, leave the rest": a
// plain value could not tell `"keep": 0` apart from an omitted key, and
// on a retention policy that difference is the difference between a
// deliberate refusal and a silently widened window.
type settingsRequest struct {
	Retention *retentionUpdateRequest `json:"retention"`
}

type retentionUpdateRequest struct {
	Timezone     *string `json:"timezone"`
	WeekStartsOn *string `json:"week_starts_on"`
	// Tiers replaces FR-18's whole chain. Absent (or null) leaves it
	// alone; an explicitly empty list is passed through as an empty,
	// non-nil slice so core/service can refuse it, because in the config
	// file an empty chain reinstates the default policy rather than
	// meaning "keep nothing" (service.RetentionUpdate.Tiers' own doc).
	Tiers []retentionTierBody `json:"tiers"`
	// ProtectLastKnownGood turns FR-19's protection on or off. Turning it
	// off is what internal/retention calls "a materially more dangerous
	// configuration"; the operator-facing confirmation for that lives in
	// the UI (ui/shared/src/pages/SettingsPage.tsx), in front of the
	// human, since this layer cannot tell a confirmed request from an
	// unconfirmed one.
	ProtectLastKnownGood *bool `json:"protect_last_known_good"`
}

// namesNothing reports a retention object that was sent but carries no
// field at all, which this route refuses exactly as it refuses an absent
// one. The check is structural rather than "is the section present",
// because `{"retention":{}}` passes the presence test while asking for
// nothing: honouring it would rewrite the operator's config file, move
// ConfigRevision (invalidating every outstanding retention preview) and
// answer 200 for a request with no content. core/service applies the
// identical guard (service.UpdateSettingsRequest's own doc), so a caller
// that reached it around this layer is refused too; this one exists so a
// refused request never reaches the backend at all.
//
// `"tiers": []` is deliberately NOT "nothing named": it is a request with
// a meaning, and core/service refuses it with a message that explains what
// emptying the chain would actually do.
func (r retentionUpdateRequest) namesNothing() bool {
	return r.Timezone == nil &&
		r.WeekStartsOn == nil &&
		r.Tiers == nil &&
		r.ProtectLastKnownGood == nil
}

// retentionTierBody is one link in the chain, on the wire. Shared by the
// request and the response so a client round-trips the identical shape it
// was served, rather than reading one spelling and having to write
// another.
type retentionTierBody struct {
	Name        string `json:"name"`
	Granularity string `json:"granularity"`
	PeriodDays  int    `json:"period_days,omitempty"`
	Keep        int    `json:"keep"`
	WindowUnit  string `json:"window_unit,omitempty"`
}

// settingsResponse is what both GET and PATCH /api/v1/settings return:
// the settings now in effect, plus the closed value sets and bounds those
// settings are validated against.
//
// Serving the schema alongside the values is what lets a form build its
// pickers and enforce its bounds from the rules core/internal/config
// actually applies, instead of a second copy of them transcribed into a
// frontend and free to drift the next time a granularity is added.
// config.RetentionTier's own doc anticipates exactly this.
type settingsResponse struct {
	Retention retentionSettingsBody `json:"retention"`
	Schema    settingsSchemaBody    `json:"schema"`
}

type retentionSettingsBody struct {
	Timezone     string `json:"timezone"`
	WeekStartsOn string `json:"week_starts_on"`
	// Tiers is always the RESOLVED chain, so a config file written with
	// the legacy scalars reports the three tiers those keys stand for.
	// A client therefore renders one shape for one policy, and never has
	// to know the sugar exists.
	Tiers                []retentionTierBody `json:"tiers"`
	ProtectLastKnownGood bool                `json:"protect_last_known_good"`
}

type settingsSchemaBody struct {
	Retention retentionSchemaBody `json:"retention"`
}

type retentionSchemaBody struct {
	Granularities    []string `json:"granularities"`
	WindowUnits      []string `json:"window_units"`
	TierNamePattern  string   `json:"tier_name_pattern"`
	ReservedTierName string   `json:"reserved_tier_name"`
	KeepMax          int      `json:"keep_max"`
	PeriodDaysMax    int      `json:"period_days_max"`
	// DefaultTiers is the chain a config that configures neither
	// spelling resolves to. Served so the form's "restore the default
	// chain" affordance fills itself from the product's own default
	// (core/internal/config.DefaultRetentionTiers) rather than from a
	// second copy of those numbers living in the frontend, where a
	// narrowed window could be saved as an explicit chain and
	// permanently migrate a legacy config onto it.
	DefaultTiers []retentionTierBody `json:"default_tiers"`
}

// getSettings is GET /api/v1/settings: read-only (docs/
// EPIC-B-multi-nas.md §50's "view configuration"), so no CSRF and no
// destructive gate, exactly like the other GET routes in this package.
func (h *handlers) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.backend.Settings(r.Context())
	if err != nil {
		// Deliberately not err.Error(): a read failure here has no
		// classified vocabulary to map, so an unclassified error could
		// carry filesystem-internal text (the same default every other
		// handler in this package applies).
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to read settings")
		return
	}
	writeJSON(w, http.StatusOK, toSettingsResponse(settings))
}

// updateSettings is PATCH /api/v1/settings: issue #140 (B3.7)'s generic
// settings-write endpoint, the write path the Settings page's retention
// form calls.
//
// # PATCH, and only PATCH
//
// The verb is the contract: this applies exactly the settings the body
// names and leaves every other setting as it was. PUT would promise the
// opposite (the body is the whole resource), which no caller of this
// endpoint means and which would turn every omitted field into a silent
// reset — on a retention policy, a data-loss-shaped mistake. POST and PUT
// are therefore not registered at all, so a client that guesses one gets
// a 405 rather than a request that looks like it worked.
//
// # CSRF yes, destructive gate no
//
// State-changing but non-destructive under §50, in the same bucket as
// "create/edit backup set": this edits configuration and touches no
// backup data. See router.go's own comment on the route, and
// destructiveGateExemptRoutes (router_test.go), for the full reasoning
// including the last-known-good case.
func (h *handlers) updateSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBodyBytes)

	// DisallowUnknownFields, unlike every other handler in this package,
	// and for the same reason config.Load uses KnownFields(true): this is
	// the one route whose body IS a configuration edit, so a misspelled
	// key ("retenton", "protect_last_know_good") that this layer silently
	// dropped would answer 200 for a change that never happened, and the
	// operator would be looking at a settings page reporting the old
	// value with no error anywhere to explain it. Refusing is the only
	// honest answer, and it mirrors what a hand-edited YAML file already
	// gets.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var body settingsRequest
	if err := dec.Decode(&body); err != nil {
		writeSettingsDecodeError(w, err)
		return
	}

	req, err := toUpdateSettingsRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	settings, err := h.backend.UpdateSettings(r.Context(), req)
	if err != nil {
		writeSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettingsResponse(settings))
}

// toUpdateSettingsRequest translates the wire body into core/service's
// own request type, and refuses a body that names no setting at all
// rather than reporting 200 for a write that changed nothing.
//
// The nil-versus-empty distinction on Tiers is carried across
// deliberately and is the reason this is a function rather than a struct
// tag: `"tiers": []` must stay an empty non-nil slice all the way to
// core/service, which is the only layer that knows an empty chain
// reinstates the default policy and therefore has to refuse it.
func toUpdateSettingsRequest(body settingsRequest) (service.UpdateSettingsRequest, error) {
	if body.Retention == nil || body.Retention.namesNothing() {
		return service.UpdateSettingsRequest{}, errors.New("a settings write must name at least one setting to change")
	}

	update := service.RetentionUpdate{
		Timezone:             body.Retention.Timezone,
		WeekStartsOn:         body.Retention.WeekStartsOn,
		ProtectLastKnownGood: body.Retention.ProtectLastKnownGood,
	}
	if body.Retention.Tiers != nil {
		update.Tiers = make([]service.RetentionTier, 0, len(body.Retention.Tiers))
		for _, t := range body.Retention.Tiers {
			update.Tiers = append(update.Tiers, service.RetentionTier{
				Name:        t.Name,
				Granularity: t.Granularity,
				PeriodDays:  t.PeriodDays,
				Keep:        t.Keep,
				WindowUnit:  t.WindowUnit,
			})
		}
	}
	return service.UpdateSettingsRequest{Retention: &update}, nil
}

func toSettingsResponse(s service.Settings) settingsResponse {
	tiers := toTierBodies(s.Retention.Tiers)
	schema := service.RetentionSchema()
	return settingsResponse{
		Retention: retentionSettingsBody{
			Timezone:             s.Retention.Timezone,
			WeekStartsOn:         s.Retention.WeekStartsOn,
			Tiers:                tiers,
			ProtectLastKnownGood: s.Retention.ProtectLastKnownGood,
		},
		Schema: settingsSchemaBody{
			Retention: retentionSchemaBody{
				Granularities:    schema.Granularities,
				WindowUnits:      schema.WindowUnits,
				TierNamePattern:  schema.TierNamePattern,
				ReservedTierName: schema.ReservedTierName,
				KeepMax:          schema.KeepMax,
				PeriodDaysMax:    schema.PeriodDaysMax,
				DefaultTiers:     toTierBodies(schema.DefaultTiers),
			},
		},
	}
}

// toTierBodies projects a chain onto the wire, shared by the policy in
// effect and by the schema's default chain so the two cannot be spelled
// differently.
func toTierBodies(tiers []service.RetentionTier) []retentionTierBody {
	out := make([]retentionTierBody, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, retentionTierBody{
			Name:        t.Name,
			Granularity: t.Granularity,
			PeriodDays:  t.PeriodDays,
			Keep:        t.Keep,
			WindowUnit:  t.WindowUnit,
		})
	}
	return out
}

// writeSettingsDecodeError extends writeDecodeError
// (handlers_backupsets.go) with the one failure only this route can
// produce, since it is the only one that decodes with
// DisallowUnknownFields: encoding/json reports an unknown key as a plain
// *json.SyntaxError-free error whose message names the field, and
// collapsing that into "malformed JSON body" would tell an operator who
// typed "retenton" to go looking for a missing brace.
func writeSettingsDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("request body exceeds the %d byte limit", maxSettingsBodyBytes))
		return
	}
	// encoding/json has no exported type for this one; the prefix is
	// stable and documented in its own source, and the fallback below is
	// still correct if it ever changes.
	if bytes.HasPrefix([]byte(err.Error()), []byte("json: unknown field ")) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"unknown setting in request body: "+err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON body")
}

// writeSettingsError maps core/service's settings-write error vocabulary
// onto this package's one error envelope.
func writeSettingsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidRequest):
		// Safe to echo, for exactly the reason writeBackupSetError gives
		// for the identical line: an ErrInvalidRequest from this path is
		// either core/service's own refusal text or a
		// config.ValidationError built from internal/config's field
		// descriptions and the caller's own submitted values, never from
		// a state or rclone internal.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrConfigNotFileBacked):
		writeError(w, http.StatusInternalServerError, "INTERNAL", "this deployment has no configuration file to persist to")
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to update settings")
	}
}
