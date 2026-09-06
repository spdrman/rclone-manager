//go:build unix

// Issue #537's both-directions proof for DetectRunningEngine, at the level
// the detection itself lives: a deployment a process has announced itself
// as serving is found, one nothing serves is reported as free, a plain
// journal reader is not mistaken for an engine, and the probe leaves both
// the engine and the directory exactly as it found them.
//
// The engine here is a second open file description rather than a second
// process, which is the same thing to flock(2): the lock lives on the open
// file description, so two open() calls conflict whether or not they are
// in the same process (lock_unix_test.go's startup-lock tests are written
// the same way, for the same reason). The genuinely-two-processes shape,
// which is the one #535 actually reported, is proved from the CLI's side
// in core/cmd/backup-manager/liveengine_test.go, against a real child.
package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDetectRunningEngine_FindsTheProcessServingTheDeployment is the
// "with an engine up it is detected" half. The engine is announced
// through AnnounceServing, which is the exact call `daemon` and the web
// host make and therefore the exact lock a live engine holds.
func TestDetectRunningEngine_FindsTheProcessServingTheDeployment(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	stopServing := announceForTest(t, configPath)

	engine, err := DetectRunningEngine(configPath)
	if err != nil {
		t.Fatalf("DetectRunningEngine: %v", err)
	}
	if engine == nil {
		t.Fatal("DetectRunningEngine found nothing while a process serves this deployment, want the engine reported (this is #535's defect: the missed engine)")
	}
	if engine.StateDatabase != dbPath {
		t.Errorf("engine.StateDatabase = %q, want %q", engine.StateDatabase, dbPath)
	}
	if err := stopServing(); err != nil {
		t.Fatalf("stop serving: %v", err)
	}
}

// TestDetectRunningEngine_DoesNotCallAPlainJournalReaderAnEngine is the
// false-positive half, and it is the one #537 spends most of its text on:
// reporting an engine that is not there strands the CLI on a host with
// nothing running, which is the case the direct path exists for.
//
// The holder below is what an ordinary host is doing most of the time. A
// `backup-manager status`, a `sources`, a cron `run` in the middle of a
// backup cycle: every one of them has the journal open for as long as it
// runs, and lock_unix.go's own doc calls that ordinary use of this CLI.
// The first version of this detector read exactly that lock and refused
// every configuration write on those hosts, while telling the operator to
// use a Web UI that a `backup-manager run` does not serve.
func TestDetectRunningEngine_DoesNotCallAPlainJournalReaderAnEngine(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	_, journal, releaseJournal, err := OpenConfigAndJournal(context.Background(), configPath)
	if err != nil {
		t.Fatalf("OpenConfigAndJournal (standing in for a `status` or a cron `run`): %v", err)
	}
	t.Cleanup(func() {
		_ = journal.Close()
		_ = releaseJournal()
	})

	engine, err := DetectRunningEngine(configPath)
	if err != nil {
		t.Fatalf("DetectRunningEngine: %v", err)
	}
	if engine != nil {
		t.Fatalf("DetectRunningEngine reported %+v while a plain reader has the journal open; nothing is serving this deployment and a configuration write here is the CLI acting as the only authority there is", engine)
	}
}

// TestDetectRunningEngine_ReportsNothingWhenTheEngineHasStopped is the
// other false-positive shape: the lock FILE outlives the process that
// took it, and reading its existence as an engine would refuse every
// write on every host that has ever run one.
func TestDetectRunningEngine_ReportsNothingWhenTheEngineHasStopped(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	stopServing := announceForTest(t, configPath)
	if err := stopServing(); err != nil {
		t.Fatalf("stop serving: %v", err)
	}
	if _, err := os.Stat(dbPath + servingLockSuffix); err != nil {
		t.Fatalf("the serving lock file should still be on disk after the engine stopped: %v", err)
	}

	engine, err := DetectRunningEngine(configPath)
	if err != nil {
		t.Fatalf("DetectRunningEngine: %v", err)
	}
	if engine != nil {
		t.Fatalf("DetectRunningEngine reported %+v after the only engine stopped, want nil", engine)
	}
}

