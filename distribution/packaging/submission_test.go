package packaging

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Issue #90 (B5.4)'s runner: one submission preflight across every target
// this repository distributes, recording a per-target readiness verdict
// rather than a single pass or fail, plus EPIC B Phase 6's adapter
// conformance drift gate.
//
// The shape is deliberately the one #86 already established, reused
// rather than re-invented, because the two guards that make it work are
// the whole reason it works and they are not obvious:
//
//  1. completeness. A target that omits a rule fails. Absence reads as a
//     pass in every result set ever printed, so omission has to be the
//     thing that breaks.
//  2. staleness. A declaration that is not a plain "supported" still has
//     its check run, and a check that PASSES against an unsupported,
//     not-applicable or blocked declaration fails the cell. Without it,
//     submission.json would slowly become a list of reasons not to look.
//
// Both come from matrix.go unchanged. What this file adds is the rules
// themselves, the per-target readiness verdict #178 consumes, and the
// wiring that makes "a new NAS costs metadata, not an implementation"
// something a build can fail on.

// ---------------------------------------------------------------------
// The target under test
// ---------------------------------------------------------------------

// targetUnderTest is a providerUnderTest plus what submission needs: the
// store it is submitted to, the files its package actually carries, and
// the shared bundle it draws its listing from.
type targetUnderTest struct {
	providerUnderTest
	sub    SubmissionProvider
	bundle Bundle
	// conf is the cross-provider conformance declaration this target is
	// registered in, carried rather than re-loaded from the embedded
	// file. That is what lets a target this repository has never shipped
	// be registered and decided by all eight drift elements: an element
	// that reaches for MustLoadConformance() can only ever resolve a
	// target that is already checked in, which would have made "a new
	// NAS costs metadata, not an implementation" untestable.
	conf Conformance
}

// scanArtifact runs one hard rule over the files this target's package
// carries. A target whose scan reads no files fails: "found nothing" and
// "read nothing" produce the same empty violation list and only one of
// them is a pass.
func (t targetUnderTest) scanArtifact(rule func(path, text string) []Violation) (bool, string) {
	if len(t.sub.ArtifactFiles) == 0 {
		return false, "this target declares no packaged files, so there is nothing to inspect and a clean result would prove nothing"
	}
	v, files, err := ScanArtifact(t.sub.ArtifactFiles, rule)
	if err != nil {
		return false, err.Error()
	}
	if files == 0 {
		return false, fmt.Sprintf("the declared packaged files (%v) hold nothing to read", t.sub.ArtifactFiles)
	}
	if len(v) > 0 {
		return false, fmt.Sprintf("%d finding(s) across %d packaged file(s): %s", len(v), files, oneLine(v))
	}
	return true, fmt.Sprintf("%d packaged file(s) inspected, clean", files)
}

func (t targetUnderTest) hasStore() bool { return t.sub.Store.Kind == "catalog" }

// readBundle reads one shared submission asset.
func (t targetUnderTest) readBundle(capability string) (string, string, error) {
	rel, ok := t.bundle.Assets[capability]
	if !ok {
		return "", "", fmt.Errorf("submission.json's bundle declares no asset for %q", capability)
	}
	data, err := os.ReadFile(Path(rel))
	return rel, string(data), err
}

// ---------------------------------------------------------------------
// The rules, one per capability
// ---------------------------------------------------------------------

// hardRule adapts one of §73's four negative verification items to the
// runner. Each is a function over text, which is what lets
// submission_controls_test.go point it at a deliberately broken package
// and watch it fire; a negative claim nobody has seen fail is not a
// claim.
func hardRule(rule func(path, text string) []Violation) func(targetUnderTest) (bool, string) {
	return func(t targetUnderTest) (bool, string) { return t.scanArtifact(rule) }
}

