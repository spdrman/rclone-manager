package recovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanManifests_MissingDir_ReturnsEmptyNotError(t *testing.T) {
	manifests, errs := ScanManifests(filepath.Join(t.TempDir(), "never-created"))
	if len(manifests) != 0 || len(errs) != 0 {
		t.Fatalf("ScanManifests(missing dir) = %+v, %+v, want both empty", manifests, errs)
	}
}

func TestScanManifests_EmptyDir_ReturnsEmpty(t *testing.T) {
	manifests, errs := ScanManifests(t.TempDir())
	if len(manifests) != 0 || len(errs) != 0 {
		t.Fatalf("ScanManifests(empty dir) = %+v, %+v, want both empty", manifests, errs)
	}
}

func TestScanManifests_ReadsManifestsAndIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	m1 := testManifest()
	m1.ArtifactName = "a.dump"
	m2 := testManifest()
	m2.ArtifactName = "b.dump"
	if err := WriteManifest(dir, m1); err != nil {
		t.Fatalf("WriteManifest a: %v", err)
	}
	if err := WriteManifest(dir, m2); err != nil {
		t.Fatalf("WriteManifest b: %v", err)
	}

	// The real artifact data file, and a stray .partial, must never be
	// mistaken for a manifest.
	if err := os.WriteFile(filepath.Join(dir, "a.dump"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.dump.partial"), []byte("still transferring"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	manifests, errs := ScanManifests(dir)
	if len(errs) != 0 {
		t.Fatalf("errs = %+v, want none", errs)
	}
	if len(manifests) != 2 {
		t.Fatalf("len(manifests) = %d, want 2 (got %+v)", len(manifests), manifests)
	}
	if manifests[0].ArtifactName != "a.dump" || manifests[1].ArtifactName != "b.dump" {
		t.Errorf("manifests = %+v, want a.dump then b.dump (sorted)", manifests)
	}
}

func TestScanManifests_CollectsErrorsWithoutAbortingOthers(t *testing.T) {
	dir := t.TempDir()
	good := testManifest()
	good.ArtifactName = "good.dump"
	if err := WriteManifest(dir, good); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	badPath := ManifestPath(dir, "bad.dump")
	if err := os.WriteFile(badPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	manifests, errs := ScanManifests(dir)
	if len(manifests) != 1 || manifests[0].ArtifactName != "good.dump" {
		t.Fatalf("manifests = %+v, want exactly good.dump", manifests)
	}
	if len(errs) != 1 || errs[0].Path != badPath {
		t.Fatalf("errs = %+v, want exactly one error for %s", errs, badPath)
	}
}
