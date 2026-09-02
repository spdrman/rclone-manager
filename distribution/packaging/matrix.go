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
	// Kind is "compose", "canonical-compose", "unraid-template", "spk" or
	// "none".
	//
	// "canonical-compose" is issue #170's Dockge shape and it is a real
	// distinction rather than a synonym for "compose": this provider
	// ships no runtime definition of its own and deploys
	// container/compose.yaml itself, so its services are read from the
	// canonical definition and Root holds only its documentation. A
	// provider declaring it is held to shipping no stack of its own
	// (ScanForForkedStack), which is what stops the shape becoming a way
	// to inherit another column's evidence.
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
	// PackageUIBundle names where to read whether this provider's own
	// NATIVE package carries a UI bundle and serves it. Empty for a
	// provider with no such package, which is every provider whose
	// adapter is metadata only, and empty matters: without it a provider
	// would inherit another one's evidence for the one claim that is
	// hardest to check by reading.
	PackageUIBundle PackageUIBundle `json:"packageUIBundle"`
	// ArchitectureClaim is this provider's OWN statement about which
	// architectures its package supports. Empty for a provider that
	// makes no claim of its own, which is what stops the repository-wide
	// release manifest standing in for seven per-provider answers.
	ArchitectureClaim ArchitectureClaim `json:"architectureClaim"`
}

