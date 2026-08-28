package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/internal/model"
	"github.com/spdrman/rclone-manager/internal/state"
)

type fakeJournal struct {
	recorded []state.Transition
	err      error
}

func (f *fakeJournal) Get(context.Context, model.ArtifactID) (state.Record, error) {
	return state.Record{}, errors.New("not used")
}

func (f *fakeJournal) RecordTransition(_ context.Context, t state.Transition) (state.Outcome, error) {
	f.recorded = append(f.recorded, t)
	return state.Outcome{Applied: true}, f.err
}

func mustID(t *testing.T) model.ArtifactID {
	t.Helper()
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	id, err := model.NewArtifactID(set, "backup.dump.zst")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return id
}

// The point of Advance: an illegal move must never reach the journal. If it
// does, the ordering guarantee is only as good as each caller's memory.
func TestAdvanceRefusesAnIllegalMoveWithoutTouchingTheJournal(t *testing.T) {
	j := &fakeJournal{}
	_, err := Advance(context.Background(), Deps{Journal: j}, state.Transition{
		Artifact: mustID(t), Key: "k1",
		From: string(Discovered), To: string(RemoteDeletePending),
	})
	if err == nil {
		t.Fatal("Advance allowed DISCOVERED straight to REMOTE_DELETE_PENDING")
	}
	if len(j.recorded) != 0 {
		t.Fatalf("the journal was written despite an illegal transition: %+v", j.recorded)
	}
}

// The specific shortcut that would let a remote source be deleted without a
// durable local copy. Every state except COMMITTED must be refused.
func TestAdvanceLetsOnlyCommittedReachRemoteDeletePending(t *testing.T) {
	for _, from := range AllStates {
		j := &fakeJournal{}
		_, err := Advance(context.Background(), Deps{Journal: j}, state.Transition{
			Artifact: mustID(t), Key: "k", From: string(from), To: string(RemoteDeletePending),
		})
		allowed := err == nil
		want := from == Committed || from == RemoteDeletePending // same-state is an idempotent no-op
		if allowed != want {
			t.Errorf("from %s to REMOTE_DELETE_PENDING: allowed=%v, want %v (err=%v)", from, allowed, want, err)
		}
		if !allowed && len(j.recorded) != 0 {
			t.Errorf("from %s: refused but still wrote to the journal", from)
		}
	}
}

func TestAdvancePassesLegalMovesThrough(t *testing.T) {
	j := &fakeJournal{}
	out, err := Advance(context.Background(), Deps{Journal: j}, state.Transition{
		Artifact: mustID(t), Key: "k2", From: string(Verified), To: string(Committing),
	})
	if err != nil {
		t.Fatalf("a legal move was refused: %v", err)
	}
	if !out.Applied || len(j.recorded) != 1 {
		t.Fatalf("legal move did not reach the journal: applied=%v recorded=%d", out.Applied, len(j.recorded))
	}
	if j.recorded[0].OccurredAt.IsZero() {
		t.Fatal("Advance did not stamp OccurredAt")
	}
}

func TestAdvanceStampsInjectedTime(t *testing.T) {
	fixed := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	j := &fakeJournal{}
	_, err := Advance(context.Background(), Deps{Journal: j, Now: func() time.Time { return fixed }},
		state.Transition{Artifact: mustID(t), Key: "k3", From: string(Verified), To: string(Committing)})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !j.recorded[0].OccurredAt.Equal(fixed) {
		t.Fatalf("OccurredAt = %v, want the injected %v", j.recorded[0].OccurredAt, fixed)
	}
}

func TestAdvanceRefusesRowCreationInAnUnknownState(t *testing.T) {
	j := &fakeJournal{}
	_, err := Advance(context.Background(), Deps{Journal: j}, state.Transition{
		Artifact: mustID(t), Key: "k4", From: "", To: "NOT_A_STATE", RemotePath: "/x",
	})
	if err == nil {
		t.Fatal("Advance created an artifact in an invented state")
	}
	if len(j.recorded) != 0 {
		t.Fatal("the journal was written for an invented state")
	}
}
