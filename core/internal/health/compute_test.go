package health

import (
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// FR-24's four states are a precedence order rather than four independent
// checks, so most of what can go wrong here is a verdict that is perfectly
// defensible on its own and wrong next to another one.
//
// That is why so much of this file is pairs rather than single cases. A
// QUARANTINED_LOST artifact is put next to a fresh known-good backup and
// then next to a stale set; a retrying failure is put next to a stuck one;
// a quarantined newest arrival is put next to an older good backup that is
// still perfectly fine. Read alone, each of those has an answer that looks
// reasonable. Only the pairing says which one is supposed to win.
//
// The stale window has both its edges asserted, exactly at the threshold
// and one nanosecond past it, because "inside the window" is where an
// off-by-one hides and the two answers either side of it are a page an
// operator gets and a page they do not.
//
// TestInjectedInputsPassThroughUnchangedAndNeverAffectState is the one
// guarding the package doc's central claim. Free space and version strings
// arrive as caller-supplied inputs, and this checks they reach the report
// and move no verdict. It is the readable half of a separation that
// evidence's own shape already enforces structurally, and it is here so a
// reader can see the claim tested rather than only argued.

var testSet = mustSet("prod", "postgres-primary")

func mustSet(source, set string) model.BackupSetID {
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		panic(err)
	}
	return id
}

func mustArtifact(name string) model.ArtifactID {
	id, err := model.NewArtifactID(testSet, name)
	if err != nil {
		panic(err)
	}
	return id
}

// rec builds a minimal journal record: just enough for the health
// computations under test to read (Artifact, State, DiscoveredAt,
// UpdatedAt, NextRetryAt). Every other state.Record field is left zero,
// which is fine because ComputeBackupSetHealth never reads them.
func rec(name string, st lifecycle.State, discovered, updated time.Time) state.Record {
	return state.Record{
		Artifact:     mustArtifact(name),
		State:        string(st),
		DiscoveredAt: discovered,
		UpdatedAt:    updated,
	}
}

func withRetry(r state.Record, at time.Time) state.Record {
	r.NextRetryAt = &at
	return r
}

const day = 24 * time.Hour