// PackageUIBundle points at the two files that decide whether a native
// package ships its own UI bridge: the layout declaring which provider's
// bundle it carries, and the start script that has to actually serve it.
// Two files, because carrying a bundle nothing points --ui-dir at is a
// package that installs cleanly and shows the wrong interface.
type PackageUIBundle struct {
	Layout      string `json:"layout"`
	StartScript string `json:"startScript"`
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

const (
	// PhaseFour is the §72 Phase 4 Exit Gate's phase: the six targets
	// #86 and #81 name.
	PhaseFour = "4"
	// PhaseSix is Phase 6's release qualification (#170): every target
	// this refactor claims, which is Phase 4's six plus Portainer,
	// Dockge, CasaOS and ZimaOS.
	PhaseSix = "6"
	// AllPhases is the phase argument that restricts nothing, which is
	// what the release-qualification gate is computed over.
	AllPhases = ""
)

// Provider is one column of the matrix.
type Provider struct {
	DisplayName string `json:"displayName"`
	// Tier is the §4A support tier.
	Tier string `json:"tier"`
	// Epic is the EPIC whose gate consumes this column. Every column is
	// checked; only PhaseFourEpic's columns decide Phase 4.
	Epic Epic `json:"epic"`
	// Phase is the phase whose exit gate this column belongs to, within
	// its epic (issue #170).
	//
	// It exists because one matrix now answers two gates. The §72 Phase 4
	// Exit Gate names six targets and is settled; Phase 6's release
	// qualification names ten, the same six plus the four container
	// managers and app stores #170 adds. Without this field the four new
	// columns would silently join Phase 4's verdict and change what a
	// finished gate was computed over, which is a worse failure than
	// leaving them out: a gate whose target set moves under it cannot be
	// cited afterwards.
	Phase       string   `json:"phase"`
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

// The two declaration files this package resolves cells from. A cell's
// declaration is only ever repaired by editing one of these, so the
// failure that reports a stale one names the file it came from rather
// than the reader guessing between them.
const (
	ConformanceSource = "conformance.json"
	SubmissionSource  = "submission.json"
)

// declarationField is the path to one cell's declaration, in the form a
// reader can act on without opening anything first. A staleness failure
// that says only "update the declaration" has already made the reader do
// the work of finding it, and the pointer to the regeneration command
// that used to accompany it is not the fix: regeneration records the new
// verdict, it does not decide what the declaration should now say.
func declarationField(source, provider, capability string) string {
	return fmt.Sprintf("%s -> providers.%s.cells.%s.declared", source, provider, capability)
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

// ProviderIDsForPhase is ProviderIDsFor narrowed to one phase of that
// epic, in the same order. phase == AllPhases restricts nothing, which is
// the release-qualification gate's own set.
func (c Conformance) ProviderIDsForPhase(epic Epic, phase string) []string {
	var out []string
	for _, id := range c.ProviderIDsFor(epic) {
		if phase == AllPhases || c.Providers[id].Phase == phase {
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
	// Phase is the phase this verdict was computed for, or AllPhases for
	// every column the epic claims.
	Phase string
	// Providers are the columns this epic claims in this phase, in report
	// order.
	Providers []string
	// OtherPhase are the columns this epic claims in a DIFFERENT phase.
	// Same epic, same checks, same run, and not this gate's to decide:
	// Phase 4 closed over six targets and must keep meaning that after
	// Phase 6 registers four more against the same matrix.
	OtherPhase []string
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

// Verdict computes epic's gate over every column that epic claims, which
// is Phase 6's release qualification (#170).
func (m *Matrix) Verdict(epic Epic) Verdict { return m.VerdictForPhase(epic, AllPhases) }

// VerdictForPhase computes epic's gate over the columns it claims in one
// phase. Columns the same epic claims in another phase are still checked,
// still reported, and are not this gate's.
func (m *Matrix) VerdictForPhase(epic Epic, phase string) Verdict {
	c := m.Conformance
	v := Verdict{Epic: epic, Phase: phase}
	for _, pid := range c.ProviderIDs() {
		if c.Providers[pid].Epic != epic {
			v.Informational = append(v.Informational, pid)
			continue
		}
		if phase != AllPhases && c.Providers[pid].Phase != phase {
			v.OtherPhase = append(v.OtherPhase, pid)
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
// report rather than for the runner. Both halves are needed: which epic
// decides the column, and which of that epic's phase gates it belongs
// to, since this matrix now answers two of them.
func gateLabel(p Provider) string {
	if p.Epic != PhaseFourEpic {
		return "EPIC " + string(p.Epic) + " (reported here, gated there)"
	}
	return "EPIC " + string(p.Epic) + " (Phase " + p.Phase + ")"
}

// gatedBy is gateLabel in the short form a heading can carry.
func gatedBy(p Provider) string {
	if p.Epic != PhaseFourEpic {
		return "reported here, gated by EPIC " + string(p.Epic)
	}
	return "gated by EPIC " + string(p.Epic) + "'s Phase " + p.Phase
}

// columnLabel is a provider's heading in the report, marked with its epic
// when that is not Phase 4's, so a reader of the widest table can see at a
// glance which column is not part of the gate.
func columnLabel(pr Provider) string {
	if pr.Epic != PhaseFourEpic {
		return fmt.Sprintf("%s (EPIC %s)", pr.DisplayName, pr.Epic)
	}
	if pr.Phase != PhaseFour {
		return fmt.Sprintf("%s (P%s)", pr.DisplayName, pr.Phase)
	}
	return pr.DisplayName
}

var outcomeAbbrev = map[Outcome]string{
	OutcomePass:            "PASS",
	OutcomeFail:            "FAIL",
	OutcomeUnsupported:     "UNSUP",
	OutcomeNotApplicable:   "N/A",
	OutcomeBlocked:         "BLOCKED",
	OutcomePendingOperator: "OPERATOR",
}

// renderGate renders one gate's verdict: met, or every cell holding it
// open and what tracks each one.
func renderGate(c Conformance, v Verdict) string {
	var b strings.Builder
	if v.Met() {
		b.WriteString("**Met.** Every cell of every one of those columns was decided, and none of them failed.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "**Not met.** %d cell(s) failed and %d could not be decided, every one of them in a column this gate claims:\n\n", len(v.Failures), len(v.Blocked))
	b.WriteString("| Provider | Capability | Outcome | Tracked by |\n|---|---|---|---|\n")
	for _, r := range append(append([]Result{}, v.Failures...), v.Blocked...) {
		tracked := c.Providers[r.Provider].Cells[r.Capability].Blocker
		if tracked == "" {
			tracked = "not tracked anywhere yet"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.Providers[r.Provider].DisplayName, c.CapabilityTitle(r.Capability), outcomeAbbrev[r.Outcome], tracked)
	}
	return b.String()
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
		fmt.Fprintf(&b, "| %s | %s | %s | %s | `%s` |\n", pr.DisplayName, pr.Tier, gateLabel(pr), pr.WorkPackage, acc)
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

	release := m.VerdictForPhase(PhaseFourEpic, AllPhases)
	b.WriteString("\n### Phase 6 release qualification (issue #170)\n\n")
	fmt.Fprintf(&b, "Computed over every one of the %d targets this refactor claims: %s.\n\n",
		len(release.Providers), strings.Join(c.DisplayNames(release.Providers), ", "))
	b.WriteString(renderGate(c, release))

	phaseFour := m.VerdictForPhase(PhaseFourEpic, PhaseFour)
	b.WriteString("\n### Phase 4 Exit Gate\n\n")
	fmt.Fprintf(&b, "Computed over the %d providers the §72 exit gate names, and over nothing else: %s.\n",
		len(phaseFour.Providers), strings.Join(c.DisplayNames(phaseFour.Providers), ", "))
	fmt.Fprintf(&b, "The %d target(s) Phase 6 adds (%s) are checked and reported here on the same\nterms and are not in this verdict: a finished gate whose target set moves under it\ncannot be cited afterwards.\n\n",
		len(phaseFour.OtherPhase), strings.Join(c.DisplayNames(phaseFour.OtherPhase), ", "))
	b.WriteString(renderGate(c, phaseFour))

	v := release
	for _, pid := range v.Informational {
		pr := c.Providers[pid]
		blockers := m.Blockers(pid)
		tracked := "nothing"
		if len(blockers) > 0 {
			tracked = strings.Join(blockers, " and ")
		}
		fmt.Fprintf(&b, "\n**%s is EPIC %s's column** (work package %s).\nAll %d of its cells are decided by the same runner, on the same terms as every\nother column, and reported in full below; %d are blocked today, on %s.\nNone of them is in either verdict above. A capability EPIC %s owns cannot hold\nEPIC %s's Phase 4 or its Phase 6 release qualification open, and an EPIC %s\ncolumn that goes green cannot close either of them.\n",
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
		fmt.Fprintf(&b, "\n#### %s (Tier %s, %s)\n\n", pr.DisplayName, pr.Tier, gatedBy(pr))
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
	Commit string `json:"commit"`
	// Version is the VERSION build argument the recorded binaries were
	// stamped with, which is what `/backup-manager version` answers. It
	// is NOT necessarily the semantic version the provider packages
	// advertise: the generator defaults it to `git describe --tags
	// --always`, and this repository has no tags, so today it is an
	// abbreviated commit. VersionParityComplaints is where the two are
	// held to each other, and it turns the difference into a refusal the
	// moment canonical.json says the image is published (#88).
	Version string `json:"version"`
	// GeneratedAt is when the recording run wrote this file. The
	// provenance bundle's SBOM takes its SPDX creation timestamp from
	// here rather than from the clock, so regenerating the SBOM is
	// byte-stable and "regenerate and diff" can be a real check.
	GeneratedAt string `json:"generated_at"`
	// UnsafeLocalBuild is the stamp
	// scripts/release/record-release-hashes.sh writes when it was run
	// with UNSAFE_LOCAL_BUILD=1, which waives every guard that makes a
	// manifest reproducible: the recorded commit need not be HEAD, the
	// tree it was built from may be dirty, and the commit need not be on
	// main at all.
	//
	// A waived manifest is otherwise indistinguishable from a good one,
	// which is the whole problem: it pins a reachable commit and records
	// hashes for bytes no commit produced, so every check downstream
	// passes while comparing against nothing. The stamp is how it
	// announces itself, and the generator also defaults a waived run to
	// a gitignored path so it takes deliberate effort to get here.
	// Absent means false, which is what every honest run writes.
	UnsafeLocalBuild bool                  `json:"unsafe_local_build"`
	Architectures    []ReleaseArchitecture `json:"architectures"`
	// IndexDigest is the digest of the multi-architecture image index
	// `docker buildx build --push` produced, read back with
	// `docker buildx imagetools inspect` the same way each architecture's
	// own RegistryDigest is: what the push believes it sent is not what
	// this records, what the registry holds is. It is what cosign signs
	// and attaches the SBOM attestation to, one level above the
	// per-architecture manifests RegistryDigest names.
	//
	// A pointer for the same reason RegistryDigest is one: "not pushed
	// yet" and "pushed, index digest not recorded" have to stay different
	// values, never the same empty string. IndexDigestComplaints holds it
	// to canonical.json's image.published flag exactly as
	// registryDigestComplaints already holds every RegistryDigest, so a
	// published flag with a top-level digest missing is refused the same
	// way a per-architecture one missing already was.
	IndexDigest *string `json:"index_digest"`
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
	// It is real as of the 0.1.0 push: canonical.json records
	// image.published true, and this is the per-architecture manifest
	// digest read back with `docker buildx imagetools inspect`, not
	// scraped from the push's own output. The manifest's sibling field
	// local_image_id_sha256 is deliberately NOT modelled here, because it
	// is not a digest and nothing outside the machine that built it can
	// resolve it.
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

// AncestryRef is the ref a reachability check should be asked against,
// and the reason that one answered.
type AncestryRef struct {
	Ref string
	Why string
}

// ancestryRefPreference is the order to ask in, strongest question
// first. Asking HEAD is the weakest form: a commit that exists only on
// the current branch is reachable from HEAD and disappears from the
// history the moment the branch is squash merged, which is #174 exactly.
// Asking origin/main is the same question the generator refuses on.
var ancestryRefPreference = []string{"origin/main", "main", "HEAD"}

var ancestryRefWhy = map[string]string{
	"origin/main": "the branch a squash merge lands on, which is the question scripts/release/record-release-hashes.sh refuses on",
	"main":        "origin/main is not in this checkout, so the local main is the strongest ref available",
	"HEAD":        "neither origin/main nor main is in this checkout, so this is the weakest form of the question: a commit that only this branch has still passes",
}

// releaseAncestryRefPreference is the ordered set of refs a published
// build may be pinned to, strongest question first.
//
// It is longer than ancestryRefPreference by exactly one idea: `release`
// (issue #258). #174's rule was "an ancestor of origin/main", and the
// reason was never main specifically. It was that main does not rewrite:
// a commit reachable from it stays reachable, so a manifest pinned to it
// stays checkable. `release` has that property by policy rather than by
// accident, and docs/release-branch.md is where the policy is written
// down: it is append-only, never force-pushed, never rebased, never
// squash-merged into, and nothing is branched off it.
//
// It has to be here, and not as a nicety. A release cut carries the
// pipeline change that publishes it, so the first commit on `release` is
// by construction not yet on main. Without this entry the only ways to
// publish it are to weaken the check or to skip it, and both of those
// end at #174.
//
// HEAD stays last, and stays the weakest form: a commit only this branch
// has is reachable from HEAD and stops existing the moment the branch is
// squash merged. It is here so a fresh clone with no remote refs reports
// something rather than nothing, and ResolveReachableAncestryRef names
// it so a fallback never reads as the full-strength check.
var releaseAncestryRefPreference = []string{"origin/main", "origin/release", "main", "release", "HEAD"}

var releaseAncestryRefWhy = map[string]string{
	"origin/main":    "the branch a squash merge lands on, which is the question scripts/release/record-release-hashes.sh refuses on",
	"origin/release": "the publish branch, which docs/release-branch.md holds to being append-only, so a commit on it stays checkable exactly as one on main does",
	"main":           "origin/main is not in this checkout, so the local main is the strongest ref available",
	"release":        "origin/release is not in this checkout, so the local release branch is what answered",
	"HEAD":           "no main and no release ref is in this checkout, so this is the weakest form of the question: a commit that only this branch has still passes",
}

// ResolveReachableAncestryRef asks every rewrite-free ref this checkout
// has whether it can reach commit, and reports the first that can.
//
// This is a different question from ResolveAncestryRef's, which is why
// it is a second function rather than a change to that one.
// ResolveAncestryRef picks ONE ref, the strongest that exists, and asks
// only that. That is right when the question is "which ref is the
// authority here". It is wrong when the question is "is this build
// pinned to something that will still be there", because there is more
// than one ref that answers yes to that, and stopping at the first one
// that merely EXISTS refuses a commit that a later ref in the list would
// have accepted.
//
// Three outcomes, kept apart for the reason CommitReachableFrom keeps
// its two apart:
//
//	(ref, true, nil)   reachable, and ref says which one answered
//	(_, false, nil)    every ref this checkout has said no. A fact about
//	                   the manifest.
//	(_, false, err)    nobody decided: a shallow clone, a missing object,
//	                   or no ref to ask at all. A fact about the checkout,
//	                   and never to be reported as a no.
func ResolveReachableAncestryRef(repoDir, commit string) (AncestryRef, bool, error) {
	var asked, undecided []string
	for _, ref := range releaseAncestryRefPreference {
		cmd := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err := cmd.Run(); err != nil {
			continue
		}
		asked = append(asked, ref)
		reachable, err := CommitReachableFrom(repoDir, commit, ref)
		if err != nil {
			undecided = append(undecided, ref)
			continue
		}
		if reachable {
			return AncestryRef{Ref: ref, Why: releaseAncestryRefWhy[ref]}, true, nil
		}
	}
	return classifyUnreachedOutcome(repoDir, commit, asked, undecided)
}

// classifyUnreachedOutcome turns the results of asking every rewrite-free
// ref into the verdict ResolveReachableAncestryRef returns once none of
// them answered yes. Split out from the loop above so the rule that
// matters - one undecided ref must not be swallowed by the others
// answering no - is a fact this package can assert directly, rather than
// one that only shows up through a git fixture engineered to make merge-base
// fail for exactly one ref in a preference list and succeed for the rest.
//
// A single undecided ref is enough to withhold a "not reachable" verdict.
// "no" from the refs that did decide is not evidence about the one that
// did not: that ref could just as easily have said yes, and reporting
// false here is reporting a fact about the manifest (#174's "unreachable
// commit") when what actually happened is a fact about the checkout (a
// shallow clone, a missing object).
func classifyUnreachedOutcome(repoDir, commit string, asked, undecided []string) (AncestryRef, bool, error) {
	switch {
	case len(asked) == 0:
		return AncestryRef{}, false, fmt.Errorf("none of %v resolves to a commit in %s, so there is nothing to check reachability against", releaseAncestryRefPreference, repoDir)
	case len(undecided) > 0:
		return AncestryRef{}, false, fmt.Errorf("git could not decide whether %s is reachable from %v (asked %v in total); that is a fact about this checkout (a shallow clone, a missing object), not about the commit", commit, undecided, asked)
	}
	return AncestryRef{Why: fmt.Sprintf("asked %v", asked)}, false, nil
}

// ResolveAncestryRef picks the strongest ref this checkout actually has
// to ask a reachability question against.
//
// The generator asks whether the recorded commit is an ancestor of
// origin/main; the checks downstream used to ask only whether it is an
// ancestor of HEAD. That gap is not academic. Anything that reaches the
// manifest without going through the generator (a hand edit, a waived
// run, future tooling) passes on the feature branch and fails only once
// the squash merge has landed it on main, which is #174's timeline
// restated: green on the branch, broken on main, found late. Asking the
// strongest available ref closes it, and naming which ref answered keeps
// a fallback from looking like the full-strength check.
//
// Requires git on PATH and repoDir inside a repository, the same
// prerequisite CommitReachableFrom carries.
func ResolveAncestryRef(repoDir string) (AncestryRef, error) {
	for _, ref := range ancestryRefPreference {
		cmd := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err := cmd.Run(); err == nil {
			return AncestryRef{Ref: ref, Why: ancestryRefWhy[ref]}, nil
		}
	}
	return AncestryRef{}, fmt.Errorf("none of %v resolves to a commit in %s, so there is nothing to check reachability against", ancestryRefPreference, repoDir)
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
