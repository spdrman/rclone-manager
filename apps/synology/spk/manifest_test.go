// Loading the release manifest, including the real one in this
// repository.
//
// That second test is the unusual one and it is deliberate. A parser
// exercised only against fixtures written by the same person who wrote the
// parser agrees with itself perfectly and can still be unable to read the
// file that actually exists; pointing it at the checked-in manifest is
// what catches a schema that moved.
//
// The malformed cases assert refusals rather than best-effort reads,
// because every value in this file is a digest somebody later compares
// bytes against, and a digest that was silently truncated or defaulted
// turns a parity check into a check that cannot fail.
package spk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadReleaseManifest reads the shape 4.1's
// scripts/release/record-release-hashes.sh actually writes, since that
// file is the only source of the per-architecture SHA-256 §3.7 requires
// the SPK to carry.
func TestLoadReleaseManifest(t *testing.T) {
	const good = `{
  "version": "c51a07f",
  "commit": "c51a07f0e377b69d43260186c0c73764e1d65f6b",
  "architectures": [
    {
      "architecture": "amd64",
      "binary_sha256": {
        "backup-manager": "91d5727a3aef5c6c3e707a31ad3d994274a03b716b3f49fcb4f1cf78b447ca7e",
        "backup-manager-web": "250b3b4f6eeeeb134e96928a7bd999ffe8e7085e299c853e913ed9ffab3a2f16"
      },
      "local_image_id_sha256": "3d311fbfa88fa28d9733ee170da44d7a3bf8c8df6e2db3f05aae089ce8a7dbb0"
    }
  ]
}`

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "the shape 4.1 writes", body: good},
		{
			name:    "not JSON at all",
			body:    "version=1\n",
			wantErr: "parse",
		},
		{
			name:    "an architecture entry with no hashes",
			body:    `{"version":"v","commit":"c","architectures":[{"architecture":"amd64","binary_sha256":{}}]}`,
			wantErr: "binary_sha256",
		},
		{
			name:    "a hash that is not a SHA-256",
			body:    `{"version":"v","commit":"c","architectures":[{"architecture":"amd64","binary_sha256":{"backup-manager":"deadbeef","backup-manager-web":"` + strings.Repeat("a", 64) + `"}}]}`,
			wantErr: "backup-manager",
		},
		{
			name:    "no architectures at all",
			body:    `{"version":"v","commit":"c","architectures":[]}`,
			wantErr: "architectures",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release-manifest.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			m, err := LoadReleaseManifest(path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadReleaseManifest: %v", err)
				}
				entry, err := m.Arch("amd64")
				if err != nil {
					t.Fatalf("Arch(amd64): %v", err)
				}
				if got := entry.BinarySHA256["backup-manager"]; !strings.HasPrefix(got, "91d5727a") {
					t.Fatalf("backup-manager hash = %q", got)
				}
				if _, err := m.Arch("arm64"); err == nil {
					t.Fatal("Arch(arm64) succeeded on a manifest that only records amd64")
				}
				return
			}
			if err == nil {
				t.Fatalf("LoadReleaseManifest accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadReleaseManifest_TheRepositoryOne reads the real file 4.1
// committed. It is a smoke test with a point: if the release manifest's
// shape ever changes, this package's whole parity claim silently stops
// being checkable, and that should break here rather than in a release.
func TestLoadReleaseManifest_TheRepositoryOne(t *testing.T) {
	path := repoFile(t, "container/release-manifest.json")
	m, err := LoadReleaseManifest(path)
	if err != nil {
		t.Fatalf("LoadReleaseManifest(%s): %v", path, err)
	}
	for _, a := range Arches {
		if _, err := m.Arch(a.GOARCH); err != nil {
			t.Fatalf("the release manifest records no %s entry, so no %s SPK can be verified: %v", a.GOARCH, a.DSM, err)
		}
	}
}

// repoFile resolves a repository-root-relative path from inside this
// package's own directory.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", rel)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", rel, err)
	}
	return path
}
