package packaging

import (
	"fmt"
	"os"
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
// behind choosing Apache-2.0: nothing in the graph is copyleft. That is a
// fact about today's go.mod, not a property of the project, so it is
// checked rather than remembered.
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
		{"an MPL component", base(Component{Name: "example.com/m", Version: "v1", Ecosystem: EcosystemGo, LicenseID: "MPL-2.0"}), "which is copyleft"},
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

// TestLicensePolicyAgainstTheRealInventory is the thin caller.
func TestLicensePolicyAgainstTheRealInventory(t *testing.T) {
	data, err := os.ReadFile(Path(InventoryPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v\n\nGenerate it with: (cd apps/common && go run ./cmd/provenance -write)", InventoryPath, err)
	}
	inv, err := ParseInventory(data)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", InventoryPath, err)
	}
	if inv.Schema != InventorySchema {
		t.Errorf("%s declares schema %q and this code reads %q", InventoryPath, inv.Schema, InventorySchema)
	}
	for _, complaint := range LicensePolicyComplaints(MustLoadCompliance(), inv) {
		t.Error(complaint)
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
