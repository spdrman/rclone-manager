package packaging

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

// Provider is one column of the matrix.
type Provider struct {
	DisplayName string `json:"displayName"`
	// Tier is the §4A support tier.
	Tier        string   `json:"tier"`
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
	b.WriteString("| Provider | Tier | Work package | Acceptance procedure |\n|---|---|---|---|\n")
	for _, p := range providers {
		pr := c.Providers[p]
		acc := pr.Acceptance
		if acc == "" {
			acc = "none (automated instead)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | `%s` |\n", pr.DisplayName, pr.Tier, pr.WorkPackage, acc)
	}

	b.WriteString("\n### Per-capability results\n\n")
	b.WriteString("| Capability |")
	for _, p := range providers {
		fmt.Fprintf(&b, " %s |", c.Providers[p].DisplayName)
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
		fmt.Fprintf(&b, "\n#### %s (Tier %s)\n\n", pr.DisplayName, pr.Tier)
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
