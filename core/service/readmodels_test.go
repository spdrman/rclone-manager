package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/health"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// TestListArtifacts_ReportsWhatARealCycleProduced is GET /api/v1/backups.
func TestListArtifacts_ReportsWhatARealCycleProduced(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	before, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts (before): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("len (before) = %d, want 0; the assertion below would then not be evidence a cycle did anything", len(before))
	}

	runOneCycle(t, svc)

	got, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1; got %+v", len(got), got)
	}
	a := got[0]
	if a.ID != "production/postgres-primary/backup.dump" {
		t.Errorf("ID = %q, want production/postgres-primary/backup.dump", a.ID)
	}
	if a.BackupSetID != "production/postgres-primary" {
		t.Errorf("BackupSetID = %q", a.BackupSetID)
	}
	if a.SourceName != "production" || a.SetName != "postgres-primary" || a.Name != "backup.dump" {
		t.Errorf("identity split wrong: %+v", a)
	}
	if a.State == "" {
		t.Error("State is empty, so nothing can render this artifact's lifecycle")
	}
	if a.LocalPath == "" {
		t.Error("LocalPath is empty for a committed artifact")
	}
	if a.DiscoveredAt.IsZero() {
		t.Error("DiscoveredAt is zero")
	}
	if a.Quarantined {
		t.Errorf("Quarantined = true for a clean cycle: %+v", a)
	}
	if a.Validation != "passed" && a.Validation != "pending" {
		t.Errorf("Validation = %q, want passed or pending, never a bare empty string", a.Validation)
	}
}

// TestListArtifacts_FilterSelectsOneBackupSet, with the unfiltered read
// above as the control: a filter that matched nothing at all would
// otherwise look identical to a filter that worked.
func TestListArtifacts_FilterSelectsOneBackupSet(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	all, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("the control read found nothing, so a filter test proves nothing")
	}

	matching, err := svc.ListArtifacts(ctx, ArtifactFilter{BackupSetID: "production/postgres-primary"})
	if err != nil {
		t.Fatalf("ListArtifacts (matching): %v", err)
	}
	if len(matching) != len(all) {
		t.Errorf("len (matching) = %d, want %d", len(matching), len(all))
	}

	// A filter naming nothing is REFUSED, not answered with an empty list
	// (the rule issue #187 established for the same filter on the CLI
	// side). An empty list has to keep meaning "this backup set exists and
	// has no backups yet", or a renamed set reads as "your backups are
	// gone".
	for _, id := range []string{"production/nothing-here", "no-such-source/postgres-primary", "malformed"} {
		other, err := svc.ListArtifacts(ctx, ArtifactFilter{BackupSetID: id})
		if !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("ListArtifacts(%q) = %d artifacts, error %v; want ErrBackupSetNotFound", id, len(other), err)
		}
	}
}

func TestGetArtifact_ReturnsOneAndRefusesAnUnknownID(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	got, err := svc.GetArtifact(ctx, "production/postgres-primary/backup.dump")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if got.Name != "backup.dump" {
		t.Errorf("Name = %q", got.Name)
	}

	for _, id := range []string{"production/postgres-primary/nope.dump", "not-an-artifact-id", "a/b"} {
		if _, err := svc.GetArtifact(ctx, id); !errors.Is(err, ErrArtifactNotFound) {
			t.Errorf("GetArtifact(%q) error = %v, want ErrArtifactNotFound", id, err)
		}
	}
}

// TestListArtifacts_QuarantinedOnlyIsEmptyOnAHealthyDeployment. The
// positive control is the unfiltered read: an empty quarantine list is
// only meaningful next to a non-empty artifact list.
func TestListArtifacts_QuarantinedOnlyIsEmptyOnAHealthyDeployment(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	all, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("nothing was backed up at all, so an empty quarantine list says nothing")
	}

	quarantined, err := svc.ListArtifacts(ctx, ArtifactFilter{QuarantinedOnly: true})
	if err != nil {
		t.Fatalf("ListArtifacts(QuarantinedOnly): %v", err)
	}
	if len(quarantined) != 0 {
		t.Errorf("len = %d, want 0 after a clean cycle; got %+v", len(quarantined), quarantined)
	}
}

func TestRetryArtifactIngestion_RefusesAnArtifactThatIsNotQuarantined(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	err := svc.RetryArtifactIngestion(ctx, "production/postgres-primary/backup.dump")
	if !errors.Is(err, ErrArtifactNotQuarantined) {
		t.Fatalf("error = %v, want ErrArtifactNotQuarantined", err)
	}

	if err := svc.RetryArtifactIngestion(ctx, "production/postgres-primary/nope.dump"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("error = %v, want ErrArtifactNotFound", err)
	}
}

