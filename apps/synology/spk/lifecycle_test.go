package spk

import (
	"strings"
	"testing"
)

// TestLifecycleScripts_DeleteNothingOutsideThePackageFootprint is the
// static half of issue #85's uninstall/retained-data safety criterion.
// The hardware half (docs/acceptance/synology-dsm-package-lifecycle.md
// step 5) puts a canary in the backup share and diffs the share before
// and after an uninstall; that cannot run here. What CAN run here is a
// read of the shipped scripts themselves, because a deletion that is not
// written down in them cannot happen.
//
// DSM already removes `target` on uninstall and upgrade and keeps `etc`,
// `var` and `home` (Synology's package FHS), and a data-share shared
// folder is documented as never removed on uninstall "since it might
// delete the user's personal data as well". None of that helps if a
// lifecycle script deletes something itself, which is what this scans for.
func TestLifecycleScripts_DeleteNothingOutsideThePackageFootprint(t *testing.T) {
	scripts, err := LifecycleScripts()
	if err != nil {
		t.Fatalf("LifecycleScripts: %v", err)
	}
	if len(scripts) != len(LifecycleScriptNames) {
		t.Fatalf("got %d shipped scripts, want %d (%v)",
			len(scripts), len(LifecycleScriptNames), LifecycleScriptNames)
	}

	for name, body := range scripts {
		t.Run(name, func(t *testing.T) {
			if findings := ScanForUnsafeDeletes(name, body); len(findings) != 0 {
				t.Fatalf("shipped script deletes outside the package footprint:\n  %s",
					strings.Join(findings, "\n  "))
			}
		})
	}
}

// TestScanForUnsafeDeletes is the positive control for the scan above: a
// scanner that finds nothing in a clean tree proves nothing until it is
// shown finding something in a dirty one.
func TestScanForUnsafeDeletes(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantFind bool
	}{
		{
			name:     "removing the package's own run directory is fine",
			body:     "#!/bin/sh\nrm -f \"${SYNOPKG_PKGVAR}/run/engine.pid\"\n",
			wantFind: false,
		},
		{
			name:     "removing a staged temp dir under the package is fine",
			body:     "#!/bin/sh\nrm -rf \"${SYNOPKG_PKGDEST}/tmp\"\n",
			wantFind: false,
		},
		{
			name:     "wiping the backup share is caught",
			body:     "#!/bin/sh\nrm -rf /volume1/backup-manager\n",
			wantFind: true,
		},
		{
			name:     "wiping a volume with a glob is caught",
			body:     "#!/bin/sh\nrm -rf /volume*/backup-manager/*\n",
			wantFind: true,
		},
		{
			name:     "find -delete outside the footprint is caught",
			body:     "#!/bin/sh\nfind /volume1/backup-manager -type f -delete\n",
			wantFind: true,
		},
		{
			name:     "rmdir outside the footprint is caught",
			body:     "#!/bin/sh\nrmdir /volume1/backup-manager\n",
			wantFind: true,
		},
		{
			name:     "a deletion hidden behind an unresolvable variable is caught",
			body:     "#!/bin/sh\nrm -rf \"${BACKUP_ROOT}\"\n",
			wantFind: true,
		},
		{
			name:     "a comment mentioning rm -rf is not a deletion",
			body:     "#!/bin/sh\n# deliberately does not rm -rf /volume1/backup-manager\nexit 0\n",
			wantFind: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanForUnsafeDeletes("fixture", tc.body)
			if tc.wantFind && len(got) == 0 {
				t.Fatalf("scanner found nothing in:\n%s", tc.body)
			}
			if !tc.wantFind && len(got) != 0 {
				t.Fatalf("scanner flagged a safe script: %v\n%s", got, tc.body)
			}
		})
	}
}

// TestLifecycleScripts_KeepStateOutsideTarget pins the one decision the
// update criterion rests on: DSM replaces `target` wholesale on upgrade,
// so anything the package must keep has to live under `var` or `etc`.
func TestLifecycleScripts_KeepStateOutsideTarget(t *testing.T) {
	scripts, err := LifecycleScripts()
	if err != nil {
		t.Fatalf("LifecycleScripts: %v", err)
	}
	shared, err := SharedScript()
	if err != nil {
		t.Fatalf("SharedScript: %v", err)
	}
	// postinst sources the shared library, so the paths it acts on are
	// only visible in the two files together. Reading the stage alone
	// would make this test pass or fail on where a constant happens to be
	// written down rather than on where state actually lands.
	postinst := shared + scripts["postinst"]

	// Synology documents SYNOPKG_PKGDEST (the target directory) but does
	// NOT document an environment variable for the var/etc directories, so
	// the scripts spell out the documented FHS paths instead of relying on
	// an undocumented SYNOPKG_PKGVAR/SYNOPKG_PKGETC that may or may not be
	// exported on a given DSM build.
	for _, want := range []string{PkgVarPath, PkgEtcPath} {
		if !strings.Contains(postinst, want) {
			t.Fatalf("postinst never mentions %s, so nothing it creates survives an upgrade:\n%s", want, postinst)
		}
	}
	// The control: state written into the target directory would be
	// destroyed by the next upgrade, so postinst must not put any there.
	for _, forbidden := range []string{"SYNOPKG_PKGDEST}/state", "SYNOPKG_PKGDEST}/var", "SYNOPKG_PKGDEST}/etc"} {
		if strings.Contains(postinst, forbidden) {
			t.Fatalf("postinst writes state under the target directory (%s), which DSM replaces on every upgrade", forbidden)
		}
	}
}
