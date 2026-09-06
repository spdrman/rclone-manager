package lifecycle

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file holds the safety property issue #220's new edge is allowed to
// exist because of: an artifact that has been re-trusted after quarantine
// never authorises the destruction of its remote source, forever, no
// matter how completely it passes every other FR-15 check afterwards.
//
// Without that property the new edge would be a way to launder a corrupt
// artifact back into delete eligibility, which is the exact failure this
// whole repository exists to prevent.

// reinstatedDeleteFixture builds the strongest possible case FOR deleting:
// a real journal row walked to COMMITTED, a real local file whose size and
// sha256 both match what was recorded, and a remote whose Stat returns the
// same hash, so model.CompareIdentity reaches an unchanged verdict at
// strong confidence and every other FR-15 check clears.
//
// quarantined controls the one difference under test: whether that
// artifact took a detour through QUARANTINED and was reinstated, or
// reached COMMITTED the ordinary way and stayed there.
func reinstatedDeleteFixture(t *testing.T, reinstate bool) (*state.Journal, model.ArtifactID, *deleteTransport) {
	t.Helper()
	j, artifact, tp := stableFixture(t)

	if !reinstate {
		return j, artifact, tp
	}

	ctx := context.Background()
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "fixture-quarantine",
		From: string(Committed), To: string(Quarantined),
		Detail: "reconciliation found the durable local copy invalid",
	}); err != nil {
		t.Fatalf("fixture -> QUARANTINED: %v", err)
	}
	if _, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "fixture-reinstate",
		Evidence:   conclusiveEvidence(),
	}); err != nil {
		t.Fatalf("fixture reinstatement: %v", err)
	}
	return j, artifact, tp
}

// The decisive test. The two halves differ in exactly one thing: whether
// the artifact was ever reinstated out of quarantine.
//
// The positive control is not decoration here. Every other FR-15 check has
// to actually clear for the refusal to mean anything: if the fixture were
// undeletable for some unrelated reason (a size mismatch, a weak identity
// comparison, an unknown completion strategy), the "was never deleted"
// assertion below would pass for a reason that has nothing to do with
// quarantine, and the property would be untested. The control proves the
// same fixture deletes when the detour is removed.
func TestDeleteRemoteRefusesAnArtifactReinstatedFromQuarantine(t *testing.T) {
	ctx := context.Background()

	t.Run("positive control: the same artifact deletes when it was never quarantined", func(t *testing.T) {
		j, artifact, tp := reinstatedDeleteFixture(t, false)

		out, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
			Artifact:           artifact,
			AttemptKey:         "attempt-1",
			CompletionStrategy: "rename",
		})
		if err != nil {
			t.Fatalf("the control fixture was refused, so the test below proves nothing: %v", err)
		}
		if out.Record.State != string(Complete) {
			t.Fatalf("state = %q, want COMPLETE", out.Record.State)
		}
		if tp.deleteCalls != 1 {
			t.Fatalf("transport.DeleteRemote called %d times, want exactly 1", tp.deleteCalls)
		}
	})

	t.Run("a reinstated artifact is refused", func(t *testing.T) {
		j, artifact, tp := reinstatedDeleteFixture(t, true)

		before := transitionCount(t, j, artifact)

		_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
			Artifact:           artifact,
			AttemptKey:         "attempt-1",
			CompletionStrategy: "rename",
		})

		refusal := requireRefusal(t, err, reinstatementCheck)
		if refusal.Reason == "" {
			t.Error("the refusal carries no reason; an operator reading a preserved remote has to be told why")
		}
		if tp.deleteCalls != 0 {
			t.Fatalf("transport.DeleteRemote called %d times, want 0: a reinstated artifact must never authorise destroying its remote source", tp.deleteCalls)
		}

		// The refusal must land before intent is recorded, so it leaves no
		// mark at all. Counted in the append-only log rather than read off
		// UpdatedAt: a same-state write stamps UpdatedAt with the caller's
		// own OccurredAt, so an unchanged UpdatedAt does not prove an
		// unchanged journal.
		if after := transitionCount(t, j, artifact); after != before {
			t.Fatalf("state_transitions grew from %d to %d rows on a pre-write refusal", before, after)
		}
		rec, getErr := j.Get(ctx, artifact)
		if getErr != nil {
			t.Fatalf("Get: %v", getErr)
		}
		if rec.State != string(Committed) {
			t.Fatalf("state = %q, want it left at COMMITTED", rec.State)
		}
	})
}

// The forfeiture is permanent, not a cooling-off period: re-running the
// whole delete attempt with a fresh AttemptKey, which is what a caller does
// when it wants every check re-evaluated from scratch, still refuses.
func TestDeleteRemoteForfeitureSurvivesAFreshAttempt(t *testing.T) {
	ctx := context.Background()
	j, artifact, tp := reinstatedDeleteFixture(t, true)

	for _, key := range []string{"attempt-1", "attempt-2", "attempt-3"} {
		_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
			Artifact:           artifact,
			AttemptKey:         key,
			CompletionStrategy: "rename",
		})
		_ = requireRefusal(t, err, reinstatementCheck)
	}
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times across three attempts, want 0", tp.deleteCalls)
	}
}

// The gate reads its rule out of the Transitions table rather than from a
// hand-maintained list, so a future edge that returns an artifact from
// quarantine into a state the rest of the system treats as a durable
// restore point is covered the moment it is declared, without anyone
// remembering to come back here.
//
// This test is what makes that claim testable rather than aspirational: it
// walks the real table and requires every such edge to be one the delete
// gate consults.
func TestEveryQuarantineExitIntoADurableStateForfeitsRemoteDeletion(t *testing.T) {
	covered := make(map[Transition]bool, len(ReinstatementEdges()))
	for _, e := range ReinstatementEdges() {
		covered[e] = true
	}
	if len(covered) == 0 {
		t.Fatal("ReinstatementEdges() is empty; this test would pass vacuously")
	}

	found := 0
	for _, tr := range Transitions {
		if !IsQuarantineState(tr.From) || !IsDurableRestorePoint(tr.To) {
			continue
		}
		found++
		if !covered[tr] {
			t.Errorf("%s -> %s returns an artifact from quarantine to a durable restore point but is not one of ReinstatementEdges(), so the FR-15 delete gate would not know about it", tr.From, tr.To)
		}
	}
	if found == 0 {
		t.Fatal("no edge from a quarantine state into a durable restore point exists in Transitions; either the table or this test's premise is wrong")
	}

	// And nothing else may be claimed as a reinstatement edge: an entry
	// here that the table does not declare would silently forfeit deletion
	// for artifacts that were never reinstated.
	for e := range covered {
		if !IsQuarantineState(e.From) || !IsDurableRestorePoint(e.To) {
			t.Errorf("ReinstatementEdges() contains %s -> %s, which is not a quarantine exit into a durable restore point", e.From, e.To)
		}
		if !transitionSet[e] {
			t.Errorf("ReinstatementEdges() contains %s -> %s, which the Transitions table does not declare", e.From, e.To)
		}
	}
}
