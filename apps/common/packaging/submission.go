// Provider store and catalog submission preflight (issue #90 / WP5.4,
// docs/EPIC-B-multi-nas.md §73), plus the adapter conformance drift gate
// EPIC B's Phase 6 (#81) lands here.
//
// This file is the layer above matrix.go, not a second copy of it.
// matrix.go answers "does this provider's package hold together"; this
// one answers "is that package fit to hand to a store reviewer", which is
// a different question with a different audience and a different failure
// cost. A conformance cell that fails is a bug somebody fixes; a
// submission that fails is a rejection from a reviewer this repository
// has no relationship with and cannot appeal to.
//
// # Why the verdict is recorded rather than computed on demand
//
// EPIC D's #178 consumes this: it refuses to submit the UGREEN package
// without a recorded verdict from here, and it does not re-run these
// checks. A verdict that only exists inside a green test run is not
// something another EPIC can refuse to proceed without, so every run
// renders docs/conformance/submission-preflight.md and the suite fails
// when the checked-in report and a fresh run disagree.
//
// # Why UGREEN is in the mechanism and out of the gate
//
// §73's work package names four stores, one of which is UGREEN's App
// Center, and splitting the preflight per store is exactly how two stores
// end up preflighted by two different rules. So UGREEN registers here
// like every other target. What it must never do is block: the .UPK is
// EPIC D's #83, on hardware nobody in this repository owns, and a Phase 5
// gate that waits on it is a gate that cannot close. HasArtifact below is
// how that stays honest without a flag anybody can set: a provider whose
// packaging metadata declares no artifact and ships no store artifact has
// nothing to preflight, its row is recorded NOT_YET_APPLICABLE, and the
// day EPIC D's package lands in conformance.json the row starts being
// decided by these same checks with no edit here.
//
// # Why so much of this is a negative claim
//
// Four of §73's six verification items are negative ("no self-update, no
// `latest`, no privileged mode, no mandatory telemetry"), and a negative
// claim is worth nothing without a positive control proving it can fail.
// Every rule below is therefore a function over text or over a parsed
// Service, so submission_controls_test.go can point it at a deliberately
// broken input and watch it fire. A rule written as three strings.Contains
// calls inside an assertion cannot be pointed anywhere.
package packaging

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed submission.json
var submissionJSON []byte

// ---------------------------------------------------------------------
// The declaration file
// ---------------------------------------------------------------------

// Store is where a provider's package is actually submitted, and what
// that store asks for. A provider with no store is not an error and not
// an omission: a Tier C deployment profile has nowhere to submit to, and
// §73's own treatment of Dockge ("supported by Compose compatibility
// rather than by packaging, so it needs a documented workflow rather
// than a submission bundle") is the shape that case takes.
type Store struct {
	// Kind is "catalog" for a target with a store submission, "none" for
	// one whose deliverable is a documented deployment workflow.
	Kind string `json:"kind"`
	// Name is the store's own name, as a reviewer would recognise it.
	Name string `json:"name"`
	// Checklist is this target's own submission checklist, or its
	// documented workflow when Kind is "none". Always present: a target
	// with no recorded deliverable at all is how a target gets forgotten.
	Checklist string `json:"checklist"`
	// Reference is the store's own published submission requirements,
	// which is what "matches that provider's own submission checklist
	// format" is measured against by a human. Recorded here so the
	// checklist can be re-derived rather than trusted.
	Reference string `json:"reference"`
	// Screenshots is how many the store asks for. Zero for Kind "none".
	Screenshots int `json:"screenshots"`
}

