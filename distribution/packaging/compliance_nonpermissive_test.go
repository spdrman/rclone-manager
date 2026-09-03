package packaging

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Issue #402. Registering rclone's s3 backend links two MPL-2.0 modules
// into both shipped binaries, and MPL-2.0 is not permissive: it is
// file-level copyleft with a live source obligation under §3.2.
//
// The tests here are driven over a FIXTURE inventory rather than over the
// live module graph, and that is the point rather than a convenience.
// core/internal/transport/rclone/adapter.go registers local and sftp
// only, so the real inventory has no MPL component in it today and
// TestLicensePolicyAgainstTheRealInventory is green. The question this
// issue asks is what the policy says about the graph the s3 backend
// PRODUCES, and waiting for that graph to exist before asking would mean
// discovering the answer during a merge.
//
// So the fixture is the real inventory plus exactly the two components
// the backend drags in, at the versions and with the licence hashes the
// generator would record for them. Everything else about it is real.

// mplComponentsTheS3BackendLinks is what `go list -deps` reports for
// core/cmd/backup-manager once backend/s3 is registered, transcribed
// from the module cache rather than invented.
//
//	rclone/backend/s3 -> IBM/go-sdk-core/v5/core -> hashicorp/go-retryablehttp
//	                                             -> hashicorp/go-cleanhttp
//
// backend/s3's ibm_signer.go carries no build tag and is the only file
// importing the IBM SDK, so there is no build-tag route that registers
// the backend and leaves these two out.
//
// The licence hashes are the SHA-256 of each module's own LICENSE as it
// ships in the module cache. go-retryablehttp's matches the record the
// regenerated inventory already carries.
func mplComponentsTheS3BackendLinks() []Component {
	return []Component{
		{
			Name:          "github.com/hashicorp/go-cleanhttp",
			Version:       "v0.5.2",
			Ecosystem:     EcosystemGo,
			LicenseID:     "MPL-2.0",
			LicenseFile:   "LICENSE",
			LicenseSHA256: "60222c28c1a7f6a92c7df98e5c5f4459e624e6e285e0b9b94467af5f6ab3343d",
			LinkedInto:    []string{"backup-manager", "backup-manager-web"},
		},
		{
			Name:          "github.com/hashicorp/go-retryablehttp",
			Version:       "v0.7.8",
			Ecosystem:     EcosystemGo,
			LicenseID:     "MPL-2.0",
			LicenseFile:   "LICENSE",
			LicenseSHA256: "d6b1a865f1c8c697d343bd4e0ce61025f91898486a1f00d727f32e8644af77d3",
			LinkedInto:    []string{"backup-manager", "backup-manager-web"},
		},
	}
}

// inventoryTheS3BackendWouldProduce is the real inventory with those two
// components merged in.
func inventoryTheS3BackendWouldProduce(t *testing.T) Inventory {
	t.Helper()
	data, err := os.ReadFile(Path(InventoryPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", InventoryPath, err)
	}
	inv, err := ParseInventory(data)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", InventoryPath, err)
	}
	// The control for the fixture itself. If the real inventory already
	// carried these, the test below would be asserting nothing new and
	// the whole framing here would be stale.
	for _, comp := range inv.Components {
		if strings.HasPrefix(comp.Name, "github.com/hashicorp/go-") {
			t.Fatalf("%s@%s is already in the real inventory, so this fixture no longer adds the case it exists to add", comp.Name, comp.Version)
		}
	}
	inv.Components = append(append([]Component{}, inv.Components...), mplComponentsTheS3BackendLinks()...)
	SortComponents(inv.Components)
	return inv
}

