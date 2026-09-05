package compat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gate itself: capture every FR-35 surface from this working tree and
// hold it against the corpus checked in beside it.
//
// One test, because there is one question. Splitting it per cell would
// mean capturing the deployment several times over for no extra evidence,
// and would lose the property that matters most on a failure: every
// finding for every surface arrives in one message, so a change that moved
// four cells is read once rather than fixed one red cell at a time.
//
// COMPAT_UPDATE=1 rewrites the corpus instead of comparing against it.
// That is the only way to change a pinned line, and it is deliberately an
// environment variable and not a flag: it has to be something a person
// types on purpose, with the reason going into the commit message, rather
// than something a gate could ever set for itself.
//
// The failure text below is part of the mechanism. A reader who meets this
// gate for the first time is meeting it while something is red, so the
// message says which cells may grow, which may not, and what regenerating
// actually claims.

// TestMediumFreeSurfacesAreUnchanged is EPIC E's FR-35 gate.
//
// It drives every surface FR-35 names against a medium-free deployment and
// compares what came back to the corpus checked in beside it. A failure
// here is not a number to refresh: it is either a compatibility break EPIC
// E was not allowed to make, or a deliberate behavior change that has to
// be re-captured on purpose, with the reason in the commit message.
//
// The one thing this test must never become is a test that passes because
// it looked at nothing. Compare refuses an empty cell on either side, and
// refuses a cell that appears on only one side, for exactly that reason.
func TestMediumFreeSurfacesAreUnchanged(t *testing.T) {
	ctx := context.Background()

	coreRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving the core module root: %v", err)
	}

	current, err := CaptureAll(ctx, t.TempDir(), coreRoot, "testdata/configs")
	if err != nil {
		t.Fatalf("capturing the medium-free surfaces: %v", err)
	}

	if os.Getenv("COMPAT_UPDATE") == "1" {
		if err := current.Save(CorpusPath); err != nil {
			t.Fatalf("writing %s: %v", CorpusPath, err)
		}
		t.Logf("COMPAT_UPDATE=1: rewrote %s with %d cells. Every line that changed is a behavior change somebody has to justify in the commit message.",
			CorpusPath, len(current.Cells))
		return
	}

	baseline, err := LoadCorpus(CorpusPath)
	if err != nil {
		t.Fatalf("reading the checked-in corpus: %v\n\nIf this is the first run on a new checkout, capture one with:\n  COMPAT_UPDATE=1 go test ./tests/compat/", err)
	}

	findings := Compare(baseline, current)
	if len(findings) == 0 {
		return
	}

	t.Errorf("FR-35 says a medium-free deployment behaves byte for byte as it did before EPIC E. %d surface(s) disagree:\n\n%s\n\n%s",
		len(findings), strings.Join(findings, "\n\n"), whatToDoAboutIt)
}

const whatToDoAboutIt = `Each finding above names the cell, what that cell certifies, and the exact lines
that moved. Two of those are allowed to grow and the rest are not:
03-migrated-schema may gain tables and migrations (FR-29 adds both), and it may
never change or drop one that already exists. Everything else is compared line
for line, because a medium-free deployment has no non-local placement, so FR-35
allows it no additive column either.

If the change is genuinely intended, re-capture with:
  COMPAT_UPDATE=1 go test ./tests/compat/
and say in the commit message which surface moved and why an operator upgrading
into it is not surprised.`

// TestTheCorpusIsNotEmpty is the positive control for the control.
//
// A corpus file that got truncated, or a Compare that stopped finding
// anything to compare, would leave the test above passing silently. This
// repository has shipped that shape before, so the corpus's own size is
// asserted rather than assumed.
func TestTheCorpusIsNotEmpty(t *testing.T) {
	baseline, err := LoadCorpus(CorpusPath)
	if err != nil {
		t.Fatalf("reading %s: %v", CorpusPath, err)
	}
	if len(baseline.Cells) == 0 {
		t.Fatal("the corpus has no cells, so the gate above compares nothing")
	}
	for name, cell := range baseline.Cells {
		if len(cell.Lines) == 0 {
			t.Errorf("corpus cell %q has no lines, so it passes whatever the product does", name)
		}
		if strings.TrimSpace(cell.Certifies) == "" {
			t.Errorf("corpus cell %q does not say what it certifies, so nobody can judge its failure", name)
		}
		switch cell.Rule {
		case RuleIdentical, RuleAdditiveOnly:
		default:
			t.Errorf("corpus cell %q is compared under unknown rule %q", name, cell.Rule)
		}
	}
}

