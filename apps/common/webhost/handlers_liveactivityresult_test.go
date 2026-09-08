package webhost

import (
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/service"
)

// What the wire says about how an operation went, and about the one that
// never said (issue #625).
//
// These are on the boundary rather than in core/service because that is
// where the claim has to hold. The renderer this feature exists for is in
// a browser: it colours a line by an outcome it reads off the response,
// and it says "started and has not reported an outcome" from a list it
// reads off the response. A field that core/service produces and the
// handler drops is a field that does not exist as far as any of that is
// concerned.

// TestLiveActivity_SerializesHowEachLineWent is the field that had no way
// to exist before. A completion that went well used to arrive at info,
// read as a neutral note, and every client that wanted to draw it as good
// news had to re-derive one from the event's name.
func TestLiveActivity_SerializesHowEachLineWent(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	at := time.Date(2026, 9, 7, 0, 16, 29, 0, time.UTC)
	set := transferringSet()
	set.Events = []service.LiveActivityEvent{
		{
			Sequence: 1,
			At:       at,
			Level:    "info",
			Result:   "success",
			Event:    "commit",
			Scope:    service.LiveActivityScopeSet,
			Message:  "durable commit complete",
		},
		{
			Sequence: 2,
			At:       at,
			Level:    "info",
			Event:    "hash",
			Scope:    service.LiveActivityScopeSet,
			Message:  "content hash",
		},
	}
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: at,
		Epoch:      "7f3a91c2d0b45e68",
		PollAfter:  time.Second,
		Sets:       []service.LiveActivitySet{set},
	})

	events := liveEventsOfFirstSet(t, getLiveActivity(t, router, ""))
	if events[0]["result"] != "success" {
		t.Errorf("a commit serialises result %v, want %q; without it a completion that went well is indistinguishable on the wire from a note about nothing",
			events[0]["result"], "success")
	}
	// Absent, not empty. A line that states no outcome has reported no
	// operation at all, and an "outcome": "" would be a client's evidence
	// that one was stated and came out blank.
	if v, ok := events[1]["result"]; ok {
		t.Errorf("a line that states no result serialises result %v anyway", v)
	}
}

// TestLiveActivity_SerializesTheStartThatNeverGotItsCompletion is the
// state an operator most needs named, on the wire. A cycle belongs to no
// backup set, so the deployment bucket is the only place it can be
// reported, and this is the bucket the global terminal reads.
func TestLiveActivity_SerializesTheStartThatNeverGotItsCompletion(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	started := time.Date(2026, 9, 7, 0, 16, 29, 0, time.UTC)
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: started.Add(time.Minute),
		Epoch:      "7f3a91c2d0b45e68",
		PollAfter:  time.Second,
		Sets:       []service.LiveActivitySet{},
		Deployment: &service.LiveActivityDeployment{
			Events: []service.LiveActivityEvent{{
				Sequence: 1,
				At:       started,
				Level:    "info",
				Event:    "cycle_start",
				Scope:    service.LiveActivityScopeDeployment,
				Message:  "cycle starting",
				Action:   "cycle",
				ActionID: "cycle-42",
			}},
			Unfinished: []service.LiveActivityAction{{
				Action:    "cycle",
				ActionID:  "cycle-42",
				StartedAt: started,
				Sequence:  1,
			}},
			OldestSequence: 1,
			LatestSequence: 1,
		},
	})

	body := getLiveActivity(t, router, "")
	deployment, ok := body["deployment"].(map[string]any)
	if !ok {
		t.Fatalf("the reading carries no deployment bucket: %v", body)
	}

	raw, ok := deployment["unfinished_actions"].([]any)
	if !ok {
		t.Fatalf("the deployment bucket carries no unfinished_actions array: %v", deployment)
	}
	if len(raw) != 1 {
		t.Fatalf("the deployment bucket reports %d unfinished actions and a cycle started and never ended", len(raw))
	}
	open, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("the unfinished action is %#v, want an object", raw[0])
	}
	if open["action"] != "cycle" || open["action_id"] != "cycle-42" {
		t.Errorf("the unfinished action serialises as %v / %v, want cycle / cycle-42", open["action"], open["action_id"])
	}
	if open["started_at"] != "2026-09-07T00:16:29Z" {
		t.Errorf("the unfinished action serialises started_at %v; without the moment nothing can say how long it has been quiet", open["started_at"])
	}

	// And the start line itself carries the pairing, so a client holding
	// the tail can find the line the notice is about.
	events, ok := deployment["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("the deployment bucket carries %v events, want the one start line", deployment["events"])
	}
	start, ok := events[0].(map[string]any)
	if !ok {
		t.Fatalf("the event is %#v, want an object", events[0])
	}
	if start["action"] != "cycle" || start["action_id"] != "cycle-42" {
		t.Errorf("the start line serialises action %v id %v; a pair matched by the habit of spelling one event cycle_start is what this replaces",
			start["action"], start["action_id"])
	}
	if v, ok := start["result"]; ok {
		t.Errorf("the start line serialises result %v; an action that has not finished has not gone any way yet, and that absence is what marks it as a start", v)
	}
}

// TestLiveActivity_AlwaysSerializesAnUnfinishedActionsArray keeps the
// same rule truncated and dropped are held to: a client reading an absent
// key as "nothing is open" reads a server that stopped sending it the
// same way.
func TestLiveActivity_AlwaysSerializesAnUnfinishedActionsArray(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: time.Date(2026, 9, 7, 0, 16, 38, 0, time.UTC),
		Epoch:      "7f3a91c2d0b45e68",
		PollAfter:  time.Second,
		Sets:       []service.LiveActivitySet{transferringSet()},
	})

	got := liveSets(t, getLiveActivity(t, router, ""))[0]
	raw, ok := got["unfinished_actions"]
	if !ok {
		t.Fatal("a set with nothing open omits unfinished_actions entirely; a client cannot tell that from a server that has stopped answering the question")
	}
	list, ok := raw.([]any)
	if !ok || len(list) != 0 {
		t.Errorf("a set with nothing open serialises unfinished_actions as %#v, want an empty array rather than null", raw)
	}
}

// liveEventsOfFirstSet is the first set's event objects, decoded.
func liveEventsOfFirstSet(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := liveSets(t, body)[0]["events"].([]any)
	if !ok {
		t.Fatalf("the set carries no events array: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for i, e := range raw {
		event, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("event %d is %#v, want an object", i, e)
		}
		out = append(out, event)
	}
	return out
}