// TestTheLicencePolicyOverTheInventoryTheS3BackendWillProduce is the
// headline. It is the same question TestLicensePolicyAgainstTheRealInventory
// asks, put to the graph FR-28 requires instead of the graph that exists.
func TestTheLicencePolicyOverTheInventoryTheS3BackendWillProduce(t *testing.T) {
	c := MustLoadCompliance()
	inv := inventoryTheS3BackendWouldProduce(t)
	if got := LicensePolicyComplaints(c, inv); len(got) != 0 {
		t.Errorf("the licence policy refuses the graph the s3 backend produces:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestAnUnrecordedMPLModuleIsStillRefused is the falsification for the
// test above. Whatever admits the two components has to admit those two
// and not the licence they happen to carry, or the check has been turned
// into a wider allowlist with extra steps.
func TestAnUnrecordedMPLModuleIsStillRefused(t *testing.T) {
	c := MustLoadCompliance()
	inv := inventoryTheS3BackendWouldProduce(t)
	inv.Components = append(inv.Components, Component{
		Name:        "github.com/hashicorp/vault-client-go",
		Version:     "v0.4.3",
		Ecosystem:   EcosystemGo,
		LicenseID:   "MPL-2.0",
		LicenseFile: "LICENSE",
		LinkedInto:  []string{"backup-manager"},
	})
	got := LicensePolicyComplaints(c, inv)
	if len(got) != 1 {
		t.Fatalf("an MPL-2.0 module nobody has recorded produced %d complaints, want exactly one: %v", len(got), got)
	}
	if !strings.Contains(got[0], "vault-client-go") {
		t.Errorf("the complaint does not name the module it is about: %q", got[0])
	}
}

// TestADriftedVersionOfARecordedModuleIsStillRefused is the second
// falsification, and the one that decides whether the acceptance is
// about artifacts somebody read or about a licence id.
//
// v0.7.9 is a different upload with a different licence file hash and
// notices nobody has looked at, so it has to be refused exactly as
// firmly as a module from a different project.
func TestADriftedVersionOfARecordedModuleIsStillRefused(t *testing.T) {
	c := MustLoadCompliance()
	inv := inventoryTheS3BackendWouldProduce(t)
	for i := range inv.Components {
		if inv.Components[i].Name == "github.com/hashicorp/go-retryablehttp" {
			inv.Components[i].Version = "v0.7.9"
		}
	}
	got := LicensePolicyComplaints(c, inv)
	if len(got) != 1 {
		t.Fatalf("bumping a recorded module to an unrecorded version produced %d complaints, want exactly one: %v", len(got), got)
	}
	if !strings.Contains(got[0], "v0.7.9") {
		t.Errorf("the complaint does not name the version that drifted: %q", got[0])
	}
}

// TestClassifyLicenseReadsBothSpellingsOfTheMPL pins the marker table
// against the two headers this licence is actually distributed under.
//
// Mozilla's canonical text opens "Mozilla Public License Version 2.0".
// HashiCorp ships a variant header, "Mozilla Public License, version
// 2.0", with a comma and a lower-case v, and both go-cleanhttp@v0.5.2 and
// go-retryablehttp@v0.7.8 carry that one. A classifier that reads only
// the first spelling calls them NOASSERTION, which is still a refusal
// here but a refusal that cannot name the licence it is refusing, and a
// compliance record that cannot name a copyleft dependency is worse than
// one that refuses it.
//
// The bodies below are transcribed from the module cache. They are short
// because ClassifyLicense matches on the header, and the point of the
// test is the header.
func TestClassifyLicenseReadsBothSpellingsOfTheMPL(t *testing.T) {
	const canonical = `Mozilla Public License Version 2.0
==================================

1. Definitions
`
	const hashicorp = `Copyright (c) 2015 HashiCorp, Inc.

Mozilla Public License, version 2.0

1. Definitions

1.1. "Contributor"
`
	for _, tc := range []struct{ name, text string }{
		{"Mozilla's canonical header", canonical},
		{"HashiCorp's variant header", hashicorp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyLicense(tc.text); got != "MPL-2.0" {
				t.Errorf("ClassifyLicense read %q, want MPL-2.0; an unnamed copyleft licence is a refusal nobody can act on", got)
			}
		})
	}
	// The control. Widening the table must not have turned it into
	// something that says MPL-2.0 to anything with the word Mozilla in
	// it.
	if got := ClassifyLicense("This file is part of the Mozilla test suite.\n"); got != "" {
		t.Errorf("ClassifyLicense read %q from a file that states no licence at all; a classifier that guesses is worse than one that gives up", got)
	}
}

// hardCodedComponentCount catches arithmetic in prose. "54 modules" is
// how this file came to assert a number nobody checks.
var hardCodedComponentCount = regexp.MustCompile(`\b\d+\s+(modules|packages|components|dependencies)\b`)

// TestTheRationaleAssertsNoArithmeticNobodyChecks.
//
// compliance.json's own header says nothing in it is hand-maintained
// arithmetic, and the rationale was the one place that was not true: it
// said "54 modules" against a graph that has held 50 for some time. The
// counts belong to the inventory, which is regenerated from the module
// graph on every run, and prose that repeats them is a second copy that
// can only ever go stale.
func TestTheRationaleAssertsNoArithmeticNobodyChecks(t *testing.T) {
	c := MustLoadCompliance()
	for i, line := range c.License.Rationale {
		if m := hardCodedComponentCount.FindString(line); m != "" {
			t.Errorf("license.rationale line %d says %q; the count belongs to %s, which is regenerated from the module graph on every run, and a copy of it in prose is a claim that goes stale silently",
				i, m, c.License.Inventory)
		}
	}
}

// TestTheRationaleAccountsForTheNonPermissiveLicencesInTheGraph.
//
// The first acceptance criterion on #402 is that the reasoning lives in
// the rationale rather than in a commit message, and the rationale said
// the opposite of what is true: "Nothing in the graph is copyleft, so the
// choice was open".
func TestTheRationaleAccountsForTheNonPermissiveLicencesInTheGraph(t *testing.T) {
	c := MustLoadCompliance()
	// Joined with a space and re-collapsed, because the rationale is
	// stored as wrapped lines and the sentence this is about straddles
	// two of them. A search over the array as written would miss it and
	// pass for a reason that has nothing to do with the text.
	body := strings.Join(strings.Fields(strings.Join(c.License.Rationale, " ")), " ")
	if strings.Contains(body, "Nothing in the graph is copyleft") {
		t.Error("license.rationale still says nothing in the graph is copyleft; the s3 backend FR-28 requires links two MPL-2.0 modules into both binaries, so that sentence is the premise of the Apache-2.0 choice stated as a fact that stopped being one")
	}
	if !strings.Contains(body, "MPL-2.0") {
		t.Error("license.rationale never mentions MPL-2.0, so the one non-permissive licence in the graph the product ships is recorded nowhere a reader of this file would look")
	}
}
