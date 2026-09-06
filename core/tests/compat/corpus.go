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
//
// It names the scoped form first because that is the one almost every
// change wants, and the sweep second with what it costs, rather than the
// other way round. A note that offers the sweep as THE regenerate command
// is how a change about one surface ends up carrying five others (#549).
const corpusNote = "Captured by core/tests/compat. Every line here is something a " +
	"medium-free deployment does today. EPIC E's FR-35 says it must keep doing all " +
	"of it, so a diff in this file is a behavior change somebody has to justify, " +
	"never a number to refresh. Re-capture the one cell that moved with " +
	"COMPAT_UPDATE=<cell-name> go test ./tests/compat/ and put the reason in the " +
	"commit message. COMPAT_UPDATE=1 sweeps every cell instead, and brings back " +
	"whatever drifted in the surfaces your change never touched."

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

// ParseUpdateRequest reads what COMPAT_UPDATE is asking for: the whole
// corpus, or the named cells and nothing else.
//
// "1" is the sweep, which is what it has always meant. Anything else is a
// comma-separated list of cell names.
//
// The scoped form exists because the sweep is dangerous in a way that does
// not show up in a green build. EPIC #536 had to respell six published API
// path templates, and renaming a template takes lines off a list this
// corpus only lets grow, so a sweep was forced rather than chosen. It came
// back carrying about 117 further lines from five other surfaces that had
// nothing to do with the change, and that commit was safe only because
// somebody read all 171 insertions and wrote down what each one was. A
// re-capture aimed at one cell cannot launder the others, so an honest
// small change stops being expensive and a careless one stops being
// dangerous (#549).
//
// A spec that is neither is an error rather than a fallback to either
// behaviour. "COMPAT_UPDATE=true" quietly meaning "compare, do not update"
// leaves somebody believing they re-captured when they did not, and quietly
// meaning "sweep everything" is the exact thing this is here to stop.
//
// Duplicates are collapsed, in the order they were first named, so the
// count this reports back is the count of cells that will actually move.
func ParseUpdateRequest(spec string) (all bool, cells []string, err error) {
	if spec == "" {
		return false, nil, fmt.Errorf("COMPAT_UPDATE is empty, so nothing was asked for")
	}
	if spec == "1" {
		return true, nil, nil
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			return false, nil, fmt.Errorf(
				"COMPAT_UPDATE=%q has an empty cell name in it. Naming nothing is how a re-capture that meant to write one cell writes none and still reads as done; spell the cells out, or use COMPAT_UPDATE=1 for the whole corpus",
				spec)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		cells = append(cells, name)
	}
	return false, cells, nil
}

// MergeCells returns the baseline with the named cells replaced by what
// this run captured, and every other cell left exactly as it was checked
// in.
//
// A name the current capture does not hold is refused rather than skipped.
// A typo that quietly wrote nothing would leave somebody believing they had
// re-captured, which is worse than a red gate, because the red gate is at
// least honest about what it knows.
//
// Cells the baseline has and this run did not capture are carried through
// untouched. Dropping them here would let a scoped re-capture shrink the
// gate, which is the failure Compare refuses in the first of its three
// structural rules.
func MergeCells(baseline, current Corpus, names []string) (Corpus, error) {
	if len(names) == 0 {
		return Corpus{}, fmt.Errorf("a scoped re-capture was asked for and no cell was named")
	}
	out := Corpus{Note: baseline.Note, Cells: make(map[string]Cell, len(baseline.Cells))}
	for name, cell := range baseline.Cells {
		out.Cells[name] = cell
	}
	for _, name := range names {
		cell, ok := current.Cells[name]
		if !ok {
			return Corpus{}, fmt.Errorf(
				"this run captured no cell named %q, so there is nothing to write into the corpus for it. The cells it did capture are: %s",
				name, strings.Join(sortedKeys(current.Cells), ", "))
		}
		out.Cells[name] = cell
	}
	return out, nil
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
				"cell %q was captured but has no corpus baseline, so it is currently comparing against nothing. Capture one with COMPAT_UPDATE=%s.",
				name, name))
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
