package service

import (
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// TestARestoreOperationNeverCarriesAProgressReading is FR-34's "no
// progress percentage, ETA or cost is ever rendered for a restore",
// enforced where a reading would actually be attached.
//
// # Why it is worth a test when nothing registers a restore's progress
//
// Because "nothing registers one" is a fact about today's callers and this
// is a fact about the type. A restore runs at the provider for hours and
// S3 reports it as running or finished and nothing else, so a reading for
// one could only ever be invented; the refusal belongs beside the code
// that attaches readings, where somebody adding a reading has to walk past
// it.
//
// The run_cycle half is the control, and it is the half that would catch a
// refusal written too broadly: a cycle's progress is measured from real
// byte counts and must keep being served.
func TestARestoreOperationNeverCarriesAProgressReading(t *testing.T) {
	b := &BackupService{progress: newLiveProgress()}

	reading := b.progress.begin("op_1")
	reading.ObserveProgress(app.Progress{
		Stage: "transferring", BackupSetsTotal: 1, ArtifactsDone: 1,
	})

	restore := Operation{ID: "op_1", Status: state.OperationRunning, Action: ActionRestorePlacement}
	if got := b.withLiveProgress(restore); got.Progress != nil {
		t.Fatalf("a restore operation came back carrying a progress reading: %+v", got.Progress)
	}

	cycle := Operation{ID: "op_1", Status: state.OperationRunning, Action: ActionRunCycle}
	if got := b.withLiveProgress(cycle); got.Progress == nil {
		t.Fatal("a running run_cycle lost its progress reading; the refusal above is written too broadly")
	}
}

// TestTheStartupSweepSparesAnExternallyExecutedAction pins the list
// core/service actually passes, so a restore added to internal/archive and
// forgotten here would fail rather than being quietly swept on the next
// restart.
func TestTheStartupSweepSparesAnExternallyExecutedAction(t *testing.T) {
	found := false
	for _, a := range externallyExecutedActions {
		if a == ActionRestorePlacement {
			found = true
		}
		if a == ActionRunCycle {
			t.Fatalf("%q is executed by a goroutine in this process, so a dead process really did abandon it and the sweep has to fail it", a)
		}
	}
	if !found {
		t.Fatalf("%q runs at the storage provider and survives a restart, so the sweep must leave it alone", ActionRestorePlacement)
	}
}
