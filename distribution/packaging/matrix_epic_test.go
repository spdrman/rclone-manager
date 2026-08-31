package packaging

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// This file holds the proof that re-homing UGOS out of EPIC B's Phase 4
// gate re-homed exactly one thing: whose gate counts its column. #86 and
// #81 both now name six Phase 4 targets, and #170 states the rule this
// matrix has to implement, that the runner fails on the targets an EPIC
// claims and carries another EPIC's target as informational.
//
// The temptation was to delete the UGOS column instead. That would have
// thrown away a check that works, and it would have been invisible: the
// store-packaging rule reads in both directions, and it is what caught
// UGOS claiming app-store packaging with no UPK behind it. A column that
// is not there does not report a blocker, it reports nothing, and nothing
// reads as clean.
//
// One practical note for rerunning any of this. These checks read files
// all over the tree, and `go test` will happily serve a cached result
// after an edit it did not notice, because what it tracks reliably is
// this module's own inputs. Anything that turns on a file outside
// apps/common needs `-count=1`, and that includes regenerating the
// report.
//
// So every test here is a pair. One half says a UGOS cell cannot move
// Phase 4's verdict; the other half says the identical change somewhere
// EPIC B claims does move it, or that the guard still fires on UGOS. A
// test that only proves insensitivity cannot tell "correctly excluded"
// from "measuring nothing".

// runMatrix resolves every cell of c the way TestCrossProviderConformanceMatrix
// does, so a test can ask what a verdict would be under a declaration it
// changed. The checks are the real ones, against the real tree: the
// declaration is the only thing these tests fabricate.
func runMatrix(c Conformance) *Matrix {
	canonical := MustLoad()
	m := NewMatrix(c)
	for _, pid := range c.ProviderIDs() {
		put := providerUnderTest{id: pid, spec: c.Providers[pid], canonical: canonical}
		for _, cap := range c.Capabilities {
			m.Record(resolve(put, cap, c.Providers[pid].Cells[cap.ID]))
		}
	}
	return m
}

// copyConformance deep-copies far enough to mutate a cell safely.
// Provider.Cells is a map, so a shallow copy would edit the embedded
// conformance.json for every test that ran after it.
func copyConformance(c Conformance) Conformance {
	out := c
	out.Providers = make(map[string]Provider, len(c.Providers))
	for id, p := range c.Providers {
		cells := make(map[string]Cell, len(p.Cells))
		for k, v := range p.Cells {
			cells[k] = v
		}
		p.Cells = cells
		out.Providers[id] = p
	}
	return out
}

// withCell returns c with one provider's declaration of one capability
// replaced.
func withCell(c Conformance, provider, capability string, cell Cell) Conformance {
	out := copyConformance(c)
	out.Providers[provider].Cells[capability] = cell
	return out
}

// withoutCell returns c with one provider's declaration of one capability
// deleted, which is the omission the completeness guard exists to catch.
func withoutCell(c Conformance, provider, capability string) Conformance {
	out := copyConformance(c)
	delete(out.Providers[provider].Cells, capability)
	return out
}

// verdictKey is a verdict reduced to what it decides: which columns it
// covers, whether it is met, and every cell holding it open. Failure
// details are left out so a reworded message is not mistaken for a moved
// verdict; the columns it does not cover are left out because gaining an
// informational row is not a change to the gate, which is the distinction
// half of this file exists to draw.
func verdictKey(v Verdict) string {
	cells := make([]string, 0, len(v.Failures)+len(v.Blocked))
	for _, r := range append(append([]Result{}, v.Failures...), v.Blocked...) {
		cells = append(cells, fmt.Sprintf("%s/%s=%s", r.Provider, r.Capability, r.Outcome))
	}
	sort.Strings(cells)
	return fmt.Sprintf("epic=%s met=%v over=%v\n%s",
		v.Epic, v.Met(), v.Providers, strings.Join(cells, "\n"))
}

