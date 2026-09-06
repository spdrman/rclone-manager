// This file is the create path's half of the repoint acknowledgement
// (issue #411); backupsetrepoint_test.go is the update path's half, and
// backupsetrepoint.go is the reasoning both of them are about.
//
// Creating over an id that already has history is the case nobody
// designs for and operators reach anyway: a set removed and added back, a
// configuration rebuilt by hand, an id reused after a NAS was replaced.
// The journal is keyed by the id rather than by the row, so the artifacts
// are still there and still claimed by whatever is created next.
//
// Both halves have to refuse identically, which is why the cases here
// deliberately mirror the update file's rather than exploring their own
// shapes. A guard that fires on edit and not on create is not a weaker
// guard, it is an invitation to remove and re-add instead of editing.
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
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// createdSetWithHistory is this file's fixture: one backup set created
// through CreateBackupSet (so its address is an sftp one a create request
// can actually restate), one artifact on record for it, and then the
// removal that frees the id up again.
//
// It returns the request that created it, so a test can re-send exactly
// that request (the undo) or change one field of it (the repoint) and
// have the difference be the only thing under test.
func createdSetWithHistory(t *testing.T, svc *BackupService) CreateBackupSetRequest {
	t.Helper()
	ctx := context.Background()

	req := validCreateReq(t, svc, "alpha")
	req.SourceName = "production"
	if err := os.MkdirAll(req.LocalPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", req.LocalPath, err)
	}
	if _, err := svc.CreateBackupSet(ctx, req); err != nil {
		t.Fatalf("CreateBackupSet (fixture): %v", err)
	}

	setID, err := model.NewBackupSetID("production", "alpha")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	seedCompleteArtifact(t, ctx, svc.journal, config.BackupSet{ID: setID, LocalPath: req.LocalPath},
		"alpha.dump", time.Now().UTC().Add(-24*time.Hour), "one artifact on record")

	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet (fixture): %v", err)
	}
	if got := svc.artifactCountFor(ctx, "production/alpha"); got != 1 {
		t.Fatalf("the fixture leaves %d artifacts on record for production/alpha, want 1: every test below turns on there being history", got)
	}
	return req
}

// TestCreateBackupSet_RefusesToCreateOverHistoryAtADifferentAddressWithoutAnAcknowledgement
// is this issue's central one. Removing a backup set frees its id up, and
// creating it again pointing somewhere else lands in exactly the state the
// update path refuses: the new set takes every artifact the old one left
// on record, discovery reads a colliding name at the new address as
// already backed up and never fetches it, and retention refuses to prune
// a single artifact under the old root for as long as the set exists.
//
// The file and the known_hosts directory are both checked afterwards,
// because a refusal that had already written half the creation would be
// worse than no refusal.
func TestCreateBackupSet_RefusesToCreateOverHistoryAtADifferentAddressWithoutAnAcknowledgement(t *testing.T) {
	svc, configPath := openTestService(t)
	req := createdSetWithHistory(t, svc)

	before := mustRead(t, configPath)
	elsewhere := filepath.Join(t.TempDir(), "moved")
	recordedRemote := req.RemotePath
	req.RemotePath = "/backups/somewhere-else"
	req.LocalPath = elsewhere

	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrHistoryRepointNotAcknowledged) {
		t.Fatalf("CreateBackupSet error = %v, want ErrHistoryRepointNotAcknowledged", err)
	}
	// Actionable or it is just an "are you sure": which fields moved,
	// what is on record, how many artifacts are on record, and the exact
	// word that lets the caller proceed.
	for _, want := range []string{"remote_path", "local_path", recordedRemote, elsewhere, "acknowledge_repoint", "1 artifact"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if after := mustRead(t, configPath); after != before {
		t.Error("the configuration file changed even though the creation was refused")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(configPath), "known_hosts", "production_alpha_known_hosts")); statErr == nil {
		t.Error("a refused creation still wrote this set's known_hosts file; nothing must be persisted before the refusal")
	}
}

// TestCreateBackupSet_MovingTheHostAloneIsRefusedToo: remote.host is one
// of the three, on this path for the same reason it is on the update
// path. A second NAS answering on a new address with the same directory
// layout is the exact shape that comes back AlreadyKnown and is never
// fetched.
func TestCreateBackupSet_MovingTheHostAloneIsRefusedToo(t *testing.T) {
	svc, _ := openTestService(t)
	req := createdSetWithHistory(t, svc)

	recordedHost := req.Host
	req.Host = "replacement.internal"

	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrHistoryRepointNotAcknowledged) {
		t.Fatalf("CreateBackupSet error = %v, want ErrHistoryRepointNotAcknowledged", err)
	}
	for _, want := range []string{"remote.host", recordedHost, "replacement.internal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestCreateBackupSet_OverHistoryAtTheSameAddressNeedsNoCeremony is the
// control that keeps the refusal from being a blanket one, and it is the
// case the whole design is protecting: undoing a removal. Re-sending the
// request that created the set in the first place must cost nothing, or
// getting back where you already were means re-fetching a volume full of
// backups.
func TestCreateBackupSet_OverHistoryAtTheSameAddressNeedsNoCeremony(t *testing.T) {
	svc, _ := openTestService(t)
	req := createdSetWithHistory(t, svc)

	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("re-creating the set at exactly the address it was removed from: %v", err)
	}
	if got := svc.artifactCountFor(context.Background(), "production/alpha"); got != 1 {
		t.Errorf("the re-created set holds %d artifacts, want the 1 the removed one left behind", got)
	}
}