func TestRevalidateArtifact_RefusesAnArtifactThatIsNotQuarantined(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	_, err := svc.RevalidateArtifact(ctx, "production/postgres-primary/backup.dump")
	if !errors.Is(err, ErrArtifactNotQuarantined) {
		t.Fatalf("error = %v, want ErrArtifactNotQuarantined", err)
	}

	if _, err := svc.RevalidateArtifact(ctx, "production/postgres-primary/nope.dump"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("error = %v, want ErrArtifactNotFound", err)
	}
}

func TestReinstateArtifact_RefusesAnArtifactThatIsNotQuarantined(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	_, err := svc.ReinstateArtifact(ctx, "production/postgres-primary/backup.dump", "")
	if !errors.Is(err, ErrArtifactNotQuarantined) {
		t.Fatalf("error = %v, want ErrArtifactNotQuarantined", err)
	}

	if _, err := svc.ReinstateArtifact(ctx, "production/postgres-primary/nope.dump", ""); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("error = %v, want ErrArtifactNotFound", err)
	}
}

// TestListActivity_ReportsTheTransitionsARealCycleRecorded is GET
// /api/v1/activity: the feed is a read of the append-only transition log,
// not a second event stream invented at the API boundary.
func TestListActivity_ReportsTheTransitionsARealCycleRecorded(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	before, err := svc.ListActivity(ctx, 0)
	if err != nil {
		t.Fatalf("ListActivity (before): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("len (before) = %d, want 0", len(before))
	}

	runOneCycle(t, svc)

	got, err := svc.ListActivity(ctx, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("len = %d, want at least a discovery and one move; got %+v", len(got), got)
	}
	if got[0].To == "" {
		t.Error("To is empty on the newest event, so nothing can say what happened")
	}
	if got[0].ArtifactID != "production/postgres-primary/backup.dump" {
		t.Errorf("ArtifactID = %q", got[0].ArtifactID)
	}
	if got[0].BackupSetID != "production/postgres-primary" {
		t.Errorf("BackupSetID = %q", got[0].BackupSetID)
	}
	if got[0].OccurredAt.IsZero() {
		t.Error("OccurredAt is zero")
	}
	// Newest first: the oldest event in a pipeline run is the discovery.
	if got[len(got)-1].To != "DISCOVERED" {
		t.Errorf("oldest event = %q, want DISCOVERED; the feed is not newest-first", got[len(got)-1].To)
	}
}

func TestListActivity_ClampsTheLimit(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	all, err := svc.ListActivity(ctx, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(all) < 2 {
		t.Fatalf("only %d events, so a limit test proves nothing", len(all))
	}

	one, err := svc.ListActivity(ctx, 1)
	if err != nil {
		t.Fatalf("ListActivity(1): %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("len = %d, want 1", len(one))
	}

	// Above the cap is clamped, not refused: the caller asked for a feed.
	huge, err := svc.ListActivity(ctx, MaxActivityLimit*10)
	if err != nil {
		t.Fatalf("ListActivity(huge): %v", err)
	}
	if len(huge) != len(all) {
		t.Errorf("len = %d, want %d", len(huge), len(all))
	}
}

// TestListOperations_ReportsSubmittedOperationsNewestFirst is GET
// /api/v1/operations, which was a 405 before issue #211.
func TestListOperations_ReportsSubmittedOperationsNewestFirst(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	before, err := svc.ListOperations(ctx, 0)
	if err != nil {
		t.Fatalf("ListOperations (before): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("len (before) = %d, want 0", len(before))
	}

	runOneCycle(t, svc)

	got, err := svc.ListOperations(ctx, 0)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1; got %+v", len(got), got)
	}
	if got[0].Action != ActionRunCycle {
		t.Errorf("Action = %q, want %q", got[0].Action, ActionRunCycle)
	}
	if got[0].Status != "completed" {
		t.Errorf("Status = %q, want completed", got[0].Status)
	}
	if got[0].ID == "" {
		t.Error("ID is empty")
	}
}

// TestHealth_ReportsEveryConfiguredBackupSet is GET /api/v1/system/health,
// the same FR-24 computation `backup-manager status` prints.
func TestHealth_ReportsEveryConfiguredBackupSet(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	report, err := svc.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(report.BackupSets) != 1 {
		t.Fatalf("len(BackupSets) = %d, want 1", len(report.BackupSets))
	}
	bs := report.BackupSets[0]
	if bs.BackupSetID != "production/postgres-primary" {
		t.Errorf("BackupSetID = %q", bs.BackupSetID)
	}
	if bs.State == "" {
		t.Error("State is empty")
	}
	if bs.Reason == "" {
		t.Error("Reason is empty: a health state with no sentence is a colour with no explanation")
	}
	if report.GeneratedAt.IsZero() {
		t.Error("GeneratedAt is zero")
	}
	// A set that has never produced anything is DEGRADED, never HEALTHY:
	// silence is not evidence (internal/health's own rule).
	if bs.State == "HEALTHY" {
		t.Errorf("State = HEALTHY before any backup ran; silence must never read as healthy")
	}

	runOneCycle(t, svc)

	after, err := svc.Health(ctx)
	if err != nil {
		t.Fatalf("Health (after): %v", err)
	}
	if after.BackupSets[0].NewestGoodBackupAt.IsZero() {
		t.Error("NewestGoodBackupAt is still zero after a successful cycle")
	}
}

// TestScanCatalog_WritesNothingAndRebuildIsANoOpOnAHealthyJournal.
// The scan is the same code path as the rebuild with a dry-run flag, so
// the pair is asserted together: a preview computed by a second
// implementation would be a preview of something else.
func TestScanCatalog_WritesNothingAndRebuildIsANoOpOnAHealthyJournal(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	scan, err := svc.ScanCatalog(ctx)
	if err != nil {
		t.Fatalf("ScanCatalog: %v", err)
	}
	if !scan.DryRun {
		t.Error("DryRun = false on a scan")
	}
	if scan.Scanned == 0 {
		t.Fatalf("Scanned = 0, so this assertion is about an empty deployment: %+v", scan)
	}
	if scan.Reconstructed != 0 {
		t.Errorf("Reconstructed = %d, want 0: the journal already knows about every artifact", scan.Reconstructed)
	}
	if scan.AlreadyPresent == 0 {
		t.Errorf("AlreadyPresent = 0 for a journal that has just recorded an artifact: %+v", scan)
	}

	before, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}

	rebuild, err := svc.RebuildCatalog(ctx)
	if err != nil {
		t.Fatalf("RebuildCatalog: %v", err)
	}
	if rebuild.DryRun {
		t.Error("DryRun = true on a rebuild")
	}
	if rebuild.Reconstructed != 0 {
		t.Errorf("Reconstructed = %d, want 0: a rebuild must never duplicate rows the journal already has", rebuild.Reconstructed)
	}
	if rebuild.AlreadyPresent != scan.AlreadyPresent {
		t.Errorf("the rebuild saw %d already-present artifacts and its own dry run saw %d; the preview is meant to predict the real pass exactly",
			rebuild.AlreadyPresent, scan.AlreadyPresent)
	}

	after, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts (after): %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("artifact count moved from %d to %d across a rebuild of a healthy journal", len(before), len(after))
	}
}

// TestSetBackupSetEnabled_PersistsAndHotReloads is POST
// /api/v1/backup-sets/{id}/enabled.
func TestSetBackupSetEnabled_PersistsAndHotReloads(t *testing.T) {
	svc, configPath := openTestService(t)
	ctx := context.Background()

	before, err := svc.GetBackupSet(ctx, "production/postgres-primary")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if before.Disabled {
		t.Fatal("the fixture backup set is already disabled, so turning it off proves nothing")
	}
	revisionBefore := svc.ConfigRevision()

	updated, err := svc.SetBackupSetEnabled(ctx, "production/postgres-primary", false)
	if err != nil {
		t.Fatalf("SetBackupSetEnabled(false): %v", err)
	}
	if !updated.Disabled {
		t.Error("the returned backup set still reports Disabled = false")
	}

	live, err := svc.GetBackupSet(ctx, "production/postgres-primary")
	if err != nil {
		t.Fatalf("GetBackupSet (after): %v", err)
	}
	if !live.Disabled {
		t.Error("the running service still reports the set as enabled: the hot reload did not happen")
	}
	if svc.ConfigRevision() == revisionBefore {
		t.Error("ConfigRevision did not move, so an outstanding retention plan would not be recognised as stale")
	}

	onDisk, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(onDisk), "disabled: true") {
		t.Errorf("the configuration file does not record the change:\n%s", onDisk)
	}

	// And back on again, so the operation is proven reversible rather
	// than one-way.
	if _, err := svc.SetBackupSetEnabled(ctx, "production/postgres-primary", true); err != nil {
		t.Fatalf("SetBackupSetEnabled(true): %v", err)
	}
	reEnabled, err := svc.GetBackupSet(ctx, "production/postgres-primary")
	if err != nil {
		t.Fatalf("GetBackupSet (re-enabled): %v", err)
	}
	if reEnabled.Disabled {
		t.Error("the set is still disabled after being turned back on")
	}
}

