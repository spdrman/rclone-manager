package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// fixtureModelSetID is fixtureSetID as the typed id a CycleReport
// carries, so the stubbed reports below name the same backup set the
// rest of this file drives.
func fixtureModelSetID(t *testing.T) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

// TestBeginBackupSetEdit_StopsTheSchedulerStartingAPassForThatSet is the
// end-to-end proof of the issue's "the scheduler is held too, not just
// the in-flight run": with a hold in force, a real run cycle against a
// real config, journal and transport must journal nothing at all for the
// held set.
//
// It runs the cycle through SubmitRunCycle rather than calling
// internal/app directly, because that is the path an operator's Run
// actually takes and it is the one that has to carry the holds registry
// onto the context.
func TestBeginBackupSetEdit_StopsTheSchedulerStartingAPassForThatSet(t *testing.T) {
	svc, _ := openTestService(t)

	if _, err := svc.BeginBackupSetEdit(context.Background(), fixtureSetID); err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}

	runOneCycle(t, svc)

	artifacts, err := svc.ListArtifacts(context.Background(), ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 0 {
		t.Errorf("a cycle journaled %d artifact(s) for a backup set held for editing: %+v", len(artifacts), artifacts)
	}
}

// TestARunCycleWithNoHoldProcessesTheSet is the control the test above
// needs to mean anything: the same fixture, the same call, no hold, and
// the artifact is picked up. Without this, a BackupService that had
// simply stopped working would pass the test above.
func TestARunCycleWithNoHoldProcessesTheSet(t *testing.T) {
	svc, _ := openTestService(t)

	runOneCycle(t, svc)

	artifacts, err := svc.ListArtifacts(context.Background(), ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) == 0 {
		t.Fatal("a cycle with no hold journaled nothing; the fixture is not exercising a real transfer, so the hold test above would prove nothing")
	}
}

// TestEndBackupSetEdit_ReleasesTheHoldSoBackupsResume is the issue's "a
// set left permanently paused because someone closed a tab is a backup
// silently not happening": leaving edit mode has to actually let work
// start again, checked by running a real cycle after the release rather
// than by reading the registry back.
func TestEndBackupSetEdit_ReleasesTheHoldSoBackupsResume(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	if _, err := svc.BeginBackupSetEdit(ctx, fixtureSetID); err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}
	if err := svc.EndBackupSetEdit(ctx, fixtureSetID); err != nil {
		t.Fatalf("EndBackupSetEdit: %v", err)
	}

	runOneCycle(t, svc)

	artifacts, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) == 0 {
		t.Error("no artifact was processed after edit mode was left; the hold was not released")
	}
}

// TestEndBackupSetEdit_IsSafeToCallTwice: every route out of edit mode
// releases, and several of them fire for one edit (a save-and-exit
// followed by the form unmounting). A duplicate release must not be
// something a client has to take care to avoid.
func TestEndBackupSetEdit_IsSafeToCallTwice(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	if _, err := svc.BeginBackupSetEdit(ctx, fixtureSetID); err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := svc.EndBackupSetEdit(ctx, fixtureSetID); err != nil {
			t.Fatalf("EndBackupSetEdit call %d: %v", i+1, err)
		}
	}
}

// TestBackupSetEditHold_LapsesOnItsOwn is the closed-tab case, and the
// one property this design must not get wrong: a client that stops
// renewing must stop holding, with nothing else having to happen. It
// drives the registry's own clock rather than sleeping, so the test is
// about the lease and not about timing.
func TestBackupSetEditHold_LapsesOnItsOwn(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	clock := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	svc.holds.now = func() time.Time { return clock }

	if _, err := svc.BeginBackupSetEdit(ctx, fixtureSetID); err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}
	if !svc.holds.Held(fixtureSetID) {
		t.Fatal("the hold is not in force immediately after it was taken")
	}

	clock = clock.Add(editHoldLease + time.Second)
	if svc.holds.Held(fixtureSetID) {
		t.Error("the hold is still in force after its lease expired; a closed tab would pause this backup set forever")
	}

	// And a real cycle actually runs again, which is the observable
	// consequence a reader cares about.
	runOneCycle(t, svc)
	artifacts, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) == 0 {
		t.Error("no artifact was processed after the hold lapsed")
	}
}

