package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// This file is issue #372: an artifact left at COMMITTING by a process
// that died inside lifecycle.Commit, and what the real pipeline does with
// it on the next cycle.
//
// The crash matrix already proves lifecycle.Commit resumes from COMMITTING
// (TestCrash_DuringCommit_Fuzz). It proves it about its own harness,
// though, because core/tests/crashmatrix/harness carries a second state
// machine and that one lists COMMITTING alongside VERIFIED.
// processArtifact, which is what an operator's `run` and `daemon`
// actually execute, listed only VERIFIED. So the suite proved the resume
// exists and nothing proved the product ever reached it. Everything here
// goes through RunCycle for exactly that reason.

// wedgeAtCommitting runs one real cycle that gets all the way to
// lifecycle.Commit and fails inside it, leaving the journal row at
// COMMITTING with its .partial file on disk and the CommittingKey already
// spent. That is precisely what a process killed between Commit's first
// journal write and its last leaves behind, which is the state issue #372
// is about, and the transport double is only used to open the window in
// which it can be produced in-process: a stray file appearing at the final
// path after lifecycle.Transfer's collision guard has already run and
// before Commit's rename. commitFile has a named error for that exact
// race (FinalPathCollisionError), so the cause is real too, not only the
// state it leaves.
//
// It returns the artifact and the stray file's path, so a caller can
// decide whether the second cycle finds the obstruction cleared or still
// there.
func wedgeAtCommitting(t *testing.T, svc *Service, tr *fakeTransport, localDir string) (model.ArtifactID, string) {
	t.Helper()
	stray := filepath.Join(localDir, "backup.dump")
	tr.afterCopyToLocal = func(string) {
		mustWriteFile(t, stray, "somebody else's file, sitting on the final name")
	}

	report := svc.RunCycle(context.Background())
	tr.afterCopyToLocal = nil

	if len(report.Sets) != 1 {
		t.Fatalf("len(report.Sets) = %d, want 1", len(report.Sets))
	}
	if len(report.Sets[0].Discovery.Discovered) != 1 {
		t.Fatalf("Discovery.Discovered = %+v, want exactly one artifact", report.Sets[0].Discovery.Discovered)
	}
	artifact := report.Sets[0].Discovery.Discovered[0].Artifact

	rec, err := svc.Journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Committing) {
		t.Fatalf("fixture: journal state = %q, want %q; the first cycle was supposed to die inside the commit",
			rec.State, lifecycle.Committing)
	}
	partial := filepath.Join(localDir, "backup.dump.partial")
	if rec.LocalPath != partial {
		t.Fatalf("fixture: recorded local path = %q, want the .partial %q", rec.LocalPath, partial)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("fixture: the transferred bytes are not at %s: %v", partial, err)
	}
	return artifact, stray
}

// TestRunCycle_ResumesAnArtifactLeftAtCommitting is issue #372's first
// acceptance criterion, driven through the product rather than the crash
// matrix's harness: the obstruction that stopped the first commit is gone
// by the time the second cycle runs, so the artifact must go all the way
// to COMPLETE.
//
// Against the code before the fix this fails on the assertion below with
// the row still at COMMITTING, because processArtifact reached its commit
// step only from VERIFIED and its trailing switch had no case for
// COMMITTING either, so the row fell through to default and returned.
func TestRunCycle_ResumesAnArtifactLeftAtCommitting(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup.dump", "commit resume payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	artifact, stray := wedgeAtCommitting(t, svc, tr, localDir)

	// Whatever was on the final name is gone by the next cycle. Moved
	// rather than deleted, so the fixture never destroys anything, and
	// out of localDir so the cycle cannot see it again.
	if err := os.Rename(stray, filepath.Join(t.TempDir(), "moved-aside")); err != nil {
		t.Fatalf("clearing the final path before the resuming cycle: %v", err)
	}

	svc.RunCycle(context.Background())

	rec, err := journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Complete) {
		t.Fatalf("after a second cycle the artifact is at %q, want %q: a row left at COMMITTING by a crash "+
			"is never picked up again by the pipeline an operator actually runs, so it sits there forever "+
			"while its bytes are on disk (issue #372)", rec.State, lifecycle.Complete)
	}
	final := filepath.Join(localDir, "backup.dump")
	if _, err := os.Stat(final); err != nil {
		t.Errorf("the durable local copy is not at %s: %v", final, err)
	}
	if _, err := os.Stat(final + ".partial"); !os.IsNotExist(err) {
		t.Errorf("the .partial file is still at %s (err = %v): the commit never finished", final+".partial", err)
	}
}