// TestDetectRunningEngine_OnADeploymentNothingHasEverServed covers the
// bare host before its first start: no lock file has ever been created,
// and the probe must answer "nothing is running" without creating one.
// Every process that serves creates that file on the way in, so its
// absence is a conclusive answer rather than a guess.
func TestDetectRunningEngine_OnADeploymentNothingHasEverServed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	engine, err := DetectRunningEngine(configPath)
	if err != nil {
		t.Fatalf("DetectRunningEngine: %v", err)
	}
	if engine != nil {
		t.Fatalf("DetectRunningEngine reported %+v against a deployment nothing has ever served, want nil", engine)
	}
	if _, err := os.Stat(dbPath + servingLockSuffix); !os.IsNotExist(err) {
		t.Errorf("the probe created %s; detecting must leave nothing behind (stat err = %v)", dbPath+servingLockSuffix, err)
	}
}

// TestDetectRunningEngine_LeavesTheEngineUndisturbed is the
// non-destructive half of the requirement: probing must not take anything
// away from the process it just found, and must not leave a lock of its
// own behind for the next caller to trip over.
func TestDetectRunningEngine_LeavesTheEngineUndisturbed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	stopServing := announceForTest(t, configPath)
	_, journal, releaseJournal, err := OpenConfigAndJournal(context.Background(), configPath)
	if err != nil {
		t.Fatalf("OpenConfigAndJournal (the engine's own journal): %v", err)
	}

	for i := range 3 {
		if _, err := DetectRunningEngine(configPath); err != nil {
			t.Fatalf("DetectRunningEngine #%d: %v", i, err)
		}
	}

	// The engine still has a working journal after being probed three
	// times: the probe neither closed it nor moved anything under it.
	if _, err := journal.ListBackupSetIDs(context.Background()); err != nil {
		t.Errorf("the engine's journal stopped working after being probed: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	if err := releaseJournal(); err != nil {
		t.Fatalf("release journal lock: %v", err)
	}
	if err := stopServing(); err != nil {
		t.Fatalf("stop serving: %v", err)
	}

	// And nothing the probe did is still holding anything itself.
	engine, err := DetectRunningEngine(configPath)
	if err != nil {
		t.Fatalf("DetectRunningEngine after the engine let go: %v", err)
	}
	if engine != nil {
		t.Fatalf("DetectRunningEngine still reports %+v after the only engine released; the probe left a lock behind", engine)
	}
}

