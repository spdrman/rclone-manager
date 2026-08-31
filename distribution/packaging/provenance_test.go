package packaging

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// Issue #88 (B5.2): the release's supply-chain evidence.
//
// #174 built the release manifest's own anti-drift check and left three
// things here: the push that turns a local image ID into a registry
// digest, generating the compliance record inside the release step so
// drift is structural rather than merely loud, and signing, attestation
// and an SBOM over what the digest points at. This file is the second and
// third of those, plus §73 WP5.2's licence inventory, checksums and
// version parity.
//
// The generator lives in provenance_build.go and returns bytes rather
// than writing them, which is what makes the central check here possible:
// regenerate everything from the tree and compare it, byte for byte, to
// what is checked in. An SBOM nothing re-derives is a document, not
// evidence, and the specific way it stops being evidence is silent: a
// module enters the graph, nobody regenerates, and the release ships
// stating terms it never read.

// generated caches one generation for the whole file. GenerateProvenance
// shells out to `go list -deps` four times (two binaries, two
// architectures) and several tests want the same answer.
var (
	generateOnce sync.Once
	generated    GeneratedProvenance
	generateErr  error
)

func generateForTest(t *testing.T) GeneratedProvenance {
	t.Helper()
	generateOnce.Do(func() { generated, generateErr = GenerateProvenance() })
	if generateErr != nil {
		t.Fatalf("cannot derive the release's compliance artifacts from this tree: %v", generateErr)
	}
	return generated
}

// ---------------------------------------------------------------------
// The licence classifier
// ---------------------------------------------------------------------

// Real fragments, long enough to be recognised the way a whole licence
// file is. Trimmed to the lines the classifier keys on plus enough
// surrounding text that a rule matching on something else would not be
// satisfied by accident.
const (
	gpl2Text = `                    GNU GENERAL PUBLIC LICENSE
                       Version 2, June 1991

 Copyright (C) 1989, 1991 Free Software Foundation, Inc.
 Everyone is permitted to copy and distribute verbatim copies
 of this license document, but changing it is not allowed.`

	gpl3Text = `                    GNU GENERAL PUBLIC LICENSE
                       Version 3, 29 June 2007

 Copyright (C) 2007 Free Software Foundation, Inc. <https://fsf.org/>`

	agpl3Text = `                    GNU AFFERO GENERAL PUBLIC LICENSE
                       Version 3, 19 November 2007`

	lgpl21Text = `                  GNU LESSER GENERAL PUBLIC LICENSE
                       Version 2.1, February 1999`

	mplText = `Mozilla Public License Version 2.0
==================================

1. Definitions`

	apacheText = `                                 Apache License
                           Version 2.0, January 2004
                        http://www.apache.org/licenses/

   TERMS AND CONDITIONS FOR USE, REPRODUCTION, AND DISTRIBUTION`

	mitText = `MIT License

Copyright (c) 2026 Somebody

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction.`

	bsd3Text = `Copyright (c) 2026 Somebody. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

   * Neither the name of the copyright holder nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.`

	bsd2Text = `Copyright (c) 2026 Somebody. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice.
2. Redistributions in binary form must reproduce the above copyright notice.`

	iscText = `Copyright (c) 2026 Somebody

Permission to use, copy, modify, and/or distribute this software for any purpose
with or without fee is hereby granted.`

	cc0Text = `This work is released into the public domain with CC0 1.0.

Creative Commons Legal Code

CC0 1.0 Universal`
)