// TestAnotherEpicsColumnCannotMovePhaseFoursVerdict is the whole point of
// Provider.Epic, and its positive control is the second half.
func TestAnotherEpicsColumnCannotMovePhaseFoursVerdict(t *testing.T) {
	base := MustLoadConformance()
	if got := base.Providers["ugos"].Epic; got == PhaseFourEpic {
		t.Fatalf("ugos is declared to EPIC %s, the same epic Phase 4 gates, so there is nothing here to test", got)
	}

	before := runMatrix(base).Verdict(PhaseFourEpic)
	if len(before.Failures) != 0 {
		t.Fatalf("Phase 4 already has %d failing cell(s), so this test cannot tell a new one from an old one: %v", len(before.Failures), before.Failures)
	}

	// The flip. UGOS's state-persistence is declared blocked on #83
	// because there is no UPK and therefore no compose to read a mount
	// out of. Declaring it supported is the worst thing a UGOS cell can
	// say: the check still fails, so the cell resolves FAIL, and before
	// this change a FAIL anywhere in the matrix reddened the one run that
	// stood for Phase 4.
	flipped := runMatrix(withCell(base, "ugos", "state-persistence", Cell{Declared: DeclSupported}))
	if got := flipped.Results["ugos"]["state-persistence"].Outcome; got != OutcomeFail {
		t.Fatalf("the flipped UGOS cell resolved %s, not %s, so nothing was proved", got, OutcomeFail)
	}
	after := flipped.Verdict(PhaseFourEpic)
	if verdictKey(after) != verdictKey(before) {
		t.Errorf("a UGOS cell moved Phase 4's verdict.\nbefore:\n%s\nafter:\n%s", verdictKey(before), verdictKey(after))
	}
	if !containsString(after.Informational, "ugos") {
		t.Errorf("UGOS is not carried as an informational column either, so its blockers are reported to nobody: %v", after.Informational)
	}
	if len(after.Failures) != 0 {
		t.Errorf("Phase 4 counted %d failure(s) after a failure in a column EPIC D owns: %v", len(after.Failures), after.Failures)
	}

	// The positive control. The same change to a column EPIC B claims has
	// to move the verdict, or the assertions above are satisfied by a
	// verdict that measures nothing at all.
	//
	// The cell has moved twice, and for the same reason both times: a
	// control planted on a cell that is blocked on an open issue stops
	// proving anything the day that issue is fixed, silently, because the
	// flip then resolves PASS and every assertion below it is satisfied
	// by a check that no longer fails. It was Proxmox's
	// release-manifest-integrity until #174 was fixed, then Synology's
	// embedded-window until #169 shipped the provider bundles that made
	// it real. Proxmox's app-store-packaging is the end of that road:
	// unsupported not because nobody has built it yet, but because
	// Proxmox VE has no third-party application store to package into at
	// all, which is not a thing a later issue can close.
	control := runMatrix(withCell(base, "proxmox", "app-store-packaging", Cell{Declared: DeclSupported}))
	if got := control.Results["proxmox"]["app-store-packaging"].Outcome; got != OutcomeFail {
		t.Fatalf("the control flip resolved %s, not %s, so the control proves nothing either", got, OutcomeFail)
	}
	controlled := control.Verdict(PhaseFourEpic)
	if verdictKey(controlled) == verdictKey(before) {
		t.Errorf("the same flip on an EPIC %s column left Phase 4's verdict untouched, so this verdict is insensitive to everything rather than to UGOS:\n%s", PhaseFourEpic, verdictKey(controlled))
	}
	if len(controlled.Failures) != 1 {
		t.Errorf("Phase 4 counted %d failure(s) after a failure in a column it claims, want 1: %v", len(controlled.Failures), controlled.Failures)
	}
}

// TestAnotherEpicsColumnIsStillSubjectToTheStalenessGuard is the other
// half of the re-homing. "EPIC D gates it" must not quietly become "EPIC
// D checks it, one day". Drift in a UGOS declaration still fails the
// cell, exactly as it does anywhere else, and the run still reports it.
func TestAnotherEpicsColumnIsStillSubjectToTheStalenessGuard(t *testing.T) {
	base := MustLoadConformance()
	const capability = "no-bundled-secrets"

	if got := runMatrix(base).Results["ugos"][capability]; got.Outcome != OutcomePass {
		t.Fatalf("UGOS's %s resolves %s today, so declaring it unsupported would not be drift and this test would prove nothing", capability, got.Outcome)
	}

	stale := runMatrix(withCell(base, "ugos", capability, Cell{
		Declared: DeclUnsupported,
		Reason:   "a declaration the repository has outgrown, which is what this test is",
	}))
	r := stale.Results["ugos"][capability]
	if r.Outcome != OutcomeFail {
		t.Errorf("UGOS declared %q unsupported while the check passes and the cell resolved %s; the staleness guard stopped applying to the column when its gate moved", capability, r.Outcome)
	}
	if !strings.Contains(r.Detail, "the check now passes") {
		t.Errorf("the stale UGOS cell reported %q, which does not say the declaration is out of date", r.Detail)
	}

	// And it is still only EPIC D's to fix: a stale declaration in this
	// column is a real, red failure of the run, and it is not one of
	// Phase 4's.
	if failures := stale.Verdict(PhaseFourEpic).Failures; len(failures) != 0 {
		t.Errorf("the stale UGOS cell entered Phase 4's verdict: %v", failures)
	}
}

