package revalidate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Revalidation re-reads an artifact off local disk and re-hashes it. FR-29
// makes it ask the placement where that copy is rather than assuming
// Record.LocalPath still points at one.
//
// The failure this rules out is specific and quiet: after a move, the
// landing path can hold something else entirely, and a pass that re-hashed
// whatever is at that name would report a hash mismatch and quarantine a
// perfectly good artifact whose bytes are safe on a medium.
func TestRunChecksAsksThePlacementForTheCopyToReRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.dump")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// sha256 of "payload"
	const sum = "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5"

	rec := state.Record{
		LocalPath:    path,
		LocalHash:    sum,
		LocalHashAlg: string(transport.SHA256),
		Placements: []state.Placement{{
			Medium:   state.MediumLocal,
			Location: path,
			Status:   state.PlacementActive,
		}},
	}
	cfg := config.Revalidation{Hash: true}

	checked, passed, reason, err := runChecks(context.Background(), cfg, rec)
	if err != nil {
		t.Fatalf("runChecks: %v", err)
	}
	if !checked || !passed {
		t.Fatalf("an artifact with an active local placement did not pass: checked=%v passed=%v reason=%q", checked, passed, reason)
	}

	rec.Placements[0].Status = state.PlacementGone
	checked, passed, reason, err = runChecks(context.Background(), cfg, rec)
	if err != nil {
		t.Fatalf("runChecks: %v", err)
	}
	if passed {
		t.Fatal("an artifact whose local placement is GONE passed revalidation off a file the journal no longer claims")
	}
	if !checked {
		t.Error("checked = false: the pass did look, and reporting otherwise would leave the due-ness clock unmoved for a different reason than the one that applies")
	}
	if !strings.Contains(reason, "no active local copy") {
		t.Errorf("reason = %q, want it to say there is no active local copy to re-read", reason)
	}
}