// TestClassifyLicense is the table, and the copyleft rows are the reason
// it exists.
//
// The worst thing this function can do is not "fail to recognise a
// licence": that is loud, because an unidentified component is refused
// downstream. The worst thing it can do is read a copyleft text as a
// permissive one, because the project's whole Apache-2.0 choice rests on
// the graph being permissive and that answer would be laundered straight
// into NOTICE. So every GNU-family row asserts twice: the exact id, and
// that the result is on compliance.json's own copyleft list.
func TestClassifyLicense(t *testing.T) {
	c := MustLoadCompliance()

	cases := []struct {
		name         string
		text         string
		want         string
		wantCopyleft bool
	}{
		{"GPL-2.0", gpl2Text, "GPL-2.0-only", true},
		{"GPL-3.0", gpl3Text, "GPL-3.0-only", true},
		{"AGPL-3.0", agpl3Text, "AGPL-3.0-only", true},
		{"LGPL-2.1", lgpl21Text, "LGPL-2.1-only", true},
		{"MPL-2.0", mplText, "MPL-2.0", true},
		{"Apache-2.0", apacheText, "Apache-2.0", false},
		{"MIT", mitText, "MIT", false},
		{"BSD-3-Clause", bsd3Text, "BSD-3-Clause", false},
		{"BSD-2-Clause", bsd2Text, "BSD-2-Clause", false},
		{"ISC", iscText, "ISC", false},
		{"CC0-1.0", cc0Text, "CC0-1.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyLicense(tc.text)
			if got != tc.want {
				t.Errorf("ClassifyLicense = %q, want %q", got, tc.want)
			}
			if gotCopyleft := c.IsCopyleft(got); gotCopyleft != tc.wantCopyleft {
				t.Errorf("IsCopyleft(%q) = %t, want %t: a copyleft text read as permissive is the one failure that reaches NOTICE without anyone noticing",
					got, gotCopyleft, tc.wantCopyleft)
			}
		})
	}
}

// TestClassifyLicense_RefusesRatherThanGuesses is the other half. A
// classifier with a fallback is worse than one that gives up: the
// fallback is what the inventory records, and LicensePolicyComplaints
// refuses an empty id precisely so that giving up stays loud.
func TestClassifyLicense_RefusesRatherThanGuesses(t *testing.T) {
	for _, text := range []string{
		"",
		"All rights reserved. You may not copy this.",
		"This software is provided under the terms of the Widget Foundation Licence, v4.",
	} {
		if got := ClassifyLicense(text); got != "" {
			t.Errorf("ClassifyLicense(%q) = %q, want the empty string: an unrecognised licence is not evidence of a permissive one", text, got)
		}
	}
}

// TestClassifyLicense_GNUFamilyIsAskedFirst pins the one ordering
// property the table above depends on but does not isolate.
//
// A GPL file that also carries an Apache-2.0 compatibility note, or a
// linking exception quoting MIT's grant, has to come out GPL. Ordering
// inside licenceMarkers is what decides that, and ordering is exactly the
// kind of thing a later edit reshuffles without noticing.
func TestClassifyLicense_GNUFamilyIsAskedFirst(t *testing.T) {
	mixed := gpl3Text + "\n\n" + apacheText + "\n\n" + mitText
	if got := ClassifyLicense(mixed); got != "GPL-3.0-only" {
		t.Errorf("a GPL-3.0 text that also quotes Apache-2.0 and MIT classified as %q; the GNU family has to be asked first or a copyleft component classifies permissive", got)
	}
}

// ---------------------------------------------------------------------
// The drift gate
// ---------------------------------------------------------------------

// TestCompliancArtifactsMatchThisTree is the check §73 WP5.2's RED step
// asks for ("an undeclared dependency gets caught"), and it is the reason
// the generator returns bytes instead of writing them.
//
// It re-derives every compliance artifact from the live module graph, the
// live lockfile and the live files, and compares the result byte for byte
// with what is checked in. A dependency added to any go.mod, a frontend
// package added to the lockfile, an upstream that relicenses in place, a
// distributed artifact edited, or the release manifest regenerated: each
// of those changes the derived bytes and turns this red.
func TestComplianceArtifactsMatchThisTree(t *testing.T) {
	g := generateForTest(t)
	for _, f := range g.Files() {
		onDisk, err := os.ReadFile(Path(f.Path))
		if err != nil {
			t.Errorf("%s is not in the tree: %v\n\nRegenerate with: (cd distribution && go run ./cmd/provenance -write)", f.Path, err)
			continue
		}
		if !bytes.Equal(onDisk, f.Data) {
			t.Errorf(`%s is not what this tree generates (%d bytes checked in, %d derived).

Something changed that the release's compliance record has to follow: a module
entered or left the graph, a frontend package moved, an upstream relicensed in
place, a distributed artifact was edited, or container/release-manifest.json was
regenerated.

Regenerate with: (cd distribution && go run ./cmd/provenance -write)`, f.Path, len(onDisk), len(f.Data))
		}
	}
}

