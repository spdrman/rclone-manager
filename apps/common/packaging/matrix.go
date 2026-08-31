package packaging

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// This file is the cross-provider conformance matrix's data model
// (issue #86 / WP4.5, docs/EPIC-B-multi-nas.md §63A and the §72 Phase 4
// TDD Gate). The checks themselves live in matrix_test.go; everything
// here is the vocabulary they report in, the declaration file they are
// held against, and the renderer that turns a finished run into
// docs/conformance/phase-4-matrix.md.
//
// The whole point of the split is §63A's own instruction:
//
//	The conformance suite SHALL distinguish
//	  SUPPORTED / UNSUPPORTED / NOT_APPLICABLE
//	rather than silently skipping missing provider features.
//
// A Go test that stops at the first failure cannot do that: it produces
// one verdict for a run, and a capability nobody wrote a check for is
// indistinguishable from a capability that passed. So every provider
// declares an outcome for every capability up front, in conformance.json,
// and the runner's job is to agree or disagree with each declaration
// individually.

//go:embed conformance.json
var conformanceJSON []byte

// Declaration is what conformance.json claims about one provider's one
// capability, before any check runs.
type Declaration string

const (
	// DeclSupported: this provider is expected to satisfy the capability
	// here, in this repository or through a prewritten operator
	// procedure. Anything less is a failure.
	DeclSupported Declaration = "supported"
	// DeclUnsupported: the provider genuinely does not have this
	// capability at its §4A support tier. Requires a reason.
	DeclUnsupported Declaration = "unsupported"
	// DeclNotApplicable: the capability does not apply to this
	// provider's shape at all, usually because the platform expresses
	// the same guarantee somewhere else. Requires a reason, and normally
	// a verifiedBy pointing at wherever that is.
	DeclNotApplicable Declaration = "not-applicable"
	// DeclBlocked: the check is implemented and correct, and cannot
	// reach a verdict today for a reason tracked elsewhere. Requires a
	// blocker issue number. This is deliberately NOT a pass and NOT a
	// fail: reporting it as either would be a lie in a different
	// direction.
	DeclBlocked Declaration = "blocked"
)

// Outcome is what the runner concluded about one cell.
type Outcome string

const (
	OutcomePass          Outcome = "PASS"
	OutcomeFail          Outcome = "FAIL"
	OutcomeUnsupported   Outcome = "UNSUPPORTED"
	OutcomeNotApplicable Outcome = "NOT_APPLICABLE"
	OutcomeBlocked       Outcome = "BLOCKED"
	// OutcomePendingOperator: the capability is supported and the
	// prewritten acceptance procedure covering it exists, but deciding
	// it needs the real platform (§68). The automated half passed; the
	// hardware half has not run.
	OutcomePendingOperator Outcome = "PENDING_OPERATOR"
)

// Mode says how a capability can be decided.
type Mode string

const (
	// ModeRepo: decidable from the repository alone, on any laptop.
	ModeRepo Mode = "repo"
	// ModeOperator: decidable only on the real platform. What is
	// automatable is that the §68 procedure exists and covers it.
	ModeOperator Mode = "operator"
)

// Capability is one row of the matrix.
type Capability struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Spec is the section of docs/EPIC-B-multi-nas.md this comes from.
	Spec string `json:"spec"`
	Mode Mode   `json:"mode"`
}

// Cell is one provider's declaration about one capability.
type Cell struct {
	Declared Declaration `json:"declared"`
	// Reason is mandatory for everything except a plain "supported".
	Reason string `json:"reason"`
	// Blocker is mandatory for "blocked": the issue tracking why.
	Blocker string `json:"blocker"`
	// ExpectedDetail is mandatory for "blocked": a substring the check's
	// own failure message must contain for the blocker to be accepted as
	// the explanation. Without it, "blocked" silences ANY failure of that
	// check for as long as the declaration stands, which is the one
	// direction the staleness guard does not cover.
	ExpectedDetail string `json:"expectedDetail"`
	// VerifiedBy points at whatever proves the guarantee instead, when
	// this provider expresses it somewhere other than here. Either a
	// path, or "path:TestName" to name the test rather than the file it
	// lives in. Naming the test is the stronger form: a file keeps
	// existing long after the assertion inside it is deleted.
	VerifiedBy string `json:"verifiedBy"`
}

