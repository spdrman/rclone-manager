package reconcile

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// --- assertion helpers ---

func requireNoErrors(t *testing.T, report Report) {
	t.Helper()
	if len(report.Errors) != 0 {
		t.Fatalf("report.Errors = %v, want none", report.Errors)
	}
}

func requireOneFinding(t *testing.T, report Report) Finding {
	t.Helper()
	if len(report.Findings) != 1 {
		t.Fatalf("report.Findings has %d entries, want 1: %+v", len(report.Findings), report.Findings)
	}
	return report.Findings[0]
}

func assertJournalState(t *testing.T, j *state.Journal, artifact model.ArtifactID, want lifecycle.State) state.Record {
	t.Helper()
	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(want) {
		t.Errorf("journal state = %s, want %s", rec.State, want)
	}
	return rec
}

// remotePathFor mirrors driveTo's own construction, so a test can build the
// exact key fakeTransport.stat needs without repeating the literal.
func remotePathFor(artifact model.ArtifactID) string {
	return "backups/" + artifact.Name
}

func statTransportFor(artifact model.ArtifactID, fn func() (transport.RemoteArtifact, error)) *fakeTransport {
	return &fakeTransport{stat: map[string]func() (transport.RemoteArtifact, error){
		remotePathFor(artifact): fn,
	}}
}

// --- input validation ---

func TestReconcile_RefusesIncompleteDeps(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	tp := &fakeTransport{}
	set := testSet(t)

	if _, err := Reconcile(ctx, Deps{Transport: tp}, testSource, set); err == nil {
		t.Error("Reconcile with no Journal succeeded, want an error")
	}
	if _, err := Reconcile(ctx, Deps{Journal: j}, testSource, set); err == nil {
		t.Error("Reconcile with no Transport succeeded, want an error")
	}
	if _, err := Reconcile(ctx, Deps{Journal: j, Transport: tp}, testSource, model.BackupSetID{}); err == nil {
		t.Error("Reconcile with a zero backup set succeeded, want an error")
	}
}

// --- row: exists, absent, DISCOVERED -> transfer ---

func TestReconcile_Discovered_IsAlreadyConsistent(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "discovered.dump")
	size := int64(10)
	driveTo(t, j, driveParams{artifact: artifact, remote: state.RemoteIdentity{Size: &size}, stopAt: lifecycle.Discovered})

	tp := &fakeTransport{}
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.Changed() {
		t.Errorf("Finding.Changed() = true, want false (From=%s To=%s)", f.From, f.To)
	}
	if f.From != lifecycle.Discovered {
		t.Errorf("From = %s, want DISCOVERED", f.From)
	}
	if tp.statCalls != 0 {
		t.Errorf("statCalls = %d, want 0: DISCOVERED never needs to check the remote", tp.statCalls)
	}
}

// --- row: exists, partial, TRANSFERRING -> safe retry/restart,
// generalised to every pre-COMMITTED state ---

func TestReconcile_PreCommitStates_AreAlreadyConsistent(t *testing.T) {
	states := []lifecycle.State{
		lifecycle.Transferring,
		lifecycle.Transferred,
		lifecycle.Verifying,
		lifecycle.Verified,
		lifecycle.Committing,
	}
	for _, st := range states {
		t.Run(string(st), func(t *testing.T) {
			j := openTestJournal(t)
			artifact := testArtifact(t, "inflight.dump")
			size := int64(10)
			localPath := writeLocalFile(t, size) // never consulted before COMMITTED
			driveTo(t, j, driveParams{
				artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
				transfer: &state.TransferResult{BytesTransferred: size}, stopAt: st,
			})

			tp := &fakeTransport{}
			report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			requireNoErrors(t, report)
			f := requireOneFinding(t, report)
			if f.Changed() {
				t.Errorf("Finding.Changed() = true, want false (From=%s To=%s)", f.From, f.To)
			}
			if tp.statCalls != 0 {
				t.Errorf("statCalls = %d, want 0: %s never needs to check the remote", tp.statCalls, st)
			}
		})
	}
}

