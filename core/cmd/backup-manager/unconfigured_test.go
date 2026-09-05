package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The surface for backups whose backup set is no longer configured, and the
// residue a removal can strand.
//
// The fixture plants a row at TRANSFERRING pointing at a .partial file that
// really is on disk, because that is what a cycle interrupted by a removal
// leaves behind and a healthy run can never produce it: a healthy run
// finishes. Writing it through the journal directly is the only way to reach
// the state the command exists to clear.
//
// What the cells are guarding is a category of backup that is invisible,
// ungoverned and growing. Nothing retains, reconciles or advances these
// artifacts, so the promises worth checking are that they are still listed,
// that they are marked where they appear, and that clearing
// the residue destroys no backup.

var strandedFixtureEpoch = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// stageStrandedArtifact plants the shape a cycle stopped mid-flight by a
// removal leaves behind: a journal row at TRANSFERRING pointing at a
// .partial file that really is on the disk.
//
// It writes against the config's own state database directly, for the
// same reason quarantine_test.go's own fixture does: a healthy `run` can
// never produce this state, because a healthy `run` finishes the
// transfer. The row it builds is the real thing, driven through the real
// journal on the real edge, not a hand-written state string.
func stageStrandedArtifact(t *testing.T, configPath, name string) (model.ArtifactID, string) {
	t.Helper()
	dir := filepath.Dir(configPath)
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	partial := filepath.Join(localDir, name+".partial")
	if err := os.WriteFile(partial, []byte("half of "+name), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx := context.Background()
	j, err := state.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := j.Discover(ctx, artifact, name+"-discover", "/backups/"+name, state.RemoteIdentity{}, strandedFixtureEpoch); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        name + "-transferring",
		From:       string(lifecycle.Discovered),
		To:         string(lifecycle.Transferring),
		LocalPath:  &partial,
		OccurredAt: strandedFixtureEpoch,
	}); err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}
	return artifact, partial
}

