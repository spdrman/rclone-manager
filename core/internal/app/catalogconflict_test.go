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
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// FR-32 at the recovery door: a sidecar is a proposal, and a disagreement is
// never resolved in its favour.
//
// The agreeing case is the control and it is doing real work. Without it,
// an implementation that reported every sidecar as a conflict would satisfy
// every other test in this file, and the report would be useless in exactly
// the deployment where nothing is wrong.
//
// The disagreeing case is the one the requirement exists for. The sidecar is
// rewritten on disk so it contradicts the journal, and the assertion is that
// the row is untouched: a rebuild that took the sidecar's hash or timestamp
// would be a path by which a stale or tampered file on the backup root
// rewrites what retention keeps and what verification compares against.
//
// The placement cases cover the newer half. A committed artifact's manifest
// has to carry its placements or a rebuilt catalog forgets where the copies
// are, and a medium placement proposed by a sidecar is reported and not
// adopted, for the same reason a hash is.

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

// rewriteSidecar replaces a manifest the product wrote with one that disagrees
// with the journal, which is how every conflict in this file is planted.
//
// It encodes through recovery.EncodeManifest rather than writing JSON by hand,
// so the planted file is one the reader would genuinely accept. A hand-written
// sidecar that failed to parse would be reported as a manifest error, and the
// test would pass without ever reaching the conflict path it is named after.
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

// TestRebuildCatalog_AMediumPlacementInASidecarIsReportedAndNotAdopted is
// the other half of FR-32's untrusted-proposal rule, and it applies to the
// artifact a rebuild DOES reconstruct rather than to one it leaves alone.
//
// A reconstructed row gets its local placement derived the trusted way,
// from the backup set's own root and the artifact's name. There is no
// equivalent derivation for an object in a bucket: the only source for it
// is the sidecar. Writing an ACTIVE medium placement on that say-so would
// put a copy nobody has verified into the journal, where FR-30's standing
// invariant counts it as one of the artifact's durable copies and a
// medium-aware prune becomes willing to delete an object on the strength
// of it.
//
// Dropping it silently is the other wrong answer and it is the one that
// bites during a real recovery, so the test asserts both halves: the
// journal did not take it, and the operator was told about it.
func TestRebuildCatalog_AMediumPlacementInASidecarIsReportedAndNotAdopted(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup.dump", "payload the sidecar will overclaim about", epoch.Unix())

	cfg := testConfig(t, testSource("production", bs))
	writer := New(cfg, openJournalAt(t, filepath.Join(t.TempDir(), "journal.db")), tr, nil)
	writer.Now = fixedNow(epoch)
	writer.RunCycle(context.Background())

	manifestPath := recovery.ManifestPath(localDir, "backup.dump")
	m := readSidecar(t, manifestPath)
	m.Placements = append(m.Placements, recovery.ManifestPlacement{
		Medium:            "offsite_s3",
		Location:          "backups/production/postgres-primary/backup.dump",
		VerificationClass: "content",
		Status:            "ACTIVE",
	})
	rewriteSidecar(t, manifestPath, m)

	// A fresh journal is the situation this whole command exists for: the
	// operator has the backup root and has lost the database.
	rebuilt := openJournalAt(t, filepath.Join(t.TempDir(), "rebuilt.db"))
	reader := New(cfg, rebuilt, tr, nil)
	reader.Now = fixedNow(epoch)

	report, err := reader.RebuildCatalog(context.Background(), bs.ID, false)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Action != CatalogRebuildReconstructed {
		t.Fatalf("Findings = %+v, want one reconstruction", report.Findings)
	}

	finding := report.Findings[0]
	if len(finding.Notes) != 1 {
		t.Fatalf("Notes = %+v, want exactly one saying the medium copy was not adopted", finding.Notes)
	}
	if !strings.Contains(finding.Notes[0], "offsite_s3") {
		t.Errorf("the note is %q; it has to name the medium, because the operator's next move is to go and look there", finding.Notes[0])
	}
	if finding.ManifestPath != manifestPath {
		t.Errorf("ManifestPath = %q, want %q", finding.ManifestPath, manifestPath)
	}

	artifact, err := model.NewArtifactID(bs.ID, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	rec, err := rebuilt.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("reading the reconstructed row: %v", err)
	}
	for _, p := range rec.Placements {
		if p.Medium != state.MediumLocal {
			t.Fatalf("the rebuilt row carries a %s placement at %q; a placement on a medium can only have come from the sidecar, and a sidecar is a proposal", p.Medium, p.Location)
		}
	}
}