// TestGeneratingTwiceIsByteIdentical is the positive control for the
// check above.
//
// If generation were not deterministic, that check would be red on every
// run for reasons having nothing to do with the dependency graph, and the
// first thing anyone would do is weaken it. The specific hazard is the
// SPDX creation timestamp: BuildSPDX takes it from the release manifest's
// generated_at rather than from the clock precisely so this holds.
func TestGeneratingTwiceIsByteIdentical(t *testing.T) {
	first := generateForTest(t)
	second, err := GenerateProvenance()
	if err != nil {
		t.Fatalf("second generation failed: %v", err)
	}
	for i, f := range first.Files() {
		if !bytes.Equal(f.Data, second.Files()[i].Data) {
			t.Errorf("%s differs between two generations over an unchanged tree, so the drift check above can never be trusted", f.Path)
		}
	}
	if !strings.Contains(string(first.SBOM), MustLoadCompliance().SourceRepository.URL) {
		t.Error("the SBOM's document namespace does not mention the source repository, so two projects' SBOMs could share an identifier")
	}
}

// TestSBOMTakesItsTimestampFromTheManifestNotTheClock isolates the
// property the control above depends on, so that a future edit which
// reintroduces time.Now() fails here with the reason rather than there
// with a diff.
func TestSBOMTakesItsTimestampFromTheManifestNotTheClock(t *testing.T) {
	inv := Inventory{ProjectLicense: "Apache-2.0", Components: []Component{
		{Name: "example.com/x", Version: "v1.0.0", Ecosystem: EcosystemGo, LicenseID: "MIT"},
	}}
	doc := BuildSPDX(inv, "n", "ns", "2026-01-02T03:04:05Z")
	if doc.CreationInfo.Created != "2026-01-02T03:04:05Z" {
		t.Fatalf("created = %q, want the value passed in", doc.CreationInfo.Created)
	}
	again := BuildSPDX(inv, "n", "ns", "2026-01-02T03:04:05Z")
	if doc.CreationInfo.Created != again.CreationInfo.Created {
		t.Error("two renders of the same inventory disagree about their creation time")
	}
	if len(doc.Packages) != 1 || doc.Packages[0].VersionInfo != "v1.0.0" {
		t.Fatalf("the document does not describe the inventory it was given: %+v", doc.Packages)
	}
	if len(doc.Relationships) != 1 || doc.Relationships[0].RelatedSPDXElement != doc.Packages[0].SPDXID {
		t.Error("the document DESCRIBES relationship does not point at the package it rendered")
	}
}

// ---------------------------------------------------------------------
// What `go list` reports with no version
// ---------------------------------------------------------------------

// TestParseGoListModulesRefusesAnUnversionedThirdPartyModule covers the
// one input that removes a dependency from the SBOM while leaving every
// check green.
//
// `go list -deps` reports an empty version for three different things:
// the main module, a module under this repository's own namespace, and
// any module satisfied by a directory `replace`. The first two are the
// product. The third is a third-party dependency, and dropping it takes
// it out of the inventory, out of the SBOM, and out of reach of the
// licence policy, while the drift test still passes because the
// regenerated bytes omit it too. Nothing is red and the graph the
// Apache-2.0 choice rests on is no longer the graph that ships.
//
// This is driven against constructed `go list` output rather than a real
// module graph because the hole is latent: today's three replace
// directives are all first-party. Constructing the line is the only way
// to watch the refusal fire before the day somebody adds the fourth.
func TestParseGoListModulesRefusesAnUnversionedThirdPartyModule(t *testing.T) {
	const (
		mainModule  = "github.com/spdrman/rclone-manager/core\t\t/repo/core"
		firstParty  = "github.com/spdrman/rclone-manager/apps/common\t\t/repo/apps/common"
		thirdParty  = "github.com/rclone/rclone\tv1.70.0\t/gopath/rclone@v1.70.0"
		replacedDep = "github.com/some/thirdparty\t\t/repo/vendor/thirdparty"
	)

	// The permitted shape: two unversioned first-party lines and one
	// real dependency, which is what every build produces today.
	refs, err := parseGoListModules(strings.Join([]string{mainModule, firstParty, thirdParty}, "\n"))
	if err != nil {
		t.Fatalf("a graph with only first-party replaces is refused: %v", err)
	}
	if len(refs) != 1 || refs[0].Path != "github.com/rclone/rclone" || refs[0].Version != "v1.70.0" {
		t.Fatalf("the third-party module did not survive the parse, so the refusal below proves nothing: %v", refs)
	}

	// The same graph with one third-party module behind a local
	// replace. Silently dropping it is the defect; failing generation
	// is the fix, because an unversioned third-party module is exactly
	// what this bundle exists to refuse.
	refs, err = parseGoListModules(strings.Join([]string{mainModule, firstParty, thirdParty, replacedDep}, "\n"))
	if err == nil {
		for _, r := range refs {
			if r.Path == "github.com/some/thirdparty" {
				t.Fatal("the replaced module was recorded with no version, which is not a fact the SBOM can carry")
			}
		}
		t.Fatal("a third-party module satisfied by a local replace vanished from the inventory instead of failing the generation, so the licence policy never judges it and the drift test cannot notice")
	}
	for _, want := range []string{"github.com/some/thirdparty", "local replace", "/repo/vendor/thirdparty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q, so whoever hits it cannot tell which module or why: %v", want, err)
		}
	}
}

