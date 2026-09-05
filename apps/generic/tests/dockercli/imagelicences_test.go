// This file is issue #407's delivered-form evidence: the licence, the
// notices and the third-party inventory are copied back OUT of a real
// built image and compared byte for byte with the checked-in files.
//
// Everything else about #407 reads container/Dockerfile.
// distribution/packaging's TestTheImageCarriesTheLicenceMaterials refuses
// a missing, builder-stage or relative COPY, and it is a good check that
// cannot answer the question the issue actually asks: "proven against a
// built image (or its layer listing) rather than against the Dockerfile
// alone". A Dockerfile check believes the Dockerfile. It cannot see a
// .dockerignore that empties a COPY's source, a base image that puts
// something else at the same path, or a build argument that skips a
// stage. This can, because it opens the image.
//
// It lives here rather than in distribution/packaging for the reason
// dockercli_test.go's own header gives: nothing in distribution/ builds
// an image, this package already builds container/Dockerfile once per
// test process, and the local gate refuses to start at all without a
// reachable Docker daemon (scripts/ci-local.sh's gate_require_docker), so
// requireDocker's t.Skip cannot silently swallow this on a full run. On a
// run that opts out with CI_LOCAL_SKIP_DOCKER=1 the gate ledgers the
// skip and ends INCOMPLETE, which is the loud version of the same thing.
//
// What it does NOT prove: that the published multi-architecture image
// carries them. This builds one architecture, natively, from the working
// tree. Architecture parity is the release manifest's job
// (scripts/release/verify-manifest-parity.sh), and the licence files are
// architecture-independent bytes copied from the build context, which is
// the one part of the image where "it is the same on both" is a claim
// about a COPY rather than about a compiler.
package dockercli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
)

// licenceDeclaration is the part of
// distribution/packaging/compliance.json this test needs: which files
// are the licence materials, and where the image is declared to put
// them.
//
// Read as JSON rather than imported from distribution/packaging, because
// apps/generic's module replaces core/ and apps/common/ and nothing else
// (see its go.mod), and adding a third replace so a test can borrow two
// struct fields would make every build of this module depend on the
// distribution module. The file is the interface either way.
//
// Reading the declaration at all, rather than listing the paths here, is
// the point: distribution/packaging pins the declaration to what
// container/Dockerfile does, this pins it to what the built image does,
// and a path that moves has to move in all three or one of the two
// tests goes red.
type licenceDeclaration struct {
	License struct {
		File       string `json:"file"`
		NoticeFile string `json:"noticeFile"`
		Inventory  string `json:"inventory"`
	} `json:"license"`
	Distribution struct {
		LicenceDelivery struct {
			ImagePaths map[string]string `json:"imagePaths"`
			Labels     map[string]string `json:"labels"`
		} `json:"licenceDelivery"`
	} `json:"distribution"`
}

func readLicenceDeclaration(t *testing.T, root string) licenceDeclaration {
	t.Helper()
	path := filepath.Join(root, "distribution", "packaging", "compliance.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	var d licenceDeclaration
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("cannot parse %s: %v", path, err)
	}
	materials := []string{d.License.File, d.License.NoticeFile, d.License.Inventory}
	for _, rel := range materials {
		if strings.TrimSpace(rel) == "" {
			t.Fatalf("%s declares an empty licence material in %v, so this test would look for nothing", path, materials)
		}
		if _, ok := d.Distribution.LicenceDelivery.ImagePaths[rel]; !ok {
			t.Fatalf("%s declares %s as a licence material and distribution.licenceDelivery.imagePaths says nothing about where the image puts it, so there is no path to check", path, rel)
		}
	}
	return d
}

func (d licenceDeclaration) materials() []string {
	return []string{d.License.File, d.License.NoticeFile, d.License.Inventory}
}

