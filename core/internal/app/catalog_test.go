package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/recovery"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// openJournalAt opens a real, on-disk journal at an explicit path, unlike
// openJournal (helpers_test.go), which always picks a fresh one inside its
// own t.TempDir(). This file's tests need to control the path: they close
// a journal, delete the file, and reopen a brand new one at the very same
// path to simulate the SQLite journal itself being lost.
func openJournalAt(t *testing.T, path string) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// TestRebuildCatalog_ReconstructsFromSidecarManifestsAfterJournalLoss is
// issue #102's central scenario: a backup set's journal is produced by a
// real cycle (so its sidecar manifest is the real one commit.go writes,
// not a hand-built fixture), the journal file is then destroyed and
// reopened fresh at the same path (simulating the SQLite journal being
// deleted or corrupted and an operator removing the corrupted file, per
// this project's existing "refuse rather than guess" convention for a
// database state.Open cannot make sense of), and RebuildCatalog is proven
// to reconstruct a row whose identity and retention-relevant timestamp
// match the original exactly, first previewing it with --dry-run
// (which must write nothing), then for real.
func TestRebuildCatalog_ReconstructsFromSidecarManifestsAfterJournalLoss(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = "" // fakeTransport ignores Source.Root; see pipeline_test.go's testBackupSet caller.

	tr := newFakeTransport()
	tr.put("backup.dump", "payload for rebuild", epoch.Unix())

	dbPath := filepath.Join(t.TempDir(), "journal.db")
	journal := openJournalAt(t, dbPath)
	cfg := testConfig(t, testSource("production", bs))
	svc := New(cfg, journal, tr, nil)
	svc.Now = fixedNow(epoch)

	ctx := context.Background()
	svc.RunCycle(ctx)

	artifact, err := model.NewArtifactID(bs.ID, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	before, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("precondition: Get before loss: %v", err)
	}
	switch lifecycle.State(before.State) {
	case lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.Complete:
	default:
		t.Fatalf("precondition: unexpected state %s before simulated loss", before.State)
	}

	manifestPath := recovery.ManifestPath(localDir, "backup.dump")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("precondition: sidecar manifest missing before simulated loss: %v", err)
	}

	// Simulate total state loss: close the journal, remove the file, and
	// open a brand new one at the same path. state.Open creates a fresh,
	// empty, freshly-migrated schema when nothing is there, exactly as it
	// would for an operator who removed a corrupted database by hand.
	if err := journal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	lostJournal := openJournalAt(t, dbPath)
	svc2 := New(cfg, lostJournal, tr, nil)
	svc2.Now = fixedNow(epoch)

	if _, err := lostJournal.Get(ctx, artifact); !errors.Is(err, state.ErrArtifactNotFound) {
		t.Fatalf("precondition: journal should be empty after simulated loss, got err=%v", err)
	}

	// Dry run: must report exactly what it would reconstruct, and write
	// nothing.
	dryReport, err := svc2.RebuildCatalog(ctx, bs.ID, true)
	if err != nil {
		t.Fatalf("RebuildCatalog dry-run: %v", err)
	}
	if len(dryReport.Errors) != 0 {
		t.Fatalf("dry-run Errors = %+v, want none", dryReport.Errors)
	}
	if len(dryReport.Findings) != 1 || dryReport.Findings[0].Artifact != artifact || dryReport.Findings[0].Action != CatalogRebuildReconstructed {
		t.Fatalf("dry-run Findings = %+v, want exactly one CatalogRebuildReconstructed finding for %s", dryReport.Findings, artifact)
	}
	if _, err := lostJournal.Get(ctx, artifact); !errors.Is(err, state.ErrArtifactNotFound) {
		t.Fatalf("dry-run wrote a journal row; want none, got err=%v", err)
	}

	// Real run: must reconstruct a matching row.
	report, err := svc2.RebuildCatalog(ctx, bs.ID, false)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Errors = %+v, want none", report.Errors)
	}
	if len(report.Findings) != 1 || report.Findings[0].Action != CatalogRebuildReconstructed {
		t.Fatalf("Findings = %+v, want exactly one CatalogRebuildReconstructed finding", report.Findings)
	}

	after, err := lostJournal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get after rebuild: %v", err)
	}
	if after.State != string(lifecycle.RemoteDeletePending) {
		t.Errorf("State = %s, want REMOTE_DELETE_PENDING", after.State)
	}
	if !after.DiscoveredAt.Equal(before.DiscoveredAt) {
		t.Errorf("DiscoveredAt = %v, want %v (the retention-relevant timestamp must survive rebuild)", after.DiscoveredAt, before.DiscoveredAt)
	}
	if after.RemotePath != before.RemotePath {
		t.Errorf("RemotePath = %q, want %q", after.RemotePath, before.RemotePath)
	}
	wantLocal := mustFinalArtifactPath(t, localDir, artifact)
	if after.LocalPath != wantLocal {
		t.Errorf("LocalPath = %q, want %q", after.LocalPath, wantLocal)
	}
	if after.LocalHash != before.LocalHash || after.LocalHashAlg != before.LocalHashAlg {
		t.Errorf("LocalHash/Alg = %s/%s, want %s/%s", after.LocalHash, after.LocalHashAlg, before.LocalHash, before.LocalHashAlg)
	}
	if before.Remote.Size == nil || after.Remote.Size == nil || *after.Remote.Size != *before.Remote.Size {
		t.Errorf("Remote.Size = %v, want %v", after.Remote.Size, before.Remote.Size)
	}

	// Running again must recognise the now-populated row and skip it.
	again, err := svc2.RebuildCatalog(ctx, bs.ID, false)
	if err != nil {
		t.Fatalf("second RebuildCatalog: %v", err)
	}
	if len(again.Findings) != 1 || again.Findings[0].Action != CatalogRebuildAlreadyPresent {
		t.Fatalf("second run Findings = %+v, want exactly one CatalogRebuildAlreadyPresent", again.Findings)
	}
}