// driftRule turns one of #81's eight contract elements into a check.
//
// The four elements the cross-provider matrix already decides resolve by
// consuming that matrix's verdict for this exact column, and "satisfied"
// means it resolved PASS and nothing else. A NOT_APPLICABLE is not a
// pass: the matrix declined to decide, and laundering a declined decision
// into a green drift cell is precisely the move this gate exists to stop.
func driftRule(e DriftElement) func(targetUnderTest) (bool, string) {
	return func(t targetUnderTest) (bool, string) {
		if e.MatrixCapability != "" {
			conf := t.conf
			var cap Capability
			for _, c := range conf.Capabilities {
				if c.ID == e.MatrixCapability {
					cap = c
				}
			}
			if cap.ID == "" {
				return false, fmt.Sprintf("this element defers to conformance capability %q, which no longer exists", e.MatrixCapability)
			}
			r := resolve(t.providerUnderTest, cap, conf.Providers[t.id].Cells[cap.ID])
			detail := fmt.Sprintf("the cross-provider conformance matrix resolves %s as %s for this target: %s", cap.ID, r.Outcome, r.Detail)
			return r.Outcome == OutcomePass, detail
		}

		base, err := APIContractBase()
		if err != nil {
			return false, err.Error()
		}
		svcs, err := t.services()
		if err != nil {
			return false, err.Error()
		}
		if len(svcs) == 0 {
			return false, "this target declares no container services, so there is nothing for the drift gate to compare against the canonical runtime contract"
		}
		var all []Violation
		for _, s := range svcs {
			all = append(all, e.Service(s, t.canonical, base)...)
		}
		if len(all) > 0 {
			return false, fmt.Sprintf("%d drift(s) from the canonical runtime contract: %s", len(all), oneLine(all))
		}
		return true, fmt.Sprintf("%d service(s) agree with the canonical runtime contract", len(svcs))
	}
}

// materialRule holds one shared listing asset to what a store reviewer
// opens it expecting to find.
func materialRule(capability string, minChars int, required []string) func(targetUnderTest) (bool, string) {
	return func(t targetUnderTest) (bool, string) {
		if !t.hasStore() {
			return false, "this target has no store or catalog to submit to, so there is no listing for this asset to appear on"
		}
		rel, text, err := t.readBundle(capability)
		if err != nil {
			return false, err.Error()
		}
		if v := CheckMaterial(rel, text, minChars, required); len(v) > 0 {
			return false, fmt.Sprintf("%d finding(s) in %s: %s", len(v), rel, oneLine(v))
		}
		return true, fmt.Sprintf("%s covers %v", rel, required)
	}
}

func checkStoreIcon(t targetUnderTest) (bool, string) {
	if !t.hasStore() {
		return false, "this target has no store or catalog to submit to, so there is no listing for an icon to appear on"
	}
	rel, text, err := t.readBundle("materials-icon")
	if err != nil {
		return false, err.Error()
	}
	if v := CheckStoreIcon(rel, text, 256); len(v) > 0 {
		return false, fmt.Sprintf("%d finding(s) in %s: %s", len(v), rel, oneLine(v))
	}
	return true, fmt.Sprintf("%s renders as a store listing icon, not only as an in-app mark", rel)
}

// checkScreenshots is the automated half of a material no laptop can
// produce. A screenshot is of the app running on the provider's own
// hardware, so what is decidable here is that the target says how many it
// needs, that the bundle says what to capture, and that the §82 procedure
// has a section for this target telling an operator how. The hardware
// half is what PENDING_OPERATOR records.
func checkScreenshots(t targetUnderTest) (bool, string) {
	if !t.hasStore() {
		return false, "this target has no store or catalog to submit to, so there is no listing for screenshots to appear on"
	}
	if t.sub.Store.Screenshots <= 0 {
		return false, "this target is submitted to a store and asks for no screenshots, which no store accepts; the count belongs in submission.json"
	}
	rel, text, err := t.readBundle("materials-screenshots")
	if err != nil {
		return false, err.Error()
	}
	want := fmt.Sprintf("%d", t.sub.Store.Screenshots)
	if !strings.Contains(text, t.sub.Store.Name) {
		return false, fmt.Sprintf("%s never mentions %s, so an operator capturing screenshots has nothing telling them what that store asks for", rel, t.sub.Store.Name)
	}
	if !strings.Contains(text, want) {
		return false, fmt.Sprintf("%s never states the %s screenshots submission.json records for %s", rel, want, t.sub.Store.Name)
	}
	return t.acceptanceCovers("screenshot")
}