// TestSetBackupSetEnabled_DisablingDeletesNothing is the safety claim that
// justifies this route not standing behind the destructive gate.
func TestSetBackupSetEnabled_DisablingDeletesNothing(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	before, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("nothing was backed up, so \"deletes nothing\" is not observable here")
	}
	localPath := before[0].LocalPath
	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("the committed local file is not on disk to begin with: %v", err)
	}

	if _, err := svc.SetBackupSetEnabled(ctx, "production/postgres-primary", false); err != nil {
		t.Fatalf("SetBackupSetEnabled: %v", err)
	}

	after, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts (after): %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("artifact count moved from %d to %d across a disable", len(before), len(after))
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Errorf("the committed local file is gone after a disable: %v", err)
	}
}

func TestSetBackupSetEnabled_RefusesAnUnknownBackupSet(t *testing.T) {
	svc, _ := openTestService(t)

	for _, id := range []string{"production/nope", "no-such-source/postgres-primary", "malformed"} {
		if _, err := svc.SetBackupSetEnabled(context.Background(), id, false); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("SetBackupSetEnabled(%q) error = %v, want ErrBackupSetNotFound", id, err)
		}
	}
}

// TestTestBackupSetConnection_ChecksThePersistedSetWithoutBeingToldItsSecrets
// is the persisted half of POST /api/v1/backup-sets/test-connection. The
// fixture's source is a local one, so a successful list here proves the
// stored configuration was actually resolved and used: a caller supplied
// nothing but an id.
func TestTestBackupSetConnection_ChecksThePersistedSetWithoutBeingToldItsSecrets(t *testing.T) {
	svc, _ := openTestService(t)

	got, err := svc.TestBackupSetConnection(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("TestBackupSetConnection: %v", err)
	}
	if !got.OK {
		t.Fatalf("OK = false (%q) against the fixture's own reachable source", got.Message)
	}
	if got.Message != "" {
		t.Errorf("Message = %q, want empty on success", got.Message)
	}
}

