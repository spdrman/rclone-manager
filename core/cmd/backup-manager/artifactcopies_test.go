package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

func localCopyView() app.ArtifactCopy {
	return app.ArtifactCopy{
		Medium:            state.MediumLocal,
		Location:          "/srv/backups/production/postgres/dump.zst",
		Status:            state.PlacementActive,
		VerificationClass: state.VerificationContent,
		Access:            archive.Immediate,
	}
}

func archivedCopyView() app.ArtifactCopy {
	return app.ArtifactCopy{
		Medium:            "cold-store",
		Location:          "prefix/production/postgres/dump.zst",
		Status:            state.PlacementActive,
		VerificationClass: state.VerificationContent,
		StorageClass:      "DEEP_ARCHIVE",
		Access:            archive.RequiresRestore,
		CheckableAs:       "existence",
		RetrievalBilled:   true,
		Detail:            archive.Describe(archive.RequiresRestore, "DEEP_ARCHIVE", nil),
	}
}

// TestAnOrdinaryLocalArtifactPrintsNoCopyBlock is FR-35's compatibility
// promise where an operator would actually notice it breaking.
//
// Every artifact in every deployment that never configured a storage
// medium has exactly one ACTIVE local placement, and local_path is already
// printed three lines up. A copy block for it would be a new paragraph of
// output on every artifact of every existing install, which is precisely
// the "identical CLI output except for additive columns that render only
// when a non-local placement exists" line FR-35 draws.
func TestAnOrdinaryLocalArtifactPrintsNoCopyBlock(t *testing.T) {
	out := captureStdout(t, func() {
		printArtifactCopies([]app.ArtifactCopy{localCopyView()}, time.RFC3339)
	})
	if out != "" {
		t.Fatalf("an ordinary local-only artifact printed a copy block:\n%s", out)
	}
}

// TestAnArchivedCopyTellsATerminalOperatorItCannotBeRead is FR-34's "the
// CLI mirrors the same vocabulary" half.
//
// The specific thing being pinned is that `verified_as: content` and
// `access: requires_restore` appear TOGETHER. Those two lines look
// contradictory at a glance and they are both true: the bytes really were
// verified once, and nobody can read them now. An operator who sees only
// the first is being told a backup is fine in the tone of voice that
// means "and you can have it", which is the false half.
func TestAnArchivedCopyTellsATerminalOperatorItCannotBeRead(t *testing.T) {
	out := captureStdout(t, func() {
		printArtifactCopies([]app.ArtifactCopy{localCopyView(), archivedCopyView()}, time.RFC3339)
	})

	for _, want := range []string{
		"copy:                cold-store",
		"access:            requires_restore",
		"storage_class:     DEEP_ARCHIVE",
		"verified_as:       content",
		"checkable_as:      existence",
		"the provider bills to read this copy back",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the copy block does not contain %q:\n%s", want, out)
		}
	}

	if strings.ContainsAny(strings.SplitN(out, "note:", 2)[0], "%") {
		t.Errorf("the copy block carries a percent sign:\n%s", out)
	}
	if strings.Contains(out, "$") {
		t.Errorf("the copy block quotes a price, and this product holds no price list:\n%s", out)
	}
}

// TestACopyNothingHasVerifiedSaysSoInWords rather than printing an empty
// field, which reads as "not applicable" instead of as "nobody has
// checked".
func TestACopyNothingHasVerifiedSaysSoInWords(t *testing.T) {
	c := archivedCopyView()
	c.VerificationClass = ""
	out := captureStdout(t, func() {
		printArtifactCopies([]app.ArtifactCopy{c}, time.RFC3339)
	})
	if !strings.Contains(out, "nothing has verified this copy") {
		t.Fatalf("a never-verified copy printed nothing about it:\n%s", out)
	}
}

// TestALocalCopyTheJournalNoLongerBelievesInIsPrinted, because one local
// placement that is not ACTIVE is exactly the mid-move state that
// local_path alone cannot describe.
func TestALocalCopyTheJournalNoLongerBelievesInIsPrinted(t *testing.T) {
	c := localCopyView()
	c.Status = state.PlacementDeletePending
	out := captureStdout(t, func() {
		printArtifactCopies([]app.ArtifactCopy{c}, time.RFC3339)
	})
	if !strings.Contains(out, state.PlacementDeletePending) {
		t.Fatalf("a local copy already marked for deletion printed nothing:\n%s", out)
	}
}