// acceptanceCovers reports whether the §82 store-submission procedure has
// a section for this target that actually covers a topic.
//
// ProcedureSection rather than a search of the whole document, on
// purpose: the word "screenshot" appearing in another target's section,
// or in the preamble, is not this target having a procedure.
func (t targetUnderTest) acceptanceCovers(topic string) (bool, string) {
	if !HasArtifact(t.spec) {
		return false, "there is no package for this target yet, so nobody can install one and no operator step against it can be run; a procedure that cannot be executed is not coverage"
	}
	path := Path(t.bundle.Acceptance)
	want := regexp.MustCompile(`(?i)^#+ .*\b` + regexp.QuoteMeta(t.spec.DisplayName))
	body, err := ProcedureSection(path, want)
	if err != nil {
		return false, err.Error()
	}
	if strings.TrimSpace(body) == "" {
		return false, fmt.Sprintf("%s has no section for %s, so no operator has been told how to finish this target's submission on real hardware", t.bundle.Acceptance, t.spec.DisplayName)
	}
	if !strings.Contains(strings.ToLower(body), strings.ToLower(topic)) {
		return false, fmt.Sprintf("%s's %s section never covers %q", t.bundle.Acceptance, t.spec.DisplayName, topic)
	}
	return true, fmt.Sprintf("%s's %s section covers %q; the hardware run has not happened", t.bundle.Acceptance, t.spec.DisplayName, topic)
}

// checkSupportSourceLicense is the one material this work package cannot
// finish on its own. The support and source halves are written here; the
// licence half is B5.2's (#88) OSS compliance deliverable, and inventing
// a licence for somebody else's project is not a thing a preflight gets
// to do.
func checkSupportSourceLicense(t targetUnderTest) (bool, string) {
	if !t.hasStore() {
		return false, "this target has no store or catalog to submit to, so there is no listing for support, source and licence materials to appear on"
	}
	rel, text, err := t.readBundle("materials-support-source-license")
	if err != nil {
		return false, err.Error()
	}
	if v := CheckMaterial(rel, text, 400, []string{"support", "source", "licen"}); len(v) > 0 {
		return false, fmt.Sprintf("%d finding(s) in %s: %s", len(v), rel, oneLine(v))
	}
	if _, err := os.Stat(Path("LICENSE")); err != nil {
		return false, fmt.Sprintf("%s names the project's support and source, and the repository ships no LICENSE file for a reviewer to read: %v", rel, err)
	}
	return true, fmt.Sprintf("%s covers support, source and licence, and LICENSE is in the tree", rel)
}

// WorkflowChecklistItems are the rows a target with no store accounts for
// instead of the seven listing assets. §73 sets the precedent itself:
// Dockge is "supported by Compose compatibility rather than by packaging,
// so it needs a documented workflow rather than a submission bundle".
var WorkflowChecklistItems = []string{"install", "update", "remove", "recovery", "support"}

func checkSubmissionChecklist(t targetUnderTest) (bool, string) {
	rel := t.sub.Store.Checklist
	if rel == "" {
		return false, "this target records no submission checklist and no documented workflow, which is how a target gets forgotten"
	}
	data, err := os.ReadFile(Path(rel))
	if err != nil {
		return false, err.Error()
	}
	required := WorkflowChecklistItems
	kind := "documented workflow"
	if t.hasStore() {
		required = MaterialsCapabilityIDs
		kind = t.sub.Store.Name + " submission checklist"
	}
	if v := CheckChecklist(rel, string(data), required); len(v) > 0 {
		return false, fmt.Sprintf("%d finding(s) in the %s: %s", len(v), kind, oneLine(v))
	}
	if t.sub.Store.Reference != "" && !strings.Contains(string(data), t.sub.Store.Reference) {
		return false, fmt.Sprintf("%s never cites %s, so nobody can re-derive whether it still matches that target's own required format", rel, t.sub.Store.Reference)
	}
	if !strings.Contains(string(data), t.bundle.Recovery) {
		return false, fmt.Sprintf("%s never points at %s, so this target's support materials do not reach the recovery steps §73 requires", rel, t.bundle.Recovery)
	}
	return true, fmt.Sprintf("%s accounts for every row of its %s and cites the store's own requirements", rel, kind)
}

