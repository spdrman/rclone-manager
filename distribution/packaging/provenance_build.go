// The generator behind provenance/, issue #88 (B5.2).
//
// §73 WP5.2's REFACTOR step asks for one release step rather than an SBOM
// step, a checksum step and a manifest step that each rediscover the same
// facts. This is that step, and it is a library function rather than a
// shell pipeline for one reason: the check that an undeclared dependency
// gets caught IS this function, run again and compared. A generator you
// cannot call from a test can only be verified by reading its output,
// which is how an inventory ends up describing last quarter's go.mod.
//
// Nothing here writes anything. Generate returns bytes; apps/common's
// cmd/provenance writes them and provenance_test.go compares them against
// what is checked in. That split is what makes the drift check possible.
package packaging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GeneratedProvenance is one generation's complete output.
type GeneratedProvenance struct {
	Inventory []byte
	SBOM      []byte
	Checksums []byte
	Bundle    []byte
	Notice    []byte
}

// Files maps each generated artifact to its repository-root-relative
// path, in the order a writer should create them.
func (g GeneratedProvenance) Files() []struct {
	Path string
	Data []byte
} {
	return []struct {
		Path string
		Data []byte
	}{
		{"NOTICE", g.Notice},
		{InventoryPath, g.Inventory},
		{SBOMPath, g.SBOM},
		{ChecksumsPath, g.Checksums},
		{ProvenancePath, g.Bundle},
	}
}

// inventoryNote and provenanceNote are written INTO the generated files,
// so the argument for what is in them reaches whoever opens the artifact
// rather than only whoever opens this package. A generated file that says
// nothing about how it was derived gets hand-edited the first time it
// disagrees with somebody, and these two are evidence, so a hand edit is
// exactly the thing that must not happen quietly.
var inventoryNote = []string{
	"Generated. Do not hand-edit: run `go run ./cmd/provenance -write` from apps/common.",
	"",
	"Every Go component below is a module `go list -deps` reports as linked into one",
	"of the two binaries container/Dockerfile copies into the runtime stage, resolved",
	"for GOOS=linux on every architecture canonical.json claims. Every npm component",
	"is a non-dev entry of ui/shared/package-lock.json, which is what the frontend",
	"build stage installs and what ends up embedded in backup-manager-web.",
	"",
	"licenseSha256 is the SHA-256 of the licence text as it appears in the module,",
	"not of the SPDX id. An upstream that relicenses in place keeps its id and",
	"changes those bytes, and a bare id could not tell.",
	"",
	"TestThirdPartyInventoryMatchesTheLiveModuleGraph re-derives this whole file and",
	"fails on any difference, so an undeclared dependency is a red build rather than",
	"a discrepancy nobody looks for.",
}

var provenanceNote = []string{
	"Generated. Do not hand-edit: run `go run ./cmd/provenance -write` from apps/common.",
	"",
	"The compliance half of the release record. container/release-manifest.json is",
	"the other half, and it is a separate file because it records what a",
	"two-architecture Docker build produced and nothing else: adding a field to it",
	"that no build produces would need either a rebuild to change one string, or a",
	"no-build write path into the file that carries the build record, and the second",
	"is a hole in exactly the guard #174 put there.",
	"",
	"releaseManifest.sha256 is what stops the two halves drifting. Regenerating the",
	"manifest without regenerating this bundle changes that digest and",
	"TestProvenanceBundleIsTiedToTheReleaseManifest goes red.",
}

// noticeHeader is the fixed part of NOTICE.
//
// The last %s is the one sentence in it that is conditional, and it is
// conditional on a fact rather than on taste. This file used to call the
// inventory "the complete inventory" of third-party software, which was
// true while every piece of it arrived through a package manager. Once
// this project vendors somebody else's material into its own source
// (#621), it stops being true: the inventory is derived from a module
// graph and a lockfile and neither can report a file this repository
// carries. Pointing a reader at a file that does not have the thing they
// came for is worse than pointing them nowhere, so the sentence appears
// exactly when there is something it has to warn them about.
const noticeHeader = `%s
%s

This product includes third-party software. Every dependency, with its version,
the SPDX identifier of its licence and the SHA-256 of the licence text as that
component ships it, is listed in %s.
It is generated from the module graph and the frontend lockfile on every run
rather than maintained by hand.%s

This file is the NOTICE file Apache-2.0 section 4(d) refers to. Redistributing
this work, or a derivative of it, means carrying this file with it.
`

