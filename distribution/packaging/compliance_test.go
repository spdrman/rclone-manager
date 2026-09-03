package packaging

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Issue #88 (B5.2): the compliance materials §73 Work Package 5.2 asks
// for, and the rules that keep them true of the tree.
//
// The acceptance criterion these serve is "source/privacy/license/support
// links are valid", and "valid" is doing two separate jobs in that
// sentence. One is that the target exists and says something. The other
// is that a store reviewer can reach it. This repository satisfies the
// first and does not satisfy the second, because it is private, and the
// tests below keep those two answers apart rather than letting a green
// run imply both.

// validCompliance is the fixture the link rules are driven against.
// Constructed rather than the real file, because the real file can only
// exercise the arm where everything is fine.
func validCompliance() Compliance {
	return Compliance{
		SourceRepository: SourceRepository{
			URL:        "https://github.com/example/project",
			Visibility: VisibilityPublic,
		},
		License: ComplianceLicense{SPDXID: "Apache-2.0"},
		Links: []ComplianceLink{
			{ID: "source", URL: "https://github.com/example/project", TargetsThisRepository: true},
			{ID: "privacy", RepoPath: "docs/privacy.md", MustMention: []string{"telemetry"}},
		},
	}
}

func readerFor(files map[string]string) ReadFileFunc {
	return func(rel string) ([]byte, error) {
		body, ok := files[rel]
		if !ok {
			return nil, fmt.Errorf("open %s: no such file or directory", rel)
		}
		return []byte(body), nil
	}
}

// enough returns a body long enough to clear the placeholder threshold.
func enough(contains string) string {
	return contains + "\n" + strings.Repeat("x", minimumLinkBody)
}

// TestLinkComplaints leads with the positive control. Every other row is
// a negative assertion, and a table of negatives all passing is exactly
// what a rule that refuses nothing looks like.
func TestLinkComplaints(t *testing.T) {
	good := map[string]string{"docs/privacy.md": enough("telemetry")}

	cases := []struct {
		name    string
		mutate  func(*Compliance)
		files   map[string]string
		want    string
		wantN   int  // how many complaints, 0 meaning "exactly one"
		wantAll bool // want no complaints at all
	}{
		{"a complete, public declaration", nil, good, "", 0, true},
		{
			"a private repository is not itself a link failure",
			func(c *Compliance) { c.SourceRepository.Visibility = VisibilityPrivate },
			good, "", 0, true,
		},
		{
			"an unrecognised visibility",
			func(c *Compliance) { c.SourceRepository.Visibility = "internal" },
			good, "neither \"public\" nor \"private\"", 0, false,
		},
		{
			"a link target that is not in the tree",
			nil,
			map[string]string{}, "which is not in the tree", 0, false,
		},
		{
			"a link target that exists and is a placeholder",
			nil,
			map[string]string{"docs/privacy.md": "TODO"}, "is a placeholder", 0, false,
		},
		{
			"a link target that never mentions what it promises",
			nil,
			map[string]string{"docs/privacy.md": enough("nothing relevant")}, "never mentions", 0, false,
		},
		{
			"a link that is neither a path nor a URL",
			func(c *Compliance) { c.Links[0].URL = "" },
			good, "names neither a URL nor a repository path", 0, false,
		},
		{
			// Two complaints on purpose: an http URL is also not under
			// the https source URL, and reporting only one of those
			// would hide the second the day a link is moved as well as
			// downgraded.
			"a link served over plain http",
			func(c *Compliance) { c.Links[0].URL = "http://github.com/example/project" },
			good, "which is not https", 2, false,
		},
		{
			"a link claiming to target this repository and pointing somewhere else",
			func(c *Compliance) { c.Links[0].URL = "https://example.invalid/other" },
			good, "which is not under", 0, false,
		},
		{
			"the same link declared twice",
			func(c *Compliance) { c.Links = append(c.Links, c.Links[0]) },
			good, "declared twice", 0, false,
		},
		{
			"no links at all",
			func(c *Compliance) { c.Links = nil },
			good, "passes by default", 0, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCompliance()
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			got := LinkComplaints(c, readerFor(tc.files))
			if tc.wantAll {
				if len(got) != 0 {
					t.Fatalf("expected no complaint, got %v", got)
				}
				return
			}
			wantN := tc.wantN
			if wantN == 0 {
				wantN = 1
			}
			if len(got) != wantN {
				t.Fatalf("expected %d complaint(s), the first containing %q, got %v", wantN, tc.want, got)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("the complaint does not say why: got %q, want it to contain %q", got[0], tc.want)
			}
		})
	}
}

// TestDeclaredLinksResolveInTheTree is the thin caller against the real
// declaration and the real files.
func TestDeclaredLinksResolveInTheTree(t *testing.T) {
	for _, complaint := range LinkComplaints(MustLoadCompliance(), RepoReader()) {
		t.Error(complaint)
	}
}

// TestLinkReachabilityIsNotOverstated is the half the check above
// deliberately does not decide.
//
// Every link target exists and has substance, and none of them resolves
// for anybody outside this repository's ACL, because the repository is
// private. Those are different facts and a compliance package that
// reports the first as if it were the second is exactly the kind of
// paperwork §73 WP5.2 exists to stop. So the recorded verdict has to
// track the recorded visibility, in both directions.
func TestLinkReachabilityIsNotOverstated(t *testing.T) {
	c := MustLoadCompliance()
	p := readProvenance(t)

	if got, want := p.Links.PubliclyReachable, c.StoreReadyForPublicLinks(); got != want {
		t.Errorf("the provenance bundle records publiclyReachable=%t and compliance.json's visibility %q implies %t",
			got, c.SourceRepository.Visibility, want)
	}
	if c.SourceRepository.Visibility == VisibilityPrivate && p.Links.PubliclyReachable {
		t.Error("the source repository is private and the bundle claims the links are publicly reachable; a store reviewer would get a 404 from every one of them")
	}
	if p.Links.Reason == "" {
		t.Error("the bundle records a link verdict with no reason, so a reader cannot tell whether it was measured or assumed")
	}

	// Both directions, so the assertion is not satisfied by a constant.
	public := c
	public.SourceRepository.Visibility = VisibilityPublic
	if !public.StoreReadyForPublicLinks() {
		t.Error("a public repository does not read as store-ready, so this verdict is not derived from visibility at all")
	}
	unknown := c
	unknown.SourceRepository.Visibility = "somethingelse"
	if unknown.StoreReadyForPublicLinks() {
		t.Error("an unrecognised visibility reads as store-ready; an unparsed string treated as public is a compliance claim made on no evidence")
	}
}

// ---------------------------------------------------------------------
// The licence
// ---------------------------------------------------------------------

