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
	build := func() []byte {
		path, err := Build(BuildOptions{
			GOARCH: "amd64", Version: "1.0.0-1", BinariesDir: bins, OutDir: t.TempDir(),
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
		return dropEntry(inner, PayloadRoot+"/"+DSMUIDir+"/config")
	})
	rep, err = Verify(broken, manifest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	requireFail(t, rep, CheckLauncher, "config")
}