// TestRunCycle_ACommittingRowItCannotMoveIsVisibleInTheCycleVerdict is
// issue #372's last acceptance criterion: "a row the pipeline cannot move
// is not silently invisible to the cycle's own outcome".
//
// Here the obstruction is still there on the second cycle, so the commit
// fails again and the row is still at COMMITTING afterwards. That is now a
// counted attempt that did not land, so the set's verdict says nothing got
// through, which is what an operator's exit status and the FR-23 stream
// are built on (issues #283 and #361).
//
// Against the code before the fix this fails on the Walked assertion:
// acquiring() left COMMITTING out on the grounds that processArtifact
// could not act on it, so the row counted as neither work attempted nor
// work landed and the cycle reported itself perfectly healthy.
func TestRunCycle_ACommittingRowItCannotMoveIsVisibleInTheCycleVerdict(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup.dump", "still blocked payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	artifact, _ := wedgeAtCommitting(t, svc, tr, localDir)

	// The stray file stays exactly where it is, so this cycle's commit
	// attempt fails the same way the first one did.
	report := svc.RunCycle(context.Background())
	if len(report.Sets) != 1 {
		t.Fatalf("len(report.Sets) = %d, want 1", len(report.Sets))
	}
	set := report.Sets[0]

	rec, err := journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Committing) {
		t.Fatalf("precondition: journal state = %q, want %q (the obstruction is still there, so this "+
			"cycle's commit must fail too)", rec.State, lifecycle.Committing)
	}

	if set.Progress.Walked != 1 {
		t.Errorf("Progress.Walked = %d, want 1: a COMMITTING row is work this cycle attempted and did "+
			"not land, and a cycle that reports zero walked rows reports itself healthy while an "+
			"artifact whose bytes are on disk goes nowhere", set.Progress.Walked)
	}
	if set.Progress.Durable != 0 {
		t.Errorf("Progress.Durable = %d, want 0: nothing was committed", set.Progress.Durable)
	}
	if !set.Verdict().NothingGotThrough() {
		t.Errorf("CycleVerdict.NothingGotThrough() = false for a cycle whose only artifact could not be "+
			"committed; verdict = %+v", set.Verdict())
	}
}

// TestAcquiring_CountsCommittingAsWorkInFlight pins the classification
// itself, next to the two states around it, so the reason for it cannot be
// lost in a later edit to processArtifact. COMMITTED is the boundary: at
// that point the bytes are durable and the backup has happened, which is
// why it is deliberately not counted (see acquiring's own doc).
func TestAcquiring_CountsCommittingAsWorkInFlight(t *testing.T) {
	for _, tc := range []struct {
		state lifecycle.State
		want  bool
		why   string
	}{
		{lifecycle.Verified, true, "verified but not yet committed is work that has not landed"},
		{lifecycle.Committing, true, "processArtifact resumes a COMMITTING row now (issue #372), so a row still sitting there is work this cycle attempted and did not land"},
		{lifecycle.Committed, false, "the bytes are durable; what is left is remote cleanup"},
		{lifecycle.Complete, false, "a finished backup is not work in flight"},
	} {
		if got := acquiring(tc.state); got != tc.want {
			t.Errorf("acquiring(%s) = %v, want %v: %s", tc.state, got, tc.want, tc.why)
		}
	}
}

// TestRunCycle_ResumingACommittingRowReusesTheSameCommittingKey is the
// mechanism the resume rests on, asserted rather than assumed. If the
// second cycle derived a different key, lifecycle.Commit's first Advance
// would try to apply a fresh VERIFIED -> COMMITTING transition against a
// row that is already at COMMITTING, and the resume branch its comment
// describes would never be reached at all.
func TestRunCycle_ResumingACommittingRowReusesTheSameCommittingKey(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup.dump", "same key payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	artifact, stray := wedgeAtCommitting(t, svc, tr, localDir)

	wedged, err := journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	keyBefore := attemptKey(wedged) + ":committing"

	if err := os.Rename(stray, filepath.Join(t.TempDir(), "moved-aside")); err != nil {
		t.Fatalf("clearing the final path: %v", err)
	}
	svc.RunCycle(context.Background())

	after, err := journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := attemptKey(after) + ":committing"; got != keyBefore {
		t.Fatalf("the resuming cycle derives CommittingKey %q, the crashed attempt used %q: "+
			"the two must match or lifecycle.Commit's resume branch is unreachable", got, keyBefore)
	}
	if after.RetryCount != wedged.RetryCount {
		t.Errorf("RetryCount moved from %d to %d across the resume; attemptKey is derived from it",
			wedged.RetryCount, after.RetryCount)
	}
}
