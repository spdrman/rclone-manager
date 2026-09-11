// The refusals a per-set run makes, and the one property that makes it
// safe to have beside a deployment-wide one.
//
// The HTTP half of this issue's evidence is in
// apps/common/webhost/serve/runcontrols_test.go, driven through
// serve.NewEngine, and that is where "the bytes moved" is proven. What is
// here is the arithmetic that file cannot force deterministically: two
// submissions racing for one lock, and a set an operator is holding.
//
// Each refusal is asserted by its sentinel rather than by "an error came
// back", because the whole point of the vocabulary is that a client acts
// on them differently: one says wait, one says leave edit mode, one says
// reload the page, and one says check the id.
package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/app"
)

// submitFixtureRun is the submission this file makes over and over, with
// only the field under test varying.
func submitFixtureRun(t *testing.T, svc *BackupService, key, setID string) (Operation, error) {
	t.Helper()
	return svc.SubmitRunBackupSet(context.Background(), RunBackupSetRequest{
		IdempotencyKey: key,
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
		BackupSetID:    setID,
	})
}

// TestSubmitRunBackupSet_RefusesASetHeldForEditing is the race #350
// exists to prevent, reached through the new door.
//
// RunCycle skips a held set, so a deployment-wide run already honours the
// hold. A per-set run names the set explicitly, which is exactly the
// request that would walk straight into two writers on one definition if
// nothing checked, and no amount of operator intent makes that safe.
func TestSubmitRunBackupSet_RefusesASetHeldForEditing(t *testing.T) {
	svc, _ := openTestService(t)

	if _, err := svc.BeginBackupSetEdit(context.Background(), fixtureSetID); err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}

	_, err := submitFixtureRun(t, svc, "held-1", fixtureSetID)
	if !errors.Is(err, ErrBackupSetHeldForEditing) {
		t.Fatalf("err = %v, want ErrBackupSetHeldForEditing", err)
	}
	if errors.Is(err, ErrOperationAlreadyRunning) {
		t.Error("the hold refusal also matches ErrOperationAlreadyRunning, so a client cannot tell 'wait for the run' from 'leave edit mode'")
	}
}

// TestSubmitRunBackupSet_RunsOnceTheHoldIsReleased is the control for the
// test above. Without it, a SubmitRunBackupSet that refused every request
// for any reason would pass.
func TestSubmitRunBackupSet_RunsOnceTheHoldIsReleased(t *testing.T) {
	svc, _ := openTestService(t)

	if _, err := svc.BeginBackupSetEdit(context.Background(), fixtureSetID); err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}
	if err := svc.EndBackupSetEdit(context.Background(), fixtureSetID); err != nil {
		t.Fatalf("EndBackupSetEdit: %v", err)
	}

	op, err := submitFixtureRun(t, svc, "held-then-released", fixtureSetID)
	if err != nil {
		t.Fatalf("SubmitRunBackupSet after the hold was released: %v", err)
	}
	if done := waitForTerminalStatus(t, svc, op.ID); done.Status != "completed" {
		t.Fatalf("status = %q, want completed (error = %q)", done.Status, done.Error)
	}
}

// TestSubmitRunBackupSet_RecordsTheSetOnTheDurableRow is the acceptance
// criterion the operation model has had a column-shaped hole for since
// §14: a per-set run's row says which set it was for, so a later reader
// (the poll, the operations list, a support conversation) can tell it
// from a cycle without reparsing the parameters.
func TestSubmitRunBackupSet_RecordsTheSetOnTheDurableRow(t *testing.T) {
	svc, _ := openTestService(t)

	op, err := submitFixtureRun(t, svc, "records-the-set", fixtureSetID)
	if err != nil {
		t.Fatalf("SubmitRunBackupSet: %v", err)
	}
	if op.BackupSetID != fixtureSetID {
		t.Errorf("BackupSetID = %q, want %q", op.BackupSetID, fixtureSetID)
	}
	if op.Action != ActionRunBackupSet {
		t.Errorf("Action = %q, want %q", op.Action, ActionRunBackupSet)
	}

	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Status != "completed" {
		t.Fatalf("status = %q, want completed (error = %q)", done.Status, done.Error)
	}
	// The summary is recorded in the SAME shape a cycle's is, on purpose,
	// so every reader above this layer keeps working. One set processed,
	// because a per-set run visits exactly one.
	if done.Cycle == nil {
		t.Fatal("a completed per-set run recorded no cycle summary, so nothing above this layer can say what it did")
	}
	if done.Cycle.BackupSetsProcessed != 1 {
		t.Errorf("BackupSetsProcessed = %d, want 1", done.Cycle.BackupSetsProcessed)
	}
	// Absent rather than a pair of zeroes: FR-30's move pass is a
	// deployment-wide bound RunCycle runs once after every set, and a
	// fetch never attempts one. Reporting 0/0 would say "every move was
	// refused" about a run that asked for none.
	if done.Cycle.Moves != nil {
		t.Errorf("Moves = %+v, want absent: a per-set run runs no move pass, and zeroes would read as 'every move was refused'", done.Cycle.Moves)
	}
}

