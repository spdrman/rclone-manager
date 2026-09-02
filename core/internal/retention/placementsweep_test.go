package retention

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// FR-20's prune is the one function in this product that deletes a
// customer's backup, and the check that keeps it honest is "the file I am
// about to remove is the one the journal knows this artifact as". FR-29
// moves that identification onto the placement.
//
// The direction of the change is what matters. An artifact whose bytes
// have moved to a medium has no ACTIVE local placement, so the comparison
// is against an empty path, does not match, and prune refuses. Comparing
// Record.LocalPath instead would match, because LocalPath is the landing
// path and goes on being true after the file is gone, and prune would then
// delete whatever had since taken that name.
func TestPruneRefusesAnArtifactWhoseLocalCopyHasBeenRetired(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "prune-placement", "set")
	artifact := gfsMustArtifact(t, set, "expired.zst")
	final := filepath.Join(root, "expired.zst")
	if err := os.WriteFile(final, []byte("something else entirely"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Old enough that no tier keeps it, and last-known-good protection
	// off, so the only thing standing between this artifact and a delete
	// is the identification check.
	discovered := pruneNow.Add(-365 * 24 * time.Hour)
	now := pruneNow
	resolved := pruneTodayOnlyChain()
	bs := pruneBackupSet(set, root)

	// The control first: with an ACTIVE local placement, this artifact is
	// a genuine delete candidate, so the refusal below is about the
	// placement and not about the fixture being un-deletable.
	rec := pruneRecord(artifact, lifecycle.Complete, discovered, final)
	verdicts, err := PruneDecide(now, resolved, bs, []state.Record{rec})
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	if len(verdicts) != 1 || verdicts[0].Action != PruneDelete {
		t.Fatalf("with an active local placement: %+v, want a single DELETE", verdicts)
	}

	// Now retire the local copy, leaving the file on disk.
	rec.Placements[0].Status = state.PlacementGone
	verdicts, err = PruneDecide(now, resolved, bs, []state.Record{rec})
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("got %d verdicts, want 1", len(verdicts))
	}
	if verdicts[0].Action != PruneRefuse {
		t.Fatalf("Action = %s, want REFUSE: nothing may delete a file the journal no longer records a local copy at", verdicts[0].Action)
	}
	if !strings.Contains(verdicts[0].Reason, "refusing to guess") {
		t.Errorf("Reason = %q, want the refusal to say it will not guess", verdicts[0].Reason)
	}

	// And PruneApply, which is the one that actually removes files, agrees
	// and leaves the file exactly where it is.
	applied, err := PruneApply(now, resolved, bs, []state.Record{rec})
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if len(applied) != 1 || applied[0].Action != PruneRefuse {
		t.Fatalf("PruneApply = %+v, want a single REFUSE", applied)
	}
	if _, err := os.Lstat(final); err != nil {
		t.Fatalf("the file at the old landing path was removed anyway: %v", err)
	}
}
