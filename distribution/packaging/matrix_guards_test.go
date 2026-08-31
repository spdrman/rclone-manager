package packaging

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file holds the targeted tests for the consolidated review of
// PR #179, one per finding. Nearly all of them assert that something does
// NOT happen: a cell does not go green without a byte comparison, a
// blocker does not swallow an unrelated failure, a mount with no known
// role is not skipped, a procedure with the right nouns and no commands
// does not count as coverage.
//
// A negative assertion proves nothing on its own, because a check that
// can never fire satisfies every one of them. So each test here carries
// its own positive control: the same assertion run against input that
// SHOULD trip it, failing the test if it does not. Where the control is
// missing, the test is decoration.

// tempProvider builds a providerUnderTest whose metadata root is a
// throwaway directory, expressed the way conformance.json expresses it
// (relative to the repository root) so the checks resolve it through
// Path() exactly as they do in a real run.
func tempProvider(t *testing.T, id string) (providerUnderTest, string) {
	t.Helper()
	dir := t.TempDir()
	rel := relToRepo(t, dir)
	return providerUnderTest{
		id:        id,
		spec:      Provider{Metadata: Metadata{Root: rel}},
		canonical: MustLoad(),
	}, dir
}

// relToRepo expresses an absolute path the way conformance.json does,
// relative to the repository root, so Path() resolves it. RepoRoot is
// itself relative to this package's directory, which is where `go test`
// runs, so it has to be made absolute before anything can be measured
// against it.
func relToRepo(t *testing.T, abs string) string {
	t.Helper()
	root, err := filepath.Abs(Path("."))
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		t.Fatalf("relativise %s against %s: %v", abs, root, err)
	}
	return rel
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------
// M1. Neither parity capability may go green without measuring something
// ---------------------------------------------------------------------

// TestCoreBinaryHashParity_NeedsARealByteComparison is the acceptance bar
// the review asked for by name: corrupt one recorded hash and confirm the
// cell changes outcome. The old check could not, because it never opened
// a packaged artifact.
func TestCoreBinaryHashParity_NeedsARealByteComparison(t *testing.T) {
	p, dir := tempProvider(t, "fictional")
	binary := []byte("not really a binary, but it has a SHA-256 like everything else")
	write(t, filepath.Join(dir, "payload", "backup-manager-web"), string(binary))
	p.spec.Metadata.BinaryArtifacts = map[string]string{
		"/backup-manager-web": filepath.Join("payload", "backup-manager-web"),
	}

	good := ReleaseManifest{
		Commit: "0123456789abcdef",
		Architectures: []ReleaseArchitecture{
			{Architecture: "amd64", BinarySHA256: map[string]string{"backup-manager-web": sha256Of(binary)}},
		},
	}

	// Positive control: the check CAN pass, so a refusal below means the
	// comparison ran and disagreed rather than that nothing ever agrees.
	if ok, detail := coreBinaryHashParity(p, good); !ok {
		t.Fatalf("a binary that hashes to the recorded value must satisfy the check, got: %s", detail)
	}

	corrupted := ReleaseManifest{
		Commit: "0123456789abcdef",
		Architectures: []ReleaseArchitecture{
			{Architecture: "amd64", BinarySHA256: map[string]string{"backup-manager-web": sha256Of([]byte("a different build"))}},
		},
	}
	ok, detail := coreBinaryHashParity(p, corrupted)
	if ok {
		t.Errorf("one corrupted recorded hash must change the outcome, and the check still passed")
	}
	if !strings.Contains(detail, "records for no architecture") {
		t.Errorf("the refusal should say which side disagreed, got: %s", detail)
	}
}

// TestCoreBinaryHashParity_RefusesEveryProviderThatShipsNoBinary pins the
// live state: no provider in this repository checks in a core binary, so
// no cell may be green today. This is what stops #174 landing and
// flipping all seven cells to PASS without a byte having been compared.
func TestCoreBinaryHashParity_RefusesEveryProviderThatShipsNoBinary(t *testing.T) {
	conf := MustLoadConformance()
	canonical := MustLoad()
	for _, id := range conf.ProviderIDs() {
		p := providerUnderTest{id: id, spec: conf.Providers[id], canonical: canonical}
		if len(p.spec.Metadata.BinaryArtifacts) > 0 {
			continue // a provider that DOES ship one is measured for real
		}
		ok, detail := checkCoreBinaryHashParity(p)
		if ok {
			t.Errorf("%s: passed core-binary-hash-parity while shipping no binary to hash: %s", id, detail)
		}
	}
}

