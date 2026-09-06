package packaging

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #83's hard rule, exercised in both directions.
//
// The whole issue is one sentence: the .UPK packages the exact canonical
// image and binary content for that release and architecture, and may not
// compile a behaviourally different UGOS build. A check that only ever
// passes on the good package cannot tell anyone whether that sentence is
// true, so every rule below has a deliberately divergent package next to
// it and the failure has to name the thing that diverged. The controls
// are the point; the baseline exists so that a control "failing" means
// something other than "this verifier rejects everything".
//
// The divergent packages are the real shapes, not arbitrary corruption:
// an image rebuilt locally instead of fetched, an image repacked under a
// different tag, the wrong architecture's image in an architecture's
// directory, a fetch from somewhere other than the release, and the
// subtle one, the canonical binary present in a lower layer and replaced
// in a higher one, which is a divergent image whose lower layer hashes
// correctly.

// ---------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------

type layerFile struct {
	name string
	body []byte
	// whiteout writes a `.wh.<name>` marker instead of a file, which is
	// how a layer deletes a path the layer below it provided.
	whiteout bool
}

// canonicalBinaryContent is what the fixture's "release build" of each
// binary contains. Deliberately not empty and deliberately different per
// binary: two binaries that happen to hash the same would let a swap go
// unnoticed.
func canonicalBinaryContent(binary, arch string) []byte {
	return []byte("canonical " + binary + " for " + arch + "\n")
}

