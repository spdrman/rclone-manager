// What a built package must be, with determinism at the centre.
//
// The determinism test is the load-bearing one, because the whole "this
// package carries the exact release binary" claim depends on somebody else
// being able to build it again and get the same bytes. It fails on a
// timestamp, on map iteration order leaking into an archive, on anything
// that reads the clock.
//
// The refusal cases are the other half, and the UI-bundle one is worth
// singling out. A missing bundle is refused rather than defaulted, because
// the alternative produces a package that installs, runs and shows the
// wrong provider's interface, and nothing about the finished artifact
// would say so.
//
// The start/stop/status test runs the shipped script rather than reading
// it: what matters is that it serves the bundle the package carries, and
// only executing it can show that.
package spk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuild_NamesTheArtifactTheWayTheToolkitDoes pins the filename
// pkg_make_spk produces: "<package>-<arch>-<version>.spk".
func TestBuild_NamesTheArtifactTheWayTheToolkitDoes(t *testing.T) {
	for _, tc := range []struct{ goarch, want string }{
		{"amd64", "BackupManager-x86_64-1.0.0-1.spk"},
		{"arm64", "BackupManager-armv8-1.0.0-1.spk"},
	} {
		t.Run(tc.goarch, func(t *testing.T) {
			path, _ := buildFixture(t, tc.goarch)
			if got := filepath.Base(path); got != tc.want {
				t.Fatalf("built %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuild_IsDeterministic: two builds of the same inputs must be
// byte-identical, or "the SPK carries the release digest" is a claim
// nobody downstream can re-check.
func TestBuild_IsDeterministic(t *testing.T) {
	bins := stagedBinaries(t, "amd64", "release")
	bundle := stagedUIBundle(t, UIBundlePlatform)
	build := func() []byte {
		path, err := Build(BuildOptions{
			GOARCH: "amd64", Version: "1.0.0-1", BinariesDir: bins, UIBundleDir: bundle, OutDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return body
	}
	if a, b := build(), build(); string(a) != string(b) {
		t.Fatalf("two builds of identical inputs differ (%d vs %d bytes)", len(a), len(b))
	}
}

// TestBuild_RefusesIncompleteInput is the control on Build's own inputs:
// silently producing a package with a binary missing would defeat every
// check Verify makes afterwards, because there would be nothing to hash.
func TestBuild_RefusesIncompleteInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, opts *BuildOptions)
		wantErr string
	}{
		{
			name:    "an architecture the release does not build",
			mutate:  func(_ *testing.T, o *BuildOptions) { o.GOARCH = "386" },
			wantErr: "386",
		},
		{
			name:    "no version",
			mutate:  func(_ *testing.T, o *BuildOptions) { o.Version = "" },
			wantErr: "version",
		},
		{
			name: "one of the core binaries is missing",
			mutate: func(t *testing.T, o *BuildOptions) {
				if err := os.Remove(filepath.Join(o.BinariesDir, "backup-manager-web")); err != nil {
					t.Fatalf("remove: %v", err)
				}
			},
			wantErr: "backup-manager-web",
		},
		{
			name: "a staged binary is not an executable at all",
			mutate: func(t *testing.T, o *BuildOptions) {
				p := filepath.Join(o.BinariesDir, "backup-manager")
				if err := os.WriteFile(p, []byte("#!/bin/sh\necho nope\n"), 0o755); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			wantErr: "ELF",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := BuildOptions{
				GOARCH:      "amd64",
				Version:     "1.0.0-1",
				BinariesDir: stagedBinaries(t, "amd64", "release"),
				OutDir:      t.TempDir(),
			}
			tc.mutate(t, &opts)
			_, err := Build(opts)
			if err == nil {
				t.Fatalf("Build accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestBuild_PayloadCarriesTheDSMLauncher checks the one thing acceptance
// criterion "DSM desktop launcher opens the shared Web UI" needs from the
// build: the dsmuidir INFO declares actually exists in package.tgz, with
// a parseable launcher config in it. Whether DSM then draws an icon is a
// hardware question (docs/acceptance/synology-dsm-package-lifecycle.md
// step 3), but a package whose dsmuidir points at nothing cannot pass it.
func TestBuild_PayloadCarriesTheDSMLauncher(t *testing.T) {
	path, manifest := buildFixture(t, "amd64")
	rep, err := Verify(path, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requirePass(t, rep, CheckLauncher)

	// Control: strip the launcher config out and the check must notice,
	// rather than treating "dsmuidir declared" as sufficient.
	broken := mutateInnerPayload(t, path, func(inner []tarEntry) []tarEntry {
		return dropEntry(inner, DSMUIDir+"/config")
	})
	rep, err = Verify(broken, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requireFail(t, rep, CheckLauncher, "config")
}

// TestBuild_RequiresTheProvidersOwnUIBundle is issue #180's control on
// this side, and every row of it is a package that would install cleanly
// and show the wrong user interface.
//
// The .spk carries the release binary unchanged, so the UI bridge cannot
// be chosen at compile time (§3.7), which is why the bundle is a build
// input at all. What makes it a REQUIRED input is that the alternative
// failure is silent: a package built without one starts, serves, and
// tells a Synology operator they are running a generic Docker deployment.
func TestBuild_RequiresTheProvidersOwnUIBundle(t *testing.T) {
	bins := stagedBinaries(t, "amd64", "release")

	build := func(t *testing.T, bundleDir string) (string, error) {
		t.Helper()
		return Build(BuildOptions{
			GOARCH: "amd64", Version: "1.0.0-1",
			BinariesDir: bins, UIBundleDir: bundleDir, OutDir: t.TempDir(),
		})
	}

	// Positive control first: the correct bundle builds, and the payload
	// really carries it. Without this the refusals below would pass just
	// as happily against a Build that refused everything.
	path, err := build(t, stagedUIBundle(t, UIBundlePlatform))
	if err != nil {
		t.Fatalf("Build with this provider's own bundle: %v", err)
	}
	carried := map[string]bool{}
	mutateInnerPayload(t, path, func(inner []tarEntry) []tarEntry {
		for _, e := range inner {
			carried[e.hdr.Name] = true
		}
		return inner
	})
	for _, want := range []string{
		PayloadUIBundleDir + "/index.html",
		PayloadUIBundleDir + "/" + UIBundleMarkerName,
		PayloadUIBundleDir + "/assets/app.js",
	} {
		if !carried[want] {
			t.Errorf("the package does not carry %s; serve-ui --ui-dir would have nothing to serve", want)
		}
	}

	for _, tc := range []struct {
		name    string
		bundle  func(t *testing.T) string
		wantMsg string
	}{
		{
			name:    "no bundle at all",
			bundle:  func(*testing.T) string { return "" },
			wantMsg: "#180",
		},
		{
			name: "a bundle built for another provider",
			bundle: func(t *testing.T) string {
				return stagedUIBundle(t, "truenas")
			},
			wantMsg: "built for \"truenas\"",
		},
		{
			name: "a directory with no marker, so nothing says what it is",
			bundle: func(t *testing.T) string {
				dir := stagedUIBundle(t, UIBundlePlatform)
				if err := os.Remove(filepath.Join(dir, UIBundleMarkerName)); err != nil {
					t.Fatalf("remove marker: %v", err)
				}
				return dir
			},
			wantMsg: UIBundleMarkerName,
		},
		{
			name: "a marker but no app shell, which is what an unmounted bundle looks like",
			bundle: func(t *testing.T) string {
				dir := stagedUIBundle(t, UIBundlePlatform)
				if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
					t.Fatalf("remove index.html: %v", err)
				}
				return dir
			},
			wantMsg: "index.html",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := build(t, tc.bundle(t))
			if err == nil {
				t.Fatalf("Build accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Build refused %s with %q, which never mentions %q, so the refusal does not say what is wrong", tc.name, err, tc.wantMsg)
			}
		})
	}
}

// TestStartStopStatusServesThePackagedBundle pins the other half. Build
// can carry the bundle perfectly and the package still serve the generic
// bridge, because what decides that is one flag in a shell script DSM
// runs.
func TestStartStopStatusServesThePackagedBundle(t *testing.T) {
	scripts, err := LifecycleScripts()
	if err != nil {
		t.Fatalf("LifecycleScripts: %v", err)
	}
	body := scripts["start-stop-status"]
	shared, err := SharedScript()
	if err != nil {
		t.Fatalf("SharedScript: %v", err)
	}

	if !strings.Contains(shared, `UI_BUNDLE_DIR="${SYNOPKG_PKGDEST}/`+PayloadUIBundleDir+`"`) {
		t.Errorf("common.sh does not derive the bundle directory from the documented package target and %s", PayloadUIBundleDir)
	}
	if !strings.Contains(shared, `RUNTIME_PROFILE="`+UIBundlePlatform+`"`) {
		t.Errorf("common.sh does not select the %s runtime profile", UIBundlePlatform)
	}
	if !strings.Contains(body, `--ui-dir "${UI_BUNDLE_DIR}"`) {
		t.Errorf("start-stop-status does not point serve-ui at the packaged bundle, so an installed package serves the bridge compiled into the binary, which is the generic one (#180):\n%s", body)
	}
	// Both processes, not just the UI: the engine's profile is what
	// GET /api/v1/system/capabilities reports the platform as.
	if got := strings.Count(body, `--profile="${RUNTIME_PROFILE}"`); got != 2 {
		t.Errorf("start-stop-status names the runtime profile %d time(s), want 2 (the engine and the UI host)", got)
	}
}
