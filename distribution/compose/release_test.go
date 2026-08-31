package compose_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/distribution/compose"
	"github.com/spdrman/rclone-manager/distribution/packaging"
)

// TestArchitecturesAgreeAcrossTheThreePlacesTheyAreWrittenDown is the
// multi-arch half of #167's contract. "The same source revision produces
// linux/amd64 and linux/arm64 images" is only checkable if the three
// records of that claim cannot drift: the runtime definition an operator
// deploys, the canonical metadata every adapter derives from, and the
// release manifest that records what was actually built.
func TestArchitecturesAgreeAcrossTheThreePlacesTheyAreWrittenDown(t *testing.T) {
	t.Parallel()

	doc := canonical(t)
	fromCompose, err := doc.Architectures()
	if err != nil {
		t.Fatalf("read architectures from the canonical runtime definition: %v", err)
	}
	fromCanonical := packaging.MustLoad().Architectures

	manifest, err := packaging.ReadReleaseManifest()
	if err != nil {
		t.Fatalf("read the release manifest: %v", err)
	}
	fromManifest := manifest.ArchitectureSet()

	norm := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, v := range in {
			out = append(out, strings.TrimPrefix(v, "linux/"))
		}
		sort.Strings(out)
		return out
	}

	fromDoc, canon, release := norm(fromCompose), norm(fromCanonical), norm(fromManifest)
	if len(fromDoc) < 2 {
		t.Fatalf("the runtime definition claims %v; #167 requires at least linux/amd64 and linux/arm64", fromCompose)
	}
	if strings.Join(fromDoc, ",") != strings.Join(canon, ",") {
		t.Errorf("the runtime definition claims %v but distribution/packaging/canonical.json says %v", fromDoc, canon)
	}
	if strings.Join(fromDoc, ",") != strings.Join(release, ",") {
		t.Errorf("the runtime definition claims %v but the release manifest records %v", fromDoc, release)
	}
}

// TestDigestPolicyPointsAtSomethingReal: "how an operator pins a release"
// is worth nothing as prose. The policy has to name the artifact the
// digests are actually recorded in, and that artifact has to parse.
func TestDigestPolicyPointsAtSomethingReal(t *testing.T) {
	t.Parallel()

	doc := canonical(t)
	policy, err := doc.DigestPolicy()
	if err != nil {
		t.Fatalf("read the digest policy: %v", err)
	}
	if policy.Manifest == "" {
		t.Fatal("the digest policy names no manifest, so an operator has nowhere to read a digest from")
	}
	// The policy has to name the manifest the parity checks actually
	// read, not merely a manifest. A policy pointing somewhere plausible
	// but unread would be prose with a file path in it.
	const readByTheParityChecks = "container/release-manifest.json"
	if policy.Manifest != readByTheParityChecks {
		t.Errorf("the digest policy names %q but every parity check in this repository reads %q", policy.Manifest, readByTheParityChecks)
	}
	if policy.Pin == "" {
		t.Error("the digest policy says nothing about how an operator pins a release")
	}
	manifest, err := packaging.ReadReleaseManifest()
	if err != nil {
		t.Fatalf("the digest policy names %q, which does not read back as a release manifest: %v", policy.Manifest, err)
	}
	if len(manifest.Architectures) == 0 {
		t.Fatalf("%s records no architecture at all", policy.Manifest)
	}
	if ok, why := manifest.RecordsEveryBinary(packaging.MustLoad().Binaries); !ok {
		t.Errorf("%s does not record every canonical binary: %s", policy.Manifest, why)
	}
}

// TestProfilesDeclaredByTheRuntimeDefinitionAreTheOnesTheBinaryImplements
// keeps `--profile=` from acquiring a value nothing implements. The
// runtime definition declares a range; the executable is the authority on
// what exists.
func TestProfilesDeclaredByTheRuntimeDefinitionAreTheOnesTheBinaryImplements(t *testing.T) {
	t.Parallel()

	doc := canonical(t)
	declared, err := doc.Profiles()
	if err != nil {
		t.Fatalf("read the declared profiles: %v", err)
	}
	if len(declared) < 2 {
		t.Fatalf("the runtime definition declares %v; #167 requires generic and ugos at a minimum", declared)
	}

	// distribution/ is its own module and may not import apps/, so the
	// authority is read as source rather than linked: the profile table
	// is a declaration, and reading a declaration is what the layer
	// boundary permits.
	implemented, err := compose.ImplementedProfiles()
	if err != nil {
		t.Fatalf("read the implemented profiles: %v", err)
	}

	sort.Strings(declared)
	sort.Strings(implemented)
	if strings.Join(declared, ",") != strings.Join(implemented, ",") {
		t.Errorf("the runtime definition declares profiles %v but the executable implements %v", declared, implemented)
	}
}