// Metadata says where a provider's packaging metadata lives and what
// shape it is, so one runner can read four different formats.
type Metadata struct {
	// Kind is "compose", "unraid-template", "spk" or "none".
	Kind string `json:"kind"`
	// Root is relative to the repository root.
	Root      string   `json:"root"`
	Compose   string   `json:"compose"`
	Env       string   `json:"env"`
	Templates []string `json:"templates"`
	// Files must all exist for package-metadata to pass.
	Files []string `json:"files"`
	// StoreArtifacts are the checked-in files that make this provider
	// installable through a platform's own application store or
	// catalog. Empty for a Tier C deployment profile, which by
	// definition has no store to appear in.
	StoreArtifacts []string `json:"storeArtifacts"`
	// BinaryArtifacts maps a canonical binary path
	// ("/backup-manager-web") to a checked-in file in this provider's
	// package that is supposed to BE those bytes. Empty for every
	// provider that consumes the OCI image by reference, which is all of
	// them today, and that is the point: core-binary-hash-parity cannot
	// go green without a file here to hash.
	BinaryArtifacts map[string]string `json:"binaryArtifacts"`
	// ArchitectureClaim is this provider's OWN statement about which
	// architectures its package supports. Empty for a provider that
	// makes no claim of its own, which is what stops the repository-wide
	// release manifest standing in for seven per-provider answers.
	ArchitectureClaim ArchitectureClaim `json:"architectureClaim"`
}

// ArchitectureClaim is where a provider states which architectures its
// own package supports, and what it states.
type ArchitectureClaim struct {
	// Source is the file making the claim, relative to the repository
	// root.
	Source string `json:"source"`
	// Architectures are the GOARCH values it claims. Every one of them
	// must appear verbatim in Source, so a claim recorded here that the
	// package does not actually make is a failure rather than a
	// decoration.
	Architectures []string `json:"architectures"`
}

// Epic names the EPIC whose gate consumes a provider's column: the one
// that has to be satisfied before that EPIC can close. It is not a
// statement about who wrote the checks or where the column is reported.
// Every column is decided by the same checks, on the same terms, whoever
// owns it; this field only says whose gate the answer counts towards.
//
// It exists because those two things came apart. UGOS packaging is EPIC
// D's #83 (D1.2) since the UGOS split, so a UGOS cell must not be able to
// hold EPIC B's Phase 4 open, and #86 and #81 both now name six Phase 4
// targets rather than seven. Dropping the column instead would have
// thrown away a check that works: the two-directional store-packaging
// rule is what caught UGOS claiming app-store packaging with no UPK
// behind it, and a column that is simply absent reads as a clean one.
//
// Deliberately a free string rather than a checked set of epic names.
// #170 asks for a matrix that accepts a target registered by another
// EPIC without being modified, and an enum of the epics that exist today
// is exactly the modification it asks to avoid.
type Epic string

// PhaseFourEpic is the EPIC whose gate the §72 Phase 4 Exit Gate is.
// A column declared to any other epic is checked here, reported here,
// and gated there.
const PhaseFourEpic Epic = "B"

// Provider is one column of the matrix.
type Provider struct {
	DisplayName string `json:"displayName"`
	// Tier is the §4A support tier.
	Tier string `json:"tier"`
	// Epic is the EPIC whose gate consumes this column. Every column is
	// checked; only PhaseFourEpic's columns decide Phase 4.
	Epic        Epic     `json:"epic"`
	WorkPackage string   `json:"workPackage"`
	Metadata    Metadata `json:"metadata"`
	// ScanRoots are the directories the structural scanners run over,
	// relative to the repository root.
	ScanRoots []string `json:"scanRoots"`
	// Acceptance is the §68 procedure, relative to the repository root,
	// or empty when the provider has none.
	Acceptance string          `json:"acceptance"`
	Cells      map[string]Cell `json:"cells"`
}