// TestProjectLicenseIsInTheTreeAndIsWhatItSaysItIs is the check #90's
// preflight is waiting on. Its materials-support-source-license cell
// reports BLOCKED on this issue for three store targets, with the reason
// "support and source written, licence not chosen", and the missing
// artifact is literally a LICENSE file in the tree.
//
// It asserts the licence is Apache-2.0 by reading the file rather than by
// trusting the declaration, because a LICENSE file whose contents are MIT
// under a declaration that says Apache-2.0 is worse than no file at all.
func TestProjectLicenseIsInTheTreeAndIsWhatItSaysItIs(t *testing.T) {
	c := MustLoadCompliance()
	if c.License.SPDXID == "" {
		t.Fatal("compliance.json declares no licence, so nothing can be distributed")
	}
	data, err := os.ReadFile(Path(c.License.File))
	if err != nil {
		t.Fatalf("compliance.json says this project is under %s and %s is not in the tree: %v", c.License.SPDXID, c.License.File, err)
	}
	if got := ClassifyLicense(string(data)); got != c.License.SPDXID {
		t.Errorf("%s reads as %s and compliance.json declares %s: a licence file that disagrees with the declaration is worse than no file, because both are cited as if they agreed",
			c.License.File, got, c.License.SPDXID)
	}
	if !strings.Contains(string(data), c.Project.Copyright) {
		t.Errorf("%s does not carry the copyright line %q that compliance.json declares", c.License.File, c.Project.Copyright)
	}
}

