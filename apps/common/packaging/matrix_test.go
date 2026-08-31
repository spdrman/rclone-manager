package packaging

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file is issue #86 (B4.5)'s half of the Phase 4 TDD Gate: one
// conformance run across all seven providers, reporting pass/fail per
// capability rather than a single opaque result.
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

// checkCoreBinaryHashParity is the gate's "core version/hash parity"
// line, and it is the one check in this file that cannot conclude today.
//
// The parity claim is that the binaries a provider ships are the binaries
// container/release-manifest.json recorded. That is only a claim about
// anything if the manifest describes a build you can reach: it pins a
// commit, and comparing against a commit that is not in the history is
// comparing against nothing. Today that commit is not an ancestor of
// main (#174), so this reports BLOCKED rather than being loosened into
// something that passes.
func checkCoreBinaryHashParity(p providerUnderTest) (bool, string) {
	data, err := os.ReadFile(Path(filepath.Join("container", "release-manifest.json")))
	if err != nil {
		return false, err.Error()
	}
	var manifest struct {
		Commit        string `json:"commit"`
		Architectures []struct {
			Architecture string            `json:"architecture"`
			BinarySHA256 map[string]string `json:"binary_sha256"`
		} `json:"architectures"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false, err.Error()
	}
	if manifest.Commit == "" {
		return false, "the release manifest pins no commit"
	}
	if err := exec.Command("git", "-C", Path("."), "merge-base", "--is-ancestor", manifest.Commit, "HEAD").Run(); err != nil {
		return false, fmt.Sprintf("release manifest pins commit %s, which is not an ancestor of HEAD, so its hashes describe a build that is not in this history", manifest.Commit[:7])
	}
	for _, a := range manifest.Architectures {
		for _, binary := range p.canonical.Binaries {
			if a.BinarySHA256[strings.TrimPrefix(binary, "/")] == "" {
				return false, fmt.Sprintf("no SHA-256 recorded for %s on %s", binary, a.Architecture)
			}
		}
	}
	return true, fmt.Sprintf("manifest commit %s is reachable and records every binary hash", manifest.Commit[:7])
}

func checkArchitectureParity(p providerUnderTest) (bool, string) {
	if p.spec.Metadata.Kind == "none" {
		return false, "this provider ships no package, so it makes no architecture claim to check"
	}
	data, err := os.ReadFile(Path(filepath.Join("container", "release-manifest.json")))
	if err != nil {
		return false, err.Error()
	}
	var manifest struct {
		Architectures []struct {
			Architecture string            `json:"architecture"`
			BinarySHA256 map[string]string `json:"binary_sha256"`
		} `json:"architectures"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false, err.Error()
	}
	var built []string
	for _, a := range manifest.Architectures {
		built = append(built, a.Architecture)
		for _, binary := range p.canonical.Binaries {
			if a.BinarySHA256[strings.TrimPrefix(binary, "/")] == "" {
				return false, fmt.Sprintf("the manifest records no %s for %s", binary, a.Architecture)
			}
		}
	}
	claimed := append([]string(nil), p.canonical.Architectures...)
	sort.Strings(claimed)
	sort.Strings(built)
	if strings.Join(claimed, ",") != strings.Join(built, ",") {
		return false, fmt.Sprintf("canonical.json claims %v, the release manifest records %v", claimed, built)
	}
	return true, fmt.Sprintf("claims and build agree on %v", claimed)
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
// and refuses when two services disagree about where a role comes from.
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
				continue
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

// operatorCoverageRules is what a §68 acceptance procedure has to contain
// for a capability that only a real platform can decide. Matching is
// against the procedure's headings and checklist boxes rather than its
// whole text, because "the word update appears in a footnote" is not a
// step an operator performs.
var operatorCoverageRules = map[string][]*regexp.Regexp{
	"install-update-remove": {
		regexp.MustCompile(`(?i)\binstall`),
		regexp.MustCompile(`(?i)\bupdate|\bupgrade`),
		regexp.MustCompile(`(?i)\b(uninstall|remove|removal|destroy)`),
	},
	"ui-launch": {
		regexp.MustCompile(`(?i)web ui|web interface|webui|portal|web app`),
	},
	"upgrade-preserves-state": {
		regexp.MustCompile(`(?i)\bupdate|\bupgrade`),
		regexp.MustCompile(`(?i)surviv|preserv|persist|still there|unchanged`),
	},
	"remove-preserves-backups": {
		regexp.MustCompile(`(?i)retained.(backup|artifact)`),
	},
}

func operatorCoverage(capability string) func(providerUnderTest) (bool, string) {
	return func(p providerUnderTest) (bool, string) {
		if p.spec.Acceptance == "" {
			return false, "this provider has no §68 acceptance procedure"
		}
		steps, err := ProcedureSteps(Path(p.spec.Acceptance))
		if err != nil {
			return false, err.Error()
		}
		if steps == "" {
			return false, p.spec.Acceptance + " has no headings or checklist steps"
		}
		for _, re := range operatorCoverageRules[capability] {
			if !re.MatchString(steps) {
				return false, fmt.Sprintf("%s has no step matching %s", p.spec.Acceptance, re)
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
	return true, fmt.Sprintf("the bridge claims store packaging and %d catalog artifact(s) back it", len(artifacts))
}

func bridgeFlag(key string) func(providerUnderTest) (bool, string) {
	return func(p providerUnderTest) (bool, string) {
		on, err := BridgeDeclaresCapability(p.bridgePath(), key)
		if err != nil {
			return false, err.Error()
		}
		if on {
			return true, "the bridge opts in to " + key
		}
		return false, "the bridge does not opt in to " + key + " (ui/shared's NO_CAPABILITIES default is false, so this is a declared no)"
	}
}

// ---------------------------------------------------------------------
// The declaration guards
// ---------------------------------------------------------------------

// phaseFourExitGateProviders is the §72 Phase 4 Exit Gate list, verbatim
// and in its own order. Hard-coded rather than derived from
// conformance.json on purpose: a list derived from the file it is meant
// to check cannot catch a provider being dropped from it.
var phaseFourExitGateProviders = []string{
	"generic", "ugos", "truenas", "unraid", "openmediavault", "synology", "proxmox",
}

func TestTheMatrixCoversEveryPhaseFourExitGateProvider(t *testing.T) {
	c := MustLoadConformance()
	for _, want := range phaseFourExitGateProviders {
		if _, ok := c.Providers[want]; !ok {
			t.Errorf("the Phase 4 Exit Gate names %s and conformance.json declares nothing for it", want)
		}
	}
	if len(c.Providers) != len(phaseFourExitGateProviders) {
		t.Errorf("conformance.json declares %d providers, the exit gate names %d", len(c.Providers), len(phaseFourExitGateProviders))
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

// TestEveryProviderDeclaresEveryCapability is the guard that makes the
// whole design work. §63A's failure mode is a capability that is silently
// missing from a result set, because absence reads as passing; the only
// way to stop that is to make omission itself a failure.
func TestEveryProviderDeclaresEveryCapability(t *testing.T) {
	c := MustLoadConformance()
	caps := c.CapabilityIDs()

	for _, pid := range c.ProviderIDs() {
		t.Run(pid, func(t *testing.T) {
			p := c.Providers[pid]
			for _, id := range caps {
				cell, ok := p.Cells[id]
				if !ok {
					t.Errorf("declares no outcome for %q; an undeclared capability reads as passing, which is exactly what §63A forbids", id)
					continue
				}
				switch cell.Declared {
				case DeclSupported:
				case DeclUnsupported, DeclNotApplicable:
					if strings.TrimSpace(cell.Reason) == "" {
						t.Errorf("%q is declared %q with no reason; an unexplained exemption is indistinguishable from an oversight", id, cell.Declared)
					}
				case DeclBlocked:
					if strings.TrimSpace(cell.Reason) == "" {
						t.Errorf("%q is declared blocked with no reason", id)
					}
					if !regexp.MustCompile(`^#\d+$`).MatchString(cell.Blocker) {
						t.Errorf("%q is declared blocked with blocker %q, want a tracked issue like #174", id, cell.Blocker)
					}
				default:
					t.Errorf("%q is declared %q, which is not one of supported/unsupported/not-applicable/blocked", id, cell.Declared)
				}
				if cell.VerifiedBy != "" {
					if _, err := os.Stat(Path(cell.VerifiedBy)); err != nil {
						t.Errorf("%q points verifiedBy at %s, which does not exist: %v", id, cell.VerifiedBy, err)
					}
				}
			}
			for id := range p.Cells {
				if _, ok := capabilityChecks[id]; !ok {
					t.Errorf("declares an outcome for %q, which is not a capability", id)
				}
			}
			if p.Acceptance != "" {
				if _, err := os.Stat(Path(p.Acceptance)); err != nil {
					t.Errorf("names acceptance procedure %s, which does not exist: %v", p.Acceptance, err)
				}
			}
			if p.Tier != "A" && p.Tier != "B" && p.Tier != "C" {
				t.Errorf("declares §4A tier %q, want A, B or C", p.Tier)
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
		t.Run(pid, func(t *testing.T) {
			for _, cap := range conf.Capabilities {
				t.Run(cap.ID, func(t *testing.T) {
					r := resolve(put, cap, conf.Providers[pid].Cells[cap.ID])
					m.Record(r)
					t.Logf("%s: %s", r.Outcome, r.Detail)
					if r.Outcome == OutcomeFail {
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

	compareOrUpdateReport(t, m)
}

// resolve turns one declaration plus one check result into one cell.
func resolve(p providerUnderTest, cap Capability, cell Cell) Result {
	check := capabilityChecks[cap.ID]
	satisfied, detail := check(p)

	r := Result{Provider: p.id, Capability: cap.ID, Detail: detail}

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
	t.Errorf("%s is out of date with a real run. Regenerate it with:\n\n\tcd apps/common && CONFORMANCE_UPDATE=1 GOWORK=off go test ./packaging/ -run TestCrossProviderConformanceMatrix\n", MatrixReportPath)
}
