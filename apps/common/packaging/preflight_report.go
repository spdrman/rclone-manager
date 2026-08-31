package packaging

import (
	"fmt"
	"strings"
)

// The recorded half of issue #90: docs/conformance/submission-preflight.md.
//
// EPIC D's #178 refuses to submit the UGREEN package without a verdict
// from here and does not re-run these checks, which only works if the
// verdict is a thing that exists outside a passing test run. So the
// report is generated from a real run and then compared against the
// checked-in copy, exactly as docs/conformance/phase-4-matrix.md is: a
// hand-maintained record of a test run is a record of what somebody
// believed at the time.

const (
	preflightBeginMarker = "<!-- BEGIN GENERATED PREFLIGHT -->"
	preflightEndMarker   = "<!-- END GENERATED PREFLIGHT -->"
)

// PreflightReportPath is the checked-in report, relative to the
// repository root.
const PreflightReportPath = "docs/conformance/submission-preflight.md"

// SplicePreflightReport replaces the generated region of doc with body.
func SplicePreflightReport(doc, body string) (string, error) {
	start := strings.Index(doc, preflightBeginMarker)
	end := strings.Index(doc, preflightEndMarker)
	if start < 0 || end < 0 || end < start {
		return "", fmt.Errorf("packaging: %s is missing its %s / %s markers", PreflightReportPath, preflightBeginMarker, preflightEndMarker)
	}
	return doc[:start+len(preflightBeginMarker)] + "\n\n" + body + "\n" + doc[end:], nil
}

// SubmissionEpic is the EPIC whose Phase 5 gate this preflight is. Same
// letter as the Phase 4 gate's, and named separately anyway: these are
// two gates over two matrices, and one constant standing for both is how
// a change to one silently moves the other.
const SubmissionEpic Epic = "B"

// PreflightRun is a finished preflight: the matrix of cells, and the
// per-target readiness verdicts computed from it.
type PreflightRun struct {
	Submission Submission
	Matrix     *Matrix
	Readiness  []ProviderReadiness
}

// NewPreflightRun computes the readiness rows for a finished matrix, in
// the matrix's own report order.
func NewPreflightRun(s Submission, m *Matrix) PreflightRun {
	run := PreflightRun{Submission: s, Matrix: m}
	for _, id := range m.Conformance.ProviderIDs() {
		run.Readiness = append(run.Readiness, ReadinessFor(m, s, id))
	}
	return run
}

// ReadinessOf returns one target's recorded row.
func (r PreflightRun) ReadinessOf(id string) ProviderReadiness {
	for _, row := range r.Readiness {
		if row.Provider == id {
			return row
		}
	}
	return ProviderReadiness{Provider: id}
}

