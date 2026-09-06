// These cover the local half of revalidation: scheduling, and re-checking an
// artifact whose durable copy is a file on disk. The medium half is in
// medium_test.go.
//
// The fixtures drive artifacts to their durable states through the REAL
// journal, one transition at a time, rather than inserting a row that says
// COMMITTED. That is more code than it looks like it needs to be and it is
// deliberate: this package's whole scheduling decision reads UpdatedAt as a
// proxy for "when was this last looked at", and UpdatedAt is a value the
// journal computes from the transitions it is given. A hand-built record
// would let a test pass while the real thing selected nothing.
//
// Every test pins the clock and expresses times as offsets from it, so the
// interval boundary is exact. The recurring assertion to watch for is the
// one about UpdatedAt after a pass that checked NOTHING: several tests run
// twice and assert that the artifact is still selected the second time,
// which is how the checked-versus-passed distinction is proved rather than
// merely described.
package revalidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// --- fixtures ---

// openJournal opens a real SQLite journal per test. See this file's header
// for why these tests use the real one: UpdatedAt is the journal's own
// computation, and it is the value every scheduling decision here turns on.
func openJournal(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// artifactNamed builds an id in one fixed backup set, so every artifact a
// test creates is enumerated by the same ListByBackupSet call Run makes.
func artifactNamed(t *testing.T, name string) model.ArtifactID {
	t.Helper()
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	id, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return id
}

// sha256Hex is the hash the fixtures record as a verification baseline. It
// computes the same digest recomputeLocalHash does, which is what makes an
// unmodified file pass and a modified one fail without either side being
// told the answer.
func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// mustScript writes an executable shell script for the restore-test hook
// tests to run.
//
// A real executable rather than a stubbed hook interface, because the
// distinction these tests exist to pin lives at the process boundary: a hook
// that runs and exits non-zero is a verdict about the artifact, and a hook
// that cannot be executed at all is an infrastructure error that must not
// quarantine anything. Only a real file can be made to fail the second way.
func mustScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	return path
}

// commitArtifact drives artifact through the real nominal FR-11/FR-13/FR-14
// sequence up to COMMITTED, with content actually written to disk and a
// real recorded hash, stamping every transition (and so, in the end,
// UpdatedAt) with occurredAt. It returns the local final path.
func commitArtifact(t *testing.T, j *state.Journal, artifact model.ArtifactID, content []byte, occurredAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	localPath := filepath.Join(t.TempDir(), artifact.Name)
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatalf("writing local file: %v", err)
	}
	size := int64(len(content))
	sum := sha256Hex(content)

	if _, err := j.Discover(ctx, artifact, artifact.String()+":discover", "backups/"+artifact.Name, state.RemoteIdentity{Size: &size}, occurredAt); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, tr := range []state.Transition{
		{Artifact: artifact, Key: artifact.String() + ":transferring", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: occurredAt, LocalPath: &localPath},
		{Artifact: artifact, Key: artifact.String() + ":transferred", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: occurredAt, Transfer: &state.TransferResult{BytesTransferred: size}},
		{Artifact: artifact, Key: artifact.String() + ":verifying", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: occurredAt},
		{Artifact: artifact, Key: artifact.String() + ":verified", From: "VERIFYING", To: "VERIFIED", OccurredAt: occurredAt, Hashes: &state.HashUpdate{Hash: sum, Alg: "sha256"}},
		{Artifact: artifact, Key: artifact.String() + ":committing", From: "VERIFIED", To: "COMMITTING", OccurredAt: occurredAt},
		{Artifact: artifact, Key: artifact.String() + ":committed", From: "COMMITTING", To: "COMMITTED", OccurredAt: occurredAt, LocalPath: &localPath},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s: %v", tr.To, err)
		}
	}
	return localPath
}

// commitArtifactWithoutHash is commitArtifact's twin for a backup set that
// verifies without hash: sha256: the VERIFIED transition carries no
// HashUpdate at all, exactly like verify.go's decide() when cfg.Hash == "".
func commitArtifactWithoutHash(t *testing.T, j *state.Journal, artifact model.ArtifactID, content []byte, occurredAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	localPath := filepath.Join(t.TempDir(), artifact.Name)
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatalf("writing local file: %v", err)
	}
	size := int64(len(content))

	if _, err := j.Discover(ctx, artifact, artifact.String()+":discover", "backups/"+artifact.Name, state.RemoteIdentity{Size: &size}, occurredAt); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, tr := range []state.Transition{
		{Artifact: artifact, Key: artifact.String() + ":transferring", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: occurredAt, LocalPath: &localPath},
		{Artifact: artifact, Key: artifact.String() + ":transferred", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: occurredAt, Transfer: &state.TransferResult{BytesTransferred: size}},
		{Artifact: artifact, Key: artifact.String() + ":verifying", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: occurredAt},
		{Artifact: artifact, Key: artifact.String() + ":verified", From: "VERIFYING", To: "VERIFIED", OccurredAt: occurredAt},
		{Artifact: artifact, Key: artifact.String() + ":committing", From: "VERIFIED", To: "COMMITTING", OccurredAt: occurredAt},
		{Artifact: artifact, Key: artifact.String() + ":committed", From: "COMMITTING", To: "COMMITTED", OccurredAt: occurredAt, LocalPath: &localPath},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s: %v", tr.To, err)
		}
	}
	return localPath
}