// alertKinds are §71's four conditions, verbatim, as core/service/alerts.go
// names them.
var alertKinds = []string{"STALE_BACKUP", "REPEATED_FAILURE", "HOST_KEY_CHANGED", "CRITICAL_STORAGE_PRESSURE"}

// checkProactiveAlertDelivery is the automated half of §73's "proactive
// alerts work" item.
//
// Two things are decidable without hardware, and both matter. The shipped
// UI has to render the conditions at all, because on every Tier B and C
// target the platform's own notification centre is UNSUPPORTED (§4A) and
// the administrator's path to a stale backup is the app's own dashboard,
// which is a UI path and not a log line. And the §82 procedure has to
// have a section for this target naming all four conditions, so the
// operator run that decides it is written down before it happens rather
// than after.
func checkProactiveAlertDelivery(t targetUnderTest) (bool, string) {
	if !HasArtifact(t.spec) {
		return false, "there is no package for this target yet, so no administrator can install it and watch an alert arrive"
	}
	health, err := os.ReadFile(Path("ui/shared/src/components/HealthSummary.tsx"))
	if err != nil {
		return false, err.Error()
	}
	for _, want := range []string{"setsStale", "setsFailing"} {
		if !strings.Contains(string(health), want) {
			return false, fmt.Sprintf("the shared dashboard no longer renders %s, so a stale or failing backup reaches the administrator through nothing but a log line on every target whose platform notifications are unsupported", want)
		}
	}
	path := Path(t.bundle.Acceptance)
	want := regexp.MustCompile(`(?i)^#+ .*\b` + regexp.QuoteMeta(t.spec.DisplayName))
	body, err := ProcedureSection(path, want)
	if err != nil {
		return false, err.Error()
	}
	if strings.TrimSpace(body) == "" {
		return false, fmt.Sprintf("%s has no section for %s, so nobody has written down how a proactive alert is exercised on it", t.bundle.Acceptance, t.spec.DisplayName)
	}
	var missing []string
	for _, kind := range alertKinds {
		if !strings.Contains(body, kind) {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("%s's %s section never exercises %v; §71 has four conditions and a procedure that covers three of them reports a pass for the one nobody tried", t.bundle.Acceptance, t.spec.DisplayName, missing)
	}
	return true, fmt.Sprintf("the dashboard renders the conditions and %s's %s section exercises all four; the hardware run has not happened", t.bundle.Acceptance, t.spec.DisplayName)
}

// terminalOnlyMarkers are the things a recovery step cannot ask for from
// an administrator who has no terminal, which §73's own acceptance
// criterion is written about and docs/recovery.md is made almost entirely
// of.
// A fenced shell block rather than a keyword list. "ssh" is a word this
// document has to be able to use, because a changed host key is one of
// the three failures it covers and naming the protocol is not the same as
// telling somebody to open a terminal. What is decidable is that the
// document contains no command for anyone to run.
var terminalOnlyMarkers = []string{"```bash", "```sh", "```console", "```shell", "sqlite3", "docker exec", "docker compose"}

func checkRecoveryDocsNoTerminal(t targetUnderTest) (bool, string) {
	rel := t.bundle.Recovery
	data, err := os.ReadFile(Path(rel))
	if err != nil {
		return false, err.Error()
	}
	text := string(data)
	for _, want := range []string{"stale", "retention", "host key"} {
		if !strings.Contains(strings.ToLower(text), want) {
			return false, fmt.Sprintf("%s never covers the %q failure, which §73 names as one of the common no-terminal recovery paths", rel, want)
		}
	}
	for _, marker := range terminalOnlyMarkers {
		if strings.Contains(text, marker) {
			return false, fmt.Sprintf("%s tells the administrator to run %s, and it is the document for the administrator who has no terminal; a recovery path that needs one belongs in docs/recovery.md", rel, backquote(strings.TrimSpace(marker)))
		}
	}
	return true, fmt.Sprintf("%s covers the stale-backup, failed-retention and changed-host-key paths without a terminal", rel)
}

// checkArtifactProvenance is the honest answer to "can anyone check that
// the bytes this store receives are the bytes this repository built".
//
// Today: no. #174's release manifest pins a commit that is not an
// ancestor of main, so its recorded hashes describe a build that is not
// in this history and there is nothing real on the other side of any
// comparison. This work package does not own that manifest and will not
// paper over it, so every EPIC B row is declared blocked on #174 and the
// gate reports undecided rather than letting a green run imply traceable
// bytes.
func checkArtifactProvenance(t targetUnderTest) (bool, string) {
	if len(t.sub.ArtifactFiles) == 0 {
		return false, "this target ships no packaged files, so there is nothing whose provenance could be recorded"
	}
	if ok, detail := checkReleaseManifestIntegrity(t.providerUnderTest); !ok {
		return false, detail
	}
	for _, f := range t.sub.ArtifactFiles {
		if _, err := os.Stat(Path(f)); err != nil {
			return false, fmt.Sprintf("this target declares packaged file %s, which is not in the tree: %v", backquote(f), err)
		}
	}
	return true, fmt.Sprintf("the release manifest is reachable and every one of this target's %d declared packaged path(s) is in the tree", len(t.sub.ArtifactFiles))
}

// submissionChecks maps a preflight capability id to the rule that
// decides it. TestEverySubmissionCapabilityHasACheck pins this map's key
// set to submission.json's capability list in both directions.
var submissionChecks = map[string]func(targetUnderTest) (bool, string){
	"no-self-update":                   hardRule(CheckNoSelfUpdate),
	"no-floating-tag":                  hardRule(CheckNoFloatingTag),
	"no-privileged-mode":               hardRule(CheckNoPrivilegedMode),
	"no-mandatory-telemetry":           hardRule(CheckNoMandatoryTelemetry),
	"materials-description":            materialRule("materials-description", 400, []string{"backup", "sftp", "retention"}),
	"materials-icon":                   checkStoreIcon,
	"materials-screenshots":            checkScreenshots,
	"materials-release-notes":          materialRule("materials-release-notes", 400, []string{"1.0.0"}),
	"materials-privacy-disclosure":     materialRule("materials-privacy-disclosure", 400, []string{"telemetry", "personal data"}),
	"materials-permission-rationale":   materialRule("materials-permission-rationale", 400, []string{"cap_drop", "read-only", "non-root"}),
	"materials-support-source-license": checkSupportSourceLicense,
	"materials-submission-checklist":   checkSubmissionChecklist,
	"proactive-alert-delivery":         checkProactiveAlertDelivery,
	"recovery-docs-no-terminal":        checkRecoveryDocsNoTerminal,
	"artifact-provenance":              checkArtifactProvenance,
}

func init() {
	// The eight drift elements register themselves rather than being
	// listed again by hand. #81's claim is that a new target declares
	// metadata and inherits every rule; a second hand-written list of
	// the rules is the first place that stops being true.
	for _, e := range DriftElements {
		submissionChecks[e.Capability] = driftRule(e)
	}
}

// ---------------------------------------------------------------------
// The guards
// ---------------------------------------------------------------------

func TestEverySubmissionCapabilityHasACheck(t *testing.T) {
	s := MustLoadSubmission()

	declared := map[string]bool{}
	for _, cap := range s.Capabilities {
		declared[cap.ID] = true
		if _, ok := submissionChecks[cap.ID]; !ok {
			t.Errorf("preflight capability %q is declared but no rule decides it", cap.ID)
		}
		if cap.Title == "" || cap.Spec == "" {
			t.Errorf("preflight capability %q has no title or no spec reference", cap.ID)
		}
		if cap.Mode != ModeRepo && cap.Mode != ModeOperator {
			t.Errorf("preflight capability %q has mode %q, want %q or %q", cap.ID, cap.Mode, ModeRepo, ModeOperator)
		}
	}
	for id := range submissionChecks {
		if !declared[id] {
			t.Errorf("rule %q exists but submission.json declares no such capability, so no target is measured against it", id)
		}
	}

	// The drift gate's eight elements, pinned as a set rather than
	// iterated out of the thing being checked. #81 names eight; a ninth
	// that nobody declared, or one quietly dropped, is the failure this
	// catches and a loop over DriftElements never could.
	want := []string{
		"drift-api-compatibility", "drift-architecture-support", "drift-expected-ports",
		"drift-forbidden-privileges", "drift-health-check", "drift-image-reference",
		"drift-required-mounts", "drift-runtime-profile",
	}
	if got := DriftCapabilityIDs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the drift gate covers %v, and #81 names %v", got, want)
	}
	for _, id := range want {
		if !declared[id] {
			t.Errorf("#81's drift element %q is not declared in submission.json, so no adapter is held to it", id)
		}
	}
}

