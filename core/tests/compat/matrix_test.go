package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Whether docs/conformance/epic-e-matrix.md is still describing this
// repository, or has quietly become prose.
//
// It lives in this package because it is the same failure the corpus is
// built around, one level up. A cell that captured nothing passes every
// comparison; a PASS row citing a suite that does not exist reads clean
// and certifies nothing at all. Both are declarations that cannot fail,
// and this repository has shipped that shape before.
//
// So every path a PASS row names is resolved against the tree, and a PASS
// row that cites nothing is refused outright, since a claim with no
// evidence is the cheapest kind to write. BLOCKED rows are exempt from the
// existence check and only from that one: naming a file that does not
// exist yet is what being blocked means, and they still have to name an
// issue, which has to be OPEN. See
// TestEveryBlockedRowCitesAnIssueThatIsStillOpen for why that word is
// load-bearing and what it costs.

// matrixPath is the conformance matrix this package's cells are cited by.
const matrixPath = "../../../docs/conformance/epic-e-matrix.md"

// specPath is the contract the matrix is an account of. Its two exit gates
// are checkbox lists, one line per gate line, and the matrix carries one
// row per line of them.
const specPath = "../../../docs/EPIC-E-alternative-storage.md"

// matrixRepo is the repository whose issues a BLOCKED row cites. It is
// written down rather than inferred from a git remote because the reduced
// trees this suite also runs in (scripts/architecture deletes whole
// layers) are not always full clones, and a check that silently resolved
// against a different repository would answer confidently and wrongly.
const matrixRepo = "spdrman/rclone-manager"

// backtickPath matches a `like/this` span that looks like a repository
// path: it has a slash and no spaces.
var backtickPath = regexp.MustCompile("`([A-Za-z0-9_./-]+/[A-Za-z0-9_./-]+)`")

// issueCitation matches a `#123` issue reference in an outcome cell.
var issueCitation = regexp.MustCompile(`#(\d+)`)