// TestArchitectureParity_IsPerProvider proves the check reads this
// provider's own claim rather than a repository-wide fact, in both
// directions.
func TestArchitectureParity_IsPerProvider(t *testing.T) {
	manifest := ReleaseManifest{Architectures: []ReleaseArchitecture{
		{Architecture: "amd64", BinarySHA256: map[string]string{"backup-manager": "x", "backup-manager-web": "x"}},
		{Architecture: "arm64", BinarySHA256: map[string]string{"backup-manager": "x", "backup-manager-web": "x"}},
	}}

	p, dir := tempProvider(t, "fictional")
	rel := relToRepo(t, filepath.Join(dir, "arch.go"))
	write(t, filepath.Join(dir, "arch.go"), `var Arches = []Arch{{GOARCH: "amd64"}, {GOARCH: "arm64"}}`)
	p.spec.Metadata.ArchitectureClaim = ArchitectureClaim{Source: rel, Architectures: []string{"amd64", "arm64"}}

	// Positive control.
	if ok, detail := architectureParity(p, manifest); !ok {
		t.Fatalf("a claim the manifest builds must satisfy the check, got: %s", detail)
	}

	// A provider claiming an architecture nobody built.
	p.spec.Metadata.ArchitectureClaim.Architectures = []string{"amd64", "riscv64"}
	write(t, filepath.Join(dir, "arch.go"), `var Arches = []Arch{{GOARCH: "amd64"}, {GOARCH: "riscv64"}}`)
	if ok, detail := architectureParity(p, manifest); ok {
		t.Errorf("claiming riscv64 against an amd64/arm64 build must fail: %s", detail)
	}

	// A claim recorded in conformance.json that the named source does not
	// actually make.
	write(t, filepath.Join(dir, "arch.go"), `var Arches = []Arch{{GOARCH: "amd64"}}`)
	if ok, detail := architectureParity(p, manifest); ok {
		t.Errorf("a claim its own source does not mention must fail: %s", detail)
	}

	// And a provider with no claim of its own says so rather than
	// borrowing the repository-wide one.
	p.spec.Metadata.ArchitectureClaim = ArchitectureClaim{}
	ok, detail := architectureParity(p, manifest)
	if ok || !strings.Contains(detail, "no architecture claim of its own") {
		t.Errorf("a provider with no claim must refuse and say why, got ok=%v %s", ok, detail)
	}
}

// TestReleaseManifestIntegrity_SeparatesGitFailingFromGitSayingNo keeps
// "the commit is not an ancestor" apart from "git could not answer",
// which are different facts and must not share a blocker.
//
// It used to read those two off the real repository, because the real
// manifest really did pin an unreachable commit. That is fixed (#174), so
// the failures are now constructed instead: a manifest is cheap to build,
// and a guard that can only fire while the repository is broken stops
// being a guard the moment it is fixed.
func TestReleaseManifestIntegrity_SeparatesGitFailingFromGitSayingNo(t *testing.T) {
	conf := MustLoadConformance()
	p := providerUnderTest{id: "generic", spec: conf.Providers["generic"], canonical: MustLoad()}
	fx := newSquashMergeFixture(t)

	manifestAt := func(commit string) ReleaseManifest { return fixtureManifest(p, commit) }

	// The positive control, and it has to come first: if a manifest
	// pinning a commit the history really has cannot pass, then the two
	// refusals below prove nothing except that the check always refuses.
	if ok, detail := releaseManifestIntegrity(p, manifestAt(fx.squashed), fx.dir); !ok {
		t.Fatalf("a manifest pinning a commit HEAD can reach must pass, got: %s", detail)
	}

	// git saying no: the branch commit a squash merge rewrote away.
	ok, detail := releaseManifestIntegrity(p, manifestAt(fx.feature), fx.dir)
	if ok {
		t.Fatalf("a manifest pinning a commit that is not in this history must not conclude: %s", detail)
	}
	if strings.Contains(detail, "git could not decide") {
		t.Fatalf("git itself failed here, so this run cannot tell the two apart: %s", detail)
	}
	if !strings.Contains(detail, "is not an ancestor of main") {
		t.Errorf("expected the ancestry refusal, got: %s", detail)
	}

	// git failing to answer: a well-formed SHA that is not an object.
	// Same verdict, and it must not be reported as the same fact.
	ok, detail = releaseManifestIntegrity(p, manifestAt(unknownSHA), fx.dir)
	if ok {
		t.Fatalf("a manifest pinning a SHA git cannot resolve must not conclude: %s", detail)
	}
	if !strings.Contains(detail, "git could not decide") {
		t.Errorf("an undecidable case was reported as a decided one, which is how a broken check hides behind a known blocker: %s", detail)
	}

	// And an empty commit is its own refusal rather than either of those.
	if ok, detail := releaseManifestIntegrity(p, manifestAt(""), fx.dir); ok || !strings.Contains(detail, "pins no commit") {
		t.Errorf("a manifest with no commit at all must say so, got ok=%v %s", ok, detail)
	}
}

