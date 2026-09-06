package lifecycle

// This file answers, for a whole backup set at once, the question
// remotedelete.go's lastReinstatement answers for one artifact: has this
// artifact been re-trusted after being distrusted, and therefore
// permanently forfeited the deletion of its remote source?
//
// # Why a second reader of the same fact is safe here
//
// It normally would not be. Two independently-maintained answers to a
// safety question drift, and the one that drifts silently is the reporting
// one: an artifact the gate refuses but the report never counts is a
// permanent retention nobody is told about, which is the entire complaint
// issue #227 makes about the state of things before this existed.
//
// So this is not a second answer. Both readers derive their edge list from
// the same reinstatementEdges value, which is itself derived from the
// Transitions table (see machine.go), and both read the same append-only
// state_transitions rows. A new quarantine exit into a durable restore
// point is therefore covered by the delete gate AND appears in the count
// of forfeited deletes the moment it is declared, without anyone having to
// remember either place.
// TestReinstatedArtifactsAgreesWithTheDeleteGatesOwnRead walks a real
// journal and proves the two answers match artifact by artifact.
//
// # Why it is not a method on Deps.Journal
//
// Deps is what a lifecycle STEP is handed, and every method on that
// interface is something a step needs in order to decide one artifact's
// fate. This is a reporting read over a whole backup set; it moves
// nothing, writes nothing, and no step calls it. Giving it its own
// one-method interface keeps the step surface honest and keeps the fakes
// that exist to test steps from growing a method none of them use.

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// ReinstatementLog is the one journal read ReinstatedArtifacts needs:
// which artifacts in a backup set have a given set of edges recorded in
// the append-only transition log. *state.Journal satisfies it.
type ReinstatementLog interface {
	ArtifactsWithAnyTransition(ctx context.Context, set model.BackupSetID, edges []state.TransitionEdge) ([]model.ArtifactID, error)
}

// ReinstatedArtifacts returns every artifact in set that has been
// reinstated out of quarantine, and whose remote delete DeleteRemote will
// therefore refuse forever.
//
// The answer is about history, not about current state. A reinstated
// artifact and one that was never distrusted are both simply COMMITTED on
// the artifacts row; only the append-only log distinguishes them, which is
// exactly why the refusal reads the log and why this does too.
//
// An error is an error. Nothing here turns a failed read into an empty
// slice: "no artifact in this set has been reinstated" and "the journal
// could not be asked" are different facts, and only one of them is
// reassuring.
func ReinstatedArtifacts(ctx context.Context, log ReinstatementLog, set model.BackupSetID) ([]model.ArtifactID, error) {
	edges := make([]state.TransitionEdge, 0, len(reinstatementEdges))
	for _, e := range reinstatementEdges {
		edges = append(edges, state.TransitionEdge{From: string(e.From), To: string(e.To)})
	}

	out, err := log.ArtifactsWithAnyTransition(ctx, set, edges)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: reinstated artifacts for %s: %w", set, err)
	}
	return out, nil
}