// TestCreateBackupSet_OverHistoryAtADifferentAddressOnceAcknowledged: the
// refusal is an acknowledgement, not a prohibition. An operator whose NAS
// really did get a new address has a legitimate change to make and this
// is the only route to it.
func TestCreateBackupSet_OverHistoryAtADifferentAddressOnceAcknowledged(t *testing.T) {
	svc, _ := openTestService(t)
	req := createdSetWithHistory(t, svc)

	req.LocalPath = filepath.Join(t.TempDir(), "moved")
	req.AcknowledgeRepoint = true

	created, err := svc.CreateBackupSet(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateBackupSet with an acknowledgement: %v", err)
	}
	if created.Set.LocalPath != req.LocalPath {
		t.Errorf("LocalPath = %q, want %q", created.Set.LocalPath, req.LocalPath)
	}
}

// TestCreateBackupSet_OverAnIDWithNoHistoryNeedsNoCeremony is the other
// control. A set nothing has ever been journaled for is a set nothing can
// be orphaned from, and the ordinary create must not be made to feel
// dangerous.
func TestCreateBackupSet_OverAnIDWithNoHistoryNeedsNoCeremony(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	req := validCreateReq(t, svc, "alpha")
	req.SourceName = "production"
	if _, err := svc.CreateBackupSet(ctx, req); err != nil {
		t.Fatalf("CreateBackupSet (fixture): %v", err)
	}
	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	req = validCreateReq(t, svc, "alpha")
	req.SourceName = "production"
	req.RemotePath = "/backups/somewhere-else"
	if _, err := svc.CreateBackupSet(ctx, req); err != nil {
		t.Fatalf("creating over an id that never journaled anything: %v", err)
	}
}

// TestCreateBackupSet_AnUnrelatedFieldOverHistoryIsNotRefused: the
// acknowledgement covers the three fields that decide which data the set
// is about. Asking for it because a staleness budget or an include
// pattern differs would make it something an operator clicks through by
// habit, which protects nothing.
func TestCreateBackupSet_AnUnrelatedFieldOverHistoryIsNotRefused(t *testing.T) {
	svc, _ := openTestService(t)
	req := createdSetWithHistory(t, svc)

	req.StaleAfter = 48 * time.Hour
	req.Include = []string{"*.dump", "*.tar"}
	req.Port = 2222
	req.User = "someone-else"

	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("re-creating the set with unrelated fields changed: %v", err)
	}
}

// TestCreateBackupSet_WithoutARecordedAddressStillGuardsTheLocalPath is
// the fallback, and the reason it exists is worth stating: the address a
// set was last configured with is recorded when its configuration is
// removed, so an id whose configuration vanished some other way (a
// journal carried onto a rebuilt config.yaml, a set removed by an older
// build) has no such record. What the journal still holds for it is where
// its artifacts actually landed, and that is exactly the half retention
// refuses on, so it is checked even with nothing else to check against.
func TestCreateBackupSet_WithoutARecordedAddressStillGuardsTheLocalPath(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	landed := filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(landed, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	setID, err := model.NewBackupSetID("api", "ghost")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	seedCompleteArtifact(t, ctx, svc.journal, config.BackupSet{ID: setID, LocalPath: landed},
		"ghost.dump", time.Now().UTC().Add(-24*time.Hour), "landed here, and nowhere else")

	req := validCreateReq(t, svc, "ghost")
	req.LocalPath = filepath.Join(t.TempDir(), "elsewhere")
	_, err = svc.CreateBackupSet(ctx, req)
	if !errors.Is(err, ErrHistoryRepointNotAcknowledged) {
		t.Fatalf("CreateBackupSet error = %v, want ErrHistoryRepointNotAcknowledged", err)
	}
	if !strings.Contains(err.Error(), landed) {
		t.Errorf("the refusal does not name where the artifacts on record actually landed (%q): %v", landed, err)
	}

	// The control: the same create pointed at where those artifacts
	// actually are is the undo, and costs nothing. Written with a
	// trailing slash on purpose: config.Validate accepts one and never
	// cleans it, retention computes the same file path either way, so an
	// artifact under it is not stranded and must not be asked about.
	req = validCreateReq(t, svc, "ghost")
	req.LocalPath = landed + "/"
	if _, err := svc.CreateBackupSet(ctx, req); err != nil {
		t.Fatalf("creating over that history at the path its artifacts actually landed under: %v", err)
	}
}

// TestRemoveBackupSet_RecordsTheAddressItRemoved is the write half of the
// guard above, checked on its own so a create-path test cannot pass
// because both halves are broken in the same direction.
func TestRemoveBackupSet_RecordsTheAddressItRemoved(t *testing.T) {
	svc, _, localA, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if before := svc.state.Load().inner.Config.Sources[0].BackupSets[0].LocalPath; before != localA {
		t.Fatalf("fixture: first set's local_path = %q, want %q", before, localA)
	}

	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	setID, err := model.NewBackupSetID("production", "alpha")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	addr, found, err := svc.journal.BackupSetAddress(ctx, setID)
	if err != nil {
		t.Fatalf("BackupSetAddress: %v", err)
	}
	if !found {
		t.Fatal("removing a backup set recorded nothing about the address it was pointing at, so a later create over the same id has nothing to check against")
	}
	if addr.LocalPath != localA {
		t.Errorf("recorded local_path = %q, want %q", addr.LocalPath, localA)
	}
}
