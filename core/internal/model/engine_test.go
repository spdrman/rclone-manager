// The engine decision, tested from the direction that can actually hurt a
// deployment: what happens to a backup set that never mentioned an engine.
//
// Every set configured before EPIC K is silent about this, and there are
// two of them in every deployment's config file for every one that will
// ever say "kopia". So the load-bearing case here is not parsing the new
// value, it is the old silence continuing to mean exactly what it always
// meant, decided in one named place rather than by a zero value nobody
// wrote down.

package model

import (
	"strings"
	"testing"
)

// TestResolveBackupEngine_SilenceIsTheArtifactEngine is EPIC K's promise to
// every configuration that already exists: an omitted engine key is an
// artifact set, and nothing about an upgrade reinterprets it.
func TestResolveBackupEngine_SilenceIsTheArtifactEngine(t *testing.T) {
	t.Parallel()

	got, err := ResolveBackupEngine("")
	if err != nil {
		t.Fatalf("ResolveBackupEngine(\"\") = error %v; an omitted engine key is what every existing backup set says", err)
	}
	if got != EngineArtifact {
		t.Errorf("ResolveBackupEngine(\"\") = %q, want %q; a set that never heard of engines must keep running the engine it has always run",
			got, EngineArtifact)
	}
}

// TestResolveBackupEngine_NamesBothEngines covers the two values the schema
// accepts, so the case above is a decision about silence rather than a
// resolver that answers "artifact" for everything.
func TestResolveBackupEngine_NamesBothEngines(t *testing.T) {
	t.Parallel()

	for _, want := range BackupEngines() {
		got, err := ResolveBackupEngine(string(want))
		if err != nil {
			t.Errorf("ResolveBackupEngine(%q): %v", want, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveBackupEngine(%q) = %q", want, got)
		}
	}
}

// TestResolveBackupEngine_RefusesAnythingElse is the other half of "never
// silently reinterpret an existing job".
//
// Both directions of a default would be wrong, and the table says which
// mistakes are in scope: a misspelling, a case variant, and a value with
// whitespace around it. None of them may resolve to an engine, because
// reading a typo as the artifact engine silently ignores an operator who
// asked for snapshots, and reading it as the incremental engine silently
// changes what a run does to a source tree.
func TestResolveBackupEngine_RefusesAnythingElse(t *testing.T) {
	t.Parallel()

	for _, declared := range []string{
		"artefact",    // the other spelling, and a real one
		"Artifact",    // configuration vocabulary is lower case
		"KOPIA",       //
		" kopia",      // leading whitespace, as a YAML quote can carry
		"kopia ",      // trailing whitespace
		"incremental", // what the EPIC calls the engine in prose, which is not its identifier
		"rclone",      // the transport, which is not an engine
		"local",       // a repository storage kind, which is not an engine either
	} {
		got, err := ResolveBackupEngine(declared)
		if err == nil {
			t.Errorf("ResolveBackupEngine(%q) = %q with no error; an unrecognised engine must be refused rather than defaulted in either direction",
				declared, got)
			continue
		}
		// The refusal has to say what was written and what is legal, because
		// the operator who typed it is the only person who can fix it.
		msg := err.Error()
		for _, want := range []string{declared, string(EngineArtifact), string(EngineKopia)} {
			if !strings.Contains(msg, want) {
				t.Errorf("ResolveBackupEngine(%q) error %q does not mention %q, so it does not say what was refused or what to write instead",
					declared, msg, want)
			}
		}
	}
}

// TestBackupEngine_UsesRepository pins which engine has a repository, which
// is the fact the configuration layer branches on when it decides whether a
// Repository Domain reference is required or meaningless.
//
// The unknown-engine arm is the one worth having: an engine this package
// has not been taught must not be treated as having a repository, because
// that would invent a security boundary for something nobody modelled.
func TestBackupEngine_UsesRepository(t *testing.T) {
	t.Parallel()

	if EngineArtifact.UsesRepository() {
		t.Error("the artifact engine reports a repository; it copies artifacts to durable storage and has no repository to share with anything")
	}
	if !EngineKopia.UsesRepository() {
		t.Error("the incremental engine reports no repository; a set running it must name the Repository Domain whose boundaries it shares")
	}
	if BackupEngine("something-else").UsesRepository() {
		t.Error("an unrecognised engine reports a repository, which would invent a security boundary for an engine nobody modelled")
	}
}

// TestBackupEngines_IsTheWholeSet guards the count, because a third engine
// is not a constant: it is a lifecycle, a catalog shape, a verification
// story and a wizard page, and it should not arrive as a one-line edit.
func TestBackupEngines_IsTheWholeSet(t *testing.T) {
	t.Parallel()

	if len(BackupEngines()) != 2 {
		t.Fatalf("BackupEngines() has %d entries (%v); a third engine is an architectural decision, not a constant", len(BackupEngines()), BackupEngines())
	}
	if BackupEngines()[0] != EngineArtifact {
		t.Errorf("BackupEngines()[0] = %q; the engine every deployment already runs is offered first", BackupEngines()[0])
	}
}

// TestClosedVocabularies_HandOutCopies covers all four of this package's
// EPIC K vocabularies in one place, because the hazard is the same one
// four times and it is not a style point.
//
// Each of these lists is the vocabulary a Parse function accepts and a
// surface renders. Exported as a slice VARIABLE, any caller in the process
// -- a handler, a test, a wizard -- can assign through it, and the next
// ParseVerificationLevel or ParseRepositoryIsolation in any goroutine
// accepts a value this package never defined, or refuses one it does. That
// is a closed set that is not closed.
//
// The accessor form is already this repository's convention for exactly
// this reason: backend.CapabilityKeys() hands out a slices.Clone.
func TestClosedVocabularies_HandOutCopies(t *testing.T) {
	t.Parallel()

	if got := BackupEngines(); len(got) > 0 {
		got[0] = BackupEngine("smuggled")
		if BackupEngines()[0] == BackupEngine("smuggled") {
			t.Error("BackupEngines() hands out the package's own slice, so a caller can redefine which engines exist")
		}
	}

	if got := VerificationLevels(); len(got) > 0 {
		got[0] = VerificationLevel("smuggled")
		if VerificationLevels()[0] == VerificationLevel("smuggled") {
			t.Error("VerificationLevels() hands out the package's own slice, so a caller can put a rung on the ladder that nothing implements")
		}
		if _, err := ParseVerificationLevel("smuggled"); err == nil {
			t.Error("ParseVerificationLevel accepted a level written into the vocabulary from outside")
		}
	}

	if got := RepositoryIsolations(); len(got) > 0 {
		got[0] = RepositoryIsolation("smuggled")
		if RepositoryIsolations()[0] == RepositoryIsolation("smuggled") {
			t.Error("RepositoryIsolations() hands out the package's own slice, so a caller can invent a third answer to a security question")
		}
		if _, err := ParseRepositoryIsolation("smuggled"); err == nil {
			t.Error("ParseRepositoryIsolation accepted an isolation written into the vocabulary from outside")
		}
	}

	if got := RepositoryBoundaries(); len(got) > 0 {
		got[0] = RepositoryBoundary("smuggled")
		if RepositoryBoundaries()[0] == RepositoryBoundary("smuggled") {
			t.Error("RepositoryBoundaries() hands out the package's own slice, so a caller can drop a boundary out of the list co-tenancy is refused against")
		}
	}
}

// TestBackupEngine_Describe keeps every engine renderable, since the wizard
// and the run report are both renderings of these sentences and an engine
// without one shows up as a blank row.
func TestBackupEngine_Describe(t *testing.T) {
	t.Parallel()

	for _, e := range BackupEngines() {
		if desc := e.Describe(); desc == "" {
			t.Errorf("engine %q has no description, so a surface that lists engines renders a blank row for it", e)
		}
	}
	if !strings.Contains(EngineKopia.Describe(), "never deletes") {
		t.Errorf("the incremental engine's description (%q) does not say that a snapshot deletes nothing on the source, "+
			"which is the difference between the two engines an operator most needs to read", EngineKopia.Describe())
	}
}
