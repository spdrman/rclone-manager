// These cover ScanManifests, and between them they pin the one property a
// catalog rebuild depends on: a scan is a survey, not a transaction.
//
// The four cases are the four ways a backup directory can disappoint a
// rebuild. It can not exist yet, it can exist and be empty, it can be full
// of files that are not manifests, and it can contain a manifest that is
// broken. Only the last of those is an error, and even then only for the
// one file: the assertions below are written to fail if a future change
// makes any of the other three loud, or makes the fourth one fatal.
package recovery

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanManifests_MissingDir_ReturnsEmptyNotError pins the adoption
// case from EPIC-B section 71 Work Package 3.3: an operator points this
// manager at a backup root it has never written to, and the scan has to
// answer "no manifests" rather than "I could not look". A missing
// directory reported as an error here would make adopting a fresh root
// indistinguishable from a permissions problem on an existing one.
func TestScanManifests_MissingDir_ReturnsEmptyNotError(t *testing.T) {
	manifests, errs := ScanManifests(filepath.Join(t.TempDir(), "never-created"))
	if len(manifests) != 0 || len(errs) != 0 {
		t.Fatalf("ScanManifests(missing dir) = %+v, %+v, want both empty", manifests, errs)
	}
}

// TestScanManifests_EmptyDir_ReturnsEmpty is the positive control for the
// case above. Without it, ScanManifests could return the empty result for
// every input at all and the missing-directory test would still pass, so
// this pins that the empty answer is reached by looking and finding
// nothing rather than by never looking.
func TestScanManifests_EmptyDir_ReturnsEmpty(t *testing.T) {
	manifests, errs := ScanManifests(t.TempDir())
	if len(manifests) != 0 || len(errs) != 0 {
		t.Fatalf("ScanManifests(empty dir) = %+v, %+v, want both empty", manifests, errs)
	}
}

// TestScanManifests_ReadsManifestsAndIgnoresOtherFiles pins what a
// manifest is recognised BY, which is the suffix and nothing else.
//
// The two decoys are the two files that will always be in that directory:
// the artifact itself, and a .partial left by an interrupted transfer. A
// scan that tried to read either of them would turn a perfectly normal
// backup directory into a directory full of scan errors, and an operator
// reading that list during a rebuild has no way to tell the noise from the
// one line that matters.
//
// The sort order is asserted rather than the set, because the result feeds
// a rebuild whose output an operator compares between runs; leaving it at
// the mercy of directory-listing order would make two identical rebuilds
// look different.
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

// TestScanManifests_CollectsErrorsWithoutAbortingOthers is the one that
// matters most, and it is deliberately arranged so the broken file sorts
// BEFORE the good one: "bad.dump" precedes "good.dump", so an
// implementation that returned on first error would find the corruption
// first and report zero recovered artifacts.
//
// That is the failure this exists to stop. The journal is already lost by
// the time anything calls this, so one truncated sidecar hiding every
// other artifact's recovery metadata turns a partial loss into a total
// one.
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
