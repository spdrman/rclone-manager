package packaging

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file is issue #83's hard rule, made checkable.
//
// The rule: the UGOS .UPK packages the EXACT canonical image and binary
// content for that release and architecture. It may not compile a
// behaviourally different UGOS build. If the package's image content does
// not match the canonical release's recorded digest and binary SHA-256
// for the target architecture, the package is wrong no matter how well it
// installs.
//
// That is an identity claim, and an identity claim asserted in a README
// is worth nothing. What follows is the claim as a check, run over the
// staged ugcli project BEFORE `ugcli pack` rather than after, so a
// divergent image fails the build instead of shipping.
//
// # Why the binary hashes are the claim and the digest is corroboration
//
// The obvious check would be "the tar's digest equals the release
// manifest's registry_digest", and it cannot be written, because that
// digest does not survive the tar. `docker save` re-serialises the
// manifest into an OCI layout of its own and Docker's own uncompressed
// layer media types, so the archive's index.json names a manifest digest
// that is NOT the one ghcr.io assigned, and its layer blobs are diff_ids
// rather than the registry's compressed layer digests. Measured, not
// assumed: saving ghcr.io/spdrman/backup-manager pulled at
// sha256:11998a45… produces an index.json naming sha256:30600630….
//
// So the check is built the other way round, on the thing that does
// survive and is what actually matters: THE BYTES THAT EXECUTE. Every
// canonical binary is extracted out of the archive's own layers and
// hashed, and compared against container/release-manifest.json, which
// records exactly that ("binary_sha256 is hashed from the two binaries
// extracted out of the built image"). A locally rebuilt image, a patched
// binary, a different release or a UGOS-specific build all move those
// hashes; nothing that leaves them alone can have changed what runs.
//
// The registry digest is checked too, from the provenance sidecar
// build-upk.sh writes at fetch time (the RepoDigest Docker recorded for
// the pull), and it is deliberately NOT trusted on its own: a sidecar is
// a claim about where bytes came from, and the binary hashes are proof of
// what the bytes are. Either alone can be satisfied by a divergent
// package; together they cannot.
//
// # The unpublished release
//
// 0.3.0 is cut and not pushed, so container/release-manifest.json carries
// a null registry_digest per architecture and canonical.json says
// image.published false. The digest half of the claim therefore has
// nothing to compare against yet, and the honest verdict for that is
// DEFERRED and not PASS. UPKDeferred is a distinct status for exactly
// that reason: a report with a deferred check is not a report that
// passed, Report.String says so out loud, and
// TestUPKDeferredIsNotAPass keeps the two from collapsing into one.

// UPKStatus is one check's verdict.
type UPKStatus string

const (
	// UPKPass: the check ran and the package satisfied it.
	UPKPass UPKStatus = "PASS"
	// UPKFail: the check ran and the package did not satisfy it. Any
	// failure means the package must not be packed.
	UPKFail UPKStatus = "FAIL"
	// UPKDeferred: the check could not run because the thing it compares
	// against does not exist yet, and the reason is recorded. Never a
	// pass, and never silent.
	UPKDeferred UPKStatus = "DEFERRED"
)

// The check ids. They are constants so a test names the rule it is
// exercising rather than a string that can drift out from under it.
const (
	// UPKCheckStageLayout: the staged ugcli project has the files a
	// Docker app needs, and exactly one image tar for the architecture.
	UPKCheckStageLayout = "stage-layout"
	// UPKCheckPackageVersion: project.yaml's version is the canonical
	// release's tag, so the package cannot advertise one release while
	// carrying another's bytes.
	UPKCheckPackageVersion = "package-version"
	// UPKCheckImageReference: the archive, the compose wrapper and the
	// provenance sidecar all name the one canonical image reference.
	UPKCheckImageReference = "image-reference"
	// UPKCheckImageArchitecture: the archive's image config is for the
	// architecture this rootfs_<arch> directory claims.
	UPKCheckImageArchitecture = "image-architecture"
	// UPKCheckBinaryContentParity: THE identity claim. Every canonical
	// binary extracted from the archive hashes to what the release
	// manifest recorded for this architecture.
	UPKCheckBinaryContentParity = "binary-content-parity"
	// UPKCheckRegistryDigestParity: the digest the image was fetched at
	// is the digest the release manifest records for this architecture.
	UPKCheckRegistryDigestParity = "registry-digest-parity"
)

