package reconcile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// FR-29's sweep, at the site where getting it wrong costs the most: this
// is the check FR-17 uses to decide whether a local copy is still good,
// and the answer feeds decisions that end in a remote delete.
//
// It asks the placement now, not Record.LocalPath. The two say the same
// thing for every artifact in every deployment today, which is why the
// change lands behaviour-neutral. This pins what happens when they stop:
// an artifact whose local copy has been retired must read as invalid, and
// it must read that way even though the file at its old landing path is
// still sitting there, because that file is no longer this artifact's
// copy as far as the journal is concerned.
func TestCheckLocalFinalAsksThePlacementAndNotTheLandingPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.dump")
	payload := []byte("payload")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	set, err := model.NewBackupSetID("nas", "daily")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	size := int64(len(payload))
	rec := state.Record{
		Artifact:     artifact,
		State:        string(lifecycle.Complete),
		LocalPath:    path,
		DiscoveredAt: time.Unix(0, 0).UTC(),
		UpdatedAt:    time.Unix(0, 0).UTC(),
		Transfer:     &state.TransferResult{BytesTransferred: size},
		Placements: []state.Placement{{
			Medium:   state.MediumLocal,
			Location: path,
			Status:   state.PlacementActive,
		}},
	}

	// The ordinary case, and the control: an ACTIVE local placement
	// pointing at a file that is really there reads as valid.
	if got := checkLocalFinal(rec); !got.Valid {
		t.Fatalf("an artifact with an active local placement read as invalid: %s", got.Reason)
	}

	// Retire the local copy the way the move engine will, leaving the file
	// itself untouched on disk.
	rec.Placements[0].Status = state.PlacementGone
	got := checkLocalFinal(rec)
	if got.Valid {
		t.Fatal("an artifact whose local placement is GONE read as having a valid local copy, purely because a file is still sitting at its old landing path")
	}
	if !strings.Contains(got.Reason, "no local final path") {
		t.Errorf("reason = %q, want it to say there is no recorded local final path", got.Reason)
	}
	if rec.LocalPath != path {
		t.Errorf("LocalPath = %q, want the landing path %q to survive unchanged: it is a historical fact, not a promise", rec.LocalPath, path)
	}
}
