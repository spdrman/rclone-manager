package compat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The corpus format and the comparison over it: what a captured cell is,
// how two captures are held against each other, and how a difference is
// reported to whoever has to act on it.
//
// It is deliberately separate from the capture files beside it. Everything
// here is pure: it reads no database, builds no binary and runs no
// command, so it is the half of this package that can be reasoned about
// and changed without a deployment to point it at. That split is also why
// the rules live here rather than at the call sites, where each cell would
// have grown its own idea of what "the same" means.
//
// The comparison is asymmetric on purpose, and the asymmetry is the
// package's whole opinion: a captured line that is missing is a break, a
// captured line that is new is only a break under RuleIdentical, and a
// cell that captured nothing is a break under every rule there is. Compare
// argues each of those where it makes the decision.

// Rule is how a cell's captured lines are compared against the corpus.
type Rule string

const (
	// RuleIdentical means the current capture must equal the corpus line
	// for line. This is the default, and it is what FR-35's "identical"
	// clauses mean: a medium-free deployment has no non-local placement,
	// so there is no additive column for it to render and nothing to
	// forgive.
	RuleIdentical Rule = "identical"

	// RuleAdditiveOnly means every corpus line must still be there, in the
	// same relative order, and new lines may appear between them. It is
	// for the two surfaces EPIC E is explicitly allowed to grow: the
	// database schema (FR-29 adds tables) and the set of applied
	// migrations. Nothing that already exists may change or disappear.
	RuleAdditiveOnly Rule = "additive-only"
)

// Cell is one observed surface.
//
// Certifies is not decoration. A cell whose failure a reader cannot
// interpret gets muted, and a muted cell is worse than no cell, so the
// sentence travels with the data and is printed on failure.
type Cell struct {
	Certifies string   `json:"certifies"`
	Rule      Rule     `json:"rule"`
	Lines     []string `json:"lines"`
}

// Corpus is the whole capture: every cell, keyed by a stable name.
type Corpus struct {
	Note  string          `json:"note"`
	Cells map[string]Cell `json:"cells"`
}

// corpusNote is written into the file so the next person to see a red
// build reads the rule before they reach for the regenerate command.
const corpusNote = "Captured by core/tests/compat. Every line here is something a " +
	"medium-free deployment does today. EPIC E's FR-35 says it must keep doing all " +
	"of it, so a diff in this file is a behavior change somebody has to justify, " +
	"never a number to refresh. Regenerate with COMPAT_UPDATE=1 go test ./tests/compat/ " +
	"and put the reason in the commit message."

// Save writes the corpus to path with stable formatting.
func (c Corpus) Save(path string) error {
	c.Note = corpusNote
	blob, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(blob, '\n'), 0o644)
}

// LoadCorpus reads a corpus from path.
func LoadCorpus(path string) (Corpus, error) {
	var c Corpus
	blob, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(blob, &c); err != nil {
		return c, fmt.Errorf("parsing %s: %w", path, err)
	}
	return c, nil
}