// UPKChecks is every check VerifyUPKStage runs, in report order. Exported
// so a test can assert the verifier covers all of them: a gate whose
// declared list and whose implementation disagree has a hole in it that
// is invisible from either side alone.
var UPKChecks = []string{
	UPKCheckStageLayout,
	UPKCheckPackageVersion,
	UPKCheckImageReference,
	UPKCheckImageArchitecture,
	UPKCheckBinaryContentParity,
	UPKCheckRegistryDigestParity,
}

// UPKCheck is one check's result.
type UPKCheck struct {
	Name   string
	Status UPKStatus
	Detail string
}

// UPKReport is the verdict on one staged package for one architecture.
type UPKReport struct {
	Stage        string
	Architecture string
	Checks       []UPKCheck
}

// OK reports whether nothing FAILED. It is deliberately not "everything
// passed": a deferred check is a check that could not run, and treating
// that as a failure would make the whole package unbuildable for as long
// as the release is unpushed, which would be a worse lie than the one it
// prevents. Callers that need "everything passed" ask Deferred() as well,
// and build-upk.sh prints both.
func (r *UPKReport) OK() bool {
	for _, c := range r.Checks {
		if c.Status == UPKFail {
			return false
		}
	}
	return true
}

// Deferred lists every check that could not run.
func (r *UPKReport) Deferred() []UPKCheck { return r.withStatus(UPKDeferred) }

// Failed lists every check the package did not satisfy.
func (r *UPKReport) Failed() []UPKCheck { return r.withStatus(UPKFail) }

func (r *UPKReport) withStatus(s UPKStatus) []UPKCheck {
	var out []UPKCheck
	for _, c := range r.Checks {
		if c.Status == s {
			out = append(out, c)
		}
	}
	return out
}

// Check returns the named check, and whether it ran at all. A check that
// silently stopped running is indistinguishable from one that passed
// unless a caller can ask this question.
func (r *UPKReport) Check(name string) (UPKCheck, bool) {
	for _, c := range r.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return UPKCheck{}, false
}

// CheckNames lists what actually ran.
func (r *UPKReport) CheckNames() []string {
	out := make([]string, 0, len(r.Checks))
	for _, c := range r.Checks {
		out = append(out, c.Name)
	}
	return out
}

func (r *UPKReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "UPK stage %s (%s)\n", r.Stage, r.Architecture)
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "  %-8s %s: %s\n", c.Status, c.Name, c.Detail)
	}
	switch {
	case !r.OK():
		b.WriteString("VERDICT: FAIL. This package must not be packed.\n")
	case len(r.Deferred()) > 0:
		b.WriteString("VERDICT: PASS WITH DEFERRALS. Every check that could run passed; the deferred ones above are not passes.\n")
	default:
		b.WriteString("VERDICT: PASS.\n")
	}
	return b.String()
}

// UPKImageSource is the provenance sidecar build-upk.sh writes beside the
// image tar: where the bytes came from, recorded at fetch time.
//
// It is a claim and it is treated as one. Nothing here proves what the
// archive contains; UPKCheckBinaryContentParity does that, from the
// archive itself. What this adds is the one fact the archive cannot
// carry, because `docker save` discards it: the registry digest the image
// was pulled at.
type UPKImageSource struct {
	Reference    string `json:"reference"`
	Digest       string `json:"digest"`
	Architecture string `json:"architecture"`
	Tar          string `json:"tar"`
	FetchedAt    string `json:"fetched_at"`
	FetchedBy    string `json:"fetched_by"`
	Note         string `json:"note,omitempty"`
}

// upkProject is the part of a ugcli project.yaml this verifier reads.
type upkProject struct {
	AppID       string   `yaml:"app_id"`
	Version     string   `yaml:"version"`
	SupportArch []string `yaml:"support_arch"`
	IsDockerApp bool     `yaml:"is_docker_app"`
	OpenType    string   `yaml:"open_type"`
	Port        int      `yaml:"port"`
}

