// This file holds the boundary's whole share of the catalog rebuild: one
// test, about the one thing this layer decides for itself.
//
// internal/app does the recovering and the disagreeing, and it has its
// own suite for that. What is only decidable here is the translation:
// catalogPass switches over the verdicts it is handed, so a verdict this
// build has not heard of is counted and then never mentioned. That gap is
// invisible from either side on its own, which is why the coverage sits
// at the seam rather than with the engine.
package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/recovery"
)

// TestCatalogPass_ReportsAConflictingSidecarThroughTheServiceSurface holds
// the API half of FR-32's rebuild rule.
//
// The rule is that a sidecar disagreeing with an existing journal row is
// reported and never applied, and internal/app does the reporting. What
// this covers is the step after: catalogPass switches on a finding's
// action, so a verdict it has never heard of is counted in Scanned and
// then mentioned nowhere. That would be strictly worse than the behaviour
// the conflict verdict replaced, because before conflicts existed a
// disagreeing sidecar at least showed up in AlreadyPresent.
//
// It runs both routes, the dry-run scan and the real rebuild, because the
// two share a code path and the whole value of a dry run is that it
// predicts what the real one does.
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

	// The control. Without it, everything below would pass just as
	// happily against a pass that called every existing row a conflict.
	scan, err := svc.ScanCatalog(ctx)
	if err != nil {
		t.Fatalf("ScanCatalog: %v", err)
	}
	if scan.AlreadyPresent == 0 || len(scan.Failures) != 0 {
		t.Fatalf("an untouched sidecar reported AlreadyPresent=%d Failures=%+v; it should be an ordinary already-present artifact and no failure at all", scan.AlreadyPresent, scan.Failures)
	}

	m.Checksum = strings.Repeat("0", 64)
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
			t.Errorf("%s: Path = %q, want the sidecar that disagrees, %q", tc.name, f.Path, manifestPath)
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
