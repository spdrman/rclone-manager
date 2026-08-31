package state

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// ArtifactsWithAnyTransition is the set-wide half of the question
// LastTransition answers one artifact at a time. Issue #227 needs it
// because a health pass reports on a whole backup set: asking
// LastTransition once per artifact per declared edge is two round trips
// per artifact per pass, on a table that only ever grows.
//
// The two reads have to agree, so the tests below are written against the
// same fixture shape lasttransition_test.go uses, and the population under
// test always contains a member that must be excluded for each distinct
// reason it could be excluded for.

// walkToCommitted records the ordinary pipeline path for artifact, so a
// test's controls are artifacts that genuinely reached COMMITTED rather
// than rows that never moved.
func walkToCommitted(t *testing.T, j *Journal, artifact model.ArtifactID, keyPrefix string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := j.Discover(ctx, artifact, keyPrefix+"-discover", "/incoming/"+artifact.Name, RemoteIdentity{}, at); err != nil {
		t.Fatalf("Discover %s: %v", artifact, err)
	}
	for i, tr := range []Transition{
		{From: "DISCOVERED", To: "TRANSFERRING"},
		{From: "TRANSFERRING", To: "TRANSFERRED"},
		{From: "TRANSFERRED", To: "VERIFYING"},
		{From: "VERIFYING", To: "VERIFIED"},
		{From: "VERIFIED", To: "COMMITTING"},
		{From: "COMMITTING", To: "COMMITTED"},
	} {
		tr.Artifact = artifact
		tr.Key = keyPrefix + "-" + tr.To
		tr.OccurredAt = at.Add(time.Duration(i) * time.Minute)
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("%s -> %s: %v", artifact, tr.To, err)
		}
	}
}

func recordEdge(t *testing.T, j *Journal, artifact model.ArtifactID, key, from, to string, at time.Time) {
	t.Helper()
	if _, err := j.RecordTransition(context.Background(), Transition{
		Artifact: artifact, Key: key, From: from, To: to, OccurredAt: at,
	}); err != nil {
		t.Fatalf("%s: %s -> %s: %v", artifact, from, to, err)
	}
}

func mustArtifactNamed(t *testing.T, set model.BackupSetID, name string) model.ArtifactID {
	t.Helper()
	id, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%s): %v", name, err)
	}
	return id
}

// reinstatementEdges is the pair internal/lifecycle derives from its own
// Transitions table. It is spelled out here rather than imported because
// internal/lifecycle imports this package; the test that proves the two
// cannot drift lives on the lifecycle side, where both are in scope.
var testReinstatementEdges = []TransitionEdge{
	{From: "QUARANTINED", To: "COMMITTED"},
	{From: "QUARANTINED_LOST", To: "COMPLETE"},
}

// The count has to be a real number, not zero-or-one, and every artifact
// that must NOT be counted has to be present for its own reason:
//
//   - one that never left the happy path (never distrusted at all);
//   - one that is quarantined right now and has not been reinstated (the
//     control the whole count would otherwise pass by counting);
//   - one that went round the loop the other way, COMMITTED -> QUARANTINED,
//     which shares both state names with the edge under test and would be
//     matched by a query that lost its direction.
func TestArtifactsWithAnyTransitionCountsOnlyTheDeclaredEdges(t *testing.T) {
	j, _ := openJournal(t)
	c := context.Background()
	base := testArtifact(t).Set
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// Two reinstated artifacts, one per declared edge.
	reinstatedCommitted := mustArtifactNamed(t, base, "reinstated-committed.dump")
	walkToCommitted(t, j, reinstatedCommitted, "rc", t0)
	recordEdge(t, j, reinstatedCommitted, "rc-q", "COMMITTED", "QUARANTINED", t0.Add(time.Hour))
	recordEdge(t, j, reinstatedCommitted, "rc-r", "QUARANTINED", "COMMITTED", t0.Add(2*time.Hour))

	reinstatedComplete := mustArtifactNamed(t, base, "reinstated-complete.dump")
	walkToCommitted(t, j, reinstatedComplete, "rl", t0)
	recordEdge(t, j, reinstatedComplete, "rl-p", "COMMITTED", "REMOTE_DELETE_PENDING", t0.Add(time.Hour))
	recordEdge(t, j, reinstatedComplete, "rl-c", "REMOTE_DELETE_PENDING", "COMPLETE", t0.Add(2*time.Hour))
	recordEdge(t, j, reinstatedComplete, "rl-l", "COMPLETE", "QUARANTINED_LOST", t0.Add(3*time.Hour))
	recordEdge(t, j, reinstatedComplete, "rl-r", "QUARANTINED_LOST", "COMPLETE", t0.Add(4*time.Hour))

	// A third reinstatement, so the answer is a number rather than a pair
	// a hand-written "did I get both" assertion could be satisfied by.
	reinstatedAgain := mustArtifactNamed(t, base, "reinstated-again.dump")
	walkToCommitted(t, j, reinstatedAgain, "ra", t0)
	recordEdge(t, j, reinstatedAgain, "ra-q", "COMMITTED", "QUARANTINED", t0.Add(time.Hour))
	recordEdge(t, j, reinstatedAgain, "ra-r", "QUARANTINED", "COMMITTED", t0.Add(2*time.Hour))

	// Control 1: never distrusted.
	neverQuarantined := mustArtifactNamed(t, base, "never-quarantined.dump")
	walkToCommitted(t, j, neverQuarantined, "nq", t0)

	// Control 2: quarantined right now, never reinstated. This is the one
	// a count that counted "artifacts that have ever been quarantined"
	// would wrongly include, and it is the reason that count would look
	// right on a fixture with no such artifact in it.
	stillQuarantined := mustArtifactNamed(t, base, "still-quarantined.dump")
	walkToCommitted(t, j, stillQuarantined, "sq", t0)
	recordEdge(t, j, stillQuarantined, "sq-q", "COMMITTED", "QUARANTINED", t0.Add(time.Hour))

	// Control 3: the same two state names in the other direction, which a
	// query that compared an unordered pair would match.
	//
	// stillQuarantined already carries COMMITTED -> QUARANTINED, so this
	// is asserted through it rather than through a fourth artifact.

	got, err := j.ArtifactsWithAnyTransition(c, base, testReinstatementEdges)
	if err != nil {
		t.Fatalf("ArtifactsWithAnyTransition: %v", err)
	}

	want := map[model.ArtifactID]bool{
		reinstatedCommitted: true,
		reinstatedComplete:  true,
		reinstatedAgain:     true,
	}
	if len(got) != len(want) {
		t.Fatalf("ArtifactsWithAnyTransition returned %d artifact(s) %v, want exactly %d", len(got), got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("ArtifactsWithAnyTransition returned %s, which took no reinstatement edge", id)
		}
		delete(want, id)
	}
	for id := range want {
		t.Errorf("ArtifactsWithAnyTransition did not return %s, which did take a reinstatement edge", id)
	}
}