// Blockers returns every tracked issue holding any row back, sorted and
// deduplicated, which is the one line a reader of a release checklist
// actually wants.
func (r PreflightRun) Blockers() []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range r.Readiness {
		for _, b := range row.Blockers {
			if !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// submissionGateLabel is gateLabel for this report. gateLabel says
// "Phase 4", which is the other gate over the other matrix, and one label
// standing for both is how a reader ends up believing a Phase 5 column is
// reported against a Phase 4 verdict.
func submissionGateLabel(e Epic) string {
	if e == SubmissionEpic {
		return "EPIC " + string(e) + " (Phase 5)"
	}
	return "EPIC " + string(e) + " (reported here, gated there)"
}

// submissionGatedBy is submissionGateLabel in the short form a heading
// can carry.
func submissionGatedBy(e Epic) string {
	if e == SubmissionEpic {
		return "gated by EPIC " + string(e) + "'s Phase 5"
	}
	return "reported here, gated by EPIC " + string(e)
}

func storeLabel(st Store) string {
	if st.Kind == "none" {
		return "no store (documented workflow)"
	}
	return st.Name
}

// Render turns a finished preflight into the generated region of
// docs/conformance/submission-preflight.md.
func (r PreflightRun) Render() string {
	m := r.Matrix
	c := m.Conformance
	providers := c.ProviderIDs()

	var b strings.Builder

	b.WriteString("### Recorded readiness verdicts\n\n")
	b.WriteString("This is the table EPIC D's #178 consumes. A target it cannot find a row for here has not\nbeen preflighted, and #178 refuses to submit on that basis rather than re-running any of\nthese checks.\n\n")
	b.WriteString("| Target | Store or catalog | Gated by | Verdict | Undecided, tracked by | Needs the real platform |\n|---|---|---|---|---|---|\n")
	for _, row := range r.Readiness {
		blockers := "none"
		if len(row.Blockers) > 0 {
			blockers = strings.Join(row.Blockers, ", ")
		}
		pending := "nothing"
		if len(row.Pending) > 0 {
			pending = fmt.Sprintf("%d step(s)", len(row.Pending))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | **%s** | %s | %s |\n",
			row.DisplayName, storeLabel(row.Store), submissionGateLabel(row.Epic), row.Readiness, blockers, pending)
	}

	b.WriteString("\nWhy each target reads the way it does:\n\n")
	for _, row := range r.Readiness {
		fmt.Fprintf(&b, "- **%s** — %s\n", row.DisplayName, row.Why)
	}

	b.WriteString("\n### Per-rule results\n\n")
	b.WriteString("| Rule |")
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

	b.WriteString("\n### Phase 5 submission gate\n\n")
	v := m.Verdict(SubmissionEpic)
	fmt.Fprintf(&b, "Computed over the %d targets EPIC B ships, and over nothing else: %s.\n\n",
		len(v.Providers), strings.Join(c.DisplayNames(v.Providers), ", "))
	if v.Met() {
		b.WriteString("**Met.** Every rule that applies to every one of those targets was decided here and held.\nExternal store approval stays outside this repository's control (§75).\n")
	} else {
		fmt.Fprintf(&b, "**Not met.** %d rule(s) failed and %d could not be decided:\n\n", len(v.Failures), len(v.Blocked))
		b.WriteString("| Target | Rule | Outcome | Tracked by |\n|---|---|---|---|\n")
		for _, res := range append(append([]Result{}, v.Failures...), v.Blocked...) {
			tracked := r.Submission.Providers[res.Provider].Cells[res.Capability].Blocker
			if tracked == "" {
				tracked = "not tracked anywhere yet"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.Providers[res.Provider].DisplayName, c.CapabilityTitle(res.Capability), outcomeAbbrev[res.Outcome], tracked)
		}
		if bl := r.Blockers(); len(bl) > 0 {
			fmt.Fprintf(&b, "\nNothing in this run failed on a rule this work package owns. What stands between these\ntargets and a submission is %s, and the gate reports that as undecided rather than\nletting a green run imply bytes nobody can trace.\n", strings.Join(bl, " and "))
		}
	}

	for _, pid := range v.Informational {
		pr := c.Providers[pid]
		row := r.ReadinessOf(pid)
		fmt.Fprintf(&b, "\n**%s is EPIC %s's column** (work package %s), and it is recorded **%s**.\nIt is decided by these same checks, on the same terms as every other target, and it is in\nnobody's Phase 5 verdict: a rule EPIC %s owns cannot hold EPIC %s's Phase 5 open, and an\nEPIC %s column that goes green cannot close it. %s\n",
			pr.DisplayName, pr.Epic, pr.WorkPackage, row.Readiness, pr.Epic, SubmissionEpic, pr.Epic, row.Why)
	}

	b.WriteString("\n### Every cell that is not a plain PASS\n\n")
	b.WriteString("A rule that is not run reads as a rule that passed, so every cell this run did not pass is\nbelow, with why.\n")
	for _, p := range providers {
		pr := c.Providers[p]
		sp := r.Submission.Providers[p]
		var rows []string
		for _, cap := range c.Capabilities {
			res := m.Results[p][cap.ID]
			if res.Outcome == OutcomePass {
				continue
			}
			reason := sp.Cells[cap.ID].Reason
			if sp.Cells[cap.ID].Blocker != "" {
				reason = sp.Cells[cap.ID].Blocker + " — " + reason
			}
			if reason == "" {
				reason = res.Detail
			}
			rows = append(rows, fmt.Sprintf("| %s | %s | %s |\n", cap.Title, outcomeAbbrev[res.Outcome], reason))
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n#### %s (Tier %s, %s, %s)\n\n", pr.DisplayName, pr.Tier, storeLabel(sp.Store), submissionGatedBy(pr.Epic))
		b.WriteString("| Rule | Outcome | Why |\n|---|---|---|\n")
		for _, row := range rows {
			b.WriteString(row)
		}
	}

	return b.String()
}