// TestLicensePolicyComplaints is the executable form of the premise
// behind choosing Apache-2.0: every linked component is either permissive
// or under one of the non-permissive licences this project accepts on
// purpose with its obligation recorded. That is a fact about today's
// go.mod, not a property of the project, so it is checked rather than
// remembered.
func TestLicensePolicyComplaints(t *testing.T) {
	c := MustLoadCompliance()
	base := func(comps ...Component) Inventory {
		return Inventory{ProjectLicense: c.License.SPDXID, Components: comps}
	}
	ok := Component{Name: "example.com/x", Version: "v1.0.0", Ecosystem: EcosystemGo, LicenseID: "MIT"}

	cases := []struct {
		name string
		inv  Inventory
		want string
	}{
		{"a permissive component", base(ok), ""},
		{"a GPL component", base(Component{Name: "example.com/g", Version: "v1", Ecosystem: EcosystemGo, LicenseID: "GPL-3.0-only"}), "which is copyleft"},
		{"an AGPL component", base(Component{Name: "example.com/a", Version: "v1", Ecosystem: EcosystemGo, LicenseID: "AGPL-3.0-only"}), "which is copyleft"},
		// MPL-2.0 used to be refused here and is now accepted, and
		// the reason is #402: rclone's s3 backend links two MPL-2.0
		// modules and cannot be registered without them, so the
		// project accepted the licence with its §3.2 obligation
		// recorded rather than pretend the graph is permissive.
		// The acceptance is what makes this row pass, and
		// TestTheAcceptedNonPermissiveCategoryRefusesAnEmptyAcceptance
		// is the control that proves so.
		{"an MPL component, which this project accepts on purpose", base(Component{Name: "example.com/m", Version: "v1", Ecosystem: EcosystemGo, LicenseID: "MPL-2.0"}), ""},
		{"a copyleft licence nobody accepted", base(Component{Name: "example.com/e", Version: "v1", Ecosystem: EcosystemGo, LicenseID: "EPL-2.0"}), "which is copyleft"},
		{"a component whose licence could not be identified", base(Component{Name: "example.com/u", Version: "v1", Ecosystem: EcosystemGo}), "not evidence of a permissive one"},

		// The rows a denylist cannot reach. Each of these is
		// non-empty, so the unidentified-licence rule above lets it
		// through, and none of them is on copyleftIds, so an
		// exact-match denylist lets it through as well. They are what
		// npm registry metadata is actually full of.
		{"npm's deprecated bare GPL id", base(Component{Name: "leftpad", Version: "1.0.0", Ecosystem: EcosystemNPM, LicenseID: "GPL-3.0"}), "not on the permissive allowlist"},
		{"npm's deprecated bare LGPL id", base(Component{Name: "rightpad", Version: "1.0.0", Ecosystem: EcosystemNPM, LicenseID: "LGPL-2.1"}), "not on the permissive allowlist"},
		{"a dual-licence choice", base(Component{Name: "eitheror", Version: "1.0.0", Ecosystem: EcosystemNPM, LicenseID: "(MIT OR GPL-3.0)"}), "an expression rather than a decided licence"},
		{"a licence with an exception", base(Component{Name: "example.com/x", Version: "v1", Ecosystem: EcosystemGo, LicenseID: "GPL-2.0-or-later WITH Classpath-exception-2.0"}), "an expression rather than a decided licence"},
		{"npm's read-the-file placeholder", base(Component{Name: "opaque", Version: "1.0.0", Ecosystem: EcosystemNPM, LicenseID: "SEE LICENSE IN LICENSE"}), "an expression rather than a decided licence"},
		{"a recognised licence nobody has decided about", base(Component{Name: "example.com/e", Version: "v1", Ecosystem: EcosystemGo, LicenseID: "EUPL-1.2"}), "not on the permissive allowlist"},
		{"a permissive npm component", base(Component{Name: "react", Version: "19.0.0", Ecosystem: EcosystemNPM, LicenseID: "MIT"}), ""},
		{"a component with no version", base(Component{Name: "example.com/n", Ecosystem: EcosystemGo, LicenseID: "MIT"}), "no version"},
		{"an inventory that lists nothing", base(), "passes by default"},
		{"an inventory generated against a different licence", Inventory{ProjectLicense: "MIT", Components: []Component{ok}}, "discharges nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LicensePolicyComplaints(c, tc.inv)
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

	// The switch. It used to decide whether a copyleft component was
	// refused at all, which made the whole premise behind Apache-2.0
	// editable from a data file: one character in compliance.json and
	// the refusal became a note, with every test still green because
	// the real inventory has nothing to complain about either way.
	//
	// Now it decides the wording and not the verdict. Both halves are
	// asserted, because "the refusal survived" and "the message still
	// explains itself" fail in different directions.
	gpl := base(Component{Name: "example.com/g", Version: "v1", Ecosystem: EcosystemGo, LicenseID: "GPL-3.0-only"})
	off := c
	off.License.CopyleftBlocksTheLicenseChoice = false
	got := LicensePolicyComplaints(off, gpl)
	if len(got) != 1 {
		t.Fatalf("with copyleftBlocksTheLicenseChoice off, a GPL component produced %d complaints; turning a boolean off in a data file must not be able to admit a GPL dependency: %v", len(got), got)
	}
	if !strings.Contains(got[0], "refused regardless") {
		t.Errorf("the complaint does not say the refusal outlived the switch: %q", got[0])
	}

	// And the allowlist is what does the refusing, so emptying it
	// refuses everything rather than accepting everything.
	empty := c
	empty.License.PermissiveIDs = nil
	if got := LicensePolicyComplaints(empty, base(ok)); len(got) != 1 || !strings.Contains(got[0], "empty reading is a refusal") {
		t.Errorf("an empty permissive allowlist passes a component instead of refusing it: %v", got)
	}
}

// TestTheCopyleftSwitchIsOnInTheShippedFile pins the declaration itself.
//
// The test above proves the refusal no longer depends on this boolean.
// This one proves the boolean is still true, because it decides whether
// a reader of provenance/ is told "GPL, which is copyleft" or the vaguer
// allowlist wording, and because a false here is somebody having tried.
func TestTheCopyleftSwitchIsOnInTheShippedFile(t *testing.T) {
	c := MustLoadCompliance()
	if !c.License.CopyleftBlocksTheLicenseChoice {
		t.Error("compliance.json has copyleftBlocksTheLicenseChoice false; the Apache-2.0 choice rests on the linked graph being permissive, and turning this off removes the sentence that says so from every complaint")
	}
	if len(c.License.PermissiveIDs) == 0 {
		t.Fatal("compliance.json declares no permissive allowlist, so the licence policy has nothing to admit against and would refuse the whole inventory")
	}
	// The allowlist is the refusal, so a copyleft id appearing on both
	// lists would be admitted with a copyleft complaint's wording and
	// no complaint at all.
	for _, id := range c.License.PermissiveIDs {
		if c.IsCopyleft(id) {
			t.Errorf("%s is on both permissiveIds and copyleftIds, so it is admitted by the list that decides and named by the list that only explains", id)
		}
		if LicenseExpressionIsUndecided(id) {
			t.Errorf("permissiveIds carries %q, which is an expression rather than a decided licence, so it can never match a component id anyway", id)
		}
	}
}

// TestTheLicencePolicyJudgesTheNPMHalfAsItIsDerived runs a synthetic
// lockfile through the real derivation and then through the real policy.
//
// The table above builds Components by hand, so every planted violation
// in it travels a path no production component takes. The npm half is
// derived from package-lock.json by NPMProductionComponents, which sets
// LicenseID verbatim from the lockfile's own metadata, and that metadata
// is where deprecated bare ids and compound expressions live. This is the
// same violation, planted where it actually arrives from.
func TestTheLicencePolicyJudgesTheNPMHalfAsItIsDerived(t *testing.T) {
	lockfile := []byte(`{
	  "lockfileVersion": 3,
	  "packages": {
	    "": { "name": "ui-shared", "version": "0.0.0" },
	    "node_modules/permissive": { "version": "1.2.3", "license": "MIT", "integrity": "sha512-aaa" },
	    "node_modules/copyleft": { "version": "4.5.6", "license": "GPL-3.0", "integrity": "sha512-bbb" },
	    "node_modules/dual": { "version": "7.8.9", "license": "(MIT OR GPL-2.0)", "integrity": "sha512-ccc" },
	    "node_modules/devonly": { "version": "1.0.0", "license": "GPL-3.0", "dev": true, "integrity": "sha512-ddd" }
	  }
	}`)
	comps, err := NPMProductionComponents(lockfile)
	if err != nil {
		t.Fatalf("NPMProductionComponents: %v", err)
	}
	if len(comps) != 3 {
		t.Fatalf("expected the three production packages and not the dev one, got %d: %v", len(comps), comps)
	}

	c := MustLoadCompliance()
	inv := Inventory{ProjectLicense: c.License.SPDXID, Components: comps}
	complaints := LicensePolicyComplaints(c, inv)
	if len(complaints) != 2 {
		t.Fatalf("expected the bare GPL-3.0 and the dual-licence expression to be refused and the MIT package to pass, got %d complaints: %v", len(complaints), complaints)
	}
	joined := strings.Join(complaints, "\n")
	for _, want := range []string{"copyleft@4.5.6", "dual@7.8.9"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the policy never names %s: %s", want, joined)
		}
	}
	if strings.Contains(joined, "permissive@1.2.3") {
		t.Errorf("the policy refuses an MIT package, so it refuses everything and these assertions mean nothing: %s", joined)
	}
	// The control for the pair: the same lockfile with the two bad ids
	// corrected has to produce nothing, or the two complaints above are
	// the policy refusing whatever it is handed.
	clean := []byte(strings.ReplaceAll(strings.ReplaceAll(string(lockfile), "(MIT OR GPL-2.0)", "MIT"), `"license": "GPL-3.0", "integrity": "sha512-bbb"`, `"license": "Apache-2.0", "integrity": "sha512-bbb"`))
	cleanComps, err := NPMProductionComponents(clean)
	if err != nil {
		t.Fatalf("NPMProductionComponents (clean): %v", err)
	}
	if got := LicensePolicyComplaints(c, Inventory{ProjectLicense: c.License.SPDXID, Components: cleanComps}); len(got) != 0 {
		t.Errorf("a lockfile carrying only permissive ids still complains, so the refusals above are not about the licences: %v", got)
	}
}

// TestLicensePolicyAgainstTheRealInventory is the thin caller, and it
// goes through LicenceComplaints rather than through the two halves by
// hand, because the pairing is the gate. It used to call both halves
// itself, which was closed, and it was the only place that did; nothing
// stopped the next caller running one half and calling that a pass.
func TestLicensePolicyAgainstTheRealInventory(t *testing.T) {
	data, err := os.ReadFile(Path(InventoryPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v\n\nGenerate it with: (cd distribution && go run ./cmd/provenance -write)", InventoryPath, err)
	}
	inv, err := ParseInventory(data)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", InventoryPath, err)
	}
	if inv.Schema != InventorySchema {
		t.Errorf("%s declares schema %q and this code reads %q", InventoryPath, inv.Schema, InventorySchema)
	}
	c := MustLoadCompliance()
	// Both halves. The pure one judges whether every licence is
	// permissive or accepted with its obligation declared; the one that
	// reads NOTICE and the source offer asks whether they carry that
	// offer for each component at its exact version. An accepted licence
	// whose artifacts stop naming the module, the version or the address
	// is the failure mode that would otherwise look exactly like a pass.
	for _, complaint := range LicenceComplaints(c, inv, RepoReader()) {
		t.Error(complaint)
	}
}

// liveInventory reads the inventory this tree ships, for the tests that
// plant one thing into it.
func liveInventory(t *testing.T) Inventory {
	t.Helper()
	data, err := os.ReadFile(Path(InventoryPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", InventoryPath, err)
	}
	inv, err := ParseInventory(data)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", InventoryPath, err)
	}
	if len(inv.Components) == 0 {
		t.Fatalf("%s lists no components, so planting one into it measures nothing", InventoryPath)
	}
	return inv
}

// assertEveryDischargingArtifactComplains checks that a refusal came
// from every artifact the acceptance names, not from one of them. The
// obligation check reads each one; a test that accepted a complaint from
// NOTICE alone would stay green after the source offer stopped being
// read.
func assertEveryDischargingArtifactComplains(t *testing.T, c Compliance, complaints []string) {
	t.Helper()
	joined := strings.Join(complaints, "\n")
	for _, a := range c.License.AcceptedNonPermissive {
		for _, rel := range a.DischargedBy {
			if !strings.Contains(joined, rel+" never") {
				t.Errorf("no complaint comes from %s, so that artifact was not read against the planted component:\n%s", rel, joined)
			}
		}
	}
}

// TestAnUnrecordedMPLModuleIsStillRefused is the falsification for the
// combined gate. Whatever admits go-cleanhttp and go-retryablehttp has to
// admit those two and not the licence they carry, or the category is a
// wider allowlist with extra steps.
//
// It goes through LicenceComplaints on purpose. The pure half keys its
// acceptance on the id and gives this module zero complaints (measured),
// so a test that called LicensePolicyComplaints here would be proving the
// hole rather than covering it.
func TestAnUnrecordedMPLModuleIsStillRefused(t *testing.T) {
	c := MustLoadCompliance()
	inv := liveInventory(t)
	if got := LicenceComplaints(c, inv, RepoReader()); len(got) != 0 {
		t.Fatalf("the real inventory already complains, so nothing below measures the planted module: %v", got)
	}
	inv.Components = append(inv.Components, Component{
		Name:        "github.com/hashicorp/vault-client-go",
		Version:     "v0.4.3",
		Ecosystem:   EcosystemGo,
		LicenseID:   "MPL-2.0",
		LicenseFile: "LICENSE",
		LinkedInto:  []string{"backup-manager"},
	})
	got := LicenceComplaints(c, inv, RepoReader())
	if len(got) == 0 {
		t.Fatal("an MPL-2.0 module nobody has recorded passes the gate, so accepting the licence once accepted every module that will ever arrive under it")
	}
	for _, complaint := range got {
		if !strings.Contains(complaint, "github.com/hashicorp/vault-client-go@v0.4.3") {
			t.Errorf("a complaint about something other than the planted module, so the refusal is not specific to it: %q", complaint)
		}
	}
	assertEveryDischargingArtifactComplains(t, c, got)
}

// TestADriftedVersionOfARecordedModuleIsStillRefused is the second
// falsification, and the one that decides whether the acceptance is
// about a release somebody read or about a licence id.
//
// v0.7.9 is a different upload with a different licence file and notices
// nobody has looked at, so it has to be refused as firmly as a module
// from a different project. The pure half cannot tell the two versions
// apart; the artifact check can, because the offer names v0.7.8.
func TestADriftedVersionOfARecordedModuleIsStillRefused(t *testing.T) {
	c := MustLoadCompliance()
	inv := liveInventory(t)
	const module = "github.com/hashicorp/go-retryablehttp"
	bumped := 0
	for i := range inv.Components {
		if inv.Components[i].Name == module {
			inv.Components[i].Version = "v0.7.9"
			bumped++
		}
	}
	if bumped != 1 {
		t.Fatalf("%s appears %d times in the real inventory, want once; the bump below drifted nothing", module, bumped)
	}
	got := LicenceComplaints(c, inv, RepoReader())
	if len(got) == 0 {
		t.Fatal("a recorded module at a version nobody read passes the gate, so the acceptance is per licence and not per release")
	}
	for _, complaint := range got {
		if !strings.Contains(complaint, module+"@v0.7.9") {
			t.Errorf("a complaint that does not name the drifted version: %q", complaint)
		}
	}
	assertEveryDischargingArtifactComplains(t, c, got)
}

// TestAMissingReaderIsAComplaintAndNotACrash. Everything else in this
// package answers a bad input with a complaint that names it, and this
// was the one call that answered with a nil pointer dereference.
func TestAMissingReaderIsAComplaintAndNotACrash(t *testing.T) {
	c, inv := acceptedFixture()
	got := LicenceObligationComplaints(c, inv, nil)
	if len(got) != 1 || !strings.Contains(got[0], "no reader was supplied") {
		t.Fatalf("a nil reader produced %v, want exactly one complaint saying nothing was checked", got)
	}
	// And through the entry point, where the pure half finds nothing
	// wrong and the complaint has to be what stops it reading as a pass.
	got = LicenceComplaints(c, inv, nil)
	if len(got) != 1 || !strings.Contains(got[0], "no reader was supplied") {
		t.Fatalf("LicenceComplaints with a nil reader produced %v, want the one complaint from the obligation half", got)
	}
}

// TestNoticeAttributesEveryComponent is Apache-2.0 §4(d) checked for
// coverage. A NOTICE that exists and names nine of fifty-nine components
// discharges the obligation for nine of them.
func TestNoticeAttributesEveryComponent(t *testing.T) {
	c := MustLoadCompliance()
	notice, err := os.ReadFile(Path(c.License.NoticeFile))
	if err != nil {
		t.Fatalf("compliance.json declares %s and it is not in the tree: %v", c.License.NoticeFile, err)
	}
	data, err := os.ReadFile(Path(InventoryPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", InventoryPath, err)
	}
	inv, err := ParseInventory(data)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", InventoryPath, err)
	}
	if len(inv.Components) == 0 {
		t.Fatal("the inventory lists no components, so this coverage check measured nothing")
	}
	body := string(notice)
	missing := 0
	for _, comp := range inv.Components {
		if !strings.Contains(body, comp.Name+"@"+comp.Version) {
			t.Errorf("%s never attributes %s@%s", c.License.NoticeFile, comp.Name, comp.Version)
			missing++
			if missing > 5 {
				t.Fatalf("... and more; %s is not generated from the inventory", c.License.NoticeFile)
			}
		}
	}
	if !strings.Contains(body, c.Project.Copyright) {
		t.Errorf("%s does not carry the project's own copyright line", c.License.NoticeFile)
	}
}

// ---------------------------------------------------------------------
// Distribution targets
// ---------------------------------------------------------------------

// TestEveryDistributionTargetIsDeclared pins compliance.json's target set
// to conformance.json's provider set, in both directions.
//
// One direction stops a new provider shipping with no recorded artifacts
// and no checksums, which is the vacuous-pass the parity rules are built
// around refusing. The other stops a target lingering here after the
// provider it named is gone, which turns into a hash of a file nobody
// ships.
func TestEveryDistributionTargetIsDeclared(t *testing.T) {
	c := MustLoadCompliance()
	conf := MustLoadConformance()

	providers := conf.ProviderIDs()
	sort.Strings(providers)
	declared := c.TargetIDs()

	if strings.Join(providers, ",") != strings.Join(declared, ",") {
		t.Errorf(`conformance.json has providers %v and compliance.json declares distribution targets %v.

Every shipping path needs recorded artifact digests or a stated reason it builds
none here; a target missing from compliance.json ships with no checksums and no
SBOM coverage, and one lingering after its provider is gone hashes a file nobody
distributes.`, providers, declared)
	}

	for _, id := range declared {
		target := c.Distribution.Targets[id]
		if len(target.Artifacts) == 0 && target.UnbuiltReason == "" {
			t.Errorf("target %q declares no artifacts and no reason", id)
		}
		for _, rel := range target.Artifacts {
			if _, err := os.Stat(Path(rel)); err != nil {
				t.Errorf("target %q declares %s, which is not in the tree: %v", id, rel, err)
			}
		}
	}
}

// TestUnbuiltTargetsAreRecordedWithTheirReason keeps the honest gap
// honest: a target this repository does not build has to say so in the
// bundle a reviewer reads, not only in the declaration a developer reads.
func TestUnbuiltTargetsAreRecordedWithTheirReason(t *testing.T) {
	c := MustLoadCompliance()
	p := readProvenance(t)

	recorded := map[string]string{}
	for _, u := range p.UnbuiltTargets {
		recorded[u.Target] = u.Reason
	}
	for _, id := range c.TargetIDs() {
		want := c.Distribution.Targets[id].UnbuiltReason
		got, listed := recorded[id]
		switch {
		case want == "" && listed:
			t.Errorf("the bundle lists %q as unbuilt and compliance.json declares artifacts for it", id)
		case want != "" && !listed:
			t.Errorf("compliance.json says %q builds nothing here and the bundle does not record it, so a reader sees a target with no artifacts and no explanation", id)
		case want != "" && got != want:
			t.Errorf("the bundle's reason for %q is not the declared one", id)
		}
	}
}

// ---------------------------------------------------------------------
// Performance evidence
// ---------------------------------------------------------------------

// TestPerformanceEvidenceSetIsPinned holds the metric names to the seven
// #81 lists, as a set rather than by iterating the thing being checked.
//
// The values are all pending, which is honest and is not a satisfied
// criterion: #165 captures the baselines and #167/#170 record the after
// numbers, and none of the three has merged. What this buys is that the
// gap is enumerated. A metric quietly dropped from the list would
// otherwise leave no trace at all, which is how "performance-neutral"
// becomes an unfalsifiable claim.
func TestPerformanceEvidenceSetIsPinned(t *testing.T) {
	want := []string{
		"api-read-latency", "config-write-latency", "idle-cpu", "idle-rss",
		"image-size", "startup-to-healthy", "transfer-throughput",
	}
	c := MustLoadCompliance()

	var got []string
	for _, m := range c.Performance.Metrics {
		got = append(got, m.ID)
		if m.Unit == "" {
			t.Errorf("performance metric %q records no unit, so its value would mean nothing", m.ID)
		}
		if m.Value == nil && m.Source == "" {
			t.Errorf("performance metric %q has no value and names no issue that will produce one, which is an absence with no owner", m.ID)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("compliance.json carries performance metrics %v and #81 names %v", got, want)
	}

	// The bundle carries what the declaration holds, so a reader of the
	// release does not have to open the declaration too.
	p := readProvenance(t)
	if len(p.Performance.Metrics) != len(c.Performance.Metrics) {
		t.Errorf("the bundle carries %d performance metrics and compliance.json declares %d", len(p.Performance.Metrics), len(c.Performance.Metrics))
	}
}

// ---------------------------------------------------------------------
// Signing
// ---------------------------------------------------------------------

// TestSigningRecordMatchesWhetherAnythingIsPublished is the same shape as
// the registry-digest guard, and for the same reason: an image nobody has
// pushed cannot have been signed, and a record saying otherwise would be
// the most damaging false statement in the whole bundle.
func TestSigningRecordMatchesWhetherAnythingIsPublished(t *testing.T) {
	canonical := MustLoad()
	p := readProvenance(t)

	if !canonical.Image.Published && p.Signing.Status != "unsigned" {
		t.Errorf("nothing has been pushed to %s and the bundle records signing status %q; there is no digest to sign",
			canonical.Image.Reference, p.Signing.Status)
	}
	if canonical.Image.Published && p.Signing.Status == "unsigned" && len(p.Signing.Note) == 0 {
		t.Error("the image is published and unsigned with no reason recorded")
	}
	if p.Signing.Identity == "" {
		t.Error("the bundle records no signing identity, so nobody could verify a signature even once one exists; the identity a verifier pins has to be settled before the first signature, not after it")
	}
	if p.Signing.Method != "sigstore-keyless" {
		t.Errorf("the bundle records signing method %q; keyless is the design, and it is what makes it true that this repository holds no signing key", p.Signing.Method)
	}
}

// ---------------------------------------------------------------------
// The third category: accepted, non-permissive, obligation recorded
// ---------------------------------------------------------------------
//
// Issue #402. Registering rclone's s3 backend links go-cleanhttp and
// go-retryablehttp, both MPL-2.0, into both shipped binaries, and there
// is no build tag that separates them from the backend. The one-line way
// out was to put MPL-2.0 on permissiveIds, and these tests exist because
// that would have been a lie in the file whose whole job is to be true.
//
// So acceptance is its own category with its own shape, and the tests
// below are about the shape rather than the licence: an acceptance that
// records nothing admits nothing, an acceptance whose artifacts do not
// carry the offer admits nothing, and a licence nobody accepted is
// refused exactly as it was before.

// acceptedFixture is a self-contained project with one accepted
// non-permissive licence and one component under it.
//
// The encumbered module's path carries a capital letter on purpose. A Go
// module proxy path is case-escaped, so the address a recipient follows
// is not the module path with a prefix glued on, and a fixture whose
// paths are all lower-case cannot tell the difference between the code
// getting that right and the code ignoring it.
func acceptedFixture() (Compliance, Inventory) {
	c := Compliance{
		License: ComplianceLicense{
			SPDXID:                         "Apache-2.0",
			CopyleftBlocksTheLicenseChoice: true,
			PermissiveIDs:                  []string{"MIT"},
			CopyleftIDs:                    []string{"MPL-2.0", "GPL-3.0-only"},
			AcceptedNonPermissive: []AcceptedNonPermissiveLicence{{
				SPDXID:          "MPL-2.0",
				Scope:           "File-level weak copyleft.",
				Obligation:      "MPL-2.0 §3.2: make the Source Code Form of the covered files available.",
				LicenceTextURL:  "https://mozilla.org/MPL/2.0/",
				SourceRetrieval: "https://proxy.golang.org/{module}/@v/{version}.zip",
				DischargedBy:    []string{"THE-OFFER"},
			}},
		},
	}
	inv := Inventory{
		ProjectLicense: "Apache-2.0",
		Components: []Component{
			{Name: "example.com/plain", Version: "v1.0.0", Ecosystem: EcosystemGo, LicenseID: "MIT", LinkedInto: []string{"a-binary"}},
			{Name: "example.com/Encumbered", Version: "v2.3.4", Ecosystem: EcosystemGo, LicenseID: "MPL-2.0", LinkedInto: []string{"a-binary"}},
		},
	}
	return c, inv
}

// encumberedSourceURL is the address the fixture's offer has to carry.
const encumberedSourceURL = "https://proxy.golang.org/example.com/!encumbered/@v/v2.3.4.zip"

// completeOffer is an artifact that discharges the fixture's obligation.
func completeOffer() string {
	return "This product links MPL-2.0 components.\n" +
		"Licence: https://mozilla.org/MPL/2.0/\n" +
		"example.com/Encumbered@v2.3.4\n" +
		"source: " + encumberedSourceURL + "\n"
}

// TestTheAcceptedNonPermissiveCategoryAdmitsOnlyWhatItRecords drives the
// pure half.
//
// Every row here is the same component under the same licence, and what
// changes is how much the acceptance wrote down. A category that admits
// a component on the strength of the id alone is permissiveIds with more
// syntax, which is the whole thing #402 refused.
func TestTheAcceptedNonPermissiveCategoryAdmitsOnlyWhatItRecords(t *testing.T) {
	cases := []struct {
		name string
		edit func(*AcceptedNonPermissiveLicence)
		want string
	}{
		{"a complete acceptance", func(*AcceptedNonPermissiveLicence) {}, ""},
		{"no scope", func(a *AcceptedNonPermissiveLicence) { a.Scope = "" }, "names no scope"},
		{"no obligation", func(a *AcceptedNonPermissiveLicence) { a.Obligation = "" }, "names no obligation"},
		{"nowhere to read the licence", func(a *AcceptedNonPermissiveLicence) { a.LicenceTextURL = "" }, "obtain the licence text"},
		{"nowhere to get the source", func(a *AcceptedNonPermissiveLicence) { a.SourceRetrieval = "" }, "obtain the source"},
		{"a source address that names a project rather than a release",
			func(a *AcceptedNonPermissiveLicence) { a.SourceRetrieval = "https://github.com/hashicorp/go-cleanhttp" },
			"does not vary by component"},
		{"a source address with no version in it",
			func(a *AcceptedNonPermissiveLicence) { a.SourceRetrieval = "https://proxy.golang.org/{module}/" },
			"does not vary by component"},
		{"nothing that carries the offer", func(a *AcceptedNonPermissiveLicence) { a.DischargedBy = nil }, "names no artifact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, inv := acceptedFixture()
			tc.edit(&c.License.AcceptedNonPermissive[0])
			got := LicensePolicyComplaints(c, inv)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("a complete acceptance still refuses its own component: %v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly one complaint containing %q, got %v", tc.want, got)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("the complaint does not say what is missing: got %q, want it to contain %q", got[0], tc.want)
			}
			if !strings.Contains(got[0], "example.com/Encumbered@v2.3.4") {
				t.Errorf("the complaint never names the component it refuses: %q", got[0])
			}
		})
	}
}

