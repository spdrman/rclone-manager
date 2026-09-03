// Release provenance: issue #88 (B5.2), docs/EPIC-B-multi-nas.md §61 and
// §73 Work Package 5.2.
//
// §61 asks release automation to produce checksums, OCI digests, an SBOM,
// dependency manifests, the build commit, the rclone version, the Go
// version, the frontend lockfile state, a third-party licence inventory
// and per-architecture binary hashes. #174 built the last of those and
// left the rest here, along with the push that turns a local image ID
// into a registry digest.
//
// Everything in this file derives its answer from the tree rather than
// from a checked-in claim about the tree. The inventory is re-derived
// from `go list -deps` and ui/shared's lockfile on every run and compared
// byte for byte against provenance/third-party-licenses.json;
// the artifact hashes are re-computed from the files themselves. A
// checked-in SBOM that nothing re-derives is a document, not evidence,
// and the specific failure it invites is the one this work package is
// named after: a dependency enters the graph, nobody updates the
// inventory, and the release ships with a licence obligation nobody read.
//
// Two vacuity traps are guarded explicitly, because both are ways a
// supply-chain check passes by looking at nothing:
//
//   - a parity check over an empty artifact set has nothing to disagree
//     with, so a distribution target that declares no artifacts must say
//     why in as many words (DistributionTarget.UnbuiltReason's job), and
//     ArtifactParityComplaints refuses one that does not;
//   - a licence classifier that returns "unknown" for a licence it cannot
//     read, and an inventory that treats unknown as acceptable, together
//     launder a copyleft dependency into a permissive graph. ClassifyLicense
//     returns the empty string rather than a guess, and
//     LicensePolicyComplaints refuses the empty string.
package packaging

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------
// Licence identification
// ---------------------------------------------------------------------

// licenceMarker is one recognisable licence text and the SPDX id it
// means. Order is load-bearing and the table is walked in order: the
// GNU family has to be asked about first, because an Apache-2.0
// compatibility note or a GPL-linking exception inside another licence's
// text would otherwise be matched by a later, looser rule and a copyleft
// dependency would classify as permissive. That is the single worst
// failure this function can have, so it is the first one designed out.
var licenceMarkers = []struct {
	spdxID string
	all    []string
}{
	{"AGPL-3.0-only", []string{"GNU AFFERO GENERAL PUBLIC LICENSE", "Version 3"}},
	{"LGPL-3.0-only", []string{"GNU LESSER GENERAL PUBLIC LICENSE", "Version 3"}},
	{"LGPL-2.1-only", []string{"GNU LESSER GENERAL PUBLIC LICENSE", "Version 2.1"}},
	{"GPL-3.0-only", []string{"GNU GENERAL PUBLIC LICENSE", "Version 3"}},
	{"GPL-2.0-only", []string{"GNU GENERAL PUBLIC LICENSE", "Version 2"}},
	{"MPL-2.0", []string{"Mozilla Public License Version 2.0"}},
	// The same licence, spelled the way HashiCorp's projects spell it in
	// their LICENSE files: "Mozilla Public License, version 2.0", with a
	// comma and a lower-case v. It is a second exact phrase rather than a
	// looser match on "Mozilla Public License" plus "2.0", because those
	// two substrings co-occur in files that only MENTION the MPL, and a
	// permissive dependency classified as copyleft is a wrong answer in
	// the other direction.
	//
	// And it is a second ROW rather than a second needle on the row
	// above, because the needles on one row are ANDed: every one of them
	// has to be in the text. Both spellings on one row would match
	// neither file. TestClassifyLicense_AMentionOfMozillaIsNotTheMPL is
	// the control that keeps the second row exact.
	//
	// Registering rclone's s3 backend (#235) is what surfaced this:
	// go-cleanhttp and go-retryablehttp arrive under it, both are
	// MPL-2.0, and both landed in the generated NOTICE as NOASSERTION.
	// An unclassified WEAK-COPYLEFT dependency in a shipped compliance
	// record is the failure this table exists to prevent, so the spelling
	// is added rather than the entries explained away.
	{"MPL-2.0", []string{"Mozilla Public License, version 2.0"}},
	{"EPL-2.0", []string{"Eclipse Public License - v 2.0"}},
	{"CDDL-1.0", []string{"COMMON DEVELOPMENT AND DISTRIBUTION LICENSE"}},
	{"Apache-2.0", []string{"Apache License", "Version 2.0"}},
	{"CC0-1.0", []string{"CC0 1.0 Universal"}},
	{"BSD-3-Clause", []string{"Redistribution and use in source and binary forms", "Neither the name"}},
	{"BSD-2-Clause", []string{"Redistribution and use in source and binary forms"}},
	{"ISC", []string{"Permission to use, copy, modify, and", "distribute this software for any purpose"}},
	{"MIT", []string{"Permission is hereby granted, free of charge"}},
	{"Unlicense", []string{"This is free and unencumbered software released into the public domain"}},
}

