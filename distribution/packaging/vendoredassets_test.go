package packaging

import (
	"os"
	"strings"
	"testing"
)

// The vendored-asset register, and NOTICE actually carrying it (#621).
//
// #621 replaced the UI's Unicode glyph icons with Font Awesome Free SVG
// paths compiled into the web bundle. Those are CC BY 4.0, which is an
// attribution licence, so shipping them obliges this project to say whose
// work it is, under what terms, where the terms can be read, and whether
// anything was changed. That obligation had nowhere to live: NOTICE is
// generated from the module graph and the frontend lockfile, and vendored
// artwork is in neither, so the file whose entire job is third-party
// attribution could not attribute the one piece of third-party material
// this repository carries in its own source.
//
// These are the tests for closing that. The order below is the order they
// matter in: first that the tree is swept at all, then that a half-filled
// declaration is refused, then that the declaration is checked against the
// files rather than believed, and last that NOTICE carries the result.
//
// The one that cannot be satisfied by declaring nothing is
// TestTheTreeCarriesNoArtworkNothingAttributes. Every other check here
// reads the register, so a register with no entries in it passes all of
// them for the wrong reason; that one reads the TREE, and it is red until
// the material in the tree is declared. It is the anchor and the rest are
// the detail.

// vendoredFixture is a complete, well-formed entry. Every negative row
// below is this with one field taken away, which is what keeps the table
// a test of one rule at a time rather than of whatever the author
// happened to leave out.
func vendoredFixture() VendoredAsset {
	return VendoredAsset{
		ID:             "example-artwork",
		Name:           "Example Artwork",
		Version:        "1.2.3",
		Creator:        "Example, Inc.",
		Copyright:      "Copyright 2026 Example, Inc.",
		SPDXID:         "CC-BY-4.0",
		LicenceName:    "Creative Commons Attribution 4.0 International",
		LicenceTextURL: "https://creativecommons.org/licenses/by/4.0/",
		Modified:       false,
		ModifiedNote:   "reproduced verbatim and unmodified",
		WhatShips:      "two paths, compiled into the bundle",
		CarriedAs:      VendoredCarriedAsSource,
		VendoredInto:   []string{"src/art.tsx"},
		Marker:         "Example Artwork 1.2.3",
		LinkedInto:     []string{"backup-manager-web"},
		SourceURL:      "https://example.invalid/artwork-1.2.3.tgz",
		RecordedIn:     []string{"NOTICE"},
	}
}

// vendoredArtifact is a body that carries everything the fixture's
// attribution needs. A row that wants to prove one string missing takes
// it out of this.
func vendoredArtifact(v VendoredAsset) string {
	return strings.Join([]string{
		v.Name + " " + v.Version, v.Creator, v.Copyright,
		v.SPDXID, v.LicenceName, v.LicenceTextURL, v.ModifiedNote,
	}, "\n")
}

func complianceWithVendored(assets ...VendoredAsset) Compliance {
	var c Compliance
	c.License.VendoredAssets = assets
	return c
}

