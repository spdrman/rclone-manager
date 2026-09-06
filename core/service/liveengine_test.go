//go:build unix

// Issue #537's both-directions proof for DetectRunningEngine, at the level
// the detection itself lives: a journal another open file description
// holds is found, a journal nothing holds is reported as free, and the
// probe leaves both the holder and the directory exactly as it found
// them.
//
// The holder here is a second open file description rather than a second
// process, which is the same thing to flock(2): the lock lives on the open
// file description, so two open() calls conflict whether or not they are
// in the same process (lock_unix_test.go's startup-lock tests are written
// the same way, for the same reason). The genuinely-two-processes shape,
// which is the one #535 actually reported, is proved from the CLI's side
// in core/cmd/backup-manager/liveengine_test.go, against a real child.
package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDetectRunningEngine_FindsTheProcessHoldingTheJournal is the "with
// an engine up it is detected" half. The holder is opened through
// OpenConfigAndJournal, which is the exact call a `serve`/`daemon`
// process makes and therefore the exact lock a live engine holds.
func TestDetectRunningEngine_FindsTheProcessHoldingTheJournal(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	_, journal, releaseJournal, err := OpenConfigAndJournal(context.Background(), configPath)
	if err != nil {
		t.Fatalf("OpenConfigAndJournal (standing in for a live engine): %v", err)
	}
	t.Cleanup(func() {
		_ = journal.Close()
		_ = releaseJournal()
	})

	engine, err := DetectRunningEngine(configPath)
	if err != nil {
		t.Fatalf("DetectRunningEngine: %v", err)
	}
	if engine == nil {
		t.Fatal("DetectRunningEngine found nothing while this deployment's journal is open, want the holder reported (this is #535's defect: the missed engine)")
	}
	if engine.StateDatabase != dbPath {
		t.Errorf("engine.StateDatabase = %q, want %q", engine.StateDatabase, dbPath)
	}
}

// TestDetectRunningEngine_ReportsNothingWhenNothingHoldsTheJournal is the
// other half, and the one that matters just as much: a false positive
// strands the CLI on a bare host, which is the case the direct path
// exists for.
func TestDetectRunningEngine_ReportsNothingWhenNothingHoldsTheJournal(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	// A journal that has been opened and closed again, so the lock file
	// exists on disk and nothing holds it. That is the shape a host takes
	// after any previous command has run, and reading "the lock file is
	// there" as "an engine is there" would refuse every write on it.
	_, journal, releaseJournal, err := OpenConfigAndJournal(context.Background(), configPath)
	if err != nil {
		t.Fatalf("OpenConfigAndJournal (seeding the lock file): %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	if err := releaseJournal(); err != nil {
		t.Fatalf("release journal lock: %v", err)
	}

	engine, err := DetectRunningEngine(configPath)
	if err != nil {
		t.Fatalf("DetectRunningEngine: %v", err)
	}
	if engine != nil {
		t.Fatalf("DetectRunningEngine reported %+v with nothing holding the journal, want nil", engine)
	}
}

// TestDetectRunningEngine_OnAJournalNothingHasEverOpened covers the bare
// host before its first start: no lock file has ever been created, and
// the probe must answer "nothing is running" without creating one. Every
// process that opens the journal creates that file on the way in, so its
// absence is a conclusive answer rather than a guess.
func TestDetectRunningEngine_OnAJournalNothingHasEverOpened(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	engine, err := DetectRunningEngine(configPath)
	if err != nil {
		t.Fatalf("DetectRunningEngine: %v", err)
	}
	if engine != nil {
		t.Fatalf("DetectRunningEngine reported %+v against a journal nothing has ever opened, want nil", engine)
	}
	if _, err := os.Stat(dbPath + journalLockSuffix); !os.IsNotExist(err) {
		t.Errorf("the probe created %s; detecting must leave nothing behind (stat err = %v)", dbPath+journalLockSuffix, err)
	}
}

// TestDetectRunningEngine_LeavesTheHolderUndisturbed is the
// non-destructive half of the requirement: probing must not take anything
// away from the process it just found, and must not leave a lock of its
// own behind for the next caller to trip over.
func TestDetectRunningEngine_LeavesTheHolderUndisturbed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	_, journal, releaseJournal, err := OpenConfigAndJournal(context.Background(), configPath)
	if err != nil {
		t.Fatalf("OpenConfigAndJournal (standing in for a live engine): %v", err)
	}

	for i := range 3 {
		if _, err := DetectRunningEngine(configPath); err != nil {
			t.Fatalf("DetectRunningEngine #%d: %v", i, err)
		}
	}

	// The holder still has a working journal after being probed three
	// times: the probe neither closed it nor moved anything under it.
	if _, err := journal.ListBackupSetIDs(context.Background()); err != nil {
		t.Errorf("the held journal stopped working after being probed: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	if err := releaseJournal(); err != nil {
		t.Fatalf("release journal lock: %v", err)
	}

	// And nothing the probe did is still holding the journal itself.
	engine, err := DetectRunningEngine(configPath)
	if err != nil {
		t.Fatalf("DetectRunningEngine after the holder let go: %v", err)
	}
	if engine != nil {
		t.Fatalf("DetectRunningEngine still reports %+v after the only holder released; the probe left a lock behind", engine)
	}
}

// TestDetectRunningEngine_SaysNothingAboutAConfigurationItCannotRead:
// with no readable configuration there is no journal path to probe, and
// nothing this answer feeds can write either — the open that follows it
// fails on the same file. Reporting an error here would replace the
// "your deployment has not been set up yet" message an operator needs
// (ErrConfigAbsent) with a locking complaint about a file that is not
// there.
func TestDetectRunningEngine_SaysNothingAboutAConfigurationItCannotRead(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		name string
		path string
	}{
		{"a configuration file that is not there", filepath.Join(dir, "absent.yaml")},
		{"a configuration file that is not YAML", writeFileFor(t, filepath.Join(dir, "junk.yaml"), "\tthis: [is not: yaml")},
		{"a configuration that names no state database", writeFileFor(t, filepath.Join(dir, "nodb.yaml"), "poll_interval: 15m\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, err := DetectRunningEngine(tc.path)
			if err != nil {
				t.Fatalf("DetectRunningEngine = %v, want no error", err)
			}
			if engine != nil {
				t.Fatalf("DetectRunningEngine reported %+v, want nil", engine)
			}
		})
	}
}

func writeFileFor(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	return path
}