// Conformance is conformance.json.
type Conformance struct {
	Capabilities []Capability        `json:"capabilities"`
	Providers    map[string]Provider `json:"providers"`
}

// LoadConformance parses the embedded conformance.json.
func LoadConformance() (Conformance, error) {
	var c Conformance
	if err := json.Unmarshal(conformanceJSON, &c); err != nil {
		return Conformance{}, fmt.Errorf("packaging: parse conformance.json: %w", err)
	}
	return c, nil
}

// MustLoadConformance is LoadConformance for callers that cannot proceed
// without it.
func MustLoadConformance() Conformance {
	c, err := LoadConformance()
	if err != nil {
		panic(err)
	}
	return c
}

// ProviderIDs returns the declared providers in a stable order: by §4A
// tier first (A before B before C), then alphabetically, so the rendered
// matrix reads down the support tiers rather than in map order.
func (c Conformance) ProviderIDs() []string {
	ids := make([]string, 0, len(c.Providers))
	for id := range c.Providers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := c.Providers[ids[i]], c.Providers[ids[j]]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		return ids[i] < ids[j]
	})
	return ids
}

// ProviderIDsFor returns the providers whose gate is epic's, in the same
// order as ProviderIDs. This is the set an epic's gate is computed over,
// and it is a subset rather than the whole file on purpose: a column
// another epic owns is reported alongside these and decides nothing here.
func (c Conformance) ProviderIDsFor(epic Epic) []string {
	var out []string
	for _, id := range c.ProviderIDs() {
		if c.Providers[id].Epic == epic {
			out = append(out, id)
		}
	}
	return out
}

// DisplayNames maps provider ids to their display names, in the order
// given.
func (c Conformance) DisplayNames(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.Providers[id].DisplayName)
	}
	return out
}

// CapabilityTitle returns a capability's human title, or its id when the
// matrix has no such capability.
func (c Conformance) CapabilityTitle(id string) string {
	for _, cap := range c.Capabilities {
		if cap.ID == id {
			return cap.Title
		}
	}
	return id
}

// CapabilityIDs returns the capability ids in declaration order.
func (c Conformance) CapabilityIDs() []string {
	out := make([]string, 0, len(c.Capabilities))
	for _, cap := range c.Capabilities {
		out = append(out, cap.ID)
	}
	return out
}

// Path resolves a repository-root-relative path from this package's own
// directory, which is where `go test` runs.
func Path(rel string) string { return filepath.Join(RepoRoot, rel) }

// ---------------------------------------------------------------------
// Results
// ---------------------------------------------------------------------

// Result is one finished cell.
type Result struct {
	Provider   string
	Capability string
	Outcome    Outcome
	// Detail is what the check actually observed, pass or fail. It is
	// never empty: a cell that says only "UNSUPPORTED" is the silent
	// skip §63A rules out.
	Detail string
}

// Matrix is a whole run.
type Matrix struct {
	Conformance Conformance
	Results     map[string]map[string]Result // provider -> capability -> result
}

// NewMatrix returns an empty matrix for c.
func NewMatrix(c Conformance) *Matrix {
	m := &Matrix{Conformance: c, Results: map[string]map[string]Result{}}
	for id := range c.Providers {
		m.Results[id] = map[string]Result{}
	}
	return m
}

// Record stores one cell.
func (m *Matrix) Record(r Result) { m.Results[r.Provider][r.Capability] = r }

// Count returns how many cells hold the given outcome.
func (m *Matrix) Count(o Outcome) int {
	n := 0
	for _, byCap := range m.Results {
		for _, r := range byCap {
			if r.Outcome == o {
				n++
			}
		}
	}
	return n
}

// ---------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------

