package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// writeRemovalFixtureConfig is writeTestConfigFile (open_test.go) with TWO
// backup sets under one source, both on the local transport with a real
// file waiting on each remote.
//
// Two, because every interesting claim in this file is about telling one
// set apart from another. A single-set fixture cannot distinguish "the
// removed set stopped being processed" from "the cycle stopped running",
// and those are very different products.
//
// The source declares read_only: true so this file can also prove what
// happens to that declaration when its last set goes. It is a real safety
// posture (issue #282, "pull from here, never delete here"), not a
// decoration, and losing it silently is the failure this fixture exists
// to catch.
func writeRemovalFixtureConfig(t *testing.T) (configPath string, localA, localB string) {
	t.Helper()
	return writeRemovalFixtureConfigWithPosture(t, true)
}

// writeRemovalFixtureConfigWithPosture is writeRemovalFixtureConfig with
// the source's read_only declaration under the caller's control. The
// safety cases below need it OFF: FR-15 delete-from-source is the hazard
// the removal hold exists to prevent, and a read-only source never
// deletes, so a probe against the default fixture could not tell "the
// hold stopped the cycle" from "the cycle had nothing to delete anyway".
func writeRemovalFixtureConfigWithPosture(t *testing.T, readOnly bool) (configPath string, localA, localB string) {
	t.Helper()
	dir := t.TempDir()

	remoteA := filepath.Join(dir, "remote-alpha")
	remoteB := filepath.Join(dir, "remote-beta")
	localA = filepath.Join(dir, "local-alpha")
	localB = filepath.Join(dir, "local-beta")
	for _, d := range []string{remoteA, remoteB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(remoteA, "alpha.dump"), []byte("alpha payload"), 0o644); err != nil {
		t.Fatalf("WriteFile(alpha): %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteB, "beta.dump"), []byte("beta payload"), 0o644); err != nil {
		t.Fatalf("WriteFile(beta): %v", err)
	}

	set := func(name, remotePath, localPath string) string {
		return "      - id: " + name + "\n" +
			"        remote:\n" +
			"          type: local\n" +
			"        remote_path: " + remotePath + "\n" +
			"        local_path: " + localPath + "\n" +
			"        include:\n" +
			"          - \"*.dump\"\n" +
			"        completion:\n" +
			"          strategy: rename\n" +
			"        stale_after: 24h\n"
	}

	posture := ""
	if readOnly {
		posture = "    read_only: true\n"
	}
	configPath = filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		posture +
		"    backup_sets:\n" +
		set("alpha", remoteA, localA) +
		set("beta", remoteB, localB) +
		"retention:\n  timezone: UTC\n  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}
	return configPath, localA, localB
}

// openRemovalFixtureService is writeRemovalFixtureConfig plus Open, wired as
// t.Cleanup, exactly as openTestService (backupsets_test.go) is for the
// single-set fixture.
func openRemovalFixtureService(t *testing.T) (*BackupService, string, string, string) {
	t.Helper()
	return openRemovalFixtureServiceWithPosture(t, true)
}

// openRemovalFixtureServiceWithPosture is openRemovalFixtureService over
// writeRemovalFixtureConfigWithPosture.
func openRemovalFixtureServiceWithPosture(t *testing.T, readOnly bool) (*BackupService, string, string, string) {
	t.Helper()
	configPath, localA, localB := writeRemovalFixtureConfigWithPosture(t, readOnly)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return svc, configPath, localA, localB
}

// cycleReportFrom drives one real pass over whichever *app.Service is given,
// with this service's own hold registry installed exactly the way
// runScheduledCycle installs it (scheduler.go).
//
// It takes the inner service rather than reading it off b.state, and that
// is the whole point for the safety case below: a cycle that is already
// running kept the pointer, and the configuration snapshot behind it,
// from before any removal. Passing an inner captured earlier is how a
// test stands in for one.
func cycleReportFrom(inner *app.Service, holds *editHolds) app.CycleReport {
	return inner.RunCycle(app.WithBackupSetHolds(context.Background(), holds))
}

// setsInReport lists the backup set ids one cycle report covers, in order.
func setsInReport(report app.CycleReport) []string {
	out := make([]string, 0, len(report.Sets))
	for _, s := range report.Sets {
		out = append(out, s.Set.String())
	}
	return out
}

func containsSetID(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// artifactIDsUnderSet lists every artifact this deployment holds for one
// backup set id, read through the UNFILTERED list.
//
// Unfiltered deliberately. ListArtifacts' by-set filter refuses an id the
// configuration does not name (artifacts.go, issue #187), which after a
// removal is every id this file cares about, and the promise being
// checked is the dialog's own: the backups "remain listed under Backups",
// which is the unfiltered list the Backups page actually calls.
func artifactIDsUnderSet(t *testing.T, svc *BackupService, setID string) []string {
	t.Helper()
	all, err := svc.ListArtifacts(context.Background(), ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	var out []string
	for _, a := range all {
		if a.BackupSetID == setID {
			out = append(out, a.ID)
		}
	}
	return out
}

// TestRemoveBackupSet_StopsCollectionAndKeepsEverythingAlreadyCollected is
// this issue's central proof, and it is the exact sentence the
// confirmation dialog has been promising while calling nothing: Backup
// Manager stops collecting for this set, and the backups it already took
// stay on storage and stay listed.
//
// Every one of the four claims is asserted, because any one of them alone
// passes for the wrong reason. "Absent from the cycle report" is
// satisfied by a cycle that ran nothing at all, which is why the other
// set has to still be there. "Still listed" is satisfied by a journal
// nothing ever wrote to, which is why the first cycle's artifacts are
// counted before the removal and the count is refused if it is zero.
func TestRemoveBackupSet_StopsCollectionAndKeepsEverythingAlreadyCollected(t *testing.T) {
	svc, _, localA, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	before := cycleReportFrom(svc.state.Load().inner, svc.holds)
	if got := setsInReport(before); !containsSetID(got, "production/alpha") || !containsSetID(got, "production/beta") {
		t.Fatalf("the first cycle covered %v, want both production/alpha and production/beta; "+
			"without both, everything below would pass vacuously", got)
	}

	alphaBefore := artifactIDsUnderSet(t, svc, "production/alpha")
	if len(alphaBefore) == 0 {
		t.Fatal("the first cycle journaled no artifact for production/alpha, so 'the backups survive removal' has nothing to survive")
	}
	filesBefore := localFilesBelow(t, localA)
	if len(filesBefore) == 0 {
		t.Fatalf("no file landed under %s, so 'the files stay on storage' has nothing to check", localA)
	}

	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	after := cycleReportFrom(svc.state.Load().inner, svc.holds)
	got := setsInReport(after)
	if containsSetID(got, "production/alpha") {
		t.Errorf("the cycle after removal covered %v; production/alpha was removed and must not be collected for again", got)
	}
	if !containsSetID(got, "production/beta") {
		t.Errorf("the cycle after removal covered %v, want production/beta; removing one set must not stop the others", got)
	}

	if alphaAfter := artifactIDsUnderSet(t, svc, "production/alpha"); len(alphaAfter) != len(alphaBefore) {
		t.Errorf("production/alpha lists %d artifacts after removal, want the %d it had before; "+
			"the dialog promises they remain listed under Backups", len(alphaAfter), len(alphaBefore))
	}
	if filesAfter := localFilesBelow(t, localA); len(filesAfter) != len(filesBefore) {
		t.Errorf("%d files remain under %s, want the %d that were there before removal; "+
			"the dialog promises they stay on storage", len(filesAfter), localA, len(filesBefore))
	}

	if _, err := svc.GetBackupSet(ctx, "production/alpha"); !errors.Is(err, ErrBackupSetNotFound) {
		t.Errorf("GetBackupSet(production/alpha) after removal: err = %v, want ErrBackupSetNotFound", err)
	}
}

// localFilesBelow lists every regular file under root, recursively.
func localFilesBelow(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// TestRemoveBackupSet_StopsACycleRunningAgainstThePreRemovalConfiguration
// is the safety case, and it is the reason the hold exists at all.
//
// Writing the file and swapping this service's *app.Service does not
// reach a cycle that is already running: it kept the pointer and the
// configuration snapshot it started with (scheduler.go reads
// state.Load().inner once). So a removal that only wrote the file would
// leave that cycle discovering, transferring and, for a set that is not
// read-only, deleting from the operator's source machine for a set they
// just removed and watched the dialog close on.
//
// The cycle here is driven through the inner service captured BEFORE the
// removal, which is exactly what a run already in flight is holding.
func TestRemoveBackupSet_StopsACycleRunningAgainstThePreRemovalConfiguration(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)

	// The snapshot a cycle in flight would be holding: it still names
	// both sets, and it always will.
	inFlight := svc.state.Load().inner

	if err := svc.RemoveBackupSet(context.Background(), "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	report := cycleReportFrom(inFlight, svc.holds)
	got := setsInReport(report)
	if containsSetID(got, "production/alpha") {
		t.Errorf("a cycle running against the pre-removal configuration covered %v; "+
			"production/alpha was removed, and this snapshot is exactly what a run already in flight is holding", got)
	}
	if !containsSetID(got, "production/beta") {
		t.Fatalf("a cycle running against the pre-removal configuration covered %v, want production/beta; "+
			"without it this test would pass for a cycle that did nothing at all", got)
	}
}

// TestRemoveBackupSet_TheHoldDoesNotExpire is the other half of the case
// above, and the one a lease cannot satisfy.
//
// An edit hold lapses after ninety seconds, on purpose: a closed tab must
// not pause a backup set forever. A removal is the opposite shape. One
// cycle can run for hours, so a hold that lapsed after ninety seconds
// would be a cycle that reaches the removed set forty minutes later, gets
// false back from Held, and processes it from the old snapshot.
//
// The clock is driven forward rather than slept through, using the same
// seam edithold_test.go already uses to prove a lease DOES lapse.
func TestRemoveBackupSet_TheHoldDoesNotExpire(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)

	if err := svc.RemoveBackupSet(context.Background(), "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}
	if !svc.holds.Held("production/alpha") {
		t.Fatal("Held(production/alpha) is false immediately after removal; nothing is stopping a cycle in flight")
	}

	// Well past editHoldLease, and past any cycle a real deployment runs.
	svc.holds.now = func() time.Time { return time.Now().Add(6 * time.Hour) }

	if !svc.holds.Held("production/alpha") {
		t.Error("Held(production/alpha) went false six hours after removal; a removal's hold must outlive the edit lease, " +
			"because the cycle it has to stop can run for longer than one")
	}
}

// TestRemoveBackupSet_LastSetUnderASourceLeavesTheSourceAndItsPosture is
// the case an operator hits first on a small install, and the one the
// configuration rules refused before this issue.
//
// The sharp assertion is the reopen. Every service write validates
// against the same config.Validate the daemon runs at boot, so a removal
// that persisted a file Validate refuses is an operator who removes their
// last set through the UI and then finds the service will not start
// again. Opening the written file is the only way to prove that did not
// happen; re-reading it in this process would prove nothing, because this
// process already has a running service.
func TestRemoveBackupSet_LastSetUnderASourceLeavesTheSourceAndItsPosture(t *testing.T) {
	svc, configPath, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	for _, id := range []string{"production/alpha", "production/beta"} {
		if err := svc.RemoveBackupSet(ctx, id); err != nil {
			t.Fatalf("RemoveBackupSet(%s): %v", id, err)
		}
	}

	sets, err := svc.ListBackupSets(ctx)
	if err != nil {
		t.Fatalf("ListBackupSets: %v", err)
	}
	if len(sets) != 0 {
		t.Errorf("ListBackupSets returned %d sets after both were removed, want 0", len(sets))
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath): %v", err)
	}
	var onDisk config.Config
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("yaml.Unmarshal(configPath): %v", err)
	}
	if len(onDisk.Sources) != 1 {
		t.Fatalf("the written config has %d sources, want the 1 it started with; a removal must not take the source with it:\n%s",
			len(onDisk.Sources), raw)
	}
	if got := len(onDisk.Sources[0].BackupSets); got != 0 {
		t.Errorf("the written config's source still declares %d backup sets, want 0", got)
	}
	if !onDisk.Sources[0].ReadOnly {
		t.Errorf("the written config's source lost read_only: true. That is issue #282's "+
			"\"pull from here, never delete here\" for a whole host, and a set created under this "+
			"source name later would silently come back without it:\n%s", raw)
	}

	reopened, cleanup, err := Open(ctx, configPath)
	if err != nil {
		t.Fatalf("Open on the configuration a removal wrote: %v\nA daemon restarting after this removal would fail exactly here.", err)
	}
	defer func() { _ = cleanup() }()
	if reopened.ConfigRevision() == "" {
		t.Error("the reopened service has an empty ConfigRevision")
	}
}

// TestRemoveBackupSet_UnknownIDIsRefusedAndPausesNothing covers the two
// refusals together, because they share the failure that matters: a
// refusal that had already taken the hold, and did not give it back,
// would silently pause a set that is still configured.
func TestRemoveBackupSet_UnknownIDIsRefusedAndPausesNothing(t *testing.T) {
	svc, configPath, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	for _, id := range []string{"production/does-not-exist", "not-an-id", "production/alpha/extra"} {
		if err := svc.RemoveBackupSet(ctx, id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("RemoveBackupSet(%q): err = %v, want ErrBackupSetNotFound", id, err)
		}
		if svc.holds.Held(id) {
			t.Errorf("RemoveBackupSet(%q) was refused but left a hold behind", id)
		}
	}

	if svc.holds.Held("production/alpha") {
		t.Error("a refused removal left production/alpha held; that set is still configured and would silently stop backing up")
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a refused removal rewrote the configuration file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRemoveBackupSet_AFailedWriteGivesTheHoldBack covers the refusals
// that happen AFTER the hold has been taken, which is the half
// TestRemoveBackupSet_UnknownIDIsRefusedAndPausesNothing above cannot
// reach: an unknown id is turned away before the registry is touched at
// all, so that test would pass against a removal that never gave a hold
// back.
//
// This one gets past that check and fails at the write, and the property
// it protects is the worst outcome available on this path: a removal that
// refused, left the set configured, and left it permanently held would be
// a backup set that silently stops backing up with nothing anywhere
// saying why. The lease that covers an edit hold does not cover this one,
// deliberately, so giving it back is the only thing that can.
//
// The write is made to fail by taking write permission off the directory
// the configuration lives in, so writeConfigBytesAtomically cannot create
// its temporary file. Skipped for root, which ignores the mode bits.
func TestRemoveBackupSet_AFailedWriteGivesTheHoldBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file mode bits do not stop a write, so this cannot fail the way it needs to")
	}
	svc, configPath, _, _ := openRemovalFixtureService(t)

	dir := filepath.Dir(configPath)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%s): %v", dir, err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, info.Mode()) })

	if err := svc.RemoveBackupSet(context.Background(), "production/alpha"); err == nil {
		t.Fatal("RemoveBackupSet succeeded against a configuration directory it cannot write to")
	}

	if svc.holds.Held("production/alpha") {
		t.Error("the removal failed and production/alpha is still held. It is still configured, its hold does not expire, " +
			"and nothing anywhere would say why it had stopped backing up")
	}

	// The control: the set really is still configured, so the assertion
	// above is about a set that matters rather than one already gone.
	if _, err := svc.GetBackupSet(context.Background(), "production/alpha"); err != nil {
		t.Errorf("GetBackupSet(production/alpha) after a failed removal: %v, want the set to still be configured", err)
	}
}

// TestRemoveBackupSet_SecondRemovalIsRefusedRatherThanReportedAsSuccess
// pins the idempotency decision rather than leaving it to fall out.
//
// The EFFECT is idempotent: removing an already-removed set changes
// nothing. The STATUS deliberately is not. On a destructive control a
// mistyped name must not come back as success, which is the whole shape
// of the defect this issue is about.
func TestRemoveBackupSet_SecondRemovalIsRefusedRatherThanReportedAsSuccess(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("first RemoveBackupSet: %v", err)
	}
	if err := svc.RemoveBackupSet(ctx, "production/alpha"); !errors.Is(err, ErrBackupSetNotFound) {
		t.Errorf("second RemoveBackupSet: err = %v, want ErrBackupSetNotFound", err)
	}
	if !svc.holds.Held("production/alpha") {
		t.Error("the second, refused removal released the hold the first one took")
	}
}