// TestTheMatrixDoesNotCiteSuitesThatDoNotExist keeps
// docs/conformance/epic-e-matrix.md from being prose.
//
// The matrix's whole value is that PASS means "there is a check, it runs,
// and it has been watched to fail". A PASS row citing a file the
// repository does not have is the same failure as the phase 4 matrix
// shrinking when a capability is omitted: the declaration reads clean and
// certifies nothing. So every path a PASS row names is resolved against
// the tree, and a row that cites nothing at all is refused too, because a
// PASS with no evidence is the cheapest kind to write.
//
// BLOCKED rows are deliberately exempt from the existence check: they name
// where a check WILL live, and that path not existing yet is the whole
// reason they are blocked. What they are not exempt from is naming an open
// issue, which is TestEveryBlockedRowCitesAnIssueThatIsStillOpen's job.
//
// # What replaced the blocked floor
//
// This test used to end with `if blocked == 0 { t.Error(...) }`, a floor
// asserting that SOME row was still blocked. It was true when it was
// written and it barred the finished state: the day the last row earned
// its PASS, the check that exists to keep the matrix honest would have
// failed for the matrix being complete, and whoever hit that would have
// deleted the line rather than replaced it. #522 replaced it with the
// thing the floor was standing in for, which is that the matrix has to
// keep having a row per gate line. A row cannot be quietly dropped to
// tidy the table, and the parser silently matching nothing still fails,
// because the ids are compared against the SPEC's own exit-gate lines
// rather than against a number written down here.
func TestTheMatrixDoesNotCiteSuitesThatDoNotExist(t *testing.T) {
	blob, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("reading %s: %v", matrixPath, err)
	}
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}

	rows := gateRows(string(blob))
	if len(rows) == 0 {
		t.Fatal("no gate rows parsed out of the matrix, so this check inspected nothing")
	}

	var (
		pass, partial, blocked int
		missing                []string
	)
	for _, row := range rows {
		switch {
		case strings.HasPrefix(row.outcome, "PASS"):
			pass++
		case strings.HasPrefix(row.outcome, "PARTIAL"):
			partial++
		case strings.HasPrefix(row.outcome, "BLOCKED"):
			blocked++
			continue
		default:
			t.Errorf("row %s has outcome %q, which is not one of PASS, PARTIAL or BLOCKED", row.id, row.outcome)
			continue
		}

		cited := backtickPath.FindAllStringSubmatch(row.where, -1)
		if len(cited) == 0 {
			t.Errorf("row %s claims %s and cites no file at all, so there is nothing to check it against", row.id, row.outcome)
			continue
		}
		for _, m := range cited {
			// A citation whose whole top-level directory is absent is not a
			// matrix that has gone stale, it is this suite running inside a
			// deliberately reduced tree. scripts/architecture's dependency
			// proofs delete apps/ and distribution/ outright and then run
			// core/'s tests, to show core builds without them, and a row
			// citing evidence in apps/ has nothing to say about that.
			//
			// The distinction is exact rather than forgiving: if the
			// directory IS there and the file is not, that is a stale
			// citation and still fails. Only the wholesale absence of a
			// layer is treated as "not this tree's question", and it is
			// logged so a run that skipped a check says so out loud rather
			// than passing quietly.
			if top := topLevelDir(m[1]); top != "" {
				if _, err := os.Stat(filepath.Join(repoRoot, top)); err != nil {
					t.Logf("row %s cites %s and %s/ is not in this tree at all, so this is a reduced tree (the dependency-rule proof deletes whole layers) and the citation is not checked here", row.id, m[1], top)
					continue
				}
			}
			p := filepath.Join(repoRoot, m[1])
			if _, err := os.Stat(p); err != nil {
				missing = append(missing, row.id+" cites "+m[1])
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d matrix citation(s) name something this repository does not have:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}

	if pass == 0 {
		t.Error("no row in the matrix claims PASS, which means either nothing is checked or the parser stopped working; both are worth failing over")
	}
	t.Logf("matrix rows: %d PASS, %d PARTIAL, %d BLOCKED", pass, partial, blocked)
}

// TestTheMatrixHasARowPerGateLine is the replacement for the blocked
// floor, and it is aimed at the same failure from the side that stays true
// when the work is finished.
//
// The matrix opens by promising "one row per line of the spec's two exit
// gates, with nothing dropped for not being ready yet". That promise is
// what the floor was really protecting: a row nobody can make green is the
// row most worth deleting, and deleting it leaves a matrix that reads
// complete. So the row ids are compared against the SPEC's own exit-gate
// checkbox lists, which is a count nobody maintains by hand, and they have
// to be contiguous from 1, so a row cannot go missing out of the middle
// either.
func TestTheMatrixHasARowPerGateLine(t *testing.T) {
	matrix, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("reading %s: %v", matrixPath, err)
	}
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}

	rows := gateRows(string(matrix))
	byPhase := map[string][]string{}
	for _, row := range rows {
		phase := row.id[:strings.Index(row.id, ".")]
		byPhase[phase] = append(byPhase[phase], row.id)
	}

	for phase, heading := range map[string]string{
		"P1": "### Phase 1 exit gate",
		"P2": "### Phase 2 exit gate",
	} {
		lines := specGateLines(string(spec), heading)
		if len(lines) == 0 {
			t.Errorf("no checkbox lines parsed out of %q in %s, so this check inspected nothing for %s", heading, specPath, phase)
			continue
		}
		want := make([]string, 0, len(lines))
		for i := range lines {
			want = append(want, fmt.Sprintf("%s.%d", phase, i+1))
		}
		got := append([]string(nil), byPhase[phase]...)
		sort.Strings(got)
		sort.Strings(want)
		if !sameStrings(got, want) {
			t.Errorf("%s names %d line(s) under %q and the matrix carries rows %v; it has to carry one row per line (%v), because the row nobody can make green is the row most worth deleting",
				specPath, len(lines), heading, byPhase[phase], want)
		}
	}
}