// TestSubmitRunBackupSet_RefusesAnIdThisDeploymentDoesNotConfigure covers
// both shapes of "no such set" with one sentinel: an id that could name
// something and does not, and one that could not name anything at all. A
// caller never has to tell those apart, which is unconfiguredSetID's own
// rule applied here.
func TestSubmitRunBackupSet_RefusesAnIdThisDeploymentDoesNotConfigure(t *testing.T) {
	svc, _ := openTestService(t)

	for _, id := range []string{"production/not-configured", "no-slash-at-all", "production/", "/postgres-primary"} {
		if _, err := submitFixtureRun(t, svc, "unknown-"+id, id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("SubmitRunBackupSet(%q): err = %v, want ErrBackupSetNotFound", id, err)
		}
	}
}

// TestSubmitRunBackupSet_RefusesAStaleConfigurationRevision is the
// optimistic-concurrency half, and it is asserted BEFORE the id is
// resolved on purpose: a caller on a screen that has moved on may be
// naming a set that has since been removed, and telling them the id is
// wrong sends them hunting something they can still see.
func TestSubmitRunBackupSet_RefusesAStaleConfigurationRevision(t *testing.T) {
	svc, _ := openTestService(t)

	_, err := svc.SubmitRunBackupSet(context.Background(), RunBackupSetRequest{
		IdempotencyKey: "stale-revision",
		Actor:          "alice",
		ConfigRevision: "a-revision-this-deployment-has-never-had",
		BackupSetID:    "production/removed-since-the-page-loaded",
	})
	if !errors.Is(err, ErrConfigRevisionStale) {
		t.Fatalf("err = %v, want ErrConfigRevisionStale", err)
	}
	if errors.Is(err, ErrBackupSetNotFound) {
		t.Error("a stale revision was reported as an unknown backup set, which sends an operator hunting an id rather than reloading the page")
	}
}