// VerifyUPKStage holds one staged ugcli project, for one architecture, to
// the canonical release.
//
// It takes Canonical and ReleaseManifest as values rather than loading
// them itself, so a test can drive it against a deliberately altered
// source of truth and watch the answer change. A checker that loads its
// own expectations can only ever be tested against the one tree it ships
// in, which means its negative case is never exercised, which means
// nobody ever finds out it stopped checking.
func VerifyUPKStage(stage, arch string, c Canonical, m ReleaseManifest) *UPKReport {
	r := &UPKReport{Stage: stage, Architecture: arch}

	layout, tarPath, source := checkUPKLayout(r, stage, arch)
	checkUPKVersion(r, stage, c)

	if !layout {
		// Everything below reads the archive. Reporting five more
		// failures that all have the one cause above would bury it.
		return r
	}

	img, err := readImageArchive(tarPath, c.Binaries)
	if err != nil {
		r.add(UPKCheckImageReference, UPKFail, fmt.Sprintf("%s could not be read as an image archive: %v", rel(stage, tarPath), err))
		r.add(UPKCheckImageArchitecture, UPKFail, "unreadable archive")
		r.add(UPKCheckBinaryContentParity, UPKFail, "unreadable archive")
		checkUPKDigest(r, arch, source, c, m)
		return r
	}

	checkUPKReference(r, stage, img, source, c)
	checkUPKArchitecture(r, arch, img)
	checkUPKBinaryParity(r, arch, img, c, m)
	checkUPKDigest(r, arch, source, c, m)
	return r
}

func (r *UPKReport) add(name string, status UPKStatus, detail string) {
	r.Checks = append(r.Checks, UPKCheck{Name: name, Status: status, Detail: detail})
}

// checkUPKLayout answers the question every later check depends on: is
// there a staged Docker app here at all, and exactly one image tar for
// this architecture. Exactly one, because two tars in a rootfs_<arch>
// directory means UGOS loads both and the compose file's `image:`
// silently selects whichever one won.
func checkUPKLayout(r *UPKReport, stage, arch string) (bool, string, *UPKImageSource) {
	var problems []string

	project := filepath.Join(stage, "project.yaml")
	if _, err := os.Stat(project); err != nil {
		problems = append(problems, "no project.yaml")
	}
	compose := filepath.Join(stage, "rootfs_common", "docker-compose.yaml")
	if _, err := os.Stat(compose); err != nil {
		problems = append(problems, "no rootfs_common/docker-compose.yaml")
	}

	imagesDir := filepath.Join(stage, "rootfs_"+arch, "images")
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		problems = append(problems, fmt.Sprintf("no rootfs_%s/images directory, so this package carries no image for %s at all", arch, arch))
		r.add(UPKCheckStageLayout, UPKFail, strings.Join(problems, "; "))
		return false, "", nil
	}
	var tars []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tar") {
			tars = append(tars, filepath.Join(imagesDir, e.Name()))
		}
	}
	sort.Strings(tars)
	switch len(tars) {
	case 0:
		problems = append(problems, fmt.Sprintf("rootfs_%s/images holds no .tar; run build-upk.sh to fetch the canonical release", arch))
	case 1:
	default:
		problems = append(problems, fmt.Sprintf("rootfs_%s/images holds %d image tars (%v); UGOS loads all of them and the compose file's image reference then silently selects whichever won", arch, len(tars), tars))
	}

	var source *UPKImageSource
	sourcePath := filepath.Join(imagesDir, "image-source.json")
	raw, srcErr := os.ReadFile(sourcePath)
	if srcErr != nil {
		problems = append(problems, fmt.Sprintf("no rootfs_%s/images/image-source.json, so nothing records which digest these bytes were fetched at", arch))
	} else {
		var s UPKImageSource
		if err := json.Unmarshal(raw, &s); err != nil {
			problems = append(problems, fmt.Sprintf("image-source.json is not readable JSON: %v", err))
		} else {
			source = &s
		}
	}

	if len(problems) > 0 {
		r.add(UPKCheckStageLayout, UPKFail, strings.Join(problems, "; "))
		if len(tars) != 1 {
			return false, "", source
		}
		return false, tars[0], source
	}
	r.add(UPKCheckStageLayout, UPKPass, fmt.Sprintf("project.yaml, the derived compose wrapper and one image tar for %s, with its provenance sidecar", arch))
	return true, tars[0], source
}

