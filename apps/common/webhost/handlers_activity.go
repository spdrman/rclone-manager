// This file is issue #211's activity feed: a read of the durable,
// append-only lifecycle record core has kept since the first migration.
//
// It is deliberately not a second event stream. FR-23's event catalog
// (core/internal/obs) writes the same moments to the process log, but a
// log line is not queryable after the fact, so nothing an operator can
// open in a browser could ever have been built from it.
package webhost

import (
	"encoding/json"
	"log/slog"
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
	// NextCursor is where this page ended, for a caller that wants the
	// events behind it: send it back as ?before=. Absent when this page
	// was not full, which is as much as the read can say cheaply about
	// whether anything older exists.
	NextCursor string `json:"next_cursor,omitempty"`
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
//
// before is the same kind of advisory: an opaque cursor from an earlier
// response's next_cursor, naming where that page ended, and a value this
// feed did not issue reads as no cursor at all rather than a 400. It is
// what keeps the payload bounded AND the record reachable: without it the
// only way to see past the newest page is to raise limit, and the page
// that opens on a year-old deployment then carries the whole history
// (issue #730).
func (h *handlers) listActivity(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}

	events, nextCursor, err := h.backend.ListActivity(r.Context(), limit, r.URL.Query().Get("before"))
	if err != nil {
		h.internalError(w, r, "INTERNAL", "failed to list activity", err)
		return
	}

	resp := listActivityResponse{
		Events:     make([]activityEventResponse, 0, len(events)),
		NextCursor: nextCursor,
	}
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
	h.logActivityDebug(w, r, limit, resp)
	writeJSON(w, http.StatusOK, resp)
}

// logActivityDebug is issue #730's server half: a UGREEN NAS deployment
// where the browser's fetch('/api/v1/activity') fails with a bare
// TypeError — no HTTP response reaches JS at all — while curl against
// the same route answers 401 cleanly. A synthetic three-container rig
// did not reproduce it, so the only way left is to record, on the real
// deployment, what this handler actually produced for the request the
// browser could not read.
//
// Everything here is behind h.debug (RM_DEBUG=1 or LOG_LEVEL=debug), and
// a default INFO deployment neither marshals the payload a second time
// nor sets a header it did not set before. That includes the correlation
// id: a 200 carries none normally, but the browser-side debug log reads
// X-Correlation-Id off every response, and without one on the success
// path there is nothing to join a browser's record of a failed read to
// the line below describing what was sent.
//
// The forwarded headers are logged because the operator's own front end
// is the other live suspect: if the request reaching this handler has
// a proto or host that disagrees with what the browser asked for, the
// hop in front rewrote it, and the fault is there rather than here.
func (h *handlers) logActivityDebug(w http.ResponseWriter, r *http.Request, limit int, resp listActivityResponse) {
	if h == nil || !h.debug || h.logger == nil {
		return
	}

	id := correlationID()
	w.Header().Set("X-Correlation-Id", id)

	// Marshalled a second time purely to report the size the client
	// should have received; writeJSON does its own encoding and is left
	// exactly as it is on every other path. An error here is not worth
	// reporting as anything but the size it produced, since writeJSON is
	// about to hit the same one.
	body, _ := json.Marshal(resp)

	h.logger.Event(r.Context(), slog.LevelDebug, "activity_debug", "served activity feed",
		slog.String("correlation_id", id),
		slog.Int("limit", limit),
		// The cursor pair is here because the payload size above is #730's
		// live suspect: a request that sent no cursor and came back with a
		// next one is the operator's browser on page one of a record that
		// is longer than the page, which is exactly the shape this route
		// used to answer in a single unbounded response.
		slog.String("before", r.URL.Query().Get("before")),
		slog.String("next_cursor", resp.NextCursor),
		slog.Int("event_count", len(resp.Events)),
		slog.Int("bytes", len(body)),
		slog.String("x_forwarded_for", r.Header.Get("X-Forwarded-For")),
		slog.String("x_forwarded_proto", r.Header.Get("X-Forwarded-Proto")),
		slog.String("x_forwarded_host", r.Header.Get("X-Forwarded-Host")),
	)
}