// SubmissionProvider is one target registered with this preflight.
//
// Deliberately thin. Everything about the adapter itself (its §4A tier,
// its owning EPIC, its packaging metadata, its acceptance procedure)
// comes from conformance.json, because a second declaration of the same
// facts is a second thing to keep in step. What a target adds here is
// only what submission needs: where it is submitted, which checked-in
// files the package actually carries, and its own cells.
type SubmissionProvider struct {
	Store Store `json:"store"`
	// ArtifactFiles are the repository paths whose bytes the packaged
	// artifact actually carries, relative to the repository root. Files
	// or directories. This is what the four hard rules are scanned over,
	// and it is deliberately NOT the provider's whole apps/<id>/ tree: a
	// README that explains why the package never asks for privileged mode
	// is not a package asking for privileged mode, and a rule that cannot
	// tell those apart is a rule nobody can leave switched on.
	//
	// Empty only for a target with no artifact yet, which HasArtifact
	// reports and the runner records as NOT_YET_APPLICABLE.
	ArtifactFiles []string        `json:"artifactFiles"`
	Cells         map[string]Cell `json:"cells"`
}

// Bundle is the submission material every target draws from. One set,
// not one per store: §73 asks for descriptions, icons, screenshots,
// release notes, privacy disclosures, permission rationale and
// support/source/license materials, and every one of those describes the
// same product. A per-store copy of the privacy disclosure is a per-store
// opportunity for one of them to be wrong.
//
// What varies per store is the checklist that draws on them, which is
// Store.Checklist.
type Bundle struct {
	// Assets maps a materials capability id to the file that satisfies
	// it. Keyed by capability id rather than by a name of its own so
	// TestEveryMaterialHasAnAsset can pin the two sets together: an asset
	// nothing measures, or a materials capability with no asset behind
	// it, is a failure rather than a quiet gap.
	Assets map[string]string `json:"assets"`
	// Acceptance is the §82 operator procedure covering the half of this
	// work package no laptop can decide: capturing the store screenshots
	// and exercising a proactive alert end to end on real hardware.
	Acceptance string `json:"acceptance"`
	// Recovery is the no-terminal recovery documentation §73 asks for.
	Recovery string `json:"recovery"`
}

// Submission is submission.json.
type Submission struct {
	Capabilities []Capability                  `json:"capabilities"`
	Bundle       Bundle                        `json:"bundle"`
	Providers    map[string]SubmissionProvider `json:"providers"`
}

// LoadSubmission parses the embedded submission.json.
func LoadSubmission() (Submission, error) {
	var s Submission
	if err := json.Unmarshal(submissionJSON, &s); err != nil {
		return Submission{}, fmt.Errorf("packaging: parse submission.json: %w", err)
	}
	return s, nil
}

// MustLoadSubmission is LoadSubmission for callers that cannot proceed
// without it.
func MustLoadSubmission() Submission {
	s, err := LoadSubmission()
	if err != nil {
		panic(err)
	}
	return s
}

// CapabilityIDs returns the preflight capability ids in declaration order.
func (s Submission) CapabilityIDs() []string {
	out := make([]string, 0, len(s.Capabilities))
	for _, c := range s.Capabilities {
		out = append(out, c.ID)
	}
	return out
}

// AsConformance projects this preflight onto the shape matrix.go's
// runner, guards, verdict and outcome arithmetic already understand,
// borrowing each target's tier, epic and packaging metadata from the
// cross-provider conformance declaration rather than restating them.
//
// This is what makes the drift gate one suite rather than a second one.
// A target that registers with #86's matrix is registered with this
// preflight by the same act; the Verdict type that keeps a column another
// EPIC owns out of EPIC B's gate keeps UGREEN out of this one, on exactly
// the same terms and through exactly the same code.
func (s Submission) AsConformance(c Conformance) Conformance {
	out := Conformance{Capabilities: s.Capabilities, Providers: map[string]Provider{}}
	for id, sp := range s.Providers {
		base := c.Providers[id]
		base.Cells = sp.Cells
		out.Providers[id] = base
	}
	return out
}

