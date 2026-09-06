// Package config_test exists for exactly one check: this package's own
// copy of the archive storage-class set, pinned against internal/archive's
// table, in both directions.
//
// It has to be an external test package. internal/archive imports
// internal/config (its table is keyed by this package's class constants),
// so config cannot import archive back to ask which classes are archive,
// which is why the set is copied here at all. An external test package is
// not part of config, so it may import both and compare them, and that is
// the same arrangement archive/class_test.go already uses to pin its table
// against config.StorageClasses().
package config_test

import (
	"sort"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// TestTheArchiveClassSetMatchesInternalArchive is #442's third
// acceptance line, and both directions cost something different when they
// break.
//
// A class archive calls Archive that this package does not is a retention
// tier that validates and can never take delivery, which is the whole bug
// #442 is about, arriving again through a class nobody added here.
//
// A class this package calls archive that archive's table does not is a
// configuration refused for a reason that is not true: an operator told
// their tier cannot deliver to a class that reads on demand perfectly
// well. GLACIER_IR is the row that makes that a real risk rather than a
// theoretical one.
func TestTheArchiveClassSetMatchesInternalArchive(t *testing.T) {
	fromConfig := config.ArchiveStorageClasses()
	sort.Strings(fromConfig)

	var fromTable []string
	for _, class := range archive.Classes() {
		b, err := archive.Of(class)
		if err != nil {
			t.Fatalf("archive.Of(%q): %v", class, err)
		}
		if b.Archive {
			fromTable = append(fromTable, class)
		}
	}
	sort.Strings(fromTable)

	if len(fromConfig) != len(fromTable) {
		t.Fatalf("the two archive-class sets disagree:\n  config:  %v\n  archive: %v", fromConfig, fromTable)
	}
	for i := range fromTable {
		if fromConfig[i] != fromTable[i] {
			t.Fatalf("the two archive-class sets disagree:\n  config:  %v\n  archive: %v", fromConfig, fromTable)
		}
	}
	if len(fromTable) == 0 {
		t.Fatal("archive's table flags no class as archive at all, so this comparison would pass against an empty set on both sides")
	}
}

// TestEveryClassConfigAcceptsHasAnArchiveVerdict is the completeness half.
//
// The set comparison above says the two lists agree. It says nothing about
// a class that is in neither, which is the shape a new storage class
// arrives in: added to config's closed set, given a row in archive's
// table, and never considered by the pairing rule. Asking archive.IsArchive
// about every class the schema accepts, and requiring the answer to match
// membership of config's set, closes that.
func TestEveryClassConfigAcceptsHasAnArchiveVerdict(t *testing.T) {
	inSet := map[string]bool{}
	for _, class := range config.ArchiveStorageClasses() {
		inSet[class] = true
	}
	accepted := config.StorageClasses()
	if len(accepted) < 3 {
		t.Fatalf("the schema accepts %d storage class(es), which is too few for this to be checking anything", len(accepted))
	}
	for _, class := range accepted {
		if got := archive.IsArchive(class); got != inSet[class] {
			t.Errorf("archive.IsArchive(%q) = %v and config's archive set says %v; a tier's medium on this class would be %s",
				class, got, inSet[class],
				map[bool]string{true: "refused at load and refused again by the engine", false: "accepted at load and refused for ever by the engine"}[inSet[class]])
		}
	}
}
