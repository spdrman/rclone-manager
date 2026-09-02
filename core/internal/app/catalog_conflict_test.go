package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/recovery"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// runCycleAndReadBack runs one real cycle so the sidecar on disk is the
// one commit.go actually writes, and hands back the journal, the config
// and the artifact it produced.
func runCycleAndReadBack(t *testing.T, localDir string) (*state.Journal, *Service, model.ArtifactID, state.Record) {
	t.Helper()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""
	tr := newFakeTransport()
	tr.put("backup.dump", "conflict payload", epoch.Unix())
	cfg := testConfig(t, testSource("production", bs))

	journal := openJournal(t)
	svc := New(cfg, journal, tr, nil)
	svc.Now = fixedNow(epoch)
	ctx := context.Background()
	svc.RunCycle(ctx)

	artifact, err := model.NewArtifactID(bs.ID, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return journal, svc, artifact, rec
}

// FR-32: a sidecar is an untrusted proposal, so one that disagrees with an
// existing journal row is REPORTED, and the row is left exactly as it was.
// Before this the disagreement was invisible: the rebuild saw a row, said
// "already present", and moved on, which is silently resolving a conflict
// in favour of whichever record happened to be in the database.
func TestRebuildCatalog_ReportsASidecarThatDisagreesWithTheJournalRow(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	journal, svc, artifact, before := runCycleAndReadBack(t, localDir)

	// Rewrite the sidecar the way a manifest copied off another machine,
	// or one written before the artifact was re-transferred, would look.
	m, err := recovery.ReadManifest(recovery.ManifestPath(localDir, artifact.Name))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	m.Checksum = "0000000000000000000000000000000000000000000000000000000000000000"
	m.RemotePath = "somewhere/else/backup.dump"
	m.SizeBytes = before.Transfer.BytesTransferred + 512
	if err := recovery.WriteManifest(localDir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	for _, dryRun := range []bool{true, false} {
		report, err := svc.RebuildCatalog(ctx, artifact.Set, dryRun)
		if err != nil {
			t.Fatalf("RebuildCatalog(dryRun=%v): %v", dryRun, err)
		}
		if len(report.Findings) != 1 {
			t.Fatalf("dryRun=%v: %d findings, want 1: %+v", dryRun, len(report.Findings), report.Findings)
		}
		f := report.Findings[0]
		if f.Action != CatalogRebuildConflict {
			t.Fatalf("dryRun=%v: action %s, want %s", dryRun, f.Action, CatalogRebuildConflict)
		}
		joined := strings.Join(f.Conflicts, "\n")
		for _, want := range []string{"checksum", "remote path", "size"} {
			if !strings.Contains(joined, want) {
				t.Errorf("dryRun=%v: conflicts do not mention %q:\n%s", dryRun, want, joined)
			}
		}

		// And the row is untouched, in both modes. A conflict changes what
		// is reported, never what is written.
		after, err := journal.Get(ctx, artifact)
		if err != nil {
			t.Fatalf("Get after rebuild: %v", err)
		}
		if after.LocalHash != before.LocalHash {
			t.Errorf("dryRun=%v: the journal's hash changed from %q to %q; a sidecar must never be applied over an existing row",
				dryRun, before.LocalHash, after.LocalHash)
		}
		if after.RemotePath != before.RemotePath {
			t.Errorf("dryRun=%v: the journal's remote path changed from %q to %q", dryRun, before.RemotePath, after.RemotePath)
		}
		if after.UpdatedAt != before.UpdatedAt {
			t.Errorf("dryRun=%v: the journal row was updated at %s, was %s", dryRun, after.UpdatedAt, before.UpdatedAt)
		}
	}
}

// The negative control for the test above: the sidecar the pipeline itself
// wrote agrees with the row the pipeline itself wrote, so a rebuild over
// an intact journal reports no conflict at all. Without this, a conflict
// detector that fired on everything would pass the test above and make
// `catalog rebuild` useless on a healthy deployment.
func TestRebuildCatalog_AnUntouchedSidecarIsNotAConflict(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	_, svc, artifact, _ := runCycleAndReadBack(t, localDir)

	report, err := svc.RebuildCatalog(ctx, artifact.Set, false)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Action != CatalogRebuildAlreadyPresent {
		t.Fatalf("Findings = %+v, want exactly one CatalogRebuildAlreadyPresent", report.Findings)
	}
}

// A sidecar naming a copy on a medium the journal has never heard of is a
// conflict too, and it is the one FR-32's rebuild-from-a-medium story
// turns on: an operator who has lost their journal needs to be told the
// sidecar says there is a copy in a bucket somewhere, not to have that
// quietly dropped because the row already existed.
func TestRebuildCatalog_ReportsAPlacementTheJournalDoesNotKnowAbout(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	_, svc, artifact, _ := runCycleAndReadBack(t, localDir)

	m, err := recovery.ReadManifest(recovery.ManifestPath(localDir, artifact.Name))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if len(m.Placements) == 0 {
		t.Fatal("the sidecar the pipeline wrote carries no placements at all")
	}
	m.Placements = append(m.Placements, recovery.ManifestPlacement{
		Medium:   "cold_s3",
		Location: "backups/production/testset/backup.dump",
		Status:   recovery.PlacementActive,
	})
	if err := recovery.WriteManifest(localDir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	report, err := svc.RebuildCatalog(ctx, artifact.Set, true)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Action != CatalogRebuildConflict {
		t.Fatalf("Findings = %+v, want exactly one CatalogRebuildConflict", report.Findings)
	}
	joined := strings.Join(report.Findings[0].Conflicts, "\n")
	if !strings.Contains(joined, "cold_s3") {
		t.Errorf("the conflict does not name the medium the sidecar claims a copy on:\n%s", joined)
	}
}

// The sidecar the real pipeline writes has to carry the artifact's
// placements, or a rebuild from one can only ever propose that an artifact
// existed and never where its bytes are.
func TestCommitWritesTheArtifactsPlacementsIntoTheSidecar(t *testing.T) {
	localDir := t.TempDir()
	_, _, artifact, rec := runCycleAndReadBack(t, localDir)

	m, err := recovery.ReadManifest(recovery.ManifestPath(localDir, artifact.Name))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if len(m.Placements) != len(rec.Placements) {
		t.Fatalf("the sidecar carries %d placements, the journal has %d", len(m.Placements), len(rec.Placements))
	}
	local, ok := rec.LocalPlacement()
	if !ok {
		t.Fatal("the journal row has no local placement")
	}
	if m.Placements[0].Medium != local.Medium || m.Placements[0].Location != local.Location ||
		m.Placements[0].Status != local.Status || m.Placements[0].Checksum != local.Hash {
		t.Errorf("the sidecar's placement %+v does not match the journal's %+v", m.Placements[0], local)
	}
}

// The recovery case this whole sidecar extension exists for: an operator
// has lost their journal, and the sidecar next to the artifact says there
// is also a copy in a bucket. That copy cannot be adopted (nothing here
// can check it is there, and an unverified ACTIVE placement would later be
// enough for a medium-aware prune to delete an object), and it must not be
// silently dropped either, because the person whose journal is gone is
// precisely the person who needs to be told.
func TestRebuildCatalog_ReportsAMediumCopyItCannotAdopt(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	_, svc, artifact, _ := runCycleAndReadBack(t, localDir)

	m, err := recovery.ReadManifest(recovery.ManifestPath(localDir, artifact.Name))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	m.Placements = append(m.Placements, recovery.ManifestPlacement{
		Medium:   "cold_s3",
		Location: "backups/production/testset/backup.dump",
		Status:   recovery.PlacementActive,
	})
	if err := recovery.WriteManifest(localDir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	// A brand new journal, so this is a real reconstruction rather than a
	// conflict against a row that already exists.
	fresh := New(svc.Config, openJournal(t), newFakeTransport(), nil)
	report, err := fresh.RebuildCatalog(ctx, artifact.Set, false)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Action != CatalogRebuildReconstructed {
		t.Fatalf("Findings = %+v, want exactly one CatalogRebuildReconstructed", report.Findings)
	}
	joined := strings.Join(report.Findings[0].Notes, "\n")
	if !strings.Contains(joined, "cold_s3") || !strings.Contains(joined, "not written") {
		t.Errorf("notes = %q, want the medium copy reported as read but not written", joined)
	}

	// And the reconstructed row carries its LOCAL placement, derived the
	// trusted way, and only that one.
	rec, err := fresh.Journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get after rebuild: %v", err)
	}
	if len(rec.Placements) != 1 {
		t.Fatalf("the rebuilt row has %d placements, want only the local one it could derive: %+v", len(rec.Placements), rec.Placements)
	}
	local, ok := rec.LocalPlacement()
	if !ok {
		t.Fatal("a rebuilt row has no local placement, so a code path can observe an artifact with none")
	}
	if local.Location != filepath.Join(localDir, artifact.Name) {
		t.Errorf("the rebuilt local placement is at %q, want the path derived from the backup set root, %q",
			local.Location, filepath.Join(localDir, artifact.Name))
	}
	if local.VerificationClass != "" {
		t.Errorf("the rebuilt placement claims verification class %q; nothing read the bytes", local.VerificationClass)
	}
}