// TestReleaseManifest_RecordsEveryBinary is the positive control for the
// hash-completeness half.
func TestReleaseManifest_RecordsEveryBinary(t *testing.T) {
	full := ReleaseManifest{Architectures: []ReleaseArchitecture{
		{Architecture: "amd64", BinarySHA256: map[string]string{"backup-manager": "a", "backup-manager-web": "b"}},
	}}
	if ok, detail := full.RecordsEveryBinary([]string{"/backup-manager", "/backup-manager-web"}); !ok {
		t.Fatalf("a complete manifest must be accepted, got: %s", detail)
	}
	partial := ReleaseManifest{Architectures: []ReleaseArchitecture{
		{Architecture: "amd64", BinarySHA256: map[string]string{"backup-manager": "a"}},
	}}
	if ok, _ := partial.RecordsEveryBinary([]string{"/backup-manager", "/backup-manager-web"}); ok {
		t.Errorf("a manifest missing one binary's hash must be refused")
	}
	empty := ReleaseManifest{}
	if ok, _ := empty.RecordsEveryBinary([]string{"/backup-manager"}); ok {
		t.Errorf("a manifest with no architectures at all must be refused")
	}
}

// ---------------------------------------------------------------------
// M2. A capability flag only counts where a shipped artifact loads it
// ---------------------------------------------------------------------

func TestBridgeFlagsOnlyCountWhereABundleLoadsThem(t *testing.T) {
	shipped, source, err := ShippedBridgeProvider()
	if err != nil {
		t.Fatal(err)
	}
	if shipped == "" || source == "" {
		t.Fatalf("no shipped bridge could be determined (%q from %q)", shipped, source)
	}

	conf := MustLoadConformance()
	canonical := MustLoad()

	// Positive control: the provider whose bundle IS shipped is accepted,
	// so a refusal below is about reachability rather than about the
	// check never saying yes.
	if _, ok := conf.Providers[shipped]; !ok {
		t.Fatalf("the shipped bridge is %q, which is not a declared provider", shipped)
	}
	sp := providerUnderTest{id: shipped, spec: conf.Providers[shipped], canonical: canonical}
	if reached, detail := bridgeReachesAShippedArtifact(sp); !reached {
		t.Fatalf("%s is the bundle the build selects and must be reachable: %s", shipped, detail)
	}

	// Second control, and a different mechanism: Synology's bridge is
	// reached through the .spk's own payload rather than through the
	// image, so accepting it proves the check has more than one way to
	// say yes. It used to be this test's NEGATIVE case, on the grounds
	// that nothing shipped that bridge, and issue #169 made it false.
	syn := providerUnderTest{id: "synology", spec: conf.Providers["synology"], canonical: canonical}
	if on, err := BridgeDeclaresCapability(syn.bridgePath(), "embeddedWindow"); err != nil {
		t.Fatal(err)
	} else if on {
		if ok, detail := bridgeFlag("embeddedWindow")(syn); !ok {
			t.Errorf("synology embedded-window was refused, and the .spk carries and serves that bridge: %s", detail)
		}
	}

	// The negative case is UGOS, and it is the honest one: its bridge
	// opts in to four capabilities and this repository produces no
	// artifact of any kind for it, because the UPK is EPIC D's #83. A
	// flag with no artifact behind it is repository intent, and a matrix
	// that reports intent as a pass is reporting a capability nobody can
	// reach.
	ugos := providerUnderTest{id: "ugos", spec: conf.Providers["ugos"], canonical: canonical}
	on, err := BridgeDeclaresCapability(ugos.bridgePath(), "embeddedWindow")
	if err != nil {
		t.Fatal(err)
	}
	if !on {
		t.Skip("apps/ugos/frontend/platform.ts no longer opts in to embeddedWindow")
	}
	ok, detail := bridgeFlag("embeddedWindow")(ugos)
	if ok {
		t.Errorf("ugos embedded-window passed on a bridge no shipped artifact loads")
	}
	if !strings.Contains(detail, "ships no deployable artifact") {
		t.Errorf("the refusal should say that nothing is shipped at all, got: %s", detail)
	}

	// Store artifacts present plus the flag on is still not a pass while
	// nothing loads the bridge that flag describes.
	if ok, detail := checkAppStorePackaging(ugos); ok {
		t.Errorf("ugos app-store-packaging passed with an unreachable bridge: %s", detail)
	}

	// And the sharp one: an adapter that DOES select a bundle from the
	// image, but somebody else's. This is the shape a copy-pasted
	// compose file takes, it produces a container that starts and serves
	// a working-looking UI, and neither the store artifacts nor the
	// bridge flag would notice.
	wrong := SelectUIBundle(&Service{
		Name:        "backup-manager-ui",
		Command:     []string{"/backup-manager-web", "serve-ui", "--profile=truenas"},
		Environment: map[string]string{"UI_ROOT": "/ui/bundles"},
	}, UIBundleSelection{Mechanism: UIBundleNone}, "unraid")
	if wrong.Provider != "truenas" {
		t.Errorf("a Web UI selecting --profile=truenas resolved to %q; the selector reads the artifact, not the platform it was found under", wrong.Provider)
	}
}