// TestVendoredAssetComplaints leads with the positive control, for the
// reason TestLinkComplaints gives about its own: a table of negatives
// that all pass is exactly what a rule refusing nothing looks like.
func TestVendoredAssetComplaints(t *testing.T) {
	full := vendoredFixture()

	without := func(edit func(*VendoredAsset)) VendoredAsset {
		v := vendoredFixture()
		edit(&v)
		return v
	}

	cases := []struct {
		name  string
		asset VendoredAsset
		files map[string]string
		want  string // "" means no complaint
	}{
		{
			name:  "a complete entry the tree bears out",
			asset: full,
			files: map[string]string{"src/art.tsx": "// Example Artwork 1.2.3", "NOTICE": vendoredArtifact(full)},
			want:  "",
		},
		{
			name:  "no version, so nobody can tell which release is in the product",
			asset: without(func(v *VendoredAsset) { v.Version = "" }),
			files: map[string]string{"src/art.tsx": "// Example Artwork 1.2.3", "NOTICE": vendoredArtifact(full)},
			want:  "names no version",
		},
		{
			name:  "no creator, which is the first thing the licence asks for",
			asset: without(func(v *VendoredAsset) { v.Creator = "" }),
			files: map[string]string{"src/art.tsx": "// Example Artwork 1.2.3", "NOTICE": vendoredArtifact(full)},
			want:  "names no creator",
		},
		{
			name:  "no address for the licence text, so the id is a name and not terms",
			asset: without(func(v *VendoredAsset) { v.LicenceTextURL = "" }),
			files: map[string]string{"src/art.tsx": "// Example Artwork 1.2.3", "NOTICE": vendoredArtifact(full)},
			want:  "says nowhere a recipient can read the licence text",
		},
		{
			name:  "no modification statement, which is asked for either way",
			asset: without(func(v *VendoredAsset) { v.ModifiedNote = "" }),
			files: map[string]string{"src/art.tsx": "// Example Artwork 1.2.3", "NOTICE": vendoredArtifact(full)},
			want:  "makes no statement about modification",
		},
		{
			name:  "a licence expression rather than a decided licence",
			asset: without(func(v *VendoredAsset) { v.SPDXID = "(CC-BY-4.0 AND OFL-1.1 AND MIT)" }),
			files: map[string]string{"src/art.tsx": "// Example Artwork 1.2.3", "NOTICE": vendoredArtifact(full)},
			want:  "which is an expression rather than a decided licence",
		},
		{
			name:  "a file it claims to be vendored into that is not there",
			asset: full,
			files: map[string]string{"NOTICE": vendoredArtifact(full)},
			want:  "which is not in the tree",
		},
		{
			// The upgrade case, and the reason the marker exists at all.
			// Bump the artwork and leave the register alone and this is
			// what it looks like from here.
			name:  "a file that stopped carrying the version the register names",
			asset: full,
			files: map[string]string{"src/art.tsx": "// Example Artwork 2.0.0", "NOTICE": vendoredArtifact(full)},
			want:  "either the material was upgraded without this register following it",
		},
		{
			name:  "an artifact that records the attribution without the creator in it",
			asset: full,
			files: map[string]string{
				"src/art.tsx": "// Example Artwork 1.2.3",
				"NOTICE":      strings.ReplaceAll(vendoredArtifact(full), full.Creator, "somebody"),
			},
			want: "never carries",
		},
		{
			name:  "an artifact that names the licence and never says where to read it",
			asset: full,
			files: map[string]string{
				"src/art.tsx": "// Example Artwork 1.2.3",
				"NOTICE":      strings.ReplaceAll(vendoredArtifact(full), full.LicenceTextURL, ""),
			},
			want: "the address of the licence text",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VendoredAssetComplaints(complianceWithVendored(tc.asset), readerFor(tc.files))
			// The sweep of the real tree runs inside the same call and
			// has nothing to do with this fixture, so only complaints
			// about this entry are this table's business.
			var mine []string
			for _, complaint := range got {
				if !strings.Contains(complaint, "compliance.json claims it") {
					mine = append(mine, complaint)
				}
			}
			if tc.want == "" {
				if len(mine) != 0 {
					t.Fatalf("a complete entry the tree bears out was refused:\n  %s", strings.Join(mine, "\n  "))
				}
				return
			}
			for _, complaint := range mine {
				if strings.Contains(complaint, tc.want) {
					return
				}
			}
			t.Fatalf("no complaint mentioned %q; got:\n  %s", tc.want, strings.Join(mine, "\n  "))
		})
	}
}

// TestAVendoredCheckWithNoReaderRefuses is the empty-reading rule this
// package already applies to the obligation check. A check handed no way
// to read anything has not verified an attribution; it has skipped one.
func TestAVendoredCheckWithNoReaderRefuses(t *testing.T) {
	if got := VendoredAssetComplaints(complianceWithVendored(vendoredFixture()), nil); len(got) == 0 {
		t.Fatal("a vendored-asset check with no reader returned no complaints, so an unread artifact passed for a discharged obligation")
	}
}

