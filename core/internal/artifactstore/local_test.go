package artifactstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

func testSetID(t *testing.T) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID("src", "set")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

func testArtifact(t *testing.T, name string) model.ArtifactID {
	t.Helper()
	a, err := model.NewArtifactID(testSetID(t), name)
	if err != nil {
		t.Fatalf("NewArtifactID(%q): %v", name, err)
	}
	return a
}

// TestLocalLocatorAgreesWithLifecyclesOwnFormula is the anti-drift check
// this seam exists to make possible.
//
// Where a committed artifact sits used to be computed in two places, each
// carrying a comment naming the other as the only other place allowed to
// compute it. Consolidating them into a store is only an improvement if
// the store's answer is byte-identical to what the pipeline already
// produces, because anything else silently relocates every artifact on
// every existing deployment. This pins the two together rather than
// asserting they agree.
func TestLocalLocatorAgreesWithLifecyclesOwnFormula(t *testing.T) {
	dirs := []string{"/data/backups", "/mnt/tank/backup-manager/backups", "relative/dir"}
	names := []string{"backup.dump", "backup.dump.zst", "a b.tar", "2026-09-01T00-00-00Z.sql"}

	for _, dir := range dirs {
		for _, name := range names {
			artifact := testArtifact(t, name)
			want := lifecycle.FinalArtifactPath(dir, artifact)
			got := artifactstore.LocalLocator(dir, artifact)
			if got != want {
				t.Errorf("LocalLocator(%q, %q) = %q, lifecycle computes %q", dir, name, got, want)
			}
		}
	}
}

// TestLocalLocatorMatchesTheConfiguredBackupSetRoot proves the Store
// method resolves through the backup set the same way the free function
// does, so a caller holding a Store and a caller holding a path cannot
// disagree.
func TestLocalLocatorMatchesTheConfiguredBackupSetRoot(t *testing.T) {
	bs := config.BackupSet{ID: testSetID(t), LocalPath: "/data/backups"}
	artifact := testArtifact(t, "backup.dump")

	got, err := artifactstore.Local{}.Locator(bs, artifact)
	if err != nil {
		t.Fatalf("Locator: %v", err)
	}
	if want := artifactstore.LocalLocator(bs.LocalPath, artifact); got != want {
		t.Errorf("Locator = %q, want %q", got, want)
	}
}

// TestLocalLocatorRefusesABackupSetWithNoLocalPath keeps a missing root
// from silently resolving to a bare basename in the process's working
// directory, which is the shape of mistake that writes an artifact
// somewhere nobody is backing up.
func TestLocalLocatorRefusesABackupSetWithNoLocalPath(t *testing.T) {
	_, err := artifactstore.Local{}.Locator(config.BackupSet{ID: testSetID(t)}, testArtifact(t, "backup.dump"))
	if err == nil {
		t.Fatal("expected a refusal for a backup set with no local_path, got nil")
	}
}

func TestLocalStatReportsSizeAndAbsence(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.dump")
	if err := os.WriteFile(present, []byte("twelve bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := artifactstore.Local{}.Stat(context.Background(), present)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size == nil || *st.Size != 12 {
		t.Errorf("Size = %v, want 12", st.Size)
	}
	if st.ModTime == nil {
		t.Error("ModTime is nil, want the file's modification time")
	}

	_, err = artifactstore.Local{}.Stat(context.Background(), filepath.Join(dir, "absent.dump"))
	if !errors.Is(err, artifactstore.ErrNotPresent) {
		t.Errorf("Stat on an absent file = %v, want ErrNotPresent", err)
	}
}

// TestLocalStatRefusesASymlink covers the same anomaly the prune path
// treats as disqualifying: Commit never produces a symlink at a final
// name, so one found there describes a file this store did not place.
func TestLocalStatRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.dump")
	if err := os.WriteFile(target, []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.dump")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	if _, err := (artifactstore.Local{}).Stat(context.Background(), link); err == nil {
		t.Fatal("expected a refusal for a symlink at an artifact's final path, got nil")
	}
}

func TestLocalOpenReadsTheBytesAndReportsAbsence(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.dump")
	if err := os.WriteFile(p, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	rc, err := artifactstore.Local{}.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("read %q, want %q", got, "payload")
	}

	if _, err := (artifactstore.Local{}).Open(context.Background(), filepath.Join(dir, "absent")); !errors.Is(err, artifactstore.ErrNotPresent) {
		t.Errorf("Open on an absent file = %v, want ErrNotPresent", err)
	}
}

// TestLocalPutIsAtomicallyNamed proves a reader never sees a partially
// written artifact under its final name, which is the property a mover
// would depend on when it confirms a destination copy before removing the
// origin.
func TestLocalPutIsAtomicallyNamed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "put.dump")

	if err := (artifactstore.Local{}).Put(context.Background(), p, bytes.NewReader([]byte("written"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "written" {
		t.Errorf("content = %q, want %q", got, "written")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("Put left %d entries behind (%v), want only the artifact", len(entries), names)
	}
}

// TestLocalRemoveIsIdempotent: the caller's intent is that these bytes
// not be in this store, and that is already true of a file that is gone.
func TestLocalRemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gone.dump")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (artifactstore.Local{}).Remove(context.Background(), p); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := (artifactstore.Local{}).Remove(context.Background(), p); err != nil {
		t.Fatalf("second Remove on an absent file: %v, want nil", err)
	}
}

// TestStoreHasNoMoveMethod is a design assertion, not a behavioural one.
//
// The seam deliberately offers Put, Stat, Open and Remove and no Move, so
// that the put-then-confirm-then-remove ordering a mover needs lives in
// one auditable place rather than being re-decided inside every adapter.
// Adding Move to the interface would make the dangerous ordering
// (remove before the destination copy is confirmed) expressible as a
// single call, which is the one shape that can leave zero copies of a
// backup. If this test is failing because someone added Move, read the
// package doc before deleting the test.
func TestStoreHasNoMoveMethod(t *testing.T) {
	var s artifactstore.Store = artifactstore.Local{}
	if _, hasMove := any(s).(interface {
		Move(context.Context, string, string) error
	}); hasMove {
		t.Error("Store gained a Move method; see this package's doc for why the seam offers put/confirm/remove separately")
	}
}