// ClassifyLicense maps a licence text to an SPDX id, or to the empty
// string when it recognises nothing.
//
// The empty string is a deliberate refusal rather than a fallback. A
// classifier that guesses is worse than one that gives up, because the
// guess is what the inventory records and the inventory is what the
// licence choice rests on.
func ClassifyLicense(text string) string {
	for _, m := range licenceMarkers {
		matched := true
		for _, needle := range m.all {
			if !strings.Contains(text, needle) {
				matched = false
				break
			}
		}
		if matched {
			return m.spdxID
		}
	}
	return ""
}

// licenceFileNames are the filenames a module may carry its terms in,
// most specific first.
var licenceFileNames = []string{
	"LICENSE", "LICENSE.txt", "LICENSE.md", "LICENCE", "LICENCE.txt",
	"LICENSE-MIT", "COPYING", "COPYING.txt", "COPYRIGHT",
}

// findLicenseFile returns the first licence-looking file in dir, or "".
func findLicenseFile(dir string) string {
	for _, name := range licenceFileNames {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
			return name
		}
	}
	return ""
}

// ---------------------------------------------------------------------
// The inventory
// ---------------------------------------------------------------------

// Component is one third-party dependency that reaches a shipped
// artifact.
type Component struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Ecosystem string `json:"ecosystem"`
	LicenseID string `json:"licenseId"`
	// LicenseFile and LicenseSHA256 pin the terms that were actually
	// read, not just their name. An upstream that relicenses in place
	// keeps its SPDX id and changes these, which is the case a bare
	// "MIT" string cannot notice.
	LicenseFile   string `json:"licenseFile"`
	LicenseSHA256 string `json:"licenseSha256"`
	// Integrity is the lockfile's own subresource hash, for ecosystems
	// that record one. Empty for Go, whose equivalent lives in go.sum.
	Integrity string `json:"integrity,omitempty"`
	// LinkedInto names the shipped artifacts this component reaches.
	LinkedInto []string `json:"linkedInto"`
}

// Inventory is provenance/third-party-licenses.json.
type Inventory struct {
	Schema         string      `json:"schema"`
	Note           []string    `json:"note"`
	ProjectLicense string      `json:"projectLicense"`
	Components     []Component `json:"components"`
}

// InventorySchema is the current shape's identifier.
const InventorySchema = "backup-manager/third-party-licenses/1"

// ParseInventory reads an inventory document.
func ParseInventory(data []byte) (Inventory, error) {
	var inv Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return Inventory{}, err
	}
	return inv, nil
}

// EcosystemGo and EcosystemNPM are the two dependency ecosystems that
// reach a shipped artifact: Go modules compiled into the binaries, and
// npm packages built into the embedded web bundle.
const (
	EcosystemGo  = "go"
	EcosystemNPM = "npm"
)

// SortComponents puts an inventory in its canonical order, so
// regenerating it is byte-stable and a diff is a real change rather than
// map iteration.
func SortComponents(components []Component) {
	sort.Slice(components, func(i, j int) bool {
		if components[i].Ecosystem != components[j].Ecosystem {
			return components[i].Ecosystem < components[j].Ecosystem
		}
		if components[i].Name != components[j].Name {
			return components[i].Name < components[j].Name
		}
		return components[i].Version < components[j].Version
	})
}

