package main

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/apps/synology/spk"
)

// The library under this CLI is thoroughly tested, which makes this seam
// more dangerous rather than less. spk.Verify's contract is that it
// returns a Report carrying failures rather than an error, so "exit
// non-zero when the report is not OK" is a fail-closed behaviour only
// this file implements: a cmdVerify that returned 0 on a failing report
// would send an operator to real hardware with a package nobody checked
// while every unit test in spk/ stayed green. Preconditions 4 and 5 of
// the acceptance procedure make this CLI the mandatory first step of
// that hardware run.

// fakeELF builds a minimal but genuinely parseable ELF64 little-endian
// executable header, which is all Build's architecture check reads. A
// second copy of spk's own test helper, because that one is internal to
// the package and cross-compiling a real binary here would put minutes
// into a suite that runs in under a second.
func fakeELF(machine elf.Machine, payload []byte) []byte {
	const ehsize = 64
	b := make([]byte, ehsize)
	copy(b[0:4], []byte{0x7f, 'E', 'L', 'F'})
	b[4] = byte(elf.ELFCLASS64)
	b[5] = byte(elf.ELFDATA2LSB)
	b[6] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(b[16:18], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(b[18:20], uint16(machine))
	binary.LittleEndian.PutUint32(b[20:24], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(b[52:54], ehsize)
	binary.LittleEndian.PutUint16(b[54:56], 56)
	binary.LittleEndian.PutUint16(b[58:60], 64)
	return append(b, payload...)
}

// stagedBinaries writes an amd64 pair whose contents depend on flavour,
// so a caller can build a package from one set and a manifest from
// another.
func stagedBinaries(t *testing.T, flavour string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range spk.CoreBinaries {
		body := fakeELF(elf.EM_X86_64, []byte(name+" "+flavour))
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// stagedUIBundle writes a minimal valid bundle for this provider: the
// marker naming it and an app shell. `spkctl build` requires one, because
// a package without it would serve the generic bridge on a Synology NAS
// (issue #180).
func stagedUIBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		spk.UIBundleMarkerName: `{"schema":"rclone-manager/ui-bundle/1","platform":"` + spk.UIBundlePlatform + `"}`,
		"index.html":           "<!doctype html><title>Backup Manager</title>",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func writeManifest(t *testing.T, binariesDir string) string {
	t.Helper()
	entry := spk.ArchEntry{Architecture: "amd64", BinarySHA256: map[string]string{}}
	for _, name := range spk.CoreBinaries {
		body, err := os.ReadFile(filepath.Join(binariesDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sum := sha256.Sum256(body)
		entry.BinarySHA256[name] = hex.EncodeToString(sum[:])
	}
	raw, err := json.MarshalIndent(spk.ReleaseManifest{
		Version:       "1.0.0-1",
		Commit:        "0000000000000000000000000000000000000000",
		Architectures: []spk.ArchEntry{entry},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := filepath.Join(t.TempDir(), "release-manifest.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// capture runs fn with os.Stdout and os.Stderr redirected, and returns
// everything it wrote. run() prints the report and the refusal directly,
// so there is no other way to assert on what an operator would see.
func capture(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	code := fn()
	os.Stdout, os.Stderr = stdout, stderr
	_ = w.Close()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	_ = r.Close()
	return sb.String(), code
}

// buildFixture runs the CLI's own build command, so the package every
// verify case below reads is one this CLI produced.
func buildFixture(t *testing.T, binariesDir string) string {
	t.Helper()
	out := t.TempDir()
	printed, code := capture(t, func() int {
		return run([]string{"build", "--arch", "amd64", "--version", "1.0.0-1",
			"--binaries", binariesDir, "--ui-bundle", stagedUIBundle(t), "--out", out})
	})
	if code != 0 {
		t.Fatalf("spkctl build exited %d:\n%s", code, printed)
	}
	path := strings.TrimSpace(printed)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spkctl build printed %q, which is not a file it wrote: %v", path, err)
	}
	return path
}

func TestRun_BuildThenVerify(t *testing.T) {
	binaries := stagedBinaries(t, "release")
	spkPath := buildFixture(t, binaries)
	manifest := writeManifest(t, binaries)

	out, code := capture(t, func() int {
		return run([]string{"verify", "--spk", spkPath, "--manifest", manifest})
	})
	if code != 0 {
		t.Fatalf("verify of a package built from the manifest's own binaries exited %d:\n%s", code, out)
	}
	// The report is the evidence a release keeps, so it has to name what
	// it compared against, not just say ok.
	if !strings.Contains(out, manifest) {
		t.Fatalf("the verify output never names the manifest it used:\n%s", out)
	}
}

// TestRun_VerifyRefusesAPackageThatFailsACheck is the one this file
// exists for. Verify reports a failure rather than returning an error,
// so only the CLI can turn that into a non-zero exit.
func TestRun_VerifyRefusesAPackageThatFailsACheck(t *testing.T) {
	spkPath := buildFixture(t, stagedBinaries(t, "release"))
	// A manifest of binaries that are not the ones in the package: the
	// rebuilt-elsewhere case §3.7 exists to catch.
	manifest := writeManifest(t, stagedBinaries(t, "rebuilt"))

	out, code := capture(t, func() int {
		return run([]string{"verify", "--spk", spkPath, "--manifest", manifest})
	})
	if code != 1 {
		t.Fatalf("verify of a package whose binaries do not match the manifest exited %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, spk.CheckBinaryParity) {
		t.Fatalf("the refusal never names the check that failed:\n%s", out)
	}
}

func TestRun_UsageErrors(t *testing.T) {
	spkPath := buildFixture(t, stagedBinaries(t, "release"))
	missing := filepath.Join(t.TempDir(), "no-such-manifest.json")

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no arguments at all", nil, 2},
		{"an unknown subcommand", []string{"publish"}, 2},
		{"verify without --spk", []string{"verify"}, 2},
		{"an unparseable flag", []string{"verify", "--nope"}, 2},
		{"a manifest that is not there", []string{"verify", "--spk", spkPath, "--manifest", missing}, 1},
		{"a .spk that is not there", []string{"verify", "--spk", missing, "--manifest", missing}, 1},
		{"build without --arch", []string{"build", "--version", "1.0.0-1", "--binaries", t.TempDir()}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := capture(t, func() int { return run(tc.args) })
			if code != tc.want {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.want, out)
			}
			if strings.TrimSpace(out) == "" {
				t.Fatal("refused silently, so an operator has nothing to act on")
			}
		})
	}
}