// --- row: exists, final, COMMITTED -> verify and proceed toward delete ---

func TestReconcile_Committed_ValidLocal_IsConsistent(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "committed-valid.dump")
	size := int64(128)
	localPath := writeLocalFile(t, size)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Committed,
	})

	tp := &fakeTransport{} // COMMITTED must never call Stat, see reconcileCommitted's doc.
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.Changed() {
		t.Errorf("Finding.Changed() = true, want false (To=%s)", f.To)
	}
	if tp.statCalls != 0 {
		t.Errorf("statCalls = %d, want 0", tp.statCalls)
	}
	assertJournalState(t, j, artifact, lifecycle.Committed)
}

// --- row: exists, invalid final, any (COMMITTED half) -> preserve remote; quarantine local ---

func TestReconcile_Committed_InvalidLocal_Quarantines(t *testing.T) {
	cases := []struct {
		name      string
		localPath func(t *testing.T) string
	}{
		{"corrupted size", func(t *testing.T) string { return writeLocalFile(t, 4) }}, // recorded size is 128
		{"missing file", func(t *testing.T) string { return t.TempDir() + "/never-written.final" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := openTestJournal(t)
			artifact := testArtifact(t, "committed-invalid.dump")
			size := int64(128)
			driveTo(t, j, driveParams{
				artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: tc.localPath(t),
				transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Committed,
			})

			tp := &fakeTransport{}
			report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			requireNoErrors(t, report)
			f := requireOneFinding(t, report)
			if f.To != lifecycle.Quarantined {
				t.Errorf("To = %s, want QUARANTINED (reason: %s)", f.To, f.Reason)
			}
			if tp.statCalls != 0 {
				t.Errorf("statCalls = %d, want 0: an invalid COMMITTED copy can only reach QUARANTINED, never QUARANTINED_LOST, so remote status is irrelevant", tp.statCalls)
			}
			assertJournalState(t, j, artifact, lifecycle.Quarantined)
		})
	}
}

// --- row: absent, final, REMOTE_DELETE_PENDING -> reconcile COMPLETE ---

func TestReconcile_DeletePending_RemoteAbsent_ValidLocal_ReconcilesComplete(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "pending-absent-valid.dump")
	size := int64(64)
	localPath := writeLocalFile(t, size)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteDeletePending,
	})

	tp := statTransportFor(artifact, statNotFound)
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.To != lifecycle.Complete {
		t.Errorf("To = %s, want COMPLETE (reason: %s)", f.To, f.Reason)
	}
	rec := assertJournalState(t, j, artifact, lifecycle.Complete)
	if rec.RemoteDeletedAt == nil {
		t.Error("RemoteDeletedAt was not recorded")
	}
}

// --- row: absent, final, COMPLETE -> no-op ---

func TestReconcile_Complete_ValidLocal_IsConsistent(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "complete-valid.dump")
	size := int64(32)
	localPath := writeLocalFile(t, size)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Complete,
	})

	tp := &fakeTransport{} // COMPLETE must never call Stat either, see reconcileComplete's doc.
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.Changed() {
		t.Errorf("Finding.Changed() = true, want false (To=%s)", f.To)
	}
	if tp.statCalls != 0 {
		t.Errorf("statCalls = %d, want 0", tp.statCalls)
	}
}

// TestReconcile_RemoteRetained_IsConsistent proves issue #282's terminal
// state does not break FR-17's startup pass: reconcileOne must not error
// out (the pre-#282 default case would have refused every unrecognised
// state), must take no action, and must never call Stat -- this manager
// never examined the remote object on the way into REMOTE_RETAINED and has
// no business examining it now either.
func TestReconcile_RemoteRetained_IsConsistent(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "retained.dump")
	size := int64(32)
	localPath := writeLocalFile(t, size)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteRetained,
	})

	tp := &fakeTransport{}
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.Changed() {
		t.Errorf("Finding.Changed() = true, want false (From=%s To=%s)", f.From, f.To)
	}
	if f.From != lifecycle.RemoteRetained {
		t.Errorf("From = %s, want REMOTE_RETAINED", f.From)
	}
	if tp.statCalls != 0 {
		t.Errorf("statCalls = %d, want 0: a read-only set's retained remote is never examined", tp.statCalls)
	}
}