// TestAnUnacceptedNonPermissiveLicenceIsStillRefused is the control for
// the category as a whole.
//
// The point of a third category is that it admits ONE licence somebody
// read and decided about. If accepting MPL-2.0 also softened the answer
// for GPL-3.0, AGPL-3.0 or an id nobody has ever looked at, the category
// would be a wider allowlist and this suite would be theatre.
func TestAnUnacceptedNonPermissiveLicenceIsStillRefused(t *testing.T) {
	for _, id := range []string{"GPL-3.0-only", "AGPL-3.0-only", "LGPL-3.0-only", "SSPL-1.0", "EUPL-1.2", "MPL-1.1"} {
		t.Run(id, func(t *testing.T) {
			c, inv := acceptedFixture()
			inv.Components = append(inv.Components, Component{
				Name: "example.com/other", Version: "v9", Ecosystem: EcosystemGo, LicenseID: id, LinkedInto: []string{"a-binary"},
			})
			got := LicensePolicyComplaints(c, inv)
			if len(got) != 1 {
				t.Fatalf("%s produced %d complaints; accepting MPL-2.0 must not admit anything else: %v", id, len(got), got)
			}
			if !strings.Contains(got[0], "example.com/other@v9") {
				t.Errorf("the refusal names the wrong component: %q", got[0])
			}
		})
	}
}

