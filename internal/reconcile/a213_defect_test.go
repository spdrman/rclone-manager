package reconcile

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/internal/lifecycle"
	"github.com/spdrman/rclone-manager/internal/state"
	"github.com/spdrman/rclone-manager/internal/transport"
	"github.com/spdrman/rclone-manager/internal/transport/rclone"
)

// TestReconcile_RealAdapter_CannotConvergeRemoteDeletePendingToComplete_KnownDefect
// is the end-to-end proof behind the "adapter never classifies its own
// errors" defect this PR reports (see
// internal/transport/rclone/error_classification_gap_a213_test.go for the
// narrower proof, and the PR description for the full writeup and the
// recommended fix).
//
// This is the exact fixture TestReconcile_DeletePending_RemoteAbsent_ValidLocal_ReconcilesComplete
// (in reconcile_test.go) already proves converges correctly: an artifact at
// REMOTE_DELETE_PENDING whose remote object is genuinely, confirmably gone
// must reconcile straight to COMPLETE (FR-17's own row for exactly this
// case, and the row crash_safety.go's REMOTE_DELETE_PENDING -> COMPLETE
// walkthrough depends on for "the remote delete had actually already
// succeeded before the crash"). That test uses statTransportFor(artifact,
// statNotFound), a fake transport whose Stat method hands back an
// already-classified transport.NotFound error, which is what
// reconcileDeletePending's statRemote helper needs to see to conclude
// "confirmed absent".
//
// This test swaps that fake for the real rclone.Adapter's local backend,
// pointed at a source where the remote object is genuinely absent from
// disk, and nothing else changes. It proves that against the real adapter,
// Reconcile cannot reach the same, already-proven-correct verdict: the raw
// error the real adapter returns is never classified, so statRemote's
// transport.CategoryOf check never sees NotFound, and the row that must
// converge to COMPLETE instead surfaces as a per-artifact Errors entry,
// leaving the journal stuck at REMOTE_DELETE_PENDING. That is a real
// "artifact stuck with no way forward" (docs/EPIC.md's crash matrix
// exactly names this as a failure the crash matrix must prove does not
// happen), even though it can never cause an unauthorized deletion:
// reconcileDeletePending only ever moves a row toward COMPLETE or
// QUARANTINED, or leaves it exactly where it is; it never calls
// DeleteRemote itself.
func TestReconcile_RealAdapter_CannotConvergeRemoteDeletePendingToComplete_KnownDefect(t *testing.T) {
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

	if len(report.Errors) == 1 && len(report.Findings) == 0 && rec.State == string(lifecycle.RemoteDeletePending) {
		t.Logf("CONFIRMED (known defect): Reconcile could not convert a confirmed-absent remote into COMPLETE "+
			"through the real adapter; it reported a per-artifact error instead and left the journal at %s: %v",
			rec.State, report.Errors[0])
		return
	}

	t.Fatalf("this test's fixture assumption changed: got %d Errors, %d Findings, journal state %s (Errors=%v); "+
		"if Reconcile now reaches COMPLETE here, the adapter-classification defect this test documents has been fixed "+
		"(see internal/transport/rclone/error_classification_gap_a213_test.go and the PR description) and this test "+
		"should be deleted, not adjusted to still pass",
		len(report.Errors), len(report.Findings), rec.State, report.Errors)
}
