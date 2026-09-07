package webhost

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/spdrman/rclone-manager/core/service"
)

// This file is issue #573's live feed: what each backup set is DOING,
// as opposed to what state it is in.
//
// handlers_activity.go next to it is the durable, append-only lifecycle
// record, and the two are easy to confuse. That one answers "what
// happened", queryably, from a table; this one answers "what is happening
// right now", from a bounded in-memory tail the serving process is
// holding. Neither can be built from the other: a journal has no idea a
// transfer is at 4.1 MB/s, and an in-memory tail has forgotten last
// Tuesday.
//
// # Polled, not streamed
//
// The strip refreshes by asking again, with a cursor, at an interval the
// server names. That decision is about where this runs. The product is
// deployed on a NAS behind whatever reverse proxy the operator already
// had, and in the shipped two-container topology behind a second one of
// our own (webhost/serve/ui.go), and a held-open response has to survive
// every buffering and idle-timeout default in that path. Go's own
// ReverseProxy would in fact flush an event stream unbuffered, so the hop
// we ship is not the problem; the hops we do not ship are, and a design
// that only works when nothing in the path buffers is not a design.
//
// There are two more reasons that hold even with a cooperative proxy. A
// browser will not open more than a handful of concurrent connections to
// one origin over HTTP/1.1, and this strip is per backup set, so a stream
// each would exhaust that budget and wedge the rest of the page on a
// deployment with a few sets. And a long-lived connection still gets cut
// by idle timeouts, so a client needs cursor-and-resume logic anyway,
// which is the whole of the polling design and none of the streaming one.
// liveActivityFieldResponse is one field of one event, as a pair.
type liveActivityFieldResponse struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// liveActivityEventResponse is one line of the feed.
//
// It carries the engine's own event name and the engine's own level, and
// nothing this package composed. That is the same argument
// handlers_activity.go makes for the transition log one file over: what a
// moment is worth calling, and how loudly, is presentation, and it belongs
// to whichever client is presenting. The level is not such a decision, it
// is what the emitter picked when it decided a line was a warning rather
// than a note, so it is passed through rather than re-derived here.
type liveActivityEventResponse struct {
	Sequence int64                       `json:"sequence"`
	At       string                      `json:"at"`
	Level    string                      `json:"level"`
	Event    string                      `json:"event"`
	Scope    string                      `json:"scope"`
	Message  string                      `json:"message"`
	Fields   []liveActivityFieldResponse `json:"fields"`
}

// liveActivitySetResponse is one backup set's strip.
type liveActivitySetResponse struct {
	BackupSetID string `json:"backup_set_id"`
	Active      bool   `json:"active"`
	Stage       string `json:"stage,omitempty"`
	Artifact    string `json:"artifact,omitempty"`

	ArtifactsCompleted int    `json:"artifacts_completed"`
	ArtifactsTotal     *int   `json:"artifacts_total,omitempty"`
	ProgressBasis      string `json:"progress_basis"`

	BytesTransferred *int64 `json:"bytes_transferred,omitempty"`
	BytesTotal       *int64 `json:"bytes_total,omitempty"`
	BytesPerSecond   *int64 `json:"bytes_per_second,omitempty"`

	Failures   int    `json:"failures"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`

	Events         []liveActivityEventResponse `json:"events"`
	OldestSequence int64                       `json:"oldest_sequence"`
	LatestSequence int64                       `json:"latest_sequence"`
}

type liveActivityResponse struct {
	ObservedAt string `json:"observed_at"`

	// PollAfterMS is milliseconds rather than a duration string because
	// it goes straight into a client's timer, and a client that has to
	// parse "1s" to set one is a client that will eventually parse it
	// wrong.
	PollAfterMS int `json:"poll_after_ms"`

	Sets []liveActivitySetResponse `json:"sets"`
}

// getLiveActivity is GET /api/v1/activity/live. Read-only (§50), so no
// CSRF and no destructive gate.
//
// Both query parameters are advisory in the same way GET /activity's limit
// already is: absent, unparseable or non-positive means the server's own
// answer. A strip is only ever trying to render a list, and refusing one
// over a malformed number would blank the panel an operator went to look
// at, which is the one moment it exists for. backup_set is the exception
// and is refused when this deployment does not have it, because an empty
// feed for a set that does not exist reads exactly like a quiet set.
func (h *handlers) getLiveActivity(w http.ResponseWriter, r *http.Request) {
	req := service.LiveActivityRequest{BackupSetID: r.URL.Query().Get("backup_set")}
	if raw := r.URL.Query().Get("since"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			req.Since = parsed
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			req.Limit = parsed
		}
	}

	live, err := h.backend.LiveActivity(r.Context(), req)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to read live activity")
		return
	}

	resp := liveActivityResponse{
		ObservedAt:  formatTime(live.ObservedAt),
		PollAfterMS: int(live.PollAfter.Milliseconds()),
		// A non-nil empty slice, so a deployment with no backup sets
		// serialises "sets": [] rather than null and a client has one
		// shape to render instead of two.
		Sets: make([]liveActivitySetResponse, 0, len(live.Sets)),
	}
	for _, s := range live.Sets {
		resp.Sets = append(resp.Sets, liveActivitySetOf(s))
	}
	writeJSON(w, http.StatusOK, resp)
}

func liveActivitySetOf(s service.LiveActivitySet) liveActivitySetResponse {
	out := liveActivitySetResponse{
		BackupSetID:        s.BackupSetID,
		Active:             s.Active,
		Stage:              s.Stage,
		Artifact:           s.Artifact,
		ArtifactsCompleted: s.ArtifactsCompleted,
		ArtifactsTotal:     s.ArtifactsTotal,
		ProgressBasis:      s.ProgressBasis,
		BytesTransferred:   s.BytesTransferred,
		BytesTotal:         s.BytesTotal,
		BytesPerSecond:     s.BytesPerSecond,
		Failures:           s.Failures,
		Events:             make([]liveActivityEventResponse, 0, len(s.Events)),
		OldestSequence:     s.OldestSequence,
		LatestSequence:     s.LatestSequence,
	}
	if s.StartedAt != nil {
		out.StartedAt = formatTime(*s.StartedAt)
	}
	if s.FinishedAt != nil {
		out.FinishedAt = formatTime(*s.FinishedAt)
	}
	for _, e := range s.Events {
		event := liveActivityEventResponse{
			Sequence: e.Sequence,
			At:       formatTime(e.At),
			Level:    e.Level,
			Event:    e.Event,
			Scope:    e.Scope,
			Message:  e.Message,
			Fields:   make([]liveActivityFieldResponse, 0, len(e.Fields)),
		}
		for _, f := range e.Fields {
			event.Fields = append(event.Fields, liveActivityFieldResponse{Key: f.Key, Value: f.Value})
		}
		out.Events = append(out.Events, event)
	}
	return out
}
