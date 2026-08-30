// This file is issue #104 (B3.4)'s destructive-safety suite for package
// app: the integration proof that a transfer internal/capacity refuses
// never reaches internal/lifecycle's transfer step, and — the part §56
// actually cares about — that a full disk is never "solved" by deleting
// something. Helpers added here are prefixed b34 to stay clear of the
// package's existing testBackupSet, testConfig, testSource,
// discoverOneRecord, fakeTransport, hookJournal and openJournal.
//
// pipeline_test.go's TestProcessArtifact_CapacityRefusal_NeverStartsTransfer
// already covers the single-artifact call; these run the whole
// Service.RunCycle instead — reconcile, discover, every in-flight
// artifact, retention preview — because "nothing was deleted" is a claim
// about the entire cycle, not about one step of it. Retention in
// particular only ever gets a look-in at the END of a cycle (cycle.go's
// processBackupSet), which is exactly where an "if storage is critical,
// free some space" shortcut would be tempting to add and must never
// appear.
package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// b34ImpossibleFreeBytes is a free-space requirement no real filesystem
// can satisfy (4 exbibytes), so internal/capacity.CheckBeforeTransfer
// refuses regardless of what StatPath actually reports on whatever
// machine this test runs on. Setting BOTH thresholds to it puts the
// assessment at CRITICAL rather than merely WARNING, which is the state
// §56 is written about.
const b34ImpossibleFreeBytes = uint64(1) << 62

// b34SeedRetainedFile writes a file into a backup set's local
// destination standing in for an already-retained, already-committed
// backup artifact — something FR-18/FR-19 retention protects and that a
// "delete until there is enough space" shortcut would be reaching for
// first. It returns the path and its exact bytes so a caller can prove
// the file is not merely still present but byte-for-byte untouched.
func b34SeedRetainedFile(t *testing.T, localDir string) (string, []byte) {
	t.Helper()
	content := []byte("an already-retained backup nobody is allowed to delete to make room")
	path := filepath.Join(localDir, "retained-2026-08-01.dump")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("seeding retained file: %v", err)
	}
	return path, content
}

