// The two "show me what has been happening" reads, RecentActivity over the
// transition log and ListOperations over the operations table, tested
// together because they have the same two failure modes and neither one is
// visible in a passing assertion about contents alone.
//
// Order is asserted everywhere, not as a tidiness check. A feed rendered
// oldest-first is a different product: an operator opening it lands on the
// deployment's first day and concludes nothing has happened since.
//
// A non-positive limit is asserted to be a refusal rather than "everything",
// because both tables only ever grow. The reading that would be convenient
// here, zero means unbounded, is the one that makes a dashboard load get
// slower every week until somebody notices.

package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// discoverAndAdvance records one artifact and walks it through the states
// named, returning the artifact id. Every transition gets its own
// idempotency key, so nothing here is a replay.
func discoverAndAdvance(t *testing.T, j *Journal, name string, states ...string) model.ArtifactID {
	t.Helper()
	ctx := context.Background()

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := j.Discover(ctx, artifact, "discover-"+name, "/incoming/"+name, RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover(%s): %v", name, err)
	}

	from := "DISCOVERED"
	for i, to := range states {
		if _, err := j.RecordTransition(ctx, Transition{
			Artifact:   artifact,
			Key:        name + "-" + to,
			From:       from,
			To:         to,
			OccurredAt: time.Date(2026, 8, 30, 10, i, 0, 0, time.UTC),
			Detail:     "step " + to,
		}); err != nil {
			t.Fatalf("RecordTransition(%s -> %s): %v", from, to, err)
		}
		from = to
	}
	return artifact
}

// TestRecentActivity_ReturnsTheTransitionLogNewestFirst is the read GET
// /api/v1/activity is built on. The point of asserting the ORDER, and not
// only the contents, is that a feed rendered oldest-first is not the same
// product: an operator opening it sees the deployment's first day.
func TestRecentActivity_ReturnsTheTransitionLogNewestFirst(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	discoverAndAdvance(t, j, "backup-a.dump", "TRANSFERRING", "TRANSFERRED")

	got, err := j.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	// Discover itself writes a transition ("" -> DISCOVERED), so three
	// rows, not two.
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3; got %+v", len(got), got)
	}
	if got[0].To != "TRANSFERRED" || got[2].To != "DISCOVERED" {
		t.Fatalf("order = %q, %q, %q; want newest (TRANSFERRED) first", got[0].To, got[1].To, got[2].To)
	}
	if got[0].From != "TRANSFERRING" {
		t.Errorf("From = %q, want TRANSFERRING", got[0].From)
	}
	if got[0].Artifact.Name != "backup-a.dump" {
		t.Errorf("Artifact.Name = %q, want backup-a.dump", got[0].Artifact.Name)
	}
	if got[0].Artifact.Set.Source != "production" || got[0].Artifact.Set.Set != "postgres-primary" {
		t.Errorf("Artifact.Set = %+v, want production/postgres-primary", got[0].Artifact.Set)
	}
	if got[0].Detail != "step TRANSFERRED" {
		t.Errorf("Detail = %q, want %q", got[0].Detail, "step TRANSFERRED")
	}
	if got[0].OccurredAt.IsZero() {
		t.Error("OccurredAt is zero, so the feed has nothing to render a timestamp from")
	}
}

// TestRecentActivity_HonoursTheLimit. The positive control is the
// unlimited-enough read above: without it, a limit that returned nothing
// at all would look identical to a limit that worked.
func TestRecentActivity_HonoursTheLimit(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	discoverAndAdvance(t, j, "backup-a.dump", "TRANSFERRING", "TRANSFERRED")

	all, err := j.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatalf("RecentActivity(10): %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("the control read returned %d rows, so a limit test below would prove nothing", len(all))
	}

	got, err := j.RecentActivity(ctx, 2)
	if err != nil {
		t.Fatalf("RecentActivity(2): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].To != all[0].To || got[1].To != all[1].To {
		t.Errorf("a limited read returned a different window than the head of the full one: %+v vs %+v", got, all[:2])
	}
}

func TestRecentActivity_RefusesANonPositiveLimit(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	discoverAndAdvance(t, j, "backup-a.dump")

	for _, limit := range []int{0, -1} {
		got, err := j.RecentActivity(ctx, limit)
		if err == nil {
			t.Fatalf("RecentActivity(%d) returned %d rows and no error; an unbounded read of an append-only table has to be asked for by number", limit, len(got))
		}
		if !strings.Contains(err.Error(), "limit must be positive") {
			t.Errorf("RecentActivity(%d) error = %v, want it to name the limit", limit, err)
		}
	}
}

func TestRecentActivity_OnAnEmptyJournalIsEmptyNotAnError(t *testing.T) {
	j, _ := openJournal(t)

	got, err := j.RecentActivity(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0 on a journal nothing has happened in yet", len(got))
	}
}

// TestListOperations_ReturnsEveryOperationNewestFirst backs GET
// /api/v1/operations, which before issue #211 was a 405: the contract
// declared POST on that path and nothing else.
func TestListOperations_ReturnsEveryOperationNewestFirst(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"op_1", "op_2", "op_3"} {
		req := testOperationRequest(id, "idem-"+id)
		req.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		if _, err := j.CreateOperation(ctx, req); err != nil {
			t.Fatalf("CreateOperation(%s): %v", id, err)
		}
	}

	got, err := j.ListOperations(ctx, 10)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].OperationID != "op_3" || got[2].OperationID != "op_1" {
		t.Fatalf("order = %q, %q, %q; want newest (op_3) first",
			got[0].OperationID, got[1].OperationID, got[2].OperationID)
	}
	if got[0].Status != OperationQueued {
		t.Errorf("Status = %q, want %q", got[0].Status, OperationQueued)
	}
	if got[0].Action != "run_cycle" {
		t.Errorf("Action = %q, want run_cycle", got[0].Action)
	}
	if got[0].ConfigRevision != "rev-1" {
		t.Errorf("ConfigRevision = %q, want rev-1", got[0].ConfigRevision)
	}
}

func TestListOperations_HonoursTheLimit(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"op_1", "op_2", "op_3"} {
		req := testOperationRequest(id, "idem-"+id)
		req.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		if _, err := j.CreateOperation(ctx, req); err != nil {
			t.Fatalf("CreateOperation(%s): %v", id, err)
		}
	}

	all, err := j.ListOperations(ctx, 10)
	if err != nil {
		t.Fatalf("ListOperations(10): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("the control read returned %d, so a limit test below would prove nothing", len(all))
	}

	got, err := j.ListOperations(ctx, 1)
	if err != nil {
		t.Fatalf("ListOperations(1): %v", err)
	}
	if len(got) != 1 || got[0].OperationID != "op_3" {
		t.Fatalf("got %+v, want just op_3", got)
	}
}

func TestListOperations_RefusesANonPositiveLimit(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	if _, err := j.CreateOperation(ctx, testOperationRequest("op_1", "idem-1")); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	for _, limit := range []int{0, -1} {
		got, err := j.ListOperations(ctx, limit)
		if err == nil {
			t.Fatalf("ListOperations(%d) returned %d rows and no error", limit, len(got))
		}
		if !strings.Contains(err.Error(), "limit must be positive") {
			t.Errorf("ListOperations(%d) error = %v, want it to name the limit", limit, err)
		}
	}
}
