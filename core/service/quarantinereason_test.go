package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is issue #308's proof at the core/service boundary: GET
// /api/v1/backups and GET /api/v1/quarantine both render quarantine_reason
// from core/service.Artifact.QuarantineReason (apps/common/webhost's
// artifactResponse mirrors it field for field), and until this existed it
// was empty for every quarantine cause except the application-validator
// one. toServiceArtifact derived it from rec.LastError (only ever set at
// RELEASE time by internal/lifecycle.ReleaseFromQuarantine, so always
// empty for an artifact still sitting in quarantine) falling back to
// rec.ValidationDetail (only set by internal/lifecycle/verify.go's
// application-validator branch). internal/reconcile's own quarantine
// transitions - exercised here through a real second run cycle over a
// corrupted, already-committed local file - set neither, which is exactly
// the second-order bug issue #284's own agent found while building #284
// (an unrelated CLI feature) and filed separately as #308.

// TestGetArtifact_QuarantineReasonCarriesTheReconcileDetail proves
// GetArtifact's QuarantineReason for a reconcile-triggered quarantine
// (FR-17: a durable local copy found invalid after COMMITTED) is the
// literal sentence internal/reconcile recorded, not empty and not a
// generic placeholder.
func TestGetArtifact_QuarantineReasonCarriesTheReconcileDetail(t *testing.T) {
	svc, configPath := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	const id = "production/postgres-primary/backup.dump"
	before, err := svc.GetArtifact(ctx, id)
	if err != nil {
		t.Fatalf("GetArtifact (before): %v", err)
	}
	if before.Quarantined {
		t.Fatalf("Quarantined = true after the first cycle; want a clean commit so the corruption below is what causes the quarantine, not something else")
	}

	localFinal := filepath.Join(filepath.Dir(configPath), "local", "backup.dump")
	if err := os.WriteFile(localFinal, []byte("tampered bytes that do not match the recorded hash"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A second cycle: RunCycle always reconciles a backup set's existing
	// journal rows (FR-17) before touching anything else, so this is what
	// discovers the tampered COMMITTED artifact's local copy is now
	// invalid and quarantines it - the real production path, not a
	// hand-built lifecycle.Advance call.
	runOneCycle(t, svc)

	got, err := svc.GetArtifact(ctx, id)
	if err != nil {
		t.Fatalf("GetArtifact (after): %v", err)
	}
	if !got.Quarantined {
		t.Fatalf("Quarantined = false after corrupting the committed local file and running another cycle, want true: reconcile should have caught it")
	}
	if got.QuarantineReason == "" {
		t.Fatal("QuarantineReason is empty for a reconcile-triggered quarantine; this is issue #308")
	}
	assertReconcileDetail(t, got.QuarantineReason)
}

// TestListArtifacts_QuarantineReasonCarriesTheReconcileDetail is the same
// proof through ListArtifacts (GET /api/v1/backups and GET
// /api/v1/quarantine's shared read path), which builds its own
// []Artifact independently of GetArtifact and had exactly the same gap.
func TestListArtifacts_QuarantineReasonCarriesTheReconcileDetail(t *testing.T) {
	svc, configPath := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	localFinal := filepath.Join(filepath.Dir(configPath), "local", "backup.dump")
	if err := os.WriteFile(localFinal, []byte("tampered bytes that do not match the recorded hash"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runOneCycle(t, svc)

	got, err := svc.ListArtifacts(ctx, ArtifactFilter{QuarantinedOnly: true})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want exactly 1 quarantined artifact: %+v", len(got), got)
	}
	if got[0].QuarantineReason == "" {
		t.Fatal("QuarantineReason is empty for a reconcile-triggered quarantine; this is issue #308")
	}
	assertReconcileDetail(t, got[0].QuarantineReason)
}

// assertReconcileDetail checks reason is the literal, specific sentence
// internal/reconcile recorded, not merely non-empty. The local-transport
// fixture these tests share deletes the remote object right after commit
// (FR-15), so by the time the second cycle's reconcile pass finds the
// tampered local file, the artifact has already reached COMPLETE and
// takes reconcile.go's COMPLETE -> QUARANTINED_LOST branch ("the remote
// source is already confirmed gone and the durable local copy is now
// invalid: ..."), not the COMMITTED -> QUARANTINED branch ("reconciliation
// found the durable local copy invalid: ..."). Both are FR-17, both carry
// the same underlying checkLocalFinal reason text (the byte-count
// mismatch), so this asserts on what both branches actually share rather
// than pinning one exact sentence.
func assertReconcileDetail(t *testing.T, reason string) {
	t.Helper()
	if !strings.Contains(reason, "FR-17") {
		t.Errorf("QuarantineReason = %q, want the journal's own recorded FR-17 sentence, not a generic placeholder", reason)
	}
	if !strings.Contains(reason, "bytes, expected") {
		t.Errorf("QuarantineReason = %q, want the specific byte-count mismatch checkLocalFinal recorded, not a generic placeholder", reason)
	}
}