// b34DirEntries lists localDir's entry names, so a test can assert the
// directory is EXACTLY as it was rather than only that one named file
// survived — a deletion that took some other file, or a partial transfer
// that created one, would both slip past a per-file check.
func b34DirEntries(t *testing.T, localDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(localDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", localDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestRunCycle_CapacityRefusal_NeverReachesTheLifecycleTransferStep is
// this issue's INTEGRATION claim, made mechanical rather than inferred:
// internal/lifecycle.Transfer's very first act is recording the
// DISCOVERED -> TRANSFERRING transition, so a journal wrapper that fails
// the test the instant such a transition is written proves the transfer
// step was never entered at all — not merely that the copy did not
// finish, and not merely that the artifact happens to have ended the
// cycle back where it started.
//
// The refusal comes from internal/capacity itself (via pipeline.go's
// admitCapacity, which is the ONLY capacity gate in this codebase); this
// test adds no capacity logic of its own, it only makes the thresholds
// unsatisfiable.
func TestRunCycle_CapacityRefusal_NeverReachesTheLifecycleTransferStep(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = "" // fakeTransport ignores Source.Root, so this is inert here.

	tr := newFakeTransport()
	tr.put("backup.dump", "payload that must never be copied", epoch.Unix())

	journal := &hookJournal{Journal: openJournal(t), onRecordTransition: func(tr state.Transition, out state.Outcome) {
		if out.Record.State == string(lifecycle.Transferring) {
			t.Errorf("journal recorded a transition to %s (%+v) — a transfer internal/capacity refused must never reach internal/lifecycle's transfer step at all", lifecycle.Transferring, tr)
		}
	}}

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)
	svc.Capacity.WarningFreeBytes = b34ImpossibleFreeBytes
	svc.Capacity.CriticalFreeBytes = b34ImpossibleFreeBytes

	report := svc.RunCycle(context.Background())

	if len(report.Sets) != 1 {
		t.Fatalf("len(report.Sets) = %d, want 1", len(report.Sets))
	}
	set := report.Sets[0]
	if set.Err != nil {
		t.Fatalf("BackupSetCycleResult.Err = %v, want nil: a capacity refusal is a refusal to transfer, not a cycle-level failure", set.Err)
	}
	if len(set.Discovery.Discovered) != 1 {
		t.Fatalf("Discovery.Discovered = %+v, want exactly one artifact (discovery itself is not gated on capacity)", set.Discovery.Discovered)
	}

	if got := tr.copyToLocalCalls(); got != 0 {
		t.Errorf("CopyToLocal was called %d time(s), want 0", got)
	}

	final, err := journal.Get(context.Background(), set.Discovery.Discovered[0].Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Discovered) {
		t.Errorf("journal state = %q, want %q: a refused artifact must be left exactly where it was, for a later cycle to retry once there is room", final.State, lifecycle.Discovered)
	}
}

// TestRunCycle_SustainedCriticalStorage_DeletesNothingToMakeRoom is the
// §56 claim: "storage is critical" must never escalate into freeing
// space. Not on the first cycle, and — the case an "it will sort itself
// out" design actually fails on — not on the tenth either, with the disk
// still full and the same artifact still waiting.
//
// The refusal is allowed to be permanent. What is not allowed is the
// daemon deciding, at any point, that the way out is to delete a
// retained local artifact (FR-18/FR-19's business, never a capacity
// gate's) or to drop the remote original (which would destroy the ONLY
// remaining copy, since the local one was never made). internal/capacity
// contains no deletion path at all by design, and this proves nothing
// around it quietly supplies one.
func TestRunCycle_SustainedCriticalStorage_DeletesNothingToMakeRoom(t *testing.T) {
	localDir := t.TempDir()
	retainedPath, retainedContent := b34SeedRetainedFile(t, localDir)

	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup.dump", "payload that must never be copied", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)
	svc.Capacity.WarningFreeBytes = b34ImpossibleFreeBytes
	svc.Capacity.CriticalFreeBytes = b34ImpossibleFreeBytes

	const cycles = 10
	for i := 0; i < cycles; i++ {
		report := svc.RunCycle(context.Background())
		if len(report.Sets) != 1 {
			t.Fatalf("cycle %d: len(report.Sets) = %d, want 1", i, len(report.Sets))
		}
		if err := report.Sets[0].Err; err != nil {
			t.Fatalf("cycle %d: BackupSetCycleResult.Err = %v, want nil", i, err)
		}
	}

	if got := tr.copyToLocalCalls(); got != 0 {
		t.Errorf("CopyToLocal was called %d time(s) across %d cycles, want 0", got, cycles)
	}
	if got := tr.deleteCallCount(); got != 0 {
		t.Errorf("DeleteRemote was called %d time(s) across %d cycles, want 0: the remote original is the only copy that exists while the transfer keeps being refused", got, cycles)
	}
	if _, ok := tr.objects["backup.dump"]; !ok {
		t.Error("the remote object is gone — a transfer that never happened must leave the remote original exactly where it is")
	}

	got, err := os.ReadFile(retainedPath)
	if err != nil {
		t.Fatalf("the already-retained local artifact is unreadable after %d critical-storage cycles: %v", cycles, err)
	}
	if string(got) != string(retainedContent) {
		t.Error("the already-retained local artifact's content changed under sustained storage pressure")
	}

	entries := b34DirEntries(t, localDir)
	if len(entries) != 1 || entries[0] != filepath.Base(retainedPath) {
		t.Errorf("local destination contains %v, want exactly [%s]: a refused cycle must neither delete anything to make room nor leave a partial file behind",
			entries, filepath.Base(retainedPath))
	}
}

// TestRunCycle_CriticalStorage_RetentionStillOnlyPreviews closes the one
// remaining door, and it is the one that matters most: an artifact that
// ALREADY completed, and whose local copy retention is therefore now
// responsible for, must survive storage going critical afterwards.
//
// A "critical storage" state that quietly upgraded the cycle's retention
// step from "report what would be kept" to "apply it, we need the room"
// would pass every other test in this file — on a fresh deployment there
// is nothing eligible to delete yet, so nothing would visibly go wrong —
// and would destroy data on a real one. So this test deliberately builds
// the real situation first: run a cycle with capacity satisfiable so one
// artifact goes all the way to COMPLETE with a durable local file, THEN
// make storage critical with a second artifact waiting, and prove the
// completed one and its file are still exactly there afterwards, with
// retention still classifying the same way it did before the pressure.
func TestRunCycle_CriticalStorage_RetentionStillOnlyPreviews(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup-a.dump", "the first artifact, which does complete", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	// Phase 1: ordinary conditions. Zero thresholds are what every other
	// caller in this codebase runs with today (see Service.Capacity's own
	// doc), so this is the normal path, not a special case.
	settled := svc.RunCycle(context.Background())
	if err := settled.Sets[0].Err; err != nil {
		t.Fatalf("settling cycle: BackupSetCycleResult.Err = %v, want nil", err)
	}
	completed := settled.Sets[0].Discovery.Discovered[0].Artifact
	rec, err := journal.Get(context.Background(), completed)
	if err != nil {
		t.Fatalf("Get after settling cycle: %v", err)
	}
	if rec.State != string(lifecycle.Complete) {
		t.Fatalf("after the settling cycle state = %q, want %q — this test needs a genuinely completed, retention-managed artifact to be about anything", rec.State, lifecycle.Complete)
	}
	verdictsBefore := len(settled.Sets[0].Retention.Verdicts)
	if verdictsBefore == 0 {
		t.Fatal("retention classified 0 artifacts after a completed cycle — this test would prove nothing")
	}
	filesBefore := b34DirEntries(t, localDir)
	deletesBefore := tr.deleteCallCount()

	// Phase 2: storage goes critical, with a second artifact arriving.
	tr.put("backup-b.dump", "the second artifact, which must never be copied", epoch.Unix())
	svc.Capacity.WarningFreeBytes = b34ImpossibleFreeBytes
	svc.Capacity.CriticalFreeBytes = b34ImpossibleFreeBytes

	const pressuredCycles = 5
	var pressured CycleReport
	for i := 0; i < pressuredCycles; i++ {
		pressured = svc.RunCycle(context.Background())
		if err := pressured.Sets[0].Err; err != nil {
			t.Fatalf("pressured cycle %d: BackupSetCycleResult.Err = %v, want nil", i, err)
		}
	}

	if got := len(pressured.Sets[0].Retention.Verdicts); got != verdictsBefore {
		t.Errorf("retention classified %d artifacts under critical storage, %d before it — storage pressure must not change what retention decides", got, verdictsBefore)
	}
	if got := tr.deleteCallCount(); got != deletesBefore {
		t.Errorf("DeleteRemote calls went from %d to %d under critical storage — nothing about a full local disk justifies removing a remote original", deletesBefore, got)
	}

	after, err := journal.Get(context.Background(), completed)
	if err != nil {
		t.Fatalf("Get after the pressured cycles: %v", err)
	}
	if after.State != string(lifecycle.Complete) {
		t.Errorf("the completed artifact is now %q, want %q — storage pressure must not retire, delete or otherwise disturb an artifact retention is already protecting", after.State, lifecycle.Complete)
	}

	if got := b34DirEntries(t, localDir); !b34SameNames(got, filesBefore) {
		t.Errorf("local destination contains %v after %d critical-storage cycles, want %v unchanged: nothing may be deleted to make room, and no partial file may be left behind",
			got, pressuredCycles, filesBefore)
	}
}

// b34SameNames reports whether two directory listings hold the same
// names. os.ReadDir already returns them sorted, so a plain elementwise
// comparison is enough.
func b34SameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
