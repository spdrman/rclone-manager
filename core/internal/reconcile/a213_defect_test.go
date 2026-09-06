package reconcile

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// This file exists because every other test in this package answers the
// remote through a fake, and a fake is where a whole class of defect
// hides.
//
// reconcileDeletePending concludes "the remote is confirmed absent" from
// transport.CategoryOf reporting NotFound, and nowhere else. Every fixture
// in reconcile_test.go hands it a transport whose Stat already returns an
// error carrying that category, so those tests prove the reconciliation
// logic and say nothing at all about whether a real adapter ever produces
// the category they depend on. It did not, once, and this file is what
// caught that.
//
// The whole test stays here now that the adapter has been fixed, because a
// gap between what a fake promises and what the real thing delivers is not
// a one-off. This is the one test in the package that will notice if the
// adapter stops classifying its errors again.

// TestReconcile_RealAdapter_ConvergesRemoteDeletePendingToComplete runs the
// exact scenario TestReconcile_DeletePending_RemoteAbsent_ValidLocal_ReconcilesComplete
// covers in reconcile_test.go, with the fake transport swapped for the real
// rclone adapter's local backend and nothing else changed.
//
// An artifact at REMOTE_DELETE_PENDING whose remote object is genuinely
// gone has to reconcile straight to COMPLETE. That is FR-17's own "absent /
// final / REMOTE_DELETE_PENDING" row, and it is the row crash_safety.go's
// walkthrough leans on for the crash that happened just after a real delete
// succeeded and just before COMPLETE was journaled.
//
// It used to assert the opposite, as a known defect: the adapter returned
// its errors raw, transport.CategoryOf could not tell a NotFound from
// anything else, and the row that had to converge surfaced as a
// per-artifact error with the journal left at REMOTE_DELETE_PENDING
// forever. That is docs/EPIC.md's "artifact stuck with no way forward",
// though never an unauthorized deletion: this path only ever moves a row
// toward COMPLETE or QUARANTINED or leaves it alone, and never calls
// DeleteRemote at all.
func TestReconcile_RealAdapter_ConvergesRemoteDeletePendingToComplete(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "known-defect-stuck.dump")
	size := int64(64)
	localPath := writeLocalFile(t, size)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteDeletePending,
	})

	// A real local backend, pointed at a source whose "backups/<name>"
	// object (the fixed remote path driveTo's fixtures use) has genuinely
	// never existed: exactly what the remote side of REMOTE_DELETE_PENDING
	// looks like right after a crash that happened just after the real
	// delete succeeded but before COMPLETE was journaled (this PR's crash
	// matrix "before COMPLETE" / "remote deletion" points).
	source := transport.Source{ID: "known-defect-real-adapter", Type: "local", Root: t.TempDir()}

	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: rclone.New()}, source, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rec, getErr := j.Get(context.Background(), artifact)
	if getErr != nil {
		t.Fatalf("Journal.Get: %v", getErr)
	}

	// This used to assert that Reconcile COULD NOT get here. The adapter
	// never wrapped its own errors, so transport.CategoryOf could not tell a
	// NotFound from anything else, and the "remote confirmed absent" path had
	// nothing to switch on. Reconcile reported a per-artifact error and left
	// the journal stuck at REMOTE_DELETE_PENDING forever.
	//
	// The adapter wraps its errors now, so a genuinely absent remote is
	// recognised and the artifact converges to COMPLETE, which is FR-17's
	// "absent / final / REMOTE_DELETE_PENDING -> reconcile COMPLETE" row.
	if len(report.Errors) != 0 {
		t.Fatalf("Reconcile reported errors for a confirmed-absent remote: %v", report.Errors)
	}
	if rec.State != string(lifecycle.Complete) {
		t.Fatalf("journal state = %s, want %s; a confirmed-absent remote should converge to COMPLETE",
			rec.State, lifecycle.Complete)
	}
}
