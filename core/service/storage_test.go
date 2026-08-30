package service

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
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
	// errors.As, not a type assertion: this is exactly how pipeline.go's
	// own admitCapacity classifies the same error, and matching it is the
	// point — a refusal this test recognises but the real gate would not
	// (or the reverse) would make the whole comparison meaningless.
	var insufficient *capacity.InsufficientCapacityError
	if !errors.As(err, &insufficient) {
		t.Fatalf("capacity.CheckBeforeTransfer against impossible thresholds: error = %v, want *InsufficientCapacityError", err)
	}
	if assessment.Level != capacity.Critical {
		t.Fatalf("direct CheckBeforeTransfer Level = %v, want Critical", assessment.Level)
	}
}

// TestListStorageStatus_MultipleBackupSetsAreAllReported is a regression
// guard for storage.go's own promise that one uninitialised backup set
// never hides the assessment for every other configured one: a set whose
// LocalPath nothing has created yet must come back as its own
// Available: false entry, not as an error that takes the whole call, and
// with it every healthy set's reading, down with it.
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

// storageTestSet builds the one-backup-set config every test below shares,
// so each one differs only in the thing it is actually about.
func storageTestSet(t *testing.T, localPath string) *config.Config {
	t.Helper()
	return testConfig(config.Source{
		Name: "alpha",
		BackupSets: []config.BackupSet{{
			Name:       "one",
			Remote:     config.Remote{Type: "local"},
			RemotePath: t.TempDir(),
			LocalPath:  localPath,
			Completion: config.Completion{Strategy: "rename"},
			StaleAfter: config.Duration(24 * time.Hour),
		}},
	})
}

// withStatPath swaps the statfs seam for the duration of one test. A real
// filesystem cannot be asked to report a FreeBytes and an AvailableBytes
// that differ, and the gap between those two numbers is the entire subject
// of the test below.
func withStatPath(t *testing.T, fn func(string) (capacity.Stat, error)) {
	t.Helper()
	prev := statPath
	statPath = fn
	t.Cleanup(func() { statPath = prev })
}

// TestListStorageStatus_ReportsTheNumberTheLevelWasDecidedFrom is the
// review's M2 fix. statfs reports two different free-space counts:
// f_bfree, every free block including the ones only a privileged process
// may allocate into, and f_bavail, what this process can actually use.
// internal/capacity decides Level and FR-21's transfer refusal from the
// second one, by that package's explicit design.
//
// Publishing only the first is how a storage screen ends up honestly
// rendering "free: 6 GB, critical threshold: 2 GB, level: CRITICAL" — on
// the one screen whose entire job is making a refusal legible. So the
// deciding number is on the wire too, named for what it is.
func TestListStorageStatus_ReportsTheNumberTheLevelWasDecidedFrom(t *testing.T) {
	// A 5% root reserve on a 1 TB volume, with 60 GB of raw free blocks:
	// 10 GB of that is reserved, so only 10 GB is genuinely usable.
	const (
		total     = uint64(1_000_000_000_000)
		free      = uint64(60_000_000_000)
		available = uint64(10_000_000_000)
		critical  = uint64(20_000_000_000)
	)
	withStatPath(t, func(string) (capacity.Stat, error) {
		return capacity.Stat{TotalBytes: total, FreeBytes: free, AvailableBytes: available}, nil
	})

	svc := New(storageTestSet(t, t.TempDir()), openTestJournal(t), nil, nil)
	svc.state.Load().inner.Capacity = capacity.Thresholds{
		WarningFreeBytes:  critical * 2,
		CriticalFreeBytes: critical,
	}

	statuses, err := svc.ListStorageStatus(context.Background())
	if err != nil {
		t.Fatalf("ListStorageStatus: %v", err)
	}
	got := statuses[0]

	if got.Level != capacity.Critical.String() {
		t.Fatalf("Level = %q, want %q — this test is about explaining a critical level, so it has to actually be critical", got.Level, capacity.Critical.String())
	}
	if got.AvailableBytes != available {
		t.Errorf("AvailableBytes = %d, want %d (statfs Bavail, the number Level was decided from)", got.AvailableBytes, available)
	}
	if got.AvailableBytes > got.CriticalFreeBytes {
		t.Errorf("AvailableBytes (%d) is above CriticalFreeBytes (%d) yet Level is CRITICAL: the number on the wire contradicts the verdict beside it", got.AvailableBytes, got.CriticalFreeBytes)
	}
	// The positive control for the assertion above: free_bytes is still
	// carried, still means what it always meant, and is exactly the number
	// that WOULD have contradicted the verdict had it been the only one
	// published.
	if got.FreeBytes != free {
		t.Errorf("FreeBytes = %d, want %d (statfs Bfree, kept for display)", got.FreeBytes, free)
	}
	if got.FreeBytes <= got.CriticalFreeBytes {
		t.Fatalf("FreeBytes (%d) is not above CriticalFreeBytes (%d): this fixture no longer reproduces the contradiction it exists to pin", got.FreeBytes, got.CriticalFreeBytes)
	}
	if got.TotalBytes != total {
		t.Errorf("TotalBytes = %d, want %d", got.TotalBytes, total)
	}
}