func TestNoRecordsIsDegradedNotStale(t *testing.T) {
	now := time.Now().UTC()
	got := ComputeBackupSetHealth(testSet, nil, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Degraded {
		t.Fatalf("State = %s, want %s (a set with no backups yet must never read as STALE, which implies backups stopped)", got.State, Degraded)
	}
	if got.NewestGoodBackupAt != nil || got.NewestGoodBackupAge != nil {
		t.Fatalf("expected no known-good backup, got %+v / %+v", got.NewestGoodBackupAt, got.NewestGoodBackupAge)
	}
	if got.LastCompletedBackupAt != nil {
		t.Fatalf("expected no completed backup, got %v", got.LastCompletedBackupAt)
	}
}

func TestRecentFirstAttemptInProgressIsDegradedNotStale(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("a.dump", lifecycle.Transferring, now.Add(-time.Hour), now.Add(-time.Minute)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Degraded {
		t.Fatalf("State = %s, want %s (first backup still in flight, well within the stale window)", got.State, Degraded)
	}
}

func TestNoGoodBackupAndNoRecentActivityIsStale(t *testing.T) {
	now := time.Now().UTC()
	// DISCOVERED and simply never advanced: no acute failure needing
	// intervention (that would be FAILING), just quiet for days.
	records := []state.Record{
		rec("a.dump", lifecycle.Discovered, now.Add(-10*day), now.Add(-9*day)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Stale {
		t.Fatalf("State = %s, want %s (no known-good backup, and nothing has happened in days)", got.State, Stale)
	}
}

func TestFreshKnownGoodBackupIsHealthy(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("a.dump", lifecycle.Complete, now.Add(-2*day), now.Add(-time.Hour)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Healthy {
		t.Fatalf("State = %s, want %s", got.State, Healthy)
	}
	if got.NewestGoodBackupAt == nil || !got.NewestGoodBackupAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("NewestGoodBackupAt = %v, want %v", got.NewestGoodBackupAt, now.Add(-time.Hour))
	}
	if got.NewestGoodBackupAge == nil || *got.NewestGoodBackupAge != time.Hour {
		t.Fatalf("NewestGoodBackupAge = %v, want %v", got.NewestGoodBackupAge, time.Hour)
	}
	if got.LastCompletedBackupAt == nil || !got.LastCompletedBackupAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("LastCompletedBackupAt = %v, want %v", got.LastCompletedBackupAt, now.Add(-time.Hour))
	}
}

func TestCommittedButNotYetRemoteDeletedCountsAsKnownGood(t *testing.T) {
	// FR-19: a restore point is good once durably committed locally,
	// whether or not the remote source has been deleted yet.
	now := time.Now().UTC()
	records := []state.Record{
		rec("a.dump", lifecycle.Committed, now.Add(-2*day), now.Add(-time.Hour)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Healthy {
		t.Fatalf("State = %s, want %s", got.State, Healthy)
	}
	if got.LastCompletedBackupAt != nil {
		t.Fatalf("LastCompletedBackupAt should stay nil for COMMITTED (not COMPLETE), got %v", got.LastCompletedBackupAt)
	}
	if got.NewestGoodBackupAt == nil {
		t.Fatalf("NewestGoodBackupAt should be set for COMMITTED")
	}
}

func TestExactlyAtStaleThresholdIsStillFresh(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("a.dump", lifecycle.Complete, now.Add(-2*day), now.Add(-day)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Healthy {
		t.Fatalf("State = %s, want %s (age == threshold should be inclusive-fresh)", got.State, Healthy)
	}
}

func TestOneNanosecondPastStaleThresholdIsStale(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("a.dump", lifecycle.Complete, now.Add(-2*day), now.Add(-day-time.Nanosecond)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Stale {
		t.Fatalf("State = %s, want %s", got.State, Stale)
	}
}

func TestFreshGoodBackupWithQuarantinedNewestArrivalIsDegradedNotHealthy(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("a.dump", lifecycle.Complete, now.Add(-2*day), now.Add(-time.Hour)),
		rec("b.dump", lifecycle.Quarantined, now.Add(-time.Minute), now.Add(-time.Minute)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Degraded {
		t.Fatalf("State = %s, want %s (something arrived but isn't trustworthy, so this is not HEALTHY)", got.State, Degraded)
	}
	if got.QuarantinedCount != 1 {
		t.Fatalf("QuarantinedCount = %d, want 1", got.QuarantinedCount)
	}
	if got.QuarantinedLostCount != 0 {
		t.Fatalf("QuarantinedLostCount = %d, want 0", got.QuarantinedLostCount)
	}
}

func TestQuarantinedLostAlwaysFailsEvenWithFreshGoodBackup(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		// An older artifact from the same set was lost irrecoverably...
		rec("old.dump", lifecycle.QuarantinedLost, now.Add(-30*day), now.Add(-29*day)),
		// ...but a newer one is a perfectly good, fresh restore point.
		rec("new.dump", lifecycle.Complete, now.Add(-2*day), now.Add(-time.Hour)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Failing {
		t.Fatalf("State = %s, want %s: QUARANTINED_LOST must never read as merely DEGRADED, no matter how fresh other backups are", got.State, Failing)
	}
	if got.QuarantinedCount != 1 {
		t.Fatalf("QuarantinedCount = %d, want 1 (machine.go: FR-24's quarantined count should count QUARANTINED_LOST too)", got.QuarantinedCount)
	}
	if got.QuarantinedLostCount != 1 {
		t.Fatalf("QuarantinedLostCount = %d, want 1", got.QuarantinedLostCount)
	}
}

func TestQuarantinedLostOutweighsStale(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("old.dump", lifecycle.QuarantinedLost, now.Add(-30*day), now.Add(-29*day)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Failing {
		t.Fatalf("State = %s, want %s", got.State, Failing)
	}
}

func TestStuckFailureIsFailingEvenWithFreshGoodBackup(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("good.dump", lifecycle.Complete, now.Add(-2*day), now.Add(-time.Hour)),
		rec("stuck.dump", lifecycle.Failed, now.Add(-time.Minute), now.Add(-time.Minute)), // no NextRetryAt: exhausted
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Failing {
		t.Fatalf("State = %s, want %s (a FAILED artifact with no retry scheduled needs a human)", got.State, Failing)
	}
	if got.Failures != 1 {
		t.Fatalf("Failures = %d, want 1", got.Failures)
	}
}

func TestRetryingFailureWithFreshGoodBackupIsDegradedNotFailing(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("good.dump", lifecycle.Complete, now.Add(-2*day), now.Add(-time.Hour)),
		withRetry(rec("retrying.dump", lifecycle.Failed, now.Add(-time.Minute), now.Add(-time.Minute)), now.Add(5*time.Minute)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Degraded {
		t.Fatalf("State = %s, want %s (a scheduled retry is not yet a FAILING condition)", got.State, Degraded)
	}
}

func TestPendingDeletesAndCurrentTransfersAreCounted(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("good.dump", lifecycle.Complete, now.Add(-2*day), now.Add(-time.Hour)),
		rec("deleting.dump", lifecycle.RemoteDeletePending, now.Add(-3*day), now.Add(-2*day)),
		rec("transferring.dump", lifecycle.Transferring, now.Add(-time.Minute), now.Add(-time.Minute)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.PendingDeletes != 1 {
		t.Fatalf("PendingDeletes = %d, want 1", got.PendingDeletes)
	}
	if len(got.CurrentTransfers) != 1 {
		t.Fatalf("CurrentTransfers = %+v, want 1 entry", got.CurrentTransfers)
	}
	if got.CurrentTransfers[0].Artifact.Name != "transferring.dump" {
		t.Fatalf("CurrentTransfers[0].Artifact = %+v, want transferring.dump", got.CurrentTransfers[0].Artifact)
	}
}

func TestInjectedInputsPassThroughUnchangedAndNeverAffectState(t *testing.T) {
	now := time.Now().UTC()
	poll := now.Add(-time.Minute)
	retention := now.Add(-2 * day)
	free := uint64(123456)

	// This backup set would be STALE on evidence alone: no known-good
	// backup, nothing recent, and no acute failure needing intervention.
	records := []state.Record{
		rec("a.dump", lifecycle.Discovered, now.Add(-10*day), now.Add(-9*day)),
	}
	in := BackupSetInputs{
		LastSuccessfulPollAt: &poll,
		LastRetentionRunAt:   &retention,
		FreeBytes:            &free,
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, in, now)

	if got.State != Stale {
		t.Fatalf("State = %s, want %s: a recent successful poll must not paper over a stale backup (invariant 14)", got.State, Stale)
	}
	if got.LastSuccessfulPollAt == nil || !got.LastSuccessfulPollAt.Equal(poll) {
		t.Fatalf("LastSuccessfulPollAt = %v, want %v", got.LastSuccessfulPollAt, poll)
	}
	if got.LastRetentionRunAt == nil || !got.LastRetentionRunAt.Equal(retention) {
		t.Fatalf("LastRetentionRunAt = %v, want %v", got.LastRetentionRunAt, retention)
	}
	if got.FreeBytes == nil || *got.FreeBytes != free {
		t.Fatalf("FreeBytes = %v, want %v", got.FreeBytes, free)
	}
}

func TestUnknownJournalStateStringDoesNotCrashOrCountAsGood(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		{
			Artifact:     mustArtifact("weird.dump"),
			State:        "SOME_FUTURE_STATE_THIS_BUILD_DOES_NOT_KNOW",
			DiscoveredAt: now.Add(-time.Hour),
			UpdatedAt:    now.Add(-time.Minute),
		},
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.NewestGoodBackupAt != nil {
		t.Fatalf("an unrecognized state must never be counted as known-good, got %v", got.NewestGoodBackupAt)
	}
	// Should not be HEALTHY: there is no positive evidence of a good backup.
	if got.State == Healthy {
		t.Fatalf("State = %s, an unrecognized state must not read as HEALTHY", got.State)
	}
}

func TestZeroOrNegativeStaleThresholdDoesNotPanic(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("a.dump", lifecycle.Complete, now.Add(-time.Hour), now.Add(-time.Second)),
	}
	for _, threshold := range []time.Duration{0, -time.Hour} {
		got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, threshold, BackupSetInputs{}, now)
		if got.State == Healthy {
			t.Fatalf("threshold %s: State = %s, a non-positive stale threshold should never read as HEALTHY", threshold, got.State)
		}
	}
}

func TestMultipleBackupSetsInputsStayIndependent(t *testing.T) {
	// ComputeBackupSetHealth takes records already scoped to one backup
	// set (ListByBackupSet's contract): confirm nothing here reaches
	// outside the slice it is given.
	now := time.Now().UTC()
	otherSet := mustSet("prod", "other-set")
	otherArtifact, err := model.NewArtifactID(otherSet, "x.dump")
	if err != nil {
		t.Fatal(err)
	}
	// Records for testSet only; otherArtifact is never included.
	records := []state.Record{
		rec("a.dump", lifecycle.Complete, now.Add(-time.Hour), now),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)
	if got.Set != testSet {
		t.Fatalf("Set = %v, want %v", got.Set, testSet)
	}
	for _, tr := range got.CurrentTransfers {
		if tr.Artifact == otherArtifact {
			t.Fatalf("leaked an artifact from another backup set: %v", tr.Artifact)
		}
	}
}

// TestRemoteRetainedCountsAsKnownGoodAndIsCounted is issue #282's health
// proof: a read-only backup set whose only artifact ever reaches
// REMOTE_RETAINED (never COMPLETE, by design) must still read HEALTHY, not
// STALE -- the whole point of the feature is that this manager keeps
// producing valid backups it will simply never delete the remote copy
// of -- and ReadOnlyRetainedCount must report it, the way
// ReinstatedRemoteRetainedCount already reports #227's shape.
func TestRemoteRetainedCountsAsKnownGoodAndIsCounted(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("a.dump", lifecycle.RemoteRetained, now.Add(-2*day), now.Add(-time.Hour)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Healthy {
		t.Fatalf("State = %s, want %s: a read-only set with a fresh REMOTE_RETAINED backup is exactly as healthy as one with a fresh COMPLETE backup (reason: %s)", got.State, Healthy, got.Reason)
	}
	if got.NewestGoodBackupAt == nil {
		t.Fatalf("NewestGoodBackupAt should be set for REMOTE_RETAINED")
	}
	if got.LastCompletedBackupAt != nil {
		t.Fatalf("LastCompletedBackupAt should stay nil for REMOTE_RETAINED (not COMPLETE), got %v", got.LastCompletedBackupAt)
	}
	if got.ReadOnlyRetainedCount != 1 {
		t.Fatalf("ReadOnlyRetainedCount = %d, want 1", got.ReadOnlyRetainedCount)
	}
}

// TestReadOnlyRetainedCountDefaultsZero pins the regression this whole
// issue depends on at the health layer too: a backup set that has never
// retained anything reports zero, the same silent default every existing
// deployment already reads.
func TestReadOnlyRetainedCountDefaultsZero(t *testing.T) {
	now := time.Now().UTC()
	records := []state.Record{
		rec("a.dump", lifecycle.Complete, now.Add(-2*day), now.Add(-time.Hour)),
	}
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)
	if got.ReadOnlyRetainedCount != 0 {
		t.Fatalf("ReadOnlyRetainedCount = %d, want 0", got.ReadOnlyRetainedCount)
	}
}
