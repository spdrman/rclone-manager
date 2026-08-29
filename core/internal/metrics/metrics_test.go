package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/health"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

func mustSet(source, set string) model.BackupSetID {
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		panic(err)
	}
	return id
}

func mustArtifact(set model.BackupSetID, name string) model.ArtifactID {
	id, err := model.NewArtifactID(set, name)
	if err != nil {
		panic(err)
	}
	return id
}

func TestRenderIncludesProcessInfoAndGeneratedAt(t *testing.T) {
	report := health.NewReport(
		health.NewProcessHealth(health.ProcessInputs{BinaryVersion: "1.2.3", RcloneVersion: "v1.75.0"}),
		nil,
		time.Unix(1700000000, 0).UTC(),
	)

	out := Render(report)

	wantProcess := `backup_manager_process_info{binary_version="1.2.3",rclone_version="v1.75.0"} 1`
	if !strings.Contains(out, wantProcess) {
		t.Fatalf("Render output missing process info line %q; got:\n%s", wantProcess, out)
	}
	wantGenerated := "backup_manager_report_generated_timestamp_seconds 1700000000"
	if !strings.Contains(out, wantGenerated) {
		t.Fatalf("Render output missing generated-at line %q; got:\n%s", wantGenerated, out)
	}
}

// TestRenderEveryMetricFamilyHasHelpAndType pins the metric name catalog
// down the same way internal/obs pins its event constants down (see that
// package's doc): a metric name is a contract for anything scraping it, so
// renaming or dropping one silently should fail a test, not just show up
// as a diff nobody was looking for.
func TestRenderEveryMetricFamilyHasHelpAndType(t *testing.T) {
	report := health.NewReport(health.ProcessHealth{}, []health.BackupSetHealth{{Set: mustSet("prod", "one")}}, time.Now())
	out := Render(report)

	names := []string{
		"backup_manager_process_info",
		"backup_manager_report_generated_timestamp_seconds",
		"backup_manager_backup_set_state",
		"backup_manager_backup_set_newest_good_backup_age_seconds",
		"backup_manager_backup_set_stale_threshold_seconds",
		"backup_manager_backup_set_pending_deletes",
		"backup_manager_backup_set_failures",
		"backup_manager_backup_set_quarantined",
		"backup_manager_backup_set_quarantined_lost",
		"backup_manager_backup_set_current_transfers",
		"backup_manager_backup_set_free_bytes",
		"backup_manager_backup_set_last_successful_poll_timestamp_seconds",
		"backup_manager_backup_set_last_completed_backup_timestamp_seconds",
		"backup_manager_backup_set_last_retention_run_timestamp_seconds",
	}
	for _, name := range names {
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Errorf("missing HELP line for %s", name)
		}
		if !strings.Contains(out, "# TYPE "+name+" gauge\n") {
			t.Errorf("missing TYPE line for %s", name)
		}
	}
}

func TestRenderStateIsOneHot(t *testing.T) {
	set := mustSet("prod", "postgres-primary")
	report := health.NewReport(health.ProcessHealth{}, []health.BackupSetHealth{
		{Set: set, State: health.Degraded},
	}, time.Now())

	out := Render(report)

	want := []string{
		`backup_manager_backup_set_state{backup_set="prod/postgres-primary",state="healthy"} 0`,
		`backup_manager_backup_set_state{backup_set="prod/postgres-primary",state="degraded"} 1`,
		`backup_manager_backup_set_state{backup_set="prod/postgres-primary",state="stale"} 0`,
		`backup_manager_backup_set_state{backup_set="prod/postgres-primary",state="failing"} 0`,
	}
	for _, line := range want {
		if !strings.Contains(out, line) {
			t.Errorf("expected line %q in output:\n%s", line, out)
		}
	}
}

// TestRenderOmitsUnsetOptionalFields proves Render never fabricates a zero
// reading for a value internal/health never had evidence for, matching
// Prometheus's own convention that an unknown value has no series.
func TestRenderOmitsUnsetOptionalFields(t *testing.T) {
	set := mustSet("prod", "one")
	report := health.NewReport(health.ProcessHealth{}, []health.BackupSetHealth{{Set: set}}, time.Now())

	out := Render(report)

	forbidden := []string{
		"backup_manager_backup_set_newest_good_backup_age_seconds{",
		"backup_manager_backup_set_free_bytes{",
		"backup_manager_backup_set_last_successful_poll_timestamp_seconds{",
		"backup_manager_backup_set_last_completed_backup_timestamp_seconds{",
		"backup_manager_backup_set_last_retention_run_timestamp_seconds{",
	}
	for _, f := range forbidden {
		if strings.Contains(out, f) {
			t.Errorf("expected no sample for %s when the source field is nil; got:\n%s", f, out)
		}
	}
}