// TestEverySubmissionTargetIsARegisteredAdapter is what makes this one
// suite rather than two. A target exists once, in conformance.json; this
// file adds only what submission needs. A row here with no column there
// is a target nothing checks structurally, and a column there with no row
// here is a target nobody preflighted, which is the more dangerous of the
// two because it is the one that ships.
func TestEverySubmissionTargetIsARegisteredAdapter(t *testing.T) {
	c := MustLoadConformance()
	s := MustLoadSubmission()

	for id := range s.Providers {
		if _, ok := c.Providers[id]; !ok {
			t.Errorf("submission.json registers %q, and conformance.json declares no such adapter, so its tier, epic and packaging metadata come from nowhere", id)
		}
	}
	for _, id := range c.ProviderIDs() {
		if _, ok := s.Providers[id]; !ok {
			t.Errorf("conformance.json declares adapter %q and submission.json has no row for it: a distributed target nobody preflighted", id)
		}
	}
}

// TestEveryPreflightTargetDeclaresEveryRule is the completeness guard,
// borrowed whole from the cross-provider matrix. It runs over every
// column including the one EPIC D owns: which gate counts a column
// decides nothing about how hard it is checked.
func TestEveryPreflightTargetDeclaresEveryRule(t *testing.T) {
	c := MustLoadSubmission().AsConformance(MustLoadConformance())
	caps := c.CapabilityIDs()

	for _, pid := range c.ProviderIDs() {
		t.Run(pid, func(t *testing.T) {
			for _, finding := range auditPreflightDeclarations(c.Providers[pid], caps) {
				t.Error(finding)
			}
		})
	}
}