// removeTwiceAtOnce runs two RemoveBackupSet calls for the same id from two
// goroutines released on one signal, and sorts the outcomes: how many
// removed the set, how many were told it was already gone, and anything
// else, which is a failure of the probe rather than of the code.
func removeTwiceAtOnce(t *testing.T, svc *BackupService, id string, release func()) (removed, gone int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = svc.RemoveBackupSet(context.Background(), id)
		}(i)
	}
	close(start)
	if release != nil {
		release()
	}
	wg.Wait()
	for _, err := range errs {
		switch {
		case err == nil:
			removed++
		case errors.Is(err, ErrBackupSetNotFound):
			gone++
		default:
			t.Fatalf("RemoveBackupSet(%s) under a duplicate: %v, want nil or ErrBackupSetNotFound", id, err)
		}
	}
	return removed, gone
}

// TestRemoveBackupSet_ADuplicateRemovalInFlightDoesNotDropTheHold is the
// two-caller case none of the tests above consider, and it is the one
// that was wrong.
//
// Two removals of the same set overlap whenever two tabs, two operators,
// or a client retrying a slow response reach the route together. Exactly
// one of them removes the set; the other is told it is already gone,
// which is right. What must not happen is the loser's cleanup taking the
// winner's hold with it: a removal's hold is the ONLY thing a cycle
// already running against the pre-removal snapshot can see, and a set
// that is gone from the configuration with no hold on it is a set that
// cycle will process, and for a source that is not read-only, delete
// from.
//
// No test-side synchronisation at all: two goroutines, one start signal,
// the way two clients would arrive. Repeated, because a race that is
// drawn on the rare side once proves nothing; before the fix this lost
// the hold on every one of these trials.
func TestRemoveBackupSet_ADuplicateRemovalInFlightDoesNotDropTheHold(t *testing.T) {
	const trials = 25
	lost := 0
	overlapped := 0
	for i := 0; i < trials; i++ {
		svc, _, _, _ := openRemovalFixtureService(t)
		removed, gone := removeTwiceAtOnce(t, svc, "production/alpha", nil)
		if removed != 1 || gone != 1 {
			t.Fatalf("trial %d: %d removed and %d refused, want exactly one of each", i, removed, gone)
		}
		overlapped++
		if !svc.holds.Held("production/alpha") {
			lost++
		}
	}
	if overlapped == 0 {
		t.Fatal("no trial produced one removal and one refusal, so nothing here was exercised")
	}
	if lost > 0 {
		t.Errorf("the hold on production/alpha was lost in %d of %d concurrent duplicate removals; "+
			"the set is gone from the configuration and a cycle in flight would process it from the old snapshot", lost, trials)
	}
}