// ---------------------------------------------------------------------
// Deriving the Go half from the module graph
// ---------------------------------------------------------------------

// GoBuildTarget is one shipped Go binary and where to ask about it.
type GoBuildTarget struct {
	// Binary is the name the binary has inside the image, without the
	// leading slash, matching how release-manifest.json keys its hashes.
	Binary string
	// ModuleDir is the module's directory, repository-root-relative.
	ModuleDir string
	// Package is the main package, relative to ModuleDir.
	Package string
}

// ShippedGoBinaries are the two binaries container/Dockerfile copies into
// the runtime stage. Declared here rather than discovered so that a third
// binary appearing in the Dockerfile and not here is a difference someone
// has to make on purpose.
var ShippedGoBinaries = []GoBuildTarget{
	{Binary: "backup-manager", ModuleDir: "core", Package: "./cmd/backup-manager"},
	{Binary: "backup-manager-web", ModuleDir: "apps/generic", Package: "./cmd/backup-manager-web"},
}

// GoModuleRef is one module in a binary's linked graph.
type GoModuleRef struct {
	Path    string
	Version string
	Dir     string
}

// firstPartyModulePrefix is this repository's own module namespace.
// Modules under it are the product, not third-party dependencies of it.
const firstPartyModulePrefix = "github.com/spdrman/rclone-manager/"

// GoLinkedModules lists the third-party modules linked into one binary
// for one target platform.
//
// GOOS is not optional and there is no host-platform default, because
// the host is a Mac and the product is Linux: `go list -deps` run
// natively pulls in github.com/go-darwin/apfs and misses whatever the
// Linux build takes instead, so an SBOM generated without setting it
// describes a binary nobody ships.
func GoLinkedModules(target GoBuildTarget, goos, goarch string) ([]GoModuleRef, error) {
	cmd := exec.Command("go", "list", "-deps",
		"-f", "{{with .Module}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}", target.Package)
	cmd.Dir = Path(target.ModuleDir)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps for %s (%s/%s): %w: %s", target.Binary, goos, goarch, err, strings.TrimSpace(stderr.String()))
	}
	refs, err := parseGoListModules(string(out))
	if err != nil {
		return nil, fmt.Errorf("%s (%s/%s): %w", target.Binary, goos, goarch, err)
	}
	return refs, nil
}

// parseGoListModules turns `go list -deps` output into the third-party
// module set, and is separate from the exec so it can be driven against
// lines a real module graph does not produce today.
//
// The empty-version case is the whole reason this is a function.
// `go list` reports no version for the main module, for a module under
// this repository's own namespace, AND for any third-party module
// satisfied by a directory `replace`. Those are two different facts, and
// collapsing them (as `version == "" || first-party` did) means a
// `replace github.com/some/thirdparty => ./vendor/thirdparty` deletes
// that module from the inventory and the SBOM, silently: the licence
// policy never judges it, and the drift test still passes because the
// regenerated bytes omit it too. Every check stays green and the graph
// the licence choice rests on is no longer the graph that ships.
//
// So first-party-and-unversioned is skipped and anything else
// unversioned is an error. Failing generation is the right answer:
// an unversioned third-party module is precisely what this bundle exists
// to refuse, and the cost (a local replace during an incident blocks a
// release until someone records that module's licence) is the cost of
// the bundle meaning anything.
func parseGoListModules(out string) ([]GoModuleRef, error) {
	seen := map[string]GoModuleRef{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			continue
		}
		path, version, dir := parts[0], parts[1], parts[2]
		if version == "" {
			if isFirstPartyModule(path) {
				continue
			}
			return nil, fmt.Errorf("go list reports no version for %s (resolved from %s), which means it is satisfied by a local replace rather than by a module version; a third-party module with no version cannot be recorded in the SBOM or judged by the licence policy, so it would vanish from both instead of failing. Remove the replace, or add the module to the first-party namespace if it is genuinely ours", path, dir)
		}
		if isFirstPartyModule(path) {
			continue
		}
		seen[path+"@"+version] = GoModuleRef{Path: path, Version: version, Dir: dir}
	}
	refs := make([]GoModuleRef, 0, len(seen))
	for _, r := range seen {
		refs = append(refs, r)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Path < refs[j].Path })
	return refs, nil
}