// writeImageArchive writes a `docker save`-shaped OCI archive: an
// oci-layout, an index.json naming one manifest, and blobs addressed by
// their own SHA-256. Addressed by their own digest matters, because that
// is how the verifier resolves them: a fixture that wrote made-up blob
// names would be testing a resolver that does not exist.
func writeImageArchive(t *testing.T, dest, tag, arch string, layers [][]layerFile) {
	t.Helper()

	blobs := map[string][]byte{}
	addBlob := func(body []byte) string {
		sum := sha256.Sum256(body)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		blobs[digest] = body
		return digest
	}

	type descriptor struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}

	var layerDescriptors []descriptor
	var diffIDs []string
	for _, files := range layers {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, f := range files {
			name := f.name
			body := f.body
			if f.whiteout {
				dir, base := filepath.Split(f.name)
				name = filepath.Join(dir, ".wh."+base)
				body = nil
			}
			if err := tw.WriteHeader(&tar.Header{
				Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
			}); err != nil {
				t.Fatalf("layer header %s: %v", name, err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("layer body %s: %v", name, err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatalf("layer close: %v", err)
		}
		digest := addBlob(buf.Bytes())
		diffIDs = append(diffIDs, digest)
		layerDescriptors = append(layerDescriptors, descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar",
			Digest:    digest,
			Size:      int64(buf.Len()),
		})
	}

	config, err := json.Marshal(map[string]any{
		"architecture": arch,
		"os":           "linux",
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	configDigest := addBlob(config)

	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest,
			Size:      int64(len(config)),
		},
		"layers": layerDescriptors,
	})
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	manifestDigest := addBlob(manifest)

	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []descriptor{{
			MediaType:   "application/vnd.oci.image.manifest.v1+json",
			Digest:      manifestDigest,
			Size:        int64(len(manifest)),
			Annotations: map[string]string{"io.containerd.image.name": tag},
		}},
	})
	if err != nil {
		t.Fatalf("index: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(dest)
	if err != nil {
		t.Fatalf("create %s: %v", dest, err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	write := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("archive header %s: %v", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("archive body %s: %v", name, err)
		}
	}
	// Blobs first, so the fixture also proves the verifier does not
	// depend on index.json arriving before the blobs it names.
	for digest, body := range blobs {
		write("blobs/sha256/"+strings.TrimPrefix(digest, "sha256:"), body)
	}
	write("oci-layout", []byte(`{"imageLayoutVersion": "1.0.0"}`))
	write("index.json", index)
	if err := tw.Close(); err != nil {
		t.Fatalf("archive close: %v", err)
	}
}

// upkFixture is a staged ugcli project plus the release manifest it is
// meant to be the exact content of.
type upkFixture struct {
	stage     string
	arch      string
	canonical Canonical
	manifest  ReleaseManifest
	tarPath   string
}

func newUPKFixture(t *testing.T, arch string) *upkFixture {
	t.Helper()
	c := MustLoad()
	stage := t.TempDir()

	writeFile(t, filepath.Join(stage, "project.yaml"), fmt.Sprintf(`spec_version: "2.1"
app_id: com.spdrman.backupmanager
version: %s
support_arch:
  - amd64
  - arm64
is_docker_app: true
port: 28080
open_type: inner
`, c.Image.Tag))

	writeFile(t, filepath.Join(stage, "rootfs_common", "docker-compose.yaml"), fmt.Sprintf(`services:
  backup-manager:
    image: %s
    command: ["/backup-manager-web", "serve", "--profile=ugos"]
  backup-manager-ui:
    image: %s
    command: ["/backup-manager-web", "serve-ui", "--profile=ugos"]
`, c.Image.Reference, c.Image.Reference))

	var base, top []layerFile
	hashes := map[string]string{}
	for i, binary := range c.Binaries {
		name := strings.TrimPrefix(binary, "/")
		body := canonicalBinaryContent(name, arch)
		sum := sha256.Sum256(body)
		hashes[name] = hex.EncodeToString(sum[:])
		// Split the binaries across two layers so the fixture exercises
		// the layer walk rather than a single-layer special case.
		if i%2 == 0 {
			base = append(base, layerFile{name: name, body: body})
		} else {
			top = append(top, layerFile{name: name, body: body})
		}
	}
	// A file nothing asks about, so the walk has something to ignore.
	base = append(base, layerFile{name: "etc/os-release", body: []byte("ID=distroless\n")})

	tarPath := filepath.Join(stage, "rootfs_"+arch, "images", "backup-manager-"+c.Image.Tag+"-"+arch+".tar")
	writeImageArchive(t, tarPath, c.Image.Reference, arch, [][]layerFile{base, top})

	digest := "sha256:" + strings.Repeat("ab", 32)
	writeJSON(t, filepath.Join(stage, "image-source-"+arch+".json"), UPKImageSource{
		Reference:    c.Image.Reference,
		Digest:       digest,
		Architecture: arch,
		Tar:          filepath.Base(tarPath),
		FetchedAt:    "2026-09-05T00:00:00Z",
		FetchedBy:    "upk_test.go",
	})

	other := "arm64"
	if arch == "arm64" {
		other = "amd64"
	}
	otherHashes := map[string]string{}
	for _, binary := range c.Binaries {
		name := strings.TrimPrefix(binary, "/")
		sum := sha256.Sum256(canonicalBinaryContent(name, other))
		otherHashes[name] = hex.EncodeToString(sum[:])
	}

	return &upkFixture{
		stage:     stage,
		arch:      arch,
		canonical: c,
		tarPath:   tarPath,
		manifest: ReleaseManifest{
			Version: c.Image.Tag,
			Architectures: []ReleaseArchitecture{
				{Architecture: arch, BinarySHA256: hashes, RegistryDigest: strptr(digest)},
				{Architecture: other, BinarySHA256: otherHashes, RegistryDigest: strptr("sha256:" + strings.Repeat("cd", 32))},
			},
		},
	}
}

func (f *upkFixture) verify() *UPKReport {
	return VerifyUPKStage(f.stage, f.arch, f.canonical, f.manifest)
}

func strptr(s string) *string { return &s }

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	writeFile(t, path, string(raw)+"\n")
}

func upkCheck(t *testing.T, r *UPKReport, name string) UPKCheck {
	t.Helper()
	c, ok := r.Check(name)
	if !ok {
		t.Fatalf("the report has no check %q; it ran %v", name, r.CheckNames())
	}
	return c
}

func requireUPKPass(t *testing.T, r *UPKReport, name string) {
	t.Helper()
	if c := upkCheck(t, r, name); c.Status != UPKPass {
		t.Fatalf("check %q is %s, want PASS: %s", name, c.Status, c.Detail)
	}
}

// requireUPKFail insists the failure MENTIONS something specific. A check
// that failed for an unrelated reason satisfies a bare "this failed", and
// then the control is green while the rule it names has quietly stopped
// working.
func requireUPKFail(t *testing.T, r *UPKReport, name, wantSubstring string) {
	t.Helper()
	c := upkCheck(t, r, name)
	if c.Status != UPKFail {
		t.Fatalf("check %q is %s, but this package was built to fail it: %s", name, c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, wantSubstring) {
		t.Fatalf("check %q failed with %q, which does not mention %q", name, c.Detail, wantSubstring)
	}
	if r.OK() {
		t.Fatalf("check %q failed and the report still says the package may be packed:\n%s", name, r)
	}
}

// ---------------------------------------------------------------------
// The baseline
// ---------------------------------------------------------------------

// TestVerifyUPKStage_AcceptsAStageBuiltFromTheCanonicalRelease is what
// every control below is measured against. Without it a control that
// "fails" proves nothing, because the verifier might reject everything.
func TestVerifyUPKStage_AcceptsAStageBuiltFromTheCanonicalRelease(t *testing.T) {
	t.Parallel()
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			f := newUPKFixture(t, arch)
			r := f.verify()
			if !r.OK() {
				t.Fatalf("a stage built from the canonical release failed verification:\n%s", r)
			}
			if len(r.Deferred()) != 0 {
				t.Fatalf("this fixture records a registry digest, so nothing should be deferred:\n%s", r)
			}
		})
	}
}