// TestTheObligationIsCheckedAgainstTheArtifactsAndNotBelieved drives the
// half that reads the tree.
//
// Recording an obligation is the easy part. Each row here is a complete,
// well-formed acceptance whose artifact is missing one thing a recipient
// would need, because those are the states that read as compliant and
// are not.
func TestTheObligationIsCheckedAgainstTheArtifactsAndNotBelieved(t *testing.T) {
	offer := completeOffer()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"an artifact that carries the whole offer", offer, ""},
		{"an artifact that never names the licence", strings.ReplaceAll(offer, "MPL-2.0", "some licence"), "never names MPL-2.0"},
		{"an artifact that never says where to read the licence", strings.ReplaceAll(offer, "https://mozilla.org/MPL/2.0/", "somewhere"), "never gives https://mozilla.org/MPL/2.0/"},
		{"an artifact that names the module but not the version", strings.ReplaceAll(offer, "example.com/Encumbered@v2.3.4", "example.com/Encumbered"), "never names example.com/Encumbered@v2.3.4"},
		{"an artifact with no source address at all", strings.ReplaceAll(offer, encumberedSourceURL, "ask us"), "never gives " + encumberedSourceURL},
		{"an artifact whose source address skipped the proxy path escape",
			strings.ReplaceAll(offer, encumberedSourceURL, "https://proxy.golang.org/example.com/Encumbered/@v/v2.3.4.zip"),
			"never gives " + encumberedSourceURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, inv := acceptedFixture()
			got := LicenceObligationComplaints(c, inv, readerFor(map[string]string{"THE-OFFER": tc.body}))
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("a complete offer still complains: %v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly one complaint containing %q, got %v", tc.want, got)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("got %q, want it to contain %q", got[0], tc.want)
			}
		})
	}

	// An artifact that is declared and is not there. A missing file
	// read as an empty one contains none of the strings above, so it
	// would produce four complaints about content instead of one
	// about the file, and the four would send somebody editing a file
	// that does not exist.
	t.Run("an artifact that is not in the tree", func(t *testing.T) {
		c, inv := acceptedFixture()
		got := LicenceObligationComplaints(c, inv, readerFor(nil))
		if len(got) != 1 || !strings.Contains(got[0], "not in the tree") {
			t.Fatalf("expected one complaint about the missing artifact, got %v", got)
		}
	})

	// The mechanism rules. Both of these are ways for the acceptance
	// to still be in the file and stop deciding anything.
	t.Run("an id on the allowlist and in the accepted category at once", func(t *testing.T) {
		c, inv := acceptedFixture()
		c.License.PermissiveIDs = append(c.License.PermissiveIDs, "MPL-2.0")
		got := LicenceObligationComplaints(c, inv, readerFor(map[string]string{"THE-OFFER": offer}))
		if len(got) != 1 || !strings.Contains(got[0], "allowlist decides first") {
			t.Fatalf("an id on both lists passes silently, which is how the obligation stops being checked: %v", got)
		}
	})
	t.Run("an acceptance nothing in the inventory is under", func(t *testing.T) {
		c, inv := acceptedFixture()
		inv.Components = inv.Components[:1]
		got := LicenceObligationComplaints(c, inv, readerFor(map[string]string{"THE-OFFER": offer}))
		if len(got) != 1 || !strings.Contains(got[0], "permission granted in advance") {
			t.Fatalf("a standing acceptance for a licence the graph does not contain is exactly what this category must not become: %v", got)
		}
	})
	t.Run("an acceptance of an expression rather than a licence", func(t *testing.T) {
		c, inv := acceptedFixture()
		c.License.AcceptedNonPermissive[0].SPDXID = "(MIT OR MPL-2.0)"
		got := LicenceObligationComplaints(c, inv, readerFor(map[string]string{"THE-OFFER": offer}))
		if len(got) != 1 || !strings.Contains(got[0], "expression rather than a decided licence") {
			t.Fatalf("expected one complaint about the expression, got %v", got)
		}
	})
}