// TestRemoveBackupSet_ADuplicateRemovalCannotHandTheSetBackToACycle is the
// consequence of the case above, driven all the way to the disk.
//
// The interleaving is forced rather than hoped for: the test holds
// configMu so both callers get past the atomic-state check (which still
// names the set) before either can reach the file, and then lets them go.
// The source is NOT read-only, so if the hold is gone the cycle run from
// the pre-removal snapshot really does delete alpha.dump from the
// operator's source machine, which is the one outcome this whole
// operation is built to prevent.
func TestRemoveBackupSet_ADuplicateRemovalCannotHandTheSetBackToACycle(t *testing.T) {
	svc, configPath, _, _ := openRemovalFixtureServiceWithPosture(t, false)
	sourceFile := filepath.Join(filepath.Dir(configPath), "remote-alpha", "alpha.dump")
	if _, err := os.Stat(sourceFile); err != nil {
		t.Fatalf("the fixture's source file is missing before anything ran: %v", err)
	}

	// The snapshot a cycle in flight would be holding: it still names
	// both sets, and always will.
	inFlight := svc.state.Load().inner

	svc.configMu.Lock()
	removed, gone := removeTwiceAtOnce(t, svc, "production/alpha", func() {
		// Both goroutines are past requireBackupSet and queued on the
		// mutex well inside this; the check they run first is one atomic
		// load and a walk over two sets. If a machine is so starved that
		// one of them has not started yet, the outcome below is still
		// one removal and one refusal, only with the refusal answered
		// off the atomic state instead of the file, and the assertions
		// on the hold and the source file hold either way.
		time.Sleep(100 * time.Millisecond)
		svc.configMu.Unlock()
	})
	if removed != 1 || gone != 1 {
		t.Fatalf("%d removed and %d refused, want exactly one of each", removed, gone)
	}
	if !svc.holds.Held("production/alpha") {
		t.Errorf("production/alpha is not held after a removal raced a duplicate; the loser's cleanup dropped the winner's hold")
	}

	report := cycleReportFrom(inFlight, svc.holds)
	if got := setsInReport(report); containsSetID(got, "production/alpha") {
		t.Errorf("a cycle on the pre-removal snapshot covered %v after production/alpha was removed", got)
	}
	if _, err := os.Stat(sourceFile); err != nil {
		t.Errorf("%s is gone from the operator's source after production/alpha was removed: %v", sourceFile, err)
	}
}