// retainedArtifact is completeArtifact's issue #315 twin: it drives
// artifact to COMMITTED and then to REMOTE_RETAINED (issue #282's
// read-only path) instead of REMOTE_DELETE_PENDING -> COMPLETE.
func retainedArtifact(t *testing.T, j *state.Journal, artifact model.ArtifactID, content []byte, occurredAt time.Time) string {
	t.Helper()
	localPath := commitArtifact(t, j, artifact, content, occurredAt)
	ctx := context.Background()
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: artifact.String() + ":retained", From: "COMMITTED", To: "REMOTE_RETAINED", OccurredAt: occurredAt,
	}); err != nil {
		t.Fatalf("-> REMOTE_RETAINED: %v", err)
	}
	return localPath
}

// completeArtifact drives an artifact all the way to COMPLETE, through
// REMOTE_DELETE_PENDING, recording a deletion timestamp on the way.
//
// COMPLETE is the state whose failure routing differs from every other
// eligible one: the remote original is confirmed gone, so a corrupted local
// copy has nothing left to recover from and routes to QUARANTINED_LOST
// rather than to the recoverable QUARANTINED. This fixture exists so that
// difference can be tested against a record that genuinely got there.
func completeArtifact(t *testing.T, j *state.Journal, artifact model.ArtifactID, content []byte, occurredAt time.Time) string {
	t.Helper()
	localPath := commitArtifact(t, j, artifact, content, occurredAt)
	ctx := context.Background()
	for _, tr := range []state.Transition{
		{Artifact: artifact, Key: artifact.String() + ":pending", From: "COMMITTED", To: "REMOTE_DELETE_PENDING", OccurredAt: occurredAt},
		{Artifact: artifact, Key: artifact.String() + ":complete", From: "REMOTE_DELETE_PENDING", To: "COMPLETE", OccurredAt: occurredAt, Deletion: &state.DeletionUpdate{DeletedAt: &occurredAt}},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s: %v", tr.To, err)
		}
	}
	return localPath
}

// --- SelectDue ---

// TestSelectDue_DisabledReturnsNil pins that MaxPerCycle <= 0 wins over
// everything else, using a record that is overdue by 999 hours so it would
// certainly be selected on any other reading.
//
// The second config is the case worth having: Hash and Interval both set,
// MaxPerCycle still zero. A backup set half-configured that way is what an
// operator produces when they enable revalidation and forget the bound, and
// this pins that it stays off rather than becoming an unbounded sweep.
func TestSelectDue_DisabledReturnsNil(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []state.Record{
		{Artifact: artifactNamed(t, "a.dump"), State: "COMMITTED", UpdatedAt: now.Add(-999 * time.Hour)},
	}
	for _, cfg := range []config.Revalidation{
		{},
		{Hash: true, Interval: config.Duration(time.Hour)}, // MaxPerCycle still 0
	} {
		if got := SelectDue(records, cfg, now); got != nil {
			t.Fatalf("SelectDue with MaxPerCycle<=0 = %v, want nil", got)
		}
	}
}

// TestSelectDue_FiltersByEligibleState walks lifecycle.AllStates rather
// than a hand-written list, which is what makes it useful: a state added to
// the machine and not considered here shows up as a count that no longer
// matches, so the question "should this new state be revalidated" has to be
// answered rather than skipped.
//
// The count is asserted as well as the membership. Membership alone would
// pass for an implementation that selected nothing at all.
func TestSelectDue_FiltersByEligibleState(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := now.Add(-999 * time.Hour)
	cfg := config.Revalidation{Hash: true, Interval: config.Duration(time.Hour), MaxPerCycle: 10}

	var records []state.Record
	for _, st := range lifecycle.AllStates {
		records = append(records, state.Record{Artifact: artifactNamed(t, string(st)+".dump"), State: string(st), UpdatedAt: old})
	}

	due := SelectDue(records, cfg, now)
	if len(due) != 4 {
		t.Fatalf("len(due) = %d, want 4 (COMMITTED, REMOTE_DELETE_PENDING, COMPLETE, REMOTE_RETAINED)", len(due))
	}
	for _, rec := range due {
		st := lifecycle.State(rec.State)
		if st != lifecycle.Committed && st != lifecycle.RemoteDeletePending && st != lifecycle.Complete && st != lifecycle.RemoteRetained {
			t.Fatalf("SelectDue returned an ineligible state %s", st)
		}
	}
}

// TestSelectDue_IncludesRemoteRetained is issue #315's own proof, isolated
// from the "walk every state" sweep above: a read-only source's retained
// artifacts must be selected for scheduled revalidation exactly like an
// ordinary COMMITTED one, not silently skipped forever the way they were
// before this fix.
func TestSelectDue_IncludesRemoteRetained(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := now.Add(-999 * time.Hour)
	cfg := config.Revalidation{Hash: true, Interval: config.Duration(time.Hour), MaxPerCycle: 10}

	records := []state.Record{
		{Artifact: artifactNamed(t, "retained.dump"), State: "REMOTE_RETAINED", UpdatedAt: old},
	}

	due := SelectDue(records, cfg, now)
	if len(due) != 1 {
		t.Fatalf("len(due) = %d, want 1: a REMOTE_RETAINED artifact overdue for a check must be selected", len(due))
	}
}

