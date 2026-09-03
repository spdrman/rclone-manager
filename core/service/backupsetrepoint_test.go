package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// runOneCycleForHistory drives one real cycle so the fixture set has
// artifacts on record, and proves it did: every test below turns on the
// difference between a set with history and one without, so a fixture
// that silently journaled nothing would make the whole file vacuous.
func runOneCycleForHistory(t *testing.T, svc *BackupService) {
	t.Helper()
	runOneCycle(t, svc)
	artifacts, err := svc.ListArtifacts(context.Background(), ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) == 0 {
		t.Fatal("the fixture cycle journaled no artifacts, so this test would prove nothing about a set that has history")
	}
}

// TestUpdateBackupSet_RefusesToRepointASetWithHistoryWithoutAnAcknowledgement
// is the central one. An operator who changes local_path on a set that
// already holds forty artifacts is doing something with consequences
// neither the config file nor the next cycle report will mention: every
// one of those artifacts stops matching what retention computes for it,
// so it is refused rather than pruned from then on. The moment to say so
// is the moment of the change.
//
// The file is checked byte for byte afterwards, because a refusal that
// had already written half the edit would be worse than no refusal.
func TestUpdateBackupSet_RefusesToRepointASetWithHistoryWithoutAnAcknowledgement(t *testing.T) {
	svc, configPath := openTestService(t)
	runOneCycleForHistory(t, svc)

	before := mustRead(t, configPath)
	elsewhere := filepath.Join(t.TempDir(), "moved")

	_, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		LocalPath: strPtr(elsewhere),
	})
	if !errors.Is(err, ErrRepointNotAcknowledged) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrRepointNotAcknowledged", err)
	}
	// The refusal has to be actionable: which field, what it costs, and
	// how many artifacts are actually on record. A bare "are you sure"
	// is the shape this project's own issues rule out.
	for _, want := range []string{"local_path", elsewhere, "acknowledge_repoint"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if after := mustRead(t, configPath); after != before {
		t.Error("the configuration file changed even though the update was refused")
	}
}

// TestUpdateBackupSet_RepointsASetWithHistoryOnceAcknowledged: the
// refusal above is an acknowledgement, not a prohibition. An operator
// whose volume actually moved has a real change to make, and hand-editing
// config.yaml is exactly what this update path exists to replace.
func TestUpdateBackupSet_RepointsASetWithHistoryOnceAcknowledged(t *testing.T) {
	svc, configPath := openTestService(t)
	runOneCycleForHistory(t, svc)

	elsewhere := filepath.Join(t.TempDir(), "moved")
	updated, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		LocalPath:          strPtr(elsewhere),
		AcknowledgeRepoint: true,
	})
	if err != nil {
		t.Fatalf("UpdateBackupSet with an acknowledgement: %v", err)
	}
	if updated.LocalPath != elsewhere {
		t.Errorf("LocalPath = %q, want %q", updated.LocalPath, elsewhere)
	}
	if got := readBackupSetFromDisk(t, configPath, "production", "postgres-primary").LocalPath; got != elsewhere {
		t.Errorf("on-disk local_path = %q, want %q", got, elsewhere)
	}
}

// TestUpdateBackupSet_ASetWithNoHistoryIsRepointedWithNoCeremony is the
// control that keeps the refusal from being a blanket one. Correcting a
// mis-typed path minutes after the wizard wrote it is the single most
// ordinary edit there is, and there is nothing to orphan, so it must not
// be made to feel dangerous.
func TestUpdateBackupSet_ASetWithNoHistoryIsRepointedWithNoCeremony(t *testing.T) {
	svc, _ := openTestService(t)

	elsewhere := filepath.Join(t.TempDir(), "corrected")
	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		LocalPath: strPtr(elsewhere),
	}); err != nil {
		t.Fatalf("UpdateBackupSet on a set with no artifacts on record: %v", err)
	}
}

// TestUpdateBackupSet_AnOrdinaryEditOnASetWithHistoryIsNotRefused is the
// other control. The acknowledgement covers the three fields that decide
// which data the set is about; asking for it on a staleness budget would
// make it something an operator clicks through by habit, which protects
// nothing.
func TestUpdateBackupSet_AnOrdinaryEditOnASetWithHistoryIsNotRefused(t *testing.T) {
	svc, _ := openTestService(t)
	runOneCycleForHistory(t, svc)

	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		StaleAfter: durationPtr(48 * time.Hour),
	}); err != nil {
		t.Fatalf("UpdateBackupSet of stale_after on a set with history: %v", err)
	}
}