// isFirstPartyModule reports whether a module path is this repository's
// own. The prefix has a trailing slash, so the bare namespace is matched
// separately rather than by loosening the prefix, which would also match
// a hypothetical github.com/spdrman/rclone-manager-anything.
func isFirstPartyModule(path string) bool {
	return path == strings.TrimSuffix(firstPartyModulePrefix, "/") ||
		strings.HasPrefix(path, firstPartyModulePrefix)
}

// ---------------------------------------------------------------------
// Deriving the npm half from the lockfile
// ---------------------------------------------------------------------

// UISharedLockfile is the frontend lockfile §61 asks a release to record
// the state of.
const UISharedLockfile = "ui/shared/package-lock.json"

type npmLockfile struct {
	LockfileVersion int `json:"lockfileVersion"`
	Packages        map[string]struct {
		Version   string `json:"version"`
		License   string `json:"license"`
		Dev       bool   `json:"dev"`
		Integrity string `json:"integrity"`
		Resolved  string `json:"resolved"`
	} `json:"packages"`
}

// NPMProductionComponents reads the production dependency set out of an
// npm lockfile.
//
// It reads the lockfile rather than node_modules/ on purpose. The
// lockfile is what `npm ci` installs and what container/Dockerfile's
// frontend stage builds from, it is checked in, and it carries an
// integrity hash per package, so this answer is identical on a machine
// with no node_modules/ at all. Reading the installed tree instead would
// make the SBOM depend on whether somebody had run an install, which is
// the same class of hole as a gate that skips a suite it cannot see.
func NPMProductionComponents(data []byte) ([]Component, error) {
	var lock npmLockfile
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parse %s: %w", UISharedLockfile, err)
	}
	if lock.LockfileVersion < 2 {
		return nil, fmt.Errorf("%s is lockfileVersion %d; the per-package license and integrity fields this reads arrived in version 2", UISharedLockfile, lock.LockfileVersion)
	}
	var out []Component
	for path, pkg := range lock.Packages {
		// The empty key is the workspace root itself, and a dev
		// dependency never reaches the shipped bundle.
		if path == "" || pkg.Dev {
			continue
		}
		name := strings.TrimPrefix(path, "node_modules/")
		if i := strings.LastIndex(name, "/node_modules/"); i >= 0 {
			name = name[i+len("/node_modules/"):]
		}
		out = append(out, Component{
			Name:       name,
			Version:    pkg.Version,
			Ecosystem:  EcosystemNPM,
			LicenseID:  pkg.License,
			Integrity:  pkg.Integrity,
			LinkedInto: []string{"backup-manager-web"},
		})
	}
	SortComponents(out)
	return out, nil
}

// ---------------------------------------------------------------------
// The SPDX document
// ---------------------------------------------------------------------

// SPDXCreationInfo is the SBOM's provenance about itself.
type SPDXCreationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