// TestSelectDue_FiltersByInterval puts one record exactly ON the boundary,
// which is the only case with a decision in it.
//
// Due is >= Interval rather than >, so exactly-due.dump is selected. That
// choice matters at scale rather than in the abstract: an artifact whose
// UpdatedAt lands precisely on the interval, which is what a fixed-cadence
// scheduler produces, would otherwise slip a whole cycle every time.
func TestSelectDue_FiltersByInterval(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := config.Revalidation{Hash: true, Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10}

	records := []state.Record{
		{Artifact: artifactNamed(t, "fresh.dump"), State: "COMMITTED", UpdatedAt: now.Add(-1 * time.Hour)},
		{Artifact: artifactNamed(t, "overdue.dump"), State: "COMMITTED", UpdatedAt: now.Add(-48 * time.Hour)},
		{Artifact: artifactNamed(t, "exactly-due.dump"), State: "COMMITTED", UpdatedAt: now.Add(-24 * time.Hour)},
	}

	due := SelectDue(records, cfg, now)
	var names []string
	for _, rec := range due {
		names = append(names, rec.Artifact.Name)
	}
	if len(names) != 2 {
		t.Fatalf("due = %v, want exactly overdue.dump and exactly-due.dump", names)
	}
	for _, want := range []string{"overdue.dump", "exactly-due.dump"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("due = %v, missing %q", names, want)
		}
	}
}

// TestSelectDue_BoundedByMaxPerCycleOldestFirst asserts the bound and the
// order together, because the bound is only safe if the order is right.
//
// Truncating an unordered list to MaxPerCycle would let the same recent
// artifacts be picked every cycle while the most overdue ones are never
// reached, which is the shape of failure where revalidation is running,
// reporting passes, and never looking at the artifact that rotted.
//
// The records are deliberately supplied newest-first, so an implementation
// that skipped the sort would return the wrong pair rather than accidentally
// the right one.
func TestSelectDue_BoundedByMaxPerCycleOldestFirst(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := config.Revalidation{Hash: true, Interval: config.Duration(time.Hour), MaxPerCycle: 2}

	records := []state.Record{
		{Artifact: artifactNamed(t, "newest.dump"), State: "COMMITTED", UpdatedAt: now.Add(-3 * time.Hour)},
		{Artifact: artifactNamed(t, "oldest.dump"), State: "COMMITTED", UpdatedAt: now.Add(-100 * time.Hour)},
		{Artifact: artifactNamed(t, "middle.dump"), State: "COMMITTED", UpdatedAt: now.Add(-50 * time.Hour)},
	}

	due := SelectDue(records, cfg, now)
	if len(due) != 2 {
		t.Fatalf("len(due) = %d, want 2 (MaxPerCycle)", len(due))
	}
	if due[0].Artifact.Name != "oldest.dump" || due[1].Artifact.Name != "middle.dump" {
		t.Fatalf("due order = [%s, %s], want [oldest.dump, middle.dump] (most overdue first)", due[0].Artifact.Name, due[1].Artifact.Name)
	}
}

// TestSelectDue_DeterministicTieBreak covers what a large backfill actually
// produces: hundreds of artifacts sharing one UpdatedAt to the nanosecond.
//
// Without a tie-break the bounded subset would depend on map or listing
// order, so two identical passes could pick different artifacts and some
// records might never be selected at all. The input is supplied in reverse
// order so the sort has to do something.
func TestSelectDue_DeterministicTieBreak(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := config.Revalidation{Hash: true, Interval: config.Duration(time.Hour), MaxPerCycle: 10}
	sameTime := now.Add(-2 * time.Hour)

	records := []state.Record{
		{Artifact: artifactNamed(t, "b.dump"), State: "COMMITTED", UpdatedAt: sameTime},
		{Artifact: artifactNamed(t, "a.dump"), State: "COMMITTED", UpdatedAt: sameTime},
	}

	due := SelectDue(records, cfg, now)
	if len(due) != 2 || due[0].Artifact.Name != "a.dump" {
		t.Fatalf("due = %v, want a.dump first on a tie", due)
	}
}

// --- Run ---