// HasArtifact reports whether there is anything of this provider's to
// preflight yet.
//
// Derived from the packaging metadata rather than declared, and that is
// the whole point. "This target is not ready yet" is the single most
// useful thing for a declaration to lie about, because it turns every
// check off at once and reads as a schedule rather than as an excuse. A
// target has an artifact when its metadata names a format this repository
// can parse or it ships a file a store would receive; UGREEN has neither
// today and both the moment #83 lands.
func HasArtifact(p Provider) bool {
	return (p.Metadata.Kind != "" && p.Metadata.Kind != "none") || len(p.Metadata.StoreArtifacts) > 0
}

// ---------------------------------------------------------------------
// The readiness verdict
// ---------------------------------------------------------------------

// Readiness is one provider's recorded answer to "is this fit to submit".
// It is the format #178 consumes, which is why it is a named type with a
// fixed set of values rather than a sentence: EPIC D refuses to submit
// without one, and "refuses unless the prose sounds positive" is not a
// refusal.
type Readiness string

const (
	// ReadySubmit: every rule that applies to this target was decided
	// here and held. External approval is still outside this repository's
	// control (§75), and this says nothing about it.
	ReadySubmit Readiness = "READY_TO_SUBMIT"
	// ReadyPendingOperator: nothing failed, and something this repository
	// cannot decide is outstanding: a screenshot to capture, an alert to
	// watch arrive, on hardware. The automated half held.
	ReadyPendingOperator Readiness = "READY_PENDING_OPERATOR"
	// ReadyBlocked: nothing failed, and at least one rule could not reach
	// a verdict for a reason tracked elsewhere. Not a pass. Reporting a
	// blocked rule as either a pass or a failure is a lie in a different
	// direction, and the blocked rules here are real: #174's release
	// manifest pins a commit that is not on main, so no parity claim
	// about the shipped bytes can be checked at all today.
	ReadyBlocked Readiness = "BLOCKED"
	// ReadyNot: at least one rule failed. Submitting anyway is how a
	// store rejection happens.
	ReadyNot Readiness = "NOT_READY"
	// ReadyNotYetApplicable: there is no artifact for this target yet, so
	// there is nothing to be fit or unfit. UGREEN's row until EPIC D's
	// #83 produces the .UPK.
	ReadyNotYetApplicable Readiness = "NOT_YET_APPLICABLE"
)

// ProviderReadiness is one recorded row.
type ProviderReadiness struct {
	Provider    string
	DisplayName string
	Store       Store
	Epic        Epic
	Readiness   Readiness
	// Blockers are the tracked issues holding this row back, sorted.
	Blockers []string
	// Failed and Pending name the capabilities behind the verdict, so a
	// row is readable without the whole matrix next to it.
	Failed  []string
	Pending []string
	// Why is the one-sentence reason, for a reader who is not going to
	// count cells.
	Why string
}