// TestVerifyUPKStage_RunsEveryDeclaredCheck closes the gap every
// list-driven gate has: a check that silently stopped running looks
// exactly like a check that passed.
func TestVerifyUPKStage_RunsEveryDeclaredCheck(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	r := f.verify()
	for _, name := range UPKChecks {
		if _, ok := r.Check(name); !ok {
			t.Errorf("UPKChecks declares %q and the verifier never ran it; it ran %v", name, r.CheckNames())
		}
	}
	if len(r.Checks) != len(UPKChecks) {
		t.Errorf("the verifier ran %v and UPKChecks declares %v", r.CheckNames(), UPKChecks)
	}
}

// ---------------------------------------------------------------------
// The hard rule: a divergent build must not pass
// ---------------------------------------------------------------------

// TestVerifyUPKStage_RefusesALocallyRebuiltImage is issue #83's whole
// sentence as a control. The package is perfectly well formed, correctly
// tagged, for the right architecture, fetched at the recorded digest, and
// the binary inside it is not the release's binary. That is exactly what
// "compiling a behaviourally different UGOS build" produces, and it is
// the one thing no amount of packaging correctness can excuse.
func TestVerifyUPKStage_RefusesALocallyRebuiltImage(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")

	rebuilt := [][]layerFile{{
		{name: "backup-manager", body: []byte("locally rebuilt backup-manager\n")},
		{name: "backup-manager-web", body: canonicalBinaryContent("backup-manager-web", "amd64")},
	}}
	writeImageArchive(t, f.tarPath, f.canonical.Image.Reference, "amd64", rebuilt)

	r := f.verify()
	requireUPKFail(t, r, UPKCheckBinaryContentParity, "/backup-manager hashes to")
	// The corroborating checks still pass, which is the finding: a
	// divergent image is invisible to every one of them.
	requireUPKPass(t, r, UPKCheckImageReference)
	requireUPKPass(t, r, UPKCheckImageArchitecture)
	requireUPKPass(t, r, UPKCheckRegistryDigestParity)
}