// TestRemoveBackupSet_ADuplicateThatWinsTheLockLeavesTheHoldStanding is
// the ordering the first two cases cannot force, and the one that told
// the two candidate fixes apart.
//
// A fix that let the first caller remember "I placed the hold" and give
// it back only then still failed here: the first caller can lose the
// lock to the second, which then removes the set, and the first caller
// refuses and, being the one that placed the hold, releases it. Go's
// mutex lets a goroutine already running on another P take a lock ahead
// of a waiter that was just woken, which is what this test arranges: the
// first caller is parked on configMu, the second is spinning, and the
// unlocking goroutine keeps its P busy for a moment so the woken waiter
// cannot run before the spinner reaches the lock. Which caller wins is
// logged, not required, because the assertion has to hold either way.
func TestRemoveBackupSet_ADuplicateThatWinsTheLockLeavesTheHoldStanding(t *testing.T) {
	const trials = 40
	secondWon, lost := 0, 0
	for i := 0; i < trials; i++ {
		svc, _, _, _ := openRemovalFixtureService(t)
		ctx := context.Background()
		var wg sync.WaitGroup
		var errFirst, errSecond error
		var release atomic.Bool

		svc.configMu.Lock()
		wg.Add(1)
		go func() { defer wg.Done(); errFirst = svc.RemoveBackupSet(ctx, "production/alpha") }()
		time.Sleep(2 * time.Millisecond) // the first caller is parked on configMu
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !release.Load() {
			}
			errSecond = svc.RemoveBackupSet(ctx, "production/alpha")
		}()
		time.Sleep(time.Millisecond) // the second caller is spinning on another P
		svc.configMu.Unlock()
		release.Store(true)
		busyUntil := time.Now().Add(200 * time.Microsecond)
		for time.Now().Before(busyUntil) {
		}
		wg.Wait()

		switch {
		case errFirst == nil && errors.Is(errSecond, ErrBackupSetNotFound):
		case errSecond == nil && errors.Is(errFirst, ErrBackupSetNotFound):
			secondWon++
		default:
			t.Fatalf("trial %d: first=%v second=%v, want one nil and one ErrBackupSetNotFound", i, errFirst, errSecond)
		}
		if !svc.holds.Held("production/alpha") {
			lost++
		}
	}
	t.Logf("the second caller won the lock in %d of %d trials", secondWon, trials)
	if lost > 0 {
		t.Errorf("the hold on production/alpha was lost in %d of %d trials; "+
			"whichever call removes the set, its hold has to survive the other one's refusal", lost, trials)
	}
}