// ReadinessFor computes one provider's recorded verdict from its finished
// cells.
//
// The precedence is deliberate and is not "worst wins" arithmetic. A
// target with no artifact is NOT_YET_APPLICABLE whatever its cells say,
// because a rule that passes against nothing has proved nothing; then a
// real failure; then a blocker; then an outstanding hardware step; and
// only a run with none of those is fit to submit.
func ReadinessFor(m *Matrix, s Submission, id string) ProviderReadiness {
	pr := m.Conformance.Providers[id]
	sp := s.Providers[id]
	out := ProviderReadiness{
		Provider:    id,
		DisplayName: pr.DisplayName,
		Store:       sp.Store,
		Epic:        pr.Epic,
	}

	seen := map[string]bool{}
	for _, cap := range s.Capabilities {
		r, ok := m.Results[id][cap.ID]
		if !ok {
			out.Failed = append(out.Failed, cap.ID)
			continue
		}
		switch r.Outcome {
		case OutcomeFail:
			out.Failed = append(out.Failed, cap.ID)
		case OutcomeBlocked:
			if b := sp.Cells[cap.ID].Blocker; b != "" && !seen[b] {
				seen[b] = true
				out.Blockers = append(out.Blockers, b)
			}
		case OutcomePendingOperator:
			out.Pending = append(out.Pending, cap.ID)
		}
	}
	sort.Strings(out.Blockers)

	where := "submission"
	if sp.Store.Kind == "none" {
		where = "distribution"
	}

	// Deliberately after the cells rather than before them. A target
	// with no artifact still has every rule run and every blocker
	// recorded, because "out of the gate" must not quietly become "out
	// of the run": a row whose blockers column reads "none" while its
	// cells are blocked is a row that has stopped describing the run it
	// came from.
	if !HasArtifact(pr) {
		out.Readiness = ReadyNotYetApplicable
		out.Why = fmt.Sprintf("%s declares no package this repository can inspect and ships no store artifact, so there is nothing to preflight yet; its shared listing materials are recorded on their own merits above", pr.DisplayName)
		return out
	}

	switch {
	case len(out.Failed) > 0:
		out.Readiness = ReadyNot
		out.Why = fmt.Sprintf("%d %s rule(s) failed against the built artifact: %s", len(out.Failed), where, strings.Join(out.Failed, ", "))
	case len(out.Blockers) > 0:
		out.Readiness = ReadyBlocked
		out.Why = fmt.Sprintf("nothing failed, and %d rule(s) could not be decided, tracked by %s", len(blockedCells(m, s, id)), strings.Join(out.Blockers, " and "))
	case len(out.Pending) > 0:
		out.Readiness = ReadyPendingOperator
		out.Why = fmt.Sprintf("every rule this repository can decide held; %d step(s) need the real platform: %s", len(out.Pending), strings.Join(out.Pending, ", "))
	default:
		out.Readiness = ReadySubmit
		out.Why = "every rule held and nothing is outstanding here; external review remains outside this repository's control"
	}
	return out
}

// blockedCells is the blocked cells behind a row. The Why sentence counts
// cells rather than issues on purpose: two cells blocked on one issue is
// two undecided rules, not one, and a row that reads "1 rule could not be
// decided" when six could not is the kind of undercount that makes a
// blocked verdict look nearly ready.
func blockedCells(m *Matrix, s Submission, id string) []string {
	var out []string
	for _, cap := range s.Capabilities {
		if m.Results[id][cap.ID].Outcome == OutcomeBlocked {
			out = append(out, cap.ID)
		}
	}
	return out
}

// ---------------------------------------------------------------------
// The four hard rules (§73 WP5.4)
// ---------------------------------------------------------------------

const (
	// RuleSelfUpdate is a mechanism that replaces reviewed code without
	// going back through review (§45.4, §77 invariant #9).
	RuleSelfUpdate = "self-update-mechanism"
	// RuleFloatingTag is an image reference that can resolve to different
	// bytes tomorrow (§8, §45.4).
	RuleFloatingTag = "floating-image-tag"
	// RuleMandatoryTelemetry is an outbound call the operator did not ask
	// for (§45.5, §75).
	RuleMandatoryTelemetry = "mandatory-telemetry"
	// RuleForbiddenPrivilege is a privilege the runtime profile handed
	// back after taking it away.
	RuleForbiddenPrivilege = "forbidden-privilege"
	// RuleContractDrift is an adapter disagreeing with canonical.json.
	RuleContractDrift = "canonical-contract-drift"
	// RuleMissingMaterial is a submission asset that is absent, empty, or
	// unusable in the form a store would receive it.
	RuleMissingMaterial = "missing-submission-material"
)

