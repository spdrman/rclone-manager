package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// This file covers ManagerStorage, the one manager-wide storage reading
// issue #286's dashboard panel is drawn from.
//
// The panel it replaces summed one entry per configured backup set, which
// answered a question nobody asked ("storage across my configured sets"),
// double-counted two sets sharing a volume, and produced "0 B of 0 B used ·
// NaN%" on the fresh install that has no sets yet. Every test here is about
// one of those three.

func capacitySource(t *testing.T, localPath string) config.Source {
	t.Helper()
	return config.Source{
		Name: "alpha",
		BackupSets: []config.BackupSet{{
			Name:       "one",
			Remote:     config.Remote{Type: "local"},
			RemotePath: t.TempDir(),
			LocalPath:  localPath,
			Completion: config.Completion{Strategy: "rename"},
			StaleAfter: config.Duration(24 * time.Hour),
		}},
	}
}

func serviceWithCapacity(t *testing.T, capBlock config.Capacity, sources ...config.Source) *BackupService {
	t.Helper()
	cfg := testConfig(sources...)
	cfg.Capacity = capBlock
	return New(cfg, openTestJournal(t), nil, nil)
}

// TestAnUnconfiguredInstanceSaysCapacityIsNotKnown is the defect the issue
// opened on. With nothing configured there is no backup root, so there is
// no filesystem to measure, and the only honest answer is that we do not
// know yet. Not zero bytes, and certainly not a percentage computed from
// zero over zero.
func TestAnUnconfiguredInstanceSaysCapacityIsNotKnown(t *testing.T) {
	svc := serviceWithCapacity(t, config.Capacity{})

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if got.Known {
		t.Fatal("Known = true with no backup sets configured: there is no filesystem to have read")
	}
	if got.UnknownReason != StorageUnknownNoBackupRoot {
		t.Errorf("UnknownReason = %q, want %q", got.UnknownReason, StorageUnknownNoBackupRoot)
	}
	if got.TotalBytes != 0 || got.LimitBytes != 0 {
		t.Errorf("byte counts are populated (%d total, %d limit) on a reading that was never taken", got.TotalBytes, got.LimitBytes)
	}
	if got.Level != "" {
		t.Errorf("Level = %q on an unknown reading, want empty: an unread disk is not OK", got.Level)
	}
}

// TestABackupRootThatDoesNotExistYetIsNotCreatedAndSaysSo separates the two
// unknowns that would otherwise look identical. "I know where the backups
// go and the directory is not there yet" is a first-run fact; "I have no
// idea where the backups go" is a configuration fact, and an operator can
// only act on one of them.
func TestABackupRootThatDoesNotExistYetIsNotCreatedAndSaysSo(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not", "created", "yet")
	svc := serviceWithCapacity(t, config.Capacity{}, capacitySource(t, missing))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if got.Known {
		t.Fatal("Known = true for a backup root that does not exist")
	}
	if got.UnknownReason != StorageUnknownNotCreated {
		t.Errorf("UnknownReason = %q, want %q", got.UnknownReason, StorageUnknownNotCreated)
	}
	if got.MeasuredPath != missing {
		t.Errorf("MeasuredPath = %q, want %q: an operator has to be told which path was tried", got.MeasuredPath, missing)
	}
}

// TestWithNoCapTheDenominatorIsTheWholeVolume is Rom's stated default: no
// cap means use the whole NAS disk, and say on the dashboard that that is
// what the bar is measuring.
func TestWithNoCapTheDenominatorIsTheWholeVolume(t *testing.T) {
	root := t.TempDir()
	svc := serviceWithCapacity(t, config.Capacity{}, capacitySource(t, root))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if !got.Known {
		t.Fatalf("Known = false for a real directory (%s)", got.UnknownReason)
	}
	if got.Denominator != DenominatorDisk {
		t.Errorf("Denominator = %q, want %q", got.Denominator, DenominatorDisk)
	}
	if got.CapBytes != 0 {
		t.Errorf("CapBytes = %d, want 0", got.CapBytes)
	}
	if got.LimitBytes != got.TotalBytes {
		t.Errorf("LimitBytes = %d, want TotalBytes (%d): with no cap the whole volume is the denominator", got.LimitBytes, got.TotalBytes)
	}
	if got.TotalBytes == 0 {
		t.Error("TotalBytes = 0 for a real filesystem")
	}
	if got.MeasuredPath != root {
		t.Errorf("MeasuredPath = %q, want %q", got.MeasuredPath, root)
	}
}