// --- the gap row this package adds: absent, invalid final, any -> quarantine, unrecoverable ---

func TestReconcile_Complete_InvalidLocal_QuarantinesAsLost(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "complete-invalid.dump")
	size := int64(32)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: t.TempDir() + "/never-written.final",
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Complete,
	})

	tp := &fakeTransport{}
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.To != lifecycle.QuarantinedLost {
		t.Errorf("To = %s, want QUARANTINED_LOST (reason: %s)", f.To, f.Reason)
	}
	if tp.statCalls != 0 {
		t.Errorf("statCalls = %d, want 0: COMPLETE already means the remote is confirmed gone", tp.statCalls)
	}
	assertJournalState(t, j, artifact, lifecycle.QuarantinedLost)
}

func TestReconcile_DeletePending_RemoteAbsent_InvalidLocal_QuarantinesAsLost(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "pending-absent-invalid.dump")
	size := int64(32)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: t.TempDir() + "/never-written.final",
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteDeletePending,
	})

	tp := statTransportFor(artifact, statNotFound)
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.From != lifecycle.RemoteDeletePending {
		t.Errorf("From = %s, want REMOTE_DELETE_PENDING", f.From)
	}
	if f.To != lifecycle.QuarantinedLost {
		t.Errorf("To = %s, want QUARANTINED_LOST (reason: %s)", f.To, f.Reason)
	}
	// The journal must actually have passed through COMPLETE on the way:
	// there is no direct REMOTE_DELETE_PENDING -> QUARANTINED_LOST edge in
	// machine.go, so if reconcileToLost ever tried to skip it,
	// lifecycle.Advance would have refused the whole call and this
	// assertion would be looking at an error, not QUARANTINED_LOST.
	rec := assertJournalState(t, j, artifact, lifecycle.QuarantinedLost)
	if rec.RemoteDeletedAt == nil {
		t.Error("RemoteDeletedAt was not recorded by the intermediate COMPLETE step")
	}
}

// --- row: exists, invalid final, any (REMOTE_DELETE_PENDING half) ---

func TestReconcile_DeletePending_RemoteExists_InvalidLocal_Quarantines(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "pending-present-invalid.dump")
	size := int64(32)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: t.TempDir() + "/never-written.final",
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteDeletePending,
	})

	tp := statTransportFor(artifact, func() (transport.RemoteArtifact, error) {
		return transport.RemoteArtifact{Path: remotePathFor(artifact), Size: size}, nil
	})
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.To != lifecycle.Quarantined {
		t.Errorf("To = %s, want QUARANTINED (reason: %s)", f.To, f.Reason)
	}
	assertJournalState(t, j, artifact, lifecycle.Quarantined)
}

// --- row: changed identity, final, delete pending -> refuse delete; investigate ---

func TestReconcile_DeletePending_IdentityChanged_NeedsInvestigation(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "pending-changed.dump")
	size := int64(32)
	localPath := writeLocalFile(t, size)
	driveTo(t, j, driveParams{
		artifact:  artifact,
		remote:    state.RemoteIdentity{Size: &size, Hash: "aaaaaaaa", HashAlg: "sha256"},
		localPath: localPath, transfer: &state.TransferResult{BytesTransferred: size},
		stopAt: lifecycle.RemoteDeletePending,
	})

	tp := statTransportFor(artifact, func() (transport.RemoteArtifact, error) {
		return transport.RemoteArtifact{
			Path: remotePathFor(artifact), Size: size, Hash: "bbbbbbbb", HashAlg: transport.SHA256,
		}, nil
	})
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if !f.NeedsInvestigation {
		t.Error("NeedsInvestigation = false, want true")
	}
	if f.Changed() {
		t.Errorf("Finding.Changed() = true, want false: a changed identity refuses the delete, it does not resolve it (From=%s To=%s)", f.From, f.To)
	}
	assertJournalState(t, j, artifact, lifecycle.RemoteDeletePending)
}