var (
	// Enabling forms only, every one of them. `privileged: false`,
	// `<Privileged>false</Privileged>` and a README sentence explaining
	// that the package never asks for privileged mode all contain the
	// word, and a rule that matches the word matches all three. Each
	// pattern below matches the affirmative spelling and nothing else,
	// which is what lets these run over real packaging metadata.
	watchtowerRe   = regexp.MustCompile(`(?i)com\.centurylinklabs\.watchtower\.enable\s*[:=]\s*["']?true`)
	podmanAutoRe   = regexp.MustCompile(`(?i)io\.containers\.autoupdate\s*[:=]\s*["']?\s*(registry|image|local)`)
	pullAlwaysRe   = regexp.MustCompile(`(?i)(?:pull_policy\s*:\s*["']?always|--pull[= ]+always)`)
	autoUpdateRe   = regexp.MustCompile(`(?i)\b(?:self|auto)[-_ ]?update\w*\s*[:=]\s*["']?(?:true|yes|on|1|always)\b`)
	unraidAutoRe   = regexp.MustCompile(`(?i)<\s*(?:AutoUpdate|UpdateAvailable)\s*>\s*true\s*<`)
	fetchExecuteRe = regexp.MustCompile(`(?m)^[^#\n]*\b(curl|wget|apt-get\s+install|apk\s+add|pip\s+install|go\s+install|npm\s+i(?:nstall)?)\b`)

	// A tag is anything after the last ":" that is not a port and not a
	// digest. Written as one expression over an image reference rather
	// than as a search for the string "latest", because the far more
	// common way to ship a floating reference is to leave the tag off
	// entirely, and searching for "latest" cannot see that at all.
	// Horizontal whitespace only, and deliberately so. Written with
	// `\s*` between the key and its value, these match across a newline:
	// a bare `image:` introducing a nested mapping swallows the line
	// break and captures the NEXT key as the image reference, which is
	// how apps/truenas/catalog/ix_values.yaml was first reported as
	// deploying an image called `reference:` with no tag. The positive
	// control caught it; `[ \t]` is the fix.
	composeImageRe = regexp.MustCompile(`(?m)^[ \t]*image[ \t]*:[ \t]*["']?([^"'\s#]+)`)
	unraidImageRe  = regexp.MustCompile(`(?s)<\s*Repository\s*>(.*?)<\s*/\s*Repository\s*>`)
	yamlRefRe      = regexp.MustCompile(`(?m)^[ \t]*reference[ \t]*:[ \t]*["']?([^"'\s#]+)`)

	// Telemetry that is on unless somebody turns it off. The value half
	// matters: TELEMETRY_ENABLED=false is the disclosure §45.5 asks for,
	// not a violation of it.
	telemetryVarRe = regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:TELEMETRY|ANALYTICS|USAGE_STATS|PHONE_HOME|CRASH_REPORT|METRICS_PUSH)[A-Z0-9_]*)\s*[:=]\s*["']?([^\s"',#]+)`)
	sentryRe       = regexp.MustCompile(`(?i)\b(SENTRY_DSN|BUGSNAG_API_KEY|DATADOG_API_KEY|NEW_RELIC_LICENSE_KEY)\s*[:=]\s*["']?([^\s"',#]+)`)
	urlRe          = regexp.MustCompile(`https?://[^\s"'<>)\]}]+`)

	privilegedYAMLRe  = regexp.MustCompile(`(?im)^\s*privileged\s*:\s*["']?true\b`)
	privilegedXMLRe   = regexp.MustCompile(`(?is)<\s*Privileged\s*>\s*true\s*<`)
	privilegedFlagRe  = regexp.MustCompile(`--privileged(?:\s|=true|$)`)
	networkModeHostRe = regexp.MustCompile(`(?im)^\s*(?:network_mode|pid|ipc|uts)\s*:\s*["']?host\b`)
	capAddRe          = regexp.MustCompile(`(?im)^\s*cap_add\s*:`)
	unconfinedRe      = regexp.MustCompile(`(?i)(?:seccomp|apparmor)\s*[:=]\s*unconfined`)
	noNewPrivFalseRe  = regexp.MustCompile(`(?i)no-new-privileges\s*:\s*false`)
)