// auditPreflightDeclarations is auditDeclarations with the capability set
// this file owns. Kept as its own function returning findings rather than
// reporting them for the same reason the original is: the guard the whole
// design rests on is the one most worth being able to prove still fires.
func auditPreflightDeclarations(p Provider, caps []string) []string {
	var findings []string
	add := func(format string, args ...any) { findings = append(findings, fmt.Sprintf(format, args...)) }

	for _, id := range caps {
		cell, ok := p.Cells[id]
		if !ok {
			add("declares no outcome for %q; an undeclared rule reads as a passing one", id)
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
				add("%q is declared blocked on %s with no expectedDetail; without one the blocker excuses ANY failure of that rule", id, cell.Blocker)
			}
		default:
			add("%q is declared %q, which is not one of supported/unsupported/not-applicable/blocked", id, cell.Declared)
		}
	}
	for id := range p.Cells {
		if _, ok := submissionChecks[id]; !ok {
			add("declares an outcome for %q, which is not a preflight rule", id)
		}
	}
	sort.Strings(findings)
	return findings
}

// TestEveryMaterialHasAnAsset pins the bundle to the capability list in
// both directions. An asset nothing measures is dead weight; a materials
// capability with no asset behind it is a rule that can only ever error.
func TestEveryMaterialHasAnAsset(t *testing.T) {
	s := MustLoadSubmission()

	for _, id := range MaterialsCapabilityIDs {
		rel, ok := s.Bundle.Assets[id]
		if !ok {
			t.Errorf("materials capability %q has no asset in submission.json's bundle", id)
			continue
		}
		if _, err := os.Stat(Path(rel)); err != nil {
			t.Errorf("the bundle points %q at %s, which is not in the tree: %v", id, rel, err)
		}
	}
	for id := range s.Bundle.Assets {
		found := false
		for _, want := range MaterialsCapabilityIDs {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the bundle carries an asset for %q, which is not a materials capability, so nothing measures it", id)
		}
	}
	for _, rel := range []string{s.Bundle.Acceptance, s.Bundle.Recovery} {
		if _, err := os.Stat(Path(rel)); err != nil {
			t.Errorf("the bundle names %s, which is not in the tree: %v", rel, err)
		}
	}
	for id, sp := range s.Providers {
		if sp.Store.Checklist == "" {
			t.Errorf("%s records no checklist and no documented workflow", id)
			continue
		}
		if _, err := os.Stat(Path(sp.Store.Checklist)); err != nil {
			t.Errorf("%s names checklist %s, which is not in the tree: %v", id, sp.Store.Checklist, err)
		}
		switch sp.Store.Kind {
		case "catalog":
			if sp.Store.Name == "" {
				t.Errorf("%s declares a catalog target with no store name", id)
			}
		case "none":
		default:
			t.Errorf("%s declares store kind %q, want \"catalog\" or \"none\"", id, sp.Store.Kind)
		}
	}
}