// TestTheBuiltImageCarriesTheLicenceMaterials is #407's first acceptance
// criterion, against the thing a recipient actually receives.
//
// The image has no shell, so there is nothing to run inside it that
// could list a directory. `docker create` plus `docker cp` reads the
// container's filesystem from outside, without starting it, which is
// also exactly the recipe docs/compliance/source-offer.md gives a
// recipient: if this stops working the documented instructions have
// stopped working too.
func TestTheBuiltImageCarriesTheLicenceMaterials(t *testing.T) {
	image := buildImage(t)
	root := repoRoot(t)
	decl := readLicenceDeclaration(t, root)

	container := createContainer(t, image)
	dest := t.TempDir()

	for _, rel := range decl.materials() {
		inImage := decl.Distribution.LicenceDelivery.ImagePaths[rel]
		local := filepath.Join(dest, filepath.Base(inImage))
		out, err := exec.Command("docker", "cp", container+":"+inImage, local).CombinedOutput()
		if err != nil {
			t.Errorf("the image does not carry %s at %s: docker cp said %v\n%s\n\nThat is the whole of #407: a recipient whose only copy is this image receives the binaries and not the licence, the attribution or the inventory. container/Dockerfile's runtime stage has to COPY it, and .dockerignore has to let its source through.", rel, inImage, err, out)
			continue
		}
		got, err := os.ReadFile(local)
		if err != nil {
			t.Errorf("copied %s out of the image and could not read it back: %v", inImage, err)
			continue
		}
		want, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("cannot read %s out of the tree: %v", rel, err)
		}
		if sha256.Sum256(got) != sha256.Sum256(want) {
			t.Errorf("%s in the image is not the checked-in file: %s in the image against %s in the tree (%d bytes against %d). Something between the build context and the image is rewriting it, so what a recipient reads is not what the gate checked.",
				inImage, digest(got), digest(want), len(got), len(want))
		}
	}

	// The positive control for the method. Every assertion above rests
	// on `docker cp` failing when a path is not there; if it succeeded
	// on anything, the loop would report a green image regardless of
	// what the Dockerfile copied.
	missing := filepath.Join(dest, "no-such-file")
	if out, err := exec.Command("docker", "cp", container+":/licenses/there-is-no-such-file", missing).CombinedOutput(); err == nil {
		t.Errorf("docker cp of a path that does not exist in the image succeeded (%s); every check above is then vacuous", out)
	}
}

// TestTheBuiltImageAnswersDockerInspectAboutItsLicence is #407's GREEN
// item about the OCI label, against the built image rather than the
// LABEL line.
//
// A recipient with the image and no shell in it has `docker inspect` and
// little else. The licence id alone is half an answer, so the path label
// beside it has to name a directory that really holds the materials, and
// this reads the label off the image and then copies that directory out
// to see.
func TestTheBuiltImageAnswersDockerInspectAboutItsLicence(t *testing.T) {
	image := buildImage(t)
	root := repoRoot(t)
	decl := readLicenceDeclaration(t, root)

	declared := decl.Distribution.LicenceDelivery.Labels
	if len(declared) == 0 {
		t.Fatal("compliance.json declares no distribution.licenceDelivery.labels, so there is nothing to ask the image about")
	}
	var keys []string
	for k := range declared {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		out, err := exec.Command("docker", "image", "inspect", "--format",
			"{{index .Config.Labels \""+k+"\"}}", image).Output()
		if err != nil {
			t.Fatalf("docker image inspect for %s: %v", k, err)
		}
		if got := strings.TrimSpace(string(out)); got != declared[k] {
			t.Errorf("the built image answers %s=%q and compliance.json declares %q", k, got, declared[k])
		}
	}

	// And the directory the label names really is where they are. This
	// copies the whole directory rather than the files one by one, which
	// is what source-offer.md tells a recipient to do.
	dir := declared["com.iasbuilt.backupmanager.licenses.path"]
	if dir == "" {
		t.Fatal("compliance.json declares no licences path label, so `docker inspect` tells a recipient the licence id and not where to read it")
	}
	container := createContainer(t, image)
	dest := t.TempDir()
	if out, err := exec.Command("docker", "cp", container+":"+dir, filepath.Join(dest, "licenses")).CombinedOutput(); err != nil {
		t.Fatalf("the image labels %s as its licences directory and copying it out failed: %v\n%s", dir, err, out)
	}
	entries, err := os.ReadDir(filepath.Join(dest, "licenses"))
	if err != nil {
		t.Fatalf("cannot list the copied licences directory: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
	}
	for _, rel := range decl.materials() {
		base := filepath.Base(decl.Distribution.LicenceDelivery.ImagePaths[rel])
		if !present[base] {
			t.Errorf("the labelled licences directory %s holds %v and not %s, so `docker cp <ctr>:%s .` does not hand a recipient everything the offer points them at", dir, keysOf(present), base, dir)
		}
	}
}

// createContainer makes a stopped container from image so its filesystem
// can be read with `docker cp`, and removes it whatever the test does.
//
// Created, never started: `docker create` gives a filesystem without a
// process, which is all `docker cp` needs and is what
// docs/compliance/source-offer.md tells a recipient to do.
//
// The command is named and never runs. container/Dockerfile deliberately
// sets no ENTRYPOINT and no CMD (it ships two binaries and either would
// have to pick one), and `docker create` with neither refuses outright
// with "no command specified", so the argv is here to satisfy that check
// rather than to do anything. `version` is the harmless one to name.
func createContainer(t *testing.T, image string) string {
	t.Helper()
	requireDocker(t)
	dockerlease.Sweep()
	name := "backup-manager-licences-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) +
		"-" + time.Now().Format("150405.000000")
	out, err := exec.Command("docker", "create",
		"--name", name,
		dockerlease.LabelFlag, dockerlease.LabelSpec,
		image, "/backup-manager", "version",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker create: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	return name
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func keysOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