// ---------------------------------------------------------------------
// Platform specificity
// ---------------------------------------------------------------------

// TestGoLinkedModulesIsPlatformSpecific is the control for the one
// mistake that would silently produce a plausible, wrong SBOM.
//
// This project develops on macOS and ships Linux. `go list -deps` answers
// for whatever GOOS it is given, and the two answers genuinely differ, so
// an SBOM generated without setting GOOS describes a binary nobody ships:
// it lists the host's platform packages and omits the target's. The
// generator sets GOOS=linux; this proves that setting it is load-bearing
// rather than decorative.
func TestGoLinkedModulesIsPlatformSpecific(t *testing.T) {
	target := ShippedGoBinaries[0]
	linux, err := GoLinkedModules(target, "linux", "amd64")
	if err != nil {
		t.Fatalf("linux/amd64: %v", err)
	}
	darwin, err := GoLinkedModules(target, "darwin", "amd64")
	if err != nil {
		t.Fatalf("darwin/amd64: %v", err)
	}
	if len(linux) == 0 {
		t.Fatal("no modules linked into the core binary at all, so this control measured nothing")
	}
	set := func(refs []GoModuleRef) map[string]bool {
		m := map[string]bool{}
		for _, r := range refs {
			m[r.Path+"@"+r.Version] = true
		}
		return m
	}
	l, d := set(linux), set(darwin)
	same := len(l) == len(d)
	if same {
		for k := range l {
			if !d[k] {
				same = false
				break
			}
		}
	}
	if same {
		t.Errorf(`the linux and darwin module graphs for %s are identical (%d modules each).

That may be true today, but it means this control no longer proves that the
generator's GOOS=linux is load-bearing, and an SBOM generated on a developer's
Mac without it would go unnoticed. Replace this with a control that still
discriminates rather than deleting it.`, target.Binary, len(l))
	}
}

// ---------------------------------------------------------------------
// npm
// ---------------------------------------------------------------------

func TestNPMProductionComponents(t *testing.T) {
	lock := []byte(`{
	  "lockfileVersion": 3,
	  "packages": {
	    "": {"name": "root", "version": "1.0.0"},
	    "node_modules/react": {"version": "18.3.1", "license": "MIT", "integrity": "sha512-aaa"},
	    "node_modules/vite": {"version": "5.4.0", "license": "MIT", "dev": true},
	    "node_modules/a/node_modules/nested": {"version": "2.0.0", "license": "ISC", "integrity": "sha512-bbb"}
	  }
	}`)
	got, err := NPMProductionComponents(lock)
	if err != nil {
		t.Fatalf("NPMProductionComponents: %v", err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Name+"@"+c.Version+"/"+c.LicenseID)
	}
	want := "nested@2.0.0/ISC,react@18.3.1/MIT"
	if strings.Join(names, ",") != want {
		t.Errorf("got %v, want %s: a dev dependency never reaches the shipped bundle, and a nested package keeps its own name", names, want)
	}
	for _, c := range got {
		if c.Integrity == "" {
			t.Errorf("%s carries no integrity hash, so the lockfile's own evidence is dropped on the way into the SBOM", c.Name)
		}
		if len(c.LinkedInto) != 1 || c.LinkedInto[0] != "backup-manager-web" {
			t.Errorf("%s records linkedInto %v; the frontend bundle is embedded in backup-manager-web and nowhere else", c.Name, c.LinkedInto)
		}
	}
}