// TestRun_DisabledDoesNothing pins the cheap path. Run is called every
// cycle for every backup set, so a disabled set must cost nothing: the
// assertion that the journal row is untouched is the visible half, and the
// invisible half is that Run returns before it even lists the set.
func TestRun_DisabledDoesNothing(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	commitArtifact(t, j, artifact, []byte("hello world"), before.Add(-999*time.Hour))

	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return before }}, set, config.Revalidation{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 0 || len(report.Errors) != 0 {
		t.Fatalf("report = %+v, want empty when revalidation is disabled", report)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != "COMMITTED" {
		t.Fatalf("state = %q, want it untouched", rec.State)
	}
}

// TestRun_HashStillMatches_StaysCommittedAndRefreshesTheClock is the happy
// path, and the second Run at the end is the half that gives it teeth.
//
// A pass writes a same-state transition, which looks pointless until you
// notice what it is for: it moves UpdatedAt, which is the due-ness clock, so
// the artifact is not selected again immediately. Without the second call
// this test would pass against an implementation that checked the same
// artifact on every cycle for ever while ignoring everything else in the
// backup set.
func TestRun_HashStillMatches_StaysCommittedAndRefreshesTheClock(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	commitArtifact(t, j, artifact, []byte("hello world"), old.Add(-999*time.Hour))

	cfg := config.Revalidation{Hash: true, Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10}
	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return old }}, set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(report.Findings))
	}
	f := report.Findings[0]
	if !f.Checked || !f.Passed {
		t.Fatalf("Finding = %+v, want Checked=true Passed=true", f)
	}
	if f.From != lifecycle.Committed || f.To != lifecycle.Committed {
		t.Fatalf("Finding From/To = %s/%s, want COMMITTED/COMMITTED", f.From, f.To)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != "COMMITTED" {
		t.Fatalf("state = %q, want it to stay COMMITTED", rec.State)
	}
	if !rec.UpdatedAt.Equal(old) {
		t.Fatalf("UpdatedAt = %s, want it refreshed to %s (the pass write resets the due-ness clock)", rec.UpdatedAt, old)
	}

	// Immediately due again would be wrong: run again right now, it should
	// no longer be selected.
	report2, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return old }}, set, cfg)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(report2.Findings) != 0 {
		t.Fatalf("second Run found %d findings, want 0 (nothing should be due again immediately)", len(report2.Findings))
	}
}

// TestRun_CorruptedCommitted_RoutesToQuarantined is bit rot staged
// literally: the file is committed with a recorded hash and then rewritten
// underneath the journal, which is what a failing disk does more slowly.
//
// QUARANTINED rather than QUARANTINED_LOST is the point. A COMMITTED
// artifact has not had its remote original deleted, so the remote is
// presumptively still there and this is recoverable.
func TestRun_CorruptedCommitted_RoutesToQuarantined(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	localPath := commitArtifact(t, j, artifact, []byte("hello world"), now.Add(-999*time.Hour))

	// Bit rot: the bytes on disk no longer match what was hashed at
	// VERIFIED.
	if err := os.WriteFile(localPath, []byte("corrupted!!"), 0o600); err != nil {
		t.Fatalf("corrupting local file: %v", err)
	}

	cfg := config.Revalidation{Hash: true, Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10}
	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return now }}, set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(report.Findings))
	}
	f := report.Findings[0]
	if !f.Checked || f.Passed {
		t.Fatalf("Finding = %+v, want Checked=true Passed=false", f)
	}
	if f.To != lifecycle.Quarantined {
		t.Fatalf("Finding.To = %s, want %s", f.To, lifecycle.Quarantined)
	}
	if !strings.Contains(f.Reason, "now hashes to") {
		t.Fatalf("Reason = %q, want it to explain the hash mismatch", f.Reason)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Quarantined) {
		t.Fatalf("state = %q, want %s", rec.State, lifecycle.Quarantined)
	}
}

// TestRun_CorruptedComplete_RoutesToQuarantinedLost is the same corruption
// against an artifact whose remote original is confirmed gone, and the
// routing has to differ: there is nothing left to recover from, so it lands
// in the irrecoverable state.
//
// The pair with the COMMITTED test above is the whole assertion. Either test
// alone would pass against an implementation that always chose one
// destination.
func TestRun_CorruptedComplete_RoutesToQuarantinedLost(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	localPath := completeArtifact(t, j, artifact, []byte("hello world"), now.Add(-999*time.Hour))

	if err := os.WriteFile(localPath, []byte("corrupted!!"), 0o600); err != nil {
		t.Fatalf("corrupting local file: %v", err)
	}

	cfg := config.Revalidation{Hash: true, Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10}
	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return now }}, set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(report.Findings))
	}
	if report.Findings[0].To != lifecycle.QuarantinedLost {
		t.Fatalf("Finding.To = %s, want %s: the remote is already confirmed gone, so this must be irrecoverable", report.Findings[0].To, lifecycle.QuarantinedLost)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.QuarantinedLost) {
		t.Fatalf("state = %q, want %s", rec.State, lifecycle.QuarantinedLost)
	}
}