// TestRemoveBackupSet_RecordsWhatItRemovedAndWhatItKept covers the audit
// trail on a destructive control. A removal that left no trace of having
// happened turns a support conversation six weeks later into
// archaeology, and the number that matters is the one about what STAYED,
// because that is the half of the promise nobody can see from the config
// file afterwards.
func TestRemoveBackupSet_RecordsWhatItRemovedAndWhatItKept(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)

	if got := cycleReportFrom(svc.state.Load().inner, svc.holds); len(got.Sets) == 0 {
		t.Fatal("the seeding cycle covered no sets, so there would be nothing retained to report")
	}

	var log bytes.Buffer
	svc.logger = obs.New(&log, obs.LevelInfo)

	if err := svc.RemoveBackupSet(context.Background(), "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	line := log.String()
	if !strings.Contains(line, "backup_set_removed") {
		t.Errorf("no backup_set_removed event was written for the removal:\n%s", line)
	}
	if !strings.Contains(line, `"retained_artifacts":1`) {
		t.Errorf(`the removal event does not report "retained_artifacts":1, so it does not say what stayed behind:`+"\n%s", line)
	}
}

// TestCreateBackupSet_OverARemovedIDReadoptsItsHistoryLoudlyAndLiftsTheHold
// is this issue's answer to the re-creation question, made observable.
//
// A backup set is identified by its source and its name
// (model.NewArtifactID is source/set/name), so creating one over an id
// that already has journal rows hands it all of them. That is the
// behaviour I want, because it is what undoing a removal needs, and
// pretending otherwise would mean re-fetching a volume full of backups to
// get back where you already were. What it must not be is silent.
func TestCreateBackupSet_OverARemovedIDReadoptsItsHistoryLoudlyAndLiftsTheHold(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if got := cycleReportFrom(svc.state.Load().inner, svc.holds); len(got.Sets) == 0 {
		t.Fatal("the seeding cycle covered no sets, so there would be no history to re-adopt")
	}
	adopted := artifactIDsUnderSet(t, svc, "production/alpha")
	if len(adopted) == 0 {
		t.Fatal("production/alpha journaled nothing, so this test would prove re-adoption of an empty history")
	}

	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	var log bytes.Buffer
	svc.logger = obs.New(&log, obs.LevelInfo)

	req := validCreateReq(t, svc, "alpha")
	req.SourceName = "production"
	if _, err := svc.CreateBackupSet(ctx, req); err != nil {
		t.Fatalf("CreateBackupSet over the removed id: %v", err)
	}

	if svc.holds.Held("production/alpha") {
		t.Error("production/alpha is still held after being created again; a set brought back must actually run")
	}
	if got := artifactIDsUnderSet(t, svc, "production/alpha"); len(got) != len(adopted) {
		t.Errorf("the re-created set lists %d artifacts, want the %d the removed one left behind", len(got), len(adopted))
	}
	if line := log.String(); !strings.Contains(line, "backup_set_adopted_history") {
		t.Errorf("creating a set over an id with history on record said nothing about adopting it:\n%s", line)
	}
}