// removeTheOnlySet takes the fixture's one backup set out of the
// configuration through the real `backup-set remove` verb, which is what
// creates the state this whole issue is about. Doing it through the CLI
// rather than by editing the YAML means these tests break if removal ever
// stops leaving the journal alone.
func removeTheOnlySet(t *testing.T, configPath string) {
	t.Helper()
	out := captureStdout(t, func() {
		if code := run([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"}); code != 0 {
			t.Fatalf("backup-set remove = %d, want 0", code)
		}
	})
	_ = out
}

// TestRun_Unconfigured_NamesTheRemovedSetAndSaysNoPolicyGovernsIt is
// issue #418's third acceptance criterion driven end to end: a real
// cycle, a real removal, and then the surface that lists what is left has
// to name the policy governing it, which is none.
//
// Before this command existed there was no answer to that question on any
// surface. `artifacts` listed the rows and said nothing about them,
// `retention` never mentioned the set at all because it walks the
// configuration, and the only record of the removal was one event in a
// log.
func TestRun_Unconfigured_NamesTheRemovedSetAndSaysNoPolicyGovernsIt(t *testing.T) {
	configPath := writeTestConfig(t)
	if code := run([]string{"run", "--config", configPath}); code != 0 {
		t.Fatalf("run = %d, want 0; this test needs one finished backup to be left behind", code)
	}
	removeTheOnlySet(t, configPath)

	out := captureStdout(t, func() {
		if code := run([]string{"unconfigured", "--config", configPath}); code != 0 {
			t.Errorf("unconfigured = %d, want 0", code)
		}
	})

	if !strings.Contains(out, "production/postgres-primary") {
		t.Errorf("the report does not name the removed set:\n%s", out)
	}
	if !strings.Contains(out, "retention policy: none") {
		t.Errorf("the report does not say which policy governs these backups, which is the one thing this issue asks a listing surface to say:\n%s", out)
	}
	if !strings.Contains(out, "1 retained") {
		t.Errorf("the report does not count the backup that is pinned on storage:\n%s", out)
	}
}

// TestRun_Unconfigured_SaysSoWhenEveryRecordedSetIsConfigured is the
// positive control. An empty answer here has to be a statement rather
// than silence, because "nothing outside a policy" and "this command did
// not look" read identically as a blank screen.
func TestRun_Unconfigured_SaysSoWhenEveryRecordedSetIsConfigured(t *testing.T) {
	configPath := writeTestConfig(t)
	if code := run([]string{"run", "--config", configPath}); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}

	out := captureStdout(t, func() {
		if code := run([]string{"unconfigured", "--config", configPath}); code != 0 {
			t.Errorf("unconfigured = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "every backup set on record is configured") {
		t.Errorf("with nothing to report the command printed:\n%s", out)
	}
	if strings.Contains(out, "retention policy: none") {
		t.Errorf("a configured set was reported as ungoverned:\n%s", out)
	}
}

// TestRun_UnconfiguredClear_PreviewsBeforeItChangesAnything is acceptance
// criterion two at the surface an operator types at, and it pins the
// half that makes the operation safe to offer: the default is a preview.
func TestRun_UnconfiguredClear_PreviewsBeforeItChangesAnything(t *testing.T) {
	configPath := writeTestConfig(t)
	artifact, partial := stageStrandedArtifact(t, configPath, "stuck.dump")
	removeTheOnlySet(t, configPath)

	preview := captureStdout(t, func() {
		if code := run([]string{"unconfigured", "--config", configPath, "clear", "production/postgres-primary"}); code != 0 {
			t.Errorf("unconfigured clear (preview) = %d, want 0", code)
		}
	})
	if !strings.Contains(preview, "would clear") {
		t.Errorf("the preview does not say what it would do:\n%s", preview)
	}
	if !strings.Contains(preview, "Nothing has been changed") {
		t.Errorf("the preview does not say that it changed nothing:\n%s", preview)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("the preview removed %s: %v", partial, err)
	}

	applied := captureStdout(t, func() {
		if code := run([]string{"unconfigured", "--config", configPath, "clear", "production/postgres-primary", "--acknowledge"}); code != 0 {
			t.Errorf("unconfigured clear --acknowledge = %d, want 0", code)
		}
	})
	if !strings.Contains(applied, "cleared") {
		t.Errorf("the acknowledged run does not say what it cleared:\n%s", applied)
	}
	if !strings.Contains(applied, "untouched and still listed") {
		t.Errorf("the acknowledged run does not say the retained backups are untouched, which is the half an operator cannot see on disk:\n%s", applied)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want the residue gone", partial, err)
	}
	if got := stateOf(t, configPath, artifact); got != string(lifecycle.Failed) {
		t.Errorf("%s is %s after the sweep, want FAILED", artifact, got)
	}
}

// TestRun_UnconfiguredClear_RefusesASetTheConfigurationStillHas. The
// refusal is the whole safety story of this verb at the CLI: run it
// against a live set and it must not go anywhere near a transfer the
// cycle is in the middle of.
func TestRun_UnconfiguredClear_RefusesASetTheConfigurationStillHas(t *testing.T) {
	configPath := writeTestConfig(t)
	artifact, partial := stageStrandedArtifact(t, configPath, "stuck.dump")

	out := captureStderr(t, func() {
		if code := run([]string{"unconfigured", "--config", configPath, "clear", "production/postgres-primary", "--acknowledge"}); code == 0 {
			t.Error("unconfigured clear against a configured set = 0, want a failure")
		}
	})
	if !strings.Contains(out, "still configured") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("the refused sweep removed %s anyway: %v", partial, err)
	}
	if got := stateOf(t, configPath, artifact); got != string(lifecycle.Transferring) {
		t.Errorf("%s is %s, want it left at TRANSFERRING for the cycle to resume", artifact, got)
	}
}

// TestUsage_NamesEveryTopLevelCommand closes the same gap
// TestUsage_NamesEveryBackupSetVerb closes one level down, and it is the
// same gap `backup-set remove` shipped through: usage() is where an
// operator discovers a command and where the tests repo's black-box guard
// reads them out of, so a command absent from it is one nobody can find
// and nothing can check.
func TestUsage_NamesEveryTopLevelCommand(t *testing.T) {
	out := captureStderr(t, usage)
	var missing []string
	for name := range commands {
		// version is listed, help is not a command; every entry of the
		// map has to appear as the first word of a usage line.
		if !strings.Contains(out, "\n  "+name+" ") {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("usage() does not list %v; an operator cannot discover them and the black-box command guard cannot see them", missing)
	}
}

// stateOf reads one artifact's current lifecycle state straight out of
// the journal the config names, so a CLI test can assert what the command
// actually wrote rather than what it printed.
func stateOf(t *testing.T, configPath string, id model.ArtifactID) string {
	t.Helper()
	ctx := context.Background()
	j, err := state.Open(ctx, filepath.Join(filepath.Dir(configPath), "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()
	rec, err := j.Get(ctx, id)
	if err != nil {
		t.Fatalf("journal.Get(%s): %v", id, err)
	}
	return rec.State
}

// TestRun_Retention_NamesTheSetsNoPolicyGovernsAtAll is acceptance
// criterion three on the command whose whole subject is which policy
// applies to what. `retention` walks the configuration, so before this a
// removed set's backups were not merely unmarked here, they were absent,
// and absence reads as "nothing to think about".
func TestRun_Retention_NamesTheSetsNoPolicyGovernsAtAll(t *testing.T) {
	configPath := writeTestConfig(t)
	if code := run([]string{"run", "--config", configPath}); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	removeTheOnlySet(t, configPath)

	out := captureStdout(t, func() {
		if code := run([]string{"retention", "--config", configPath}); code != 0 {
			t.Errorf("retention = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "retained under no policy at all") {
		t.Errorf("retention does not mention the backups no chain selects:\n%s", out)
	}
	if !strings.Contains(out, "production/postgres-primary") {
		t.Errorf("retention does not name the set:\n%s", out)
	}
}

// TestRun_Retention_SaysNothingExtraWhenEverySetIsConfigured is the
// control that keeps the addition additive. This command's output is
// pinned by the black-box suite in spdrman/rclone-manager-tests, and
// every case there is a configured-sets-only deployment, so printing this
// section unconditionally would mean a cross-repo pin move for a line
// that says "none".
func TestRun_Retention_SaysNothingExtraWhenEverySetIsConfigured(t *testing.T) {
	configPath := writeTestConfig(t)
	if code := run([]string{"run", "--config", configPath}); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}

	out := captureStdout(t, func() {
		if code := run([]string{"retention", "--config", configPath}); code != 0 {
			t.Errorf("retention = %d, want 0", code)
		}
	})
	if strings.Contains(out, "no policy at all") {
		t.Errorf("retention printed the unconfigured section for a deployment that has none:\n%s", out)
	}
}

// TestRun_Artifacts_MarksTheRowsNoPolicyGoverns. This list is the one
// screen the removal confirmation promises those backups stay on, and
// since #391 widened it they have appeared here as ordinary rows of an
// ordinary set. The marker is what makes the promise honest rather than
// merely kept.
func TestRun_Artifacts_MarksTheRowsNoPolicyGoverns(t *testing.T) {
	configPath := writeTestConfig(t)
	if code := run([]string{"run", "--config", configPath}); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}

	before := captureStdout(t, func() {
		if code := run([]string{"artifacts", "--config", configPath}); code != 0 {
			t.Errorf("artifacts = %d, want 0", code)
		}
	})
	if strings.Contains(before, "configuration removed") {
		t.Fatalf("a configured set's rows were marked:\n%s", before)
	}

	removeTheOnlySet(t, configPath)

	after := captureStdout(t, func() {
		if code := run([]string{"artifacts", "--config", configPath}); code != 0 {
			t.Errorf("artifacts = %d, want 0", code)
		}
	})
	if !strings.Contains(after, "backup.dump") {
		t.Fatalf("the removed set's backup stopped being listed, which is the promise #391 makes:\n%s", after)
	}
	if !strings.Contains(after, "no retention policy") {
		t.Errorf("the row is listed with nothing saying which policy governs it:\n%s", after)
	}
}

// TestRun_Status_ReportsWhatIsRetainedOutsideEveryConfiguredSet puts the
// same fact on the screen an operator opens to ask whether anything is
// wrong, and pins the half that keeps it usable: it does not fail the
// command, because nothing has failed and a healthcheck that flapped on
// a removal would teach people to ignore it.
func TestRun_Status_ReportsWhatIsRetainedOutsideEveryConfiguredSet(t *testing.T) {
	configPath := writeTestConfig(t)
	if code := run([]string{"run", "--config", configPath}); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	removeTheOnlySet(t, configPath)

	out := captureStdout(t, func() {
		if code := run([]string{"status", "--config", configPath}); code != 0 {
			t.Errorf("status = %d, want 0; a removed backup set is a state, not a failure", code)
		}
	})
	if !strings.Contains(out, "retained outside every backup set this configuration names") {
		t.Errorf("status says nothing about backups no configured set accounts for:\n%s", out)
	}
	if !strings.Contains(out, "under no retention policy") {
		t.Errorf("status does not name the policy governing them:\n%s", out)
	}
}
