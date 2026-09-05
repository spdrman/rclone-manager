package archive

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// This file is the class table's suite, and every test in it is about a
// way the table can be wrong while still compiling.
//
// It can describe a class an operator cannot configure, or miss one they
// can, which is the drift the first test pins in both directions. It can
// get a row's Archive flag wrong, which is checked as an exact set over
// the whole table rather than as two assertions, with GLACIER_IR pulled
// out on its own because the word Glacier in the name is the trap somebody
// will fall into while tidying. It can carry a restore wait for a class
// that needs none, or fail to carry one for a class that needs one, which
// is a per-row consistency the last test walks. And it can be asked about
// a class it has never heard of, where the only safe answer is the one
// that refuses to read.

// TestClassTableAndConfigAgree pins this package's table against the closed
// set internal/config validates against, in BOTH directions.
//
// A class config accepts with no row here would reach Of and be refused at
// runtime, on a deployment whose config.yaml validated cleanly. A row here
// for a class config refuses is a rule about something no operator can
// configure, which is harmless but is also how a table starts describing a
// product that no longer exists. #241's REFACTOR step asks for exactly one
// table of storage-class knowledge; this is the test that keeps it one.
func TestClassTableAndConfigAgree(t *testing.T) {
	fromConfig := config.StorageClasses()
	sort.Strings(fromConfig)
	fromTable := Classes()

	if !reflect.DeepEqual(fromConfig, fromTable) {
		t.Fatalf("the class table and config's closed set disagree:\n  config: %v\n  table:  %v", fromConfig, fromTable)
	}
}

// TestArchiveIsExactlyGlacierAndDeepArchive pins FR-31's own list.
//
// The FR names two classes and only two, and it is worth pinning rather
// than trusting, because both directions of getting this wrong are
// expensive. Marking a class archive that is not makes every content
// verification of it refuse, so an operator's backups quietly stop being
// checked. Failing to mark one that is means the product believes it can
// read bytes it cannot.
func TestArchiveIsExactlyGlacierAndDeepArchive(t *testing.T) {
	want := map[string]bool{
		config.StorageClassGlacier:     true,
		config.StorageClassDeepArchive: true,
	}
	for _, class := range Classes() {
		b, err := Of(class)
		if err != nil {
			t.Fatalf("Of(%q): %v", class, err)
		}
		if b.Archive != want[class] {
			t.Errorf("Of(%q).Archive = %v, want %v", class, b.Archive, want[class])
		}
	}
}

// TestGlacierInstantRetrievalIsNotArchive is the name trap, on its own,
// because it is the one somebody will get wrong while tidying.
//
// GLACIER_IR reads on demand. A GET of it returns bytes. Treating it as
// archive would refuse every content verification of a class that is
// perfectly capable of one, which is exactly the "silently stops checking
// your backups" failure this package is supposed to prevent rather than
// cause.
func TestGlacierInstantRetrievalIsNotArchive(t *testing.T) {
	if IsArchive(config.StorageClassGlacierIR) {
		t.Fatal("GLACIER_IR reads on demand, and treating it as archive stops its content ever being verified")
	}
	b, err := Of(config.StorageClassGlacierIR)
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if b.RestoreWait != "" {
		t.Errorf("RestoreWait = %q, want empty for a class that needs no restore", b.RestoreWait)
	}
	if !b.RetrievalBilled {
		t.Error("RetrievalBilled = false; the provider does bill to read GLACIER_IR back, and the operator has to be told that even though no restore is involved")
	}
}

// TestUnknownClassIsNeverTreatedAsReadable is the safety direction of the
// two answers Of and IsArchive give for a class nobody listed.
//
// Of refuses, because its caller is asking a question and can be told
// there is no answer. IsArchive says true, because its caller is in the
// middle of a decision and has to get something back, and "you cannot
// read this" is the answer whose cost when wrong is a refusal rather than
// a deleted copy.
func TestUnknownClassIsNeverTreatedAsReadable(t *testing.T) {
	if _, err := Of("GLACIER_DEEP_FLEXIBLE_INSTANT"); !errors.Is(err, ErrUnknownClass) {
		t.Fatalf("Of(unknown) error = %v, want ErrUnknownClass", err)
	}
	if !IsArchive("GLACIER_DEEP_FLEXIBLE_INSTANT") {
		t.Fatal("IsArchive(unknown) = false; a class nothing knows about must never read as one whose bytes can be fetched")
	}
}

// TestEmptyClassIsStandard mirrors config.StorageMedium's own default
// resolution, so a transport.Medium built from a medium that named no
// class behaves here exactly as it behaves there.
func TestEmptyClassIsStandard(t *testing.T) {
	b, err := Of("")
	if err != nil {
		t.Fatalf("Of(\"\"): %v", err)
	}
	if b.Class != config.StorageClassStandard {
		t.Fatalf("Of(\"\").Class = %q, want %q", b.Class, config.StorageClassStandard)
	}
	if IsArchive("") {
		t.Fatal("the default class is STANDARD, which is not archive")
	}
}

// TestOnlyArchiveClassesCarryARestoreWait pins the pairing rather than
// each row: a class that needs a restore has to be able to say how long
// one takes, and a class that needs none must not carry a sentence about
// waiting for one.
func TestOnlyArchiveClassesCarryARestoreWait(t *testing.T) {
	for _, class := range Classes() {
		b, err := Of(class)
		if err != nil {
			t.Fatalf("Of(%q): %v", class, err)
		}
		if b.Archive && b.RestoreWait == "" {
			t.Errorf("%s is archive and says nothing about how long a restore takes", class)
		}
		if !b.Archive && b.RestoreWait != "" {
			t.Errorf("%s needs no restore but carries a restore wait: %q", class, b.RestoreWait)
		}
		if b.Archive && !b.RetrievalBilled {
			t.Errorf("%s is archive and does not say retrieval is billed", class)
		}
	}
}
