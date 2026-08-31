package health

import (
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Issue #227. A reinstated artifact permanently forfeits its remote
// delete, so its remote source accumulates on the producer's disk for as
// long as the deployment lives. FR-24 is where a standing condition an
// operator should be able to ask about later belongs, alongside the
// quarantined counts it already carries.
//
// The number has to mean "remote sources this manager is holding", not
// "reinstatements that happened". Those differ: QUARANTINED_LOST is
// reached only from COMPLETE, which is the state that says this manager
// already deleted the remote object, so reinstating one of those returns a
// restore point whose remote source is already gone. Counting it would
// tell an operator to go looking for storage that is not there.

func released(r state.Record, at time.Time) state.Record {
	r.RemoteDeletedAt = &at
	return r
}

// reinstatedIDs is the set-wide transition-log read a caller supplies. In
// production it is lifecycle.ReinstatedArtifacts; here it is spelled out,
// because what is under test is what ComputeBackupSetHealth does with it.
func reinstatedIDs(names ...string) []model.ArtifactID {
	out := make([]model.ArtifactID, 0, len(names))
	for _, n := range names {
		out = append(out, mustArtifact(n))
	}
	return out
}

// Five artifacts, three of them reinstated, one of those three with its
// remote source already released. The answer is two, which is a number
// that no simpler wrong rule produces: not five (every artifact), not
// three (every reinstatement), not one (a count that stops at the first
// match), and not zero.
func TestReinstatedRemoteRetainedCountsOnlySourcesStillHeld(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-time.Hour)

	records := []state.Record{
		// Reinstated, remote still held: counted.
		rec("reinstated-one.dump", lifecycle.Committed, recent, recent),
		rec("reinstated-two.dump", lifecycle.Committed, recent, recent),
		// Reinstated out of QUARANTINED_LOST, which is only reachable
		// from COMPLETE: this manager already deleted the remote object,
		// so there is nothing left for it to be holding.
		released(rec("reinstated-lost.dump", lifecycle.Complete, recent, recent), recent),
		// Quarantined and never reinstated. The control: it is in exactly
		// the population a count of "artifacts that have ever been
		// quarantined" would wrongly include.
		rec("still-quarantined.dump", lifecycle.Quarantined, recent, recent),
		// Never distrusted at all.
		rec("never-distrusted.dump", lifecycle.Committed, recent, recent),
	}

	in := BackupSetInputs{}
	got := ComputeBackupSetHealth(testSet, records,
		reinstatedIDs("reinstated-one.dump", "reinstated-two.dump", "reinstated-lost.dump"),
		day, in, now)

	if got.ReinstatedRemoteRetainedCount != 2 {
		t.Fatalf("ReinstatedRemoteRetainedCount = %d, want 2 (two reinstated artifacts whose remote source this manager has not released; a third was reinstated but its remote is already gone)",
			got.ReinstatedRemoteRetainedCount)
	}
}

// The positive control for the exclusion above: the same fixture with
// nothing released counts all three. Without this, the assertion that a
// released artifact is excluded would pass just as happily for a count
// that was broken in some other way and happened to land on two.
func TestReinstatedRemoteRetainedCountsEveryUnreleasedReinstatement(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-time.Hour)

	records := []state.Record{
		rec("reinstated-one.dump", lifecycle.Committed, recent, recent),
		rec("reinstated-two.dump", lifecycle.Committed, recent, recent),
		rec("reinstated-lost.dump", lifecycle.Complete, recent, recent),
		rec("still-quarantined.dump", lifecycle.Quarantined, recent, recent),
		rec("never-distrusted.dump", lifecycle.Committed, recent, recent),
	}

	got := ComputeBackupSetHealth(testSet, records,
		reinstatedIDs("reinstated-one.dump", "reinstated-two.dump", "reinstated-lost.dump"),
		day, BackupSetInputs{}, now)

	if got.ReinstatedRemoteRetainedCount != 3 {
		t.Fatalf("ReinstatedRemoteRetainedCount = %d, want 3: with no remote released, every reinstated artifact is still holding one",
			got.ReinstatedRemoteRetainedCount)
	}
}

// An artifact the transition log names but whose journal row is not in
// this set's records is not counted. The two reads are separate queries
// against the same database and a race between them (a row deleted by a
// catalog rebuild between the two) must not produce a count of something
// this pass cannot describe.
func TestReinstatedRemoteRetainedIgnoresAnArtifactWithNoRecord(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-time.Hour)

	records := []state.Record{
		rec("reinstated-one.dump", lifecycle.Committed, recent, recent),
	}

	got := ComputeBackupSetHealth(testSet, records,
		reinstatedIDs("reinstated-one.dump", "vanished.dump"),
		day, BackupSetInputs{}, now)

	if got.ReinstatedRemoteRetainedCount != 1 {
		t.Fatalf("ReinstatedRemoteRetainedCount = %d, want 1: an artifact with no journal row in this set cannot be described and must not be counted",
			got.ReinstatedRemoteRetainedCount)
	}
}

// Reinstatement is a standing condition, not an incident. FR-24's four
// states are decided by decideState from evidence alone, and evidence has
// no field this count could reach; the assertion here is behavioural
// proof of that structural fact. A set that is otherwise fresh and clean
// stays HEALTHY however many reinstated artifacts it holds, because the
// backups themselves are fine: what is accumulating is storage on the
// source, which is a thing to report, not a broken freshness guarantee.
func TestReinstatedRemoteRetainedDoesNotChangeTheHealthState(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-time.Hour)

	records := []state.Record{
		rec("reinstated-one.dump", lifecycle.Committed, recent, recent),
		rec("reinstated-two.dump", lifecycle.Committed, recent, recent),
		rec("reinstated-three.dump", lifecycle.Committed, recent, recent),
	}
	names := []string{"reinstated-one.dump", "reinstated-two.dump", "reinstated-three.dump"}

	with := ComputeBackupSetHealth(testSet, records, reinstatedIDs(names...), day, BackupSetInputs{}, now)
	without := ComputeBackupSetHealth(testSet, records, nil, day, BackupSetInputs{}, now)

	if with.ReinstatedRemoteRetainedCount != 3 {
		t.Fatalf("ReinstatedRemoteRetainedCount = %d, want 3", with.ReinstatedRemoteRetainedCount)
	}
	if with.State != Healthy {
		t.Errorf("State = %s (%s), want %s: a reinstated artifact is a restore point again, and holding its remote source is not a freshness failure",
			with.State, with.Reason, Healthy)
	}
	if with.State != without.State || with.Reason != without.Reason {
		t.Errorf("State/Reason changed with reinstatements present (%s / %q) versus absent (%s / %q); this count must never reach decideState",
			with.State, with.Reason, without.State, without.Reason)
	}
}