// SPDXChecksum is one recorded digest.
type SPDXChecksum struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"checksumValue"`
}

// SPDXPackage is one component in the SBOM.
type SPDXPackage struct {
	SPDXID           string         `json:"SPDXID"`
	Name             string         `json:"name"`
	VersionInfo      string         `json:"versionInfo"`
	DownloadLocation string         `json:"downloadLocation"`
	FilesAnalyzed    bool           `json:"filesAnalyzed"`
	LicenseConcluded string         `json:"licenseConcluded"`
	LicenseDeclared  string         `json:"licenseDeclared"`
	Supplier         string         `json:"supplier"`
	ExternalRefs     []SPDXRef      `json:"externalRefs,omitempty"`
	Checksums        []SPDXChecksum `json:"checksums,omitempty"`
}

// SPDXRef is a package-manager coordinate for one package.
type SPDXRef struct {
	Category string `json:"referenceCategory"`
	Type     string `json:"referenceType"`
	Locator  string `json:"referenceLocator"`
}

// SPDXRelationship ties two SPDX elements together.
type SPDXRelationship struct {
	SPDXElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSPDXElement string `json:"relatedSpdxElement"`
}

// SPDXDocument is an SPDX 2.3 JSON SBOM.
type SPDXDocument struct {
	SPDXVersion       string             `json:"spdxVersion"`
	DataLicense       string             `json:"dataLicense"`
	SPDXID            string             `json:"SPDXID"`
	Name              string             `json:"name"`
	DocumentNamespace string             `json:"documentNamespace"`
	CreationInfo      SPDXCreationInfo   `json:"creationInfo"`
	Packages          []SPDXPackage      `json:"packages"`
	Relationships     []SPDXRelationship `json:"relationships"`
}

// spdxIDSafe turns a component name into an SPDX element id fragment.
// SPDX allows letters, digits, "." and "-" only.
func spdxIDSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// noAssertion is SPDX's own word for "this document does not claim to
// know", and it is used here only where that is literally true.
const noAssertion = "NOASSERTION"

// BuildSPDX renders an inventory as an SPDX 2.3 document.
//
// created is passed in rather than read off the clock, and the caller
// passes the release manifest's own generated_at. An SBOM stamped with
// wall-clock time is a file that differs from itself on every
// regeneration, which makes "regenerate and diff" - the only check that
// can catch an undeclared dependency - impossible to run.
func BuildSPDX(inv Inventory, name, namespace, created string) SPDXDocument {
	doc := SPDXDocument{
		SPDXVersion:       "SPDX-2.3",
		DataLicense:       "CC0-1.0",
		SPDXID:            "SPDXRef-DOCUMENT",
		Name:              name,
		DocumentNamespace: namespace,
		CreationInfo: SPDXCreationInfo{
			Created:  created,
			Creators: []string{"Tool: backup-manager-provenance", "Organization: The Backup Manager Authors"},
		},
	}
	for _, c := range inv.Components {
		id := "SPDXRef-Package-" + c.Ecosystem + "-" + spdxIDSafe(c.Name) + "-" + spdxIDSafe(c.Version)
		licence := c.LicenseID
		if licence == "" {
			licence = noAssertion
		}
		pkg := SPDXPackage{
			SPDXID:           id,
			Name:             c.Name,
			VersionInfo:      c.Version,
			DownloadLocation: noAssertion,
			FilesAnalyzed:    false,
			LicenseConcluded: licence,
			LicenseDeclared:  licence,
			Supplier:         noAssertion,
		}
		switch c.Ecosystem {
		case EcosystemGo:
			pkg.ExternalRefs = []SPDXRef{{
				Category: "PACKAGE-MANAGER", Type: "purl",
				Locator: "pkg:golang/" + c.Name + "@" + c.Version,
			}}
			if c.LicenseSHA256 != "" {
				pkg.Checksums = []SPDXChecksum{{Algorithm: "SHA256", Value: c.LicenseSHA256}}
			}
		case EcosystemNPM:
			pkg.ExternalRefs = []SPDXRef{{
				Category: "PACKAGE-MANAGER", Type: "purl",
				Locator: "pkg:npm/" + strings.ReplaceAll(c.Name, "@", "%40") + "@" + c.Version,
			}}
		}
		doc.Packages = append(doc.Packages, pkg)
		doc.Relationships = append(doc.Relationships, SPDXRelationship{
			SPDXElementID:      "SPDXRef-DOCUMENT",
			RelationshipType:   "DESCRIBES",
			RelatedSPDXElement: id,
		})
	}
	return doc
}

// ---------------------------------------------------------------------
// The provenance bundle
// ---------------------------------------------------------------------

// ProvenanceSchema is the current bundle shape's identifier.
const ProvenanceSchema = "backup-manager/release-provenance/1"

// ProvenanceDir is where the generated compliance artifacts live.
//
// Top level rather than under container/, for two reasons that point the
// same way. These describe the whole release (both binaries, the image,
// every provider package) and not the container build, and container/ is
// scanned as the generic provider's own packaging surface by
// ScanForBespokeAuth, whose fingerprint list includes "oidc" - a word the
// signing record has to use, because `cosign verify
// --certificate-oidc-issuer` is the command a verifier runs. Rewording
// the record to dodge a scanner aimed at something else would be the
// wrong fix twice over.
const ProvenanceDir = "provenance"

