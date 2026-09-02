package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/recovery"
)

// A sidecar that disagrees with an existing journal row has to reach the
// operator through the API surface too, not only through the CLI.
//
// This is the regression that adding the conflict verdict would otherwise
// have introduced. catalogPass switches on the finding's action, so a
// verdict it does not know about is counted in Scanned and reported
// nowhere. Before conflicts existed, a disagreeing sidecar at least showed
// up in AlreadyPresent; a rebuild that stopped mentioning it at all would
// be strictly worse than the behaviour FR-32 was written to replace.
func TestCatalogPass_ReportsAConflictingSidecarThroughTheServiceSurface(t *testing.T) {
	svc, configPath := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	localDir := filepath.Join(filepath.Dir(configPath), "local")
	manifestPath := recovery.ManifestPath(localDir, "backup.dump")
	m, err := recovery.ReadManifest(manifestPath)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}

	// The control: an untouched sidecar is an ordinary already-present
	// artifact and no failure at all.
	scan, err := svc.ScanCatalog(ctx)
	if err != nil {
		t.Fatalf("ScanCatalog: %v", err)
	}
	if scan.AlreadyPresent == 0 || len(scan.Failures) != 0 {
		t.Fatalf("an untouched sidecar reported AlreadyPresent=%d Failures=%+v, want a clean already-present", scan.AlreadyPresent, scan.Failures)
	}

	m.Checksum = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := recovery.WriteManifest(localDir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	for _, tc := range []struct {
		name string
		run  func() (CatalogReport, error)
	}{
		{"scan", func() (CatalogReport, error) { return svc.ScanCatalog(ctx) }},
		{"rebuild", func() (CatalogReport, error) { return svc.RebuildCatalog(ctx) }},
	} {
		report, err := tc.run()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(report.Failures) != 1 {
			t.Fatalf("%s: Failures = %+v, want the one conflicting sidecar", tc.name, report.Failures)
		}
		f := report.Failures[0]
		if f.Path != manifestPath {
			t.Errorf("%s: Path = %q, want the manifest that disagrees, %q", tc.name, f.Path, manifestPath)
		}
		if !strings.Contains(f.Reason, "checksum") || !strings.Contains(f.Reason, "nothing was changed") {
			t.Errorf("%s: Reason = %q, want it to name the disagreement and say the row was left alone", tc.name, f.Reason)
		}
		if report.AlreadyPresent != 0 {
			t.Errorf("%s: AlreadyPresent = %d; a conflicting sidecar must not read as 'the journal already had this and all is well'", tc.name, report.AlreadyPresent)
		}
		if report.Reconstructed != 0 {
			t.Errorf("%s: Reconstructed = %d, want 0: nothing may be written over an existing row", tc.name, report.Reconstructed)
		}
	}
}