// TestNPMProductionComponents_RefusesALockfileItCannotRead is the
// negative control: the per-package license and integrity fields this
// reads only exist from lockfileVersion 2, and a version-1 lockfile would
// otherwise parse into an empty, cheerfully-passing component list.
func TestNPMProductionComponents_RefusesALockfileItCannotRead(t *testing.T) {
	_, err := NPMProductionComponents([]byte(`{"lockfileVersion": 1, "dependencies": {}}`))
	if err == nil {
		t.Fatal("a lockfileVersion 1 file produced no error, so the frontend half of the SBOM would silently be empty")
	}
	if !strings.Contains(err.Error(), "lockfileVersion") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// ---------------------------------------------------------------------
// Artifact parity
// ---------------------------------------------------------------------

// TestArtifactParityComplaints is §74's parity criterion, and the
// criterion's own words are "the check is demonstrated against a
// deliberately mismatched artifact". That is the third row.
//
// Against the real tree only the all-clear arm can run, so every arm that
// carries the actual contract has never executed and could not until
// something in the repository was already broken. A check first exercised
// on the day it matters is a check nobody has seen work.
func TestArtifactParityComplaints(t *testing.T) {
	hashes := map[string]string{
		"a.yaml": "aaaa",
		"b.xml":  "bbbb",
	}
	hash := func(rel string) (string, error) {
		h, ok := hashes[rel]
		if !ok {
			return "", fmt.Errorf("no such file")
		}
		return h, nil
	}
	targets := func(m map[string]DistributionTarget) map[string]DistributionTarget { return m }

	cases := []struct {
		name     string
		targets  map[string]DistributionTarget
		recorded []RecordedArtifact
		want     string
	}{
		{
			"everything declared, recorded and matching",
			targets(map[string]DistributionTarget{"one": {Artifacts: []string{"a.yaml"}}}),
			[]RecordedArtifact{{Target: "one", Path: "a.yaml", SHA256: "aaaa"}},
			"",
		},
		{
			"a target that builds nothing and says why",
			targets(map[string]DistributionTarget{"one": {UnbuiltReason: "the .spk is assembled at release time"}}),
			nil,
			"",
		},
		{
			"a deliberately mismatched artifact",
			targets(map[string]DistributionTarget{"one": {Artifacts: []string{"a.yaml"}}}),
			[]RecordedArtifact{{Target: "one", Path: "a.yaml", SHA256: "not-the-hash"}},
			"are not the bytes this release was recorded from",
		},
		{
			"a declared artifact nobody recorded",
			targets(map[string]DistributionTarget{"one": {Artifacts: []string{"a.yaml"}}}),
			nil,
			"records no digest for it",
		},
		{
			"a recorded artifact nobody declares",
			targets(map[string]DistributionTarget{"one": {Artifacts: []string{"a.yaml"}}}),
			[]RecordedArtifact{
				{Target: "one", Path: "a.yaml", SHA256: "aaaa"},
				{Target: "one", Path: "b.xml", SHA256: "bbbb"},
			},
			"which no distribution target declares",
		},
		{
			"a target that builds nothing and does not say why",
			targets(map[string]DistributionTarget{"one": {}}),
			nil,
			"passes by having nothing to compare",
		},
		{
			"a target that both builds and does not build",
			targets(map[string]DistributionTarget{"one": {Artifacts: []string{"a.yaml"}, UnbuiltReason: "not built"}}),
			[]RecordedArtifact{{Target: "one", Path: "a.yaml", SHA256: "aaaa"}},
			"says both things at once",
		},
		{
			"the same artifact claimed by two targets",
			targets(map[string]DistributionTarget{
				"one": {Artifacts: []string{"a.yaml"}},
				"two": {Artifacts: []string{"a.yaml"}},
			}),
			[]RecordedArtifact{{Target: "one", Path: "a.yaml", SHA256: "aaaa"}},
			"one recorded digest stands for two claims",
		},
		{
			"an artifact recorded against the wrong target",
			targets(map[string]DistributionTarget{"one": {Artifacts: []string{"a.yaml"}}}),
			[]RecordedArtifact{{Target: "two", Path: "a.yaml", SHA256: "aaaa"}},
			"recorded against target",
		},
		{
			"a declared artifact that is not in the tree",
			targets(map[string]DistributionTarget{"one": {Artifacts: []string{"gone.yaml"}}}),
			[]RecordedArtifact{{Target: "one", Path: "gone.yaml", SHA256: "aaaa"}},
			"cannot hash",
		},
		{
			"no targets at all",
			nil,
			nil,
			"passes by default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ArtifactParityComplaints(tc.targets, tc.recorded, hash)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected no complaint, got %v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly one complaint containing %q, got %v", tc.want, got)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("the complaint does not say why: got %q, want it to contain %q", got[0], tc.want)
			}
		})
	}
}

