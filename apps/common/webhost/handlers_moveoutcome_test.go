package webhost

import (
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// TestGetOperation_SerializesTheMoveOutcome is FR-30's counts reaching a
// polling client under the names api/v1/openapi.json declares.
//
// The contract gate next door checks that the handler type has the same
// SHAPE as the schema, which a handler that always left the object nil
// would pass. This is the other half: the values core/service read off
// the recorded summary are the values that go out.
func TestGetOperation_SerializesTheMoveOutcome(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	seedOperation(tr.backend, service.Operation{
		ID:     "op_moved",
		Action: service.ActionRunCycle,
		Status: "completed",
		Cycle: &service.CycleOutcome{
			BackupSetsProcessed: 2,
			ArtifactsWalked:     6,
			ArtifactsThrough:    6,
			Moves:               &service.CycleMoveOutcome{Attempted: 4, Landed: 0},
		},
	})

	body := getOperation(t, tr.router, "op_moved")
	cycle, ok := body["cycle"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries no cycle object: %v", body)
	}
	moves, ok := cycle["moves"].(map[string]any)
	if !ok {
		t.Fatalf("cycle carries no moves object, so a cycle where every move was refused is indistinguishable here from one where every move landed: %v", cycle)
	}
	if got := moves["attempted"]; got != float64(4) {
		t.Errorf("moves.attempted = %#v, want 4", got)
	}
	if got := moves["landed"]; got != float64(0) {
		t.Errorf("moves.landed = %#v, want 0", got)
	}
	// The pair has to be able to disagree, or nothing here is being read.
	if moves["attempted"] == moves["landed"] {
		t.Error("attempted and landed came back equal against a fixture where they differ")
	}
	for _, forbidden := range []string{"reason", "refused", "error", "detail"} {
		if _, present := moves[forbidden]; present {
			t.Errorf("moves carries a %q field; the engine's refusal sentence is built out of transport errors and FR-33 keeps it off this boundary", forbidden)
		}
	}
}

// TestGetOperation_OmitsTheMoveOutcomeWhenTheSummaryDoesNotCarryIt is the
// compatibility half.
//
// A run cycle recorded before this build has no move counts in its
// summary, so core/service reports none, and the response must carry NO
// moves key: not a null, and above all not a pair of zeroes, which a
// client would render as a cycle that was due to move nothing when the
// truth is that nobody wrote it down.
func TestGetOperation_OmitsTheMoveOutcomeWhenTheSummaryDoesNotCarryIt(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	seedOperation(tr.backend, service.Operation{
		ID:     "op_old",
		Action: service.ActionRunCycle,
		Status: "completed",
		Cycle:  &service.CycleOutcome{BackupSetsProcessed: 1, ArtifactsWalked: 3, ArtifactsThrough: 3},
	})

	body := getOperation(t, tr.router, "op_old")
	cycle, ok := body["cycle"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries no cycle object: %v", body)
	}
	if raw, present := cycle["moves"]; present {
		t.Errorf("cycle.moves = %#v for a summary that recorded none; absent and a pair of zeroes are different answers", raw)
	}
	// The control: the ingestion counts this cycle DID record are still
	// there, so the assertion above is not passing against an empty body.
	if got := cycle["artifacts_walked"]; got != float64(3) {
		t.Errorf("cycle.artifacts_walked = %#v, want 3", got)
	}
}