// TestAnotherEpicsColumnIsStillSubjectToTheCompletenessGuard covers the
// guard the whole design rests on: omission is a failure, because an
// undeclared capability reads as a passing one.
func TestAnotherEpicsColumnIsStillSubjectToTheCompletenessGuard(t *testing.T) {
	c := MustLoadConformance()
	caps := c.CapabilityIDs()

	if findings := auditDeclarations(c.Providers["ugos"], caps); len(findings) > 0 {
		t.Errorf("UGOS does not satisfy the completeness guard as declared: %s", strings.Join(findings, "; "))
	}

	// Positive control: take one declaration away and confirm the guard
	// still notices, in the column whose gate moved.
	stripped := withoutCell(c, "ugos", "backup-root-containment")
	findings := auditDeclarations(stripped.Providers["ugos"], caps)
	if len(findings) == 0 {
		t.Errorf("the completeness guard did not notice a UGOS capability with no declaration at all, so a column another epic gates could drop one silently")
	}
}

// TestAClaimedTargetWithNoResultFailsTheGateAndAnInformationalOneDoesNot
// is #170's rule stated as a test, both halves of it: "a missing claimed
// target fails, and a missing informational target does not". An unrun
// target is the failure mode a matrix is most likely to hide, because a
// column with no cells in it looks exactly like a column with nothing
// wrong.
func TestAClaimedTargetWithNoResultFailsTheGateAndAnInformationalOneDoesNot(t *testing.T) {
	c := MustLoadConformance()

	unrunInformational := runMatrix(c)
	unrunInformational.Results["ugos"] = map[string]Result{}
	if failures := unrunInformational.Verdict(PhaseFourEpic).Failures; len(failures) != 0 {
		t.Errorf("a UGOS column with no results failed Phase 4 with %d cell(s); an EPIC D target that has not shipped is recorded here, not gated here", len(failures))
	}

	unrunClaimed := runMatrix(c)
	unrunClaimed.Results["proxmox"] = map[string]Result{}
	if failures := unrunClaimed.Verdict(PhaseFourEpic).Failures; len(failures) != len(c.Capabilities) {
		t.Errorf("a Proxmox column with no results produced %d failure(s), want one per capability (%d): a claimed target nobody ran must fail rather than read as clean", len(failures), len(c.Capabilities))
	}
}

// TestRegisteringAnotherEpicsTargetDoesNotDisturbPhaseFour is #170's
// acceptance criterion that the matrix "accepts a target registered by
// another EPIC without being modified, demonstrated by registering one".
// Registered here as a target that fails everything, which is the loudest
// thing a new column can be.
func TestRegisteringAnotherEpicsTargetDoesNotDisturbPhaseFour(t *testing.T) {
	base := MustLoadConformance()
	before := verdictKey(runMatrix(base).Verdict(PhaseFourEpic))

	registered := copyConformance(base)
	cells := map[string]Cell{}
	for _, id := range base.CapabilityIDs() {
		cells[id] = Cell{Declared: DeclSupported}
	}
	registered.Providers["someothernas"] = Provider{
		DisplayName: "Some Other NAS",
		Tier:        "C",
		Epic:        "E",
		WorkPackage: "E1.1",
		Metadata:    Metadata{Kind: "none", Root: "apps/someothernas"},
		Cells:       cells,
	}

	m := runMatrix(registered)
	failed := 0
	for _, r := range m.Results["someothernas"] {
		if r.Outcome == OutcomeFail {
			failed++
		}
	}
	if failed == 0 {
		t.Fatalf("the registered target passed everything against a directory that does not exist, so it cannot demonstrate anything")
	}

	v := m.Verdict(PhaseFourEpic)
	if got := verdictKey(v); got != before {
		t.Errorf("registering an EPIC E target with %d failing cell(s) moved Phase 4's verdict.\nbefore:\n%s\nafter:\n%s", failed, before, got)
	}
	if !containsString(v.Informational, "someothernas") {
		t.Errorf("the registered target is not carried as informational either, so it is in no epic's report: %v", v.Informational)
	}
}
