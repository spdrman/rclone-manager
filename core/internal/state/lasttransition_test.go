package state

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// LastTransition answers a question LastEnteredAt cannot: not "when did
// this artifact last become COMMITTED" but "did it ever become COMMITTED
// *out of quarantine*". The difference is the whole of issue #220's audit
// requirement, because a re-trusted artifact and one that was never
// distrusted are both simply COMMITTED on the artifacts row, and only the
// append-only transition log still holds which of the two happened.
func TestLastTransitionReportsOnlyTheExactEdge(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if _, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, t0); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Nothing has been recorded yet, so every edge must report "never",
	// with a nil error rather than a zero time a caller could mistake for
	// a real answer.
	if _, ok, err := j.LastTransition(ctx, artifact, "QUARANTINED", "COMMITTED"); err != nil || ok {
		t.Fatalf("LastTransition(QUARANTINED -> COMMITTED) before anything = (ok %v, err %v), want (false, nil)", ok, err)
	}

	for _, tr := range []Transition{
		{Artifact: artifact, Key: "t1", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: t0},
		{Artifact: artifact, Key: "t2", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: t0},
		{Artifact: artifact, Key: "t3", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: t0},
		{Artifact: artifact, Key: "t4", From: "VERIFYING", To: "VERIFIED", OccurredAt: t0},
		{Artifact: artifact, Key: "t5", From: "VERIFIED", To: "COMMITTING", OccurredAt: t0},
		{Artifact: artifact, Key: "t6", From: "COMMITTING", To: "COMMITTED", OccurredAt: t0},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s: %v", tr.To, err)
		}
	}

	// The artifact is COMMITTED, and LastEnteredAt says so. That is
	// exactly the observation that must NOT be enough to authorise
	// anything about quarantine, so it is asserted here as the positive
	// control for the negative below: the log is being read, the artifact
	// really is committed, and LastTransition still answers "never" for
	// the quarantine edge.
	if _, ok, err := j.LastEnteredAt(ctx, artifact, "COMMITTED"); err != nil || !ok {
		t.Fatalf("LastEnteredAt(COMMITTED) = (ok %v, err %v), want (true, nil)", ok, err)
	}
	if _, ok, err := j.LastTransition(ctx, artifact, "QUARANTINED", "COMMITTED"); err != nil || ok {
		t.Fatalf("LastTransition(QUARANTINED -> COMMITTED) for an artifact committed the ordinary way = (ok %v, err %v), want (false, nil)", ok, err)
	}

	quarantinedAt := t0.Add(time.Hour)
	if _, err := j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "q1", From: "COMMITTED", To: "QUARANTINED", OccurredAt: quarantinedAt,
	}); err != nil {
		t.Fatalf("-> QUARANTINED: %v", err)
	}
	// Still not the edge asked about: leaving COMMITTED is not entering it.
	if _, ok, err := j.LastTransition(ctx, artifact, "QUARANTINED", "COMMITTED"); err != nil || ok {
		t.Fatalf("LastTransition(QUARANTINED -> COMMITTED) after only entering quarantine = (ok %v, err %v), want (false, nil)", ok, err)
	}

	reinstatedAt := t0.Add(2 * time.Hour)
	if _, err := j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "r1", From: "QUARANTINED", To: "COMMITTED", OccurredAt: reinstatedAt,
	}); err != nil {
		t.Fatalf("-> COMMITTED (reinstatement): %v", err)
	}

	at, ok, err := j.LastTransition(ctx, artifact, "QUARANTINED", "COMMITTED")
	if err != nil || !ok {
		t.Fatalf("LastTransition(QUARANTINED -> COMMITTED) after a reinstatement = (ok %v, err %v), want (true, nil)", ok, err)
	}
	if !at.Equal(reinstatedAt) {
		t.Fatalf("LastTransition = %s, want the reinstatement's own occurred_at %s", at, reinstatedAt)
	}

	// The reverse edge must stay separately answerable: an artifact that
	// has been round-tripped has both, and a caller asking about one must
	// never be handed the other.
	qAt, ok, err := j.LastTransition(ctx, artifact, "COMMITTED", "QUARANTINED")
	if err != nil || !ok {
		t.Fatalf("LastTransition(COMMITTED -> QUARANTINED) = (ok %v, err %v), want (true, nil)", ok, err)
	}
	if !qAt.Equal(quarantinedAt) {
		t.Fatalf("LastTransition(COMMITTED -> QUARANTINED) = %s, want %s", qAt, quarantinedAt)
	}
}

// Two artifacts, one reinstated and one not: the read must be scoped to
// the artifact it was asked about. A join that lost the artifact predicate
// would still pass every assertion in the test above, because that test
// only ever has one row in the table.
func TestLastTransitionIsScopedToOneArtifact(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	reinstated := testArtifact(t)
	untouched, err := model.NewArtifactID(reinstated.Set, "second-backup.dump.zst")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, a := range []struct {
		id  model.ArtifactID
		key string
	}{{reinstated, "a"}, {untouched, "b"}} {
		if _, err := j.Discover(ctx, a.id, a.key+"-discover", "/incoming/"+a.id.Name, RemoteIdentity{}, t0); err != nil {
			t.Fatalf("Discover %s: %v", a.id, err)
		}
		for _, tr := range []Transition{
			{Artifact: a.id, Key: a.key + "-1", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: t0},
			{Artifact: a.id, Key: a.key + "-2", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: t0},
			{Artifact: a.id, Key: a.key + "-3", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: t0},
			{Artifact: a.id, Key: a.key + "-4", From: "VERIFYING", To: "VERIFIED", OccurredAt: t0},
			{Artifact: a.id, Key: a.key + "-5", From: "VERIFIED", To: "COMMITTING", OccurredAt: t0},
			{Artifact: a.id, Key: a.key + "-6", From: "COMMITTING", To: "COMMITTED", OccurredAt: t0},
		} {
			if _, err := j.RecordTransition(ctx, tr); err != nil {
				t.Fatalf("%s -> %s: %v", a.id, tr.To, err)
			}
		}
	}

	for _, tr := range []Transition{
		{Artifact: reinstated, Key: "a-q", From: "COMMITTED", To: "QUARANTINED", OccurredAt: t0.Add(time.Hour)},
		{Artifact: reinstated, Key: "a-r", From: "QUARANTINED", To: "COMMITTED", OccurredAt: t0.Add(2 * time.Hour)},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("reinstating: %v", err)
		}
	}

	if _, ok, err := j.LastTransition(ctx, reinstated, "QUARANTINED", "COMMITTED"); err != nil || !ok {
		t.Fatalf("the reinstated artifact reports (ok %v, err %v), want (true, nil)", ok, err)
	}
	if _, ok, err := j.LastTransition(ctx, untouched, "QUARANTINED", "COMMITTED"); err != nil || ok {
		t.Fatalf("the untouched artifact reports (ok %v, err %v), want (false, nil): the read is not scoped to one artifact", ok, err)
	}
}
