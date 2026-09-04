package placement_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Every test in this file is the data-loss path #241 named, driven through
// the real engine rather than through archive.CheckSourceDelete alone.
//
// The world is deliberately as correct as it can be. The destination is
// really on the medium, the journal really records it ACTIVE and
// content-verified, that record is TRUE (the fake serves the bytes, so the
// fresh re-verification deleteSource runs before the guard passes), and
// every one of the guard's seven journal-and-filesystem clauses passes.
// The one and only thing wrong is that the destination is on an archive
// class, so its bytes cannot be read until somebody pays for a restore,
// and the journal has no column that says so.
//
// So these are tests of the eighth clause and nothing else. The
// restored-copy test is the positive control: it flips exactly the fact
// the refusal is about, and the same move finishes.

// TestTheGuardWillNotLeaveOnlyAnArchivedCopyNobodyCanRead is the refusal.
func TestTheGuardWillNotLeaveOnlyAnArchivedCopyNobodyCanRead(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	a := deleteAttempt{wantRefusal: "requires_restore"}
	plantSourceDeletePending(t, f, a)
	refusal := runDeleteAttempt(t, f, a)

	if got := f.medium.restoreStatusCount(); got != 1 {
		t.Errorf("the medium was asked about a restore %d times, want exactly 1; an archive-class survivor has to be asked, and only once", got)
	}
	// The refusal names the class, so an operator reading it knows what
	// to do (ask for a restore) rather than what went wrong (nothing).
	if !contains(refusal, config.StorageClassDeepArchive) {
		t.Errorf("the refusal does not name the storage class that is the reason: %q", refusal)
	}
	if !contains(refusal, "verified and retrievable right now") {
		t.Errorf("the refusal is not archive's own: %q", refusal)
	}
}

// TestARestoredArchivedCopyLetsTheMoveFinish is the positive control, and
// it changes exactly one fact: the medium says a restore of the
// destination is in effect and will be for two days.
//
// It has to finish, or the eighth clause is not a guard, it is a refusal
// to ever move anything to an archive class. And it is what makes the
// restore probe load-bearing: an engine that never asked the medium would
// read the destination as requires_restore in this world too, and refuse.
func TestARestoredArchivedCopyLetsTheMoveFinish(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	until := testNow2.Add(48 * time.Hour)
	f.medium.restore = &transport.RestoreState{ExpiresAt: &until}

	a := deleteAttempt{}
	plantSourceDeletePending(t, f, a)

	report, err := f.engine.RunCycle(f.ctx, nil)
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	f.guard.fail()

	mv := f.onlyMove()
	if placement.Phase(mv.Phase) != placement.Done {
		t.Fatalf("the move ended at %s with %q; a restored, content-verified copy is exactly what may stand in for the source. Outcomes: %+v", mv.Phase, mv.Error, report.Outcomes)
	}
	if f.localExists() {
		t.Error("the move reached DONE and the local source copy is still there")
	}
	if !f.medium.has(f.key) {
		t.Error("the destination object is gone after a completed move")
	}
	if got := f.medium.restoreStatusCount(); got != 1 {
		t.Errorf("the medium was asked about a restore %d times, want exactly 1", got)
	}
}

// TestACopyStillRestoringCannotStandInYet: a restore that has been asked
// for and has not finished leaves the bytes exactly as unreadable as no
// restore at all, and the vocabulary has a word for it.
func TestACopyStillRestoringCannotStandInYet(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassGlacier})
	f.medium.restore = &transport.RestoreState{InProgress: true}
	a := deleteAttempt{wantRefusal: "restoring"}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

// TestAnArchiveMediumThatWillNotSayIsUnreachableAndTheSourceStays keeps
// the two "not now" answers apart. The content re-verification just read
// the bytes, so the endpoint is up; the status call fails anyway, and the
// honest description of a copy whose restore state cannot be learned is
// unreachable, not "probably fine".
func TestAnArchiveMediumThatWillNotSayIsUnreachableAndTheSourceStays(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	f.medium.restoreErr = errors.New("restore-status: the endpoint reset the connection")
	a := deleteAttempt{wantRefusal: "unreachable"}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

// TestAnExpiredRestoreWindowReadsAsArchivedAgain is the row the archive
// lane called the one an implementation gets wrong by accident: a restore
// expires, the object goes back to being unreadable, and nothing in the
// journal changed to say so.
func TestAnExpiredRestoreWindowReadsAsArchivedAgain(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	expired := testNow2.Add(-time.Hour)
	f.medium.restore = &transport.RestoreState{ExpiresAt: &expired}
	a := deleteAttempt{wantRefusal: "requires_restore"}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

// TestACopyOnAnOnDemandClassIsNotAskedAboutARestore pins that the probe is
// spent only where the answer matters. GLACIER_IR is the class that makes
// this worth a test: the name says archive and the class serves on demand.
func TestACopyOnAnOnDemandClassIsNotAskedAboutARestore(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassGlacierIR})
	report := f.runCycle()
	f.guard.fail()
	if report.Completed != 1 {
		t.Fatalf("a move to an on-demand class did not complete: %+v", report.Outcomes)
	}
	if got := f.medium.restoreStatusCount(); got != 0 {
		t.Errorf("the medium was asked about a restore %d times for a class that reads on demand; the answer could not change anything", got)
	}
}

// TestASourceAlreadyGoneConvergesEvenWhenTheSurvivorIsArchived pins the
// order of the guard's last two clauses, which is an argument made in a
// comment and would otherwise be a comment.
//
// A crash between the source delete landing on disk and the DONE write
// leaves the journal at SOURCE_DELETE_PENDING and a file that no longer
// exists. The medium-specific proof reports that as errSourceAlreadyGone,
// and the answer is to record DONE: the delete happened, and there is
// nothing left to protect. If the readable-survivor clause ran FIRST, an
// unrestored archive-class destination would refuse this move on every
// cycle for ever, over a copy that is already gone, with a source row that
// says DELETE_PENDING about nothing. #372's shape, one machine down.
func TestASourceAlreadyGoneConvergesEvenWhenTheSurvivorIsArchived(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	a := deleteAttempt{afterPlant: func(t *testing.T, f *fixture) {
		if err := os.Remove(f.localPath()); err != nil {
			t.Fatalf("removing the source to simulate the crash: %v", err)
		}
	}}
	plantSourceDeletePending(t, f, a)

	report, err := f.engine.RunCycle(f.ctx, nil)
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	mv := f.onlyMove()
	if placement.Phase(mv.Phase) != placement.Done {
		t.Fatalf("a source that was already gone left the move at %s (%+v); the delete happened and the journal has to say so", mv.Phase, report.Outcomes)
	}
	if got := f.medium.restoreStatusCount(); got != 0 {
		t.Errorf("the medium was asked about a restore %d times for a delete that had already happened", got)
	}
}