// TestRebuildCatalog_AdoptsBackupRootWithNoPriorJournal is issue #102's RED
// bullet for adoption, distinct from recovering a lost journal: a backup
// root whose artifacts and sidecar manifests were produced under one
// journal is pointed at by a second journal that was never associated
// with it at all (a brand new path, never opened before), the way a fresh
// install adopting a pre-existing backup tree would be. RebuildCatalog
// must reconstruct it exactly the same way it recovers a lost one.
func TestRebuildCatalog_AdoptsBackupRootWithNoPriorJournal(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""
	tr := newFakeTransport()
	tr.put("backup.dump", "adoption payload", epoch.Unix())
	cfg := testConfig(t, testSource("production", bs))

	producerJournal := openJournal(t)
	producer := New(cfg, producerJournal, tr, nil)
	producer.Now = fixedNow(epoch)
	ctx := context.Background()
	producer.RunCycle(ctx)

	// A brand new journal, at a path this backup root has never been
	// pointed at before: this is adoption, not recovery from a specific
	// loss.
	freshJournal := openJournal(t)
	adopting := New(cfg, freshJournal, tr, nil)

	report, err := adopting.RebuildCatalog(ctx, bs.ID, false)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Errors = %+v, want none", report.Errors)
	}
	if len(report.Findings) != 1 || report.Findings[0].Action != CatalogRebuildReconstructed {
		t.Fatalf("Findings = %+v, want exactly one CatalogRebuildReconstructed", report.Findings)
	}

	artifact, err := model.NewArtifactID(bs.ID, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	rec, err := freshJournal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get after adoption: %v", err)
	}
	if rec.State != string(lifecycle.RemoteDeletePending) {
		t.Errorf("State = %s, want REMOTE_DELETE_PENDING", rec.State)
	}
}