// TestTheModuleProxyPathEscapeIsApplied pins the one detail in the
// source address that a containment check cannot catch.
//
// LicenceObligationComplaints asks whether an artifact contains the URL
// it rendered. If SourceURLFor rendered a URL that 404s, the artifact
// would contain it, the check would pass, and the offer this project
// makes to recipients would be a dead link. github.com/IBM/go-sdk-core
// is in this project's own graph, and the escaped and unescaped forms of
// its proxy path were both asked of proxy.golang.org while writing this:
// escaped answers 200 and unescaped answers 404.
func TestTheModuleProxyPathEscapeIsApplied(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"github.com/hashicorp/go-cleanhttp", "github.com/hashicorp/go-cleanhttp"},
		{"github.com/IBM/go-sdk-core/v5", "github.com/!i!b!m/go-sdk-core/v5"},
		{"github.com/Azure/azure-sdk-for-go/sdk/azcore", "github.com/!azure/azure-sdk-for-go/sdk/azcore"},
		{"", ""},
	} {
		if got := EscapeGoModulePath(tc.in); got != tc.want {
			t.Errorf("EscapeGoModulePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	a := AcceptedNonPermissiveLicence{SourceRetrieval: "https://proxy.golang.org/{module}/@v/{version}.zip"}
	goComp := Component{Name: "github.com/IBM/go-sdk-core/v5", Version: "v5.23.1", Ecosystem: EcosystemGo}
	if got, want := a.SourceURLFor(goComp), "https://proxy.golang.org/github.com/!i!b!m/go-sdk-core/v5/@v/v5.23.1.zip"; got != want {
		t.Errorf("SourceURLFor a Go module = %q, want %q", got, want)
	}
	// npm package names are already lower-case by rule, and the
	// escape is a Go module proxy convention, so it is not applied
	// where it would be wrong.
	npm := Component{Name: "@scope/Thing", Version: "1.2.3", Ecosystem: EcosystemNPM}
	b := AcceptedNonPermissiveLicence{SourceRetrieval: "https://registry.npmjs.org/{module}/-/{version}.tgz"}
	if got, want := b.SourceURLFor(npm), "https://registry.npmjs.org/@scope/Thing/-/1.2.3.tgz"; got != want {
		t.Errorf("SourceURLFor an npm package = %q, want %q", got, want)
	}
}

// TestTheShippedAcceptanceIsCoherent pins the declaration in the file
// this release actually ships.
func TestTheShippedAcceptanceIsCoherent(t *testing.T) {
	c := MustLoadCompliance()
	if len(c.License.AcceptedNonPermissive) == 0 {
		// A failure and not a skip. Three other tests refuse an emptied
		// register, so nothing goes uncovered either way, but a skip here
		// is a green line in the run for a file that has lost the one
		// declaration this test exists to read, and an empty reading is a
		// refusal everywhere else in this package.
		t.Fatal("compliance.json accepts no non-permissive licence, so there is no declaration to check; an empty register is a refusal here, not a reason to skip")
	}
	seen := map[string]bool{}
	for _, a := range c.License.AcceptedNonPermissive {
		if seen[strings.ToUpper(a.SPDXID)] {
			t.Errorf("%s is accepted twice, so one of the two entries decides nothing", a.SPDXID)
		}
		seen[strings.ToUpper(a.SPDXID)] = true
		if c.IsPermissive(a.SPDXID) {
			t.Errorf("%s is on permissiveIds as well; the allowlist decides first, so the obligation recorded against it would never be read", a.SPDXID)
		}
		if !c.IsCopyleft(a.SPDXID) {
			t.Errorf("%s is accepted as non-permissive and is not on copyleftIds, so a reader of the two lists cannot tell what it is", a.SPDXID)
		}
		if len(a.Rationale) == 0 {
			t.Errorf("%s is accepted with no rationale; the decision to ship under a non-permissive licence is the part that wants writing down", a.SPDXID)
		}
	}
}

// TestAGenuineCopyleftLicenceIsRefusedAgainstTheRealFile is the
// falsification for TestLicensePolicyAgainstTheRealInventory.
//
// That test is green, and a green policy test has two explanations: the
// graph is acceptable, or the policy accepts anything. This is the
// positive control that tells them apart. It takes the real inventory,
// the real compliance.json, and plants one module under a licence this
// project has not accepted and would not, then asserts the refusal names
// that module and nothing else.
func TestAGenuineCopyleftLicenceIsRefusedAgainstTheRealFile(t *testing.T) {
	data, err := os.ReadFile(Path(InventoryPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", InventoryPath, err)
	}
	live, err := ParseInventory(data)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", InventoryPath, err)
	}
	c := MustLoadCompliance()
	if got := LicensePolicyComplaints(c, live); len(got) != 0 {
		t.Fatalf("the real inventory already complains, so nothing below measures the planted module: %v", got)
	}

	for _, id := range []string{"GPL-3.0-only", "AGPL-3.0-only", "GPL-2.0-only", "SSPL-1.0", "BUSL-1.1"} {
		t.Run(id, func(t *testing.T) {
			planted := Inventory{
				ProjectLicense: live.ProjectLicense,
				Components: append(append([]Component{}, live.Components...), Component{
					Name:       "github.com/planted/copyleft",
					Version:    "v1.0.0",
					Ecosystem:  EcosystemGo,
					LicenseID:  id,
					LinkedInto: []string{"backup-manager"},
				}),
			}
			got := LicensePolicyComplaints(c, planted)
			if len(got) != 1 {
				t.Fatalf("a %s module in the real inventory produced %d complaints, want exactly 1: %v", id, len(got), got)
			}
			if !strings.Contains(got[0], "github.com/planted/copyleft@v1.0.0") || !strings.Contains(got[0], id) {
				t.Errorf("the refusal does not name the planted module and its licence: %q", got[0])
			}
		})
	}

	// And the same for the shape that is not on any list, because a
	// denylist would let it through and this policy is an allowlist
	// plus one named acceptance.
	t.Run("a licence nobody has ever looked at", func(t *testing.T) {
		planted := Inventory{
			ProjectLicense: live.ProjectLicense,
			Components: append(append([]Component{}, live.Components...), Component{
				Name: "github.com/planted/unknown", Version: "v1.0.0", Ecosystem: EcosystemGo,
				LicenseID: "Parity-7.0.0", LinkedInto: []string{"backup-manager"},
			}),
		}
		got := LicensePolicyComplaints(c, planted)
		if len(got) != 1 || !strings.Contains(got[0], "github.com/planted/unknown@v1.0.0") {
			t.Fatalf("expected exactly one refusal naming the planted module, got %v", got)
		}
	})

	// A second MPL-2.0 module is the interesting one, because MPL-2.0
	// IS accepted. It has to be refused anyway, on the ground that
	// nothing tells its recipients where its source is: the
	// acceptance is per release, not per licence.
	t.Run("a second module under the accepted licence, with no offer for it", func(t *testing.T) {
		planted := Inventory{
			ProjectLicense: live.ProjectLicense,
			Components: append(append([]Component{}, live.Components...), Component{
				Name: "github.com/planted/alsompl", Version: "v1.0.0", Ecosystem: EcosystemGo,
				LicenseID: "MPL-2.0", LinkedInto: []string{"backup-manager"},
			}),
		}
		got := LicenceObligationComplaints(c, planted, RepoReader())
		if len(got) == 0 {
			t.Fatal("a new MPL-2.0 module with no source offer anywhere passes, so accepting the licence once accepted every future module under it")
		}
		joined := strings.Join(got, "\n")
		if !strings.Contains(joined, "github.com/planted/alsompl@v1.0.0") {
			t.Errorf("the complaints never name the module nothing offers source for: %s", joined)
		}
	})
}

// TestTheLicenceRationaleDescribesTodaysGraph is the check #402 was
// missing.
//
// The rationale is prose in a data file, and until this existed nothing
// read it. That is how it went on saying "Nothing in the graph is
// copyleft" for as long as it did after the s3 backend made it false:
// every other claim in this package is re-derived from the tree on every
// run, and the one claim the licence choice actually rests on was
// re-derived from nobody's memory.
func TestTheLicenceRationaleDescribesTodaysGraph(t *testing.T) {
	c := MustLoadCompliance()
	data, err := os.ReadFile(Path(InventoryPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", InventoryPath, err)
	}
	inv, err := ParseInventory(data)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", InventoryPath, err)
	}
	if len(inv.Components) == 0 {
		t.Fatal("the inventory lists nothing, so this measured no graph at all")
	}
	rationale := strings.Join(c.License.Rationale, "\n")
	if strings.TrimSpace(rationale) == "" {
		t.Fatal("compliance.json declares no licence rationale, so the reasoning behind the project's own licence lives in a commit message")
	}

	var nonPermissive []Component
	for _, comp := range inv.Components {
		if !c.IsPermissive(comp.LicenseID) {
			nonPermissive = append(nonPermissive, comp)
		}
	}

	// No count is re-derived here, because there is no count to
	// re-derive: TestTheProseCarriesNoCountNobodyChecks is what keeps it
	// that way. What the rationale has to get right is the non-permissive
	// set, by name and version, and that is checked below.
	if len(nonPermissive) == 0 {
		return
	}

	// The sentence that was false. It is pinned rather than left to
	// review, because it read perfectly well while being wrong and
	// review is what missed it.
	for _, claim := range []string{
		"Nothing in the graph is copyleft",
		"nothing in the graph is copyleft",
		"no copyleft",
	} {
		if strings.Contains(rationale, claim) {
			t.Errorf("the rationale says %q while %d component(s) in the inventory are not permissive; that sentence is the defect #402 reported", claim, len(nonPermissive))
		}
	}

	// And it has to name them. A rationale that admits copyleft
	// exists in the abstract and never says which components carry it
	// leaves a reader no way to check the claim against the
	// inventory.
	for _, comp := range nonPermissive {
		if !strings.Contains(rationale, comp.Name) {
			t.Errorf("the rationale never names %s, which is %s and is linked into %s", comp.Name, comp.LicenseID, strings.Join(comp.LinkedInto, " and "))
		}
		if !strings.Contains(rationale, comp.Version) {
			t.Errorf("the rationale names %s and not the version %s that shipped", comp.Name, comp.Version)
		}
		if !strings.Contains(rationale, comp.LicenseID) {
			t.Errorf("the rationale never names %s, the licence %s is under", comp.LicenseID, comp.Name)
		}
	}
}

// hardCodedCount matches a tally of the dependency set written into
// prose: "54 modules", "95 of the 97 components", "88 Go modules", "9
// production npm packages", "96 of them". It leaves measured figures
// alone ("17 go-retryablehttp symbols", "SHA-256 of the") because those
// are pinned to a version the same prose has to name, so they cannot go
// stale on their own.
var hardCodedCount = regexp.MustCompile(`(?:^|[\s(])(\d+) (?:of (?:the|them)\b|(?:[A-Za-z-]+ ){0,2}(?:modules?|packages?|components?|dependencies)\b)`)

// TestTheProseCarriesNoCountNobodyChecks is the other way of answering
// the stale-count problem. compliance.json's own header says nothing in
// it is hand-maintained arithmetic, and the rationale was the one place
// that was not true: it said 54 modules for a long time after the graph
// stopped holding 54, and then said 97 components with one of its three
// figures checked and two not. Deriving all three in a test would have
// kept them true and made every dependency bump a prose edit. The tally
// belongs to the inventory, which is regenerated from the graph on every
// run, so the prose describes how the set is derived and carries no
// number for it.
func TestTheProseCarriesNoCountNobodyChecks(t *testing.T) {
	// The pattern first, in both directions, or the sweep below is a
	// regexp that happens not to match anything.
	for _, s := range []string{
		"54 modules", "95 of the 97 components", "across 88 Go modules",
		"and 9 production npm packages in", "component, 96 of them across", "(97 components)",
	} {
		if !hardCodedCount.MatchString(s) {
			t.Errorf("hardCodedCount does not match %q, which is exactly the kind of sentence it exists to refuse", s)
		}
	}
	for _, s := range []string{
		"finds 17 go-retryablehttp symbols and 1 go-cleanhttp symbol",
		"the SHA-256 of the licence text", "§3.2 is discharged", "the two binaries", "Version 2.0",
	} {
		if hardCodedCount.MatchString(s) {
			t.Errorf("hardCodedCount matches %q, which is a measured figure and not a tally of the set", s)
		}
	}

	c := MustLoadCompliance()
	refuse := func(where string, i int, line string) {
		if m := hardCodedCount.FindString(line); m != "" {
			t.Errorf("%s line %d says %q; the tally belongs to %s, which is regenerated from the module graph on every run, and a copy of it in prose is a claim that goes stale silently",
				where, i, strings.TrimSpace(m), c.License.Inventory)
		}
	}
	for i, line := range c.License.Rationale {
		refuse("license.rationale", i, line)
	}
	for _, a := range c.License.AcceptedNonPermissive {
		for i, line := range a.Rationale {
			refuse("acceptedNonPermissive["+a.SPDXID+"].rationale", i, line)
		}
	}
	// And the written offer, which carried the same figure in the same
	// way and for the same reason.
	offer, ok := c.Link("source-offer")
	if !ok || offer.RepoPath == "" {
		t.Fatal("compliance.json declares no source-offer link with a repository path, so the written offer was not swept")
	}
	data, err := os.ReadFile(Path(offer.RepoPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", offer.RepoPath, err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		refuse(offer.RepoPath, i+1, line)
	}
}
