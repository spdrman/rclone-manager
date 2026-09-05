package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is issue #418 at the boundary: the read model that lets any
// surface, not only a terminal, say what a removed backup set still holds
// and what governs it.

var unconfiguredFixtureEpoch = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// strandOneArtifact plants the shape a cycle stopped mid-flight by a
// removal hold leaves behind: a journal row at TRANSFERRING with a real
// .partial file under the set's own local directory.
//
// It writes through this service's own journal, on the real
// DISCOVERED -> TRANSFERRING edge, because a fixture that set the state
// column by hand would prove nothing about a state the machine actually
// admits.
func strandOneArtifact(t *testing.T, svc *BackupService, setID, localDir, name string) (model.ArtifactID, string) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	partial := filepath.Join(localDir, name+".partial")
	if err := os.WriteFile(partial, []byte("half of "+name), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	source, set, ok := splitBackupSetID(setID)
	if !ok {
		t.Fatalf("splitBackupSetID(%q) refused a well-formed id", setID)
	}
	parsed, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(parsed, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := svc.journal.Discover(ctx, artifact, name+"-discover", "/backups/"+name, state.RemoteIdentity{}, unconfiguredFixtureEpoch); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := svc.journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        name + "-transferring",
		From:       string(lifecycle.Discovered),
		To:         string(lifecycle.Transferring),
		LocalPath:  &partial,
		OccurredAt: unconfiguredFixtureEpoch,
	}); err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}
	return artifact, partial
}

func stateOfArtifact(t *testing.T, svc *BackupService, id model.ArtifactID) string {
	t.Helper()
	rec, err := svc.journal.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("journal.Get(%s): %v", id, err)
	}
	return rec.State
}

// TestUnconfiguredBackupSets_NamesARemovedSetAndSaysNoPolicyGovernsIt is
// the read every non-terminal surface needs and did not have. Before it,
// a removed set's backups were reachable only as anonymous rows on the
// widened artifact list, with nothing anywhere naming the set they belong
// to or the policy they are kept under.
func TestUnconfiguredBackupSets_NamesARemovedSetAndSaysNoPolicyGovernsIt(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if got := cycleReportFrom(svc.state.Load().inner, svc.holds); len(got.Sets) == 0 {
		t.Fatal("the seeding cycle covered no sets, so there would be nothing retained to report")
	}

	before, err := svc.UnconfiguredBackupSets(ctx)
	if err != nil {
		t.Fatalf("UnconfiguredBackupSets: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("UnconfiguredBackupSets reported %+v before any removal, want nothing", before)
	}

	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	after, err := svc.UnconfiguredBackupSets(ctx)
	if err != nil {
		t.Fatalf("UnconfiguredBackupSets: %v", err)
	}
	if len(after) != 1 || after[0].BackupSetID != "production/alpha" {
		t.Fatalf("UnconfiguredBackupSets = %+v, want exactly production/alpha", after)
	}
	if after[0].RetentionPolicy != "none" {
		t.Errorf("RetentionPolicy = %q, want \"none\"; a caller has to be able to RENDER which policy governs these, not infer it from a missing field", after[0].RetentionPolicy)
	}
	if after[0].Retained != 1 {
		t.Errorf("Retained = %d, want 1; the backup the confirmation promised would stay is the number this report exists to carry", after[0].Retained)
	}
	if after[0].Bytes <= 0 {
		t.Errorf("Bytes = %d, want what this set still occupies", after[0].Bytes)
	}
}

// TestClearStrandedArtifacts_ClearsTheResidueAndKeepsEveryBackup is the
// boundary half of acceptance criterion two, and the safety property with
// it: the retained backup is still there and still listed afterwards.
func TestClearStrandedArtifacts_ClearsTheResidueAndKeepsEveryBackup(t *testing.T) {
	svc, _, localA, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if got := cycleReportFrom(svc.state.Load().inner, svc.holds); len(got.Sets) == 0 {
		t.Fatal("the seeding cycle covered no sets")
	}
	kept := artifactIDsUnderSet(t, svc, "production/alpha")
	if len(kept) == 0 {
		t.Fatal("production/alpha journaled nothing, so this test could not tell a kept backup from a swept one")
	}

	stuck, partial := strandOneArtifact(t, svc, "production/alpha", localA, "stuck.dump")
	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	preview, err := svc.PreviewStrandedArtifacts(ctx, "production/alpha")
	if err != nil {
		t.Fatalf("PreviewStrandedArtifacts: %v", err)
	}
	if len(preview) != 1 || preview[0].ID != stuck.String() || preview[0].Cleared {
		t.Fatalf("PreviewStrandedArtifacts = %+v, want exactly %s, uncleared", preview, stuck)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("the preview removed %s: %v", partial, err)
	}

	cleared, err := svc.ClearStrandedArtifacts(ctx, "production/alpha")
	if err != nil {
		t.Fatalf("ClearStrandedArtifacts: %v", err)
	}
	if len(cleared) != 1 || !cleared[0].Cleared {
		t.Fatalf("ClearStrandedArtifacts = %+v, want one cleared row", cleared)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%s) = %v, want the residue gone", partial, err)
	}
	if got := stateOfArtifact(t, svc, stuck); got != string(lifecycle.Failed) {
		t.Errorf("%s is %s, want FAILED", stuck, got)
	}

	// And the promise the removal made is still kept.
	if got := artifactIDsUnderSet(t, svc, "production/alpha"); len(got) != len(kept)+1 {
		t.Errorf("production/alpha lists %d artifact(s) after the sweep, want the %d it had plus the swept row; the sweep must not take a backup off the Backups list", len(got), len(kept))
	}
	for _, id := range kept {
		if _, err := svc.GetArtifact(ctx, id); err != nil {
			t.Errorf("GetArtifact(%s) after the sweep: %v; a retained backup was destroyed", id, err)
		}
	}
}