// TestRenewBackupSetEdit_KeepsAHoldAlivePastItsOriginalLease: a client
// with its form still open must be able to hold indefinitely, or an
// operator typing carefully would have the set resume under them.
func TestRenewBackupSetEdit_KeepsAHoldAlivePastItsOriginalLease(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	clock := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	svc.holds.now = func() time.Time { return clock }

	first, err := svc.BeginBackupSetEdit(ctx, fixtureSetID)
	if err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}

	clock = clock.Add(editHoldLease / 2)
	renewed, err := svc.RenewBackupSetEdit(ctx, fixtureSetID)
	if err != nil {
		t.Fatalf("RenewBackupSetEdit: %v", err)
	}
	if !renewed.ExpiresAt.After(first.ExpiresAt) {
		t.Errorf("renewed expiry %s is not after the original %s", renewed.ExpiresAt, first.ExpiresAt)
	}

	clock = clock.Add(editHoldLease - time.Second)
	if !svc.holds.Held(fixtureSetID) {
		t.Error("the hold lapsed despite being renewed halfway through its lease")
	}
}

// TestBackupSetEditState_SaysNothingIsRunningWhenNothingIs is the
// issue's "if nothing is running, entering edit mode is silent": Running
// has to be nil, not a zero-valued struct a client would render as a
// prompt for a risk that does not exist.
func TestBackupSetEditState_SaysNothingIsRunningWhenNothingIs(t *testing.T) {
	svc, _ := openTestService(t)

	state, err := svc.BackupSetEditState(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("BackupSetEditState: %v", err)
	}
	if state.Running != nil {
		t.Errorf("Running = %+v with no cycle in flight, want nil", state.Running)
	}
	if state.Held {
		t.Error("Held is true before anything took a hold")
	}
}

// TestBackupSetEditState_NamesTheArtifactAndStageOfAnInFlightCycle is the
// warning's whole content. A bare "are you sure" is what the issue rules
// out: discarding a partial transfer of a named artifact is a materially
// different cost from cancelling a tick that has not started work, and an
// operator can only tell those apart if this says which one it is.
func TestBackupSetEditState_NamesTheArtifactAndStageOfAnInFlightCycle(t *testing.T) {
	svc, _ := openTestService(t)

	svc.cycleWatch.begin()
	defer svc.cycleWatch.end()
	svc.cycleWatch.ObserveProgress(app.Progress{
		Stage:       app.StageTransferring,
		BackupSetID: fixtureSetID,
		Artifact:    "backup.dump",
	})

	state, err := svc.BackupSetEditState(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("BackupSetEditState: %v", err)
	}
	if state.Running == nil {
		t.Fatal("Running is nil while a cycle is transferring for this set")
	}
	if state.Running.Artifact != "backup.dump" {
		t.Errorf("Running.Artifact = %q, want %q", state.Running.Artifact, "backup.dump")
	}
	if state.Running.Stage != app.StageTransferring {
		t.Errorf("Running.Stage = %q, want %q", state.Running.Stage, app.StageTransferring)
	}
}

// TestBackupSetEditState_IgnoresACycleWorkingOnADifferentSet: the
// warning is per backup set, so a cycle transferring somebody else's
// artifact must not make this set look busy. Warning about work an edit
// would not touch teaches an operator to click through the prompt.
func TestBackupSetEditState_IgnoresACycleWorkingOnADifferentSet(t *testing.T) {
	svc, _ := openTestService(t)

	svc.cycleWatch.begin()
	defer svc.cycleWatch.end()
	svc.cycleWatch.ObserveProgress(app.Progress{
		Stage:       app.StageTransferring,
		BackupSetID: "production/some-other-set",
		Artifact:    "elsewhere.dump",
	})

	state, err := svc.BackupSetEditState(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("BackupSetEditState: %v", err)
	}
	if state.Running != nil {
		t.Errorf("Running = %+v for a cycle working on a different backup set, want nil", state.Running)
	}
}

// TestCycleWatch_StopsAnsweringOnceTheCycleEnds: the reading is
// meaningless the moment the cycle it describes has stopped, and
// reporting a finished transfer as still running would make every Edit
// press warn.
func TestCycleWatch_StopsAnsweringOnceTheCycleEnds(t *testing.T) {
	w := newCycleWatch()
	w.begin()
	w.ObserveProgress(app.Progress{Stage: app.StageTransferring, BackupSetID: "a/b", Artifact: "x.dump"})
	if w.workFor("a/b") == nil {
		t.Fatal("workFor is nil while the cycle is running")
	}
	w.end()
	if got := w.workFor("a/b"); got != nil {
		t.Errorf("workFor = %+v after the cycle ended, want nil", got)
	}
}

