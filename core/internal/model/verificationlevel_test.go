// Verification levels, tested for the two properties a level is only
// useful if it has: the ladder is ordered, and each rung says honestly what
// it does and does not prove.
//
// The reason this needs tests at all rather than being four constants is
// EPIC K's finding that "structural verification is not restore
// verification". A product that reports "verified" without saying which
// rung it climbed is making a promise it has not tested, and the rung an
// operator reads has to be comparable with the rung they asked for.

package model

import (
	"strings"
	"testing"
)

// TestVerificationLevels_AreAnOrderedLadder is the whole point of the type:
// the four levels are comparable, ascending, and nothing shares a rank.
//
// Without an order there is no way to answer "did this run verify at least
// as much as the policy asked for", which is the only question a caller
// ever has.
func TestVerificationLevels_AreAnOrderedLadder(t *testing.T) {
	t.Parallel()

	if len(VerificationLevels()) != 4 {
		t.Fatalf("VerificationLevels() has %d entries (%v); EPIC K's ladder has four rungs and a fifth is a product decision",
			len(VerificationLevels()), VerificationLevels())
	}

	want := []VerificationLevel{LevelStructural, LevelContentSample, LevelContentFull, LevelRestoreDrill}
	for i, level := range want {
		if VerificationLevels()[i] != level {
			t.Fatalf("VerificationLevels()[%d] = %q, want %q; the slice is the ladder in ascending order", i, VerificationLevels()[i], level)
		}
		if got := level.Rank(); got != i+1 {
			t.Errorf("%q.Rank() = %d, want %d", level, got, i+1)
		}
	}

	if VerificationLevel("restore-drill").Rank() != 0 {
		t.Error("an unrecognised level has a rank, so it would compare as if it were somewhere on the ladder")
	}
}

// TestVerificationLevel_PersistedValues pins the strings themselves,
// because these are not internal names: they are what an operator writes
// under verification_level, what a catalog row stores and what a report
// renders, so changing one is a config migration for every deployment that
// set it.
//
// content_full rather than "full" is the #826 review's finding, and the
// reason is the rung sitting next to it: beside restore_drill, a bare
// "full" reads as "a full restore was done", which is the exact promise
// this ladder exists to stop the product making. The prefix says what it
// actually covers -- the CONTENT was read in full -- and pairs it with
// content_sample, which is the rung it genuinely differs from by degree.
func TestVerificationLevel_PersistedValues(t *testing.T) {
	t.Parallel()

	for level, want := range map[VerificationLevel]string{
		LevelStructural:    "structural",
		LevelContentSample: "content_sample",
		LevelContentFull:   "content_full",
		LevelRestoreDrill:  "restore_drill",
	} {
		if level.String() != want {
			t.Errorf("level renders as %q, want %q; this value is persisted, so a change here is a migration", level.String(), want)
		}

		back, err := ParseVerificationLevel(want)
		if err != nil {
			t.Errorf("ParseVerificationLevel(%q) = %v; a value this product writes must read back", want, err)

			continue
		}
		if back != level {
			t.Errorf("ParseVerificationLevel(%q) = %q, want %q", want, back, level)
		}
	}

	// The old spelling is refused rather than accepted as an alias: no
	// deployment has written one, and an alias would keep both spellings
	// alive in configs and catalog rows forever.
	if _, err := ParseVerificationLevel("full"); err == nil {
		t.Error(`ParseVerificationLevel("full") succeeded; the pre-#826 spelling must not survive as a silent alias`)
	}
}

// TestVerificationLevel_AtLeast is the comparison every caller actually
// makes, including the one an unrecognised value must not win.
func TestVerificationLevel_AtLeast(t *testing.T) {
	t.Parallel()

	if !LevelRestoreDrill.AtLeast(LevelStructural) {
		t.Error("a restore drill does not satisfy a structural requirement, so the ladder is not ordered")
	}
	if LevelStructural.AtLeast(LevelContentFull) {
		t.Error("a structural check satisfies a full-content requirement; that is the promise EPIC K exists to stop this product making")
	}
	if !LevelContentFull.AtLeast(LevelContentFull) {
		t.Error("a level does not satisfy itself")
	}
	if VerificationLevel("thorough").AtLeast(LevelStructural) {
		t.Error("an unrecognised level satisfies the weakest requirement; a typo must never buy a claim")
	}
	if LevelRestoreDrill.AtLeast(VerificationLevel("thorough")) {
		t.Error("the strongest level satisfies a requirement nobody can evaluate; a typo in a policy must not be satisfiable")
	}
}

// TestVerificationLevel_SaysWhatItProves pins the two claims the levels
// exist to keep apart, which is the finding behind them: a structural check
// reads no content, and only a restore drill proves a restore.
func TestVerificationLevel_SaysWhatItProves(t *testing.T) {
	t.Parallel()

	if LevelStructural.ReadsContent() {
		t.Error("the structural level claims to read content; it checks that the repository's own structures resolve, which is exactly the check EPIC K says must not be reported as content verification")
	}
	for _, level := range []VerificationLevel{LevelContentSample, LevelContentFull, LevelRestoreDrill} {
		if !level.ReadsContent() {
			t.Errorf("%q claims to read no content", level)
		}
	}

	for _, level := range []VerificationLevel{LevelStructural, LevelContentSample, LevelContentFull} {
		if level.ProvesRestorable() {
			t.Errorf("%q claims to prove a restore; only an actual restore does, which is why the fourth rung exists", level)
		}
	}
	if !LevelRestoreDrill.ProvesRestorable() {
		t.Error("the restore drill does not claim to prove a restore, which leaves nothing on the ladder that does")
	}

	if VerificationLevel("thorough").ReadsContent() || VerificationLevel("thorough").ProvesRestorable() {
		t.Error("an unrecognised level claims something; an unknown level must claim nothing at all")
	}
}

// TestParseVerificationLevel_RefusesEverythingElse keeps configuration
// honest: a level nobody recognises must not resolve to the cheapest rung
// (which silently downgrades an operator's request) nor to the most
// expensive one (which invents nightly restore drills nobody asked for).
func TestParseVerificationLevel_RefusesEverythingElse(t *testing.T) {
	t.Parallel()

	for _, level := range VerificationLevels() {
		got, err := ParseVerificationLevel(string(level))
		if err != nil {
			t.Errorf("ParseVerificationLevel(%q): %v", level, err)
		}
		if got != level {
			t.Errorf("ParseVerificationLevel(%q) = %q", level, got)
		}
	}

	for _, bad := range []string{
		"",               // silence is the caller's decision to make, not this function's
		"content-sample", // the hyphenated spelling this vocabulary does not use
		"restore-drill",
		"Full",
		"full ",
		"restore",
	} {
		if got, err := ParseVerificationLevel(bad); err == nil {
			t.Errorf("ParseVerificationLevel(%q) = %q with no error; an unrecognised level must be refused rather than resolved to either end of the ladder",
				bad, got)
		}
	}
}

// TestVerificationLevel_Describe keeps every rung renderable, and pins the
// one sentence that has to be right: the weakest rung must say what it does
// not prove, because "verified" on its own is the report this ladder exists
// to stop.
func TestVerificationLevel_Describe(t *testing.T) {
	t.Parallel()

	for _, level := range VerificationLevels() {
		if level.Describe() == "" {
			t.Errorf("level %q has no description, so a surface that lists levels renders a blank row", level)
		}
	}
	if !strings.Contains(LevelStructural.Describe(), "no content") {
		t.Errorf("the structural level's description (%q) does not say that it reads no content", LevelStructural.Describe())
	}
}