// TestRun_CorruptedRemoteRetained_RoutesToQuarantined is issue #315's
// end-to-end proof, through the real Run entry point rather than just
// SelectDue: a retained, read-only-source artifact whose durable local
// copy has bit-rotted is caught by a scheduled revalidation pass and
// quarantined, exactly the way TestRun_CorruptedCommitted_RoutesToQuarantined
// proves for an ordinary COMMITTED one. Unlike TestRun_CorruptedComplete's
// routing to QUARANTINED_LOST, this has to land in the recoverable
// QUARANTINED: REMOTE_RETAINED never confirmed the remote object gone, so
// the remote is presumptively still there for an operator to recover from.
func TestRun_CorruptedRemoteRetained_RoutesToQuarantined(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	localPath := retainedArtifact(t, j, artifact, []byte("hello world"), now.Add(-999*time.Hour))

	if err := os.WriteFile(localPath, []byte("corrupted!!"), 0o600); err != nil {
		t.Fatalf("corrupting local file: %v", err)
	}

	cfg := config.Revalidation{Hash: true, Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10}
	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return now }}, set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(report.Findings))
	}
	f := report.Findings[0]
	if !f.Checked || f.Passed {
		t.Fatalf("Finding = %+v, want Checked=true Passed=false", f)
	}
	if f.To != lifecycle.Quarantined {
		t.Fatalf("Finding.To = %s, want %s: the remote was never touched or confirmed gone, so this must stay recoverable", f.To, lifecycle.Quarantined)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Quarantined) {
		t.Fatalf("state = %q, want %s", rec.State, lifecycle.Quarantined)
	}
}

// TestRun_RemoteRetainedHashStillMatches_StaysRetainedAndRefreshesTheClock
// is the pass-side companion: a retained artifact whose local copy is
// still intact must stay exactly where it was, REMOTE_RETAINED, and get
// its due-ness clock reset like any other passing recheck.
func TestRun_RemoteRetainedHashStillMatches_StaysRetainedAndRefreshesTheClock(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	retainedArtifact(t, j, artifact, []byte("hello world"), old.Add(-999*time.Hour))

	cfg := config.Revalidation{Hash: true, Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10}
	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return old }}, set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(report.Findings))
	}
	f := report.Findings[0]
	if !f.Checked || !f.Passed {
		t.Fatalf("Finding = %+v, want Checked=true Passed=true", f)
	}
	if f.From != lifecycle.RemoteRetained || f.To != lifecycle.RemoteRetained {
		t.Fatalf("Finding From/To = %s/%s, want REMOTE_RETAINED/REMOTE_RETAINED", f.From, f.To)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.RemoteRetained) {
		t.Fatalf("state = %q, want it to stay REMOTE_RETAINED", rec.State)
	}
	if !rec.UpdatedAt.Equal(old) {
		t.Fatalf("UpdatedAt = %s, want it refreshed to %s", rec.UpdatedAt, old)
	}
}

// TestRun_RestoreTestHookFailure_RoutesToQuarantined pins that the hook's
// own verdict is a verdict about the artifact, not an error.
//
// A hook that runs and exits non-zero has said something about the file: it
// does not restore. That is exactly what an operator configured it to
// discover, so it quarantines, the same as a hash mismatch would.
func TestRun_RestoreTestHookFailure_RoutesToQuarantined(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	commitArtifact(t, j, artifact, []byte("hello world"), now.Add(-999*time.Hour))

	script := mustScript(t, "echo \"restore failed: cannot open archive\" >&2\nexit 1\n")
	cfg := config.Revalidation{
		Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10,
		Command: &config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)},
	}

	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return now }}, set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Passed {
		t.Fatalf("Findings = %+v, want exactly one failed finding", report.Findings)
	}
	if !strings.Contains(report.Findings[0].Reason, "cannot open archive") {
		t.Fatalf("Reason = %q, want it to include the hook's stderr", report.Findings[0].Reason)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Quarantined) {
		t.Fatalf("state = %q, want %s", rec.State, lifecycle.Quarantined)
	}
}

// TestRun_RestoreTestHookPasses_StaysCommitted is the positive control for
// the hook tier. Without it, a hook that never ran at all would satisfy the
// failure test above by simply never producing a failure.
func TestRun_RestoreTestHookPasses_StaysCommitted(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	commitArtifact(t, j, artifact, []byte("hello world"), now.Add(-999*time.Hour))

	script := mustScript(t, "exit 0\n")
	cfg := config.Revalidation{
		Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10,
		Command: &config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)},
	}

	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return now }}, set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 || !report.Findings[0].Passed {
		t.Fatalf("Findings = %+v, want exactly one passed finding", report.Findings)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != "COMMITTED" {
		t.Fatalf("state = %q, want it to stay COMMITTED", rec.State)
	}
}

// TestRun_HookCannotStart_ReportsAnErrorWithoutTouchingTheJournal is the
// most important test in this file, and it is one character away from the
// one above it.
//
// A hook that exits non-zero is the artifact failing. A hook that cannot be
// executed, a wrong path or a lost execute bit, is the OPERATOR's
// configuration failing, and treating that as a verdict would quarantine
// every artifact in the backup set over a typo. So it lands in Report.Errors
// and the journal is not touched at all, which the assertions check
// separately: the error alone would be satisfied by an implementation that
// reported it and quarantined anyway.
func TestRun_HookCannotStart_ReportsAnErrorWithoutTouchingTheJournal(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	commitArtifact(t, j, artifact, []byte("hello world"), now.Add(-999*time.Hour))

	cfg := config.Revalidation{
		Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10,
		Command: &config.Command{Executable: filepath.Join(t.TempDir(), "does-not-exist"), Timeout: config.Duration(5 * time.Second)},
	}

	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return now }}, set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("Findings = %+v, want none: an unstartable hook is an infra error, not a verdict", report.Findings)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("len(Errors) = %d, want 1", len(report.Errors))
	}
	if report.Errors[0].Artifact != artifact {
		t.Fatalf("Errors[0].Artifact = %s, want %s", report.Errors[0].Artifact, artifact)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != "COMMITTED" {
		t.Fatalf("state = %q, want it untouched by an infrastructure error", rec.State)
	}
}

