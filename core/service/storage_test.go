package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

func TestListStorageStatus_ReportsAssessmentForEachConfiguredBackupSet(t *testing.T) {
	localDir := t.TempDir()
	svc := New(testConfig(config.Source{
		Name: "alpha",
		BackupSets: []config.BackupSet{{
			Name:       "one",
			Remote:     config.Remote{Type: "local"},
			RemotePath: t.TempDir(),
			LocalPath:  localDir,
			Completion: config.Completion{Strategy: "rename"},
			StaleAfter: config.Duration(24 * time.Hour),
		}},
	}), openTestJournal(t), nil, nil)

	statuses, err := svc.ListStorageStatus(context.Background())
	if err != nil {
		t.Fatalf("ListStorageStatus: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("len(statuses) = %d, want 1", len(statuses))
	}

	got := statuses[0]
	if got.BackupSetID != "alpha/one" {
		t.Errorf("BackupSetID = %q, want %q", got.BackupSetID, "alpha/one")
	}
	if got.LocalPath != localDir {
		t.Errorf("LocalPath = %q, want %q", got.LocalPath, localDir)
	}
	if !got.Available {
		t.Fatal("Available = false for a real, existing local directory")
	}
	if got.TotalBytes == 0 {
		t.Error("TotalBytes = 0, want a real filesystem reading")
	}
	if got.Level == "" {
		t.Error("Level is empty for an Available assessment")
	}
}

func TestListStorageStatus_UnavailableWhenLocalPathDoesNotExistYet(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not", "created", "yet")
	svc := New(testConfig(config.Source{
		Name: "alpha",
		BackupSets: []config.BackupSet{{
			Name:       "one",
			Remote:     config.Remote{Type: "local"},
			RemotePath: t.TempDir(),
			LocalPath:  missing,
			Completion: config.Completion{Strategy: "rename"},
			StaleAfter: config.Duration(24 * time.Hour),
		}},
	}), openTestJournal(t), nil, nil)

	statuses, err := svc.ListStorageStatus(context.Background())
	if err != nil {
		t.Fatalf("ListStorageStatus: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("len(statuses) = %d, want 1", len(statuses))
	}
	got := statuses[0]
	if got.Available {
		t.Fatal("Available = true for a local path that does not exist yet")
	}
	if got.Level != "" {
		t.Errorf("Level = %q, want empty for an unavailable assessment", got.Level)
	}
	if got.TotalBytes != 0 || got.FreeBytes != 0 {
		t.Errorf("TotalBytes/FreeBytes = %d/%d, want 0/0 for an unavailable assessment", got.TotalBytes, got.FreeBytes)
	}
}

// TestListStorageStatus_LevelMatchesWhatAdmitWouldDecide is issue #104's
// core claim for "surfacing capacity's existing refusal honestly, not a
// second possibly-disagreeing check": StorageStatus's Level for a backup
// set must be the exact same Level a real capacity.CheckBeforeTransfer
// call against that same directory, under the same thresholds, would
// compute right now.
func TestListStorageStatus_LevelMatchesWhatAdmitWouldDecide(t *testing.T) {
	localDir := t.TempDir()
	// Thresholds impossible to satisfy: this filesystem cannot possibly
	// have this much free space, so both StorageStatus and a direct
	// CheckBeforeTransfer call must agree the level is CRITICAL.
	impossible := capacity.Thresholds{WarningFreeBytes: 1 << 62, CriticalFreeBytes: 1 << 62}

	cfg := testConfig(config.Source{
		Name: "alpha",
		BackupSets: []config.BackupSet{{
			Name:       "one",
			Remote:     config.Remote{Type: "local"},
			RemotePath: t.TempDir(),
			LocalPath:  localDir,
			Completion: config.Completion{Strategy: "rename"},
			StaleAfter: config.Duration(24 * time.Hour),
		}},
	})
	svc := New(cfg, openTestJournal(t), nil, nil)
	svc.state.Load().inner.Capacity = impossible

	statuses, err := svc.ListStorageStatus(context.Background())
	if err != nil {
		t.Fatalf("ListStorageStatus: %v", err)
	}
	if statuses[0].Level != capacity.Critical.String() {
		t.Fatalf("StorageStatus Level = %q, want %q", statuses[0].Level, capacity.Critical.String())
	}

	assessment, err := capacity.CheckBeforeTransfer(localDir, 0, impossible)
	var insufficient *capacity.InsufficientCapacityError
	if err == nil {
		t.Fatal("capacity.CheckBeforeTransfer against impossible thresholds: error = nil, want InsufficientCapacityError")
	}
	if !isInsufficientCapacityError(err, &insufficient) {
		t.Fatalf("capacity.CheckBeforeTransfer error = %v, want *InsufficientCapacityError", err)
	}
	if assessment.Level != capacity.Critical {
		t.Fatalf("direct CheckBeforeTransfer Level = %v, want Critical", assessment.Level)
	}
}

func isInsufficientCapacityError(err error, target **capacity.InsufficientCapacityError) bool {
	ic, ok := err.(*capacity.InsufficientCapacityError)
	if ok {
		*target = ic
	}
	return ok
}

// TestListStorageStatus_UsesTheSharedLocalDirectoryFromTestConfig is a
// regression guard: New(...) built with no explicit os.MkdirAll for the
// backup set's LocalPath must not panic or error the whole call — the
// package doc's own promise that one uninitialised set never hides the
// assessment for every other configured one.
func TestListStorageStatus_MultipleBackupSetsAreAllReported(t *testing.T) {
	dirA := t.TempDir()
	dirB := filepath.Join(t.TempDir(), "not-created")

	svc := New(testConfig(config.Source{
		Name: "alpha",
		BackupSets: []config.BackupSet{
			{
				Name: "a", Remote: config.Remote{Type: "local"}, RemotePath: t.TempDir(),
				LocalPath: dirA, Completion: config.Completion{Strategy: "rename"},
				StaleAfter: config.Duration(24 * time.Hour),
			},
			{
				Name: "b", Remote: config.Remote{Type: "local"}, RemotePath: t.TempDir(),
				LocalPath: dirB, Completion: config.Completion{Strategy: "rename"},
				StaleAfter: config.Duration(24 * time.Hour),
			},
		},
	}), openTestJournal(t), nil, nil)

	statuses, err := svc.ListStorageStatus(context.Background())
	if err != nil {
		t.Fatalf("ListStorageStatus: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("len(statuses) = %d, want 2", len(statuses))
	}

	byID := map[string]StorageStatus{}
	for _, s := range statuses {
		byID[s.BackupSetID] = s
	}
	if !byID["alpha/a"].Available {
		t.Error("alpha/a: Available = false, want true (real directory)")
	}
	if byID["alpha/b"].Available {
		t.Error("alpha/b: Available = true, want false (directory does not exist)")
	}
}
