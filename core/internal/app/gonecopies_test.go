package app

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/state"
)

// A completed move leaves the source placement GONE, and a GONE row is
// the journal saying "the copy is no longer there and I know it". This
// file is about what the operator-facing copy view does with one.
//
// core/service already drops GONE rows before they reach the API, and
// says why: "a row for it would read as a copy in every layout anyone
// would write for one". ArtifactCopy is the other view of the same
// placements, and the CLI writes exactly that layout over it.

// TestACompletedMoveLeavesNoLocalCopyOnTheDetailSurface is the whole
// finding, driven through the real move engine rather than asserted about
// a hand-built record.
//
// The move is real: a real journal, a real retention chain that decides
// the artifact belongs offsite, a real local file that really is deleted.
// What is checked afterwards is the view an operator asking "where is my
// backup" gets back.
func TestACompletedMoveLeavesNoLocalCopyOnTheDetailSurface(t *testing.T) {
	ctx := context.Background()
	medium := newCountingMedium()
	svc, bs, journal := movingService(t, medium, nil)
	artifact := seedMovableArtifact(t, ctx, journal, bs, "monthly-only.dump", retentionTestNow.AddDate(0, 0, -40))

	report := svc.RunCycle(ctx)
	if report.Moves.Completed != 1 {
		t.Fatalf("Moves = %+v, want one completed move; without one this test is looking at an artifact that never left local disk", report.Moves)
	}

	// The fixture's own control, and the reason this test drives the
	// engine instead of writing the record by hand: it establishes that a
	// completed move really does leave a GONE local row in the journal,
	// so the assertion below is about a shape the product produces.
	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("reading %s back: %v", artifact, err)
	}
	local, found := "", false
	for _, p := range rec.Placements {
		if p.IsLocal() {
			local, found = p.Status, true
		}
	}
	if !found {
		t.Fatal("the journal kept no local placement at all after the move; a deleted copy is recorded as GONE, never removed, and this test needs that row to exist")
	}
	if local != state.PlacementGone {
		t.Fatalf("the local placement is %s after a completed move, want %s", local, state.PlacementGone)
	}

	detail, err := svc.GetArtifactDetail(ctx, artifact)
	if err != nil {
		t.Fatalf("GetArtifactDetail: %v", err)
	}
	for _, c := range detail.Copies {
		if c.Status == state.PlacementGone {
			t.Errorf("the copy view carries a %s row for %q, and every other field on it is computed as though the file were still there "+
				"(access %q, verified_as %q, checkable_as %q). A row for a copy that is not there reads as a copy",
				c.Status, c.Medium, c.Access, c.VerificationClass, c.CheckableAs)
		}
	}
	if len(detail.Copies) != 1 {
		t.Fatalf("got %d copies after a completed move, want exactly the one on %q: %+v", len(detail.Copies), moveTestMedium, detail.Copies)
	}
	if detail.Copies[0].Medium != moveTestMedium {
		t.Errorf("the surviving copy is on %q, want %q", detail.Copies[0].Medium, moveTestMedium)
	}
}

// TestADeletePendingCopyIsStillACopy is the control that stops the drop
// above from being "anything not ACTIVE disappears".
//
// DELETE_PENDING is the mid-move state: the manager has written down that
// it intends to delete this copy and has not deleted it yet. The bytes
// are there, they can be read, and an operator watching a move needs to
// see them. Dropping that row would hide the one moment in a move when an
// artifact genuinely has two copies.
func TestADeletePendingCopyIsStillACopy(t *testing.T) {
	s := serviceWithMediums()
	p := placementOn(state.MediumLocal, "/srv/backups/dump.zst", state.VerificationContent)
	p.Status = state.PlacementDeletePending

	copies := s.artifactCopies(state.Record{Placements: []state.Placement{p}}, copiesNow)
	if len(copies) != 1 {
		t.Fatalf("got %d copies for one DELETE_PENDING placement, want 1: the copy is still there until it is deleted", len(copies))
	}
	if copies[0].Status != state.PlacementDeletePending {
		t.Errorf("status = %q, want %q", copies[0].Status, state.PlacementDeletePending)
	}
	if !copies[0].Retrievable() {
		t.Error("a DELETE_PENDING local copy reads as unreadable; the bytes are still on the disk")
	}
}

// TestAnArtifactWhoseOnlyCopyIsGoneHasNoCopiesAtAll is the same rule at
// the end of the road, where the difference is starkest.
//
// Absence of a copy has to be absence of a row, because that is what an
// artifact which never had one reports, and it is what makes an empty
// list mean exactly one thing. This is the shape a prune leaves behind:
// the journal remembers the copy existed, and nothing anywhere can read
// it.
func TestAnArtifactWhoseOnlyCopyIsGoneHasNoCopiesAtAll(t *testing.T) {
	s := serviceWithMediums()
	p := placementOn(state.MediumLocal, "/srv/backups/dump.zst", state.VerificationContent)
	p.Status = state.PlacementGone

	if copies := s.artifactCopies(state.Record{Placements: []state.Placement{p}}, copiesNow); len(copies) != 0 {
		t.Fatalf("got %d copies for an artifact whose only placement is GONE: %+v", len(copies), copies)
	}
}