// TestEveryBlockedRowCitesAnIssueThatIsStillOpen is the citation guard, and
// the word that matters in its name is "open".
//
// It used to be one line inside the test above: a BLOCKED outcome had to
// contain a "#". That check could not fail in the way it needed to. P1.3,
// P1.4 and P1.5 cited #235, P1.7 cited #237, P1.8 cited all three, and
// every one of those issues had been closed for weeks. The rows read as
// "somebody is working on this" and pointed at work nobody could pick up,
// and EPIC E was closed with seven of them standing (#522). A citation is
// not evidence of a live blocker unless somebody checks the issue is live.
//
// Nothing in this repository can know that. Closing an issue changes
// nothing in the tree, which is exactly why the drift was invisible, so
// this asks GitHub. That has two consequences worth stating rather than
// discovering:
//
//   - it only asks when a row is actually BLOCKED. A matrix with none, which
//     is the state #522 left it in, touches no network at all;
//   - when a row IS blocked and GitHub cannot be reached, this FAILS. A
//     BLOCKED row whose citation nobody could resolve is precisely the state
//     this check exists to refuse, and a skip there would read as a pass,
//     which is the failure mode the whole conformance matrix is built
//     around. core/tests/machines takes the same line about a missing
//     container image, deliberately and for the same reason (#160).
//
// Its ability to fail is proven twice: offline on every run, against stub
// resolvers, in TestTheBlockedCitationGuardCanFail; and through the real
// `gh` wiring by scripts/conformance/selftest.sh, which points a PASS row
// at #235 and requires this to say so.
func TestEveryBlockedRowCitesAnIssueThatIsStillOpen(t *testing.T) {
	blob, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("reading %s: %v", matrixPath, err)
	}

	rows := blockedRows(string(blob))
	if len(rows) == 0 {
		t.Log("no row in the matrix is BLOCKED, so there is no citation to resolve and GitHub is not asked")
		return
	}

	for _, complaint := range blockedCitationComplaints(rows, ghIssueState) {
		t.Error(complaint)
	}
}

// TestTheBlockedCitationGuardCanFail is the offline half, and it is the
// half that runs on every gate.
//
// The guard above resolves issues against GitHub, so on a matrix with
// nothing blocked it never executes a single comparison. An assertion that
// usually inspects nothing is the shape this repository keeps getting
// caught by, so the decision it makes is a pure function over a resolver
// and it is exercised here in all four directions: an open citation is
// silent, a closed one complains and names the issue, a citation nobody
// could resolve complains rather than passing, and a BLOCKED row citing
// nothing at all complains too.
func TestTheBlockedCitationGuardCanFail(t *testing.T) {
	rows := []blockedRow{{id: "P1.3", outcome: "BLOCKED (#235)", issues: []int{235}}}

	t.Run("a closed issue is refused", func(t *testing.T) {
		got := blockedCitationComplaints(rows, func(int) (string, error) { return "CLOSED", nil })
		if len(got) != 1 {
			t.Fatalf("complaints = %v, want exactly one", got)
		}
		for _, want := range []string{"P1.3", "#235", "CLOSED"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("the complaint %q does not name %q, so a reader cannot act on it", got[0], want)
			}
		}
	})

	t.Run("an open issue is accepted", func(t *testing.T) {
		if got := blockedCitationComplaints(rows, func(int) (string, error) { return "OPEN", nil }); len(got) != 0 {
			t.Errorf("complaints = %v for a BLOCKED row citing an OPEN issue, want none", got)
		}
	})

	t.Run("a citation that could not be resolved is refused, not skipped", func(t *testing.T) {
		got := blockedCitationComplaints(rows, func(int) (string, error) { return "", errors.New("gh: not found") })
		if len(got) != 1 {
			t.Fatalf("complaints = %v, want exactly one; a BLOCKED row nobody could check is the state this guard exists to refuse", got)
		}
		if !strings.Contains(got[0], "#235") {
			t.Errorf("the complaint %q does not name the issue it could not resolve", got[0])
		}
	})

	t.Run("a BLOCKED row citing nothing is refused", func(t *testing.T) {
		bare := []blockedRow{{id: "V4", outcome: "BLOCKED"}}
		got := blockedCitationComplaints(bare, func(int) (string, error) {
			t.Error("the resolver was called for a row that cites no issue")
			return "OPEN", nil
		})
		if len(got) != 1 {
			t.Fatalf("complaints = %v, want exactly one", got)
		}
	})
}

