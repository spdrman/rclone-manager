package app

import (
	"context"
	"testing"
)

// The one denominator a per-set activity strip can put a bar behind
// (issue #573).
//
// Progress already reports how many backup sets a cycle will visit, and
// its own doc explains at length why there is no total for the cycle's
// artifacts: what a cycle will find is discovered set by set as it goes.
// That argument holds for the CYCLE and stops holding for a SET. Once a
// set has reconciled and discovered, the rows its pass is about to walk
// are on the table and countable, so "26 of 41" is a fact rather than a
// guess.
//
// These cases pin the two halves of that fact and the window before it
// exists. The window matters most: a bar that reads zero of zero while
// discovery is still running is a bar reporting a finished cycle, and it
// is exactly the reading an operator would act on.

func TestRunCycle_ReportsHowManyArtifactsTheSetItselfWillWalk(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := reportingTransport{fakeTransport: newFakeTransport()}
	tr.put("backup.dump", "cycle payload for progress", epoch.Unix())
	tr.put("backup2.dump", "a second payload for the same set", epoch.Unix())

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	obs := &recordingObserver{}
	report := svc.RunCycle(WithProgressObserver(context.Background(), obs))
	if len(report.Sets) != 1 || report.Sets[0].Err != nil {
		t.Fatalf("cycle did not run cleanly: %+v", report.Sets)
	}

	readings := obs.all()
	if len(readings) == 0 {
		t.Fatal("the cycle published no progress at all")
	}

	// Before the walk begins there is no total, and "no total" is not
	// zero. A nil here is what lets a client draw an indeterminate bar
	// instead of a full one.
	if first := readings[0]; first.SetArtifactsPlanned != nil {
		t.Errorf("the first reading already reports a per-set total of %d; nothing has been discovered yet, so there is nothing to have counted",
			*first.SetArtifactsPlanned)
	}

	var planned *int
	for _, r := range readings {
		if r.SetArtifactsPlanned != nil {
			planned = r.SetArtifactsPlanned
			break
		}
	}
	if planned == nil {
		t.Fatal("no reading ever reported how many artifacts this set's pass would walk, so a per-set progress bar has no denominator and cannot be drawn honestly")
	}
	if *planned != 2 {
		t.Errorf("the set's pass reported %d artifacts to walk, and two were discovered", *planned)
	}

	if last := readings[len(readings)-1]; last.SetArtifactsCompleted != 2 {
		t.Errorf("the final reading reports SetArtifactsCompleted = %d, want 2: a numerator that never reaches its own denominator leaves a bar short of full on a cycle that finished",
			last.SetArtifactsCompleted)
	}

	// The numerator never runs past the denominator, at any point.
	for i, r := range readings {
		if r.SetArtifactsPlanned == nil {
			continue
		}
		if r.SetArtifactsCompleted > *r.SetArtifactsPlanned {
			t.Fatalf("reading %d reports %d of %d artifacts done, which is more than the pass said it would walk",
				i, r.SetArtifactsCompleted, *r.SetArtifactsPlanned)
		}
	}
}

func TestRunCycle_CountsEachSetsArtifactsSeparately(t *testing.T) {
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	first := testBackupSet(t, firstDir)
	first.RemotePath = ""
	second := testBackupSet(t, secondDir)
	second.RemotePath = ""
	second.ID = mustSetID(t, "staging", "postgres-primary")

	tr := reportingTransport{fakeTransport: newFakeTransport()}
	tr.put("backup.dump", "one payload", epoch.Unix())

	svc := New(testConfig(t, testSource("production", first), testSource("staging", second)),
		openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	obs := &recordingObserver{}
	svc.RunCycle(WithProgressObserver(context.Background(), obs))

	// Entering the second set resets the per-set counters: they describe
	// the set named on the reading, and carrying the first set's numbers
	// into the second would put a bar that is already full in front of a
	// pass that has not started.
	sawSecond := false
	for _, r := range obs.all() {
		if r.BackupSetID == "" || r.BackupSetID == "production/postgres-primary" {
			continue
		}
		if !sawSecond {
			sawSecond = true
			if r.SetArtifactsCompleted != 0 {
				t.Errorf("the first reading for %s already reports %d artifacts done", r.BackupSetID, r.SetArtifactsCompleted)
			}
			if r.SetArtifactsPlanned != nil {
				t.Errorf("the first reading for %s already reports a total of %d", r.BackupSetID, *r.SetArtifactsPlanned)
			}
		}
	}
	if !sawSecond {
		t.Fatal("no reading named the second backup set, so this test compared nothing")
	}
}