// Verdict is one EPIC's gate over a finished matrix: the columns it was
// computed over, the columns it deliberately was not, and every cell that
// stops it being met.
//
// The exclusion is the whole point of the type. Before this existed the
// gate was "did any cell anywhere fail", which quietly made EPIC B's
// Phase 4 answerable to a column EPIC B does not own and cannot deliver:
// UGOS packaging is #83, in EPIC D, on hardware nobody in this repository
// has. Excluding a column from a verdict is not the same as excluding it
// from the run, and it must not become that: an informational column is
// checked, resolved and reported exactly like every other one.
type Verdict struct {
	Epic Epic
	// Providers are the columns this epic claims, in report order.
	Providers []string
	// Informational are the columns another epic claims. They are
	// reported and checked, and they never appear in Failures or Blocked
	// however their cells resolve.
	Informational []string
	// Failures are the claimed cells that failed, plus any claimed cell
	// the run never recorded at all. An unrun target must never read as a
	// passing one (#170).
	Failures []Result
	// Blocked are the claimed cells that could not be decided. Not a
	// pass, so the gate is not met while one stands, and not a failure
	// either.
	Blocked []Result
}

// Met reports whether the gate holds: every claimed cell decided, and
// none of them failed.
func (v Verdict) Met() bool { return len(v.Failures) == 0 && len(v.Blocked) == 0 }

// Verdict computes epic's gate over this matrix.
func (m *Matrix) Verdict(epic Epic) Verdict {
	c := m.Conformance
	v := Verdict{Epic: epic}
	for _, pid := range c.ProviderIDs() {
		if c.Providers[pid].Epic != epic {
			v.Informational = append(v.Informational, pid)
			continue
		}
		v.Providers = append(v.Providers, pid)
		for _, cap := range c.Capabilities {
			r, ok := m.Results[pid][cap.ID]
			if !ok {
				v.Failures = append(v.Failures, Result{
					Provider:   pid,
					Capability: cap.ID,
					Outcome:    OutcomeFail,
					Detail:     "no result recorded for a target this EPIC claims, so the run never decided it",
				})
				continue
			}
			switch r.Outcome {
			case OutcomeFail:
				v.Failures = append(v.Failures, r)
			case OutcomeBlocked:
				v.Blocked = append(v.Blocked, r)
			}
		}
	}
	return v
}

