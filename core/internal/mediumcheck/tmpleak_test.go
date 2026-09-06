package mediumcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
)

// TestPreflight_LeavesNothingBehindOnThisMachineEither is the local half
// of "roll the probe back". The remote half is asserted elsewhere; this
// one is about the staging copy, because a preflight an operator wires
// into a deployment script runs over and over and one leaked temporary
// directory per run is a filesystem that eventually runs out of inodes.
func TestPreflight_LeavesNothingBehindOnThisMachineEither(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	store := newFakeStore()
	store.storedClass = config.StorageClassStandardIA
	if report := preflight(t, store, testMedium(), placement.Content); !report.OK {
		t.Fatalf("report is not OK: %+v", report.Failures())
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "rclone-manager-preflight-") {
			t.Fatalf("the preflight left %s behind under %s", filepath.Join(tmp, e.Name()), tmp)
		}
	}
}
