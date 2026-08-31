package packaging

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// This file is issue #86 (B4.5)'s half of the Phase 4 TDD Gate: one
// conformance run across every provider this matrix declares, reporting
// pass/fail per capability rather than a single opaque result. Six of
// those columns are the Phase 4 Exit Gate's own; the UGOS column is EPIC
// D's #83 (D1.2), run and reported here on the same terms and gated
// there, which is what Provider.Epic records.
//
// The shape is the design work, not the checks. Work package 4.3 already
// made most of the individual rules executable (conformance_test.go), and
// a Go suite of that kind answers exactly one question per run: did
// anything break. §63A asks for something a run cannot answer that way:
//
//	The conformance suite SHALL distinguish
//	  SUPPORTED / UNSUPPORTED / NOT_APPLICABLE
//	rather than silently skipping missing provider features.
//
// The providers sit at three different §4A support tiers, so a Tier C
// deployment profile legitimately does not do what a Tier A integration
// does, and a suite that only reported failures would say nothing at all
// about those cells. Saying nothing is the dangerous outcome: a
// capability missing from a result set reads as passing.
//
// So conformance.json declares an outcome for every provider and every
// capability up front, and this runner agrees or disagrees with each
// declaration one cell at a time. Six outcomes, and the three that are
// not pass/fail carry as much weight as the two that are:
//
//	PASS              the check ran here and held
//	FAIL              the check ran here and did not hold
//	UNSUPPORTED       the provider does not have this at its §4A tier
//	NOT_APPLICABLE    the provider expresses the guarantee elsewhere
//	BLOCKED           the check is right and cannot conclude today (#174, #83)
//	PENDING_OPERATOR  the §68 procedure exists; the hardware run has not happened
//
// Two guards keep the declarations honest, and they matter more than any
// individual check:
//
//  1. completeness. A provider that omits a capability fails. Omission is
//     the exact failure mode §63A names, so it cannot be spelled.
//  2. staleness. Every declaration that is NOT "supported" still has its
//     check run, and a check that passes against an unsupported,
//     not-applicable or blocked declaration fails the cell. Without that,
//     this file would slowly become a list of excuses for checks nobody
//     runs.

// ---------------------------------------------------------------------
// The provider under test
// ---------------------------------------------------------------------

type providerUnderTest struct {
	id        string
	spec      Provider
	canonical Canonical
}

func (p providerUnderTest) bridgePath() string {
	return Path(filepath.Join("apps", p.id, "frontend", "platform.ts"))
}

// services reads whatever metadata format this provider uses into the one
// shared Service shape. A provider with no package at all returns
// nothing, which is what makes every service-based check correctly refuse
// to conclude for it rather than passing vacuously.
func (p providerUnderTest) services() ([]Service, error) {
	md := p.spec.Metadata
	root := Path(md.Root)

	switch md.Kind {
	case "compose":
		var env map[string]string
		if md.Env != "" {
			var err error
			env, err = ReadEnvFile(filepath.Join(root, md.Env))
			if err != nil {
				return nil, err
			}
		}
		return ReadCompose(filepath.Join(root, md.Compose), env)

	case "unraid-template":
		var out []Service
		for _, f := range md.Templates {
			tpl, err := ReadUnraidTemplate(filepath.Join(root, f))
			if err != nil {
				return nil, err
			}
			out = append(out, tpl.AsService(f))
		}
		return out, nil

	case "spk", "none":
		return nil, nil
	}
	return nil, fmt.Errorf("unknown metadata kind %q", md.Kind)
}

// scan runs one of the structural scanners over every declared scan root
// and reports whether the tree is clean. A root holding no files at all
// counts as dirty: a scanner that walks nothing finds nothing, and
// "found nothing" must never be how a provider passes.
func (p providerUnderTest) scan(fn func(string) ([]Violation, error)) (bool, string) {
	if len(p.spec.ScanRoots) == 0 {
		return false, "no scan root declared"
	}
	var all []Violation
	files := 0
	for _, root := range p.spec.ScanRoots {
		abs := Path(root)
		n, err := countFiles(abs)
		if err != nil {
			return false, fmt.Sprintf("%s: %v", root, err)
		}
		files += n
		v, err := fn(abs)
		if err != nil {
			return false, fmt.Sprintf("%s: %v", root, err)
		}
		all = append(all, v...)
	}
	if files == 0 {
		return false, "scan roots hold no files, so a clean result would prove nothing"
	}
	if len(all) > 0 {
		return false, fmt.Sprintf("%d finding(s): %s", len(all), oneLine(all))
	}
	return true, fmt.Sprintf("%d file(s) scanned, clean", files)
}

func countFiles(root string) (int, error) {
	n := 0
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	return n, err
}