// TestTheTreeCarriesNoArtworkNothingAttributes is the anchor.
//
// Everything else in this file reads the register, so a register with
// nothing in it satisfies all of them. This one reads the tree: any file
// under the UI's source carrying somebody else's licence has to be
// claimed by a declared asset. It is the direction that cannot be
// satisfied by declaring less, and it is the one that was red when this
// file was written, because ui/shared/src/design-system/icons.tsx carries
// Font Awesome Free artwork under CC BY 4.0 and compliance.json declared
// nothing at all.
func TestTheTreeCarriesNoArtworkNothingAttributes(t *testing.T) {
	c := MustLoadCompliance()
	for _, complaint := range VendoredAssetComplaints(c, RepoReader()) {
		t.Errorf("%s", complaint)
	}
}

// TestTheVendoredSweepActuallyReadsTheTree is that anchor's own control.
//
// An empty result has two explanations and "the sweep walked nothing" is
// the one that would make the check above pass against a tree with
// undeclared artwork all over it. So: the sweep is pointed at a tree it
// is known to find something in, with the claim removed, and has to
// object.
func TestTheVendoredSweepActuallyReadsTheTree(t *testing.T) {
	got := undeclaredVendoredMaterial(map[string]string{})
	if len(got) == 0 {
		t.Fatal("the vendored-material sweep found nothing in this tree with nothing declared, so it cannot be what stops undeclared material shipping; ui/shared/src/design-system/icons.tsx carries Font Awesome artwork under CC BY 4.0 and should have been reported")
	}
	found := false
	for _, complaint := range got {
		if strings.Contains(complaint, "design-system/icons.tsx") {
			found = true
		}
	}
	if !found {
		t.Errorf("the sweep reported %d file(s) and none of them was the icon registry, which is the file this project actually vendors artwork into:\n  %s",
			len(got), strings.Join(got, "\n  "))
	}
}

// TestTheShippedNoticeAttributesEveryVendoredAsset is Apache-2.0 §4(d)
// and CC BY 4.0 §3(a) asked of the file a recipient actually reads.
//
// It reads the checked-in NOTICE rather than the render, deliberately.
// The render is already pinned byte-for-byte by
// TestComplianceArtifactsMatchThisTree; what this asks is the question a
// recipient asks, which is whether the file in their hands says whose
// artwork this is.
func TestTheShippedNoticeAttributesEveryVendoredAsset(t *testing.T) {
	c := MustLoadCompliance()
	if len(c.License.VendoredAssets) == 0 {
		t.Fatal("compliance.json declares no vendored assets, so this check measured nothing; the tree carries Font Awesome Free artwork under CC BY 4.0 and NOTICE has to attribute it")
	}
	notice, err := os.ReadFile(Path(c.License.NoticeFile))
	if err != nil {
		t.Fatalf("compliance.json declares %s and it is not in the tree: %v", c.License.NoticeFile, err)
	}
	body := string(notice)
	for _, v := range c.License.VendoredAssets {
		for _, missing := range v.missingFrom(body) {
			t.Errorf("%s never carries %s's %s", c.License.NoticeFile, v.Name, missing)
		}
		if !strings.Contains(body, v.WhatShips) {
			t.Errorf("%s does not say which part of %s ships, so it claims either more or less than this product actually carries", c.License.NoticeFile, v.Name)
		}
	}
}

// TestNoticeStillSaysWhatItsInventoryDoesAndDoesNotCover.
//
// NOTICE's header used to say the complete inventory of third-party
// software was provenance/third-party-licenses.json, and once vendored
// artwork ships that sentence is false: the inventory is derived from a
// module graph and a lockfile and cannot see material this repository
// carries in its own source. A NOTICE that points a reader at a file
// which does not have the thing they are looking for is worse than one
// that says nothing, so the two have to agree about which covers what.
func TestNoticeStillSaysWhatItsInventoryDoesAndDoesNotCover(t *testing.T) {
	c := MustLoadCompliance()
	notice, err := os.ReadFile(Path(c.License.NoticeFile))
	if err != nil {
		t.Fatalf("cannot read %s: %v", c.License.NoticeFile, err)
	}
	body := string(notice)
	if !strings.Contains(body, c.License.Inventory) {
		t.Errorf("%s never names %s, so a recipient is not told where the dependency inventory is", c.License.NoticeFile, c.License.Inventory)
	}
	if len(c.License.VendoredAssets) == 0 {
		return
	}
	if strings.Contains(body, "The complete inventory") {
		t.Errorf("%s still calls %s the complete inventory, and it is not: %d piece(s) of vendored material ship that no module graph and no lockfile can report",
			c.License.NoticeFile, c.License.Inventory, len(c.License.VendoredAssets))
	}
}