// Unconfirmed identity (the routine, expected outcome against a hardened
// SFTP account per internal/lifecycle/remotedelete.go's own package doc)
// must never be treated as "needs investigation": that is not a confirmed
// finding, and DeleteRemote's own retry re-checks it fully every time it
// actually runs.
func TestReconcile_DeletePending_IdentityUnconfirmed_IsConsistent(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "pending-unconfirmed.dump")
	size := int64(32)
	localPath := writeLocalFile(t, size)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, // no hash, no mtime
		localPath: localPath, transfer: &state.TransferResult{BytesTransferred: size},
		stopAt: lifecycle.RemoteDeletePending,
	})

	tp := statTransportFor(artifact, func() (transport.RemoteArtifact, error) {
		return transport.RemoteArtifact{Path: remotePathFor(artifact), Size: size}, nil
	})
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.NeedsInvestigation {
		t.Error("NeedsInvestigation = true, want false: an unconfirmed identity is the routine case, not a finding")
	}
	if f.Changed() {
		t.Errorf("Finding.Changed() = true, want false (From=%s To=%s)", f.From, f.To)
	}
}

func TestReconcile_DeletePending_IdentityUnchanged_IsConsistent(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "pending-unchanged.dump")
	size := int64(32)
	localPath := writeLocalFile(t, size)
	driveTo(t, j, driveParams{
		artifact:  artifact,
		remote:    state.RemoteIdentity{Size: &size, Hash: "cccccccc", HashAlg: "sha256"},
		localPath: localPath, transfer: &state.TransferResult{BytesTransferred: size},
		stopAt: lifecycle.RemoteDeletePending,
	})

	tp := statTransportFor(artifact, func() (transport.RemoteArtifact, error) {
		return transport.RemoteArtifact{
			Path: remotePathFor(artifact), Size: size, Hash: "cccccccc", HashAlg: transport.SHA256,
		}, nil
	})
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.NeedsInvestigation {
		t.Error("NeedsInvestigation = true, want false")
	}
	if f.Changed() {
		t.Errorf("Finding.Changed() = true, want false: an unchanged, still-present remote object is left for normal processing's own delete retry (From=%s To=%s)", f.From, f.To)
	}
}

// --- errors that must not abort the whole pass ---

func TestReconcile_AmbiguousStatError_IsReportedNotGuessed(t *testing.T) {
	j := openTestJournal(t)
	broken := testArtifact(t, "broken.dump")
	fine := testArtifact(t, "fine.dump")
	size := int64(16)

	driveTo(t, j, driveParams{
		artifact: broken, remote: state.RemoteIdentity{Size: &size}, localPath: writeLocalFile(t, size),
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteDeletePending,
	})
	driveTo(t, j, driveParams{artifact: fine, remote: state.RemoteIdentity{Size: &size}, stopAt: lifecycle.Discovered})

	tp := &fakeTransport{stat: map[string]func() (transport.RemoteArtifact, error){
		remotePathFor(broken): statAmbiguousErr,
	}}
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("report.Errors has %d entries, want 1: %+v", len(report.Errors), report.Errors)
	}
	if report.Errors[0].Artifact != broken {
		t.Errorf("errored artifact = %s, want %s", report.Errors[0].Artifact, broken)
	}
	if len(report.Findings) != 1 || report.Findings[0].Artifact != fine {
		t.Fatalf("report.Findings = %+v, want exactly one finding for %s", report.Findings, fine)
	}
	// The ambiguous error must not have moved the broken artifact anywhere.
	assertJournalState(t, j, broken, lifecycle.RemoteDeletePending)
}

func TestReconcileOne_UnknownJournalState_IsAnError(t *testing.T) {
	rec := state.Record{Artifact: testArtifact(t, "bogus.dump"), State: "BOGUS_STATE"}
	_, err := reconcileOne(context.Background(), Deps{}, testSource, rec)
	if err == nil {
		t.Fatal("reconcileOne succeeded on an unrecognised state, want an error")
	}
}