// ---------------------------------------------------------------------
// M3. verifiedBy has to point at something that is still true
// ---------------------------------------------------------------------

func TestVerifiedByCanNameATestAndIsCheckedForIt(t *testing.T) {
	// Positive control: a real file, and a real test inside it.
	if err := VerifiedByReachable("apps/synology/spk/verify_test.go:TestVerify_BinaryHashParity"); err != nil {
		t.Fatalf("a test that exists must be accepted: %v", err)
	}
	if err := VerifiedByReachable("apps/synology/spk/verify_test.go"); err != nil {
		t.Fatalf("a plain path must still be accepted: %v", err)
	}
	// The failure the os.Stat-only guard could not see: the file is
	// there, the assertion inside it is not.
	if err := VerifiedByReachable("apps/synology/spk/verify_test.go:TestVerify_SomethingNobodyWrote"); err == nil {
		t.Errorf("naming a test that does not exist must be refused")
	}
	if err := VerifiedByReachable("apps/synology/spk/nope.go"); err == nil {
		t.Errorf("a missing file must be refused")
	}
}

// TestSynologyPortIsolationIsCheckedRatherThanExcused covers the cell
// that used to be NOT_APPLICABLE on two false statements.
func TestSynologyPortIsolationIsCheckedRatherThanExcused(t *testing.T) {
	conf := MustLoadConformance()
	syn := providerUnderTest{id: "synology", spec: conf.Providers["synology"], canonical: MustLoad()}

	// Positive control: the shipped assets satisfy it.
	if ok, detail := checkSPKPortIsolation(syn); !ok {
		t.Fatalf("the checked-in package should hold: %s", detail)
	}

	// And a package that put the engine on the LAN does not. The whole
	// asset tree is copied so the check reads a real layout.
	dir := t.TempDir()
	src := Path(filepath.Join("apps", "synology", "spk", "assets"))
	dst := filepath.Join(dir, "spk", "assets")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(dst, "scripts", "common.sh")
	body, err := os.ReadFile(common)
	if err != nil {
		t.Fatal(err)
	}
	write(t, common, strings.Replace(string(body), `ENGINE_ADDR="127.0.0.1:`, `ENGINE_ADDR="0.0.0.0:`, 1))

	rel := relToRepo(t, dir)
	broken := syn
	broken.spec.Metadata.Root = rel
	ok, detail := checkSPKPortIsolation(broken)
	if ok {
		t.Errorf("an engine bound to 0.0.0.0 must fail the isolation check")
	}
	if !strings.Contains(detail, "not a loopback address") {
		t.Errorf("the refusal should name what it saw, got: %s", detail)
	}
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

// ---------------------------------------------------------------------
// M8. A blocker excuses one failure, not every future failure
// ---------------------------------------------------------------------

func TestBlockedCellDoesNotSwallowAnUnrelatedFailure(t *testing.T) {
	cap := Capability{ID: "core-binary-hash-parity", Mode: ModeRepo}
	cell := Cell{Declared: DeclBlocked, Blocker: "#174", ExpectedDetail: "is not an ancestor of HEAD", Reason: "tracked"}

	// Positive control: the failure the blocker describes is still BLOCKED.
	r := resolveWith(ConformanceSource, "generic", cap, cell, false, "release manifest pins commit c51a07f, which is not an ancestor of HEAD, so nothing lines up")
	if r.Outcome != OutcomeBlocked {
		t.Fatalf("the declared failure must still report BLOCKED, got %s: %s", r.Outcome, r.Detail)
	}

	// A completely different failure of the same check is a FAIL, not a
	// free pass for as long as the declaration stands.
	r = resolveWith(ConformanceSource, "generic", cap, cell, false, "container/release-manifest.json: no such file or directory")
	if r.Outcome != OutcomeFail {
		t.Errorf("an unrelated failure must not be excused by the blocker, got %s: %s", r.Outcome, r.Detail)
	}
	if !strings.Contains(r.Detail, "failed for a different reason") {
		t.Errorf("the failure should say why it is not the blocker, got: %s", r.Detail)
	}

	// And the direction that already worked keeps working.
	r = resolveWith(ConformanceSource, "generic", cap, cell, true, "everything now holds")
	if r.Outcome != OutcomeFail {
		t.Errorf("a blocked cell whose check now passes must fail, got %s", r.Outcome)
	}
}

// TestEveryBlockedCellCarriesAnExpectedDetailThatMatches is the live
// version of the same rule: a substring nobody's check ever emits would
// satisfy the declaration guard while quietly turning every one of that
// cell's failures into a FAIL, so the run itself has to agree.
func TestEveryBlockedCellCarriesAnExpectedDetailThatMatches(t *testing.T) {
	conf := MustLoadConformance()
	blocked := 0
	for _, pid := range conf.ProviderIDs() {
		for id, cell := range conf.Providers[pid].Cells {
			if cell.Declared != DeclBlocked {
				continue
			}
			blocked++
			if strings.TrimSpace(cell.ExpectedDetail) == "" {
				t.Errorf("%s/%s is blocked with no expectedDetail", pid, id)
			}
		}
	}
	if blocked == 0 {
		t.Fatal("no blocked cells at all, so this test is measuring nothing")
	}
}

// ---------------------------------------------------------------------
// M9. Operator coverage has to be a step that could detect the failure
// ---------------------------------------------------------------------

func TestRemovePreservesBackupsNeedsABaselineAndAComparison(t *testing.T) {
	dir := t.TempDir()
	rel := func(name string) string { return relToRepo(t, filepath.Join(dir, name)) }

	const topicOnly = `## Step 1 — Remove

- [ ] Retained backups are intact
- [ ] Nothing else was touched
`
	const captureOnly = topicOnly + "\n```bash\nfind /backups -type f | sort > /tmp/before.txt\n```\n"
	const both = captureOnly + "\n```bash\ndiff /tmp/before.txt /tmp/after.txt\n```\n"

	cases := []struct {
		name string
		body string
		want bool
	}{
		// The bar the old single topic-word regex cleared.
		{"topic word only", topicOnly, false},
		{"captures but never compares", captureOnly, false},
		// Positive control: the check does say yes to a real procedure.
		{"captures and compares", both, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := strings.ReplaceAll(tc.name, " ", "-") + ".md"
			write(t, filepath.Join(dir, name), tc.body)
			p := providerUnderTest{id: "fictional", spec: Provider{Acceptance: rel(name)}, canonical: MustLoad()}
			ok, detail := operatorCoverage("remove-preserves-backups")(p)
			if ok != tc.want {
				t.Errorf("got ok=%v want %v: %s", ok, tc.want, detail)
			}
		})
	}
}