// TestArtifactParityAgainstTheRealTree is the thin caller. Everything it
// could find is already covered above against constructed inputs; what it
// adds is that the real declaration and the real bytes agree today.
func TestArtifactParityAgainstTheRealTree(t *testing.T) {
	c := MustLoadCompliance()
	p := readProvenance(t)
	for _, complaint := range ArtifactParityComplaints(c.Distribution.Targets, p.Artifacts, SHA256RepoFile) {
		t.Error(complaint)
	}
}

func readProvenance(t *testing.T) Provenance {
	t.Helper()
	data, err := os.ReadFile(Path(ProvenancePath))
	if err != nil {
		t.Fatalf("cannot read %s: %v\n\nGenerate it with: (cd distribution && go run ./cmd/provenance -write)", ProvenancePath, err)
	}
	p, err := ParseProvenance(data)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", ProvenancePath, err)
	}
	return p
}

// ---------------------------------------------------------------------
// Version parity
// ---------------------------------------------------------------------

// TestVersionParityComplaints covers the arms the real files cannot
// reach, in the same shape as registryDigestComplaints and for the same
// reason: image.published is false today, so the row that actually
// matters, a published image whose binaries answer with a different
// string from the tag the listing advertises, has never executed.
//
// It is not hypothetical. `git describe --tags --always` in a repository
// with no tags yields an abbreviated commit, so the recorded build
// version is "8ad3100" while every provider package points at 1.0.0. The
// day of the first push is the worst moment to discover that.
func TestVersionParityComplaints(t *testing.T) {
	cases := []struct {
		name            string
		published       bool
		canonicalTag    string
		manifestVersion string
		bundleVersion   string
		buildStamp      bool
		want            string
	}{
		{"unpublished, the manifest carries a build stamp, recorded as such (today)", false, "1.0.0", "8ad3100", "1.0.0", true, ""},
		{"published and the two agree", true, "1.0.0", "1.0.0", "1.0.0", false, ""},
		{"published while the binaries still answer with a commit", true, "1.0.0", "8ad3100", "1.0.0", true, "are different strings"},
		{"the bundle describes a different version from the packages", false, "1.0.0", "8ad3100", "0.9.0", true, "has to describe the release the packages point at"},
		{"the manifest records no version at all", false, "1.0.0", "", "1.0.0", false, "records no version at all"},
		{"the build-stamp flag claims a difference that is not there", false, "1.0.0", "1.0.0", "1.0.0", true, "the record has to match the files"},
		{"the build-stamp flag hides a difference that is there", false, "1.0.0", "8ad3100", "1.0.0", false, "the record has to match the files"},
		{"no canonical tag to compare against", false, "", "8ad3100", "1.0.0", true, "no image tag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VersionParityComplaints(tc.published, tc.canonicalTag, tc.manifestVersion, tc.bundleVersion, tc.buildStamp)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected no complaint, got %v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly one complaint containing %q, got %v", tc.want, got)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("the complaint does not say why: got %q, want it to contain %q", got[0], tc.want)
			}
		})
	}
}