// TestTheVendoredRegisterAndTheHandWrittenRecordAgree.
//
// The register's RecordedIn artifacts are read by
// VendoredAssetComplaints, and NOTICE among them is not an independent
// proof: it is rendered from this register, so it cannot disagree with
// it. The hand-written record is the arm that can, and this is the test
// that says so out loud rather than leaving the pair looking like two
// proofs when it is one and a half.
func TestTheVendoredRegisterAndTheHandWrittenRecordAgree(t *testing.T) {
	c := MustLoadCompliance()
	if len(c.License.VendoredAssets) == 0 {
		t.Fatal("compliance.json declares no vendored assets, so there was nothing to agree with")
	}
	for _, v := range c.License.VendoredAssets {
		handWritten := 0
		for _, rel := range v.RecordedIn {
			if rel == c.License.NoticeFile {
				continue
			}
			handWritten++
			data, err := os.ReadFile(Path(rel))
			if err != nil {
				t.Errorf("%s names %s as a record of its attribution and it is not in the tree: %v", v.Name, rel, err)
				continue
			}
			for _, missing := range v.missingFrom(string(data)) {
				t.Errorf("%s never carries %s's %s", rel, v.Name, missing)
			}
		}
		if handWritten == 0 {
			t.Errorf("%s is recorded only in %s, which is generated from this same register and therefore cannot disagree with it; the attribution needs at least one artifact somebody wrote", v.Name, c.License.NoticeFile)
		}
	}
}

// The rest of this file is #631's half: a second kind of vendored
// material arrived, the upstream's own files redistributed rather than
// reproduced into a component this project writes, and the two kinds
// cannot be held to the same evidence. The cases above drive the source
// kind; these drive the other one and the boundary between them.

// vendoredFilesFixture is the complete, well-formed entry for material
// redistributed byte for byte, and the bodies its digests are taken from.
func vendoredFilesFixture() (VendoredAsset, map[string]string) {
	const face = "the bytes of a woff2 file"
	const licence = "SIL OPEN FONT LICENSE Version 1.1\nCopyright 1999 Example Foundry with Reserved Font Name \"Example\""
	v := VendoredAsset{
		ID:             "example-webfont",
		Name:           "Example Sans",
		Version:        "4.5.6",
		Creator:        "Example Foundry",
		Copyright:      "Copyright 1999 Example Foundry with Reserved Font Name \"Example\"",
		SPDXID:         "OFL-1.1",
		LicenceName:    "SIL Open Font License 1.1",
		LicenceTextURL: "https://example.invalid/ofl",
		Modified:       false,
		ModifiedNote:   "reproduced verbatim and unmodified",
		WhatShips:      "the Latin subset of the regular face",
		CarriedAs:      VendoredCarriedAsFiles,
		VendoredInto:   []string{"ui/shared/public/fonts/Example-Regular.woff2"},
		Digests:        map[string]string{"ui/shared/public/fonts/Example-Regular.woff2": SHA256Bytes([]byte(face))},
		LicenceFile:    "ui/shared/public/fonts/LICENSE.txt",
		LinkedInto:     []string{"backup-manager-web"},
		SourceURL:      "https://example.invalid/example-sans-4.5.6.tgz",
		RecordedIn:     []string{"NOTICE"},
	}
	files := map[string]string{
		"ui/shared/public/fonts/Example-Regular.woff2": face,
		"ui/shared/public/fonts/LICENSE.txt":           licence,
		"NOTICE":                                       vendoredArtifact(v),
	}
	return v, files
}

// mine drops the tree sweep's complaints, which arrive from the same call
// and have nothing to do with a synthetic fixture. Same filter the table
// above uses, and both sweeps share the phrase on purpose.
func mine(complaints []string) []string {
	var out []string
	for _, c := range complaints {
		if !strings.Contains(c, "compliance.json claims it") {
			out = append(out, c)
		}
	}
	return out
}