// TestCompareRefusesTheShapesThatCannotFail pins the three structural
// refusals in Compare, because they are the ones protecting every other
// cell from quietly becoming a no-op.
func TestCompareRefusesTheShapesThatCannotFail(t *testing.T) {
	full := Corpus{Cells: map[string]Cell{
		"a": {Certifies: "something", Rule: RuleIdentical, Lines: []string{"one", "two"}},
	}}

	t.Run("a cell that vanished", func(t *testing.T) {
		findings := Compare(full, Corpus{Cells: map[string]Cell{}})
		if len(findings) == 0 {
			t.Fatal("a surface that stopped being observed was reported as compatible")
		}
	})

	t.Run("a cell with no baseline", func(t *testing.T) {
		findings := Compare(Corpus{Cells: map[string]Cell{}}, full)
		if len(findings) == 0 {
			t.Fatal("a cell comparing against nothing was reported as compatible")
		}
	})

	t.Run("an empty capture", func(t *testing.T) {
		empty := Corpus{Cells: map[string]Cell{"a": {Certifies: "something", Rule: RuleIdentical}}}
		findings := Compare(full, empty)
		if len(findings) == 0 {
			t.Fatal("a cell that captured nothing was reported as compatible")
		}
	})

	t.Run("a rule that got loosened", func(t *testing.T) {
		loosened := Corpus{Cells: map[string]Cell{
			"a": {Certifies: "something", Rule: RuleAdditiveOnly, Lines: []string{"one", "two"}},
		}}
		findings := Compare(full, loosened)
		if len(findings) == 0 {
			t.Fatal("a cell whose comparison rule was weakened was reported as compatible")
		}
	})

	t.Run("additive-only still refuses a removal", func(t *testing.T) {
		base := Corpus{Cells: map[string]Cell{
			"a": {Certifies: "something", Rule: RuleAdditiveOnly, Lines: []string{"one", "two"}},
		}}
		shrunk := Corpus{Cells: map[string]Cell{
			"a": {Certifies: "something", Rule: RuleAdditiveOnly, Lines: []string{"one"}},
		}}
		if findings := Compare(base, shrunk); len(findings) == 0 {
			t.Fatal("a line that disappeared under additive-only was reported as compatible")
		}
		grown := Corpus{Cells: map[string]Cell{
			"a": {Certifies: "something", Rule: RuleAdditiveOnly, Lines: []string{"one", "new", "two"}},
		}}
		if findings := Compare(base, grown); len(findings) != 0 {
			t.Fatalf("an inserted line under additive-only was reported as a break: %v", findings)
		}
	})

	t.Run("identical refuses a reorder", func(t *testing.T) {
		reordered := Corpus{Cells: map[string]Cell{
			"a": {Certifies: "something", Rule: RuleIdentical, Lines: []string{"two", "one"}},
		}}
		if findings := Compare(full, reordered); len(findings) == 0 {
			t.Fatal("the same lines in a different order were reported as identical")
		}
	})
}

// TestUpgradingAndInstallingFreshAgreeWithEachOther is the one assertion in
// this package that a careless COMPAT_UPDATE cannot silence.
//
// Every other cell is a comparison against a checked-in baseline, so
// somebody who regenerates the corpus without reading the diff makes the
// question go away. This one compares two things captured in the SAME run:
// the artifact rows and the retention verdicts a fresh install produces,
// against the ones an existing deployment produces after every migration
// has run over its populated tables. FR-35's whole promise is that those
// two are the same deployment as far as an operator can tell, and a
// backfill that touches a row it does not own breaks the equality no
// matter what the corpus says.
func TestUpgradingAndInstallingFreshAgreeWithEachOther(t *testing.T) {
	ctx := context.Background()

	coreRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving the core module root: %v", err)
	}
	current, err := CaptureAll(ctx, t.TempDir(), coreRoot, "testdata/configs")
	if err != nil {
		t.Fatalf("capturing the medium-free surfaces: %v", err)
	}

	pairs := []struct {
		fresh, upgraded, what string
	}{
		{"02-artifact-rows-after-migration", "10-upgraded-artifact-rows",
			"the artifact rows an operator ends up with"},
		{"04-retention-verdicts", "11-upgraded-retention-verdicts",
			"the retention verdicts an operator ends up with"},
	}

	for _, p := range pairs {
		fresh, ok := current.Cells[p.fresh]
		if !ok {
			t.Fatalf("cell %q was not captured", p.fresh)
		}
		upgraded, ok := current.Cells[p.upgraded]
		if !ok {
			t.Fatalf("cell %q was not captured", p.upgraded)
		}
		if len(fresh.Lines) == 0 || len(upgraded.Lines) == 0 {
			t.Fatalf("%s: one side captured nothing (%d fresh, %d upgraded), so this comparison proves nothing",
				p.what, len(fresh.Lines), len(upgraded.Lines))
		}
		if !equalLines(fresh.Lines, upgraded.Lines) {
			t.Errorf("%s differs between a fresh install and an in-place upgrade, which is exactly what FR-35 says cannot happen:\n%s",
				p.what, unifiedish(fresh.Lines, upgraded.Lines))
		}
	}
}