// checkUPKVersion pins project.yaml's version to the canonical image tag.
// A package that advertises 0.3.0 in the App Center while carrying 0.2.0's
// bytes is not caught by any hash comparison: both halves are internally
// consistent, and the only thing wrong is what it tells the operator.
func checkUPKVersion(r *UPKReport, stage string, c Canonical) {
	raw, err := os.ReadFile(filepath.Join(stage, "project.yaml"))
	if err != nil {
		r.add(UPKCheckPackageVersion, UPKFail, fmt.Sprintf("project.yaml: %v", err))
		return
	}
	var p upkProject
	if err := yaml.Unmarshal(raw, &p); err != nil {
		r.add(UPKCheckPackageVersion, UPKFail, fmt.Sprintf("project.yaml: %v", err))
		return
	}
	var problems []string
	if p.Version != c.Image.Tag {
		problems = append(problems, fmt.Sprintf("project.yaml declares version %q and the canonical release is %q", p.Version, c.Image.Tag))
	}
	if !p.IsDockerApp {
		problems = append(problems, "project.yaml does not declare is_docker_app: true, so UGOS would not load the bundled image at all")
	}
	if len(problems) > 0 {
		r.add(UPKCheckPackageVersion, UPKFail, strings.Join(problems, "; "))
		return
	}
	r.add(UPKCheckPackageVersion, UPKPass, fmt.Sprintf("is_docker_app, version %s, the canonical release tag", p.Version))
}

// checkUPKReference holds the three places that name an image to the one
// canonical reference. The archive's own tag is the one that matters
// most: UGOS runs `docker load` and then brings the compose file up, so a
// tar tagged anything else produces a deployment that either runs the
// wrong image or does not start.
func checkUPKReference(r *UPKReport, stage string, img *imageArchive, source *UPKImageSource, c Canonical) {
	want := c.Image.Reference
	var problems []string

	if !contains(img.RepoTags, want) {
		problems = append(problems, fmt.Sprintf("the image archive is tagged %v and the canonical reference is %q", img.RepoTags, want))
	}
	if len(img.RepoTags) > 1 {
		problems = append(problems, fmt.Sprintf("the archive carries %d tags (%v); UGOS loads every one of them", len(img.RepoTags), img.RepoTags))
	}
	if source != nil && source.Reference != want {
		problems = append(problems, fmt.Sprintf("image-source.json records reference %q", source.Reference))
	}

	composePath := filepath.Join(stage, "rootfs_common", "docker-compose.yaml")
	refs, err := composeImageReferences(composePath)
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("%s: %v", rel(stage, composePath), err))
	case len(refs) == 0:
		problems = append(problems, "the compose wrapper declares no service with an image")
	default:
		for svc, ref := range refs {
			if ref != want {
				problems = append(problems, fmt.Sprintf("compose service %q runs image %q", svc, ref))
			}
		}
	}

	if len(problems) > 0 {
		r.add(UPKCheckImageReference, UPKFail, strings.Join(problems, "; "))
		return
	}
	r.add(UPKCheckImageReference, UPKPass, fmt.Sprintf("the archive, the compose wrapper and the provenance sidecar all name %s", want))
}

// checkUPKArchitecture reads the archive's image config rather than
// trusting the directory name. Hash parity proves the file is the file
// the manifest recorded and says nothing about which architecture it is
// for, so a rootfs_arm64 holding the amd64 image passes parity against an
// arm64 row only if the hashes were also swapped, and installs an image
// that cannot run either way.
func checkUPKArchitecture(r *UPKReport, arch string, img *imageArchive) {
	var problems []string
	if img.Architecture != arch {
		problems = append(problems, fmt.Sprintf("rootfs_%s holds an image whose config declares architecture %q", arch, img.Architecture))
	}
	if img.OS != "linux" {
		problems = append(problems, fmt.Sprintf("the image config declares os %q", img.OS))
	}
	if len(problems) > 0 {
		r.add(UPKCheckImageArchitecture, UPKFail, strings.Join(problems, "; "))
		return
	}
	r.add(UPKCheckImageArchitecture, UPKPass, fmt.Sprintf("the image config declares linux/%s", arch))
}