func TestRenderIncludesOptionalFieldsWhenPresent(t *testing.T) {
	set := mustSet("prod", "one")
	age := 90 * time.Second
	free := uint64(123456)
	poll := time.Unix(1700000100, 0).UTC()
	completed := time.Unix(1700000200, 0).UTC()
	retention := time.Unix(1700000300, 0).UTC()

	report := health.NewReport(health.ProcessHealth{}, []health.BackupSetHealth{{
		Set:                   set,
		NewestGoodBackupAge:   &age,
		FreeBytes:             &free,
		LastSuccessfulPollAt:  &poll,
		LastCompletedBackupAt: &completed,
		LastRetentionRunAt:    &retention,
	}}, time.Now())

	out := Render(report)

	wants := []string{
		`backup_manager_backup_set_newest_good_backup_age_seconds{backup_set="prod/one"} 90`,
		`backup_manager_backup_set_free_bytes{backup_set="prod/one"} 123456`,
		`backup_manager_backup_set_last_successful_poll_timestamp_seconds{backup_set="prod/one"} 1700000100`,
		`backup_manager_backup_set_last_completed_backup_timestamp_seconds{backup_set="prod/one"} 1700000200`,
		`backup_manager_backup_set_last_retention_run_timestamp_seconds{backup_set="prod/one"} 1700000300`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("expected line %q; got:\n%s", w, out)
		}
	}
}

func TestRenderCounters(t *testing.T) {
	set := mustSet("prod", "one")
	report := health.NewReport(health.ProcessHealth{}, []health.BackupSetHealth{{
		Set:                  set,
		PendingDeletes:       2,
		Failures:             3,
		QuarantinedCount:     4,
		QuarantinedLostCount: 1,
		CurrentTransfers: []health.TransferInProgress{
			{Artifact: mustArtifact(set, "a")},
			{Artifact: mustArtifact(set, "b")},
		},
		StaleThreshold: 6 * time.Hour,
	}}, time.Now())

	out := Render(report)

	wants := []string{
		`backup_manager_backup_set_pending_deletes{backup_set="prod/one"} 2`,
		`backup_manager_backup_set_failures{backup_set="prod/one"} 3`,
		`backup_manager_backup_set_quarantined{backup_set="prod/one"} 4`,
		`backup_manager_backup_set_quarantined_lost{backup_set="prod/one"} 1`,
		`backup_manager_backup_set_current_transfers{backup_set="prod/one"} 2`,
		`backup_manager_backup_set_stale_threshold_seconds{backup_set="prod/one"} 21600`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("expected line %q; got:\n%s", w, out)
		}
	}
}

func TestRenderOrdersBackupSetsDeterministically(t *testing.T) {
	a := mustSet("prod", "zzz")
	b := mustSet("prod", "aaa")

	report1 := health.NewReport(health.ProcessHealth{}, []health.BackupSetHealth{{Set: a}, {Set: b}}, time.Now())
	report2 := health.NewReport(health.ProcessHealth{}, []health.BackupSetHealth{{Set: b}, {Set: a}}, time.Now())

	out1 := Render(report1)
	out2 := Render(report2)

	if out1 != out2 {
		t.Fatalf("Render is not deterministic under input reordering:\n--- order 1 ---\n%s\n--- order 2 ---\n%s", out1, out2)
	}

	idxB := strings.Index(out1, `backup_set="prod/aaa"`)
	idxA := strings.Index(out1, `backup_set="prod/zzz"`)
	if idxB == -1 || idxA == -1 || idxB > idxA {
		t.Fatalf("expected prod/aaa to sort before prod/zzz in output:\n%s", out1)
	}
}

func TestRenderDoesNotMutateReportBackupSetsOrder(t *testing.T) {
	a := mustSet("prod", "zzz")
	b := mustSet("prod", "aaa")
	report := health.NewReport(health.ProcessHealth{}, []health.BackupSetHealth{{Set: a}, {Set: b}}, time.Now())

	_ = Render(report)

	if report.BackupSets[0].Set != a || report.BackupSets[1].Set != b {
		t.Fatalf("Render must not mutate report.BackupSets in place; got order %v, %v", report.BackupSets[0].Set, report.BackupSets[1].Set)
	}
}

// TestQuoteLabelEscapesBackslashQuoteAndNewline covers a real input this
// package must render safely, not a hypothetical one: model.BackupSetID's
// own validation (internal/model/ids.go's validPart) forbids "/" and
// control characters, but not backslash or double quote.
func TestQuoteLabelEscapesBackslashQuoteAndNewline(t *testing.T) {
	set, err := model.NewBackupSetID(`weird\name`, `has"quote`)
	if err != nil {
		t.Fatalf("unexpected error constructing BackupSetID: %v", err)
	}
	report := health.NewReport(health.ProcessHealth{}, []health.BackupSetHealth{{Set: set}}, time.Now())

	out := Render(report)

	want := `backup_set="weird\\name/has\"quote"`
	if !strings.Contains(out, want) {
		t.Fatalf("expected escaped label %q in output:\n%s", want, out)
	}
}

func TestFormatFloatAvoidsScientificNotation(t *testing.T) {
	got := formatFloat(1e9)
	if strings.ContainsAny(got, "eE") {
		t.Fatalf("formatFloat(1e9) = %q, want plain decimal notation", got)
	}
	if got != "1000000000" {
		t.Fatalf("formatFloat(1e9) = %q, want %q", got, "1000000000")
	}
}

func TestContentTypeIsPrometheusTextFormat(t *testing.T) {
	if !strings.HasPrefix(ContentType, "text/plain") {
		t.Fatalf("ContentType = %q, want a text/plain content type", ContentType)
	}
}