// TestSubmitRunBackupSet_RefusesTheMissingHalvesOfItsOwnRequest pins the
// three fields with no sensible default. Every one of them is
// ErrInvalidRequest rather than a 500, because every one is something the
// caller can fix.
func TestSubmitRunBackupSet_RefusesTheMissingHalvesOfItsOwnRequest(t *testing.T) {
	svc, _ := openTestService(t)

	for name, req := range map[string]RunBackupSetRequest{
		"no idempotency key": {ConfigRevision: svc.ConfigRevision(), BackupSetID: fixtureSetID},
		"no config revision": {IdempotencyKey: "k", BackupSetID: fixtureSetID},
		"no backup set":      {IdempotencyKey: "k", ConfigRevision: svc.ConfigRevision()},
	} {
		if _, err := svc.SubmitRunBackupSet(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// TestSubmitRunBackupSet_CannotOverlapADeploymentWideRun is the
// acceptance criterion "a per-set run and a deployment-wide run cannot
// overlap, and the loser is told which one it lost to".
//
// It is proven by holding the single-flight lock directly rather than by
// racing two submissions, because a race that happens to serialise proves
// nothing and a test that passes by timing is a test that will fail on a
// loaded machine. The lock is the mechanism, so the lock is what is
// asserted against.
func TestSubmitRunBackupSet_CannotOverlapADeploymentWideRun(t *testing.T) {
	svc, _ := openTestService(t)

	// Stand in for a cycle already executing: SubmitRunCycle's own
	// goroutine holds exactly this lock for the life of the pass.
	if !svc.runOnce.TryLock() {
		t.Fatal("the single-flight lock was already held before this test took it")
	}
	defer svc.runOnce.Unlock()

	_, err := submitFixtureRun(t, svc, "overlap-1", fixtureSetID)
	if !errors.Is(err, ErrOperationAlreadyRunning) {
		t.Fatalf("err = %v, want ErrOperationAlreadyRunning", err)
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("the refusal does not say what it lost to: %v", err)
	}
}

// TestSubmitRunBackupSet_IsVisibleToAnOperatorAboutToEnterEditMode is the
// half of #350's warning that a new door could have walked straight past.
//
// BackupSetEditState answers "would entering edit mode interrupt
// something" by reading the cycle watch and matching on the set id the
// readings carry. A per-set run that did not feed that watch would leave
// an operator opening the edit form, during a run of that very set,
// with no prompt at all: two writers on one definition, arriving through
// the one control that did not have the warning on it.
//
// The fetch is held open through the seam rather than raced, because a
// race that happens to serialise proves nothing.
func TestSubmitRunBackupSet_IsVisibleToAnOperatorAboutToEnterEditMode(t *testing.T) {
	svc, _ := openTestService(t)

	started := make(chan struct{})
	release := make(chan struct{})
	restore := runBackupSetFetch
	t.Cleanup(func() { runBackupSetFetch = restore })
	runBackupSetFetch = func(_ *app.Service, ctx context.Context, _, _ string) (app.FetchResult, error) {
		// Exactly what the real Fetch publishes first now: a reading that
		// already names the set (beginOneSetCycle, core/internal/app).
		if obs := app.ProgressObserverFrom(ctx); obs != nil {
			obs.ObserveProgress(app.Progress{
				BackupSetID:     fixtureSetID,
				BackupSetsTotal: 1,
				Stage:           app.StageTransferring,
				Artifact:        "backup.dump",
			})
		}
		close(started)
		<-release
		return app.FetchResult{}, nil
	}

	op, err := submitFixtureRun(t, svc, "edit-visibility", fixtureSetID)
	if err != nil {
		t.Fatalf("SubmitRunBackupSet: %v", err)
	}
	<-started

	state, err := svc.BackupSetEditState(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("BackupSetEditState: %v", err)
	}
	if state.Running == nil {
		t.Fatal("a per-set run in flight is invisible to BackupSetEditState, so entering edit mode for this very set would prompt about nothing")
	}
	if state.Running.Artifact != "backup.dump" || state.Running.Stage != app.StageTransferring {
		t.Errorf("Running = %+v, want the artifact and stage the run published; a warning that cannot say what it would interrupt is not a warning", state.Running)
	}

	close(release)
	waitForTerminalStatus(t, svc, op.ID)

	// The control: once the run has ended the watch stops answering, so
	// entering edit mode is silent again. Without this, a watch that
	// simply always said "something is running" would pass above.
	after, err := svc.BackupSetEditState(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("BackupSetEditState after the run: %v", err)
	}
	if after.Running != nil {
		t.Errorf("Running = %+v after the run finished, want nil: a permanent prompt is one an operator stops reading", after.Running)
	}
}

// TestSubmitRunBackupSet_ReplaysAnIdempotencyKeyRatherThanRunningTwice is
// the property the header exists for. A client that retries the same
// logical submission (a dropped response, a reload) must get the same
// operation back, not a second run of the same set.
func TestSubmitRunBackupSet_ReplaysAnIdempotencyKeyRatherThanRunningTwice(t *testing.T) {
	svc, _ := openTestService(t)

	first, err := submitFixtureRun(t, svc, "replayed", fixtureSetID)
	if err != nil {
		t.Fatalf("first SubmitRunBackupSet: %v", err)
	}
	second, err := submitFixtureRun(t, svc, "replayed", fixtureSetID)
	if err != nil {
		t.Fatalf("replayed SubmitRunBackupSet: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("replay returned operation %q, want the original %q", second.ID, first.ID)
	}
	waitForTerminalStatus(t, svc, first.ID)
}

// TestSubmitRunBackupSet_RefusesAKeyReusedForADifferentRequest is the
// other half of the same header, and the reason it has its own code: a
// key reused across two DIFFERENT logical requests is a client bug, and a
// client that could not tell it apart from a successful replay would
// silently drop one of the two runs an operator asked for.
func TestSubmitRunBackupSet_RefusesAKeyReusedForADifferentRequest(t *testing.T) {
	svc, _ := openTestService(t)

	if _, err := submitFixtureRun(t, svc, "reused", fixtureSetID); err != nil {
		t.Fatalf("first SubmitRunBackupSet: %v", err)
	}
	_, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "reused",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyKeyConflict", err)
	}
}