// TestTheAPIContractIsStatedOnceAndAgreedThreeTimes is the repository-wide
// half of drift-api-compatibility. The base path is written in the
// engine's router, in the Web UI host's proxy, and in the shared browser
// client, and any two of them agreeing while the third moved is a
// deployment where the UI loads and every request 404s.
func TestTheAPIContractIsStatedOnceAndAgreedThreeTimes(t *testing.T) {
	base, err := APIContractBase()
	if err != nil {
		t.Fatal(err)
	}
	if base != APIBasePath {
		t.Errorf("the engine, the Web UI host and the browser client agree on %q, and this package's contract constant is %q", base, APIBasePath)
	}
}

// ---------------------------------------------------------------------
// The run
// ---------------------------------------------------------------------

func runPreflight(t *testing.T, s Submission, conf Conformance) PreflightRun {
	t.Helper()
	c := s.AsConformance(conf)
	canonical := MustLoad()
	m := NewMatrix(c)

	for _, pid := range c.ProviderIDs() {
		tut := targetUnderTest{
			providerUnderTest: providerUnderTest{id: pid, spec: c.Providers[pid], canonical: canonical},
			sub:               s.Providers[pid],
			bundle:            s.Bundle,
			conf:              conf,
		}
		epic := c.Providers[pid].Epic
		t.Run(pid, func(t *testing.T) {
			for _, cap := range c.Capabilities {
				t.Run(cap.ID, func(t *testing.T) {
					check, ok := submissionChecks[cap.ID]
					if !ok {
						t.Fatalf("no rule decides %q", cap.ID)
					}
					satisfied, detail := check(tut)
					r := resolveWith(SubmissionSource, pid, cap, c.Providers[pid].Cells[cap.ID], satisfied, detail)
					m.Record(r)
					t.Logf("%s: %s", r.Outcome, r.Detail)
					switch {
					case r.Outcome != OutcomeFail:
					case epic != SubmissionEpic:
						t.Errorf("%s / %s: %s\n\nThis column is EPIC %s's, and the Phase 5 submission gate is not computed over it. It is checked here on the same terms as every other target, so this is a real failure and it is EPIC %s's to fix: update submission.json's declaration rather than the rule.", pid, cap.ID, r.Detail, epic, epic)
					default:
						t.Errorf("%s / %s: %s", pid, cap.ID, r.Detail)
					}
				})
			}
		})
	}

	if got := m.Count(OutcomePass); got < len(c.Providers) {
		t.Errorf("only %d cells passed across %d targets; the preflight is not deciding anything", got, len(c.Providers))
	}
	for _, pid := range c.ProviderIDs() {
		if len(m.Results[pid]) != len(c.Capabilities) {
			t.Errorf("%s produced %d results for %d rules", pid, len(m.Results[pid]), len(c.Capabilities))
		}
	}
	return NewPreflightRun(s, m)
}

