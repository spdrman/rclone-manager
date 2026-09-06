package app

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Classification only, which is what makes these tests cheap.
//
// RetentionPreview computes verdicts and deletes nothing, so these run a real
// cycle to produce a genuine COMPLETE artifact and then ask what the policy
// says about it. Keeping the only complete artifact is the base case of every
// GFS chain and the one that fails first if tier resolution breaks.
//
// The deletion side is not here. prune_test.go owns it, because that is where
// files actually disappear and where the interesting assertions are about
// what stays on disk.

// TestRetentionPreview_KeepsTheOnlyCompleteArtifact drives one artifact
// all the way to COMPLETE through the real pipeline (exactly like
// pipeline_test.go's control case), then checks that RetentionPreview
// reports it as KEEP: with only one managed-complete artifact in the
// backup set, it is trivially both the newest daily bucket's
// representative and FR-19's last-known-good protected artifact.
func TestRetentionPreview_KeepsTheOnlyCompleteArtifact(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "retention-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)
	svc.processArtifact(ctx, source, bs, rec)

	final, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Fatalf("precondition failed: journal state = %q, want %q", final.State, lifecycle.Complete)
	}

	report, err := svc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview: %v", err)
	}
	if len(report.Verdicts) != 1 {
		t.Fatalf("Verdicts = %+v, want exactly one", report.Verdicts)
	}
	v := report.Verdicts[0]
	if !v.Keep {
		t.Errorf("Verdicts[0].Keep = false, want true")
	}
	if v.Artifact != rec.Artifact {
		t.Errorf("Verdicts[0].Artifact = %s, want %s", v.Artifact, rec.Artifact)
	}

	if !report.LastKnownGood.Protected {
		t.Error("LastKnownGood.Protected = false, want true: the only complete artifact must be last-known-good")
	}
	if report.LastKnownGood.Artifact != rec.Artifact {
		t.Errorf("LastKnownGood.Artifact = %s, want %s", report.LastKnownGood.Artifact, rec.Artifact)
	}
}

// TestRetentionPreviewAll_CoversEveryConfiguredBackupSet is a smoke test
// that the all-backup-sets helper visits every one of them, in config
// order, without needing any of them to have artifacts yet.
func TestRetentionPreviewAll_CoversEveryConfiguredBackupSet(t *testing.T) {
	firstBS := testBackupSet(t, t.TempDir())
	firstBS.Name = "first"
	firstBS.ID = mustSetID(t, "production", "first")

	secondBS := testBackupSet(t, t.TempDir())
	secondBS.Name = "second"
	secondBS.ID = mustSetID(t, "production", "second")

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", firstBS, secondBS)), journal, newFakeTransport(), nil)

	reports, err := svc.RetentionPreviewAll(context.Background())
	if err != nil {
		t.Fatalf("RetentionPreviewAll: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("len(reports) = %d, want 2", len(reports))
	}
	if reports[0].Set != firstBS.ID || reports[1].Set != secondBS.ID {
		t.Errorf("reports = %+v, want config order (first, second)", reports)
	}
	for _, r := range reports {
		if len(r.Verdicts) != 0 {
			t.Errorf("Set %s: Verdicts = %+v, want none (no artifacts discovered yet)", r.Set, r.Verdicts)
		}
	}
}