// Blockers returns the distinct blocker issues one provider's blocked
// cells name, sorted.
func (m *Matrix) Blockers(provider string) []string {
	seen := map[string]bool{}
	var out []string
	for _, cap := range m.Conformance.Capabilities {
		if m.Results[provider][cap.ID].Outcome != OutcomeBlocked {
			continue
		}
		b := m.Conformance.Providers[provider].Cells[cap.ID].Blocker
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// CountFor is Count restricted to one provider.
func (m *Matrix) CountFor(provider string, o Outcome) int {
	n := 0
	for _, r := range m.Results[provider] {
		if r.Outcome == o {
			n++
		}
	}
	return n
}

// gateLabel says whose gate a column counts towards, for a reader of the
// report rather than for the runner.
func gateLabel(e Epic) string {
	if e == PhaseFourEpic {
		return "EPIC " + string(e) + " (Phase 4)"
	}
	return "EPIC " + string(e) + " (reported here, gated there)"
}

// gatedBy is gateLabel in the short form a heading can carry.
func gatedBy(e Epic) string {
	if e == PhaseFourEpic {
		return "gated by EPIC " + string(e) + "'s Phase 4"
	}
	return "reported here, gated by EPIC " + string(e)
}

// columnLabel is a provider's heading in the report, marked with its epic
// when that is not Phase 4's, so a reader of the widest table can see at a
// glance which column is not part of the gate.
func columnLabel(pr Provider) string {
	if pr.Epic == PhaseFourEpic {
		return pr.DisplayName
	}
	return fmt.Sprintf("%s (EPIC %s)", pr.DisplayName, pr.Epic)
}

var outcomeAbbrev = map[Outcome]string{
	OutcomePass:            "PASS",
	OutcomeFail:            "FAIL",
	OutcomeUnsupported:     "UNSUP",
	OutcomeNotApplicable:   "N/A",
	OutcomeBlocked:         "BLOCKED",
	OutcomePendingOperator: "OPERATOR",
}

// Render turns a finished matrix into the body of
// docs/conformance/phase-4-matrix.md. It is deterministic: same tree,
// same bytes, which is what lets the checked-in report be compared
// against a fresh run instead of being hand-maintained prose that drifts.
func (m *Matrix) Render() string {
	c := m.Conformance
	providers := c.ProviderIDs()

	var b strings.Builder

	b.WriteString("### Support tiers (§4A)\n\n")
	b.WriteString("| Provider | Tier | Gated by | Work package | Acceptance procedure |\n|---|---|---|---|---|\n")
	for _, p := range providers {
		pr := c.Providers[p]
		acc := pr.Acceptance
		if acc == "" {
			acc = "none (automated instead)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | `%s` |\n", pr.DisplayName, pr.Tier, gateLabel(pr.Epic), pr.WorkPackage, acc)
	}

	b.WriteString("\n### Per-capability results\n\n")
	b.WriteString("| Capability |")
	for _, p := range providers {
		fmt.Fprintf(&b, " %s |", columnLabel(c.Providers[p]))
	}
	b.WriteString("\n|---|")
	for range providers {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, cap := range c.Capabilities {
		fmt.Fprintf(&b, "| %s |", cap.Title)
		for _, p := range providers {
			fmt.Fprintf(&b, " %s |", outcomeAbbrev[m.Results[p][cap.ID].Outcome])
		}
		b.WriteString("\n")
	}

	b.WriteString("\n### Totals\n\n")
	b.WriteString("| Outcome | Cells |\n|---|---|\n")
	for _, o := range []Outcome{OutcomePass, OutcomePendingOperator, OutcomeUnsupported, OutcomeNotApplicable, OutcomeBlocked, OutcomeFail} {
		fmt.Fprintf(&b, "| %s | %d |\n", o, m.Count(o))
	}

	b.WriteString("\n### Phase 4 Exit Gate\n\n")
	v := m.Verdict(PhaseFourEpic)
	fmt.Fprintf(&b, "Computed over the %d providers EPIC B claims, and over nothing else: %s.\n\n",
		len(v.Providers), strings.Join(c.DisplayNames(v.Providers), ", "))
	if v.Met() {
		b.WriteString("**Met.** Every cell of every one of those columns was decided, and none of them failed.\n")
	} else {
		fmt.Fprintf(&b, "**Not met.** %d cell(s) failed and %d could not be decided, every one of them in a column EPIC B claims:\n\n", len(v.Failures), len(v.Blocked))
		b.WriteString("| Provider | Capability | Outcome | Tracked by |\n|---|---|---|---|\n")
		for _, r := range append(append([]Result{}, v.Failures...), v.Blocked...) {
			tracked := c.Providers[r.Provider].Cells[r.Capability].Blocker
			if tracked == "" {
				tracked = "not tracked anywhere yet"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.Providers[r.Provider].DisplayName, c.CapabilityTitle(r.Capability), outcomeAbbrev[r.Outcome], tracked)
		}
	}
	for _, pid := range v.Informational {
		pr := c.Providers[pid]
		blockers := m.Blockers(pid)
		tracked := "nothing"
		if len(blockers) > 0 {
			tracked = strings.Join(blockers, " and ")
		}
		fmt.Fprintf(&b, "\n**%s is EPIC %s's column** (work package %s).\nAll %d of its cells are decided by the same runner, on the same terms as every\nother column, and reported in full below; %d are blocked today, on %s.\nNone of them is in the verdict above. A capability EPIC %s owns cannot hold\nEPIC %s's Phase 4 open, and an EPIC %s column that goes green cannot close it.\n",
			pr.DisplayName, pr.Epic, pr.WorkPackage, len(c.Capabilities), m.CountFor(pid, OutcomeBlocked), tracked, pr.Epic, PhaseFourEpic, pr.Epic)
	}

	b.WriteString("\n### Every cell that is not a plain PASS\n\n")
	b.WriteString("Section 63A's requirement in full: an unsupported capability is reported, with a\nreason, rather than skipped. Every row below is a cell this run did not pass, and\nwhy.\n")
	for _, p := range providers {
		pr := c.Providers[p]
		var rows []string
		for _, cap := range c.Capabilities {
			r := m.Results[p][cap.ID]
			if r.Outcome == OutcomePass {
				continue
			}
			reason := pr.Cells[cap.ID].Reason
			if pr.Cells[cap.ID].Blocker != "" {
				reason = pr.Cells[cap.ID].Blocker + " — " + reason
			}
			if reason == "" {
				reason = r.Detail
			}
			rows = append(rows, fmt.Sprintf("| %s | %s | %s |\n", cap.Title, outcomeAbbrev[r.Outcome], reason))
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n#### %s (Tier %s, %s)\n\n", pr.DisplayName, pr.Tier, gatedBy(pr.Epic))
		b.WriteString("| Capability | Outcome | Why |\n|---|---|---|\n")
		for _, r := range rows {
			b.WriteString(r)
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------
// The generated region of docs/conformance/phase-4-matrix.md
// ---------------------------------------------------------------------

const (
	matrixBeginMarker = "<!-- BEGIN GENERATED MATRIX -->"
	matrixEndMarker   = "<!-- END GENERATED MATRIX -->"
)

// MatrixReportPath is the checked-in report, relative to the repository
// root.
const MatrixReportPath = "docs/conformance/phase-4-matrix.md"

// SpliceMatrixReport replaces the generated region of doc with body,
// leaving the hand-written framing above and below it alone.
func SpliceMatrixReport(doc, body string) (string, error) {
	start := strings.Index(doc, matrixBeginMarker)
	end := strings.Index(doc, matrixEndMarker)
	if start < 0 || end < 0 || end < start {
		return "", fmt.Errorf("packaging: %s is missing its %s / %s markers", MatrixReportPath, matrixBeginMarker, matrixEndMarker)
	}
	return doc[:start+len(matrixBeginMarker)] + "\n\n" + body + "\n" + doc[end:], nil
}

// ---------------------------------------------------------------------
// Shared helpers the checks need
// ---------------------------------------------------------------------

// bridgeCapabilityRe pulls one flag out of a bridge's capabilities({...})
// call, e.g. `nativeAuth: true`.
func bridgeCapabilityRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `:\s*true\b`)
}

// BridgeDeclaresCapability reports whether apps/<provider>/frontend/
// platform.ts opts in to key. The bridge's own default
// (ui/shared NO_CAPABILITIES) is false for everything, so absence is a
// deliberate "no" rather than an oversight.
func BridgeDeclaresCapability(bridgePath, key string) (bool, error) {
	data, err := os.ReadFile(bridgePath)
	if err != nil {
		return false, err
	}
	return bridgeCapabilityRe(key).Match(data), nil
}

// procedureLineRe matches the lines of an acceptance procedure that
// represent a step an operator actually performs: a heading, or a
// checklist box. Matching those rather than the whole document is what
// stops "the word update appears in a footnote" from counting as
// coverage.
var procedureLineRe = regexp.MustCompile(`(?m)^(#{2,3} .*|- \[ \] .*)$`)

// ProcedureSteps returns the heading and checklist lines of an acceptance
// procedure.
func ProcedureSteps(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.Join(procedureLineRe.FindAllString(string(data), -1), "\n"), nil
}

// procedureHeadingRe matches a markdown heading and captures its level.
var procedureHeadingRe = regexp.MustCompile(`(?m)^(#{2,6}) `)

// ProcedureSection returns the body of the first section of an acceptance
// procedure whose heading matches want, up to the next heading at the
// same or a higher level. Unlike ProcedureSteps it keeps the commands:
// deciding whether a step could actually detect a failure means reading
// what it tells the operator to run, not just what it calls itself.
func ProcedureSection(path string, want *regexp.Regexp) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	level := 0
	var body []string
	for _, line := range lines {
		m := procedureHeadingRe.FindStringSubmatch(line + "\n")
		if level == 0 {
			if m != nil && want.MatchString(line) {
				level = len(m[1])
			}
			continue
		}
		if m != nil && len(m[1]) <= level {
			break
		}
		body = append(body, line)
	}
	if level == 0 {
		return "", nil
	}
	return strings.Join(body, "\n"), nil
}

// ImportsProviderRe matches a quoted module path that reaches into
// apps/<provider>/. Quoting is the point: core/service/scheduler_test.go
// mentions "apps/generic's serve command" in a comment, which is prose
// about the architecture, not a dependency on it.
func ImportsProviderRe(provider string) *regexp.Regexp {
	return regexp.MustCompile(`["'][^"']*apps/` + regexp.QuoteMeta(provider) + `/`)
}

// ---------------------------------------------------------------------
// The release manifest
// ---------------------------------------------------------------------

// ReleaseManifest is the part of container/release-manifest.json the
// conformance checks read. Parsed here rather than inside a check so a
// test can feed it a manifest with one hash corrupted and watch the
// verdict change, which is the acceptance bar a parity claim has to
// clear.
type ReleaseManifest struct {
	Commit        string                `json:"commit"`
	Architectures []ReleaseArchitecture `json:"architectures"`
}

// ReleaseArchitecture is one architecture's recorded build.
type ReleaseArchitecture struct {
	Architecture string `json:"architecture"`
	// BinarySHA256 is keyed by the binary's path WITHOUT a leading
	// slash, which is how the manifest writes it.
	BinarySHA256 map[string]string `json:"binary_sha256"`
	// RegistryDigest is the digest ghcr.io assigned this architecture's
	// image on push, and it is a pointer so that "not pushed yet" and
	// "pushed, digest not recorded" are different values rather than the
	// same empty string.
	//
	// It is null throughout today, which is the honest reading of
	// canonical.json's image.published false: the registry is settled
	// (ghcr.io/spdrman/backup-manager) and nothing has been pushed to it.
	// The manifest's sibling field local_image_id_sha256 is deliberately
	// NOT modelled here, because it is not a digest and nothing outside
	// the machine that built it can resolve it. Filling this in from a
	// real push is #88's work.
	RegistryDigest *string `json:"registry_digest"`
}

// ParseReleaseManifest reads a release manifest.
func ParseReleaseManifest(data []byte) (ReleaseManifest, error) {
	var m ReleaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return ReleaseManifest{}, err
	}
	return m, nil
}

// ReadReleaseManifest reads container/release-manifest.json.
func ReadReleaseManifest() (ReleaseManifest, error) {
	data, err := os.ReadFile(Path(filepath.Join("container", "release-manifest.json")))
	if err != nil {
		return ReleaseManifest{}, err
	}
	return ParseReleaseManifest(data)
}

// ArchitectureSet returns the architectures the manifest records, sorted.
func (m ReleaseManifest) ArchitectureSet() []string {
	out := make([]string, 0, len(m.Architectures))
	for _, a := range m.Architectures {
		out = append(out, a.Architecture)
	}
	sort.Strings(out)
	return out
}

// RecordsEveryBinary reports whether every canonical binary has a
// recorded SHA-256 on every architecture, and says which one does not
// when the answer is no.
func (m ReleaseManifest) RecordsEveryBinary(binaries []string) (bool, string) {
	if len(m.Architectures) == 0 {
		return false, "the release manifest records no architecture at all"
	}
	if len(binaries) == 0 {
		return false, "no canonical binary list to check the manifest against"
	}
	for _, a := range m.Architectures {
		for _, binary := range binaries {
			if a.BinarySHA256[strings.TrimPrefix(binary, "/")] == "" {
				return false, fmt.Sprintf("no SHA-256 recorded for %s on %s", binary, a.Architecture)
			}
		}
	}
	return true, fmt.Sprintf("every binary hashed on %v", m.ArchitectureSet())
}

// HashesFor returns architecture -> recorded SHA-256 for one binary.
func (m ReleaseManifest) HashesFor(binary string) map[string]string {
	out := map[string]string{}
	for _, a := range m.Architectures {
		if h := a.BinarySHA256[strings.TrimPrefix(binary, "/")]; h != "" {
			out[a.Architecture] = h
		}
	}
	return out
}

// CommitReachableFrom reports whether commit is an ancestor of ref in
// the repository rooted at repoDir.
//
// The two failure modes are kept apart on purpose, and the separation is
// the whole reason this is a function rather than three lines inlined at
// each call site. Exit status 1 is git answering "no, that commit is not
// in this history", which is a fact about the manifest. Anything else is
// git failing to answer at all (an unknown object, a repository that is
// not there, git missing from PATH), which is a fact about the check.
// Collapsing the second into the first is how a broken check gets filed
// under a known blocker and stops being looked at.
//
// So: (true, nil) means reachable, (false, nil) means git said no, and a
// non-nil error means nobody decided anything.
func CommitReachableFrom(repoDir, commit, ref string) (bool, error) {
	cmd := exec.Command("git", "-C", repoDir, "merge-base", "--is-ancestor", commit, ref)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// SHA256File returns the lowercase hex SHA-256 of a file.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------
// verifiedBy
// ---------------------------------------------------------------------

// ParseVerifiedBy splits a verifiedBy value into its path and, when the
// declaration names one, the test function inside it. "a/b.go" yields
// ("a/b.go", ""); "a/b_test.go:TestThing" yields ("a/b_test.go",
// "TestThing").
func ParseVerifiedBy(v string) (path, fn string) {
	i := strings.LastIndex(v, ":")
	if i < 0 {
		return v, ""
	}
	return v[:i], v[i+1:]
}

// VerifiedByReachable reports whether a verifiedBy value points at
// something that is actually there. Naming a function is the whole point
// of the "path:Name" form: os.Stat on a file is satisfied by a file that
// has had the assertion deleted out of it, which is exactly the rot this
// field is supposed to prevent.
func VerifiedByReachable(v string) error {
	path, fn := ParseVerifiedBy(v)
	info, err := os.Stat(Path(path))
	if err != nil {
		return err
	}
	if fn == "" {
		return nil
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, so it cannot contain func %s", path, fn)
	}
	data, err := os.ReadFile(Path(path))
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(fn) + `\(`).Match(data) {
		return fmt.Errorf("%s contains no func %s(", path, fn)
	}
	return nil
}

// ---------------------------------------------------------------------
// Which bridge a shipped artifact actually loads
// ---------------------------------------------------------------------

// viteDefaultPlatformRe pulls the fallback out of
// ui/shared/vite.config.ts's `process.env.VITE_PLATFORM ?? "generic"`.
var viteDefaultPlatformRe = regexp.MustCompile(`VITE_PLATFORM\s*\?\?\s*"([a-z0-9-]+)"`)

// vitePlatformAssignmentRe matches a build actually selecting a platform.
var vitePlatformAssignmentRe = regexp.MustCompile(`VITE_PLATFORM[=:]\s*"?([a-z0-9-]+)"?`)

// releaseBuildInputs are the files that decide what the shipped artifacts
// contain. Deliberately short: ui/shared/scripts/e2e-all-providers.mjs
// also sets VITE_PLATFORM, per provider, but it drives Playwright rather
// than producing anything anyone installs.
var releaseBuildInputs = []string{
	filepath.Join("container", "Dockerfile"),
	filepath.Join(".github", "workflows"),
}

// ShippedBridgeProvider answers the question every bridge-derived
// capability actually turns on: which provider's
// apps/<id>/frontend/platform.ts does a bundle anyone installs load?
//
// One answer, not seven. ui/shared/vite.config.ts selects the shell at
// BUILD time from VITE_PLATFORM and `serve-ui` serves a single embedded
// bundle (one embed directive, no flag to serve another from disk), so
// the release image and the .spk that wraps the same binaries all serve
// whichever one the release build picked. Nothing
// in the release build picks one today, so it is the vite default.
func ShippedBridgeProvider() (string, string, error) {
	data, err := os.ReadFile(Path(filepath.Join("ui", "shared", "vite.config.ts")))
	if err != nil {
		return "", "", err
	}
	m := viteDefaultPlatformRe.FindSubmatch(data)
	if m == nil {
		return "", "", fmt.Errorf("packaging: ui/shared/vite.config.ts declares no VITE_PLATFORM default")
	}
	shipped := string(m[1])
	source := "ui/shared/vite.config.ts's VITE_PLATFORM default"

	for _, input := range releaseBuildInputs {
		root := Path(input)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if hit := vitePlatformAssignmentRe.FindSubmatch(body); hit != nil {
				rel, _ := filepath.Rel(Path("."), path)
				shipped = string(hit[1])
				source = rel
			}
			return nil
		})
		if err != nil {
			return "", "", err
		}
	}
	return shipped, source, nil
}