func TestProviderStoreSubmissionPreflight(t *testing.T) {
	s := MustLoadSubmission()
	run := runPreflight(t, s, MustLoadConformance())
	m := run.Matrix

	v := m.Verdict(SubmissionEpic)
	for _, r := range v.Failures {
		t.Errorf("Phase 5 submission gate: %s / %s failed: %s", r.Provider, r.Capability, r.Detail)
	}
	t.Logf("Phase 5 submission gate over %v: %d failed, %d undecided, met=%v. Informational columns, gated by another epic: %v",
		v.Providers, len(v.Failures), len(v.Blocked), v.Met(), v.Informational)

	// Every EPIC B target has a recorded verdict, and no EPIC B target
	// is recorded NOT_YET_APPLICABLE: that state means there is no
	// artifact, and a target this EPIC ships has one.
	for _, id := range v.Providers {
		row := run.ReadinessOf(id)
		if row.Readiness == "" {
			t.Errorf("%s has no recorded readiness verdict, and #178 refuses to submit without one", id)
		}
		if row.Readiness == ReadyNotYetApplicable {
			t.Errorf("%s is recorded %s, and EPIC B ships it: %s", id, row.Readiness, row.Why)
		}
		t.Logf("%s: %s (%s)", id, row.Readiness, row.Why)
	}

	// UGREEN, in the mechanism and out of the gate. Both halves are
	// asserted, because either one alone is satisfied by something
	// wrong: a row that is merely absent from the verdict could be a
	// column nobody checks, and a row that is merely checked could still
	// be holding Phase 5 open.
	ugreen := run.ReadinessOf("ugos")
	if ugreen.Readiness != ReadyNotYetApplicable {
		t.Errorf("the UGREEN row is recorded %s, and while EPIC D's #83 has produced no .UPK it must read %s", ugreen.Readiness, ReadyNotYetApplicable)
	}
	for _, id := range v.Providers {
		if id == "ugos" {
			t.Error("the UGREEN column is inside EPIC B's Phase 5 verdict, so a target on hardware nobody here owns can hold this gate open")
		}
	}
	if len(m.Results["ugos"]) != len(s.Capabilities) {
		t.Errorf("the UGREEN column produced %d of %d results; out of the gate must not become out of the run", len(m.Results["ugos"]), len(s.Capabilities))
	}

	comparePreflightReport(t, run)
}

func comparePreflightReport(t *testing.T, run PreflightRun) {
	t.Helper()
	path := Path(PreflightReportPath)

	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (the recorded verdict is the deliverable #178 consumes, not an optional artifact)", PreflightReportPath, err)
	}
	want, err := SplicePreflightReport(string(existing), run.Render())
	if err != nil {
		t.Fatal(err)
	}
	if string(existing) == want {
		return
	}
	if os.Getenv("CONFORMANCE_UPDATE") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatalf("write %s: %v", PreflightReportPath, err)
		}
		t.Logf("rewrote %s", PreflightReportPath)
		return
	}
	t.Errorf("%s is out of date with a real run. Regenerate it with:\n\n\tcd distribution && CONFORMANCE_UPDATE=1 GOWORK=off go test ./packaging/ -count=1 -run TestProviderStoreSubmissionPreflight\n", PreflightReportPath)
}
