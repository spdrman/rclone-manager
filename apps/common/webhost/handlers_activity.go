// This file is issue #211's activity feed: a read of the durable,
// append-only lifecycle record core has kept since the first migration.
//
// It is deliberately not a second event stream. FR-23's event catalog
// (core/internal/obs) writes the same moments to the process log, but a
// log line is not queryable after the fact, so nothing an operator can
// open in a browser could ever have been built from it.
package webhost

import (
	"net/http"
	"strconv"
)

// activityEventResponse is one recorded lifecycle move.
//
// It carries the raw From/To states rather than a severity and a headline.
// Which moves are worth an operator's attention, and what to call them, is
// presentation, and it belongs to whichever client is presenting; baking a
// severity into the wire would make a display decision part of the
// contract and freeze it for every other client.
type activityEventResponse struct {
	ArtifactID   string `json:"artifact_id"`
	BackupSetID  string `json:"backup_set_id"`
	SourceName   string `json:"source_name"`
	SetName      string `json:"set_name"`
	ArtifactName string `json:"artifact_name"`

	// From is omitted for the first transition: a backup being discovered
	// leaves nothing.
	From string `json:"from,omitempty"`
	To   string `json:"to"`

	OccurredAt string `json:"occurred_at"`
	Detail     string `json:"detail,omitempty"`
}

type listActivityResponse struct {
	Events []activityEventResponse `json:"events"`
}

// listActivity is GET /api/v1/activity: recent lifecycle events across
// every backup set, newest first. Read-only (§50), so no CSRF and no
// destructive gate.
//
// The limit query parameter is advisory in both directions: an absent,
// unparseable or non-positive value means the backend's default, and a
// value above its maximum is clamped rather than refused. A caller asking
// for a feed gets a feed; refusing the request over a number would fail a
// page that is only ever trying to render a list.
func (h *handlers) listActivity(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}

	events, err := h.backend.ListActivity(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list activity")
		return
	}

	resp := listActivityResponse{Events: make([]activityEventResponse, 0, len(events))}
	for _, e := range events {
		resp.Events = append(resp.Events, activityEventResponse{
			ArtifactID:   e.ArtifactID,
			BackupSetID:  e.BackupSetID,
			SourceName:   e.SourceName,
			SetName:      e.SetName,
			ArtifactName: e.ArtifactName,
			From:         e.From,
			To:           e.To,
			OccurredAt:   formatTime(e.OccurredAt),
			Detail:       e.Detail,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