// TestTheSpecsExitGateBoxesAgreeWithTheMatrix stops the spec's checkboxes
// from drifting away from the evidence again.
//
// Both exit gates in docs/EPIC-E-alternative-storage.md sat entirely
// unticked long after phase 2 landed, which is the same defect as a
// BLOCKED row citing closed work seen from the other end: the document
// somebody reads to find out where the EPIC stands was stale, and nothing
// in the repository could tell. The matrix already carries an outcome per
// gate line, watched, so the boxes are held to it: a PASS row's box is
// ticked, and a row that is anything else is not. PARTIAL is deliberately
// on the untick side. A box is one bit and a PARTIAL row is a paragraph
// about which half holds, so ticking one would be the more misleading of
// the two answers.
func TestTheSpecsExitGateBoxesAgreeWithTheMatrix(t *testing.T) {
	matrix, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("reading %s: %v", matrixPath, err)
	}
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}

	outcome := map[string]string{}
	for _, row := range gateRows(string(matrix)) {
		outcome[row.id] = row.outcome
	}

	for phase, heading := range map[string]string{
		"P1": "### Phase 1 exit gate",
		"P2": "### Phase 2 exit gate",
	} {
		for i, line := range specGateLines(string(spec), heading) {
			id := fmt.Sprintf("%s.%d", phase, i+1)
			got, ok := outcome[id]
			if !ok {
				// TestTheMatrixHasARowPerGateLine owns this failure and
				// says it better; saying it twice would make one missing
				// row look like two problems.
				continue
			}
			ticked := strings.HasPrefix(line, "- [x]")
			want := strings.HasPrefix(got, "PASS")
			if ticked == want {
				continue
			}
			if want {
				t.Errorf("%s is %q in the matrix and its box under %q is not ticked; the spec is the document somebody reads to find out where this EPIC stands:\n  %s",
					id, got, heading, truncate(line, 160))
				continue
			}
			t.Errorf("%s is %q in the matrix and its box under %q is ticked, which claims more than anything has been watched to prove:\n  %s",
				id, got, heading, truncate(line, 160))
		}
	}
}

type matrixRow struct {
	id      string
	outcome string
	where   string
}

// blockedRow is one BLOCKED declaration and the issues it cites.
type blockedRow struct {
	id      string
	outcome string
	issues  []int
}

// blockedCitationComplaints is the decision the citation guard makes,
// separated from where the answers come from so it can be exercised
// against a resolver that cannot reach anything.
//
// An issue is resolved once per run rather than once per row, because
// P1.8 cited three of them and rows share citations by design.
func blockedCitationComplaints(rows []blockedRow, resolve func(int) (string, error)) []string {
	type answer struct {
		state string
		err   error
	}
	seen := map[int]answer{}
	var out []string

	for _, row := range rows {
		if len(row.issues) == 0 {
			out = append(out, fmt.Sprintf(
				"row %s is BLOCKED and names no issue, so nobody can tell who unblocks it: %q", row.id, row.outcome))
			continue
		}
		for _, n := range row.issues {
			a, ok := seen[n]
			if !ok {
				state, err := resolve(n)
				a = answer{state: state, err: err}
				seen[n] = a
			}
			switch {
			case a.err != nil:
				out = append(out, fmt.Sprintf(
					"row %s is BLOCKED citing #%d and this check could not find out whether #%d is open: %v. "+
						"A BLOCKED row nobody can resolve is the state this guard exists to refuse, so it fails here rather than passing quietly",
					row.id, n, n, a.err))
			case !strings.EqualFold(a.state, "OPEN"):
				out = append(out, fmt.Sprintf(
					"row %s is BLOCKED citing #%d, and #%d is %s. A BLOCKED row has to point at work somebody can pick up; "+
						"citing closed work is how seven rows of this matrix stayed BLOCKED after the code they certify had landed (#522)",
					row.id, n, n, strings.ToUpper(a.state)))
			}
		}
	}
	return out
}