// TestBeginBackupSetEdit_ReportsWhatItStopped: a client should say what
// it actually interrupted, and only when it interrupted something.
func TestBeginBackupSetEdit_ReportsWhatItStopped(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	quiet, err := svc.BeginBackupSetEdit(ctx, fixtureSetID)
	if err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}
	if quiet.Stopped != nil {
		t.Errorf("Stopped = %+v with nothing running, want nil", quiet.Stopped)
	}

	svc.cycleWatch.begin()
	defer svc.cycleWatch.end()
	svc.cycleWatch.ObserveProgress(app.Progress{
		Stage:       app.StageVerifying,
		BackupSetID: fixtureSetID,
		Artifact:    "backup.dump",
	})
	busy, err := svc.BeginBackupSetEdit(ctx, fixtureSetID)
	if err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}
	if busy.Stopped == nil {
		t.Fatal("Stopped is nil while a cycle was verifying this set's artifact")
	}
	if busy.Stopped.Artifact != "backup.dump" || busy.Stopped.Stage != app.StageVerifying {
		t.Errorf("Stopped = %+v, want backup.dump at %s", busy.Stopped, app.StageVerifying)
	}
}

// TestBackupSetEditHold_UnknownSetIsNotFound: holding a name this
// deployment does not configure would be a hold nothing ever consults,
// and a client would read the success as edit mode being safe to open.
func TestBackupSetEditHold_UnknownSetIsNotFound(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	for _, id := range []string{"production/nope", "nope/postgres-primary", "no-slash", ""} {
		if _, err := svc.BeginBackupSetEdit(ctx, id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("BeginBackupSetEdit(%q) error = %v, want ErrBackupSetNotFound", id, err)
		}
		if _, err := svc.BackupSetEditState(ctx, id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("BackupSetEditState(%q) error = %v, want ErrBackupSetNotFound", id, err)
		}
		if err := svc.EndBackupSetEdit(ctx, id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("EndBackupSetEdit(%q) error = %v, want ErrBackupSetNotFound", id, err)
		}
	}
}

// TestBackupSetEditHold_SurvivesAConfigurationHotReload is the property
// that makes per-box Save usable at all: saving one box rewrites
// config.yaml and hot-reloads this service, and a hold rebuilt along with
// the rest of the state would release itself in the middle of the very
// edit it exists to protect.
func TestBackupSetEditHold_SurvivesAConfigurationHotReload(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	if _, err := svc.BeginBackupSetEdit(ctx, fixtureSetID); err != nil {
		t.Fatalf("BeginBackupSetEdit: %v", err)
	}
	if _, err := svc.UpdateBackupSet(ctx, fixtureSetID, UpdateBackupSetRequest{
		StaleAfter: durationPtr(30 * time.Hour),
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	state, err := svc.BackupSetEditState(ctx, fixtureSetID)
	if err != nil {
		t.Fatalf("BackupSetEditState: %v", err)
	}
	if !state.Held {
		t.Error("the hold was lost when saving one field hot-reloaded the configuration")
	}
}

// TestSubmitRunCycle_AHoldStoppingASetDoesNotFailTheOperation is the
// service-layer half of app.ErrBackupSetHeldForEditing. Pressing Edit
// while a run an operator submitted is in flight stops that set's pass,
// which is what the operator asked for; marking the whole operation
// FAILED for it puts "context canceled" in the activity feed as the
// reason a backup did not happen, and teaches an operator to distrust the
// one signal this product exists to produce.
//
// It drives the real executeRunCycle through the runCycle seam rather
// than racing a real transfer, because what is under test here is how
// this layer CLASSIFIES a report, not whether internal/app produces one
// (holds_test.go over there already proves that, against a real cycle).
func TestSubmitRunCycle_AHoldStoppingASetDoesNotFailTheOperation(t *testing.T) {
	svc, _ := openTestService(t)
	withStubbedRunCycle(t, func(_ *app.Service, _ context.Context) app.CycleReport {
		return app.CycleReport{Sets: []app.BackupSetCycleResult{
			{Set: fixtureModelSetID(t), Err: app.ErrBackupSetHeldForEditing},
		}}
	})

	final := submitOneCycle(t, svc)

	if final.Status != "completed" {
		t.Errorf("operation status = %q (Error = %q), want completed: a set stopped because an operator entered edit mode has not failed",
			final.Status, final.Error)
	}
}

// TestSubmitRunCycle_ARealSetFailureStillFailsTheOperation is the control
// the test above needs. Without it, an executeRunCycle that had stopped
// classifying anything as a failure would pass.
func TestSubmitRunCycle_ARealSetFailureStillFailsTheOperation(t *testing.T) {
	svc, _ := openTestService(t)
	withStubbedRunCycle(t, func(_ *app.Service, _ context.Context) app.CycleReport {
		return app.CycleReport{Sets: []app.BackupSetCycleResult{
			{Set: fixtureModelSetID(t), Err: errors.New("the source went unreachable")},
		}}
	})

	final := submitOneCycle(t, svc)

	if final.Status != "failed" {
		t.Fatalf("operation status = %q, want failed", final.Status)
	}
	if !strings.Contains(final.Error, "the source went unreachable") {
		t.Errorf("operation error = %q, want it to name the real failure", final.Error)
	}
}