// ---------------------------------------------------------------------
// M10. An unrecognised mount is a finding, not a skip
// ---------------------------------------------------------------------

func TestRoleMountsRefusesAMountWithNoKnownRole(t *testing.T) {
	conf := MustLoadConformance()
	canonical := MustLoad()

	// Positive control: the real Proxmox profile still yields a full map.
	prox := providerUnderTest{id: "proxmox", spec: conf.Providers["proxmox"], canonical: canonical}
	mounts, detail := roleMounts(prox)
	if mounts == nil {
		t.Fatalf("the checked-in proxmox profile must produce a role map: %s", detail)
	}
	for _, role := range []string{"state", "backups", "config", "sshKey", "knownHosts"} {
		if _, ok := mounts[role]; !ok {
			t.Fatalf("the proxmox profile maps no %s role", role)
		}
	}

	// A profile that bind-mounts a whole directory the image has no role
	// for used to be invisible to state-persistence and backup-root
	// containment alike.
	p, dir := tempProvider(t, "fictional")
	write(t, filepath.Join(dir, "compose.yaml"), `services:
  backup-manager:
    image: `+canonical.Image.Reference+`
    command: ["/backup-manager-web", "serve"]
    volumes:
      - /srv/app/state:/data/state
      - /srv/app/backups:/data/backups
      - /srv/app/etc:/etc/backup-manager
`)
	p.spec.Metadata.Kind = "compose"
	p.spec.Metadata.Compose = "compose.yaml"

	if mounts, detail := roleMounts(p); mounts != nil {
		t.Errorf("a mount at /etc/backup-manager has no canonical role and must be refused, got %v", mounts)
	} else if !strings.Contains(detail, "not a container path the canonical image knows about") {
		t.Errorf("the refusal should say what it could not place, got: %s", detail)
	}
	if ok, _ := checkStatePersistence(p); ok {
		t.Errorf("state-persistence must not pass over an unplaceable mount")
	}
	if ok, _ := checkBackupRootContainment(p); ok {
		t.Errorf("backup-root-containment must not pass over an unplaceable mount")
	}
}

// ---------------------------------------------------------------------
// M4. The Proxmox profile fails closed on every host path
// ---------------------------------------------------------------------