// ghIssueState asks GitHub what state an issue is in.
//
// Through the `gh` CLI rather than the REST API directly, because this
// repository is private and gh is where the credential for it already
// lives; nothing here holds, reads or writes a token.
func ghIssueState(number int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "issue", "view", strconv.Itoa(number),
		"--repo", matrixRepo, "--json", "state", "--jq", ".state")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("gh issue view %d --repo %s: %s", number, matrixRepo, detail)
	}
	state := strings.TrimSpace(stdout.String())
	if state == "" {
		return "", fmt.Errorf("gh issue view %d --repo %s answered with nothing", number, matrixRepo)
	}
	return state, nil
}

// gateRows pulls the two exit-gate tables out of the matrix.
//
// It keys on the row id shape (P1.n / P2.n) rather than on table position,
// so reordering the document, or adding prose between the tables, does not
// silently empty this check. An empty result is a failure above, which is
// what makes that safe rather than merely tidy.
func gateRows(md string) []matrixRow {
	var out []matrixRow
	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cols := splitRow(line)
		if len(cols) < 5 {
			continue
		}
		id := cols[0]
		if !regexp.MustCompile(`^P[12]\.\d+$`).MatchString(id) {
			continue
		}
		out = append(out, matrixRow{id: id, outcome: cols[1], where: cols[3]})
	}
	return out
}

// blockedRows collects every BLOCKED declaration in the matrix, from BOTH
// tables that carry one.
//
// The section 4 planted-violation table is here as well as the two exit
// gates, and that is not tidiness: V4 and V8 were BLOCKED citing #235 and
// #237 the entire time, and gateRows never looked at them, so the old
// citation check was not weak about those two rows, it was absent. Their
// table has four columns with the outcome last, which is why this parses
// rather than reusing gateRows.
func blockedRows(md string) []blockedRow {
	var out []blockedRow
	gate := regexp.MustCompile(`^P[12]\.\d+$`)
	violation := regexp.MustCompile(`^V\d+$`)

	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cols := splitRow(line)
		var id, outcome string
		switch {
		case len(cols) >= 5 && gate.MatchString(cols[0]):
			id, outcome = cols[0], cols[1]
		case len(cols) == 4 && violation.MatchString(cols[0]):
			id, outcome = cols[0], cols[3]
		default:
			continue
		}
		if !strings.HasPrefix(outcome, "BLOCKED") {
			continue
		}
		row := blockedRow{id: id, outcome: outcome}
		for _, m := range issueCitation.FindAllStringSubmatch(outcome, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			row.issues = append(row.issues, n)
		}
		out = append(out, row)
	}
	return out
}

// specGateLines returns the checkbox lines of one exit gate in the spec,
// in the order the spec writes them, which is the order the matrix
// numbers its rows in.
func specGateLines(md, heading string) []string {
	var out []string
	inside := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == heading {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			break
		}
		if strings.HasPrefix(trimmed, "- [ ]") || strings.HasPrefix(trimmed, "- [x]") {
			out = append(out, trimmed)
		}
	}
	return out
}

func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// topLevelDir is the first path segment of a repository-relative citation,
// or "" when the citation names a file at the root.
func topLevelDir(rel string) string {
	rel = filepath.ToSlash(rel)
	if i := strings.Index(rel, "/"); i > 0 {
		return rel[:i]
	}
	return ""
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
