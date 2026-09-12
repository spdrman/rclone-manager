// This file is issue #211's activity feed: a read of the durable,
// append-only lifecycle record core has kept since the first migration.
//
// It is deliberately not a second event stream. FR-23's event catalog
// (core/internal/obs) writes the same moments to the process log, but a
// log line is not queryable after the fact, so nothing an operator can
// open in a browser could ever have been built from it.
package webhost

import (
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

	// Issue #730's diagnostic, and the whole cost of it on a default
	// deployment: one boolean test. When it IS on, the response is
	// encoded exactly once and counted on its way out, rather than
	// marshalled a second time to guess at its size - the number an
	// operator needs is what left this process, which is also the only
	// number a second marshal could disagree with.
	if h.activityDebugOn() {
		counted := &countingResponseWriter{ResponseWriter: w}
		writeJSON(counted, http.StatusOK, resp)
		h.logActivityDebug(r, limit, resp, counted.n)
		return
	}
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
// Everything here is behind h.debug (LOG_LEVEL=debug, or
// BACKUPD_DEBUG=1 as the shortcut - RM_DEBUG=1 is its deprecated
// alias), and a default INFO deployment encodes the response
// exactly once and writes no extra line. What it does NOT gate anymore
// is the correlation id: every response this package produces carries
// one, minted at the edge (requestscope.go), because the browser-side
// debug log reads X-Correlation-Id off whatever response it got and an
// id that only exists while diagnostics are on cannot join a browser's
// record of a failed read to the hop that answered it.
//
// The forwarded headers are logged because the operator's own front end
// is the other live suspect: if the request reaching this handler has
// a proto or host that disagrees with what the browser asked for, the
// hop in front rewrote it, and the fault is there rather than here.
func (h *handlers) logActivityDebug(r *http.Request, limit int, resp listActivityResponse, bytes int) {
	h.logger.Event(r.Context(), slog.LevelDebug, "activity_debug", "served activity feed",
		// The edge minted this and already put it on the response
		// (requestscope.go), so the line below and the header the
		// browser's own debug log reads name the same request. Before
		// that middleware this handler minted its own id here, which
		// meant the id only existed when diagnostics were on and could
		// not be joined to the proxy hop's record of the same request.
		slog.String("correlation_id", CorrelationIDFrom(r.Context())),
		// The browser's own per-attempt id: the one identifier that
		// still exists for a request the browser got NO response to,
		// since there is no response to carry a correlation id back on.
		slog.String("client_attempt_id", ClientAttemptIDOf(r)),
		slog.Int("limit", limit),
		// The cursor pair is here because the payload size below is
		// #730's live suspect: a request that sent no cursor and came
		// back with a next one is the operator's browser on page one of
		// a record that is longer than the page, which is exactly the
		// shape this route used to answer in a single unbounded
		// response.
		slog.String("before", r.URL.Query().Get("before")),
		slog.String("next_cursor", resp.NextCursor),
		slog.Int("event_count", len(resp.Events)),
		slog.Int("bytes", bytes),
		slog.String("x_forwarded_for", r.Header.Get("X-Forwarded-For")),
		slog.String("x_forwarded_proto", r.Header.Get("X-Forwarded-Proto")),
		slog.String("x_forwarded_host", r.Header.Get("X-Forwarded-Host")),
	)
}

// activityDebugOn is the gate, in one place so the handler above and the
// writer it wraps cannot get out of step: a counted response with no
// line to report it is pure cost.
func (h *handlers) activityDebugOn() bool {
	return h != nil && h.debug && h.logger != nil
}

// countingResponseWriter counts the body bytes actually written through
// it, which is what the debug line above reports.
//
// It embeds the ResponseWriter rather than reimplementing it, so an
// optional interface a future handler on this route needs (Flush,
// Hijack) is a change here and not a silent regression - today this
// wraps exactly one plain JSON write, on one route, only when
// diagnostics are on.
type countingResponseWriter struct {
	http.ResponseWriter
	n int
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += n
	return n, err
}
