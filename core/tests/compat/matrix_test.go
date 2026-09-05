package compat

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
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
// exist yet is what being blocked means, and they still have to name the
// issue that would unblock them.

// matrixPath is the conformance matrix this package's cells are cited by.
const matrixPath = "../../../docs/conformance/epic-e-matrix.md"

// backtickPath matches a `like/this` span that looks like a repository
// path: it has a slash and no spaces.
var backtickPath = regexp.MustCompile("`([A-Za-z0-9_./-]+/[A-Za-z0-9_./-]+)`")

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
// reason they are blocked. What they are not exempt from is naming an
// issue, which is checked below.
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
			if !strings.Contains(row.outcome, "#") {
				t.Errorf("row %s is BLOCKED and names no issue, so nobody can tell who unblocks it: %q", row.id, row.outcome)
			}
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
	if blocked == 0 {
		t.Error("no row in the matrix is BLOCKED, and rows are still owed one. Phase 2 has landed, but five phase 1 rows and two section 4 violations " +
			"are waiting on an automated falsification rather than on code, so a matrix with nothing blocked has stopped telling the truth about what is missing. " +
			"The day those falsifications are automated and every row is honestly PASS, this line is the one to delete, and deleting it is a decision to record rather than a tidy-up")
	}
	t.Logf("matrix rows: %d PASS, %d PARTIAL, %d BLOCKED", pass, partial, blocked)
}

type matrixRow struct {
	id      string
	outcome string
	where   string
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