// checkUPKBinaryParity is the hard rule. Every canonical binary is
// extracted out of the archive's own layers and hashed, and every hash
// has to be the one container/release-manifest.json recorded for this
// architecture.
//
// A missing recorded hash is a FAILURE and not a deferral, and the
// difference is the point: an unpublished release still records its
// binary hashes (they come from the build, not from a push), so nothing
// legitimate produces a package this check cannot run against. A manifest
// with no row for this architecture means the package is carrying bytes
// no release ever recorded.
func checkUPKBinaryParity(r *UPKReport, arch string, img *imageArchive, c Canonical, m ReleaseManifest) {
	var row *ReleaseArchitecture
	for i := range m.Architectures {
		if m.Architectures[i].Architecture == arch {
			row = &m.Architectures[i]
			break
		}
	}
	if row == nil {
		r.add(UPKCheckBinaryContentParity, UPKFail,
			fmt.Sprintf("the release manifest records no %s build, so there is nothing this package's bytes could be the exact content of (it records %v)", arch, m.ArchitectureSet()))
		return
	}
	if len(c.Binaries) == 0 {
		r.add(UPKCheckBinaryContentParity, UPKFail, "canonical.json lists no binaries, so this check would compare nothing and pass")
		return
	}

	var problems []string
	var matched []string
	for _, binary := range c.Binaries {
		key := strings.TrimPrefix(binary, "/")
		want := row.BinarySHA256[key]
		got, found := img.Binaries[key]
		switch {
		case want == "":
			problems = append(problems, fmt.Sprintf("the release manifest records no SHA-256 for %s on %s", binary, arch))
		case !found:
			problems = append(problems, fmt.Sprintf("%s is not in the image at all", binary))
		case got != want:
			problems = append(problems, fmt.Sprintf("%s hashes to %s and the release recorded %s", binary, got, want))
		default:
			matched = append(matched, fmt.Sprintf("%s=%s", binary, got[:12]))
		}
	}
	if len(problems) > 0 {
		r.add(UPKCheckBinaryContentParity, UPKFail,
			strings.Join(problems, "; ")+" — this package would ship a build the canonical release did not produce")
		return
	}
	r.add(UPKCheckBinaryContentParity, UPKPass,
		fmt.Sprintf("every canonical binary in the archive is the %s release's own byte-for-byte (%s)", arch, strings.Join(matched, ", ")))
}

// checkUPKDigest is the corroborating half, and the half that is
// currently deferred.
//
// Four states, all of them distinguishable on purpose:
//
//   - the release records a digest and the sidecar agrees: PASS.
//   - the release records a digest and the sidecar disagrees or is
//     missing: FAIL. The package was not fetched from the release.
//   - the release records no digest and canonical.json says the image is
//     unpublished: DEFERRED, naming what would settle it.
//   - the release records no digest and canonical.json says it IS
//     published: FAIL. The two halves of that statement move together,
//     and a package built while they disagree is built against a source
//     of truth that is mid-edit.
func checkUPKDigest(r *UPKReport, arch string, source *UPKImageSource, c Canonical, m ReleaseManifest) {
	var recorded string
	found := false
	for _, a := range m.Architectures {
		if a.Architecture != arch {
			continue
		}
		found = true
		if a.RegistryDigest != nil {
			recorded = *a.RegistryDigest
		}
	}
	if !found {
		r.add(UPKCheckRegistryDigestParity, UPKFail, fmt.Sprintf("the release manifest records no %s architecture", arch))
		return
	}

	if recorded == "" {
		if c.Image.Published {
			r.add(UPKCheckRegistryDigestParity, UPKFail,
				fmt.Sprintf("canonical.json says %s is published and the release manifest records no %s registry digest; a package built against a half-written release is a package nobody can check", c.Image.Reference, arch))
			return
		}
		got := "(no sidecar)"
		if source != nil && source.Digest != "" {
			got = source.Digest
		}
		r.add(UPKCheckRegistryDigestParity, UPKDeferred,
			fmt.Sprintf("%s is cut and not pushed (canonical.json image.published false, release manifest registry_digest null for %s), so there is no published digest to compare against. This package was fetched at %s. The check binds itself the moment the push records a digest, with no edit here.",
				c.Image.Reference, arch, got))
		return
	}

	if source == nil || source.Digest == "" {
		r.add(UPKCheckRegistryDigestParity, UPKFail,
			fmt.Sprintf("the release records %s for %s and nothing records which digest this package's image was fetched at", recorded, arch))
		return
	}
	if source.Digest != recorded {
		r.add(UPKCheckRegistryDigestParity, UPKFail,
			fmt.Sprintf("this package's image was fetched at %s and the release records %s for %s", source.Digest, recorded, arch))
		return
	}
	if source.Architecture != "" && source.Architecture != arch {
		r.add(UPKCheckRegistryDigestParity, UPKFail,
			fmt.Sprintf("image-source.json records architecture %q inside rootfs_%s", source.Architecture, arch))
		return
	}
	r.add(UPKCheckRegistryDigestParity, UPKPass,
		fmt.Sprintf("fetched at %s, the digest the release records for %s", recorded, arch))
}

