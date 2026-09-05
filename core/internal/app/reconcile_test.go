package app

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// Reconciliation right after a clean cycle has to find nothing.
//
// The interesting assertion is the negative one. FR-17's whole value depends
// on a reconciliation pass distinguishing a backup that is fine from one that
// has rotted, and a pass that reported a change for a perfectly healthy
// COMPLETE artifact would be indistinguishable, on the reports alone, from
// one that had found real damage. So this drives an artifact all the way
// through a real cycle first, and then asserts that the next pass changes
// nothing.
//
// Doing it through a full RunCycle rather than by writing a COMPLETE row by
// hand is the part worth keeping. A hand-built row proves reconciliation
// agrees with the fixture; this proves it agrees with what the pipeline
// actually produces.

// TestReconcileAll_ReconcilesEveryConfiguredBackupSet proves ReconcileAll
// visits every configured backup set and reports one Finding per journal
// row it examined: drive one artifact all the way to COMPLETE through a
// full RunCycle, then reconcile again and confirm it correctly finds
// nothing to change (COMPLETE with a valid local copy is FR-17's plain
// no-op row).
func TestReconcileAll_ReconcilesEveryConfiguredBackupSet(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	tr := newFakeTransport()
	tr.put("backup.dump", "reconcile payload", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	svc.RunCycle(ctx)

	artifact, err := model.NewArtifactID(bs.ID, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	final, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Fatalf("precondition: journal state = %q, want %q (fakeTransport is built to let a full delete succeed; see helpers_test.go)", final.State, lifecycle.Complete)
	}

	reports := svc.ReconcileAll(ctx)
	if len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want 1", len(reports))
	}
	if reports[0].Err != nil {
		t.Fatalf("reports[0].Err = %v, want nil", reports[0].Err)
	}
	if len(reports[0].Report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one", reports[0].Report.Findings)
	}
	f := reports[0].Report.Findings[0]
	if f.Changed() {
		t.Errorf("Finding = %+v, want no change: reconciliation right after a clean COMPLETE should be a no-op", f)
	}
}