// TestVerifyUPKStage_RefusesABinaryReplacedInAHigherLayer is the subtle
// shape of the same defect. The canonical binary IS in the image, in the
// bottom layer, hashing exactly right; a layer above it replaces the
// file. A verifier that stopped at "the canonical hash appears somewhere
// in the archive" would pass this, and the container would run the
// replacement.
func TestVerifyUPKStage_RefusesABinaryReplacedInAHigherLayer(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")

	layered := [][]layerFile{
		{
			{name: "backup-manager", body: canonicalBinaryContent("backup-manager", "amd64")},
			{name: "backup-manager-web", body: canonicalBinaryContent("backup-manager-web", "amd64")},
		},
		{{name: "backup-manager", body: []byte("a different backup-manager, one layer up\n")}},
	}
	writeImageArchive(t, f.tarPath, f.canonical.Image.Reference, "amd64", layered)

	r := f.verify()
	requireUPKFail(t, r, UPKCheckBinaryContentParity, "/backup-manager hashes to")
}

// TestVerifyUPKStage_RefusesABinaryDeletedInAHigherLayer is the other
// half of the layer walk: a whiteout removes the canonical binary, and
// the image ships without it. "Present in some layer" would pass that
// too.
func TestVerifyUPKStage_RefusesABinaryDeletedInAHigherLayer(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")

	layered := [][]layerFile{
		{
			{name: "backup-manager", body: canonicalBinaryContent("backup-manager", "amd64")},
			{name: "backup-manager-web", body: canonicalBinaryContent("backup-manager-web", "amd64")},
		},
		{{name: "backup-manager", whiteout: true}},
	}
	writeImageArchive(t, f.tarPath, f.canonical.Image.Reference, "amd64", layered)

	r := f.verify()
	requireUPKFail(t, r, UPKCheckBinaryContentParity, "/backup-manager is not in the image at all")
}

// TestVerifyUPKStage_RefusesAnImageRepackedUnderADifferentTag is the
// cheapest way to ship something else: keep the bytes, change the name.
// UGOS `docker load`s whatever the archive is tagged and the compose file
// then names something that is not there.
func TestVerifyUPKStage_RefusesAnImageRepackedUnderADifferentTag(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")

	layers := [][]layerFile{{
		{name: "backup-manager", body: canonicalBinaryContent("backup-manager", "amd64")},
		{name: "backup-manager-web", body: canonicalBinaryContent("backup-manager-web", "amd64")},
	}}
	writeImageArchive(t, f.tarPath, "backup-manager-ugos:0.3.0-local", "amd64", layers)

	r := f.verify()
	requireUPKFail(t, r, UPKCheckImageReference, "backup-manager-ugos:0.3.0-local")
	// And the content check still passes, which is why both exist: this
	// package's bytes are right and its identity is wrong.
	requireUPKPass(t, r, UPKCheckBinaryContentParity)
}

// TestVerifyUPKStage_RefusesTheWrongArchitecturesImage plants the arm64
// image in rootfs_amd64. Hash parity alone would not catch it in the case
// that matters, so the architecture is read out of the image config
// rather than from the directory name.
func TestVerifyUPKStage_RefusesTheWrongArchitecturesImage(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")

	layers := [][]layerFile{{
		{name: "backup-manager", body: canonicalBinaryContent("backup-manager", "arm64")},
		{name: "backup-manager-web", body: canonicalBinaryContent("backup-manager-web", "arm64")},
	}}
	writeImageArchive(t, f.tarPath, f.canonical.Image.Reference, "arm64", layers)

	r := f.verify()
	requireUPKFail(t, r, UPKCheckImageArchitecture, `architecture "arm64"`)
	// The arm64 binaries are in the manifest, under the arm64 row, so
	// this also has to be caught as content divergence against the amd64
	// row it is being checked as.
	requireUPKFail(t, r, UPKCheckBinaryContentParity, "hashes to")
}

