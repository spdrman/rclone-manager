package spk

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// fakeELF builds a minimal but genuinely parseable ELF64 little-endian
// executable header for machine, followed by payload.
//
// Why synthesise one rather than cross-compile a real binary in the test:
// Verify's architecture check reads the ELF machine field, so the test
// needs real ELF headers, but it does not need real code. Cross-compiling
// even a hello-world for a second GOARCH pulls in a full runtime build the
// first time, which would put minutes into a suite that otherwise runs in
// well under a second. The header below is the real format; debug/elf
// parses it the same way it parses the shipped binaries.
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
	// e_phoff / e_shoff stay zero: no program or section headers, which
	// debug/elf reads as "this file has none" rather than as corruption.
	binary.LittleEndian.PutUint16(b[52:54], ehsize)
	binary.LittleEndian.PutUint16(b[54:56], 56) // e_phentsize
	binary.LittleEndian.PutUint16(b[58:60], 64) // e_shentsize
	return append(b, payload...)
}

// stagedBinaries writes a fakeELF pair for goarch into a fresh directory
// and returns that directory. payloadSuffix lets a caller produce a
// deliberately different binary for a mismatch control.
func stagedBinaries(t *testing.T, goarch, payloadSuffix string) string {
	t.Helper()
	machine := elf.EM_X86_64
	if goarch == "arm64" {
		machine = elf.EM_AARCH64
	}
	dir := t.TempDir()
	for _, name := range CoreBinaries {
		body := fakeELF(machine, []byte(name+" "+goarch+" "+payloadSuffix))
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// stagedUIBundle writes a minimal but valid provider UI bundle into a
// fresh directory and returns it: an app shell, one asset, and the marker
// naming the provider it was built for.
//
// Synthesised rather than built with vite, for the same reason fakeELF is
// synthesised rather than cross-compiled: what stageUIBundle reads is the
// SHAPE (a marker naming this provider, and an index.html), and running a
// real frontend build would put a node toolchain into a suite that
// otherwise finishes in under a second. platform lets a caller stage a
// bundle for the wrong provider, which is the control the marker exists
// for.
func stagedUIBundle(t *testing.T, platform string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		UIBundleMarkerName: `{"schema":"rclone-manager/ui-bundle/1","platform":"` + platform + `"}`,
		"index.html":       "<!doctype html><title>Backup Manager</title><script src=/assets/app.js></script>",
		"assets/app.js":    "// " + platform + " bridge\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return dir
}

// manifestFor hashes whatever stagedBinaries just wrote and returns a
// ReleaseManifest that agrees with it. A test that wants a mismatch builds
// the manifest from one directory and the package from another.
func manifestFor(t *testing.T, goarch, binariesDir string) ReleaseManifest {
	t.Helper()
	entry := ArchEntry{Architecture: goarch, BinarySHA256: map[string]string{}}
	for _, name := range CoreBinaries {
		sum, err := sha256File(filepath.Join(binariesDir, name))
		if err != nil {
			t.Fatalf("hash %s: %v", name, err)
		}
		entry.BinarySHA256[name] = sum
	}
	return ReleaseManifest{
		Version:       "1.0.0-1",
		Commit:        "0000000000000000000000000000000000000000",
		Architectures: []ArchEntry{entry},
	}
}

// buildFixture builds a package for goarch from freshly staged binaries
// and returns the .spk path plus the manifest that agrees with it.
func buildFixture(t *testing.T, goarch string) (string, ReleaseManifest) {
	t.Helper()
	bins := stagedBinaries(t, goarch, "release")
	manifest := manifestFor(t, goarch, bins)
	out := t.TempDir()
	path, err := Build(BuildOptions{
		GOARCH:      goarch,
		Version:     manifest.Version,
		BinariesDir: bins,
		UIBundleDir: stagedUIBundle(t, UIBundlePlatform),
		OutDir:      out,
	})
	if err != nil {
		t.Fatalf("Build(%s): %v", goarch, err)
	}
	return path, manifest
}