// telemetryHostAllowlist are hosts a submission bundle may legitimately
// point at: the project's own source, issue tracker, registry and icon.
//
// An allowlist rather than a denylist of known analytics vendors, for the
// same reason scan.go's metadataExtensions is an allowlist: a denylist of
// the vendors somebody thought of is defeated by the next one. Anything
// that is not a public DNS name at all (localhost, a .local hostname, a
// bare container name on a compose network, a private address literal)
// is not a telemetry endpoint and is not checked against this.
var telemetryHostAllowlist = map[string]bool{
	"github.com":                true,
	"raw.githubusercontent.com": true,
	"ghcr.io":                   true,
	"help.synology.com":         true,
	"www.truenas.com":           true,
	"forums.unraid.net":         true,
	"docs.docker.com":           true,
	"www.openmediavault.org":    true,
	"pve.proxmox.com":           true,
	"www.ugreen.com":            true,
	"spdx.org":                  true,
	"www.gnu.org":               true,
	"opensource.org":            true,
}

// CheckNoSelfUpdate holds one packaged file to §45.4: the package may not
// replace reviewed code without going back through review.
//
// Both halves of §45.4 are covered, and they look nothing alike. The
// declarative half is an orchestrator being asked to pull a newer image
// on its own (watchtower, podman auto-update, pull_policy: always, an
// Unraid auto-update element). The imperative half is a lifecycle script
// fetching an executable at install or upgrade time, which is how a DSM
// package would do it, and which no amount of reading compose keys would
// ever find.
func CheckNoSelfUpdate(path, text string) []Violation {
	var out []Violation
	add := func(detail string) { out = append(out, Violation{path, RuleSelfUpdate, detail}) }

	if watchtowerRe.MatchString(text) {
		add("opts the container in to Watchtower, which replaces the running image with a newer one without any review of what changed")
	}
	if podmanAutoRe.MatchString(text) {
		add("sets `io.containers.autoupdate`, which lets podman replace the reviewed image on its own")
	}
	if pullAlwaysRe.MatchString(text) {
		add("pulls the image on every start rather than the reviewed bytes, so the tag decides what runs, not the review")
	}
	if autoUpdateRe.MatchString(text) {
		add("turns a self-update or auto-update setting on; §45.4 requires a new package rather than a replaced binary")
	}
	if unraidAutoRe.MatchString(text) {
		add("declares an Unraid auto-update element as true, which updates the container outside Community Applications' own review")
	}
	if isScript(path) {
		for _, m := range fetchExecuteRe.FindAllStringSubmatch(text, -1) {
			add(fmt.Sprintf("a packaged lifecycle script runs %s, which downloads code the store never reviewed", backquote(strings.TrimSpace(m[1]))))
		}
	}

	sortViolations(out)
	return out
}

// isScript reports whether a packaged file is executed rather than read.
// Extension is not enough: every DSM lifecycle script (preinst, postinst,
// start-stop-status) has no extension at all, which is precisely where a
// download-and-run would hide.
func isScript(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".sh") || strings.HasSuffix(base, ".bash") {
		return true
	}
	switch base {
	case "preinst", "postinst", "preuninst", "postuninst", "preupgrade", "postupgrade", "start-stop-status", "installer":
		return true
	}
	return false
}

