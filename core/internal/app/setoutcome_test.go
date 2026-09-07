package app

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// How a set's pass ENDED, as opposed to how many of its artifacts failed
// (issue #573, and the review of the strip that shipped it).
//
// The strip on the dashboard drew its headline from two numbers, the
// failure count and the fraction, and both of them are zero and absent
// for the pass that goes wrong earliest. A set whose reconcile or
// discovery failed never reaches an artifact, so it counts nothing and
// plans nothing, and a panel reading only those two paints it exactly the
// way it paints a set with nothing to do. The pass's own verdict is the
// missing fact, and these cases are about it reaching a reader.

// outcomeObserver is recordingObserver plus the half a strip needs: it
// also collects how each set's pass ended.
type outcomeObserver struct {
	mu       sync.Mutex
	readings []Progress
	outcomes map[string]string
}

func newOutcomeObserver() *outcomeObserver {
	return &outcomeObserver{outcomes: map[string]string{}}
}

func (o *outcomeObserver) ObserveProgress(p Progress) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.readings = append(o.readings, p)
}

func (o *outcomeObserver) ObserveSetOutcome(backupSetID, outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outcomes[backupSetID] = outcome
}

func (o *outcomeObserver) outcomeOf(id string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.outcomes[id]
}

// TestRunCycle_SaysHowEachSetsPassEnded is the claim, on the two passes
// that end differently with identical numbers behind them.
func TestRunCycle_SaysHowEachSetsPassEnded(t *testing.T) {
	brokenDir := t.TempDir()
	brokenBS := testBackupSet(t, brokenDir)
	brokenBS.Name = "broken"
	brokenBS.ID = mustSetID(t, "production", "broken")
	brokenBS.RemotePath = ""

	healthyDir := t.TempDir()
	healthyBS := testBackupSet(t, healthyDir)
	healthyBS.Name = "healthy"
	healthyBS.ID = mustSetID(t, "production", "healthy")
	healthyBS.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup.dump", "healthy payload", epoch.Unix())
	tr.failErr = errors.New("boom: this source's remote is entirely unreachable")
	tr.failForSourceID = brokenBS.ID.String()

	svc := New(testConfig(t, testSource("production", brokenBS, healthyBS)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	obs := newOutcomeObserver()
	report := svc.RunCycle(WithProgressObserver(context.Background(), obs))
	if len(report.Sets) != 2 {
		t.Fatalf("the cycle reported %d sets and two were configured", len(report.Sets))
	}

	// The broken set failed before it reached an artifact, so it counted
	// no failures and planned no rows. Nothing but the pass's own verdict
	// separates it from the healthy one.
	if got := obs.outcomeOf(brokenBS.ID.String()); got != SetOutcomeFailed {
		t.Errorf("the set whose discovery failed reported outcome %q, want %q. It failed before it walked an artifact, so its failure count is zero and its denominator absent, and a strip reading only those two draws it as a set with nothing to do",
			got, SetOutcomeFailed)
	}
	if report.Sets[0].FailedArtifacts != 0 {
		t.Errorf("the broken set reports %d failed artifacts; this case is about the pass that fails with nothing to count", report.Sets[0].FailedArtifacts)
	}
	if got := obs.outcomeOf(healthyBS.ID.String()); got != SetOutcomeOK {
		t.Errorf("the set that ran cleanly reported outcome %q, want %q", got, SetOutcomeOK)
	}
}

// TestBackupSetCycleResult_OutcomeTellsStoppedFromFailed is the verdict
// itself, on the distinction SystemicFailure already exists to protect.
// A pass an operator stopped is something this manager was asked to do,
// and reporting it in the words a source that has gone unreachable gets
// is a false alarm in a product whose job is to be believed about
// backups.
func TestBackupSetCycleResult_OutcomeTellsStoppedFromFailed(t *testing.T) {
	for _, c := range []struct {
		name   string
		result BackupSetCycleResult
		want   string
	}{
		{"a pass that finished", BackupSetCycleResult{}, SetOutcomeOK},
		{"a reconcile that failed", BackupSetCycleResult{Err: errors.New("the source refused the connection")}, SetOutcomeFailed},
		{"artifacts that failed", BackupSetCycleResult{FailedArtifacts: 2}, SetOutcomeFailed},
		{"an edit hold", BackupSetCycleResult{Err: ErrBackupSetHeldForEditing}, SetOutcomeStopped},
		{"a cancelled pass", BackupSetCycleResult{Err: context.Canceled}, SetOutcomeStopped},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.result.Outcome(); got != c.want {
				t.Errorf("Outcome() = %q, want %q", got, c.want)
			}
		})
	}
}