// ---------------------------------------------------------------------
// Reading an image archive
// ---------------------------------------------------------------------

// imageArchive is a `docker save` output, reduced to what the identity
// claim needs.
type imageArchive struct {
	RepoTags     []string
	Architecture string
	OS           string
	// Binaries maps a path without its leading slash to the SHA-256 of
	// the file at that path in the assembled root filesystem.
	Binaries map[string]string
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}

type ociIndex struct {
	Manifests []ociDescriptor `json:"manifests"`
}

type ociManifest struct {
	Config ociDescriptor   `json:"config"`
	Layers []ociDescriptor `json:"layers"`
}

type ociConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type dockerArchiveEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// smallBlobLimit is how large a blob may be and still be slurped during
// the first pass. The manifest and the config are a few kilobytes; the
// limit exists so this never reads a whole layer into memory by accident.
const smallBlobLimit = 1 << 20

// readImageArchive opens the archive twice: once for the metadata, once
// for the layers. Twice rather than once because a tar is a stream with
// no index, blobs appear in whatever order the writer chose, and the
// second pass cannot know which blobs it wants until the first has read
// the manifest. Twice rather than "read it all into memory" because these
// archives are tens of megabytes and a verifier that needs the image
// resident is a verifier that stops working on the largest release.
func readImageArchive(tarPath string, wantBinaries []string) (*imageArchive, error) {
	meta, err := readArchiveMetadata(tarPath)
	if err != nil {
		return nil, err
	}
	hashes, err := hashPathsInLayers(tarPath, meta.layers, wantBinaries)
	if err != nil {
		return nil, err
	}
	return &imageArchive{
		RepoTags:     meta.repoTags,
		Architecture: meta.config.Architecture,
		OS:           meta.config.OS,
		Binaries:     hashes,
	}, nil
}

type archiveMetadata struct {
	repoTags []string
	config   ociConfig
	// layers are archive member names, lowest first.
	layers []string
}

func readArchiveMetadata(tarPath string) (archiveMetadata, error) {
	var out archiveMetadata

	small := map[string][]byte{}
	err := walkTar(tarPath, func(hdr *tar.Header, r io.Reader) error {
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		name := path.Clean(hdr.Name)
		if hdr.Size > smallBlobLimit && !strings.HasSuffix(name, ".json") {
			return nil
		}
		if hdr.Size > smallBlobLimit {
			return nil
		}
		body, err := io.ReadAll(io.LimitReader(r, smallBlobLimit+1))
		if err != nil {
			return err
		}
		small[name] = body
		return nil
	})
	if err != nil {
		return out, err
	}

	// The OCI layout `docker save` writes today. manifest.json is the
	// older docker-archive shape and is still written beside it; it is
	// the fallback rather than the primary because it is the one Docker
	// has been deprecating, and because index.json is what carries the
	// reference annotation.
	if raw, ok := small["index.json"]; ok {
		var idx ociIndex
		if err := json.Unmarshal(raw, &idx); err != nil {
			return out, fmt.Errorf("index.json: %w", err)
		}
		if len(idx.Manifests) != 1 {
			return out, fmt.Errorf("index.json describes %d manifests; an image archive for one architecture describes exactly one", len(idx.Manifests))
		}
		desc := idx.Manifests[0]
		if name := desc.Annotations["io.containerd.image.name"]; name != "" {
			out.repoTags = append(out.repoTags, name)
		}
		manRaw, ok := small[blobPath(desc.Digest)]
		if !ok {
			return out, fmt.Errorf("index.json names manifest %s, which is not in the archive", desc.Digest)
		}
		var man ociManifest
		if err := json.Unmarshal(manRaw, &man); err != nil {
			return out, fmt.Errorf("manifest %s: %w", desc.Digest, err)
		}
		cfgRaw, ok := small[blobPath(man.Config.Digest)]
		if !ok {
			return out, fmt.Errorf("the manifest names config %s, which is not in the archive", man.Config.Digest)
		}
		if err := json.Unmarshal(cfgRaw, &out.config); err != nil {
			return out, fmt.Errorf("config %s: %w", man.Config.Digest, err)
		}
		for _, l := range man.Layers {
			out.layers = append(out.layers, blobPath(l.Digest))
		}
		return out, nil
	}

	raw, ok := small["manifest.json"]
	if !ok {
		return out, fmt.Errorf("the archive has neither index.json nor manifest.json, so it is not a docker save output")
	}
	var entries []dockerArchiveEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return out, fmt.Errorf("manifest.json: %w", err)
	}
	if len(entries) != 1 {
		return out, fmt.Errorf("manifest.json describes %d images; an image archive for one architecture describes exactly one", len(entries))
	}
	out.repoTags = entries[0].RepoTags
	cfgRaw, ok := small[path.Clean(entries[0].Config)]
	if !ok {
		return out, fmt.Errorf("manifest.json names config %s, which is not in the archive", entries[0].Config)
	}
	if err := json.Unmarshal(cfgRaw, &out.config); err != nil {
		return out, fmt.Errorf("config %s: %w", entries[0].Config, err)
	}
	for _, l := range entries[0].Layers {
		out.layers = append(out.layers, path.Clean(l))
	}
	return out, nil
}