// TestUpdateBackupSet_ResendingAPathUnchangedIsNotARepoint is the case
// the Web UI walks into. SAVE ALL & EXIT EDIT sends whatever boxes are
// still dirty, and a per-box Save sends that box's contents; a save an
// operator makes for one reason must not start demanding an
// acknowledgement because some other box happened to be in the request
// holding the value it already had.
func TestUpdateBackupSet_ResendingAPathUnchangedIsNotARepoint(t *testing.T) {
	svc, configPath := openTestService(t)
	runOneCycleForHistory(t, svc)

	current := readBackupSetFromDisk(t, configPath, "production", "postgres-primary")
	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		LocalPath:  strPtr(current.LocalPath),
		RemotePath: strPtr(current.RemotePath),
		StaleAfter: durationPtr(36 * time.Hour),
	}); err != nil {
		t.Fatalf("UpdateBackupSet re-sending unchanged paths: %v", err)
	}
}

// TestRepointedFields_NamesExactlyTheFieldsThatDecideWhichDataASetIsAbout
// pins the membership of the list itself, which is the actual decision
// this file makes. remote.host and remote_path are where the data comes
// from and local_path is where it lives; port and user are how you reach
// the same host and who you reach it as, and putting them in would make
// the acknowledgement routine.
func TestRepointedFields_NamesExactlyTheFieldsThatDecideWhichDataASetIsAbout(t *testing.T) {
	current := config.BackupSet{
		Remote:     config.Remote{Host: "nas.internal", Port: 22, User: "backup-agent"},
		RemotePath: "/srv/dumps",
		LocalPath:  "/var/backups/dumps",
	}
	cases := []struct {
		name string
		req  UpdateBackupSetRequest
		want []string
	}{
		{"host", UpdateBackupSetRequest{Host: strPtr("other.internal")}, []string{"remote.host"}},
		{"remote path", UpdateBackupSetRequest{RemotePath: strPtr("/srv/other")}, []string{"remote_path"}},
		{"local path", UpdateBackupSetRequest{LocalPath: strPtr("/var/other")}, []string{"local_path"}},
		{"port", UpdateBackupSetRequest{Port: intPtr(2222)}, nil},
		{"user", UpdateBackupSetRequest{User: strPtr("someone-else")}, nil},
		{"include", UpdateBackupSetRequest{Include: stringsPtr([]string{"*.sql"})}, nil},
		{"host unchanged", UpdateBackupSetRequest{Host: strPtr("nas.internal")}, nil},
		{"all three at once", UpdateBackupSetRequest{
			Host:       strPtr("other.internal"),
			RemotePath: strPtr("/srv/other"),
			LocalPath:  strPtr("/var/other"),
		}, []string{"remote.host", "remote_path", "local_path"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, f := range repointedFields(current, tc.req) {
				got = append(got, f.name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("repointedFields = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUpdateBackupSet_RefusesToRepointTheRemotePathOfASetWithHistory
// covers the worse of the two failure modes end to end. A remote root
// pointed at a different dataset whose file names match ones already on
// record makes every candidate come back already-known: the cycle reports
// success, the health surface stays green, and nothing is fetched. That
// is a backup that has silently stopped happening.
func TestUpdateBackupSet_RefusesToRepointTheRemotePathOfASetWithHistory(t *testing.T) {
	svc, configPath := openTestService(t)
	runOneCycleForHistory(t, svc)

	otherRemote := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherRemote, "backup.dump"), []byte("a different dataset that happens to use the same file name"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		RemotePath: strPtr(otherRemote),
	})
	if !errors.Is(err, ErrRepointNotAcknowledged) {
		t.Fatalf("UpdateBackupSet error = %v, want ErrRepointNotAcknowledged", err)
	}
	if got := readBackupSetFromDisk(t, configPath, "production", "postgres-primary").RemotePath; got == otherRemote {
		t.Error("the remote path was repointed despite the refusal")
	}
}

// TestUpdateBackupSet_ARepointedRemoteRootReadsTheNewDataAsAlreadyBackedUp
// is why the refusal above exists at all, proved rather than asserted:
// with the acknowledgement given, the new dataset's identically-named
// file is NOT fetched, because the journal already knows that artifact
// name. If this ever stops being true the acknowledgement can be
// reconsidered; while it is true, an operator has to be told.
func TestUpdateBackupSet_ARepointedRemoteRootReadsTheNewDataAsAlreadyBackedUp(t *testing.T) {
	svc, _ := openTestService(t)
	runOneCycleForHistory(t, svc)

	before, err := svc.ListArtifacts(context.Background(), ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}

	otherRemote := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherRemote, "backup.dump"), []byte("a genuinely different dataset under the same file name"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		RemotePath:         strPtr(otherRemote),
		AcknowledgeRepoint: true,
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	runOneCycle(t, svc)

	after, err := svc.ListArtifacts(context.Background(), ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("the cycle after the repoint journaled %d artifact(s), was %d: this test's whole premise is that the new dataset is NOT picked up, so if that changed, revisit backupsetrepoint.go", len(after), len(before))
	}
}

func intPtr(i int) *int               { return &i }
func stringsPtr(s []string) *[]string { return &s }
