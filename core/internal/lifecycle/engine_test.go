// These cover Advance, and every one of them asserts the same thing twice:
// that the call was refused, AND that the journal was not written.
//
// The second half is the whole point. Advance exists because the legality
// rule and the write live in different packages, so a version that returned
// an error after recording the transition would satisfy any test that only
// looked at the return value while leaving exactly the corrupt journal the
// function exists to prevent. The fake journal here records every
// transition it is handed precisely so that can be checked.
package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// fakeJournal records what it was asked to write and answers nothing
// useful otherwise.
//
// It is a recorder rather than a working journal because these tests are
// about what reaches the journal, not about what the journal then does with
// it. Get returns an error and the two log queries report "never", which is
// the honest answer for a fake holding no history; the comments on those
// methods say why reporting a zero time instead would be dangerous.
type fakeJournal struct {
	recorded []state.Transition
	err      error
}

// Get always fails, because Advance never calls it: Advance is handed the
// From state by its caller rather than reading it. A fake that answered
// would let a change start reading the current state without any test
// noticing the extra round trip.
func (f *fakeJournal) Get(context.Context, model.ArtifactID) (state.Record, error) {
	return state.Record{}, errors.New("not used")
}

// RecordTransition appends to the recorded slice, which is the assertion
// surface for every test in this file: an empty slice after a refusal is the
// proof that the refusal happened before the write rather than after it.
func (f *fakeJournal) RecordTransition(_ context.Context, t state.Transition) (state.Outcome, error) {
	f.recorded = append(f.recorded, t)
	return state.Outcome{Applied: true}, f.err
}

// LastEnteredAt reports "never entered" rather than a zero time that would
// read as the distant past. Every caller of this that decides anything
// destructive treats absent evidence as a refusal, and a fake used by tests
// about Advance should not be the thing that quietly hands one an answer.
func (f *fakeJournal) LastEnteredAt(context.Context, model.ArtifactID, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

// LastTransition is unused by the step under test, and the safety property
// it exists for (issue #220's reinstatement forfeiture) is proved against a
// real journal in remotedelete_reinstate_test.go, not here. Reporting "no
// such edge" is the honest answer for a fake that records no log at all.
func (f *fakeJournal) LastTransition(context.Context, model.ArtifactID, string, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

// mustID builds one valid artifact id through the real constructors. Every
// test here uses the same one, since none of them is about identity: what
// varies between cases is the transition, not the artifact.
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

// TestAdvancePassesLegalMovesThrough is the positive control for the two
// refusal tests above it. Without it they would be satisfied by an Advance
// that refused everything, which would keep the journal perfectly clean and
// stop the product working.
//
// The OccurredAt assertion is the second half: Advance stamps the time when
// the caller left it zero, and a transition recorded with a zero timestamp
// would be read by the deletion-safety gate and by revalidation's due-ness
// check as having happened in the distant past.
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

// TestAdvanceStampsInjectedTime pins that the injected clock is what gets
// written, not merely that it is consulted. Every test elsewhere in this
// tree that stages an artifact at a controlled point in time depends on
// this: if Advance ignored Deps.Now, the fixtures would silently be stamped
// with the real clock and the interval-based gates would be untestable.
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

// TestAdvanceRefusesRowCreationInAnUnknownState covers the one path with no
// From state to validate against.
//
// Row creation skips the transition table entirely, since there is no
// predecessor, so the table's protection does not apply and something else
// has to refuse a state name nobody defined. Without this check the create
// path would be a way to put any string at all into the journal's state
// column, which is exactly the drift ParseState refuses to read back.
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