// unresolvedHostPaths expands a compose file against env and reports
// which host-path variables were left unresolved. Driven through the real
// ExpandCompose rather than a regexp, so it measures what a deployment
// would actually see.
func unresolvedHostPaths(t *testing.T, compose string, env map[string]string) map[string]bool {
	t.Helper()
	_, unresolved := ExpandCompose(compose, env)
	out := map[string]bool{}
	for _, name := range unresolved {
		out[name] = true
	}
	return out
}

func TestProxmoxProfileRefusesToStartWithAnUnsetHostPath(t *testing.T) {
	path := Path(filepath.Join("apps", "proxmox", "compose", "backup-manager.yml"))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	compose := string(body)

	hostPaths := []string{"STATE_DIR", "BACKUP_DIR", "CONFIG_DIR", "KEY_FILE", "KNOWN_HOSTS_FILE"}

	// With nothing set, every host path must stop the deployment. A
	// ${VAR:-default} would quietly resolve here, which is how a bind
	// source ends up on the guest's own root disk while the stack reports
	// healthy.
	unresolved := unresolvedHostPaths(t, compose, map[string]string{})
	for _, name := range hostPaths {
		if !unresolved[name] {
			t.Errorf("%s resolves with nothing set, so an absent share would be created silently on the guest's root disk", name)
		}
	}

	// Positive control: the same assertion over a profile that uses
	// defaults has to trip, otherwise the one above proves nothing.
	loose := compose
	for _, name := range hostPaths {
		loose = regexp.MustCompile(`\$\{`+name+`:\?[^}]*\}`).ReplaceAllString(loose, "${"+name+":-/tmp/"+name+"}")
	}
	control := unresolvedHostPaths(t, loose, map[string]string{})
	for _, name := range hostPaths {
		if control[name] {
			t.Fatalf("the positive control did not trip: %s still reported unresolved with a :- default", name)
		}
	}

	// And the checked-in env file supplies every one of them, so a
	// correct deployment never sees a refusal.
	env, err := ReadEnvFile(Path(filepath.Join("apps", "proxmox", "compose", "backup-manager.env")))
	if err != nil {
		t.Fatal(err)
	}
	if left := unresolvedHostPaths(t, compose, env); len(left) > 0 {
		t.Errorf("backup-manager.env leaves %v unresolved", left)
	}
}

// ---------------------------------------------------------------------
// M5, M6, M7. The Proxmox acceptance procedure
// ---------------------------------------------------------------------

var (
	// hardcodedGuestIDRe matches a PVE command acting on a literal VMID.
	hardcodedGuestIDRe = regexp.MustCompile(`(?m)\b(qm|pct)\s+(create|start|stop|destroy|status|config|set|clone)\s+(\d{3,})`)
	// A real command line, not an inline-code mention of one: step 0.3
	// deliberately talks about `qm destroy` in prose, and the baseline
	// rules below are about what comes before the command itself.
	destroyRe = regexp.MustCompile("(?m)^[^`\n]*\\b(qm|pct)\\s+destroy\\b.*$")
)

// auditProxmoxProcedure is every rule the review's three procedure
// findings turned into, applied to text rather than to a path, so a
// deliberately broken copy can prove each rule fires.
func auditProxmoxProcedure(text string) []string {
	var findings []string

	// M5: an irreversible command must not name a guest the procedure
	// never proved it owns.
	if m := hardcodedGuestIDRe.FindStringSubmatch(text); m != nil {
		findings = append(findings, "hardcoded VMID in `"+strings.Join(m[1:], " ")+"`")
	}
	if !strings.Contains(text, "export VMID=") {
		findings = append(findings, "no VMID is defined once for the whole procedure")
	}
	if !regexp.MustCompile(`qm\s+status\s+"\$VMID"`).MatchString(text) ||
		!regexp.MustCompile(`pct status\s+"\$VMID"`).MatchString(text) {
		findings = append(findings, "the chosen VMID is never proved free with both qm status and pct status")
	}

	// Scope the baseline rules to the section the destroy lives in. A
	// capture in the update step several pages earlier is not a baseline
	// for this one, and treating it as one is the same mistake the
	// checkbox made.
	section := text
	for _, chunk := range regexp.MustCompile(`(?m)^## `).Split(text, -1) {
		if destroyRe.MatchString(chunk) {
			section = chunk
			break
		}
	}
	destroy := destroyRe.FindStringIndex(section)
	if destroy == nil {
		findings = append(findings, "no removal step at all")
		return findings
	}
	before, after := section[:destroy[0]], section[destroy[1]:]

	// M5 again: a confirm-the-id checkbox immediately above the destroy.
	confirm := regexp.MustCompile(`(?m)^- \[ \] .*\$VMID.*`)
	if !confirm.MatchString(before) {
		findings = append(findings, "nothing asks the operator to confirm the VMID before the destroy")
	}

	// M6: a baseline before the destroy, and a comparison after it.
	if !evidenceCaptureRe.MatchString(before) {
		findings = append(findings, "no baseline is captured before the destroy")
	}
	if !regexp.MustCompile(`(?i)canary`).MatchString(before) {
		findings = append(findings, "no canary is placed in the backup root before the destroy")
	}
	if !evidenceCompareRe.MatchString(after) {
		findings = append(findings, "nothing is compared against the baseline after the destroy")
	}

	// M7: ssh-keygen writes the key as whoever ran it, so a recursive
	// chown before it leaves the key unreadable by the app.
	keygen := strings.Index(text, "ssh-keygen")
	chown := strings.Index(text, "chown -R 1000:100")
	switch {
	case keygen < 0:
		findings = append(findings, "the procedure never creates an SSH key")
	case chown < 0:
		findings = append(findings, "the procedure never chowns the tree to the app's uid")
	case chown < keygen:
		findings = append(findings, "the recursive chown runs before the SSH key exists, so the app cannot read it")
	}
	if !strings.Contains(text, "chmod 600") {
		findings = append(findings, "the private key's mode is never pinned, unlike the three sibling procedures")
	}

	return findings
}