// The read is per backup set (FR-7): a health pass for one set must never
// have its count inflated by another set's history. A query that dropped
// the source/backup_set predicate would still pass the test above, whose
// fixture has only one set in it.
func TestArtifactsWithAnyTransitionIsScopedToOneBackupSet(t *testing.T) {
	j, _ := openJournal(t)
	c := context.Background()
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	mine := testArtifact(t).Set
	other, err := model.NewBackupSetID("production", "postgres-replica")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	here := mustArtifactNamed(t, mine, "here.dump")
	walkToCommitted(t, j, here, "here", t0)
	recordEdge(t, j, here, "here-q", "COMMITTED", "QUARANTINED", t0.Add(time.Hour))
	recordEdge(t, j, here, "here-r", "QUARANTINED", "COMMITTED", t0.Add(2*time.Hour))

	elsewhere := mustArtifactNamed(t, other, "elsewhere.dump")
	walkToCommitted(t, j, elsewhere, "else", t0)
	recordEdge(t, j, elsewhere, "else-q", "COMMITTED", "QUARANTINED", t0.Add(time.Hour))
	recordEdge(t, j, elsewhere, "else-r", "QUARANTINED", "COMMITTED", t0.Add(2*time.Hour))

	got, err := j.ArtifactsWithAnyTransition(c, mine, testReinstatementEdges)
	if err != nil {
		t.Fatalf("ArtifactsWithAnyTransition: %v", err)
	}
	if len(got) != 1 || got[0] != here {
		t.Fatalf("ArtifactsWithAnyTransition(%s) = %v, want exactly [%s]: the read is not scoped to one backup set", mine, got, here)
	}

	// The positive control for the assertion above: the other set really
	// does have a reinstated artifact, so the empty half is a scoping
	// result and not an empty table.
	otherGot, err := j.ArtifactsWithAnyTransition(c, other, testReinstatementEdges)
	if err != nil {
		t.Fatalf("ArtifactsWithAnyTransition(other): %v", err)
	}
	if len(otherGot) != 1 || otherGot[0] != elsewhere {
		t.Fatalf("ArtifactsWithAnyTransition(%s) = %v, want exactly [%s]", other, otherGot, elsewhere)
	}
}

// One artifact, one row back. An artifact that took the same edge twice
// (quarantined, reinstated, quarantined, reinstated) is still one artifact
// holding one remote source, and a join that counted transition rows
// instead of artifacts would report two.
func TestArtifactsWithAnyTransitionReturnsEachArtifactOnce(t *testing.T) {
	j, _ := openJournal(t)
	c := context.Background()
	base := testArtifact(t).Set
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	artifact := mustArtifactNamed(t, base, "round-tripped.dump")
	walkToCommitted(t, j, artifact, "rt", t0)
	recordEdge(t, j, artifact, "rt-q1", "COMMITTED", "QUARANTINED", t0.Add(time.Hour))
	recordEdge(t, j, artifact, "rt-r1", "QUARANTINED", "COMMITTED", t0.Add(2*time.Hour))
	recordEdge(t, j, artifact, "rt-q2", "COMMITTED", "QUARANTINED", t0.Add(3*time.Hour))
	recordEdge(t, j, artifact, "rt-r2", "QUARANTINED", "COMMITTED", t0.Add(4*time.Hour))

	got, err := j.ArtifactsWithAnyTransition(c, base, testReinstatementEdges)
	if err != nil {
		t.Fatalf("ArtifactsWithAnyTransition: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ArtifactsWithAnyTransition = %v, want one entry: an artifact reinstated twice is still one artifact", got)
	}
}

// No edges is a caller asking nothing, and the honest answer is a refusal
// rather than an empty slice. An empty answer here reads identically to
// "nothing has ever been reinstated", which is exactly the reassuring
// wrong answer this whole issue exists to stop the system giving.
func TestArtifactsWithAnyTransitionRefusesAnEmptyEdgeSet(t *testing.T) {
	j, _ := openJournal(t)
	base := testArtifact(t).Set

	if _, err := j.ArtifactsWithAnyTransition(context.Background(), base, nil); err == nil {
		t.Fatal("ArtifactsWithAnyTransition(nil edges) returned no error; an empty edge set must be refused, not answered with an empty result that reads as \"none\"")
	}
}