// TestRun_NoHashBaseline_IsANoOpNotAFailure covers a backup set that
// verifies without a hash, so there is a durable copy and nothing recorded
// to compare it against.
//
// Neither available verdict is honest here. A pass would be a lie and would
// reset the due-ness clock; a failure would quarantine a backup nobody has
// found anything wrong with. So the finding is unchecked, the journal is
// untouched, and the second Run proves the consequence: the artifact is
// still selected, so if a hash baseline ever appears it will be used.
func TestRun_NoHashBaseline_IsANoOpNotAFailure(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	set := artifact.Set
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	occurredAt := now.Add(-999 * time.Hour)
	commitArtifactWithoutHash(t, j, artifact, []byte("hello world"), occurredAt)

	cfg := config.Revalidation{Hash: true, Interval: config.Duration(24 * time.Hour), MaxPerCycle: 10}
	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return now }}, set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(report.Findings))
	}
	f := report.Findings[0]
	if f.Checked {
		t.Fatalf("Finding.Checked = true, want false: there is no hash baseline to compare against")
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != "COMMITTED" {
		t.Fatalf("state = %q, want it untouched", rec.State)
	}
	if !rec.UpdatedAt.Equal(occurredAt) {
		t.Fatalf("UpdatedAt = %s, want it left at %s: a no-op check must never refresh the due-ness clock", rec.UpdatedAt, occurredAt)
	}

	// Because nothing was actually checked, this artifact must still be
	// selected as due on a later Run: the clock was never reset.
	report2, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return now }}, set, cfg)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(report2.Findings) != 1 || report2.Findings[0].Checked {
		t.Fatalf("second Run's Findings = %+v, want the same unchecked finding again", report2.Findings)
	}
}

// TestRun_MaxPerCycleSpreadsWorkAcrossCalls is the backlog case: five
// artifacts all due at once, which is what a large initial backfill leaves
// behind, drained two at a time.
//
// It asserts that all five are eventually covered rather than which pair
// came in which call. That is the real requirement, and it depends on the
// pass write moving UpdatedAt: without it the same two artifacts would be
// the most overdue for ever and the other three would never be checked.
func TestRun_MaxPerCycleSpreadsWorkAcrossCalls(t *testing.T) {
	j := openJournal(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	set := artifactNamed(t, "x").Set

	var artifacts []model.ArtifactID
	for i := 0; i < 5; i++ {
		a := artifactNamed(t, "backup-"+string(rune('a'+i))+".dump")
		commitArtifact(t, j, a, []byte("hello world "+string(rune('a'+i))), now.Add(-999*time.Hour))
		artifacts = append(artifacts, a)
	}

	cfg := config.Revalidation{Hash: true, Interval: config.Duration(24 * time.Hour), MaxPerCycle: 2}
	deps := Deps{Journal: j, Now: func() time.Time { return now }}

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		report, err := Run(context.Background(), deps, set, cfg)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if i < 2 && len(report.Findings) != 2 {
			t.Fatalf("Run %d: len(Findings) = %d, want 2 (MaxPerCycle)", i, len(report.Findings))
		}
		for _, f := range report.Findings {
			seen[f.Artifact.Name] = true
		}
	}

	if len(seen) != len(artifacts) {
		t.Fatalf("saw %d distinct artifacts across 3 bounded Run calls, want all %d eventually covered", len(seen), len(artifacts))
	}
}

// --- WP3.2 integration: an artifact FR-15's stable-mode gate held back is
// picked up by this package's own scheduled re-check, with zero changes
// required to this package's own selection or verdict-routing logic ---

// unreachedDeleteTransport is a transport.Transport whose every method
// panics. WP3.2's stable-mode safety check (internal/lifecycle/
// remotedelete.go) refuses before lifecycle.DeleteRemote ever reaches a
// transport call, so the test below needs a Transport only to satisfy
// DeleteRemote's own non-nil precondition, never expects any of it to
// actually run.
// unreachedDeleteTransport panics on every method rather than returning
// zero values, which is the choice worth noticing: the test using it asserts
// that a refusal happens BEFORE any remote call, and a double that answered
// politely would let a regression reach the remote and still pass.
type unreachedDeleteTransport struct{}

// Panics: see the type comment.
func (unreachedDeleteTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	panic("unreachedDeleteTransport: List not used")
}

// Panics: see the type comment.
func (unreachedDeleteTransport) Stat(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
	panic("unreachedDeleteTransport: Stat not used")
}

// Panics: see the type comment.
func (unreachedDeleteTransport) CopyToLocal(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	panic("unreachedDeleteTransport: CopyToLocal not used")
}