func TestProxmoxProcedureIsSafeToFollowLiterally(t *testing.T) {
	path := Path(filepath.Join("docs", "acceptance", "proxmox-ve-deployment.md"))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)

	if findings := auditProxmoxProcedure(text); len(findings) > 0 {
		t.Errorf("docs/acceptance/proxmox-ve-deployment.md: %s", strings.Join(findings, "; "))
	}

	// Positive controls. Each mutation reintroduces one of the three
	// defects the review found, and the audit has to notice.
	controls := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a hardcoded VMID comes back",
			text: strings.Replace(text, `qm stop "$VMID" && qm destroy "$VMID"`, "qm stop 9000 && qm destroy 9000", 1),
			want: "hardcoded VMID",
		},
		{
			name: "the baseline is dropped",
			text: regexp.MustCompile(`(?s)\*\*Capture the baseline first.*?Confirm the id`).ReplaceAllString(text, "Confirm the id"),
			want: "no baseline is captured before the destroy",
		},
		{
			name: "the chown moves back ahead of the key",
			text: "sudo chown -R 1000:100 /mnt/backup-manager\n" + text,
			want: "recursive chown runs before the SSH key exists",
		},
	}
	for _, c := range controls {
		t.Run(c.name, func(t *testing.T) {
			findings := auditProxmoxProcedure(c.text)
			if !strings.Contains(strings.Join(findings, "; "), c.want) {
				t.Errorf("the audit did not notice %q; it found %v", c.want, findings)
			}
		})
	}
}

// ---------------------------------------------------------------------
// M11. No document may claim a capability the matrix does not pass
// ---------------------------------------------------------------------

// gateItemPhrases map a capability to the words a document uses when it
// claims that capability is checked automatically.
var gateItemPhrases = map[string]*regexp.Regexp{
	"core-binary-hash-parity":    regexp.MustCompile(`(?i)hash parity|version/hash`),
	"release-manifest-integrity": regexp.MustCompile(`(?i)release manifest`),
	"architecture-parity":        regexp.MustCompile(`(?i)\barchitecture\b`),
	"backup-root-containment":    regexp.MustCompile(`(?i)backup-root containment`),
	"auth-mode-explicit":         regexp.MustCompile(`(?i)auth mode`),
	"no-bundled-secrets":         regexp.MustCompile(`(?i)no bundled secrets`),
	"no-provider-lifecycle":      regexp.MustCompile(`(?i)no provider-specific lifecycle`),
	"package-metadata":           regexp.MustCompile(`(?i)provider package metadata`),
}

// disclaimerRe is how a row says the opposite of a claim. The gate table
// in docs/acceptance/README.md carries one: #174's review would not let
// the table fold hash parity into version parity, so the hash row states
// in as many words that nothing here measures it. Reading that row as an
// overstatement, on the strength of the package name appearing in the
// same cell, is the audit being naive rather than the row being wrong.
//
// It is a marker, not an escape hatch, because matrixPasses is consulted
// in both directions below: a row that disclaims a capability the matrix
// does pass somewhere is drift too, and gets reported as such.
var disclaimerRe = regexp.MustCompile(`(?i)\*\*not claimed\*\*`)

// matrixPasses counts, per capability, how many providers the matrix
// passes it for.
func matrixPasses(m *Matrix) map[string]int {
	passes := map[string]int{}
	for _, byCap := range m.Results {
		for id, r := range byCap {
			if r.Outcome == OutcomePass {
				passes[id]++
			}
		}
	}
	return passes
}