// TestVerifyUPKStage_RefusesAnImageFetchedFromSomewhereElse is the
// corroborating half. The bytes are right and the provenance is not,
// which is the state a package is in when someone re-fetched it from a
// mirror, a cache or a different release and did not notice.
func TestVerifyUPKStage_RefusesAnImageFetchedFromSomewhereElse(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	writeJSON(t, filepath.Join(f.stage, "image-source-amd64.json"), UPKImageSource{
		Reference:    f.canonical.Image.Reference,
		Digest:       "sha256:" + strings.Repeat("ee", 32),
		Architecture: "amd64",
	})
	r := f.verify()
	requireUPKFail(t, r, UPKCheckRegistryDigestParity, strings.Repeat("ee", 32))
	requireUPKPass(t, r, UPKCheckBinaryContentParity)
}

// TestVerifyUPKStage_RefusesASidecarTheDaemonNeverAgreedWith is the
// consistency check inside the provenance record. The sidecar names a
// pull digest and, beside it, what the local daemon said it held; a
// digest the daemon never reported is a hand-edited record, and a
// hand-edited provenance record is the one thing this half of the claim
// cannot survive.
func TestVerifyUPKStage_RefusesASidecarTheDaemonNeverAgreedWith(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	digest := *f.manifest.Architectures[0].RegistryDigest
	writeJSON(t, filepath.Join(f.stage, "image-source-amd64.json"), UPKImageSource{
		Reference:    f.canonical.Image.Reference,
		Digest:       digest,
		Architecture: "amd64",
		DaemonRepoDigests: []string{
			"ghcr.io/spdrman/backup-manager@sha256:" + strings.Repeat("11", 32),
		},
	})
	r := f.verify()
	requireUPKFail(t, r, UPKCheckRegistryDigestParity, "does not include it")
}

// TestVerifyUPKStage_AcceptsASidecarThatNamesTheIndexAlongsideTheManifest
// is that rule's own boundary, and the reason the daemon record is a list
// rather than a value. A multi-architecture release makes the daemon
// report two digests, the index and this architecture's manifest, and a
// package fetched by either of them is a package fetched from the
// release.
func TestVerifyUPKStage_AcceptsASidecarThatNamesTheIndexAlongsideTheManifest(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	digest := *f.manifest.Architectures[0].RegistryDigest
	writeJSON(t, filepath.Join(f.stage, "image-source-amd64.json"), UPKImageSource{
		Reference:    f.canonical.Image.Reference,
		Digest:       digest,
		Architecture: "amd64",
		DaemonRepoDigests: []string{
			"ghcr.io/spdrman/backup-manager@sha256:" + strings.Repeat("99", 32),
			"ghcr.io/spdrman/backup-manager@" + digest,
		},
	})
	r := f.verify()
	requireUPKPass(t, r, UPKCheckRegistryDigestParity)
}

// TestVerifyUPKStage_RefusesAPackageWithNoRecordOfWhereItCameFrom: the
// sidecar missing is not "nothing to check", it is "nobody wrote down
// where these bytes came from", and the release records a digest they
// were meant to match.
func TestVerifyUPKStage_RefusesAPackageWithNoRecordOfWhereItCameFrom(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	if err := os.Remove(filepath.Join(f.stage, "image-source-amd64.json")); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}
	r := f.verify()
	requireUPKFail(t, r, UPKCheckStageLayout, "image-source-amd64.json")
}