// TestRebuildCatalog_ReportsManifestErrorsWithoutAbortingOthers proves one
// corrupted sidecar manifest never hides another artifact's legitimate
// recovery metadata.
func TestRebuildCatalog_ReportsManifestErrorsWithoutAbortingOthers(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""
	tr := newFakeTransport()
	tr.put("good.dump", "good payload", epoch.Unix())
	cfg := testConfig(t, testSource("production", bs))

	producerJournal := openJournal(t)
	producer := New(cfg, producerJournal, tr, nil)
	producer.Now = fixedNow(epoch)
	ctx := context.Background()
	producer.RunCycle(ctx)

	corruptPath := recovery.ManifestPath(localDir, "corrupt.dump")
	if err := os.WriteFile(corruptPath, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	freshJournal := openJournal(t)
	svc := New(cfg, freshJournal, tr, nil)
	report, err := svc.RebuildCatalog(ctx, bs.ID, false)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Action != CatalogRebuildReconstructed {
		t.Fatalf("Findings = %+v, want exactly one reconstructed (good.dump)", report.Findings)
	}
	if len(report.Errors) != 1 || report.Errors[0].Path != corruptPath {
		t.Fatalf("Errors = %+v, want exactly one for %s", report.Errors, corruptPath)
	}
}

// TestRebuildCatalog_ManifestFromWrongBackupSet_ReportsErrorWithoutWritingRow
// exercises catalog.go's FR-7 backup-set isolation guard: a sidecar
// manifest sitting inside one backup set's LocalPath but declaring a
// different backup set's identity, the shape a manifest hand-placed or
// copied there by operator error or a bad restore/migration would take,
// must be rejected into report.Errors rather than silently reconstructed
// into the wrong backup set's catalog, and must leave no journal row
// behind for it.
func TestRebuildCatalog_ManifestFromWrongBackupSet_ReportsErrorWithoutWritingRow(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	cfg := testConfig(t, testSource("production", bs))

	journal := openJournal(t)
	svc := New(cfg, journal, nil, nil)

	wrongSet := mustSetID(t, "production", "other-set")
	m := recovery.Manifest{
		FormatVersion:      recovery.CurrentFormatVersion,
		Source:             wrongSet.Source,
		BackupSet:          wrongSet.Set,
		ArtifactName:       "misplaced.dump",
		RemotePath:         "/backups/misplaced.dump",
		ReceivedTimestamp:  epoch,
		RetentionTimestamp: epoch,
		SizeBytes:          123,
	}
	if err := recovery.WriteManifest(localDir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	ctx := context.Background()
	report, err := svc.RebuildCatalog(ctx, bs.ID, false)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("Findings = %+v, want none (a wrong-set manifest must never be reconstructed)", report.Findings)
	}

	wantPath := recovery.ManifestPath(localDir, "misplaced.dump")
	wantErr := fmt.Sprintf("recovery: manifest declares backup set %s, expected %s", wrongSet, bs.ID)
	if len(report.Errors) != 1 || report.Errors[0].Path != wantPath || report.Errors[0].Err.Error() != wantErr {
		t.Fatalf("Errors = %+v, want exactly one {Path: %s, Err: %q}", report.Errors, wantPath, wantErr)
	}

	misplaced, err := model.NewArtifactID(wrongSet, "misplaced.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := journal.Get(ctx, misplaced); !errors.Is(err, state.ErrArtifactNotFound) {
		t.Errorf("journal row for misplaced artifact after FR-7 rejection: err=%v, want ErrArtifactNotFound (no row must be written)", err)
	}
}

// TestRebuildCatalog_NeverTouchesLocalOrRemoteFiles is issue #102's
// non-destructive-safety test: RebuildCatalog must never modify the
// committed artifact's own file, and must never call the transport.
func TestRebuildCatalog_NeverTouchesLocalOrRemoteFiles(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""
	tr := newFakeTransport()
	tr.put("backup.dump", "untouched payload", epoch.Unix())
	cfg := testConfig(t, testSource("production", bs))

	dbPath := filepath.Join(t.TempDir(), "journal.db")
	journal := openJournalAt(t, dbPath)
	svc := New(cfg, journal, tr, nil)
	svc.Now = fixedNow(epoch)
	ctx := context.Background()
	svc.RunCycle(ctx)

	artifact, err := model.NewArtifactID(bs.ID, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	final := mustFinalArtifactPath(t, localDir, artifact)
	beforeContent, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	beforeInfo, err := os.Stat(final)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	deleteCallsBefore := tr.deleteCallCount()
	copyCallsBefore := tr.copyToLocalCalls()

	if err := journal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	lostJournal := openJournalAt(t, dbPath)
	svc2 := New(cfg, lostJournal, tr, nil)

	if _, err := svc2.RebuildCatalog(ctx, bs.ID, false); err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}

	afterContent, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(afterContent) != string(beforeContent) {
		t.Error("rebuild modified the committed artifact's content")
	}
	afterInfo, err := os.Stat(final)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Error("rebuild modified the committed artifact's mtime")
	}
	if tr.deleteCallCount() != deleteCallsBefore {
		t.Errorf("DeleteRemote calls = %d, want %d (rebuild must never touch the remote)", tr.deleteCallCount(), deleteCallsBefore)
	}
	if tr.copyToLocalCalls() != copyCallsBefore {
		t.Errorf("CopyToLocal calls = %d, want %d (rebuild must never touch the remote)", tr.copyToLocalCalls(), copyCallsBefore)
	}
}