// splitGateRow splits a markdown table row into the gate item it names and
// the verdict it gives that item. Matching a capability against the whole
// line conflated the two: the hash-parity row's verdict mentions "per
// binary per architecture", so the row read as a claim about architecture
// parity as well as about its own subject. The item is decided by the
// first cell, the verdict by the rest, and a line that is not a table row
// is its own verdict and its own subject.
func splitGateRow(line string) (item, verdict string) {
	if !strings.HasPrefix(strings.TrimSpace(line), "|") {
		return line, line
	}
	cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
	if len(cells) < 2 {
		return line, line
	}
	return cells[0], strings.Join(cells[1:], "|")
}

// auditProseAgainstMatrix finds lines that tell a reader a gate item is
// checked by distribution/packaging when the matrix passes that capability
// for nobody, and lines that disclaim a gate item the matrix does pass.
// The matrix exists to be the single generated answer to "which capability
// holds where", and a hand-written table in the directory it is linked
// from is the more prominent of the two, so it may not overstate or
// understate what the generated answer says.
func auditProseAgainstMatrix(text string, m *Matrix) []string {
	passes := matrixPasses(m)
	var findings []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "distribution/packaging") {
			continue
		}
		item, verdict := splitGateRow(line)
		disclaimed := disclaimerRe.MatchString(verdict)
		for id, phrase := range gateItemPhrases {
			if !phrase.MatchString(item) {
				continue
			}
			switch {
			case !disclaimed && passes[id] == 0:
				findings = append(findings, fmt.Sprintf("claims %s is checked by distribution/packaging, and the matrix passes it for no provider: %q", id, strings.TrimSpace(line)))
			case disclaimed && passes[id] > 0:
				findings = append(findings, fmt.Sprintf("says %s is not claimed, and the matrix passes it for %d provider(s): %q", id, passes[id], strings.TrimSpace(line)))
			}
		}
	}
	sort.Strings(findings)
	return findings
}

func TestAcceptanceReadmeDoesNotContradictTheMatrix(t *testing.T) {
	conf := MustLoadConformance()
	canonical := MustLoad()
	m := NewMatrix(conf)
	for _, pid := range conf.ProviderIDs() {
		put := providerUnderTest{id: pid, spec: conf.Providers[pid], canonical: canonical}
		for _, cap := range conf.Capabilities {
			m.Record(resolve(put, cap, conf.Providers[pid].Cells[cap.ID]))
		}
	}

	path := Path(filepath.Join("docs", "acceptance", "README.md"))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if findings := auditProseAgainstMatrix(string(body), m); len(findings) > 0 {
		t.Errorf("docs/acceptance/README.md: %s", strings.Join(findings, "; "))
	}
	if !strings.Contains(string(body), "../conformance/phase-4-matrix.md") {
		t.Errorf("docs/acceptance/README.md does not point at the generated matrix")
	}

	// Positive control: the row this PR's review found, put back.
	control := "| core version/hash parity | `distribution/packaging` (canonical image reference is identical across all three platforms) |"
	if findings := auditProseAgainstMatrix(control, m); len(findings) == 0 {
		t.Errorf("the audit did not notice the contradicting row it exists to catch")
	}

	// The disclaimer is narrow. A row has to say **not claimed** to be
	// read as one; "not" appearing anywhere in the verdict is the ordinary
	// prose of a row that is still claiming coverage.
	nearMiss := "| core version/hash parity | `distribution/packaging`, though not for every platform yet |"
	if findings := auditProseAgainstMatrix(nearMiss, m); len(findings) == 0 {
		t.Errorf("a row that merely contains the word \"not\" was treated as a disclaimer; only **not claimed** is one")
	}

	// And it is symmetric, so it cannot be used to mute the audit: a row
	// that disclaims a capability the matrix does pass is drift the other
	// way round, and has to be reported too. Pick the subject from the
	// matrix rather than hardcoding it, so the control cannot go vacuous
	// if a capability's outcomes change.
	passes := matrixPasses(m)
	if passes["architecture-parity"] == 0 {
		t.Fatal("the matrix passes architecture parity for no provider, so the understatement control below would pass vacuously")
	}
	understated := "| architecture | **not claimed**. Nothing in `distribution/packaging` compares an architecture set. |"
	if findings := auditProseAgainstMatrix(understated, m); len(findings) == 0 {
		t.Errorf("the audit did not notice a row disclaiming a capability the matrix passes; the disclaimer would then be a mute button")
	}

	// The honest row #174's review forced into the gate table: it names
	// the capability, names this package, and states that nothing here
	// measures it. That is the opposite of the claim the audit hunts, and
	// reading it as one is what made these two guards look incompatible.
	honest := "| core binary hash parity | **not claimed**. Nothing in `distribution/packaging` derives a hash from any artifact. |"
	if findings := auditProseAgainstMatrix(honest, m); len(findings) > 0 {
		t.Errorf("the audit read an explicit disclaimer as a claim: %s", strings.Join(findings, "; "))
	}
}
