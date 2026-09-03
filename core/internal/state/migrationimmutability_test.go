package state

import "testing"

// Once a migration has been applied by a deployment, its FILE is frozen, and
// that includes its comments.
//
// applyMigration records the sha256 of the file it ran (loadMigrations
// computes it over the whole file, comments included), and migrate compares
// that recorded value against this binary's copy for every already-applied
// version before it does anything else. A mismatch is ErrSchemaDrift and Open
// refuses. So editing a landed migration, even to correct a comment that is
// now wrong, stops every existing deployment from starting. It is not a
// cosmetic change and there is no way to make it one.
//
// TestMigrate_RefusesChecksumDrift is the proof that this consequence is real:
// it corrupts a recorded checksum and watches Open refuse. This guard is the
// other half, the one that stops the file drifting in the first place.
//
// That is not hypothetical. Issue #396 was a real upgrade failure, and one of
// its acceptance criteria asked for exactly this: correct the false "foreign
// keys are not enabled on this connection" comments in 0002 and 0006. Doing
// that would have traded one broken upgrade for another, because 0002's
// checksum moves from 4effe022 to something else and every journal that has
// already applied version 2 then refuses to open. This table is what makes
// that mistake fail here, in a second, instead of on somebody's NAS.
//
// Adding a migration means adding a row here, deliberately. Changing a row
// that is already here means you are about to break existing installs, and
// the answer is almost always a new migration instead.
var shippedMigrationChecksums = map[int]string{
	1: "44dfe54c7a021952b2f8b6ba9c736e3ae447fa7162b5534eae9609da0b111695",
	2: "4effe0227c768eb93fde97c1d860707bd948f2cb9fc517baf476b4bb70119001",
	3: "fd12c692e9e7ff6018e44025080e3758eb4b1742d50a8fede9108f27cebb59d8",
	4: "392aba9380fa7378fedc0be333dfec6caa6cd383e896cbdd5287280d29acd3a9",
	5: "292ef23d06c2587915cf22b811e172403e268122833e125af30165a88251cad1",
	6: "f19fa502b3082e96ae36b9f581f16fbe520616ae80ad9b797a48b4c3673da597",
	7: "f9ed92b4c9412c41cae7e596f6cce2291f55743e0b0e784fd3a9952511c5d0ff",
}

const driftConsequence = "a deployment that already applied this version compares the recorded " +
	"checksum against this file, finds they differ, and refuses to open its journal with " +
	"ErrSchemaDrift. Correcting a comment is not free here. If the file says something that is " +
	"no longer true, say so in migrate.go or in a new migration, and leave this one alone."

func TestShippedMigrationsAreImmutable(t *testing.T) {
	known, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}

	// The anti-vacuity half. A table that has quietly emptied, or a
	// loadMigrations that has quietly stopped returning anything, would let
	// every assertion below pass without comparing a single checksum.
	if len(known) == 0 {
		t.Fatal("loadMigrations returned nothing, so this guard compared no checksums at all")
	}
	if len(shippedMigrationChecksums) == 0 {
		t.Fatal("the checksum table is empty, so this guard compared no checksums at all")
	}

	onDisk := make(map[int]migration, len(known))
	for _, m := range known {
		onDisk[m.version] = m
	}

	for _, m := range known {
		want, ok := shippedMigrationChecksums[m.version]
		if !ok {
			t.Errorf("migration %s is in the tree with no row in shippedMigrationChecksums.\n"+
				"Add:\n\t%d: %q,\n"+
				"and understand what you are adding: once a release carrying this file has been "+
				"applied anywhere, the file can never change again.", m.filename, m.version, m.checksum)
			continue
		}
		if m.checksum != want {
			t.Errorf("migration %s has changed.\n  recorded: %s\n  on disk:  %s\n%s",
				m.filename, want, m.checksum, driftConsequence)
		}
	}

	for version := range shippedMigrationChecksums {
		if _, ok := onDisk[version]; !ok {
			t.Errorf("shippedMigrationChecksums has a row for version %d and no such migration is in "+
				"the tree. A shipped migration cannot be withdrawn: a journal that applied it still "+
				"records it, and migrate refuses a version it does not recognise with "+
				"ErrUnknownSchemaVersion", version)
		}
	}
}