// Provenance artifact paths, repository-root-relative.
const (
	InventoryPath  = ProvenanceDir + "/third-party-licenses.json"
	SBOMPath       = ProvenanceDir + "/sbom.spdx.json"
	ChecksumsPath  = ProvenanceDir + "/checksums.txt"
	ProvenancePath = ProvenanceDir + "/release-provenance.json"
	ManifestPath   = "container/release-manifest.json"
)

// ProvenanceFile is one generated artifact, identified by its own digest.
type ProvenanceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Format string `json:"format,omitempty"`
}

// ProvenanceManifestRef ties the bundle to the build record.
//
// SHA256 is the whole point of this struct existing. The two files are
// generated by different steps (one needs a two-architecture Docker
// build, one needs no build at all), so nothing but a recorded digest
// stops a regenerated manifest from being paired with a stale bundle.
type ProvenanceManifestRef struct {
	Path          string             `json:"path"`
	SHA256        string             `json:"sha256"`
	Commit        string             `json:"commit"`
	RecordedBuild string             `json:"recordedBuildVersion"`
	Architectures []string           `json:"architectures"`
	Published     bool               `json:"published"`
	Digests       map[string]*string `json:"registryDigests"`
	// VersionIsABuildStamp records that recordedBuildVersion is not the
	// semantic version the packages advertise. It is true today: with no
	// tags in the repository, the generator's `git describe --tags
	// --always` yields an abbreviated commit, so the binaries answer
	// `version` with a SHA while every provider package points at tag
	// 1.0.0. Recording it makes the gap machine-readable, and
	// TestVersionIsABuildStampCannotBeMisstated makes the record match
	// the files. VersionParityComplaints turns it into a refusal the
	// moment a release is actually published.
	VersionIsABuildStamp bool `json:"versionIsABuildStamp"`
}