// CheckNoFloatingTag holds every image reference in a packaged file to §8:
// a provider package deploys exact bytes.
//
// "No `latest`" is the spelling §73 uses and it is the weaker half of the
// rule. An image reference with no tag at all resolves to `latest` and
// contains none of the letters in it, and `:${VERSION:-latest}` floats
// through a default nobody reads. All three are the same defect, so this
// parses the reference rather than searching for a word.
func CheckNoFloatingTag(path, text string) []Violation {
	var out []Violation
	seen := map[string]bool{}
	add := func(ref, detail string) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, Violation{path, RuleFloatingTag, detail})
	}

	refs := map[string]bool{}
	for _, re := range []*regexp.Regexp{composeImageRe, unraidImageRe, yamlRefRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if r := strings.TrimSpace(m[1]); r != "" {
				refs[r] = true
			}
		}
	}

	names := make([]string, 0, len(refs))
	for r := range refs {
		names = append(names, r)
	}
	sort.Strings(names)

	for _, ref := range names {
		// A rendered template expression is not a reference this file
		// pins; whatever fills it in is, and that value is declared in
		// the values file next to it, which is scanned as its own file.
		if strings.Contains(ref, "{{") {
			continue
		}
		tag, kind := ImageTag(ref)
		switch kind {
		case tagAbsent:
			add(ref, fmt.Sprintf("deploys %s with no tag and no digest, which Docker resolves to `latest`: the bytes that run are whatever the registry holds that day", backquote(ref)))
		case tagLatest:
			add(ref, fmt.Sprintf("deploys %s, a floating tag whose contents change under a running deployment", backquote(ref)))
		case tagFloatingDefault:
			add(ref, fmt.Sprintf("deploys %s, whose variable default is %s, so an operator who sets nothing gets a floating tag", backquote(ref), backquote(tag)))
		}
	}

	sortViolations(out)
	return out
}

type tagKind int

const (
	tagPinned tagKind = iota
	tagAbsent
	tagLatest
	tagFloatingDefault
	tagVariable
)

// ImageTag splits an OCI reference into its tag and says what kind of tag
// it is. Exported because the drift gate needs the same answer about the
// canonical reference that the hard rule needs about a provider's.
func ImageTag(ref string) (string, tagKind) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[i+1:], tagPinned
	}
	// The last colon is only a tag separator when nothing after it looks
	// like a registry port, which is what the "/" test decides.
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return "", tagAbsent
	}
	tag := ref[i+1:]
	switch {
	case strings.EqualFold(tag, "latest"):
		return tag, tagLatest
	case strings.HasPrefix(tag, "${"):
		for _, v := range VarRefs(tag) {
			if v.HasDefault && strings.EqualFold(v.Default, "latest") {
				return v.Default, tagFloatingDefault
			}
		}
		return tag, tagVariable
	case tag == "":
		return "", tagAbsent
	}
	return tag, tagPinned
}

// CheckNoPrivilegedMode holds a packaged file to the negative half of the
// runtime profile, in every spelling the four metadata formats have for
// it: a compose key, an Unraid element, a docker run flag, and the
// several ways of handing back what a dropped capability set removed.
//
// The structural half of the same rule is CheckForbiddenPrivileges, over
// a parsed Service. Both exist on purpose. This one reads the bytes a
// store receives, so it also covers a file no parser in this package
// models (a DSM privilege file, an installer script); that one understands
// the semantics, so it catches a privilege granted through a key this
// regular expression has never heard of.
func CheckNoPrivilegedMode(path, text string) []Violation {
	var out []Violation
	add := func(detail string) { out = append(out, Violation{path, RulePrivileged, detail}) }

	if privilegedYAMLRe.MatchString(text) {
		add("sets `privileged: true`, which gives the container the host's full capability set and undoes every other hardening key in the same file")
	}
	if privilegedXMLRe.MatchString(text) {
		add("declares `<Privileged>true</Privileged>`, which Community Applications shows to the operator as a full-privilege container")
	}
	if privilegedFlagRe.MatchString(text) {
		add("passes `--privileged`")
	}
	if networkModeHostRe.MatchString(text) {
		add("shares a host namespace (`network_mode`, `pid`, `ipc` or `uts` set to `host`), which puts the container's listeners and process view on the host itself")
	}
	if capAddRe.MatchString(text) {
		add("declares `cap_add`, which adds back a capability the same profile dropped with `cap_drop: ALL`")
	}
	if unconfinedRe.MatchString(text) {
		add("runs with seccomp or AppArmor unconfined, which removes the default syscall and file mediation the store's own review assumes")
	}
	if noNewPrivFalseRe.MatchString(text) {
		add("sets `no-new-privileges:false`, which re-enables setuid escalation inside the container")
	}

	sortViolations(out)
	return out
}

