package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/recovery"
)

// rebuildConflictFixture runs one full cycle so a real journal row and a
// real sidecar manifest exist and agree, then hands both back so a test
// can make the sidecar disagree.
func rebuildConflictFixture(t *testing.T) (svc *Service, artifact model.ArtifactID, setID model.BackupSetID, manifestPath string) {
	t.Helper()

	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup.dump", "payload for the conflict fixture", epoch.Unix())

	journal := openJournalAt(t, filepath.Join(t.TempDir(), "journal.db"))
	cfg := testConfig(t, testSource("production", bs))
	svc = New(cfg, journal, tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RunCycle(context.Background())

	artifact, err := model.NewArtifactID(bs.ID, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return svc, artifact, bs.ID, recovery.ManifestPath(localDir, "backup.dump")
}

func readSidecar(t *testing.T, path string) recovery.Manifest {
	t.Helper()
	m, err := recovery.ReadManifest(path)
	if err != nil {
		t.Fatalf("reading the sidecar at %s: %v", path, err)
	}
	return m
}

func rewriteSidecar(t *testing.T, path string, m recovery.Manifest) {
	t.Helper()
	data, err := recovery.EncodeManifest(m)
	if err != nil {
		t.Fatalf("encoding the rewritten sidecar: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing the rewritten sidecar: %v", err)
	}
}

// TestRebuildCatalog_AgreeingSidecarIsNotAConflict is the control. Without
// it the conflict test below would pass just as happily against a rebuild
// that called EVERY existing row a conflict, which would be useless in a
// different way.
func TestRebuildCatalog_AgreeingSidecarIsNotAConflict(t *testing.T) {
	svc, artifact, setID, _ := rebuildConflictFixture(t)

	report, err := svc.RebuildCatalog(context.Background(), setID, true)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one for %s", report.Findings, artifact)
	}
	if got := report.Findings[0].Action; got != CatalogRebuildAlreadyPresent {
		t.Errorf("Action = %s, want %s: the sidecar this cycle wrote agrees with the row this cycle wrote", got, CatalogRebuildAlreadyPresent)
	}
}

// TestRebuildCatalog_DisagreeingSidecarIsReportedAndNeverApplied is EPIC
// E's FR-32 rule for the rebuild half: medium and sidecar data are
// untrusted PROPOSALS, so a sidecar that disagrees with an existing
// journal row is reported, never applied over it.
//
// Each of the three facts a rebuild would otherwise have written gets its
// own case, and each asserts BOTH halves: the conflict is reported, and
// the journal row is byte for byte what it was.
func TestRebuildCatalog_DisagreeingSidecarIsReportedAndNeverApplied(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(m *recovery.Manifest)
		mention string
	}{
		{
			name:    "a different checksum",
			mutate:  func(m *recovery.Manifest) { m.Checksum = strings.Repeat("a", 64) },
			mention: "checksum",
		},
		{
			name:    "a different size",
			mutate:  func(m *recovery.Manifest) { m.SizeBytes += 4096 },
			mention: "bytes",
		},
		{
			name:    "a different retention timestamp",
			mutate:  func(m *recovery.Manifest) { m.RetentionTimestamp = m.RetentionTimestamp.Add(-72 * time.Hour) },
			mention: "retention timestamp",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, artifact, setID, manifestPath := rebuildConflictFixture(t)
			ctx := context.Background()

			before, err := svc.Journal.Get(ctx, artifact)
			if err != nil {
				t.Fatalf("precondition: Get: %v", err)
			}

			m := readSidecar(t, manifestPath)
			tc.mutate(&m)
			rewriteSidecar(t, manifestPath, m)

			// Both modes, because FR-32's rule is "dry-run first,
			// reconstruction never deletes, conflicts reported rather than
			// silently resolved", and a conflict that only showed up in
			// the dry run would be exactly the silent resolution it
			// forbids.
			for _, dryRun := range []bool{true, false} {
				report, err := svc.RebuildCatalog(ctx, setID, dryRun)
				if err != nil {
					t.Fatalf("RebuildCatalog(dryRun=%v): %v", dryRun, err)
				}
				if len(report.Findings) != 1 {
					t.Fatalf("dryRun=%v: Findings = %+v, want exactly one", dryRun, report.Findings)
				}
				finding := report.Findings[0]
				if finding.Action != CatalogRebuildConflict {
					t.Fatalf("dryRun=%v: Action = %s, want %s", dryRun, finding.Action, CatalogRebuildConflict)
				}
				if len(finding.Conflicts) == 0 {
					t.Fatalf("dryRun=%v: the conflict names nothing, so an operator is told there is a disagreement and not what it is", dryRun)
				}
				if !strings.Contains(strings.Join(finding.Conflicts, "; "), tc.mention) {
					t.Errorf("dryRun=%v: Conflicts = %v, want one of them to mention %q", dryRun, finding.Conflicts, tc.mention)
				}
			}

			after, err := svc.Journal.Get(ctx, artifact)
			if err != nil {
				t.Fatalf("Get after the rebuild: %v", err)
			}
			if after.LocalHash != before.LocalHash {
				t.Errorf("the rebuild rewrote the journal's local hash: %q -> %q", before.LocalHash, after.LocalHash)
			}
			if !after.DiscoveredAt.Equal(before.DiscoveredAt) {
				t.Errorf("the rebuild rewrote the journal's retention-relevant timestamp: %s -> %s", before.DiscoveredAt, after.DiscoveredAt)
			}
			if after.State != before.State {
				t.Errorf("the rebuild changed the artifact's state: %q -> %q", before.State, after.State)
			}
			if before.Transfer != nil && after.Transfer != nil && before.Transfer.BytesTransferred != after.Transfer.BytesTransferred {
				t.Errorf("the rebuild rewrote the journal's transferred size: %d -> %d",
					before.Transfer.BytesTransferred, after.Transfer.BytesTransferred)
			}
		})
	}
}

// TestCommittedArtifactsSidecarCarriesItsPlacements proves the manifest
// extension actually happens on the real commit path, rather than only in
// a unit test of the encoder.
func TestCommittedArtifactsSidecarCarriesItsPlacements(t *testing.T) {
	_, _, _, manifestPath := rebuildConflictFixture(t)

	m := readSidecar(t, manifestPath)
	if len(m.Placements) != 1 {
		t.Fatalf("the sidecar records %d placements, want 1", len(m.Placements))
	}
	p := m.Placements[0]
	if p.Medium != "local" {
		t.Errorf("Medium = %q, want %q", p.Medium, "local")
	}
	if p.Status != "ACTIVE" {
		t.Errorf("Status = %q, want ACTIVE", p.Status)
	}
	if p.Location == "" {
		t.Error("Location is empty, so the sidecar does not say where the copy is")
	}

	// FR-33's rule applied to a file that, for a medium sidecar, is
	// written into the bucket itself: nothing here may name an endpoint,
	// a bucket, a region or a credential.
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading the sidecar: %v", err)
	}
	for _, banned := range []string{"endpoint", "bucket", "region", "access_key", "secret", "credential"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("the sidecar mentions %q; a medium sidecar is readable by everyone who can read the bucket, and a local one is readable by everyone the backup root is exported to", banned)
		}
	}
}