func blobPath(digest string) string {
	return path.Clean("blobs/" + strings.Replace(digest, ":", "/", 1))
}

// hashPathsInLayers assembles the wanted paths the way a container
// runtime would: layers apply in order, a later layer's copy of a path
// replaces an earlier one, and an opaque or explicit whiteout removes it.
// Anything less would let a package pass by carrying the canonical binary
// in a lower layer and a replacement in a higher one, which is a
// divergent image that hashes correctly.
func hashPathsInLayers(tarPath string, layers []string, wantBinaries []string) (map[string]string, error) {
	want := map[string]bool{}
	for _, b := range wantBinaries {
		want[strings.TrimPrefix(b, "/")] = true
	}

	// layer member name -> its position, so an out-of-order tar still
	// applies in the right order.
	order := map[string]int{}
	for i, l := range layers {
		order[l] = i
	}
	// perLayer[i] holds this layer's verdict for each wanted path: a
	// hash, or "" for a whiteout.
	perLayer := make([]map[string]string, len(layers))

	err := walkTar(tarPath, func(hdr *tar.Header, r io.Reader) error {
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		idx, ok := order[path.Clean(hdr.Name)]
		if !ok {
			return nil
		}
		layerReader, err := maybeGunzip(r)
		if err != nil {
			return fmt.Errorf("layer %s: %w", hdr.Name, err)
		}
		found := map[string]string{}
		tr := tar.NewReader(layerReader)
		for {
			e, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("layer %s: %w", hdr.Name, err)
			}
			name := strings.TrimPrefix(path.Clean("/"+e.Name), "/")
			if base := path.Base(name); strings.HasPrefix(base, ".wh.") {
				removed := path.Join(path.Dir(name), strings.TrimPrefix(base, ".wh."))
				removed = strings.TrimPrefix(removed, "./")
				if want[removed] {
					found[removed] = ""
				}
				continue
			}
			if !want[name] || e.Typeflag != tar.TypeReg {
				continue
			}
			h := sha256.New()
			if _, err := io.Copy(h, tr); err != nil {
				return fmt.Errorf("layer %s: hashing %s: %w", hdr.Name, name, err)
			}
			found[name] = hex.EncodeToString(h.Sum(nil))
		}
		perLayer[idx] = found
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for _, layer := range perLayer {
		for name, hash := range layer {
			if hash == "" {
				delete(out, name)
				continue
			}
			out[name] = hash
		}
	}
	return out, nil
}

// maybeGunzip sniffs the two-byte gzip magic rather than trusting the
// layer's declared media type, because `docker save` writes uncompressed
// layers under OCI media types and a registry pull writes gzipped ones,
// and the same archive can be either depending on how it was produced.
func maybeGunzip(r io.Reader) (io.Reader, error) {
	var magic [2]byte
	n, err := io.ReadFull(r, magic[:])
	if err == io.EOF {
		return strings.NewReader(""), nil
	}
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	head := io.MultiReader(strings.NewReader(string(magic[:n])), r)
	if n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		return gzip.NewReader(head)
	}
	return head, nil
}

func walkTar(tarPath string, fn func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(hdr, tr); err != nil {
			return err
		}
	}
}

// composeImageReferences reads service name -> image out of a compose
// file. Deliberately its own five lines rather than a call into
// distribution/compose: this verifier runs over a STAGED tree at an
// arbitrary path, and every reader there resolves against the repository
// root.
func composeImageReferences(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, svc := range doc.Services {
		out[name] = svc.Image
	}
	return out, nil
}

func rel(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return r
	}
	return p
}