// TestWithACapTheDenominatorIsTheCap is the other reading, and the one that
// must never be confused with the first: a bar at 80% of a 2 TB disk and a
// bar at 80% of a 100 GB cap are different facts.
func TestWithACapTheDenominatorIsTheCap(t *testing.T) {
	root := t.TempDir()
	svc := serviceWithCapacity(t, config.Capacity{CapBytes: 100 << 30}, capacitySource(t, root))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if !got.Known {
		t.Fatalf("Known = false (%s)", got.UnknownReason)
	}
	if got.Denominator != DenominatorCap {
		t.Errorf("Denominator = %q, want %q", got.Denominator, DenominatorCap)
	}
	if got.CapBytes != 100<<30 {
		t.Errorf("CapBytes = %d, want %d", got.CapBytes, uint64(100)<<30)
	}
	if got.LimitBytes != 100<<30 {
		t.Errorf("LimitBytes = %d, want the cap", got.LimitBytes)
	}
	// Nothing has been transferred, so the cap is untouched and the whole
	// allowance is headroom.
	if got.UsedBytes != 0 {
		t.Errorf("UsedBytes = %d, want 0: this journal holds no artifacts", got.UsedBytes)
	}
	if !got.CatalogBytesKnown {
		t.Error("CatalogBytesKnown = false: an empty journal is a measurement of zero, not a failure to measure")
	}
}

// TestTheCatalogSumIsThisManagersOwnUsage proves the number the cap is
// enforced from comes from the catalog rather than from the volume. The
// temp directory this test measures sits on a filesystem with gigabytes of
// other people's data on it, and none of that may be attributed to us.
func TestTheCatalogSumIsThisManagersOwnUsage(t *testing.T) {
	root := t.TempDir()
	svc := serviceWithCapacity(t, config.Capacity{CapBytes: 100 << 30}, capacitySource(t, root))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if got.CatalogBytes != 0 {
		t.Errorf("CatalogBytes = %d, want 0: nothing has been transferred", got.CatalogBytes)
	}
	// The filesystem this test runs on is certainly not empty, so the gap
	// between what we account for and what the volume holds has to be
	// reported rather than folded into our own figure.
	if !got.OtherBytesKnown {
		t.Fatal("OtherBytesKnown = false on a real filesystem reading")
	}
	if got.OtherBytes == 0 {
		t.Error("OtherBytes = 0: the volume a temp directory lives on is never empty, and pretending otherwise hides that something else writes here")
	}
}

// TestTheStorageReadingNamesTheFilesystemItMeasured is the container trap.
// The engine runs in a container and the reading has to be of the backup
// root's filesystem as the container sees it; the only defence against
// measuring the wrong mount and never noticing is saying out loud which
// path was measured.
func TestTheStorageReadingNamesTheFilesystemItMeasured(t *testing.T) {
	root := t.TempDir()
	svc := serviceWithCapacity(t, config.Capacity{}, capacitySource(t, root))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if got.MeasuredPath != root {
		t.Errorf("MeasuredPath = %q, want %q", got.MeasuredPath, root)
	}
	if got.MeasuredPath == "/" {
		t.Error("MeasuredPath = \"/\": the container's own root filesystem is never the answer")
	}
}

// TestTwoBackupSetsOnOneVolumeAreMeasuredOnce is the double-counting bug
// the per-set panel had. Summing total_bytes across sets reported twice the
// disk for two sets sharing it, which is not a number that exists.
func TestTwoBackupSetsOnOneVolumeAreMeasuredOnce(t *testing.T) {
	base := t.TempDir()
	one := filepath.Join(base, "one")
	two := filepath.Join(base, "two")
	for _, d := range []string{one, two} {
		if err := mkdirAllForTest(d); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	src := capacitySource(t, one)
	src.BackupSets = append(src.BackupSets, config.BackupSet{
		Name:       "two",
		Remote:     config.Remote{Type: "local"},
		RemotePath: t.TempDir(),
		LocalPath:  two,
		Completion: config.Completion{Strategy: "rename"},
		StaleAfter: config.Duration(24 * time.Hour),
	})
	svc := serviceWithCapacity(t, config.Capacity{}, src)

	single := serviceWithCapacity(t, config.Capacity{}, capacitySource(t, base))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	want, err := single.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage (single): %v", err)
	}
	if got.TotalBytes != want.TotalBytes {
		t.Errorf("TotalBytes = %d for two sets on one volume, want %d: the volume is measured once, never once per set", got.TotalBytes, want.TotalBytes)
	}
	if got.MeasuredPath != base {
		t.Errorf("MeasuredPath = %q, want %q (the directory both sets share)", got.MeasuredPath, base)
	}
}

// TestAnExplicitBackupRootIsMeasured lets a deployment whose sets sit on
// different volumes name the one that matters instead of getting the
// not-known answer forever.
func TestAnExplicitBackupRootIsMeasured(t *testing.T) {
	root := t.TempDir()
	svc := serviceWithCapacity(t, config.Capacity{BackupRoot: root}, capacitySource(t, filepath.Join(t.TempDir(), "elsewhere")))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if !got.Known {
		t.Fatalf("Known = false (%s)", got.UnknownReason)
	}
	if got.MeasuredPath != root {
		t.Errorf("MeasuredPath = %q, want the configured backup_root %q", got.MeasuredPath, root)
	}
}