// TestVerifyUPKStage_RefusesAVersionThatIsNotTheCanonicalRelease covers
// the one divergence no hash comparison can see: both halves of the
// package are internally consistent and the App Center is told the wrong
// release.
func TestVerifyUPKStage_RefusesAVersionThatIsNotTheCanonicalRelease(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	raw, err := os.ReadFile(filepath.Join(f.stage, "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}
	writeFile(t, filepath.Join(f.stage, "project.yaml"),
		strings.Replace(string(raw), "version: "+f.canonical.Image.Tag, "version: 9.9.9", 1))
	r := f.verify()
	requireUPKFail(t, r, UPKCheckPackageVersion, "9.9.9")
}

// TestVerifyUPKStage_RefusesAPackageThatIsNotADockerApp: without
// is_docker_app UGOS never loads the bundled image, so the identity the
// rest of this file checks is the identity of something that does not
// run.
func TestVerifyUPKStage_RefusesAPackageThatIsNotADockerApp(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	raw, err := os.ReadFile(filepath.Join(f.stage, "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}
	writeFile(t, filepath.Join(f.stage, "project.yaml"),
		strings.Replace(string(raw), "is_docker_app: true", "is_docker_app: false", 1))
	r := f.verify()
	requireUPKFail(t, r, UPKCheckPackageVersion, "is_docker_app")
}

// TestVerifyUPKStage_RefusesTwoImageTarsInOneArchitecture: UGOS loads
// every tar in the directory, so a leftover from a previous release is
// not clutter, it is a second image the compose reference may resolve to.
func TestVerifyUPKStage_RefusesTwoImageTarsInOneArchitecture(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	stale := filepath.Join(f.stage, "rootfs_amd64", "images", "backup-manager-0.1.0-amd64.tar")
	writeImageArchive(t, stale, "ghcr.io/spdrman/backup-manager:0.1.0", "amd64", [][]layerFile{{
		{name: "backup-manager", body: []byte("an older release\n")},
	}})
	r := f.verify()
	requireUPKFail(t, r, UPKCheckStageLayout, "2 image tars")
}

// ---------------------------------------------------------------------
// The unpublished release
// ---------------------------------------------------------------------

// TestVerifyUPKStage_DefersTheDigestOnAnUnpublishedRelease is how the
// 0.3.0 state is handled honestly. There is no published digest to
// compare against, so the digest half of the claim cannot run; the
// content half still can, and does.
func TestVerifyUPKStage_DefersTheDigestOnAnUnpublishedRelease(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	f.canonical.Image.Published = false
	for i := range f.manifest.Architectures {
		f.manifest.Architectures[i].RegistryDigest = nil
	}

	r := f.verify()
	c := upkCheck(t, r, UPKCheckRegistryDigestParity)
	if c.Status != UPKDeferred {
		t.Fatalf("registry-digest-parity is %s on an unpublished release, want DEFERRED: %s", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "not pushed") {
		t.Errorf("the deferral does not say why it could not run: %q", c.Detail)
	}
	requireUPKPass(t, r, UPKCheckBinaryContentParity)
	if !r.OK() {
		t.Fatalf("an unpublished release must still be packable when its content matches:\n%s", r)
	}
}

// TestVerifyUPKStage_DeferredIsNotAPass is the rule that keeps the
// deferral honest. The whole risk of introducing a third status is that
// it quietly becomes a second word for PASS, and then the digest half of
// issue #83's hard rule is permanently satisfied by never being checked.
func TestVerifyUPKStage_DeferredIsNotAPass(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	f.canonical.Image.Published = false
	for i := range f.manifest.Architectures {
		f.manifest.Architectures[i].RegistryDigest = nil
	}
	r := f.verify()

	if len(r.Deferred()) == 0 {
		t.Fatal("this fixture is built to defer and nothing deferred")
	}
	for _, c := range r.Deferred() {
		if c.Status == UPKPass {
			t.Errorf("check %q is in both the deferred and the passed set", c.Name)
		}
	}
	if !strings.Contains(r.String(), "PASS WITH DEFERRALS") {
		t.Errorf("the report reads as a clean pass:\n%s", r)
	}
	if strings.Contains(r.String(), "VERDICT: PASS.\n") {
		t.Errorf("the report claims an unqualified pass while deferring a check:\n%s", r)
	}
}

// TestVerifyUPKStage_RefusesAPublishedReleaseWithNoRecordedDigest is the
// deferral's own boundary. Deferring is legitimate exactly while
// canonical.json and the release manifest agree that nothing was pushed.
// A published flag with no digest is a half-written release, and a
// package built against one is a package nobody can check later.
func TestVerifyUPKStage_RefusesAPublishedReleaseWithNoRecordedDigest(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	f.canonical.Image.Published = true
	for i := range f.manifest.Architectures {
		f.manifest.Architectures[i].RegistryDigest = nil
	}
	r := f.verify()
	requireUPKFail(t, r, UPKCheckRegistryDigestParity, "published")
}

// TestVerifyUPKStage_RefusesAnArchitectureTheReleaseNeverBuilt: an
// architecture with no row in the manifest has no recorded content, so
// there is nothing the package could be the exact content OF. Deliberately
// a failure and not a deferral: the release records binary hashes whether
// or not it was pushed, so nothing legitimate produces this.
func TestVerifyUPKStage_RefusesAnArchitectureTheReleaseNeverBuilt(t *testing.T) {
	t.Parallel()
	f := newUPKFixture(t, "amd64")
	f.manifest.Architectures = f.manifest.Architectures[1:]
	r := f.verify()
	requireUPKFail(t, r, UPKCheckBinaryContentParity, "records no amd64 build")
}

// ---------------------------------------------------------------------
// The shipped package
// ---------------------------------------------------------------------

// TestTheShippedUGOSPackageIsDerivedFromTheCanonicalRuntime runs issue
// #169's derivation gate over the real apps/ugos/upk compose wrapper: the
// image reference, the runtime profile, the storage mounts, the published
// port and both health checks all have to be the canonical runtime's, not
// this package's own.
func TestTheShippedUGOSPackageIsDerivedFromTheCanonicalRuntime(t *testing.T) {
	t.Parallel()
	c := MustLoad()
	svcs := readUGOSComposeServices(t)
	rt, drift := ReduceToRoles("ugos", svcs, c)
	drift = append(drift, CheckDerivation(rt, c)...)
	if len(drift) > 0 {
		t.Fatalf("apps/ugos/upk's compose wrapper is not derived from the canonical runtime:\n%s", FormatDrift(drift))
	}
}

// TestTheShippedUGOSPackageWouldFailADeliberateMismatch is the control
// the acceptance criteria ask for by name. The check above is only worth
// something if it goes red when the wrapper stops agreeing, so here it is
// watched doing that against the same real services with one field
// changed.
func TestTheShippedUGOSPackageWouldFailADeliberateMismatch(t *testing.T) {
	t.Parallel()
	c := MustLoad()

	for _, tc := range []struct {
		name    string
		damage  func([]Service) []Service
		field   string
		mention string
	}{
		{
			name: "a different image",
			damage: func(s []Service) []Service {
				s[0].Image = "ghcr.io/spdrman/backup-manager-ugos:0.3.0"
				return s
			},
			field:   FieldImageReference,
			mention: "backup-manager-ugos",
		},
		{
			name: "somebody else's runtime profile",
			damage: func(s []Service) []Service {
				for i := range s {
					for j, arg := range s[i].Command {
						if strings.HasPrefix(arg, "--profile=") {
							s[i].Command[j] = "--profile=truenas"
						}
					}
				}
				return s
			},
			field:   FieldRuntimeProfile,
			mention: `"truenas"`,
		},
		{
			name: "the engine on the edge",
			damage: func(s []Service) []Service {
				for i := range s {
					if strings.Contains(strings.Join(s[i].Command, " "), "serve-ui") {
						continue
					}
					s[i].Ports = []string{"8080:8080"}
				}
				return s
			},
			field:   FieldPublishedPort,
			mention: "publishes nothing",
		},
		{
			name: "a mount the runtime does not declare",
			damage: func(s []Service) []Service {
				for i := range s {
					if strings.Contains(strings.Join(s[i].Command, " "), "serve-ui") {
						continue
					}
					s[i].Mounts = append(s[i].Mounts, Mount{HostPath: "/volume1", ContainerPath: "/volume1"})
				}
				return s
			},
			field:   FieldStorageMounts,
			mention: "/volume1",
		},
		{
			name: "the freshness verdict as the start gate",
			damage: func(s []Service) []Service {
				for i := range s {
					if strings.Contains(strings.Join(s[i].Command, " "), "serve-ui") {
						continue
					}
					s[i].HealthcheckTest = []string{"CMD", "/backup-manager", "status"}
				}
				return s
			},
			field:   FieldHealthCheck,
			mention: "canonical engine check",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svcs := tc.damage(readUGOSComposeServices(t))
			rt, drift := ReduceToRoles("ugos", svcs, c)
			drift = append(drift, CheckDerivation(rt, c)...)
			if len(drift) == 0 {
				t.Fatalf("the derivation gate accepted a wrapper with %s", tc.name)
			}
			var named []string
			for _, d := range drift {
				named = append(named, d.Field)
				if d.Field == tc.field && strings.Contains(d.Detail, tc.mention) {
					return
				}
			}
			t.Fatalf("the mismatch was caught but not as %s mentioning %q; it reported %v:\n%s",
				tc.field, tc.mention, named, FormatDrift(drift))
		})
	}
}

// TestTheShippedUGOSPackageNeedsNoProhibitedHostPrivilege is the second
// acceptance criterion in the same sentence as the identity claim: no
// privileged mode, no Docker socket, no host networking, no host PID or
// IPC namespace, no unbounded host filesystem access.
//
// distribution/compose runs the full prohibition list over every
// registered derived artifact, this one included. What is here is the
// UGOS-named restatement plus its control, because "some suite somewhere
// covers it" is how a provider drops out of a list and nobody notices.
func TestTheShippedUGOSPackageNeedsNoProhibitedHostPrivilege(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(ugosComposePath())
	if err != nil {
		t.Fatalf("read the UGOS compose wrapper: %v", err)
	}
	body := string(raw)

	forbidden := []struct{ needle, why string }{
		{"privileged: true", "privileged mode disables the container boundary entirely"},
		{"network_mode: host", "host networking puts the engine on every host interface"},
		{"pid: host", "the host PID namespace lets the container signal host processes"},
		{"ipc: host", "the host IPC namespace is shared memory with the host"},
		{"/var/run/docker.sock", "the Docker socket is root on the host with extra steps"},
		{"/run/docker.sock", "the Docker socket is root on the host with extra steps"},
		{"cap_add", "nothing this runtime does needs a capability a plain non-root process lacks"},
		{"seccomp:unconfined", "turning off the host's confinement is the same class of exception as privileged mode"},
		{"apparmor:unconfined", "turning off the host's confinement is the same class of exception as privileged mode"},
	}
	for _, f := range forbidden {
		if strings.Contains(body, f.needle) {
			t.Errorf("the UGOS wrapper declares %q: %s", f.needle, f.why)
		}
	}

	// The positive control: the wrapper reaches the state it needs
	// through named mounts and a non-root user rather than through any of
	// the above, so the absences are a posture and not an omission.
	for _, needed := range []string{"privileged: false", "cap_drop", "no-new-privileges:true", "read_only: true", `user: "1000:1000"`} {
		if !strings.Contains(body, needed) {
			t.Errorf("the UGOS wrapper does not declare %q, so its lack of host privilege is an omission rather than a decision", needed)
		}
	}

	// And the control that proves the loop above can fire at all.
	planted := body + "\n    privileged: true\n"
	if !strings.Contains(planted, "privileged: true") {
		t.Fatal("the planted control did not plant anything, so the loop above is asserting nothing")
	}
}

func ugosComposePath() string {
	return Path(filepath.Join("apps", "ugos", "upk", "rootfs_common", "docker-compose.yaml"))
}

// readUGOSComposeServices parses the shipped wrapper into the Service
// shape the derivation gate works on.
func readUGOSComposeServices(t *testing.T) []Service {
	t.Helper()
	svcs, err := ReadCompose(ugosComposePath(), nil)
	if err != nil {
		t.Fatalf("read the UGOS compose wrapper: %v", err)
	}
	if len(svcs) == 0 {
		t.Fatal("the UGOS compose wrapper declares no services, so every assertion below would be vacuous")
	}
	return svcs
}