// CheckNoMandatoryTelemetry holds a packaged file to §45.5 and §75: the
// product operates locally except for the backup sources an administrator
// configured.
//
// Two shapes. A named telemetry variable set to something truthy is the
// obvious one. The one that actually matters is an outbound URL nobody
// asked for: a listing, an env file or a compose profile can point at a
// collector with no variable named anything suggestive at all. So every
// URL is extracted and held to an allowlist of the project's own hosts,
// and anything that is not a public DNS name (a container name, a .local
// host, localhost, a private address) is not an endpoint and is skipped.
func CheckNoMandatoryTelemetry(path, text string) []Violation {
	var out []Violation
	add := func(detail string) { out = append(out, Violation{path, RuleMandatoryTelemetry, detail}) }

	for _, m := range telemetryVarRe.FindAllStringSubmatch(text, -1) {
		if isTruthy(m[2]) {
			add(fmt.Sprintf("sets %s to %s, so the deployment reports out unless an operator finds and changes it; §45.5 requires no required telemetry at all", backquote(m[1]), backquote(m[2])))
		}
	}
	for _, m := range sentryRe.FindAllStringSubmatch(text, -1) {
		if strings.TrimSpace(m[2]) != "" {
			add(fmt.Sprintf("ships a populated %s, which is a crash-reporting endpoint wired in by default", backquote(m[1])))
		}
	}
	seen := map[string]bool{}
	for _, raw := range urlRe.FindAllString(text, -1) {
		u, err := url.Parse(strings.TrimRight(raw, ".,;"))
		if err != nil {
			continue
		}
		host := u.Hostname()
		if !isPublicHost(host) || telemetryHostAllowlist[strings.ToLower(host)] || seen[host] {
			continue
		}
		seen[host] = true
		add(fmt.Sprintf("points at %s, which is neither the project's own source, tracker, registry or store documentation, nor a local address; an outbound host in a shipped package is a telemetry endpoint until it is shown otherwise", backquote(host)))
	}

	sortViolations(out)
	return out
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Trim(v, `"'`))) {
	case "true", "yes", "on", "1", "enabled":
		return true
	}
	return false
}

// isPublicHost reports whether a host is a name that resolves on the
// public internet. A bare container name has no dot, `.local` and
// `.internal` are link-local or site-local by definition, and an address
// literal that starts with a digit is not a name at all. None of those
// can be a collector an operator did not configure.
func isPublicHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" || h == "localhost" || !strings.Contains(h, ".") {
		return false
	}
	if _, err := strconv.Atoi(strings.Split(h, ".")[0]); err == nil {
		return false
	}
	for _, suffix := range []string{".local", ".internal", ".lan", ".home", ".arpa", ".invalid", ".test", ".example"} {
		if strings.HasSuffix(h, suffix) {
			return false
		}
	}
	return true
}

// ScanArtifact runs one hard rule over every file a provider's package
// actually carries, and reports how many files it read.
//
// The file count is returned rather than discarded because "the scan
// found nothing" and "the scan read nothing" produce identical violation
// slices, and only one of them is a pass. Every caller here treats a zero
// as a failure, the same call scan() in matrix_test.go already makes.
func ScanArtifact(roots []string, rule func(path, text string) []Violation) ([]Violation, int, error) {
	var all []Violation
	files := 0
	for _, root := range roots {
		abs := Path(root)
		err := filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, readErr := os.ReadFile(p)
			if readErr != nil {
				return readErr
			}
			files++
			rel, relErr := filepath.Rel(Path("."), p)
			if relErr != nil {
				rel = p
			}
			all = append(all, rule(rel, string(data))...)
			return nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", root, err)
		}
	}
	sortViolations(all)
	return all, files, nil
}
