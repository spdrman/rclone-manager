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
// COMPAT_UPDATE rewrites the corpus instead of comparing against it. That
// is the only way to change a pinned line, and it is deliberately an
// environment variable and not a flag: it has to be something a person
// types on purpose, with the reason going into the commit message, rather
// than something a gate could ever set for itself. Naming a cell rewrites
// that cell alone; COMPAT_UPDATE=1 sweeps all of them, and writeCorpus
// below argues why the scoped form is the one to reach for.
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

	// "0" and the empty string are both "not an update", so an environment
	// that carries COMPAT_UPDATE=0 to mean off gets what it meant rather
	// than a refusal about a cell named 0.
	if spec := os.Getenv("COMPAT_UPDATE"); spec != "" && spec != "0" {
		writeCorpus(t, spec, current)
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

If the change is genuinely intended, re-capture the cell that moved, by name:
  COMPAT_UPDATE=<cell-name> go test ./tests/compat/
Several at once are a comma-separated list. Say in the commit message which
surface moved and why an operator upgrading into it is not surprised.

COMPAT_UPDATE=1 sweeps every cell instead. It is there for a first capture and
for a change that genuinely moves several surfaces at once, and what it costs is
that it also brings back whatever has drifted in the surfaces you did not touch,
inside a commit that says it is about yours. If you reach for it, read the whole
diff and write down what each hunk is.`

// writeCorpus is everything COMPAT_UPDATE does.
//
// The scoped path is the interesting one. It loads the corpus that is
// already checked in, replaces only the cells the operator named, and
// writes that back, so the cells nobody named keep the exact lines they had
// even though this run captured fresh ones for all of them. A change about
// CLI wording then cannot move an API contract line, whatever the API
// contract happens to be doing on that branch, and a reviewer reading the
// diff is reading the cells the commit message claims (#549).
//
// Every refusal in here is a t.Fatal. A re-capture that silently wrote
// nothing, or wrote more than was asked for, is the one outcome that is
// worse than a red gate: the red gate is at least honest about what it
// knows, while a person who believes they re-captured stops looking.
func writeCorpus(t *testing.T, spec string, current Corpus) {
	t.Helper()

	all, cells, err := ParseUpdateRequest(spec)
	if err != nil {
		t.Fatalf("%v.\n\nThe cells this run captured are:\n  %s", err, strings.Join(cellNames(current), "\n  "))
	}

	if all {
		if err := current.Save(CorpusPath); err != nil {
			t.Fatalf("writing %s: %v", CorpusPath, err)
		}
		t.Logf("COMPAT_UPDATE=1: swept all %d cells of %s. Every line that changed is a behavior change somebody has to justify in the commit message, and that includes the ones your change was not about: a sweep re-captures every surface at once. Read the whole diff.\n\nTo move one surface and leave the rest at the lines they were checked in with, name it instead:\n  COMPAT_UPDATE=<cell-name> go test ./tests/compat/\nThe cells are:\n  %s",
			len(current.Cells), CorpusPath, strings.Join(cellNames(current), "\n  "))
		return
	}

	baseline, err := LoadCorpus(CorpusPath)
	if err != nil {
		t.Fatalf("a scoped re-capture writes the named cell(s) back into the corpus that is already there, and %s could not be read: %v\n\nOn a fresh checkout with no corpus yet there is nothing to scope against, so capture the first one with COMPAT_UPDATE=1.", CorpusPath, err)
	}

	merged, err := MergeCells(baseline, current, cells)
	if err != nil {
		t.Fatalf("COMPAT_UPDATE=%s: %v", spec, err)
	}
	if err := merged.Save(CorpusPath); err != nil {
		t.Fatalf("writing %s: %v", CorpusPath, err)
	}
	t.Logf("COMPAT_UPDATE=%s: re-captured %d of the %d cells in %s and left the other %d at the lines they were checked in with. Say in the commit message which surface moved and why an operator upgrading into it is not surprised.",
		spec, len(cells), len(merged.Cells), CorpusPath, len(merged.Cells)-len(cells))
}

// cellNames is sortedKeys with a name that reads at a call site, and it is
// here rather than beside sortedKeys because the only thing that wants it
// is a failure message.
func cellNames(c Corpus) []string {
	return sortedKeys(c.Cells)
}

// TestScopedRecaptureWritesOnlyWhatItWasAskedFor pins the half of #549 that
// is not the guard: a re-capture aimed at one cell must not be able to move
// another.
//
// These are unit assertions over the pure half of the package, deliberately.
// The end-to-end version would have to run a real COMPAT_UPDATE against the
// checked-in file, which means either mutating the tree under the gate or
// building a second corpus to point it at, and neither of those tells you
// anything the two functions below do not.
func TestScopedRecaptureWritesOnlyWhatItWasAskedFor(t *testing.T) {
	baseline := Corpus{Cells: map[string]Cell{
		"cli":      {Certifies: "the terminal", Rule: RuleIdentical, Lines: []string{"old cli"}},
		"contract": {Certifies: "the API", Rule: RuleAdditiveOnly, Lines: []string{"old contract"}},
	}}
	current := Corpus{Cells: map[string]Cell{
		"cli":      {Certifies: "the terminal", Rule: RuleIdentical, Lines: []string{"new cli"}},
		"contract": {Certifies: "the API", Rule: RuleAdditiveOnly, Lines: []string{"new contract"}},
	}}

	t.Run("the named cell moves and the others do not", func(t *testing.T) {
		merged, err := MergeCells(baseline, current, []string{"cli"})
		if err != nil {
			t.Fatalf("MergeCells: %v", err)
		}
		if got := merged.Cells["cli"].Lines[0]; got != "new cli" {
			t.Errorf("the cell that was named still reads %q, so the re-capture did nothing", got)
		}
		if got := merged.Cells["contract"].Lines[0]; got != "old contract" {
			t.Errorf("a cell nobody named now reads %q. That is the sweep this whole mechanism exists to avoid: a change about one surface quietly re-capturing another.", got)
		}
	})

	t.Run("a cell this run did not capture is refused", func(t *testing.T) {
		if _, err := MergeCells(baseline, current, []string{"cli", "no-such-cell"}); err == nil {
			t.Fatal("a re-capture naming a cell that does not exist was accepted, so a typo writes nothing and reads as done")
		}
	})

	t.Run("a cell only the baseline has is carried through", func(t *testing.T) {
		thin := Corpus{Cells: map[string]Cell{"cli": current.Cells["cli"]}}
		merged, err := MergeCells(baseline, thin, []string{"cli"})
		if err != nil {
			t.Fatalf("MergeCells: %v", err)
		}
		if _, ok := merged.Cells["contract"]; !ok {
			t.Error("a scoped re-capture dropped a cell it was not asked about, so the gate got smaller without anybody saying so")
		}
	})

	t.Run("naming nothing is refused", func(t *testing.T) {
		if _, err := MergeCells(baseline, current, nil); err == nil {
			t.Fatal("a scoped re-capture with no cells named was accepted")
		}
	})
}

// TestParseUpdateRequestKeepsTheSweepAndTheScopeApart is the other half:
// whatever COMPAT_UPDATE is set to, it means one thing or it is refused.
func TestParseUpdateRequestKeepsTheSweepAndTheScopeApart(t *testing.T) {
	all, cells, err := ParseUpdateRequest("1")
	if err != nil || !all || len(cells) != 0 {
		t.Errorf(`ParseUpdateRequest("1") = (%v, %v, %v), want the whole-corpus sweep. scripts/compat/selftest.sh drives that form, and so does a first capture.`, all, cells, err)
	}

	all, cells, err = ParseUpdateRequest("06b-cli-usage-block")
	if err != nil || all || len(cells) != 1 || cells[0] != "06b-cli-usage-block" {
		t.Errorf(`ParseUpdateRequest("06b-cli-usage-block") = (%v, %v, %v), want that one cell`, all, cells, err)
	}

	all, cells, err = ParseUpdateRequest("06b-cli-usage-block, 08-api-contract-promises ,06b-cli-usage-block")
	if err != nil || all || len(cells) != 2 {
		t.Errorf("a comma-separated list with spacing and a repeat = (%v, %v, %v), want the two distinct cells", all, cells, err)
	}

	for _, spec := range []string{"", "06b-cli-usage-block,", ",", "a,,b"} {
		if _, _, err := ParseUpdateRequest(spec); err == nil {
			t.Errorf("ParseUpdateRequest(%q) was accepted. A spec with a hole in it has to be refused rather than guessed at, or somebody re-captures fewer cells than they typed and never finds out.", spec)
		}
	}
}

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

	// Certifies is the sentence printed at whoever meets the red cell, and
	// it is the only thing telling them whether the lines that moved
	// matter. A cell whose claim changed while its lines did not is a cell
	// now promising something nobody checked, so it goes red like any
	// other drift. #560 reworded one of these, and nothing noticed.
	t.Run("a claim that drifted", func(t *testing.T) {
		reworded := Corpus{Cells: map[string]Cell{
			"a": {Certifies: "something else entirely", Rule: RuleIdentical, Lines: []string{"one", "two"}},
		}}
		if findings := Compare(full, reworded); len(findings) == 0 {
			t.Fatal("a cell whose certification sentence changed was reported as compatible, so the sentence a reader is judged against can drift from the code with nothing going red")
		}
	})
}

// TestSaveRoundTripsWhatItWasGiven covers the two ways this file's writer
// used to disagree with its own promises.
//
// The note first. MergeCells hands Save a corpus carrying the checked-in
// note, on the stated promise that a scoped re-capture leaves everything it
// was not asked about exactly as it was, and Save used to overwrite that
// field unconditionally, which made the promise decoration. It now defaults
// the note and never replaces one.
//
// And the escaping. encoding/json escapes < > & by default, so a note
// containing a literal <cell-name> came back as \u003ccell-name\u003e.
// Nothing went red, because the comparison is on the decoded string, and
// every re-capture after it carried a one-line diff nobody asked for, which
// is the class of thing #549 was filed about.
func TestSaveRoundTripsWhatItWasGiven(t *testing.T) {
	cells := map[string]Cell{"a": {Certifies: "something", Rule: RuleIdentical, Lines: []string{"one"}}}

	t.Run("a note the caller set survives", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "corpus.json")
		if err := (Corpus{Note: "carried through from the baseline", Cells: cells}).Save(path); err != nil {
			t.Fatalf("saving: %v", err)
		}
		back, err := LoadCorpus(path)
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		if back.Note != "carried through from the baseline" {
			t.Errorf("Save replaced the note it was given with %q, so a scoped re-capture cannot leave the file's own note alone", back.Note)
		}
	})

	t.Run("no note gets the standing one", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "corpus.json")
		if err := (Corpus{Cells: cells}).Save(path); err != nil {
			t.Fatalf("saving: %v", err)
		}
		back, err := LoadCorpus(path)
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		if back.Note != corpusNote {
			t.Errorf("a corpus saved with no note of its own came back with %q, and a file nobody can read the rule off is how a red build turns into a regenerate", back.Note)
		}
	})

	t.Run("angle brackets are written as themselves", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "corpus.json")
		if err := (Corpus{Note: "run COMPAT_UPDATE=<cell-name> & read the diff", Cells: cells}).Save(path); err != nil {
			t.Fatalf("saving: %v", err)
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading it back: %v", err)
		}
		if !strings.Contains(string(blob), "COMPAT_UPDATE=<cell-name> & read the diff") {
			t.Errorf("the bytes on disk escaped what they were given, so every re-capture carries a diff nobody asked for:\n%s", blob)
		}
	})
}

// TestNormalizeEventTimeTakesTheClockAndNothingElse is the control for the
// third normalization this package does.
//
// A normalization is the one thing in here that can hide a real change, so
// each of the three is narrow on purpose and this one gets driven rather
// than read: it has to take the wall clock out of an FR-23 event line and
// leave every other field on it, including the two that a corpus is
// supposed to go red over, the build's version and the embedded rclone's.
func TestNormalizeEventTimeTakesTheClockAndNothingElse(t *testing.T) {
	const line = `{"time":"2026-09-06T19:49:05.212202Z","level":"INFO","msg":"backup-manager starting","event":"startup","version":"dev","commit":"none","go_version":"go1.24.0"}`

	got := normalizeEventTime(line)
	if strings.Contains(got, "2026-09-06T19:49:05") {
		t.Errorf("the clock survived, so this cell is red on every run:\n%s", got)
	}
	for _, keep := range []string{`"level":"INFO"`, `"event":"startup"`, `"version":"dev"`, `"commit":"none"`, `"go_version":"go1.24.0"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("normalizing the clock also took %s, and a normalization that takes more than the machine's fact is a place a real change hides:\n%s", keep, got)
		}
	}

	// A timestamp that is not the event line's own field is left alone: a
	// product that started printing one in a message would be a change
	// this corpus has to notice.
	const inProse = `  err| backup-manager: last run at 2026-09-06T19:49:05Z did not finish`
	if normalizeEventTime(inProse) != inProse {
		t.Errorf("a timestamp in an ordinary sentence was normalized away:\n%s", normalizeEventTime(inProse))
	}
}

// TestTheCheckedInNoteIsTheStandingOne stops the note the file carries
// drifting away from the constant now that Save no longer rewrites it on
// every write.
//
// That is the cost of making MergeCells's promise real, and it is worth
// paying with a control rather than with a rule nobody enforces: the note
// is the first thing somebody meeting a red cell reads, and one that has
// gone stale sends them to run a command that is no longer the command.
func TestTheCheckedInNoteIsTheStandingOne(t *testing.T) {
	baseline, err := LoadCorpus(CorpusPath)
	if err != nil {
		t.Fatalf("reading %s: %v", CorpusPath, err)
	}
	if baseline.Note != corpusNote {
		t.Errorf("the corpus file's note has drifted from corpusNote, so it tells the next person to meet a red cell something the tooling no longer does.\n file: %q\n code: %q", baseline.Note, corpusNote)
	}
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