// TestClearStrandedArtifacts_RefusesASetTheConfigurationStillHas. The
// sentinel matters as much as the refusal: a caller that could not tell
// this from "no such backup set" would show an operator a 404 for a set
// on their own dashboard.
func TestClearStrandedArtifacts_RefusesASetTheConfigurationStillHas(t *testing.T) {
	svc, _, localA, _ := openRemovalFixtureService(t)
	ctx := context.Background()
	stuck, partial := strandOneArtifact(t, svc, "production/alpha", localA, "stuck.dump")

	_, err := svc.ClearStrandedArtifacts(ctx, "production/alpha")
	if !errors.Is(err, ErrBackupSetConfigured) {
		t.Fatalf("ClearStrandedArtifacts on a configured set = %v, want ErrBackupSetConfigured", err)
	}
	if errors.Is(err, ErrBackupSetNotFound) {
		t.Errorf("the refusal also reads as ErrBackupSetNotFound (%v); a caller turning that into a 404 would tell an operator a set on their own dashboard does not exist", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("the refused call removed %s: %v", partial, err)
	}
	if got := stateOfArtifact(t, svc, stuck); got != string(lifecycle.Transferring) {
		t.Errorf("%s is %s, want it left for the cycle to resume", stuck, got)
	}
}

// TestClearStrandedArtifacts_RefusesAnIDNothingHasEverHeard follows issue
// #187's rule at this boundary too: on an operation that removes files, a
// typo answered with "nothing to clear" is a success message for
// something that never happened.
func TestClearStrandedArtifacts_RefusesAnIDNothingHasEverHeard(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	for _, id := range []string{"production/never-existed", "not-an-id", "a/b/c"} {
		if _, err := svc.ClearStrandedArtifacts(ctx, id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("ClearStrandedArtifacts(%q) = %v, want ErrBackupSetNotFound", id, err)
		}
	}
}

// TestRemoveBackupSet_SaysHowManyArtifactsItStranded. The removal is the
// one moment that knows what it just caught mid-flight, and #410 left
// that residue documented and uncounted. A number in the event is what
// makes "did that removal leave anything half-done" answerable later
// without re-deriving it from a journal that has moved on since.
func TestRemoveBackupSet_SaysHowManyArtifactsItStranded(t *testing.T) {
	svc, _, localA, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if got := cycleReportFrom(svc.state.Load().inner, svc.holds); len(got.Sets) == 0 {
		t.Fatal("the seeding cycle covered no sets")
	}
	strandOneArtifact(t, svc, "production/alpha", localA, "stuck.dump")

	var log bytes.Buffer
	svc.logger = obs.New(&log, obs.LevelInfo)
	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	line := log.String()
	if !strings.Contains(line, `"stranded_artifacts":1`) {
		t.Errorf(`the removal event does not report "stranded_artifacts":1, so nothing records what it caught mid-flight:`+"\n%s", line)
	}
	if !strings.Contains(line, `"retained_artifacts":2`) {
		t.Errorf(`the removal event does not still report every row it left behind:`+"\n%s", line)
	}
}

// TestRemoveBackupSet_SaysZeroStrandedWhenItStrandedNothing is the
// positive control for the count above. Zero has to be stated rather than
// omitted: an absent field and "this removal left nothing half-done" read
// identically, and only one of them is a claim.
func TestRemoveBackupSet_SaysZeroStrandedWhenItStrandedNothing(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if got := cycleReportFrom(svc.state.Load().inner, svc.holds); len(got.Sets) == 0 {
		t.Fatal("the seeding cycle covered no sets")
	}

	var log bytes.Buffer
	svc.logger = obs.New(&log, obs.LevelInfo)
	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}
	if line := log.String(); !strings.Contains(line, `"stranded_artifacts":0`) {
		t.Errorf(`a removal that stranded nothing does not say so:`+"\n%s", line)
	}
}