// Panics: see the type comment.
func (unreachedDeleteTransport) RemoteHash(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
	panic("unreachedDeleteTransport: RemoteHash not used")
}

// Panics: see the type comment.
func (unreachedDeleteTransport) DeleteRemote(context.Context, transport.Source, string) error {
	panic("unreachedDeleteTransport: DeleteRemote not used")
}

// The compile-time check that this double still satisfies the interface it
// only exists to satisfy.
var _ transport.Transport = unreachedDeleteTransport{}

// TestRun_PicksUpArtifactWP32HeldBackFromDeletion is the INTEGRATION test
// docs/EPIC-B-multi-nas.md §71 Work Package 3.2 asks for: "internal/
// revalidate's scheduled re-check picking up an artifact this work
// package quarantined". WP3.2's stable-mode gate does not move a held-back
// artifact into QUARANTINED: it leaves it exactly at COMMITTED, already
// one of this package's own eligibleStates (select.go), refuses the
// delete, and preserves the remote source (see remotedelete.go's own doc
// for why). This test proves that composition end to end: a real
// lifecycle.DeleteRemote refusal, on a real journal, produces a record
// this package's own SelectDue and Run pick straight back up, with zero
// changes required to this package's own selection or verdict-routing
// logic.
func TestRun_PicksUpArtifactWP32HeldBackFromDeletion(t *testing.T) {
	j := openJournal(t)
	artifact := artifactNamed(t, "backup.dump")
	content := []byte("hello world")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const safetyDelay = 10 * time.Minute

	commitArtifact(t, j, artifact, content, t0)

	_, err := lifecycle.DeleteRemote(context.Background(),
		lifecycle.Deps{Journal: j, Transport: unreachedDeleteTransport{}, Now: func() time.Time { return t0 }},
		lifecycle.DeleteRemoteRequest{
			Artifact:           artifact,
			AttemptKey:         "attempt-1",
			CompletionStrategy: "stable",
			DeleteSafetyDelay:  safetyDelay,
		})
	if _, ok := lifecycle.AsRemoteDeleteRefusal(err); !ok {
		t.Fatalf("DeleteRemote error = %v, want a refusal: WP3.2's own safety delay has not elapsed yet", err)
	}

	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Committed) {
		t.Fatalf("state = %q, want %s: the remote source must be preserved, held back rather than routed anywhere else", rec.State, lifecycle.Committed)
	}

	cfg := config.Revalidation{Hash: true, Interval: config.Duration(5 * time.Minute), MaxPerCycle: 10}
	dueAt := t0.Add(6 * time.Minute)

	due := SelectDue([]state.Record{rec}, cfg, dueAt)
	if len(due) != 1 {
		t.Fatalf("SelectDue returned %d records, want 1: this held-back artifact must be selected", len(due))
	}

	report, err := Run(context.Background(), Deps{Journal: j, Now: func() time.Time { return dueAt }}, artifact.Set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("report.Findings = %+v, want exactly 1", report.Findings)
	}
	if f := report.Findings[0]; !f.Checked || !f.Passed {
		t.Fatalf("Finding = %+v, want a checked, passed re-verification: the content on disk has not actually changed", f)
	}

	// --- third phase: the gate has to actually open afterwards.
	//
	// The two phases above prove this package picks a held-back artifact
	// up. On their own they say nothing about the half that can livelock,
	// because they never ask lifecycle.DeleteRemote a second time. Run's
	// passing re-check just wrote a same-state COMMITTED -> COMMITTED
	// transition, and the first version of WP3.2's gate measured its
	// safety delay from state.Record.UpdatedAt, which that write advances.
	// Any backup set whose revalidation.interval was shorter than its
	// delete_safety_delay would therefore have had the clock reset on
	// every scheduled pass, the delay would never have elapsed, and the
	// remote copy would never have been reclaimed, silently, with the
	// artifact reporting healthy the whole time.
	reread, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get after Run: %v", err)
	}

	// Positive control for the assertion below. The re-check must really
	// have moved UpdatedAt off t0, otherwise the delete succeeding at
	// t0+11m would prove nothing at all: it would just mean the old clock
	// and the new one happened to agree on this fixture.
	if !reread.UpdatedAt.After(t0) {
		t.Fatalf("UpdatedAt = %s, want it advanced past %s by the passing re-check; if the scheduled pass does not move this field, this test cannot distinguish the shared timestamp from the COMMITTED transition and proves nothing", reread.UpdatedAt, t0)
	}

	// t0+11m is past the 10m delay measured from the COMMITTED transition,
	// and short of it measured from the re-check's own UpdatedAt (t0+6m),
	// so this call passes the gate only if the clock is the transition.
	retryAt := t0.Add(safetyDelay + time.Minute)
	tp := &statMismatchTransport{}
	_, err = lifecycle.DeleteRemote(context.Background(),
		lifecycle.Deps{Journal: j, Transport: tp, Now: func() time.Time { return retryAt }},
		lifecycle.DeleteRemoteRequest{
			Artifact:           artifact,
			AttemptKey:         "attempt-2",
			CompletionStrategy: "stable",
			DeleteSafetyDelay:  safetyDelay,
		})

	refusal, ok := lifecycle.AsRemoteDeleteRefusal(err)
	if !ok {
		t.Fatalf("second DeleteRemote error = %v (%T), want a refusal from a later check", err, err)
	}
	if refusal.Check == "stable completion safety delay" {
		t.Fatalf("the safety delay refused again at %s, %s after this artifact reached COMMITTED: a scheduled re-check must not restart the deletion-safety clock, or the gate can never open and the remote is never reclaimed (%v)", retryAt, safetyDelay+time.Minute, refusal)
	}
	if refusal.Check != "remote identity" {
		t.Fatalf("refusal.Check = %q, want %q: the safety delay should have been cleared and the FR-16 identity check should be what holds this back now", refusal.Check, "remote identity")
	}
	if tp.statCalls != 1 {
		t.Fatalf("transport.Stat called %d times, want exactly 1: the delete never got as far as re-checking the remote identity", tp.statCalls)
	}
}