// TestTheTwoKindsOfVendoredMaterialAreHeldToDifferentEvidence.
//
// The positive control leads, and then every row is one edit away from
// it. The four rows in the middle are the interesting ones: each kind is
// refused for carrying the OTHER kind's evidence, not merely allowed to
// omit it. A digest over a file this project maintains and a marker
// inside a compressed binary are both checks that cannot fail, and a
// check that cannot fail is worse than an absent one, because it reads
// like coverage.
func TestTheTwoKindsOfVendoredMaterialAreHeldToDifferentEvidence(t *testing.T) {
	const path = "ui/shared/public/fonts/Example-Regular.woff2"

	cases := []struct {
		name string
		edit func(*VendoredAsset, map[string]string)
		want string
	}{
		{"a complete entry over the bytes it records", nil, ""},
		{
			"a file re-encoded under an unchanged digest",
			func(_ *VendoredAsset, f map[string]string) { f[path] = "somebody else's re-subsetting" },
			"the upstream's own bytes",
		},
		{
			"a redistributed file with no digest at all",
			func(v *VendoredAsset, _ map[string]string) { v.Digests = nil },
			"records no digest for them",
		},
		{
			"a digest for a file this entry does not ship",
			func(v *VendoredAsset, _ map[string]string) {
				v.Digests["ui/shared/public/fonts/Someone-Else.woff2"] = SHA256Bytes(nil)
			},
			"not one of the files it says it is vendored into",
		},
		{
			"source material carrying a digest anyway",
			func(v *VendoredAsset, _ map[string]string) {
				v.CarriedAs = VendoredCarriedAsSource
				v.Marker = "Example Sans 4.5.6"
			},
			"goes stale on the next edit",
		},
		{
			"redistributed bytes carrying a marker anyway",
			func(v *VendoredAsset, _ map[string]string) { v.Marker = "Example Sans 4.5.6" },
			"could never fail",
		},
		{
			"redistributed bytes with no licence text beside them",
			func(v *VendoredAsset, _ map[string]string) { v.LicenceFile = "" },
			"ships no copy of the licence text",
		},
		{
			"a licence text that does not carry the copyright notice",
			func(_ *VendoredAsset, f map[string]string) {
				f["ui/shared/public/fonts/LICENSE.txt"] = "SIL OPEN FONT LICENSE Version 1.1"
			},
			"does not carry the notice",
		},
		{
			"a licence text that is not in the tree",
			func(_ *VendoredAsset, f map[string]string) { delete(f, "ui/shared/public/fonts/LICENSE.txt") },
			"ships its licence text at",
		},
		{
			"a redistributed file that is not in the tree",
			func(_ *VendoredAsset, f map[string]string) { delete(f, path) },
			"which is not in the tree",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, files := vendoredFilesFixture()
			if tc.edit != nil {
				tc.edit(&v, files)
			}
			got := mine(VendoredAssetComplaints(complianceWithVendored(v), readerFor(files)))
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("a complete entry over the bytes it records was refused:\n  %s", strings.Join(got, "\n  "))
				}
				return
			}
			for _, complaint := range got {
				if strings.Contains(complaint, tc.want) {
					return
				}
			}
			t.Fatalf("no complaint mentioned %q; got:\n  %s", tc.want, strings.Join(got, "\n  "))
		})
	}
}

// TestAnIncompleteEntryDoesNotAlsoReportItsOwnFilesAsUndeclared.
//
// An entry missing one field used to report that field and then every
// file it covers as undeclared material, because the sweep's claim was
// registered after the completeness check rather than before it. One
// accurate complaint arrived under several that would go away on their
// own the moment it was fixed, which sends somebody declaring files that
// are already declared. The sweep's question is whether anybody has
// declared this file, and somebody has.
func TestAnIncompleteEntryDoesNotAlsoReportItsOwnFilesAsUndeclared(t *testing.T) {
	c := MustLoadCompliance()
	if len(c.License.VendoredAssets) == 0 {
		t.Fatal("the register is empty, so this measured nothing")
	}
	// Break the real register's first entry in a way that has nothing to
	// do with its files, and drive the whole check over the real tree.
	c.License.VendoredAssets[0].Creator = ""

	var undeclared []string
	for _, complaint := range VendoredAssetComplaints(c, RepoReader()) {
		if strings.Contains(complaint, "compliance.json claims it") {
			undeclared = append(undeclared, complaint)
		}
	}
	if len(undeclared) != 0 {
		t.Errorf("an entry missing its creator also reported %d of its own file(s) as undeclared, which buries the one complaint that is true:\n  %s",
			len(undeclared), strings.Join(undeclared, "\n  "))
	}
}