// TestVersionParityAgainstTheRealFiles is the thin caller, and it is also
// where the build-stamp record is held to the files it describes: the
// bundle may not say the manifest's version matches the canonical tag
// while it does not, nor the reverse.
func TestVersionParityAgainstTheRealFiles(t *testing.T) {
	c := MustLoad()
	m, err := ReadReleaseManifest()
	if err != nil {
		t.Fatalf("cannot read the release manifest: %v", err)
	}
	p := readProvenance(t)
	for _, complaint := range VersionParityComplaints(
		c.Image.Published, c.Image.Tag, m.Version, p.SemanticVersion, p.ReleaseManifest.VersionIsABuildStamp,
	) {
		t.Error(complaint)
	}
}

// ---------------------------------------------------------------------
// The tie between the two halves of the release record
// ---------------------------------------------------------------------

// TestProvenanceBundleIsTiedToTheReleaseManifest is what makes splitting
// the record across two files safe.
//
// container/release-manifest.json records what a two-architecture Docker
// build produced; provenance/release-provenance.json records
// everything derivable without a build. Two files means two chances to
// regenerate one and not the other, and the pairing digest is the only
// thing that notices.
func TestProvenanceBundleIsTiedToTheReleaseManifest(t *testing.T) {
	raw, err := os.ReadFile(Path(ManifestPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", ManifestPath, err)
	}
	m, err := ParseReleaseManifest(raw)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", ManifestPath, err)
	}
	p := readProvenance(t)

	for _, complaint := range BundleTiedToManifestComplaints(p, SHA256Bytes(raw), m) {
		t.Error(complaint)
	}

	// The negative control. The check above is an assertion that
	// something never happens, and one that has never been seen to fire
	// is indistinguishable from one that cannot: feed it a manifest whose
	// bytes moved and require it to notice, and to say which fact moved.
	stale := append(append([]byte(nil), raw...), '\n')
	complaints := BundleTiedToManifestComplaints(p, SHA256Bytes(stale), m)
	if len(complaints) != 1 {
		t.Fatalf("a manifest whose bytes moved produced %d complaints, want exactly 1: %v", len(complaints), complaints)
	}
	if !strings.Contains(complaints[0], "regenerated without regenerating the bundle") {
		t.Errorf("the complaint does not name the cause: %q", complaints[0])
	}

	moved := m
	moved.Commit = strings.Repeat("f", 40)
	if got := BundleTiedToManifestComplaints(p, SHA256Bytes(raw), moved); len(got) != 1 || !strings.Contains(got[0], "pins commit") {
		t.Errorf("a manifest pinning a different commit produced %v, want one complaint naming the commits", got)
	}
}

// ---------------------------------------------------------------------
// Coverage of the checksum manifest
// ---------------------------------------------------------------------

// TestChecksumsCoverEveryDistributedArtifact is §61's "SHA-256
// checksums", checked for coverage rather than for existence.
//
// A checksums file that exists and lists three of fourteen artifacts is
// worse than none, because `sha256sum -c` over it exits 0 and reads as a
// verified release.
func TestChecksumsCoverEveryDistributedArtifact(t *testing.T) {
	data, err := os.ReadFile(Path(ChecksumsPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", ChecksumsPath, err)
	}
	listed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			t.Errorf("%s has a line sha256sum -c cannot read: %q", ChecksumsPath, line)
			continue
		}
		listed[parts[1]] = parts[0]
	}

	c := MustLoadCompliance()
	required := []string{c.License.File, c.License.NoticeFile, InventoryPath, SBOMPath, ManifestPath, UISharedLockfile}
	for _, id := range c.TargetIDs() {
		required = append(required, c.Distribution.Targets[id].Artifacts...)
	}
	for _, rel := range required {
		recorded, ok := listed[rel]
		if !ok {
			t.Errorf("%s lists no checksum for %s, so `sha256sum -c` over it verifies a release without ever looking at that file", ChecksumsPath, rel)
			continue
		}
		actual, err := SHA256RepoFile(rel)
		if err != nil {
			t.Errorf("cannot hash %s: %v", rel, err)
			continue
		}
		if actual != recorded {
			t.Errorf("%s records %s for %s and it hashes to %s", ChecksumsPath, shortHex(recorded), rel, shortHex(actual))
		}
	}
	if len(listed) < len(required) {
		t.Errorf("%s lists %d files and %d are required", ChecksumsPath, len(listed), len(required))
	}
}