// TestRebuildCatalog_ThenReconcile_RemoteAbsentInvalidLocal_RoutesToQuarantinedLost
// is issue #102's INTEGRATION test: it runs internal/reconcile's FR-17
// pass (through Service.ReconcileAll, exactly the code path `run`/`daemon`
// use) against a freshly rebuilt journal and confirms a rebuilt row that
// is missing on the remote and invalid locally still routes to the same
// terminal QUARANTINED_LOST state a normal journal row would (see
// internal/reconcile's own
// TestReconcile_DeletePending_RemoteAbsent_InvalidLocal_QuarantinesAsLost,
// which this test's assertions deliberately mirror).
func TestRebuildCatalog_ThenReconcile_RemoteAbsentInvalidLocal_RoutesToQuarantinedLost(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""
	tr := newFakeTransport()
	tr.put("backup.dump", "integration payload", epoch.Unix())
	cfg := testConfig(t, testSource("production", bs))

	dbPath := filepath.Join(t.TempDir(), "journal.db")
	journal := openJournalAt(t, dbPath)
	svc := New(cfg, journal, tr, nil)
	svc.Now = fixedNow(epoch)
	ctx := context.Background()
	svc.RunCycle(ctx)

	artifact, err := model.NewArtifactID(bs.ID, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	// Simulate total journal loss, then rebuild.
	if err := journal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	lostJournal := openJournalAt(t, dbPath)
	svc2 := New(cfg, lostJournal, tr, nil)

	if _, err := svc2.RebuildCatalog(ctx, bs.ID, false); err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	rebuilt, err := lostJournal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get after rebuild: %v", err)
	}
	if rebuilt.State != string(lifecycle.RemoteDeletePending) {
		t.Fatalf("precondition: rebuilt state = %s, want REMOTE_DELETE_PENDING", rebuilt.State)
	}

	// Now the boundary condition the integration bullet asks for: the
	// remote copy is gone, and the local durable copy is invalid.
	delete(tr.objects, "backup.dump")
	final := mustFinalArtifactPath(t, localDir, artifact)
	if err := os.WriteFile(final, []byte("corrupted after rebuild"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reports := svc2.ReconcileAll(ctx)
	if len(reports) != 1 || reports[0].Err != nil {
		t.Fatalf("ReconcileAll = %+v", reports)
	}
	if len(reports[0].Report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one", reports[0].Report.Findings)
	}
	f := reports[0].Report.Findings[0]
	if f.To != lifecycle.QuarantinedLost {
		t.Errorf("To = %s, want QUARANTINED_LOST (a rebuilt row must reconcile exactly the way a normal journal row would); reason=%s", f.To, f.Reason)
	}

	final2, err := lostJournal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if final2.State != string(lifecycle.QuarantinedLost) {
		t.Errorf("journal State = %s, want QUARANTINED_LOST", final2.State)
	}
}

// TestRebuildCatalog_UnknownBackupSet_ReportsNotFound proves a caller
// naming a backup set that is not in config gets a clear error, not a
// nil-pointer panic or a silent no-op.
func TestRebuildCatalog_UnknownBackupSet_ReportsNotFound(t *testing.T) {
	journal := openJournal(t)
	svc := New(testConfig(t), journal, nil, nil)
	unknown := mustSetID(t, "nowhere", "nothing")

	if _, err := svc.RebuildCatalog(context.Background(), unknown, true); err == nil {
		t.Fatal("RebuildCatalog with an unconfigured backup set: want an error, got nil")
	}
}

// mustFinalArtifactPath is the test-side spelling of the error return
// lifecycle.FinalArtifactPath grew with issue #334's conversion. Every call
// here supplies a real directory, so an error is a broken test.
func mustFinalArtifactPath(t *testing.T, localDir string, artifact model.ArtifactID) string {
	t.Helper()
	p, err := lifecycle.FinalArtifactPath(localDir, artifact)
	if err != nil {
		t.Fatalf("lifecycle.FinalArtifactPath(%q, %s): %v", localDir, artifact, err)
	}
	return p
}