// TestEveryRedistributedFileIsClaimed is the binary half of the
// direction that cannot be satisfied by declaring less.
//
// The marker sweep asks whether a file says it carries somebody else's
// material, and a woff2 cannot answer: it is compressed, so it holds no
// searchable string and the sweep reads straight past it. A second
// typeface dropped into the fonts directory would ship attributed to
// nobody with every other check in this file green. So redistributed
// binaries are held to a stronger rule, and this is that rule's own
// control: pointed at the real directory with nothing claimed, it has to
// object.
func TestEveryRedistributedFileIsClaimed(t *testing.T) {
	got := sweepForUnclaimedFiles("ui/shared/public/fonts", map[string]string{})
	if len(got) == 0 {
		t.Fatal("the sweep found nothing in ui/shared/public/fonts with nothing declared, so it cannot be what stops an undeclared typeface shipping")
	}
	found := false
	for _, complaint := range got {
		if strings.Contains(complaint, ".woff2") {
			found = true
		}
	}
	if !found {
		t.Errorf("the sweep reported %d file(s) and none of them was a woff2, which is the thing this product actually redistributes:\n  %s",
			len(got), strings.Join(got, "\n  "))
	}
}

// TestTheMarkerListWouldCatchTheLicencesThisProjectActuallyCarries.
//
// The marker list started as the icon artwork's licence and nothing else,
// and that was a hole rather than a scope. #631 vendored OFL-1.1 font
// files, whose own licence text says "SIL OPEN FONT LICENSE" and never
// says "CC BY 4.0" or "Font Awesome", so the sweep would have watched
// them arrive undeclared and reported a clean tree.
//
// It is driven off the real licence text this repository ships rather
// than off a string written here, so it cannot be satisfied by a list
// that happens to contain whatever this test also contains.
func TestTheMarkerListWouldCatchTheLicencesThisProjectActuallyCarries(t *testing.T) {
	for _, rel := range []string{
		"ui/shared/public/fonts/LICENSE.txt",
		"ui/shared/src/design-system/typography.css",
		"ui/shared/src/design-system/icons.tsx",
	} {
		data, err := os.ReadFile(Path(rel))
		if err != nil {
			t.Errorf("%s carries third-party licensing material and is not in the tree: %v", rel, err)
			continue
		}
		matched := ""
		for _, marker := range vendoredAttributionMarkers {
			if strings.Contains(string(data), marker) {
				matched = marker
				break
			}
		}
		if matched == "" {
			t.Errorf("%s carries somebody else's licence and none of the %d marker(s) the sweep looks for appears in it, so vendoring material like this and forgetting the paperwork would be a green build",
				rel, len(vendoredAttributionMarkers))
		}
	}
}

// TestTheSweepReachesTheProviderShells.
//
// vendoredScanRoots covered ui/shared only, and every provider shell in
// apps/*/frontend is code that draws things and could carry vendored
// material just as easily. An unexpanded or unmatched pattern is a
// complaint rather than an empty result, for the reason this whole file
// keeps returning to: a sweep that walks nothing reports a clean tree.
func TestTheSweepReachesTheProviderShells(t *testing.T) {
	roots, err := expandScanRoot("apps/*/frontend")
	if err != nil {
		t.Fatalf("the provider shells are not swept for vendored material: %v", err)
	}
	if len(roots) < 2 {
		t.Errorf("apps/*/frontend expanded to %d root(s); this repository ships a shell per provider and the sweep should reach all of them", len(roots))
	}
	if _, err := expandScanRoot("apps/*/nothing-is-here"); err == nil {
		t.Error("a scan root matching nothing was accepted, so a directory that moves takes the sweep with it silently")
	}
}