// noticeVendoredCaveat is that conditional sentence.
const noticeVendoredCaveat = `

Which is also why that file is not the whole picture. This project carries
some third-party material inside its own SOURCE, where no package manager can
see it and nothing can derive it from the tree, and that material is listed
under "Third-party material this project vendors into its own source" below.`

// noticeObligationHeader introduces the part of NOTICE that is an offer
// rather than an attribution.
//
// It comes before the component listing on purpose. Attribution is a
// courtesy a reader can skim; this is the section a recipient has rights
// under, and burying it after several hundred lines of module names is
// how a discharge becomes technically present and practically absent.
const noticeObligationHeader = `
Source for the components that are not permissively licensed
------------------------------------------------------------

Most of this product's dependencies are permissively licensed and carry no
obligation beyond the attribution below. The components in this section are
not, and this is where the terms say a recipient's rights live. Each one is
listed at the exact version that was compiled, with the address its complete
source is served from. Nothing here is modified or vendored by this project,
so that address serves the same source that went into the binaries, and the
inventory's licenceSha256 for each component is the SHA-256 of the licence
text inside it.
`

// noticeObligationSection renders the offer, or returns nothing when no
// component in the inventory needs one.
//
// It is derived from compliance.json and the inventory rather than
// written out, so a third encumbered module arriving cannot leave the
// offer describing two. LicenceObligationComplaints checks this file
// afterwards for exactly the strings this writes, which is what makes
// the pair a check and not a convention. It is a check on this renderer
// and not a second proof of the data, though: both read the same
// register, and TestComplianceArtifactsMatchThisTree keeps the checked-in
// NOTICE byte-identical to this render, so that arm can only fail when
// this function stops emitting a string a recipient needs. The
// hand-written source-offer.md is the artifact that can disagree.
func noticeObligationSection(c Compliance, inv Inventory) string {
	var b strings.Builder
	for _, a := range c.License.AcceptedNonPermissive {
		var affected []Component
		for _, comp := range inv.Components {
			if a.Covers(comp.LicenseID) {
				affected = append(affected, comp)
			}
		}
		if len(affected) == 0 {
			continue
		}
		if b.Len() == 0 {
			b.WriteString(noticeObligationHeader)
		}
		fmt.Fprintf(&b, "\n%s\n", a.SPDXID)
		fmt.Fprintf(&b, "  Scope:      %s\n", a.Scope)
		fmt.Fprintf(&b, "  Obligation: %s\n", a.Obligation)
		fmt.Fprintf(&b, "  Licence:    %s\n", a.LicenceTextURL)
		fmt.Fprintf(&b, "  Components (%d), and where to get each one's source:\n", len(affected))
		SortComponents(affected)
		for _, comp := range affected {
			fmt.Fprintf(&b, "    %s %s@%s\n", comp.Ecosystem, comp.Name, comp.Version)
			fmt.Fprintf(&b, "      linked into: %s\n", strings.Join(comp.LinkedInto, ", "))
			fmt.Fprintf(&b, "      source:      %s\n", a.SourceURLFor(comp))
		}
	}
	return b.String()
}

// noticeVendoredHeader introduces the vendored-material listing.
//
// It sits before the component listing and after the source offer, in the
// same position and for the same reason the offer sits where it does: it
// is a licence CONDITION rather than a courtesy, and burying a condition
// after several hundred lines of module names is how a discharge becomes
// technically present and practically absent.
const noticeVendoredHeader = `
Third-party material this project vendors into its own source
-------------------------------------------------------------

The components listed further down are derived from a module graph and a
frontend lockfile. These are not, and cannot be: they are carried inside this
repository's own source, which no package manager reports. They are declared
in distribution/packaging/compliance.json and checked against the files they
name on every run, so upgrading the material and leaving this behind is a
failed build rather than a notice that quietly describes something else.
`

