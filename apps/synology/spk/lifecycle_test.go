package spk

import (
	"strings"
	"testing"
)

// The shell scanner, tested against the scripts that ship and against
// invented ones.
//
// Both are needed and neither substitutes for the other. Running it over
// the real scripts proves what ships is safe today; running it over
// deliberately unsafe snippets proves the scanner can say no at all, which
// is the assertion that stops it decaying into a function that returns an
// empty list.
//
// The determinism case exists because this scanner resolves variables by
// walking assignments, and a resolution that depended on map ordering
// would produce a check that passes on one run and fails on the next,
// which is worse than a check that always fails.
//
// The common.sh case reads the shared file out of the archive rather than
// from disk. A scanner that checked the working tree's copy would approve
// a package whose embedded copy differs, and the embedded copy is the one
// that runs as root on somebody's NAS.

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
//
// Every "caught" case below is a shape that a destructive-verb denylist
// let through, which is why the scan is an allowlist of commands a
// shipped lifecycle script may run instead.
func TestScanForUnsafeDeletes(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantFind bool
	}{
		{
			name:     "removing the package's own pid file is fine",
			body:     "#!/bin/sh\nrm -f \"${ENGINE_PID}\"\n",
			wantFind: false,
		},
		{
			name:     "removing under the documented var/log path is fine",
			body:     "#!/bin/sh\nrm -f \"/var/packages/${SYNOPKG_PKGNAME}/var/log/engine.log.1\"\n",
			wantFind: false,
		},
		{
			name:     "a fail-fast reference to the target directory is fine",
			body:     "#!/bin/sh\nrm -rf \"${SYNOPKG_PKGDEST:?unset}/tmp\"\n",
			wantFind: false,
		},
		{
			name:     "the same target directory without the fail-fast form is caught",
			body:     "#!/bin/sh\nrm -rf \"${SYNOPKG_PKGDEST}/tmp\"\n",
			wantFind: true,
		},
		{
			name:     "the undocumented PKGVAR variable is caught even under run",
			body:     "#!/bin/sh\nrm -rf \"${SYNOPKG_PKGVAR}/run\"\n",
			wantFind: true,
		},
		{
			name:     "the undocumented PKGVAR variable is caught over state",
			body:     "#!/bin/sh\nrm -rf \"${SYNOPKG_PKGVAR}/state\"\n",
			wantFind: true,
		},
		{
			name:     "the undocumented PKGETC variable is caught over the config",
			body:     "#!/bin/sh\nrm -f \"${SYNOPKG_PKGETC}/config.yaml\"\n",
			wantFind: true,
		},
		{
			name:     "deleting the state directory by its documented path is caught",
			body:     "#!/bin/sh\nrm -rf \"${STATE_DIR}\"\n",
			wantFind: true,
		},
		{
			name:     "deleting the configuration by its documented path is caught",
			body:     "#!/bin/sh\nrm -f \"${CONFIG_FILE}\"\n",
			wantFind: true,
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
			name:     "find piped into xargs rm is caught",
			body:     "#!/bin/sh\nfind /volume1/backup-manager -type f | xargs rm -f\n",
			wantFind: true,
		},
		{
			name:     "find -delete outside the footprint is caught",
			body:     "#!/bin/sh\nfind /volume1/backup-manager -type f -delete\n",
			wantFind: true,
		},
		{
			name:     "a deletion behind sh -c is caught",
			body:     "#!/bin/sh\nsh -c 'rm -rf /volume1/backup-manager'\n",
			wantFind: true,
		},
		{
			name:     "a truncating redirection is caught",
			body:     "#!/bin/sh\n: > /volume1/backup-manager/index.db\n",
			wantFind: true,
		},
		{
			name:     "cat /dev/null into a file is caught",
			body:     "#!/bin/sh\ncat /dev/null > /volume1/backup-manager/index.db\n",
			wantFind: true,
		},
		{
			name:     "cp /dev/null over a file is caught",
			body:     "#!/bin/sh\ncp /dev/null /volume1/backup-manager/index.db\n",
			wantFind: true,
		},
		{
			name:     "truncate is caught",
			body:     "#!/bin/sh\ntruncate -s 0 /volume1/backup-manager/index.db\n",
			wantFind: true,
		},
		{
			name:     "dd over a file is caught",
			body:     "#!/bin/sh\ndd if=/dev/zero of=/volume1/backup-manager/index.db\n",
			wantFind: true,
		},
		{
			name:     "moving the backup share away is caught",
			body:     "#!/bin/sh\nmv /volume1/backup-manager /tmp/\n",
			wantFind: true,
		},
		{
			name:     "removing the DSM share with synoshare is caught",
			body:     "#!/bin/sh\nsynoshare --del backup-manager\n",
			wantFind: true,
		},
		{
			name:     "a backgrounded deletion after another command is caught",
			body:     "#!/bin/sh\nsleep 1 & rm -rf /volume1/backup-manager &\n",
			wantFind: true,
		},
		{
			name:     "a recursive chown of the share is caught",
			body:     "#!/bin/sh\nchown -R nobody /volume1/backup-manager\n",
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
		{
			name:     "appending to a log is not a truncation",
			body:     "#!/bin/sh\necho hello >> \"${ENGINE_LOG}\"\n",
			wantFind: false,
		},
		{
			name:     "seeding the config from the payload is not a deletion",
			body:     "#!/bin/sh\ncp \"${SYNOPKG_PKGDEST}/share/config.yaml.seed\" \"${CONFIG_FILE}\"\n",
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

// TestScanForUnsafeDeletes_ResolutionIsDeterministic pins the step that
// decides whether a deletion is inside the footprint.
//
// expandShellVars used to iterate the variable map and substitute $NAME
// as a plain string replacement, so with one name a prefix of another
// the resolved path depended on which key Go's randomised map iteration
// reached first. start-stop-status assigns both _pid and _pidfile, so
// the colliding pair is already in the map. A safety gate whose verdict
// changes between runs is worse than one that is consistently wrong,
// because it passes in CI and may not elsewhere.
func TestScanForUnsafeDeletes_ResolutionIsDeterministic(t *testing.T) {
	vars := map[string]string{
		"_pid":     "/volume1/backup-manager/short",
		"_pidfile": "/volume1/backup-manager/long",
	}
	// Both spellings, both names: a resolver that expanded nothing at
	// all would also be deterministic, so each case pins a value.
	for _, tc := range []struct{ in, want string }{
		{"${_pidfile}", "/volume1/backup-manager/long"},
		{"$_pidfile", "/volume1/backup-manager/long"},
		{"${_pid}", "/volume1/backup-manager/short"},
		{"$_pid", "/volume1/backup-manager/short"},
	} {
		for range 200 {
			if got := expandShellVars(tc.in, vars); got != tc.want {
				t.Fatalf("expandShellVars(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	}
}

// TestScanShippedScript_ReadsTheArchivesOwnCommonSh is the control for
// verify.go's claim that the scan reads the scripts out of the built
// archive "so a build that packaged something else is caught".
//
// common.sh defines every path the other scripts delete, so resolving a
// stage against the pristine embedded copy while verifying a package
// that shipped a different one reports the substitution as clean, which
// is the one case the sentence exists to cover.
func TestScanShippedScript_ReadsTheArchivesOwnCommonSh(t *testing.T) {
	stage := "#!/bin/sh\nrm -rf \"${RUN_DIR}\"\n"

	pristine, err := SharedScript()
	if err != nil {
		t.Fatalf("SharedScript: %v", err)
	}
	// The baseline: against the shipped common.sh, RUN_DIR is under the
	// package's own var directory and the deletion is safe.
	if findings := ScanShippedScript("scripts/stage", stage, pristine); len(findings) != 0 {
		t.Fatalf("the shipped common.sh should resolve RUN_DIR into the footprint, got %v", findings)
	}

	substituted := strings.Replace(pristine,
		`RUN_DIR="${PKG_VAR}/run"`,
		`RUN_DIR="/volume1/backup-manager"`, 1)
	if substituted == pristine {
		t.Fatal("could not substitute RUN_DIR in common.sh, so this test proves nothing")
	}
	if findings := ScanShippedScript("scripts/stage", stage, substituted); len(findings) == 0 {
		t.Fatal("a package whose common.sh points RUN_DIR at a share was reported clean")
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
