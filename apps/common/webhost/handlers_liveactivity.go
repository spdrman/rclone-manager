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
	Sequence int64  `json:"sequence"`
	At       string `json:"at"`
	Level    string `json:"level"`

	// Result is how the operation this line reports WENT, and it is not
	// a second spelling of Level (issue #625). The level says how loudly
	// the emitter logged; the result says how the thing turned out, and
	// there is no "success" severity to fold the two into, because a
	// level is an ordering every reader treats as a threshold. Before
	// this field existed, a completion that went well arrived at info,
	// read as a neutral note, and every client that wanted to draw it as
	// good news re-derived one from event names and lifecycle state
	// values, with anything the derivation had never heard of staying
	// grey. Passed through, never composed here, for the same reason
	// Level is.
	//
	// Named result and not outcome deliberately. connection_test carries
	// a field of its own called outcome, and LiveActivitySet.Outcome in
	// this same response is a third vocabulary again: a client reading
	// set.outcome beside set.events[i].result is told those are different
	// questions rather than left to notice it from two enums.
	Result string `json:"result,omitempty"`

	// Action and ActionID pair a start with its completion. Omitted on
	// every line that is neither half of a pair; a line carrying an id
	// and no result is a start, and one carrying both closes it.
	Action   string `json:"action,omitempty"`
	ActionID string `json:"action_id,omitempty"`

	Event   string                      `json:"event"`
	Scope   string                      `json:"scope"`
	Message string                      `json:"message"`
	Fields  []liveActivityFieldResponse `json:"fields"`
}

