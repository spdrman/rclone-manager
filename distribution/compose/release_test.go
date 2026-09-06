// The release-facing half of issue #167: the claims a published image
// makes about itself, checked against the places those claims are
// independently written down.
//
// Every test here is an agreement test rather than a correctness test,
// and that is deliberate. Nothing in this repository can prove that a
// linux/arm64 image exists, or that a digest names the bytes an operator
// will pull; a test run on a laptop has no registry and no builder. What
// it can prove is that the architectures, the profiles, the contract
// version, the digest policy and the health checks are not recorded in
// three places that disagree, which is the failure this actually keeps
// hitting: someone adds an architecture to the build, the canonical
// metadata still lists one, and the adapters derived from that metadata
// ship a single-arch claim to a store reviewer.
//
// So the shape of each test is: read the same fact from the runtime
// definition an operator deploys, from canonical.json, and from the
// release manifest that records what was built, then normalise and
// compare. Where a fact only lives in two of the three, the test says
// which two and why the third is silent, rather than quietly comparing
// one value with itself.
package compose_test

import (
	"os"
	"path/filepath"
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

// TestProfilesAgreeWithTheCanonicalMetadata is the third place the same
// list is written down. The runtime definition declares the range, the
// executable implements it (above), and canonical.json is what the
// adapter derivation check in distribution/packaging reads, because that
// package may not reach into this one.
//
// Three records of one list is one more than anybody wants, and the
// alternative was worse: without a copy in canonical.json the derivation
// gate would have to accept any `--profile=` value an adapter cared to
// name.
func TestProfilesAgreeWithTheCanonicalMetadata(t *testing.T) {
	t.Parallel()

	declared, err := canonical(t).Profiles()
	if err != nil {
		t.Fatalf("read the declared profiles: %v", err)
	}
	fromCanonical := packaging.MustLoad().Profiles
	if len(fromCanonical) == 0 {
		t.Fatal("canonical.json declares no profiles, so the adapter derivation gate would accept any --profile= value")
	}

	sort.Strings(declared)
	sort.Strings(fromCanonical)
	if strings.Join(declared, ",") != strings.Join(fromCanonical, ",") {
		t.Errorf("the runtime definition declares profiles %v but distribution/packaging/canonical.json says %v", declared, fromCanonical)
	}
}

// TestTheContractVersionAgreesWithTheCanonicalMetadata pins the version
// every adapter records in its derivesFrom block to the contract it
// claims to derive from.
//
// Without it, bumping runtime-contract.json's version and forgetting
// canonical.json would leave every adapter's derivesFrom matching a stale
// copy, which is a derivation gate that passes precisely when a contract
// change has not been applied.
func TestTheContractVersionAgreesWithTheCanonicalMetadata(t *testing.T) {
	t.Parallel()

	contract := compose.MustLoadContract()
	if contract.Version == "" {
		t.Fatal("runtime-contract.json declares no version")
	}
	if got := packaging.MustLoad().RuntimeContract; got != contract.Version {
		t.Errorf("runtime-contract.json is version %q and distribution/packaging/canonical.json records %q; every adapter's derivesFrom is checked against the second, so they cannot differ", contract.Version, got)
	}
}

// TestTheCanonicalDefinitionIsWhereTheHealthChecksAreDecided is the tie
// that was missing, and its absence is the whole of issue #206.
//
// The engine's health check is written down in three places:
// container/compose.yaml declares it, canonical.json restates it so
// derive.go can hold four metadata formats to it, and every adapter
// declares it again. Nothing compared the first two. #167 changed the
// canonical definition to a liveness probe in a late review commit and
// left canonical.json saying `backup-manager status`, so for three work
// packages the nine adapters derived a start gate that a fresh install
// cannot pass while every suite in the tree stayed green.
//
// The direction is not arbitrary. runtime-contract.json names
// container/compose.yaml as `canonical` and every platform's derivesFrom
// block names it as its source, so that file decides and this test says
// so by pointing the failure at the restatement.
func TestTheCanonicalDefinitionIsWhereTheHealthChecksAreDecided(t *testing.T) {
	t.Parallel()

	c := packaging.MustLoad()
	if len(c.Healthchecks.Engine) == 0 || len(c.Healthchecks.WebUI) == 0 {
		t.Fatal("canonical.json declares no per-role health checks, so the derivation gate has nothing to compare against")
	}

	doc := canonical(t)
	for _, tc := range []struct {
		role     compose.Role
		restated []string
	}{
		{compose.RoleEngine, c.Healthchecks.Engine},
		{compose.RoleWebUI, c.Healthchecks.WebUI},
	} {
		declared, ok := doc.HealthcheckTest(tc.role)
		if !ok {
			t.Errorf("the canonical runtime definition declares no %s health check, so there is nothing for canonical.json's %v to be a restatement OF", tc.role, tc.restated)
			continue
		}
		if !sameHealthTest(declared, tc.restated) {
			t.Errorf("container/compose.yaml declares the %s health check %v and distribution/packaging/canonical.json restates it as %v; the canonical definition decides, and every adapter is held to the restatement, so a difference here is a start gate nine adapters derive from a file nobody changed",
				tc.role, declared, tc.restated)
		}
	}
}

// sameHealthTest compares two compose healthcheck vectors, tolerating
// the CMD prefix being present on one side only, because that is a
// spelling and not a difference in what runs. distribution/packaging's
// derive.go tolerates the same thing for the same reason.
func sameHealthTest(a, b []string) bool {
	strip := func(in []string) []string {
		if len(in) > 0 && (in[0] == "CMD" || in[0] == "CMD-SHELL") {
			return in[1:]
		}
		return in
	}
	return strings.Join(strip(a), " ") == strings.Join(strip(b), " ")
}

// TestTheImageHealthcheckIsDeliberatelyNotTheStartGate is the other side
// of the same coin, and it is a check rather than a comment because the
// two commands being different is what makes "an adapter that declares
// nothing inherits the canonical check" false.
//
// container/Dockerfile bakes in `backup-manager status`: FR-24's
// backup-freshness verdict, the right default for a plain `docker run`
// and for the headless `daemon` command, which serves no HTTP and so has
// no liveness endpoint to ask. The canonical start gate is a liveness
// probe. An adapter that declares no engine health check therefore
// inherits the verdict, not the gate, which is why derive.go refuses
// that wherever anything waits on the engine's health.
func TestTheImageHealthcheckIsDeliberatelyNotTheStartGate(t *testing.T) {
	t.Parallel()

	c := packaging.MustLoad()
	dockerfile, err := os.ReadFile(compose.Path(filepath.Join("container", "Dockerfile")))
	if err != nil {
		t.Fatalf("read the Dockerfile: %v", err)
	}
	text := string(dockerfile)

	if len(c.Commands.ImageHealthcheck) == 0 {
		t.Fatal("canonical.json declares no imageHealthcheck, so nothing records what an adapter that declares no health check actually inherits")
	}

	// The image still reports backup freshness, and canonical.json still
	// says which command that is. Losing either silently would be a real
	// regression, and nothing else in this package would notice.
	for _, arg := range c.Commands.ImageHealthcheck {
		if !strings.Contains(text, arg) {
			t.Errorf("canonical.json records the image's HEALTHCHECK as %v and container/Dockerfile never mentions %q; FR-24's verdict is what a plain `docker run` and the headless daemon report through, and an adapter that declares nothing inherits whatever is really there", c.Commands.ImageHealthcheck, arg)
		}
	}

	// And it is not the start gate. If these two ever became the same
	// command again, "inherited" would silently start meaning "derived"
	// and the engine branch of the derivation gate would go quiet.
	if sameHealthTest(c.Commands.ImageHealthcheck, c.Healthchecks.Engine) {
		t.Errorf("the image's HEALTHCHECK and the canonical engine start gate are both %v; that makes an adapter that declares nothing look derived again, which is the shape issue #206 came out of", c.Healthchecks.Engine)
	}
}