// TestListStorageStatus_ClassifiesAndLogsWhyAReadingIsUnavailable is the
// review's M5 fix. "The directory does not exist yet because no cycle has
// run" and "the mount this backup set writes to is gone" used to be the
// same response with the same zeroed fields and no log line anywhere, on
// the surface an operator consults precisely when transfers are being
// refused.
func TestListStorageStatus_ClassifiesAndLogsWhyAReadingIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statErr    error
		thresholds capacity.Thresholds
		wantReason StorageUnavailableReason
		wantLogged bool
	}{
		{
			name:       "a destination no cycle has created yet is the benign case",
			statErr:    fs.ErrNotExist,
			wantReason: StorageUnavailableNotCreated,
			wantLogged: false,
		},
		{
			name:       "a vanished mount is not, and must leave a trace",
			statErr:    fs.ErrPermission,
			wantReason: StorageUnavailableUnreadable,
			wantLogged: true,
		},
		{
			// Warning below critical: capacity.Thresholds.Validate refuses
			// it, so no level can honestly be computed for any backup set.
			name:       "thresholds that cannot be honored are a configuration fault",
			thresholds: capacity.Thresholds{WarningFreeBytes: 1, CriticalFreeBytes: 2},
			wantReason: StorageUnavailableMisconfigured,
			wantLogged: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStatPath(t, func(string) (capacity.Stat, error) {
				if tc.statErr != nil {
					return capacity.Stat{}, tc.statErr
				}
				return capacity.Stat{TotalBytes: 100, FreeBytes: 50, AvailableBytes: 50}, nil
			})

			var logged bytes.Buffer
			svc := New(storageTestSet(t, "/data/alpha"), openTestJournal(t), nil, obs.New(&logged, obs.LevelInfo))
			svc.state.Load().inner.Capacity = tc.thresholds

			statuses, err := svc.ListStorageStatus(context.Background())
			if err != nil {
				t.Fatalf("ListStorageStatus: %v", err)
			}
			got := statuses[0]
			if got.Available {
				t.Fatal("Available = true, want false")
			}
			if got.UnavailableReason != tc.wantReason {
				t.Errorf("UnavailableReason = %q, want %q", got.UnavailableReason, tc.wantReason)
			}
			if gotLogged := strings.Contains(logged.String(), "alpha/one"); gotLogged != tc.wantLogged {
				t.Errorf("logged a line naming the backup set = %v, want %v; log was: %s", gotLogged, tc.wantLogged, logged.String())
			}
		})
	}
}

// TestListStorageStatus_ProductionDefaults_ReportsZeroThresholdsAndOK
// pins what a real deployment actually gets from this endpoint today, as
// opposed to what every other test in this file constructs by hand.
//
// internal/config carries no capacity fields yet and nothing outside tests
// assigns app.Service.Capacity, so both thresholds are zero in every
// running process and Assess can only reach Critical on a genuinely full
// disk. FR-21's refusal is unaffected (it does not need a configured floor
// to refuse a transfer that will not fit) but there is no warning level to
// display until those config fields land, and that is a stated, tested
// fact here rather than something a reader has to discover from a grep.
func TestListStorageStatus_ProductionDefaults_ReportsZeroThresholdsAndOK(t *testing.T) {
	svc := New(storageTestSet(t, t.TempDir()), openTestJournal(t), nil, nil)

	statuses, err := svc.ListStorageStatus(context.Background())
	if err != nil {
		t.Fatalf("ListStorageStatus: %v", err)
	}
	got := statuses[0]
	if !got.Available {
		t.Fatalf("Available = false for a real directory, reason %q", got.UnavailableReason)
	}
	if got.WarningFreeBytes != 0 || got.CriticalFreeBytes != 0 {
		t.Errorf("thresholds = %d/%d, want 0/0: nothing in a running process populates these yet, and a test that pretends otherwise hides it",
			got.WarningFreeBytes, got.CriticalFreeBytes)
	}
	if got.Level != capacity.OK.String() {
		t.Errorf("Level = %q, want %q: with zero thresholds this endpoint cannot report anything else short of a completely full disk",
			got.Level, capacity.OK.String())
	}
}