// RecordedArtifact is one distributed file and the digest recorded for
// it.
type RecordedArtifact struct {
	Target string `json:"target"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// UnbuiltTarget is a distribution target whose artifact is not assembled
// in this repository, and the reason.
type UnbuiltTarget struct {
	Target string `json:"target"`
	Reason string `json:"reason"`
}

// SigningRecord is what has been done to the published image, and how.
type SigningRecord struct {
	Status       string   `json:"status"`
	Method       string   `json:"method"`
	Identity     string   `json:"identity"`
	Transparency string   `json:"transparencyLog"`
	Note         []string `json:"note"`
}

// LinkReadiness is the store-facing verdict on §73 WP5.2's link criterion.
type LinkReadiness struct {
	PubliclyReachable bool   `json:"publiclyReachable"`
	Reason            string `json:"reason"`
}

// Provenance is provenance/release-provenance.json: the
// compliance half of the release record.
type Provenance struct {
	Schema          string                `json:"schema"`
	Note            []string              `json:"note"`
	SemanticVersion string                `json:"semanticVersion"`
	ImageReference  string                `json:"imageReference"`
	ReleaseManifest ProvenanceManifestRef `json:"releaseManifest"`
	License         ProvenanceLicense     `json:"license"`
	SBOM            ProvenanceFile        `json:"sbom"`
	Checksums       ProvenanceFile        `json:"checksums"`
	Artifacts       []RecordedArtifact    `json:"artifacts"`
	UnbuiltTargets  []UnbuiltTarget       `json:"unbuiltTargets"`
	Links           LinkReadiness         `json:"links"`
	Signing         SigningRecord         `json:"signing"`
	Performance     Performance           `json:"performance"`
}

// ProvenanceLicense records the project's licence artifacts by digest.
type ProvenanceLicense struct {
	SPDXID     string         `json:"spdxId"`
	File       ProvenanceFile `json:"file"`
	Notice     ProvenanceFile `json:"notice"`
	Inventory  ProvenanceFile `json:"inventory"`
	Components int            `json:"componentCount"`
}

// ParseProvenance reads a provenance bundle.
func ParseProvenance(data []byte) (Provenance, error) {
	var p Provenance
	if err := json.Unmarshal(data, &p); err != nil {
		return Provenance{}, err
	}
	return p, nil
}

// ---------------------------------------------------------------------
// The rules
// ---------------------------------------------------------------------

// ArtifactParityComplaints says every way the recorded artifact set
// disagrees with what each distribution target actually declares and
// with the bytes on disk.
//
// hash is injected so a table test can hand it a deliberately mismatched
// artifact. The whole acceptance criterion is "the check is demonstrated
// against a deliberately mismatched artifact", and a check that can only
// be exercised by corrupting the real repository is a check nobody
// exercises.
func ArtifactParityComplaints(targets map[string]DistributionTarget, recorded []RecordedArtifact, hash func(rel string) (string, error)) []string {
	var out []string
	if len(targets) == 0 {
		return []string{"no distribution target is declared at all, so artifact parity has nothing to check and passes by default"}
	}

	declared := map[string]string{} // path -> target
	targetIDs := make([]string, 0, len(targets))
	for id := range targets {
		targetIDs = append(targetIDs, id)
	}
	sort.Strings(targetIDs)

	for _, id := range targetIDs {
		t := targets[id]
		switch {
		case len(t.Artifacts) == 0 && t.UnbuiltReason == "":
			out = append(out, fmt.Sprintf("target %q declares no artifacts and gives no reason: a parity check over an empty set passes by having nothing to compare, which is the one way this check can be green and mean nothing", id))
		case len(t.Artifacts) > 0 && t.UnbuiltReason != "":
			out = append(out, fmt.Sprintf("target %q declares %d artifact(s) AND a reason it builds none, so the record says both things at once", id, len(t.Artifacts)))
		}
		for _, path := range t.Artifacts {
			if other, dup := declared[path]; dup {
				out = append(out, fmt.Sprintf("%s is declared by both %q and %q, so one recorded digest stands for two claims", path, other, id))
				continue
			}
			declared[path] = id
		}
	}

	recordedBy := map[string]RecordedArtifact{}
	for _, r := range recorded {
		if _, dup := recordedBy[r.Path]; dup {
			out = append(out, fmt.Sprintf("%s is recorded twice in the provenance bundle", r.Path))
			continue
		}
		recordedBy[r.Path] = r
		if _, ok := declared[r.Path]; !ok {
			out = append(out, fmt.Sprintf("the provenance bundle records %s, which no distribution target declares, so an artifact is being shipped that nothing accounts for", r.Path))
		}
	}

	paths := make([]string, 0, len(declared))
	for p := range declared {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		rec, ok := recordedBy[path]
		if !ok {
			out = append(out, fmt.Sprintf("target %q ships %s and the provenance bundle records no digest for it", declared[path], path))
			continue
		}
		if rec.Target != declared[path] {
			out = append(out, fmt.Sprintf("%s is recorded against target %q and declared by %q", path, rec.Target, declared[path]))
		}
		got, err := hash(path)
		if err != nil {
			out = append(out, fmt.Sprintf("cannot hash %s to check it against its recorded digest: %v", path, err))
			continue
		}
		if got != rec.SHA256 {
			out = append(out, fmt.Sprintf("%s hashes to %s and the provenance bundle records %s, so the shipped bytes are not the bytes this release was recorded from", path, shortHex(got), shortHex(rec.SHA256)))
		}
	}
	return out
}

// VersionParityComplaints is §74's "image and binary version parity",
// stated as the thing that can actually be decided from these files.
//
// The shape follows registryDigestComplaints deliberately, because the
// two facts are the same fact. Until a release is pushed, the manifest's
// version is a build stamp (`git describe --tags --always`, which in a
// repository with no tags is an abbreviated commit) and the tag every
// provider package advertises is a semantic version that resolves
// nowhere. The moment a push happens, the two must be the same string,
// or `docker run ghcr.io/spdrman/backup-manager:1.0.0 /backup-manager
// version` answers with a commit SHA that the listing never mentions.
func VersionParityComplaints(published bool, canonicalTag, manifestVersion, bundleVersion string, versionIsABuildStamp bool) []string {
	var out []string
	if canonicalTag == "" {
		return []string{"canonical.json declares no image tag, so there is no semantic version for anything to be checked against"}
	}
	if bundleVersion != canonicalTag {
		out = append(out, fmt.Sprintf("the provenance bundle's semanticVersion is %q and canonical.json's image tag is %q; the bundle has to describe the release the packages point at", bundleVersion, canonicalTag))
	}
	if manifestVersion == "" {
		out = append(out, "container/release-manifest.json records no version at all, so nothing identifies what the recorded binaries answer `version` with")
		return out
	}
	if got := manifestVersion != canonicalTag; got != versionIsABuildStamp {
		out = append(out, fmt.Sprintf("the bundle records versionIsABuildStamp=%t while the manifest's version %q and the canonical tag %q %s; the record has to match the files",
			versionIsABuildStamp, manifestVersion, canonicalTag, map[bool]string{true: "do differ", false: "are the same"}[got]))
	}
	if published && manifestVersion != canonicalTag {
		out = append(out, fmt.Sprintf("canonical.json says the image is published as %q and container/release-manifest.json records the built binaries as %q, so the tag a store advertises and the version the binaries answer with are different strings",
			canonicalTag, manifestVersion))
	}
	return out
}

// BundleTiedToManifestComplaints checks the one link that stops the two
// halves of the release record drifting apart.
func BundleTiedToManifestComplaints(p Provenance, manifestSHA256 string, m ReleaseManifest) []string {
	var out []string
	if p.Schema != ProvenanceSchema {
		out = append(out, fmt.Sprintf("the provenance bundle declares schema %q, and this code reads %q", p.Schema, ProvenanceSchema))
	}
	if p.ReleaseManifest.Path != ManifestPath {
		out = append(out, fmt.Sprintf("the provenance bundle pairs itself with %q, and the release manifest is %q", p.ReleaseManifest.Path, ManifestPath))
	}
	if p.ReleaseManifest.SHA256 != manifestSHA256 {
		out = append(out, fmt.Sprintf("the provenance bundle records the release manifest as %s and it hashes to %s: the manifest was regenerated without regenerating the bundle, so the SBOM, the licence inventory and the artifact digests describe a different release from the binary hashes",
			shortHex(p.ReleaseManifest.SHA256), shortHex(manifestSHA256)))
	}
	if p.ReleaseManifest.Commit != m.Commit {
		out = append(out, fmt.Sprintf("the provenance bundle pins commit %s and the release manifest pins %s", shortHex(p.ReleaseManifest.Commit), shortHex(m.Commit)))
	}
	if strings.Join(p.ReleaseManifest.Architectures, ",") != strings.Join(m.ArchitectureSet(), ",") {
		out = append(out, fmt.Sprintf("the provenance bundle records architectures %v and the release manifest records %v", p.ReleaseManifest.Architectures, m.ArchitectureSet()))
	}
	return out
}

// shortHex abbreviates a digest or commit for a message. It is separate
// from the test helper of the same shape because these messages are
// produced by non-test code that a release pipeline reads.
func shortHex(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// SHA256Bytes is the lowercase hex SHA-256 of a byte slice.
func SHA256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SHA256RepoFile is the SHA-256 of a repository-root-relative file.
func SHA256RepoFile(rel string) (string, error) {
	data, err := os.ReadFile(Path(rel))
	if err != nil {
		return "", err
	}
	return SHA256Bytes(data), nil
}