// noticeVendoredSection renders the register, or nothing when the project
// vendors no third-party material.
//
// Each entry prints the five things CC BY 4.0 section 3(a)(1) asks an
// attribution to carry (the creator, the copyright notice, the licence,
// a URI to its text, and a statement about modification) and then the
// three that make it useful rather than merely compliant: which part of
// the upstream material actually ships, which file in this repository
// carries it, and where the hand-written record is. The first of those
// three matters most here. Font Awesome Free is three licences at once
// and only the icons are used, so a notice that named the project and
// not the part would be claiming obligations this project does not have
// and hiding the one it does.
func noticeVendoredSection(c Compliance) string {
	if len(c.License.VendoredAssets) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(noticeVendoredHeader)
	// One label width for the whole block, derived from the longest label
	// rather than counted by hand, so adding a row cannot silently leave
	// one value out of the column.
	row := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		fmt.Fprintf(&b, "  %-*s %s\n", noticeVendoredLabelWidth, label+":", value)
	}
	for _, v := range c.License.VendoredAssets {
		fmt.Fprintf(&b, "\n%s %s\n", v.Name, v.Version)
		row("Creator", v.Creator)
		row("Copyright", v.Copyright)
		licence := v.SPDXID
		if v.LicenceName != "" {
			licence += ", " + v.LicenceName
		}
		row("Licence", licence)
		row("Licence text", v.LicenceTextURL)
		// Where the licence text sits INSIDE the artifact, for the
		// material whose licence asks to travel with every copy rather
		// than to be linked. A recipient holding the image can open this
		// path; the URL above needs the network this product is built to
		// do without.
		row("Licence copy", v.LicenceFile)
		row("Modification", v.ModifiedNote)
		row("What ships", v.WhatShips)
		row("Carried in", strings.Join(v.VendoredInto, ", "))
		row("Linked into", strings.Join(v.LinkedInto, ", "))
		row("Upstream", v.SourceURL)
		// NOTICE is one of the recordedIn artifacts and is this file, so
		// pointing a reader here from here would be a loop rather than a
		// reference.
		var elsewhere []string
		for _, rel := range v.RecordedIn {
			if rel != c.License.NoticeFile {
				elsewhere = append(elsewhere, rel)
			}
		}
		row("Also stated in", strings.Join(elsewhere, ", "))
	}
	return b.String()
}

// noticeVendoredLabelWidth is the width of the label column in the block
// above, including the colon. It is the longest label this project writes
// and nothing computes it, which is the one hand-maintained number here;
// a label longer than this pushes its own value right rather than
// breaking anything, so the failure mode is cosmetic and visible.
const noticeVendoredLabelWidth = 15

// noticeComponentsHeader introduces the attribution listing.
const noticeComponentsHeader = `
Components by licence
---------------------
`