// TestDetectRunningEngine_ConcurrentProbesDoNotFindEachOther is the
// measured race, made into a check.
//
// The detector this replaced took the lock EXCLUSIVELY to ask its
// question, so two callers asking at the same moment found each other:
// 788 of 40000 concurrent probes reported a running engine against a
// deployment nothing was serving, and every one of those is a
// configuration write refused on a host with nothing to refuse it for.
// A shared request cannot collide with another shared request, which is
// why the lock is exclusive for the engine and shared for the asker.
func TestDetectRunningEngine_ConcurrentProbesDoNotFindEachOther(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	// The lock file has to exist for the probe to get as far as asking
	// the kernel anything, so seed it the way a host that has run an
	// engine before would have.
	stopServing := announceForTest(t, configPath)
	if err := stopServing(); err != nil {
		t.Fatalf("stop serving: %v", err)
	}

	const probers, each = 8, 500
	var falsePositives, failures atomic.Int64
	var wg sync.WaitGroup
	for range probers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				engine, err := DetectRunningEngine(configPath)
				if err != nil {
					failures.Add(1)
					continue
				}
				if engine != nil {
					falsePositives.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if n := falsePositives.Load(); n != 0 {
		t.Errorf("%d of %d concurrent probes reported an engine against a deployment nothing serves; probes are seeing each other", n, probers*each)
	}
	if n := failures.Load(); n != 0 {
		t.Errorf("%d of %d concurrent probes could not be performed at all", n, probers*each)
	}
}

// TestAnnounceServing_WaitsOutAProbeRatherThanFailing is the other half
// of the same race, and it is the claim the detector's own doc makes:
// asking whether an engine is running can never be the reason one stops
// working. That claim was measurably false of the version this replaces,
// where 8 of 20000 startup attempts were refused by a probe in flight.
//
// It cannot be proved by running probes and starts against each other,
// which is how it was measured: at that rate a suite would have to run
// twenty thousand starts to see one, and would then be a suite that fails
// once a fortnight for reasons nobody can reproduce. So the collision is
// held open deliberately instead. The shared lock taken below is exactly
// the one servingLockHeld takes, for microseconds; this just makes those
// microseconds long enough to watch, and asserts the announcement waits
// them out instead of failing on them.
func TestAnnounceServing_WaitsOutAProbeRatherThanFailing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	inFlight, err := os.OpenFile(dbPath+servingLockSuffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open the serving lock: %v", err)
	}
	defer inFlight.Close()
	if err := unix.Flock(int(inFlight.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatalf("holding a probe open: %v", err)
	}

	announced := make(chan error, 1)
	go func() {
		release, err := AnnounceServing(configPath)
		if err == nil {
			err = release()
		}
		announced <- err
	}()

	// Long enough that an announcement which does not wait has already
	// failed and put its error on the channel, and far inside the budget
	// one that does wait is given.
	time.Sleep(150 * time.Millisecond)
	if err := unix.Flock(int(inFlight.Fd()), unix.LOCK_UN); err != nil {
		t.Fatalf("letting the probe go: %v", err)
	}

	select {
	case err := <-announced:
		if err != nil {
			t.Fatalf("a process could not announce itself as serving because a probe was in flight: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AnnounceServing never returned after the probe let go")
	}
}

// TestAnnounceServing_RefusesASecondEngine records what the exclusive
// half of that lock buys beyond a collision-free probe.
//
// Two engines over one journal is not a shape this product has: the
// packaged deployment runs one engine beside a UI proxy that opens no
// journal, and the headless alternative is one daemon. Two would run two
// schedulers over one set of artifacts and hold two independent in-memory
// copies of one configuration, which is #535 with both halves live.
func TestAnnounceServing_RefusesASecondEngine(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	stopServing := announceForTest(t, configPath)

	if _, err := AnnounceServing(configPath); !errors.Is(err, ErrAlreadyServing) {
		t.Fatalf("second AnnounceServing error = %v, want errors.Is(_, ErrAlreadyServing)", err)
	}

	if err := stopServing(); err != nil {
		t.Fatalf("stop serving: %v", err)
	}
	release, err := AnnounceServing(configPath)
	if err != nil {
		t.Fatalf("AnnounceServing after the first engine stopped: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestBeginConfigWrite_KeepsAnEngineFromStartingUnderneathIt is the
// difference between a sample and mutual exclusion, stated as a property.
//
// A check taken once and then trusted says nothing about the seconds that
// follow it, and the seconds that follow it are where an SSH probe, a key
// import and a read of standard input live. Holding the same startup lock
// every engine start has to take is what makes "no engine was there when
// I asked" mean "no engine can finish starting before I am done".
func TestBeginConfigWrite_KeepsAnEngineFromStartingUnderneathIt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	guard, err := BeginConfigWrite(configPath)
	if err != nil {
		t.Fatalf("BeginConfigWrite: %v", err)
	}

	_, _, _, err = OpenConfigAndJournal(context.Background(), configPath)
	if !errors.Is(err, ErrStartupLocked) {
		t.Fatalf("a startup sequence under a held configuration-write claim: err = %v, want errors.Is(_, ErrStartupLocked)", err)
	}

	if err := guard.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_, journal, releaseJournal, err := OpenConfigAndJournal(context.Background(), configPath)
	if err != nil {
		t.Fatalf("a startup sequence after the claim was given back: %v", err)
	}
	_ = journal.Close()
	_ = releaseJournal()
}

// TestBeginConfigWrite_OnAHostWhoseStateDirectoryIsNotThereYet is the
// first-run case, and the reason the claim degrades instead of refusing.
//
// A brand-new deployment has no state directory: runStartupSequence
// creates it, and nothing has run yet. There is no lock file to take
// there and, more to the point, nothing can be serving a journal inside a
// directory that does not exist. Refusing here would be exactly the false
// positive #537 warns about, on the one host that has no other way in.
func TestBeginConfigWrite_OnAHostWhoseStateDirectoryIsNotThereYet(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "not-created-yet", "state.db")

	guard, err := BeginConfigWriteForJournal(dbPath)
	if err != nil {
		t.Fatalf("BeginConfigWriteForJournal on a bare host: %v", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	engine, err := DetectRunningEngineForJournal(dbPath)
	if err != nil {
		t.Fatalf("DetectRunningEngineForJournal on a bare host: %v", err)
	}
	if engine != nil {
		t.Fatalf("DetectRunningEngineForJournal reported %+v for a journal whose directory is not there", engine)
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

// announceForTest stands a process in as this deployment's engine and
// registers the release, so a test that fails part-way through does not
// leave the lock held for the next one.
func announceForTest(t *testing.T, configPath string) func() error {
	t.Helper()
	release, err := AnnounceServing(configPath)
	if err != nil {
		t.Fatalf("AnnounceServing: %v", err)
	}
	released := false
	stop := func() error {
		if released {
			return nil
		}
		released = true
		return release()
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func writeFileFor(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	return path
}