// Compare returns one human-readable finding per problem, and an empty
// slice when the current capture is compatible with the baseline.
//
// The three structural refusals come first and are deliberate:
//
//   - a cell the baseline has and the capture does not is a failure, not a
//     shrink. A gate that gets smaller when code is deleted is the
//     "omitting a capability shrinks the matrix" failure this repository
//     already fixed once, in the phase 4 conformance matrix.
//   - a cell the capture has and the baseline does not is a failure too:
//     it is a new claim nobody has a recorded baseline for, so it is
//     asserting against itself until somebody captures one.
//   - an empty cell is a failure whichever side it is on. A cell that
//     inspected nothing passes every comparison there is, which is exactly
//     the shape of check this repository keeps finding: one that cannot
//     fail.
func Compare(baseline, current Corpus) []string {
	var findings []string

	for _, name := range sortedKeys(baseline.Cells) {
		if _, ok := current.Cells[name]; !ok {
			findings = append(findings, fmt.Sprintf(
				"cell %q is in the corpus but this run captured nothing for it. A surface FR-35 pins cannot stop being observed; if it genuinely no longer exists, that is itself the compatibility break.",
				name))
		}
	}
	for _, name := range sortedKeys(current.Cells) {
		if _, ok := baseline.Cells[name]; !ok {
			findings = append(findings, fmt.Sprintf(
				"cell %q was captured but has no corpus baseline, so it is currently comparing against nothing. Capture one with COMPAT_UPDATE=1.",
				name))
		}
	}

	for _, name := range sortedKeys(current.Cells) {
		cur := current.Cells[name]
		base, ok := baseline.Cells[name]
		if !ok {
			continue
		}
		if len(cur.Lines) == 0 {
			findings = append(findings, fmt.Sprintf(
				"cell %q captured no lines at all. It certifies %q, and it cannot certify that by observing nothing.",
				name, cur.Certifies))
			continue
		}
		if len(base.Lines) == 0 {
			findings = append(findings, fmt.Sprintf(
				"cell %q has an empty baseline, so it passes whatever the product does. Recapture it.", name))
			continue
		}
		if base.Rule != cur.Rule {
			findings = append(findings, fmt.Sprintf(
				"cell %q is compared under rule %q now and was captured under %q. Loosening how a cell is compared is a change to the gate, not to the product, and it needs saying out loud.",
				name, cur.Rule, base.Rule))
			continue
		}

		switch cur.Rule {
		case RuleAdditiveOnly:
			if missing, ok := firstMissingInOrder(base.Lines, cur.Lines); !ok {
				findings = append(findings, fmt.Sprintf(
					"cell %q (%s) may only grow, and this line is gone or reordered:\n      %s\n%s",
					name, cur.Certifies, missing, unifiedish(base.Lines, cur.Lines)))
			}
		default:
			if !equalLines(base.Lines, cur.Lines) {
				findings = append(findings, fmt.Sprintf(
					"cell %q (%s) changed:\n%s",
					name, cur.Certifies, unifiedish(base.Lines, cur.Lines)))
			}
		}
	}

	return findings
}

func sortedKeys(m map[string]Cell) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalLines(a, b []string) bool {
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

// firstMissingInOrder reports whether every line of want appears in got in
// the same relative order, and names the first one that does not.
func firstMissingInOrder(want, got []string) (string, bool) {
	i := 0
	for _, line := range got {
		if i < len(want) && want[i] == line {
			i++
		}
	}
	if i == len(want) {
		return "", true
	}
	return want[i], false
}

// unifiedish renders the difference between two line sets in a form a
// reviewer can read.
//
// It prints whole lines with a - / + marker rather than two JSON blobs,
// because this repository has already shipped one gate whose failure
// message dumped two serialized structures and could not be reviewed: the
// reader could see that something differed and not what.
func unifiedish(base, cur []string) string {
	var b strings.Builder
	inBase := map[string]int{}
	for _, l := range base {
		inBase[l]++
	}
	inCur := map[string]int{}
	for _, l := range cur {
		inCur[l]++
	}

	shown := 0
	const maxShown = 40
	for _, l := range base {
		if inCur[l] == 0 {
			if shown++; shown > maxShown {
				b.WriteString("      ... (more)\n")
				return b.String()
			}
			fmt.Fprintf(&b, "      - %s\n", l)
		}
	}
	for _, l := range cur {
		if inBase[l] == 0 {
			if shown++; shown > maxShown {
				b.WriteString("      ... (more)\n")
				return b.String()
			}
			fmt.Fprintf(&b, "      + %s\n", l)
		}
	}
	if shown == 0 {
		// Same multiset, different order. Say so, rather than print an
		// empty diff and leave the reader thinking the gate misfired.
		b.WriteString("      (the same lines in a different order; order is part of the surface here)\n")
	}
	return b.String()
}