func oneLine(v []Violation) string {
	parts := make([]string, 0, len(v))
	for _, x := range v {
		parts = append(parts, x.String())
	}
	if len(parts) > 3 {
		parts = append(parts[:3], fmt.Sprintf("and %d more", len(v)-3))
	}
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------
// The checks, one per capability
// ---------------------------------------------------------------------

// capabilityChecks maps a capability id to the check that decides it.
// TestEveryCapabilityHasACheck pins this map's key set to
// conformance.json's capability list in both directions, so a capability
// declared with nothing behind it, or a check nobody declares, is a
// failure rather than a quiet gap.
var capabilityChecks = map[string]func(providerUnderTest) (bool, string){
	"provider-identity":               checkProviderIdentity,
	"package-metadata":                checkPackageMetadata,
	"canonical-image-parity":          checkCanonicalImageParity,
	"release-manifest-integrity":      checkReleaseManifestIntegrity,
	"core-binary-hash-parity":         checkCoreBinaryHashParity,
	"architecture-parity":             checkArchitectureParity,
	"state-persistence":               checkStatePersistence,
	"backup-root-containment":         checkBackupRootContainment,
	"auth-mode-explicit":              checkAuthModeExplicit,
	"no-bundled-secrets":              func(p providerUnderTest) (bool, string) { return p.scan(ScanSecrets) },
	"no-provider-lifecycle":           func(p providerUnderTest) (bool, string) { return p.scan(ScanLifecycle) },
	"api-path-isolation":              checkAPIPathIsolation,
	"provider-removal-preserves-core": checkProviderRemovalPreservesCore,
	"host-management-plane-untouched": checkHostManagementPlane,
	"install-update-remove":           operatorCoverage("install-update-remove"),
	"ui-launch":                       operatorCoverage("ui-launch"),
	"upgrade-preserves-state":         operatorCoverage("upgrade-preserves-state"),
	"remove-preserves-backups":        operatorCoverage("remove-preserves-backups"),
	"native-auth":                     bridgeFlag("nativeAuth"),
	"native-notifications":            bridgeFlag("nativeNotifications"),
	"embedded-window":                 bridgeFlag("embeddedWindow"),
	"app-store-packaging":             checkAppStorePackaging,
	"storage-picker":                  bridgeFlag("storagePicker"),
}

func checkProviderIdentity(p providerUnderTest) (bool, string) {
	data, err := os.ReadFile(p.bridgePath())
	if err != nil {
		return false, err.Error()
	}
	text := string(data)
	for _, want := range []string{`id: "` + p.id + `"`, "name:", "integration:", "deployment:", "storageMount:", "adapterVersion:"} {
		if !strings.Contains(text, want) {
			return false, fmt.Sprintf("the bridge declares no %s", strings.TrimSuffix(want, ":"))
		}
	}
	return true, "bridge declares id, name, integration and a deployment block"
}

func checkPackageMetadata(p providerUnderTest) (bool, string) {
	files := p.spec.Metadata.Files
	switch p.spec.Metadata.Kind {
	case "compose":
		files = append(append([]string(nil), files...), p.spec.Metadata.Compose)
	case "unraid-template":
		files = append(append([]string(nil), files...), p.spec.Metadata.Templates...)
	}
	if len(files) == 0 {
		return false, "no packaging metadata declared for this provider"
	}
	for _, f := range files {
		info, err := os.Stat(Path(filepath.Join(p.spec.Metadata.Root, f)))
		if err != nil {
			return false, fmt.Sprintf("missing %s: %v", f, err)
		}
		if info.Size() == 0 {
			return false, fmt.Sprintf("%s is empty", f)
		}
	}
	return true, fmt.Sprintf("%d metadata file(s) present", len(files))
}

func checkCanonicalImageParity(p providerUnderTest) (bool, string) {
	svcs, err := p.services()
	if err != nil {
		return false, err.Error()
	}
	if len(svcs) == 0 {
		return false, "this provider declares no container services, so there is no image reference to compare"
	}
	for _, s := range svcs {
		if s.Image != p.canonical.Image.Reference {
			return false, fmt.Sprintf("service %q uses %q, want the canonical %q", s.Name, s.Image, p.canonical.Image.Reference)
		}
		if len(s.UnresolvedVars) > 0 {
			return false, fmt.Sprintf("service %q leaves %v unresolved", s.Name, s.UnresolvedVars)
		}
	}
	return true, fmt.Sprintf("%d service(s) on %s", len(svcs), p.canonical.Image.Reference)
}

// checkReleaseManifestIntegrity is the repository-wide half of the old
// "core version/hash parity" line: is container/release-manifest.json a
// description of a build anyone can reach at all?
//
// It is deliberately NOT per-provider, and it is named for what it
// decides rather than for what a reader hopes it decides. It reads no
// provider metadata, returns the same verdict for all seven columns, and
// compares no bytes. The per-provider claim, that the binaries a provider
// SHIPS are the binaries this manifest recorded, is
// core-binary-hash-parity below, and splitting the two is the whole
// point: one row that is true repository-wide can no longer stand in for
// seven answers nobody measured.
//
// It could not conclude while #174 was open: the manifest pinned a
// feature-branch commit that a squash merge had rewritten out of the
// history, so there was nothing real on the other side of any
// comparison. The manifest now pins a commit that is on main, and
// TestReleaseManifestPinsACommitThisHistoryCanReach asks the same
// question a second time, outside the declaration machinery, so that
// re-declaring this cell blocked cannot make the fact go away.
func checkReleaseManifestIntegrity(p providerUnderTest) (bool, string) {
	manifest, err := ReadReleaseManifest()
	if err != nil {
		return false, err.Error()
	}
	return releaseManifestIntegrity(p, manifest, Path("."))
}

// releaseManifestIntegrity is the body, taking the manifest and the
// repository to resolve it against as arguments for the same reason
// coreBinaryHashParity and architectureParity do: a check whose refusals
// can only be observed by breaking the real repository is a check whose
// refusals are never observed at all.
func releaseManifestIntegrity(p providerUnderTest, manifest ReleaseManifest, repoDir string) (bool, string) {
	if manifest.Commit == "" {
		return false, "the release manifest pins no commit"
	}
	reachable, err := CommitReachableFrom(repoDir, manifest.Commit, "HEAD")
	if err != nil {
		return false, fmt.Sprintf("git could not decide whether %s is an ancestor of HEAD: %v", short(manifest.Commit), err)
	}
	if !reachable {
		return false, fmt.Sprintf("release manifest pins commit %s, which is not an ancestor of HEAD, so its hashes describe a build that is not in this history", short(manifest.Commit))
	}
	if ok, detail := manifest.RecordsEveryBinary(p.canonical.Binaries); !ok {
		return false, detail
	}
	claimed := append([]string(nil), p.canonical.Architectures...)
	sort.Strings(claimed)
	if strings.Join(claimed, ",") != strings.Join(manifest.ArchitectureSet(), ",") {
		return false, fmt.Sprintf("canonical.json claims %v, the release manifest records %v", claimed, manifest.ArchitectureSet())
	}
	return true, fmt.Sprintf("manifest commit %s is reachable and records every binary on %v", short(manifest.Commit), manifest.ArchitectureSet())
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// checkCoreBinaryHashParity is §3.7's one-binary rule, and it is the one
// check in this file that is allowed to go green ONLY after a byte
// comparison has actually happened.
//
// The rule is that the binaries a provider ships are the binaries
// container/release-manifest.json recorded. Deciding that needs two
// things in the same place: a recorded hash, and a file to hash. Reading
// the manifest gives you the first; the second has to be a real artifact,
// and no provider in this repository checks one in. So a provider that
// declares no binaryArtifacts is refused here rather than passing on the
// manifest alone, which is what the old single check did for all seven
// columns without ever opening a packaged file.
//
// Synology is the case that proves the shape rather than the exception to
// it: spkctl verify really does re-derive each binary's SHA-256 out of a
// finished .spk. It runs in apps/synology's own module, behind the §7.1
// boundary scripts/architecture/*.sh enforces, so the matrix records that
// cell against the test that does the work instead of executing it here.
func checkCoreBinaryHashParity(p providerUnderTest) (bool, string) {
	manifest, err := ReadReleaseManifest()
	if err != nil {
		return false, err.Error()
	}
	return coreBinaryHashParity(p, manifest)
}

func coreBinaryHashParity(p providerUnderTest, manifest ReleaseManifest) (bool, string) {
	artifacts := p.spec.Metadata.BinaryArtifacts
	if len(artifacts) == 0 {
		return false, "this provider checks in no core binary, so there is no second copy of the bytes here to hash against the release manifest"
	}
	root := Path(p.spec.Metadata.Root)
	names := make([]string, 0, len(artifacts))
	for binary := range artifacts {
		names = append(names, binary)
	}
	sort.Strings(names)
	for _, binary := range names {
		file := artifacts[binary]
		got, err := SHA256File(filepath.Join(root, file))
		if err != nil {
			return false, fmt.Sprintf("cannot hash %s: %v", file, err)
		}
		recorded := manifest.HashesFor(binary)
		if len(recorded) == 0 {
			return false, fmt.Sprintf("the release manifest records no hash for %s at all", binary)
		}
		matched := ""
		for arch, want := range recorded {
			if want == got {
				matched = arch
				break
			}
		}
		if matched == "" {
			return false, fmt.Sprintf("%s hashes to %s, which the manifest records for no architecture (%v)", file, short(got), sortedKeys(recorded))
		}
	}
	return true, fmt.Sprintf("%d shipped binary/binaries hash to what the manifest recorded", len(artifacts))
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkArchitectureParity is §40's "the architectures a package claims are
// the architectures that were built", decided against THIS provider's own
// claim.
//
// It used to read container/release-manifest.json against canonical.json
// and return the same answer for six providers, which made a green
// Synology cell read as "apps/synology/spk/arch.go's DSM architecture
// table was checked against the release build" when nothing of the kind
// had happened. The repository-wide half of that comparison now lives in
// checkReleaseManifestIntegrity; what is left here is per-provider, and a
// provider that makes no architecture claim of its own says so instead of
// borrowing one.
func checkArchitectureParity(p providerUnderTest) (bool, string) {
	manifest, err := ReadReleaseManifest()
	if err != nil {
		return false, err.Error()
	}
	return architectureParity(p, manifest)
}

func architectureParity(p providerUnderTest, manifest ReleaseManifest) (bool, string) {
	claim := p.spec.Metadata.ArchitectureClaim
	if claim.Source == "" || len(claim.Architectures) == 0 {
		return false, "this provider makes no architecture claim of its own: it consumes the multi-arch canonical image by reference, and the repository-wide claim is release-manifest-integrity's"
	}
	source, err := os.ReadFile(Path(claim.Source))
	if err != nil {
		return false, fmt.Sprintf("the declared architecture claim source %s is unreadable: %v", claim.Source, err)
	}
	for _, arch := range claim.Architectures {
		if !strings.Contains(string(source), arch) {
			return false, fmt.Sprintf("conformance.json says %s claims %s, and %s does not mention it", claim.Source, arch, claim.Source)
		}
	}
	built := manifest.ArchitectureSet()
	claimed := append([]string(nil), claim.Architectures...)
	sort.Strings(claimed)
	for _, arch := range claimed {
		if !slices.Contains(built, arch) {
			return false, fmt.Sprintf("%s claims %s, which the release manifest does not build (%v)", claim.Source, arch, built)
		}
		for _, binary := range p.canonical.Binaries {
			if manifest.HashesFor(binary)[arch] == "" {
				return false, fmt.Sprintf("the manifest records no %s for %s", binary, arch)
			}
		}
	}
	return true, fmt.Sprintf("%s claims %v, and the release manifest builds every one of them", claim.Source, claimed)
}

func checkStatePersistence(p providerUnderTest) (bool, string) {
	mounts, detail := roleMounts(p)
	if mounts == nil {
		return false, detail
	}
	state, ok := mounts["state"]
	if !ok {
		return false, "no service maps the state role, so nothing survives container replacement"
	}
	if !strings.HasPrefix(state.HostPath, "/") {
		return false, fmt.Sprintf("the state role is mounted from %q, which is a named volume rather than a host path", state.HostPath)
	}
	if backups, ok := mounts["backups"]; ok && Contains(backups.HostPath, state.HostPath) {
		return false, fmt.Sprintf("state (%s) lives inside the backup root (%s)", state.HostPath, backups.HostPath)
	}
	if state.ReadOnly {
		return false, "the state directory is mounted read-only"
	}
	return true, fmt.Sprintf("state persists at %s outside the backup root", state.HostPath)
}

func checkBackupRootContainment(p providerUnderTest) (bool, string) {
	// Prefer canonical.json, which is the declared source of truth for
	// the platforms that have an entry there.
	if platform, ok := p.canonical.Platforms[p.id]; ok {
		backups := platform.HostPaths.Backups
		if !Contains(platform.StorageMount, backups) {
			return false, fmt.Sprintf("backup root %s is outside the declared storage mount %s", backups, platform.StorageMount)
		}
		for _, role := range []string{"state", "config", "sshKey", "knownHosts"} {
			path, _ := platform.HostPaths.ByRole(role)
			if Contains(backups, path) || Contains(path, backups) {
				return false, fmt.Sprintf("%s (%s) and the backup root (%s) are not separate", role, path, backups)
			}
		}
		return true, fmt.Sprintf("backup root %s holds no state, config or key material", backups)
	}

	// Otherwise derive it from whatever the provider's own metadata
	// mounts, so a provider without a canonical.json entry still gets a
	// real answer rather than a shrug.
	mounts, detail := roleMounts(p)
	if mounts == nil {
		return false, detail
	}
	backups, ok := mounts["backups"]
	if !ok {
		return false, "no service maps the backups role"
	}
	for _, role := range []string{"state", "config", "sshKey", "knownHosts"} {
		m, ok := mounts[role]
		if !ok {
			return false, fmt.Sprintf("no service maps the %s role, so containment cannot be decided", role)
		}
		if Contains(backups.HostPath, m.HostPath) || Contains(m.HostPath, backups.HostPath) {
			return false, fmt.Sprintf("%s (%s) and the backup root (%s) are not separate", role, m.HostPath, backups.HostPath)
		}
	}
	return true, fmt.Sprintf("backup root %s holds no state, config or key material", backups.HostPath)
}

// roleMounts collapses every service's mounts into one role -> mount map,
// and refuses when two services disagree about where a role comes from,
// or when a service mounts something at a container path the canonical
// image has no role for.
//
// That second refusal is the important one, and it used to be a `continue`.
// Mount.Role's own documentation says an empty role "is itself a
// finding", and TestOnlyTheWebUIContainerPublishesAPort's sibling in
// conformance_test.go already treats it as one. Skipping it here made two
// safety-relevant checks, state persistence and backup-root containment,
// fail OPEN: a profile that bind-mounts a whole /etc/backup-manager, or a
// stray /data, was invisible to both.
func roleMounts(p providerUnderTest) (map[string]Mount, string) {
	svcs, err := p.services()
	if err != nil {
		return nil, err.Error()
	}
	if len(svcs) == 0 {
		return nil, "this provider declares no container services, so there are no mounts to inspect"
	}
	out := map[string]Mount{}
	for _, s := range svcs {
		for _, m := range s.Mounts {
			if m.Role == "" {
				return nil, fmt.Sprintf("service %q mounts %s at %s, which is not a container path the canonical image knows about, so no storage rule can be applied to it", s.Name, m.HostPath, m.ContainerPath)
			}
			if prev, dup := out[m.Role]; dup && prev.HostPath != m.HostPath {
				return nil, fmt.Sprintf("role %q is mounted from two different host paths (%s and %s)", m.Role, prev.HostPath, m.HostPath)
			}
			out[m.Role] = m
		}
	}
	if len(out) == 0 {
		return nil, "no service maps any known storage role"
	}
	return out, ""
}

// checkAuthModeExplicit is the gate's "auth mode" line. It is deliberately
// two-sided: a provider that claims no native session must report
// local-account and wire no auth of its own, and a provider that DOES
// claim one must report a native session. Only checking the first half
// would let UGOS silently downgrade to local auth while still advertising
// a native session in the UI, which §22 calls out by name.
func checkAuthModeExplicit(p providerUnderTest) (bool, string) {
	data, err := os.ReadFile(p.bridgePath())
	if err != nil {
		return false, err.Error()
	}
	text := string(data)
	native := bridgeCapabilityRe("nativeAuth").MatchString(text)

	if native {
		if !strings.Contains(text, `mode: "native-session"`) {
			return false, "the bridge claims nativeAuth but does not report a native session mode"
		}
		return true, "claims a native session and reports one"
	}
	if !strings.Contains(text, `mode: "`+p.canonical.AuthMode+`"`) {
		return false, fmt.Sprintf("the bridge does not report auth mode %q", p.canonical.AuthMode)
	}
	if ok, detail := p.scan(ScanForBespokeAuth); !ok {
		return false, "wires authentication of its own: " + detail
	}
	return true, fmt.Sprintf("reports %s and wires no auth of its own", p.canonical.AuthMode)
}

// checkAPIPathIsolation is §63A's "API reachable only through the
// intended path". Two containers from one image differ only by argv, so
// the shape that matters is: exactly one published port in the whole
// profile, and it belongs to the container running the Web UI command,
// never the engine that holds the state database and the credentials.
func checkAPIPathIsolation(p providerUnderTest) (bool, string) {
	if p.spec.Metadata.Kind == "spk" {
		return checkSPKPortIsolation(p)
	}
	svcs, err := p.services()
	if err != nil {
		return false, err.Error()
	}
	if len(svcs) == 0 {
		return false, "this provider declares no container services, so there is no port map to inspect"
	}
	var publishing []Service
	for _, s := range svcs {
		if len(s.Ports) > 0 {
			publishing = append(publishing, s)
		}
	}
	if len(publishing) != 1 {
		names := make([]string, 0, len(publishing))
		for _, s := range publishing {
			names = append(names, s.Name)
		}
		return false, fmt.Sprintf("%d service(s) publish a port (%v); exactly one must", len(publishing), names)
	}
	edge := publishing[0]
	if len(edge.Ports) != 1 {
		return false, fmt.Sprintf("service %q publishes %v, want exactly one port", edge.Name, edge.Ports)
	}
	if strings.Join(edge.Command, " ") != strings.Join(p.canonical.Commands.WebUI, " ") {
		return false, fmt.Sprintf("the published service %q runs %v, not the Web UI command %v, so the engine is on the edge",
			edge.Name, edge.Command, p.canonical.Commands.WebUI)
	}
	return true, fmt.Sprintf("only %q publishes a port (%v), running the Web UI command", edge.Name, edge.Ports)
}

// checkSPKPortIsolation is the same rule for the one provider that has no
// compose file to read it out of.
//
// Synology used to be declared NOT_APPLICABLE here on the grounds that
// "the SPK runs one process behind DSM's own reverse proxy, and the port
// comes from conf/resource". Both halves were false: start-stop-status
// starts two processes, and conf/resource holds a data-share worker with
// no port in it at all. That mattered more than a wrong sentence, because
// Synology is the one provider whose engine isolation is enforced by a
// shell script rather than by a Docker network, so it is the weakest of
// the seven and it was the one cell the matrix declined to look at.
//
// The property is real and it is checkable from the shipped assets: the
// engine binds loopback, the Web UI binds a different port, and that Web
// UI port is the only one anything on the LAN can reach.
func checkSPKPortIsolation(p providerUnderTest) (bool, string) {
	root := Path(filepath.Join(p.spec.Metadata.Root, "spk", "assets"))
	common, err := os.ReadFile(filepath.Join(root, "scripts", "common.sh"))
	if err != nil {
		return false, fmt.Sprintf("cannot read the package's shared shell definitions: %v", err)
	}
	engine := regexp.MustCompile(`ENGINE_ADDR="([^"]+)"`).FindSubmatch(common)
	if engine == nil {
		return false, "scripts/common.sh sets no ENGINE_ADDR, so nothing pins where the engine listens"
	}
	host, port, ok := strings.Cut(string(engine[1]), ":")
	if !ok {
		return false, fmt.Sprintf("ENGINE_ADDR is %q, which names no host and port", engine[1])
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false, fmt.Sprintf("the engine listens on %q, which is not a loopback address, so the API is on the LAN edge", engine[1])
	}
	ui := regexp.MustCompile(`(?m)^UI_PORT=(\d+)`).FindSubmatch(common)
	if ui == nil {
		return false, "scripts/common.sh sets no UI_PORT"
	}
	if string(ui[1]) == port {
		return false, fmt.Sprintf("the Web UI and the engine both use port %s", port)
	}
	// The engine's port must not be what DSM publishes. INFO's adminport
	// is what DSM opens; a package that put the engine's port there would
	// hand the LAN the state database and the credentials.
	starter, err := os.ReadFile(filepath.Join(root, "scripts", "start-stop-status"))
	if err != nil {
		return false, fmt.Sprintf("cannot read start-stop-status: %v", err)
	}
	if !regexp.MustCompile(`\$\{?ENGINE_ADDR\}?`).Match(starter) {
		return false, "start-stop-status does not use ENGINE_ADDR, so the loopback bind above is not what actually starts"
	}
	for _, asset := range []string{filepath.Join("conf", "resource"), filepath.Join("ui", "config")} {
		body, err := os.ReadFile(filepath.Join(root, asset))
		if err != nil {
			return false, fmt.Sprintf("cannot read %s: %v", asset, err)
		}
		if strings.Contains(string(body), port) {
			return false, fmt.Sprintf("%s mentions the engine port %s; only the Web UI port may be published", asset, port)
		}
	}
	return true, fmt.Sprintf("the engine binds %s and the Web UI's %s is the only port any shipped asset publishes", engine[1], ui[1])
}

// checkProviderRemovalPreservesCore is §63A's "provider removal does not
// alter core behavior" and §7.1's dependency rule, decided the cheap way:
// nothing under core/ or ui/shared/src/ may import out of this provider's
// directory. The repository's own scripts/architecture/*.sh prove the
// stronger version by deleting the directory and rebuilding; this is the
// fast check that catches the import the moment it is written.
//
// It matches a QUOTED module path, not the bare string. core/service/
// scheduler_test.go says "apps/generic's serve command" in a comment,
// which is prose about the architecture rather than a dependency on it,
// and a check that cannot tell those apart is a check that gets ignored.
func checkProviderRemovalPreservesCore(p providerUnderTest) (bool, string) {
	re := ImportsProviderRe(p.id)
	var hits []string
	for _, tree := range []string{"core", filepath.Join("ui", "shared", "src")} {
		err := filepath.WalkDir(Path(tree), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == "dist" {
					return filepath.SkipDir
				}
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".go", ".ts", ".tsx", ".js", ".json":
			default:
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if re.Match(data) {
				rel, _ := filepath.Rel(Path("."), path)
				hits = append(hits, rel)
			}
			return nil
		})
		if err != nil {
			return false, err.Error()
		}
	}
	if len(hits) > 0 {
		return false, fmt.Sprintf("imported from %v", hits)
	}
	return true, "nothing under core/ or ui/shared/src/ imports this provider"
}

func checkHostManagementPlane(p providerUnderTest) (bool, string) {
	ok, detail := p.scan(ScanForHostPlaneModification)
	if !ok {
		return false, detail
	}
	// OpenMediaVault carries an extra rule §4A wrote for it by name: the
	// native Workbench plugin is deferred, and a deferral nobody checks
	// decays.
	if p.id == "openmediavault" {
		if pluginOK, pluginDetail := p.scan(ScanForOMVPlugin); !pluginOK {
			return false, "native OMV plugin material: " + pluginDetail
		}
		return true, detail + ", and no native Workbench plugin material"
	}
	return true, detail
}

// coverageRule is what a §68 acceptance procedure has to contain for a
// capability that only a real platform can decide.
type coverageRule struct {
	// steps must all match the procedure's headings and checklist boxes
	// rather than its whole text, because "the word update appears in a
	// footnote" is not a step an operator performs.
	steps []*regexp.Regexp
	// section names the part of the procedure that decides this
	// capability. Where it is set, the two rules below are applied to
	// that section's body, commands included.
	section *regexp.Regexp
	// capture is a step that records what the state was BEFORE the
	// destructive operation, and compare is the step that holds the
	// state afterwards against it. Both, in the same section, or the
	// procedure cannot detect the failure it claims to rule out: an
	// operator with nothing to compare against ticks "intact" from a
	// directory listing.
	capture *regexp.Regexp
	compare *regexp.Regexp
}

// evidenceCaptureRe and evidenceCompareRe are the two halves of a step
// that could actually notice a deletion.
var (
	evidenceCaptureRe = regexp.MustCompile(`(?i)sha256sum |md5sum |canary|\| *tee |find [^\n]*>|ls -[a-zA-Z]* *[^\n]*>`)
	evidenceCompareRe = regexp.MustCompile(`(?i)sha256sum -c|md5sum -c|\bdiff\b|\bcmp\b`)
)

var operatorCoverageRules = map[string]coverageRule{
	"install-update-remove": {steps: []*regexp.Regexp{
		regexp.MustCompile(`(?i)\binstall`),
		regexp.MustCompile(`(?i)\bupdate|\bupgrade`),
		regexp.MustCompile(`(?i)\b(uninstall|remove|removal|destroy)`),
	}},
	"ui-launch": {steps: []*regexp.Regexp{
		regexp.MustCompile(`(?i)web ui|web interface|webui|portal|web app`),
	}},
	"upgrade-preserves-state": {
		steps: []*regexp.Regexp{
			regexp.MustCompile(`(?i)\bupdate|\bupgrade`),
			regexp.MustCompile(`(?i)surviv|preserv|persist|still there|unchanged`),
		},
		section: regexp.MustCompile(`(?i)^#{2,3} .*\b(update|upgrade)`),
		capture: evidenceCaptureRe,
		compare: evidenceCompareRe,
	},
	// The destructive-safety capability of the phase. One topic-word
	// regex used to satisfy it, so a document with the heading "Removal
	// and retained backups" and nothing else reported PENDING_OPERATOR,
	// which resolve() presents as "the automated half held". That was
	// measuring whether the procedure mentions the topic, not whether it
	// could detect the loss of a backup.
	"remove-preserves-backups": {
		steps: []*regexp.Regexp{
			regexp.MustCompile(`(?i)retained.(backup|artifact)`),
		},
		section: regexp.MustCompile(`(?i)^#{2,3} .*\b(remove|removal|uninstall|destroy)`),
		capture: evidenceCaptureRe,
		compare: evidenceCompareRe,
	},
}

func operatorCoverage(capability string) func(providerUnderTest) (bool, string) {
	return func(p providerUnderTest) (bool, string) {
		if p.spec.Acceptance == "" {
			return false, "this provider has no §68 acceptance procedure"
		}
		rule := operatorCoverageRules[capability]
		steps, err := ProcedureSteps(Path(p.spec.Acceptance))
		if err != nil {
			return false, err.Error()
		}
		if steps == "" {
			return false, p.spec.Acceptance + " has no headings or checklist steps"
		}
		for _, re := range rule.steps {
			if !re.MatchString(steps) {
				return false, fmt.Sprintf("%s has no step matching %s", p.spec.Acceptance, re)
			}
		}
		if rule.section != nil {
			body, err := ProcedureSection(Path(p.spec.Acceptance), rule.section)
			if err != nil {
				return false, err.Error()
			}
			if body == "" {
				return false, fmt.Sprintf("%s has no section matching %s, so nothing in it decides this", p.spec.Acceptance, rule.section)
			}
			if !rule.capture.MatchString(body) {
				return false, fmt.Sprintf("%s's %s section records no baseline before the destructive step (a checksum, a canary, or a listing written to a file), so there is nothing to compare against afterwards", p.spec.Acceptance, capability)
			}
			if !rule.compare.MatchString(body) {
				return false, fmt.Sprintf("%s's %s section captures a baseline but never compares against it (no sha256sum -c, diff or cmp), so a loss would be ticked as intact", p.spec.Acceptance, capability)
			}
		}
		return true, "covered by " + p.spec.Acceptance + ", not yet executed"
	}
}

// checkAppStorePackaging is the one capability flag that is not decided
// by the bridge alone, because it is the one that makes a claim about
// artifacts rather than about runtime behaviour. Running this suite for
// the first time is what showed why: apps/truenas and apps/unraid ship a
// TrueNAS catalog entry and two Community Applications templates
// respectively, and both bridges still declared every capability false,
// so the UI told a user installed from a store that this was a "container
// deployment". Checking the flag against the artifacts is what caught it,
// and checking the artifacts against the flag is what stops the reverse:
// UGOS's bridge claims store packaging today and #83's UPK does not exist.
//
// Both halves were still one short. Flipping the flag fixed nothing a user
// can see, because no artifact loads a provider's bridge at all (#180), so
// the third condition below is the one that decides these cells today: the
// store artifacts exist, the flag is set, and the bundle that would honour
// it is somebody else's.
func checkAppStorePackaging(p providerUnderTest) (bool, string) {
	on, err := BridgeDeclaresCapability(p.bridgePath(), "appStorePackaging")
	if err != nil {
		return false, err.Error()
	}
	artifacts := p.spec.Metadata.StoreArtifacts
	if !on {
		return false, "the bridge does not opt in to appStorePackaging"
	}
	if len(artifacts) == 0 {
		return false, "the bridge claims store packaging but the provider declares no store or catalog artifact"
	}
	for _, f := range artifacts {
		if _, err := os.Stat(Path(filepath.Join(p.spec.Metadata.Root, f))); err != nil {
			return false, fmt.Sprintf("declared store artifact %s is missing: %v", f, err)
		}
	}
	if shipped, detail := bridgeReachesAShippedArtifact(p); !shipped {
		return false, fmt.Sprintf("%d store artifact(s) are present and the bridge claims store packaging, but %s, so a user who installs from the store is still told this is a container deployment", len(artifacts), detail)
	}
	return true, fmt.Sprintf("the bridge claims store packaging and %d catalog artifact(s) back it", len(artifacts))
}

func bridgeFlag(key string) func(providerUnderTest) (bool, string) {
	return func(p providerUnderTest) (bool, string) {
		on, err := BridgeDeclaresCapability(p.bridgePath(), key)
		if err != nil {
			return false, err.Error()
		}
		if !on {
			return false, "the bridge does not opt in to " + key + " (ui/shared's NO_CAPABILITIES default is false, so this is a declared no)"
		}
		if shipped, detail := bridgeReachesAShippedArtifact(p); !shipped {
			return false, "the bridge opts in to " + key + ", but " + detail
		}
		return true, "the bridge opts in to " + key + ", and it is the bridge a shipped artifact loads"
	}
}

// bridgeReachesAShippedArtifact is the question every capability flag
// turns on and none of them used to ask: does anything a user installs
// actually load apps/<provider>/frontend/platform.ts?
//
// ui/shared/vite.config.ts picks the shell at BUILD time from
// VITE_PLATFORM, defaulting to generic, and `serve-ui` serves one
// embedded bundle with no flag to serve another from disk. So the
// canonical image and the .spk that wraps the same binaries all serve the
// generic bridge, whose capabilities() is empty and whose deployment
// label is "Docker Compose". A flag set in a provider's platform.ts is a
// statement of repository intent until #180 gives serve-ui a way to
// select a bundle, and a conformance matrix that reports intent as PASS
// is reporting a capability nobody can reach.
func bridgeReachesAShippedArtifact(p providerUnderTest) (bool, string) {
	shipped, source, err := ShippedBridgeProvider()
	if err != nil {
		return false, err.Error()
	}
	if shipped == p.id {
		return true, fmt.Sprintf("%s selects the %s bundle", source, shipped)
	}
	return false, fmt.Sprintf("no shipped artifact loads it: %s selects the %s bundle and serve-ui serves one go:embed'ed bundle, so an installed package runs the %s bridge (#180)", source, shipped, shipped)
}

// ---------------------------------------------------------------------
// The declaration guards
// ---------------------------------------------------------------------

// phaseFourExitGateProviders is the §72 Phase 4 Exit Gate list, verbatim
// and in its own order, and the same six #86 and #81 name. Hard-coded
// rather than derived from conformance.json on purpose, and for two
// reasons now. A list derived from the file it is meant to check cannot
// catch a provider being dropped from that file; and since a column also
// declares whose gate it counts towards, a list derived from the file
// could not catch a claimed provider being quietly re-homed to another
// epic either, which is a cheaper way to make a gate green than fixing
// the provider.
//
// Six, not seven. UGOS was the seventh until the UGOS split moved its
// packaging to EPIC D's #83 (D1.2), and an EPIC B gate that waits on a
// package built on hardware nobody in this repository owns is a gate that
// cannot close. It is still a column of this matrix, declared to EPIC D:
// checked, resolved and reported like every other one, and in nobody's
// Phase 4 verdict.
var phaseFourExitGateProviders = []string{
	"generic", "truenas", "unraid", "openmediavault", "synology", "proxmox",
}

func TestTheMatrixCoversEveryPhaseFourExitGateProvider(t *testing.T) {
	c := MustLoadConformance()
	for _, want := range phaseFourExitGateProviders {
		p, ok := c.Providers[want]
		if !ok {
			t.Errorf("the Phase 4 Exit Gate names %s and conformance.json declares nothing for it", want)
			continue
		}
		if p.Epic != PhaseFourEpic {
			t.Errorf("the Phase 4 Exit Gate names %s and conformance.json declares it to EPIC %s, so the gate would not be computed over it", want, p.Epic)
		}
	}
	claimed := c.ProviderIDsFor(PhaseFourEpic)
	if len(claimed) != len(phaseFourExitGateProviders) {
		t.Errorf("conformance.json declares %d providers to EPIC %s (%v), the exit gate names %d (%v)", len(claimed), PhaseFourEpic, claimed, len(phaseFourExitGateProviders), phaseFourExitGateProviders)
	}

	// A column another epic owns is allowed, and is the point of the
	// epic field, but it has to say which epic rather than defaulting
	// into one: an empty epic would silently leave a column in nobody's
	// gate at all.
	epicRe := regexp.MustCompile(`^[A-Z]$`)
	for _, id := range c.ProviderIDs() {
		if !epicRe.MatchString(string(c.Providers[id].Epic)) {
			t.Errorf("%s declares epic %q, want a single EPIC letter such as %q", id, c.Providers[id].Epic, PhaseFourEpic)
		}
	}
}

// TestEveryCapabilityHasACheck pins conformance.json's capability list to
// capabilityChecks in both directions. One direction stops a capability
// being declared with nothing behind it; the other stops a check existing
// that no provider is measured against.
func TestEveryCapabilityHasACheck(t *testing.T) {
	c := MustLoadConformance()

	declared := map[string]bool{}
	for _, cap := range c.Capabilities {
		declared[cap.ID] = true
		if _, ok := capabilityChecks[cap.ID]; !ok {
			t.Errorf("capability %q is declared but no check decides it", cap.ID)
		}
		if cap.Title == "" || cap.Spec == "" {
			t.Errorf("capability %q has no title or no spec reference", cap.ID)
		}
		if cap.Mode != ModeRepo && cap.Mode != ModeOperator {
			t.Errorf("capability %q has mode %q, want %q or %q", cap.ID, cap.Mode, ModeRepo, ModeOperator)
		}
	}
	for id := range capabilityChecks {
		if !declared[id] {
			t.Errorf("check %q exists but conformance.json declares no such capability, so no provider is measured against it", id)
		}
	}
}

// auditDeclarations is the completeness guard's body, returning findings
// rather than reporting them, so it can be pointed at a provider a test
// built as easily as at one conformance.json declares. Every guard in
// this package that can fail is worth being able to prove still fires,
// and this one is worth it twice over: it is the guard the whole design
// rests on, and it has to keep applying to a column another epic owns.
func auditDeclarations(p Provider, caps []string) []string {
	var findings []string
	add := func(format string, args ...any) { findings = append(findings, fmt.Sprintf(format, args...)) }

	for _, id := range caps {
		cell, ok := p.Cells[id]
		if !ok {
			add("declares no outcome for %q; an undeclared capability reads as passing, which is exactly what §63A forbids", id)
			continue
		}
		switch cell.Declared {
		case DeclSupported:
		case DeclUnsupported, DeclNotApplicable:
			if strings.TrimSpace(cell.Reason) == "" {
				add("%q is declared %q with no reason; an unexplained exemption is indistinguishable from an oversight", id, cell.Declared)
			}
		case DeclBlocked:
			if strings.TrimSpace(cell.Reason) == "" {
				add("%q is declared blocked with no reason", id)
			}
			if !regexp.MustCompile(`^#\d+$`).MatchString(cell.Blocker) {
				add("%q is declared blocked with blocker %q, want a tracked issue like #174", id, cell.Blocker)
			}
			if strings.TrimSpace(cell.ExpectedDetail) == "" {
				add("%q is declared blocked on %s with no expectedDetail; without one the blocker excuses ANY failure of that check, including one that has nothing to do with it", id, cell.Blocker)
			}
		default:
			add("%q is declared %q, which is not one of supported/unsupported/not-applicable/blocked", id, cell.Declared)
		}
		if cell.VerifiedBy != "" {
			if err := VerifiedByReachable(cell.VerifiedBy); err != nil {
				add("%q points verifiedBy at %s, which does not hold up: %v", id, cell.VerifiedBy, err)
			}
		}
	}
	for id := range p.Cells {
		if _, ok := capabilityChecks[id]; !ok {
			add("declares an outcome for %q, which is not a capability", id)
		}
	}
	if p.Acceptance != "" {
		if _, err := os.Stat(Path(p.Acceptance)); err != nil {
			add("names acceptance procedure %s, which does not exist: %v", p.Acceptance, err)
		}
	}
	if p.Tier != "A" && p.Tier != "B" && p.Tier != "C" {
		add("declares §4A tier %q, want A, B or C", p.Tier)
	}
	sort.Strings(findings)
	return findings
}

// TestEveryProviderDeclaresEveryCapability is the guard that makes the
// whole design work. §63A's failure mode is a capability that is silently
// missing from a result set, because absence reads as passing; the only
// way to stop that is to make omission itself a failure.
//
// It runs over every column, including the ones another epic's gate
// consumes. Which gate counts a column decides nothing about how hard the
// column is checked.
func TestEveryProviderDeclaresEveryCapability(t *testing.T) {
	c := MustLoadConformance()
	caps := c.CapabilityIDs()

	for _, pid := range c.ProviderIDs() {
		t.Run(pid, func(t *testing.T) {
			for _, finding := range auditDeclarations(c.Providers[pid], caps) {
				t.Error(finding)
			}
		})
	}
}

// ---------------------------------------------------------------------
// The run
// ---------------------------------------------------------------------

func TestCrossProviderConformanceMatrix(t *testing.T) {
	conf := MustLoadConformance()
	canonical := MustLoad()
	m := NewMatrix(conf)

	for _, pid := range conf.ProviderIDs() {
		put := providerUnderTest{id: pid, spec: conf.Providers[pid], canonical: canonical}
		epic := conf.Providers[pid].Epic
		t.Run(pid, func(t *testing.T) {
			for _, cap := range conf.Capabilities {
				t.Run(cap.ID, func(t *testing.T) {
					r := resolve(put, cap, conf.Providers[pid].Cells[cap.ID])
					m.Record(r)
					t.Logf("%s: %s", r.Outcome, r.Detail)
					switch {
					case r.Outcome != OutcomeFail:
					case epic != PhaseFourEpic:
						// Still a failure, and deliberately still red.
						// A column another epic gates is not a column
						// nobody checks: this is where the drift shows
						// up when EPIC D ships #83 and half of these
						// declarations stop being true. What it is NOT
						// is a Phase 4 result, and the verdict below is
						// computed so that it cannot become one.
						t.Errorf("%s / %s: %s\n\nThis column is EPIC %s's, and the Phase 4 Exit Gate is not computed over it. It is checked here on the same terms as every other column, so this is a real failure and it is EPIC %s's to fix: update conformance.json's declaration rather than the check.", pid, cap.ID, r.Detail, epic, epic)
					default:
						t.Errorf("%s / %s: %s", pid, cap.ID, r.Detail)
					}
				})
			}
		})
	}

	// Non-vacuity. If the runner silently stopped deciding anything, all
	// of the above would still be green.
	if got := m.Count(OutcomePass); got < len(conf.Providers) {
		t.Errorf("only %d cells passed across %d providers; the runner is not deciding anything", got, len(conf.Providers))
	}
	for _, pid := range conf.ProviderIDs() {
		if len(m.Results[pid]) != len(conf.Capabilities) {
			t.Errorf("%s produced %d results for %d capabilities", pid, len(m.Results[pid]), len(conf.Capabilities))
		}
	}

	// The Phase 4 Exit Gate, computed over the columns EPIC B claims and
	// over nothing else. A claimed target with no recorded result counts
	// as a failure here rather than as an absence, because an unrun
	// target reads as a passing one otherwise (#170).
	v := m.Verdict(PhaseFourEpic)
	if len(v.Providers) != len(phaseFourExitGateProviders) {
		t.Errorf("the Phase 4 verdict was computed over %v, want the %d providers the exit gate names", v.Providers, len(phaseFourExitGateProviders))
	}
	for _, r := range v.Failures {
		t.Errorf("Phase 4 Exit Gate: %s / %s failed: %s", r.Provider, r.Capability, r.Detail)
	}
	t.Logf("Phase 4 Exit Gate over %v: %d failed, %d blocked, met=%v. Informational columns, gated by another epic: %v",
		v.Providers, len(v.Failures), len(v.Blocked), v.Met(), v.Informational)

	compareOrUpdateReport(t, m)
}

// resolve turns one declaration plus one check result into one cell.
func resolve(p providerUnderTest, cap Capability, cell Cell) Result {
	check := capabilityChecks[cap.ID]
	satisfied, detail := check(p)
	return resolveWith(p.id, cap, cell, satisfied, detail)
}

// resolveWith is resolve with the check already run, so the declaration
// arithmetic can be tested against a check whose verdict the test chose.
func resolveWith(provider string, cap Capability, cell Cell, satisfied bool, detail string) Result {
	r := Result{Provider: provider, Capability: cap.ID, Detail: detail}

	switch cell.Declared {
	case DeclSupported:
		switch {
		case !satisfied:
			r.Outcome = OutcomeFail
		case cap.Mode == ModeOperator:
			// The automated half held: the procedure exists and covers
			// this. The platform half has not run, and calling that a
			// pass is how a provider gets described as certified when
			// nobody has touched the hardware (§68).
			r.Outcome = OutcomePendingOperator
		default:
			r.Outcome = OutcomePass
		}

	case DeclUnsupported, DeclNotApplicable, DeclBlocked:
		if satisfied {
			// The staleness guard. A declaration that the repository has
			// outgrown is worse than no declaration: it is a documented
			// reason not to look.
			r.Outcome = OutcomeFail
			r.Detail = fmt.Sprintf("declared %q, but the check now passes (%s). Update conformance.json rather than the check.", cell.Declared, detail)
			return r
		}
		switch cell.Declared {
		case DeclUnsupported:
			r.Outcome = OutcomeUnsupported
		case DeclNotApplicable:
			r.Outcome = OutcomeNotApplicable
		default:
			// The other half of the staleness guard. A blocked cell used
			// to report BLOCKED whenever its check failed, for any
			// reason at all, so "blocked" plus a tracked issue number
			// silenced every future failure of that check for as long as
			// the declaration stood. Tying the excuse to the observed
			// message is brittle if the message changes, and that is the
			// intended behaviour: a cell that is being excused should
			// break loudly when the excuse stops being the reason.
			if !strings.Contains(detail, cell.ExpectedDetail) {
				r.Outcome = OutcomeFail
				r.Detail = fmt.Sprintf("declared blocked on %s, which expects a failure containing %q, but the check failed for a different reason: %s", cell.Blocker, cell.ExpectedDetail, detail)
				return r
			}
			r.Outcome = OutcomeBlocked
		}

	default:
		r.Outcome = OutcomeFail
		r.Detail = fmt.Sprintf("unknown declaration %q", cell.Declared)
	}
	return r
}

// compareOrUpdateReport holds docs/conformance/phase-4-matrix.md to what
// a real run produces. §68's INTEGRATION step asks for the per-provider,
// per-capability results to be recorded; a hand-written record of a test
// run is a record of what someone believed at the time, so this one is
// generated and then checked.
func compareOrUpdateReport(t *testing.T, m *Matrix) {
	t.Helper()
	path := Path(MatrixReportPath)

	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (the report is part of the deliverable, not an optional artifact)", MatrixReportPath, err)
	}
	want, err := SpliceMatrixReport(string(existing), m.Render())
	if err != nil {
		t.Fatal(err)
	}
	if string(existing) == want {
		return
	}
	if os.Getenv("CONFORMANCE_UPDATE") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatalf("write %s: %v", MatrixReportPath, err)
		}
		t.Logf("rewrote %s", MatrixReportPath)
		return
	}
	t.Errorf("%s is out of date with a real run. Regenerate it with:\n\n\tcd apps/common && CONFORMANCE_UPDATE=1 GOWORK=off go test ./packaging/ -count=1 -run TestCrossProviderConformanceMatrix\n", MatrixReportPath)
}
