package quarantine

import (
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/internal/lifecycle"
	"github.com/spdrman/rclone-manager/internal/model"
	"github.com/spdrman/rclone-manager/internal/state"
)

func mustArtifact(t *testing.T, name string) model.ArtifactID {
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

func TestSummarize_ExcludesEveryNonQuarantineState(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var records []state.Record
	for _, st := range lifecycle.AllStates {
		if st == lifecycle.Quarantined || st == lifecycle.QuarantinedLost {
			continue
		}
		records = append(records, state.Record{
			Artifact:  mustArtifact(t, string(st)+".dump"),
			State:     string(st),
			UpdatedAt: now.Add(-time.Hour),
		})
	}

	report := Summarize(records, now)
	if report.Total != 0 {
		t.Fatalf("Total = %d, want 0: no non-quarantine state should ever appear in a Report", report.Total)
	}
	if len(report.Entries) != 0 {
		t.Fatalf("Entries = %#v, want empty", report.Entries)
	}
}

func TestSummarize_RecoverableVsLost(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []state.Record{
		{
			Artifact:  mustArtifact(t, "recoverable.dump"),
			State:     string(lifecycle.Quarantined),
			UpdatedAt: now.Add(-time.Hour),
		},
		{
			Artifact:  mustArtifact(t, "lost.dump"),
			State:     string(lifecycle.QuarantinedLost),
			UpdatedAt: now.Add(-2 * time.Hour),
		},
	}

	report := Summarize(records, now)
	if report.Total != 2 || report.Recoverable != 1 || report.Lost != 1 {
		t.Fatalf("counts = %+v, want Total=2 Recoverable=1 Lost=1", report)
	}

	// QUARANTINED_LOST must sort first, regardless of age, so it can never
	// be scrolled past: Phase 4 requires it to "surface differently from
	// ordinary quarantine".
	if len(report.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(report.Entries))
	}
	if report.Entries[0].State != lifecycle.QuarantinedLost {
		t.Fatalf("Entries[0].State = %s, want %s to sort first", report.Entries[0].State, lifecycle.QuarantinedLost)
	}
	if report.Entries[0].Recoverable {
		t.Fatal("Entries[0].Recoverable = true for a QUARANTINED_LOST entry, want false")
	}
	if !report.Entries[1].Recoverable {
		t.Fatal("Entries[1].Recoverable = false for a QUARANTINED entry, want true")
	}
}

func TestSummarize_RepeatedQuarantineIsVisible(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []state.Record{
		{
			Artifact:   mustArtifact(t, "first-timer.dump"),
			State:      string(lifecycle.Quarantined),
			UpdatedAt:  now,
			RetryCount: 0,
		},
		{
			Artifact:   mustArtifact(t, "repeat-offender.dump"),
			State:      string(lifecycle.Quarantined),
			UpdatedAt:  now,
			RetryCount: 3,
		},
	}

	report := Summarize(records, now)
	if report.RepeatOffenders != 1 {
		t.Fatalf("RepeatOffenders = %d, want 1", report.RepeatOffenders)
	}
	byName := map[string]Entry{}
	for _, e := range report.Entries {
		byName[e.Artifact.Name] = e
	}
	if byName["first-timer.dump"].Repeated {
		t.Fatal("a RetryCount=0 artifact was reported as Repeated")
	}
	if !byName["repeat-offender.dump"].Repeated {
		t.Fatal("a RetryCount=3 artifact was not reported as Repeated")
	}
	if got := byName["repeat-offender.dump"].TimesReturned; got != 3 {
		t.Fatalf("TimesReturned = %d, want 3", got)
	}
}

func TestSummarize_ReasonDelegatesToLifecycle(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	passed := false
	rec := state.Record{
		Artifact:         mustArtifact(t, "rejected.dump"),
		State:            string(lifecycle.Quarantined),
		UpdatedAt:        now,
		ValidationPassed: &passed,
		ValidationDetail: "pg_restore --list failed",
	}

	report := Summarize([]state.Record{rec}, now)
	if len(report.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(report.Entries))
	}
	want := lifecycle.QuarantineReason(rec)
	if report.Entries[0].Reason != want {
		t.Fatalf("Reason = %q, want it to equal lifecycle.QuarantineReason's own answer %q", report.Entries[0].Reason, want)
	}
}

func TestSummarize_AgeAndOrderingWithinAGroup(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	older := now.Add(-48 * time.Hour)
	newer := now.Add(-1 * time.Hour)

	records := []state.Record{
		{Artifact: mustArtifact(t, "newer.dump"), State: string(lifecycle.Quarantined), UpdatedAt: newer},
		{Artifact: mustArtifact(t, "older.dump"), State: string(lifecycle.Quarantined), UpdatedAt: older},
	}

	report := Summarize(records, now)
	if len(report.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(report.Entries))
	}
	// Oldest (most-neglected) first within the same recoverability group.
	if report.Entries[0].Artifact.Name != "older.dump" {
		t.Fatalf("Entries[0] = %s, want the older entry first", report.Entries[0].Artifact.Name)
	}
	if report.Entries[0].Age != 48*time.Hour {
		t.Fatalf("Age = %s, want 48h", report.Entries[0].Age)
	}
}

func TestSummarize_DeterministicOnEqualTimestamps(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []state.Record{
		{Artifact: mustArtifact(t, "b.dump"), State: string(lifecycle.Quarantined), UpdatedAt: now},
		{Artifact: mustArtifact(t, "a.dump"), State: string(lifecycle.Quarantined), UpdatedAt: now},
	}

	first := Summarize(records, now)
	second := Summarize(records, now)
	if len(first.Entries) != 2 || len(second.Entries) != 2 {
		t.Fatalf("expected 2 entries in both reports")
	}
	if first.Entries[0].Artifact.Name != "a.dump" {
		t.Fatalf("Entries[0] = %s, want the tie broken alphabetically (a.dump first)", first.Entries[0].Artifact.Name)
	}
	if first.Entries[0].Artifact != second.Entries[0].Artifact || first.Entries[1].Artifact != second.Entries[1].Artifact {
		t.Fatal("two Summarize calls over identical input produced different orderings")
	}
}

func TestSummarize_EmptyInput(t *testing.T) {
	report := Summarize(nil, time.Now())
	if report.Total != 0 || len(report.Entries) != 0 {
		t.Fatalf("report = %+v, want an empty Report for no records", report)
	}
}