// pinStat replaces this package's statfs seam for one test, so a verdict
// about thresholds is a verdict about the code rather than about how full
// the developer's laptop happens to be. Every test that asserts a Level
// uses it; the ones that assert MeasuredPath, or that a real reading comes
// back at all, deliberately do not.
func pinStat(t *testing.T, stat capacity.Stat) {
	t.Helper()
	prev := statPath
	statPath = func(string) (capacity.Stat, error) { return stat, nil }
	t.Cleanup(func() { statPath = prev })
}

// TestTheThresholdsAreTheConfiguredOnes closes the loop on the comment in
// handlers_storage.go that said they were "structurally zero until
// internal/config grows capacity fields".
func TestTheThresholdsAreTheConfiguredOnes(t *testing.T) {
	pinStat(t, capacity.Stat{TotalBytes: 4000 << 30, FreeBytes: 3000 << 30, AvailableBytes: 3000 << 30})
	root := t.TempDir()
	svc := serviceWithCapacity(t, config.Capacity{
		CapBytes:          100 << 30,
		WarningFreeBytes:  40 << 30,
		CriticalFreeBytes: 20 << 30,
	}, capacitySource(t, root))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if got.WarningFreeBytes != 40<<30 || got.CriticalFreeBytes != 20<<30 {
		t.Errorf("thresholds = %d / %d, want %d / %d",
			got.WarningFreeBytes, got.CriticalFreeBytes, uint64(40)<<30, uint64(20)<<30)
	}
	// 100 GiB of allowance, none spent, so 100 GiB of headroom: above the
	// 40 GiB warning line.
	if got.Level != "OK" {
		t.Errorf("Level = %q, want OK", got.Level)
	}
}

// TestTheLevelReflectsTheCapNotTheDisk is the reason the thresholds had to
// move onto the binding headroom rather than onto free space. Three
// terabytes are free underneath, so nothing about the volume explains the
// warning: the allowance does.
func TestTheLevelReflectsTheCapNotTheDisk(t *testing.T) {
	pinStat(t, capacity.Stat{TotalBytes: 4000 << 30, FreeBytes: 3000 << 30, AvailableBytes: 3000 << 30})
	root := t.TempDir()
	svc := serviceWithCapacity(t, config.Capacity{
		CapBytes:         30 << 30,
		WarningFreeBytes: 100 << 30,
	}, capacitySource(t, root))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if got.Level != "WARNING" {
		t.Errorf("Level = %q, want WARNING: 30 GiB of allowance is under the 100 GiB warning line, whatever the disk says", got.Level)
	}
	if got.Denominator != DenominatorCap {
		t.Errorf("Denominator = %q, want %q", got.Denominator, DenominatorCap)
	}
	if got.BindingConstraint != DenominatorCap {
		t.Errorf("BindingConstraint = %q, want %q: 30 GiB of allowance is smaller than 3 TB of disk", got.BindingConstraint, DenominatorCap)
	}
}

// TestADiskSmallerThanTheAllowanceSaysTheDiskIsWhatBinds is the pair to the
// test above, and the reason BindingConstraint exists separately from
// Denominator. An operator watching a 1 TB allowance fill has no reason to
// expect the volume underneath to run out first, so when it will, the
// reading says which one it is.
func TestADiskSmallerThanTheAllowanceSaysTheDiskIsWhatBinds(t *testing.T) {
	pinStat(t, capacity.Stat{TotalBytes: 200 << 30, FreeBytes: 5 << 30, AvailableBytes: 5 << 30})
	root := t.TempDir()
	svc := serviceWithCapacity(t, config.Capacity{CapBytes: 1000 << 30}, capacitySource(t, root))

	got, err := svc.ManagerStorage(context.Background())
	if err != nil {
		t.Fatalf("ManagerStorage: %v", err)
	}
	if got.Denominator != DenominatorCap {
		t.Errorf("Denominator = %q, want %q: the operator configured a cap, so the gauge is about the cap", got.Denominator, DenominatorCap)
	}
	if got.BindingConstraint != DenominatorDisk {
		t.Errorf("BindingConstraint = %q, want %q: 5 GiB of disk is what will refuse the next transfer, not 1 TB of unspent allowance", got.BindingConstraint, DenominatorDisk)
	}
	if got.HeadroomBytes != 5<<30 {
		t.Errorf("HeadroomBytes = %d, want %d", got.HeadroomBytes, uint64(5)<<30)
	}
}

func mkdirAllForTest(p string) error { return os.MkdirAll(p, 0o755) }
