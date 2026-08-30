package revalidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// --- fixtures ---

func openJournal(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

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

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

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

func TestSelectDue_FiltersByEligibleState(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := now.Add(-999 * time.Hour)
	cfg := config.Revalidation{Hash: true, Interval: config.Duration(time.Hour), MaxPerCycle: 10}

	var records []state.Record
	for _, st := range lifecycle.AllStates {
		records = append(records, state.Record{Artifact: artifactNamed(t, string(st)+".dump"), State: string(st), UpdatedAt: old})
	}

	due := SelectDue(records, cfg, now)
	if len(due) != 3 {
		t.Fatalf("len(due) = %d, want 3 (COMMITTED, REMOTE_DELETE_PENDING, COMPLETE)", len(due))
	}
	for _, rec := range due {
		st := lifecycle.State(rec.State)
		if st != lifecycle.Committed && st != lifecycle.RemoteDeletePending && st != lifecycle.Complete {
			t.Fatalf("SelectDue returned an ineligible state %s", st)
		}
	}
}

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