func TestTestBackupSetConnection_RefusesAnUnknownBackupSet(t *testing.T) {
	svc, _ := openTestService(t)

	for _, id := range []string{"production/nope", "no-such-source/postgres-primary", "malformed"} {
		if _, err := svc.TestBackupSetConnection(context.Background(), id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("TestBackupSetConnection(%q) error = %v, want ErrBackupSetNotFound", id, err)
		}
	}
}

// TestHealth_CarriesTheCapacityAssessmentForEachSet: one call has to be
// able to answer "are my backups healthy" completely, and a set landing on
// a nearly-full disk is not healthy in any useful sense. The control is
// ListStorageStatus itself, so an empty capacity block here cannot be
// mistaken for "this deployment has no readable destination".
func TestHealth_CarriesTheCapacityAssessmentForEachSet(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	runOneCycle(t, svc)

	statuses, err := svc.ListStorageStatus(ctx)
	if err != nil {
		t.Fatalf("ListStorageStatus: %v", err)
	}
	if len(statuses) != 1 || !statuses[0].Available {
		t.Fatalf("the capacity control reports %+v, so the health assertion below would prove nothing", statuses)
	}

	report, err := svc.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	bs := report.BackupSets[0]
	if bs.TotalBytes != statuses[0].TotalBytes {
		t.Errorf("TotalBytes = %d, want %d (the same reading ListStorageStatus gives)", bs.TotalBytes, statuses[0].TotalBytes)
	}
	if bs.StorageLevel != statuses[0].Level {
		t.Errorf("StorageLevel = %q, want %q", bs.StorageLevel, statuses[0].Level)
	}
	if bs.StorageLevel == "" {
		t.Error("StorageLevel is empty for a readable destination, so an unreadable one would be indistinguishable")
	}
}

// TestToServiceBackupSetHealth_CarriesEveryCountThrough. This layer is a
// pure translation from internal/health's value to the provider-agnostic
// one the API renders, and the way a translation breaks is by silently
// dropping a field: the result still compiles, still serialises, and
// reports a confident zero for something the core computed correctly.
//
// Issue #227's reinstated-remote count is the one that matters most here,
// because zero is its resting value: a dropped field looks exactly like a
// deployment that has never reinstated anything, which is the reassuring
// answer. Every other count is asserted alongside it with a distinct
// value, so a mapping that crossed two fields fails rather than passing on
// a pair that happened to match.
func TestToServiceBackupSetHealth_CarriesEveryCountThrough(t *testing.T) {
	got := toServiceBackupSetHealth(health.BackupSetHealth{
		Set:                           mustBackupSetID(t, "production", "postgres-primary"),
		State:                         health.Degraded,
		Reason:                        "a known-good backup exists but the newest artifact is quarantined",
		PendingDeletes:                2,
		Failures:                      3,
		QuarantinedCount:              4,
		QuarantinedLostCount:          1,
		ReinstatedRemoteRetainedCount: 5,
	})

	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"PendingDeletes", got.PendingDeletes, 2},
		{"Failures", got.Failures, 3},
		{"QuarantinedCount", got.QuarantinedCount, 4},
		{"QuarantinedLostCount", got.QuarantinedLostCount, 1},
		{"ReinstatedRemoteRetainedCount", got.ReinstatedRemoteRetainedCount, 5},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func mustBackupSetID(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}