// statMismatchTransport answers Stat with a remote object that cannot
// possibly be the one that was captured at discovery, so FR-16's identity
// comparison refuses. It exists so the test above can prove WP3.2's safety
// gate OPENED without also having to arrange a real, deletable remote: the
// refusal it produces comes from a later check, and its DeleteRemote panics
// so a gate that let a delete through would be impossible to miss.
type statMismatchTransport struct {
	statCalls int
}

// Panics: see the type comment.
func (t *statMismatchTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	panic("statMismatchTransport: List not used")
}

// Stat is the one method that answers, and it counts its calls. The size it
// reports is nothing like the one captured at discovery, so FR-16's identity
// comparison refuses; the count is what proves the delete got as far as
// asking, which is the actual assertion about the safety gate having
// opened.
func (t *statMismatchTransport) Stat(_ context.Context, _ transport.Source, path string) (transport.RemoteArtifact, error) {
	t.statCalls++
	return transport.RemoteArtifact{Path: path, Size: 999999}, nil
}

// Panics: see the type comment.
func (t *statMismatchTransport) CopyToLocal(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	panic("statMismatchTransport: CopyToLocal not used")
}

// Panics: see the type comment.
func (t *statMismatchTransport) RemoteHash(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
	panic("statMismatchTransport: RemoteHash not used")
}

// DeleteRemote panics, so a gate that let a delete through against a
// mismatched identity would be impossible to miss.
func (t *statMismatchTransport) DeleteRemote(context.Context, transport.Source, string) error {
	panic("statMismatchTransport: DeleteRemote must never be reached: the remote identity does not match")
}

// The compile-time check, for the same reason as the one above.
var _ transport.Transport = (*statMismatchTransport)(nil)

// TestIsCancelled_AClassifiedFailureIsNeverACancellation is the revalidate
// half of issue #388, and it is a direct test of the predicate rather than a
// scenario because nothing on this package's current call graph hands
// isCancelled a transport-classified error: runChecks reaches the local
// filesystem and the restore-test hook, never a Transport. That is exactly
// why this needs pinning now. The moment a check here does talk to a remote,
// a raw errors.Is fallback would read rclone's own connect timeout, which
// transport/rclone classifies as Transient and whose cause stays reachable
// as context.DeadlineExceeded through transport.Error's Unwrap, as an
// operator stopping the run.
//
// verify.go's isCancellation is the same predicate for the same reason, and
// the two are meant to agree.
func TestIsCancelled_AClassifiedFailureIsNeverACancellation(t *testing.T) {
	// The shape transport/rclone now produces for a connect timeout rclone
	// imposed on itself: Transient, with the deadline still reachable
	// underneath.
	connectTimeout := transport.NewError(transport.Transient, "remote_hash",
		fmt.Errorf(`source "prod": NewFs: couldn't connect SSH: dial tcp 192.0.2.1:22: %w`, context.DeadlineExceeded))
	if !errors.Is(connectTimeout, context.DeadlineExceeded) {
		t.Fatal("this error no longer carries context.DeadlineExceeded, so it cannot exercise the confusion this test exists for")
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "a connect timeout rclone imposed, already classified Transient",
			err:  connectTimeout,
			want: false,
		},
		{
			name: "an unsupported capability, already classified",
			err:  transport.NewError(transport.UnsupportedCapability, "remote_hash", errors.New("backend cannot compute sha256")),
			want: false,
		},
		{
			name: "a real cancellation the transport classified",
			err:  transport.NewError(transport.Cancelled, "remote_hash", context.Canceled),
			want: true,
		},
		{
			name: "a raw context.Canceled from something that never classified it",
			err:  fmt.Errorf("restore-test hook: %w", context.Canceled),
			want: true,
		},
		{
			name: "a raw context.DeadlineExceeded from something that never classified it",
			err:  fmt.Errorf("restore-test hook: %w", context.DeadlineExceeded),
			want: true,
		},
		{
			name: "an ordinary failure",
			err:  errors.New("local final file could not be read"),
			want: false,
		},
		{
			name: "no error at all",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCancelled(tc.err); got != tc.want {
				t.Fatalf("isCancelled(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