// liveActivityActionResponse is one action that started and has not
// reported an outcome (issue #625).
type liveActivityActionResponse struct {
	Action    string `json:"action"`
	ActionID  string `json:"action_id"`
	StartedAt string `json:"started_at"`
	Sequence  int64  `json:"sequence"`
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

	Failures int `json:"failures"`

	// Outcome is how the last pass ENDED, and it is a different fact from
	// Failures beside it. A pass whose reconcile or discovery failed
	// never reached an artifact, so it leaves Failures at zero and
	// ArtifactsTotal absent, and a client drawing its headline from those
	// two alone paints the earliest failure there is the way it paints a
	// set with nothing to do. Omitted until a pass has ended.
	Outcome string `json:"outcome,omitempty"`

	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`

	Events []liveActivityEventResponse `json:"events"`

	// UnfinishedActions is every action inside this set that announced
	// itself and has not said how it went. Always present rather than
	// omitted when empty, so a client has one shape to render: an absent
	// key and an empty list would otherwise read the same, and one of
	// them is a server that stopped answering the question.
	UnfinishedActions []liveActivityActionResponse `json:"unfinished_actions"`

	// Truncated and Dropped are the two ways this tail can be less than
	// what the caller asked for, and they are always present rather than
	// omitted-when-false: a client reading an absent key as "nothing is
	// missing" reads a server that stopped sending them the same way, and
	// the whole point of both is that silence about a gap is what this
	// feed must never do.
	Truncated bool `json:"truncated"`
	Dropped   bool `json:"dropped"`

	OldestSequence int64 `json:"oldest_sequence"`
	LatestSequence int64 `json:"latest_sequence"`
}

// liveActivityDeploymentResponse is the log that belongs to no single
// backup set (issue #593).
//
// It used to be copied into every set's feed, on the argument that
// dropping it would hide it and pinning it to one set would put it on the
// wrong screen. Both of those are true and both alternatives are worse;
// what was missing was a third place to put it, which is what issue
// #599's global terminal is. So the shared ring is served here, once,
// and a set's feed carries that set's own ring alone.
type liveActivityDeploymentResponse struct {
	Events []liveActivityEventResponse `json:"events"`

	// UnfinishedActions is every deployment-wide action still owing an
	// outcome. A cycle is the one this matters most for: it belongs to no
	// single set, so this bucket is the only place a cycle that started
	// and went quiet can be reported at all.
	UnfinishedActions []liveActivityActionResponse `json:"unfinished_actions"`

	// The same two honesty flags a set's strip carries, always present
	// rather than omitted when false, for the same reason: a client
	// reading an absent key as "nothing is missing" reads a server that
	// stopped sending them the same way.
	Truncated bool `json:"truncated"`
	Dropped   bool `json:"dropped"`

	OldestSequence int64 `json:"oldest_sequence"`
	LatestSequence int64 `json:"latest_sequence"`
}

type liveActivityResponse struct {
	ObservedAt string `json:"observed_at"`

	// Epoch names the process this reading came from and changes on every
	// start. It is what lets a client notice a restart: the sequence
	// counter its cursor is built from is per-process and starts again at
	// zero, so a browser that kept polling across one would ask for
	// everything after a number the new process has not reached, be told
	// there is nothing new, and go on showing a dead cycle's log.
	Epoch string `json:"epoch"`

	// PollAfterMS is milliseconds rather than a duration string because
	// it goes straight into a client's timer, and a client that has to
	// parse "1s" to set one is a client that will eventually parse it
	// wrong.
	PollAfterMS int `json:"poll_after_ms"`

	Sets []liveActivitySetResponse `json:"sets"`

	// Deployment is omitted, not empty, when this reading was narrowed to
	// one backup set: a caller that named a set asked about that set, and
	// an empty bucket would read as "the deployment has said nothing"
	// rather than "you did not ask".
	Deployment *liveActivityDeploymentResponse `json:"deployment,omitempty"`
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
	// scope is advisory in the same way since and limit are: the one
	// value it takes narrows the reading, and anything else is ignored
	// rather than refused, because blanking a panel over a query string
	// is the one thing this feed exists not to do.
	if r.URL.Query().Get("scope") == service.LiveActivityScopeDeployment {
		req.DeploymentOnly = true
	}
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
		h.internalError(w, r, "INTERNAL", "failed to read live activity", err)
		return
	}

	resp := liveActivityResponse{
		ObservedAt:  formatTime(live.ObservedAt),
		Epoch:       live.Epoch,
		PollAfterMS: int(live.PollAfter.Milliseconds()),
		// A non-nil empty slice, so a deployment with no backup sets
		// serialises "sets": [] rather than null and a client has one
		// shape to render instead of two.
		Sets: make([]liveActivitySetResponse, 0, len(live.Sets)),
	}
	for _, s := range live.Sets {
		resp.Sets = append(resp.Sets, liveActivitySetOf(s))
	}
	if live.Deployment != nil {
		resp.Deployment = &liveActivityDeploymentResponse{
			Events:            liveActivityEventsOf(live.Deployment.Events),
			UnfinishedActions: liveActivityActionsOf(live.Deployment.Unfinished),
			Truncated:         live.Deployment.Truncated,
			Dropped:           live.Deployment.Dropped,
			OldestSequence:    live.Deployment.OldestSequence,
			LatestSequence:    live.Deployment.LatestSequence,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// liveActivityEventsOf renders one bucket's events. Shared by a set's
// strip and the deployment's bucket, so the two can never drift into
// describing the same event differently.
func liveActivityEventsOf(events []service.LiveActivityEvent) []liveActivityEventResponse {
	out := make([]liveActivityEventResponse, 0, len(events))
	for _, e := range events {
		event := liveActivityEventResponse{
			Sequence: e.Sequence,
			At:       formatTime(e.At),
			Level:    e.Level,
			Result:   e.Result,
			Action:   e.Action,
			ActionID: e.ActionID,
			Event:    e.Event,
			Scope:    e.Scope,
			Message:  e.Message,
			Fields:   make([]liveActivityFieldResponse, 0, len(e.Fields)),
		}
		for _, f := range e.Fields {
			event.Fields = append(event.Fields, liveActivityFieldResponse{Key: f.Key, Value: f.Value})
		}
		out = append(out, event)
	}
	return out
}

// liveActivityActionsOf renders one bucket's unfinished actions. A
// non-nil empty slice, so a bucket with nothing open serialises
// "unfinished_actions": [] rather than null and a client has one shape to
// render instead of two.
func liveActivityActionsOf(actions []service.LiveActivityAction) []liveActivityActionResponse {
	out := make([]liveActivityActionResponse, 0, len(actions))
	for _, a := range actions {
		out = append(out, liveActivityActionResponse{
			Action:    a.Action,
			ActionID:  a.ActionID,
			StartedAt: formatTime(a.StartedAt),
			Sequence:  a.Sequence,
		})
	}
	return out
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
		Outcome:            s.Outcome,
		Events:             liveActivityEventsOf(s.Events),
		UnfinishedActions:  liveActivityActionsOf(s.Unfinished),
		Truncated:          s.Truncated,
		Dropped:            s.Dropped,
		OldestSequence:     s.OldestSequence,
		LatestSequence:     s.LatestSequence,
	}
	if s.StartedAt != nil {
		out.StartedAt = formatTime(*s.StartedAt)
	}
	if s.FinishedAt != nil {
		out.FinishedAt = formatTime(*s.FinishedAt)
	}
	return out
}