// buildNotice renders NOTICE from the inventory.
func buildNotice(c Compliance, inv Inventory) []byte {
	var b strings.Builder
	caveat := ""
	if len(c.License.VendoredAssets) > 0 {
		caveat = noticeVendoredCaveat
	}
	fmt.Fprintf(&b, noticeHeader, c.Project.DisplayName, c.Project.Copyright, c.License.Inventory, caveat)
	b.WriteString(noticeObligationSection(c, inv))
	b.WriteString(noticeVendoredSection(c))
	b.WriteString(noticeComponentsHeader)

	byLicence := map[string][]string{}
	for _, comp := range inv.Components {
		id := comp.LicenseID
		if id == "" {
			id = noAssertion
		}
		byLicence[id] = append(byLicence[id], fmt.Sprintf("  %s %s@%s", comp.Ecosystem, comp.Name, comp.Version))
	}
	ids := make([]string, 0, len(byLicence))
	for id := range byLicence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		lines := byLicence[id]
		sort.Strings(lines)
		fmt.Fprintf(&b, "\n%s (%d)\n", id, len(lines))
		for _, l := range lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

// goComponents derives the Go half of the inventory from the live module
// graph.
//
// It asks once per binary per claimed architecture and unions the
// answers, then records which binaries each module reaches. Asking per
// architecture rather than once is not defensive padding: build
// constraints are per-GOARCH, so a module that only the arm64 build links
// is invisible to an amd64-only enumeration, and the release ships both.
func goComponents(architectures []string) ([]Component, error) {
	type key struct{ path, version string }
	found := map[key]GoModuleRef{}
	reaches := map[key]map[string]bool{}

	for _, target := range ShippedGoBinaries {
		for _, arch := range architectures {
			refs, err := GoLinkedModules(target, "linux", arch)
			if err != nil {
				return nil, err
			}
			for _, r := range refs {
				k := key{r.Path, r.Version}
				found[k] = r
				if reaches[k] == nil {
					reaches[k] = map[string]bool{}
				}
				reaches[k][target.Binary] = true
			}
		}
	}

	out := make([]Component, 0, len(found))
	for k, ref := range found {
		comp := Component{
			Name:      k.path,
			Version:   k.version,
			Ecosystem: EcosystemGo,
		}
		if ref.Dir == "" {
			return nil, fmt.Errorf("go list reports no directory for %s@%s, so its licence cannot be read; run `go mod download` first", k.path, k.version)
		}
		name := findLicenseFile(ref.Dir)
		if name == "" {
			return nil, fmt.Errorf("%s@%s ships no recognisable licence file in %s, so this release cannot state its terms", k.path, k.version, ref.Dir)
		}
		data, err := os.ReadFile(filepath.Join(ref.Dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s@%s licence: %w", k.path, k.version, err)
		}
		comp.LicenseFile = name
		comp.LicenseSHA256 = SHA256Bytes(data)
		comp.LicenseID = ClassifyLicense(string(data))

		binaries := make([]string, 0, len(reaches[k]))
		for b := range reaches[k] {
			binaries = append(binaries, b)
		}
		sort.Strings(binaries)
		comp.LinkedInto = binaries
		out = append(out, comp)
	}
	SortComponents(out)
	return out, nil
}

// marshal renders a document the way every generated file here is
// rendered: two-space indent and a trailing newline, so the output is a
// well-behaved text file and a diff is line-oriented.
func marshal(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// GenerateProvenance derives every compliance artifact from the tree.
//
// It reads container/release-manifest.json but never writes it, and it
// takes the SBOM's creation timestamp from that file rather than from the
// clock, so two runs over an unchanged tree produce identical bytes.
func GenerateProvenance() (GeneratedProvenance, error) {
	var g GeneratedProvenance

	c, err := LoadCompliance()
	if err != nil {
		return g, err
	}
	canonical, err := Load()
	if err != nil {
		return g, err
	}
	manifestRaw, err := os.ReadFile(Path(ManifestPath))
	if err != nil {
		return g, fmt.Errorf("read %s: %w", ManifestPath, err)
	}
	manifest, err := ParseReleaseManifest(manifestRaw)
	if err != nil {
		return g, fmt.Errorf("parse %s: %w", ManifestPath, err)
	}

	components, err := goComponents(canonical.Architectures)
	if err != nil {
		return g, err
	}
	lockRaw, err := os.ReadFile(Path(UISharedLockfile))
	if err != nil {
		return g, fmt.Errorf("read %s: %w", UISharedLockfile, err)
	}
	npm, err := NPMProductionComponents(lockRaw)
	if err != nil {
		return g, err
	}
	components = append(components, npm...)
	SortComponents(components)

	inv := Inventory{
		Schema:         InventorySchema,
		Note:           inventoryNote,
		ProjectLicense: c.License.SPDXID,
		Components:     components,
	}
	if g.Inventory, err = marshal(inv); err != nil {
		return g, err
	}

	g.Notice = buildNotice(c, inv)

	sbom := BuildSPDX(inv,
		c.Project.DisplayName+" "+canonical.Image.Tag,
		fmt.Sprintf("%s/spdx/%s", c.SourceRepository.URL, manifest.Commit),
		manifest.GeneratedAt)
	if g.SBOM, err = marshal(sbom); err != nil {
		return g, err
	}

	// Artifact digests, and the checksum manifest over everything a
	// release hands out plus the compliance artifacts themselves.
	licenseText, err := os.ReadFile(Path(c.License.File))
	if err != nil {
		return g, fmt.Errorf("%s is the licence this release is distributed under and it is not in the tree: %w", c.License.File, err)
	}
	sums := map[string]string{
		c.License.File:                SHA256Bytes(licenseText),
		c.License.NoticeFile:          SHA256Bytes(g.Notice),
		InventoryPath:                 SHA256Bytes(g.Inventory),
		SBOMPath:                      SHA256Bytes(g.SBOM),
		ManifestPath:                  SHA256Bytes(manifestRaw),
		"ui/shared/package-lock.json": SHA256Bytes(lockRaw),
	}
	var artifacts []RecordedArtifact
	var unbuilt []UnbuiltTarget
	for _, id := range c.TargetIDs() {
		t := c.Distribution.Targets[id]
		if t.UnbuiltReason != "" {
			unbuilt = append(unbuilt, UnbuiltTarget{Target: id, Reason: t.UnbuiltReason})
		}
		for _, rel := range t.Artifacts {
			sum, err := SHA256RepoFile(rel)
			if err != nil {
				return g, fmt.Errorf("target %q ships %s: %w", id, rel, err)
			}
			artifacts = append(artifacts, RecordedArtifact{Target: id, Path: rel, SHA256: sum})
			sums[rel] = sum
		}
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })

	paths := make([]string, 0, len(sums))
	for p := range sums {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var checks strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&checks, "%s  %s\n", sums[p], p)
	}
	g.Checksums = []byte(checks.String())

	digests := map[string]*string{}
	for _, a := range manifest.Architectures {
		digests[a.Architecture] = a.RegistryDigest
	}

	bundle := Provenance{
		Schema:          ProvenanceSchema,
		Note:            provenanceNote,
		SemanticVersion: canonical.Image.Tag,
		ImageReference:  canonical.Image.Reference,
		ReleaseManifest: ProvenanceManifestRef{
			Path:                 ManifestPath,
			SHA256:               SHA256Bytes(manifestRaw),
			Commit:               manifest.Commit,
			RecordedBuild:        manifest.Version,
			Architectures:        manifest.ArchitectureSet(),
			Published:            canonical.Image.Published,
			Digests:              digests,
			VersionIsABuildStamp: manifest.Version != canonical.Image.Tag,
		},
		License: ProvenanceLicense{
			SPDXID:     c.License.SPDXID,
			File:       ProvenanceFile{Path: c.License.File, SHA256: sums[c.License.File]},
			Notice:     ProvenanceFile{Path: c.License.NoticeFile, SHA256: sums[c.License.NoticeFile]},
			Inventory:  ProvenanceFile{Path: InventoryPath, SHA256: sums[InventoryPath], Format: InventorySchema},
			Components: len(inv.Components),
		},
		SBOM:           ProvenanceFile{Path: SBOMPath, SHA256: sums[SBOMPath], Format: "SPDX-2.3"},
		Checksums:      ProvenanceFile{Path: ChecksumsPath, SHA256: SHA256Bytes(g.Checksums), Format: "sha256sum"},
		Artifacts:      artifacts,
		UnbuiltTargets: unbuilt,
		Links:          linkReadiness(c),
		Signing:        signingRecord(canonical),
		Performance:    c.Performance,
	}
	if g.Bundle, err = marshal(bundle); err != nil {
		return g, err
	}
	return g, nil
}

// linkReadiness turns the one recorded fact (whether the source
// repository is public) into the sentence a store reviewer's situation
// actually calls for. The negative branch is the long one on purpose: it
// separates "the materials do not exist" from "the materials exist and a
// reviewer cannot reach them", says which of those this is, names who can
// change it, and points at the written source offer that stands until
// somebody does. A bare false would read as the first.
func linkReadiness(c Compliance) LinkReadiness {
	if c.StoreReadyForPublicLinks() {
		return LinkReadiness{
			PubliclyReachable: true,
			Reason:            "the source repository is public, so every declared link resolves for a reviewer outside the project",
		}
	}
	return LinkReadiness{
		PubliclyReachable: false,
		Reason: "the source repository is " + c.SourceRepository.Visibility + ", so every link into it is a 404 for a reviewer outside the project. " +
			"The materials exist and are checked for substance on every run; what is missing is public access to them, which only the repository owner can grant. " +
			"docs/compliance/source-offer.md is the written offer that stands until then.",
	}
}

// signingRecord states what has actually been done to the image.
//
// "unsigned" is the honest answer while nothing has been pushed: there is
// no digest to sign. The method is recorded anyway, because the identity
// a verifier checks is part of the release contract and has to be settled
// before the first signature rather than discovered after it.
//
// The identity and the command both come from signing.go, which is also
// where the reason they are pinned to the release branch rather than to a
// tag is written down (issue #510). Neither is spelled out again here:
// two copies of a verification command is how one of them goes stale.
func signingRecord(canonical Canonical) SigningRecord {
	if !canonical.Image.Published {
		return SigningRecord{
			Status:       "unsigned",
			Method:       "sigstore-keyless",
			Identity:     SigningIdentity,
			Transparency: "https://rekor.sigstore.dev",
			Note: []string{
				"Nothing has been pushed to " + canonical.Image.Reference + ", so there is no digest to sign and no signature to verify.",
				"The mechanism is scripts/release/publish-image.sh; the key design is in docs/compliance/release-provenance.md.",
				"There is no signing key to store, rotate or leak: the workflow's OIDC identity is exchanged for a short-lived Fulcio certificate at signing time.",
			},
		}
	}
	return SigningRecord{
		Status:       "signed",
		Method:       "sigstore-keyless",
		Identity:     SigningIdentity,
		Transparency: "https://rekor.sigstore.dev",
		Note: []string{
			"Verify with: " + SigningVerifyCommand(canonical.Image.Reference),
		},
	}
}
