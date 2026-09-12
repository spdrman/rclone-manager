// Package kopia_test holds the Phase 0 feasibility spike for issue #790.
//
// This is a compatibility smoke test, not product code and not a unit test.
// Its job is to answer one question that cannot be answered by reading
// upstream documentation: can this repository drive a complete backup
// lifecycle, in process, through the embedded engine's Go packages, with no
// subprocess of the vendor's CLI anywhere? If a future version bump breaks
// that answer, this test is where it shows up, which is why it survives the
// spike instead of being deleted with it.
//
// It deliberately exercises the whole sequence in one test function rather
// than splitting it into independent cases. The sequence is the claim: a
// snapshot that cannot be verified, restored, deleted and then garbage
// collected is not a backup, and the states only exist in order.
package kopia_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
	"github.com/backupdproject/backupd/core/internal/secretref"
)

// bigFileSize is large enough that copying it twice is unmistakable in the
// repository's on-disk size, and small enough to stay a fast test. It is
// filled with random bytes so compression cannot make the reuse measurement
// ambiguous.
const bigFileSize = 8 << 20

const testPassphrase = "spike-passphrase-not-a-secret"

func TestInProcessLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	root := t.TempDir()
	srcDir := filepath.Join(root, "source")
	restoreDir := filepath.Join(root, "restored")

	writeSourceTree(t, srcDir)

	loc := localLocation(t, root, "production")
	blobDir := repoDir(t, loc)

	eng := kopia.New()

	// --- create repository -------------------------------------------------

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	// Creating over an existing repository would orphan every snapshot in
	// it, so the refusal is part of the contract, not a convenience.
	if err := eng.CreateRepository(ctx, loc); !errors.Is(err, backupengine.ErrRepositoryExists) {
		t.Fatalf("CreateRepository on existing repository: got %v, want ErrRepositoryExists", err)
	}

	// --- open repository ---------------------------------------------------

	wrongPass := loc
	wrongPass.Passphrase = secretref.Ref{File: filepath.Join(t.TempDir(), "wrong")}
	mustWrite(t, wrongPass.Passphrase.File, []byte("definitely-not-the-passphrase"))
	wrongPass.StateDir = filepath.Join(t.TempDir(), "wrong-state")

	if _, err := eng.OpenRepository(ctx, wrongPass); !errors.Is(err, backupengine.ErrPassphrase) {
		t.Fatalf("OpenRepository with wrong passphrase: got %v, want ErrPassphrase", err)
	}

	missing := loc
	missing.Root = filepath.Join(t.TempDir(), "no-such-backup-root")
	missing.StateDir = filepath.Join(t.TempDir(), "missing-state")

	if _, err := eng.OpenRepository(ctx, missing); !errors.Is(err, backupengine.ErrRepositoryNotFound) {
		t.Fatalf("OpenRepository on empty location: got %v, want ErrRepositoryNotFound", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	src := backupengine.Source{Host: "spike-host", User: "spike-user", Path: srcDir}

	if snaps, err := rep.ListSnapshots(ctx, src); err != nil || len(snaps) != 0 {
		t.Fatalf("ListSnapshots on fresh repository: got %d snapshots, err %v; want 0, nil", len(snaps), err)
	}

	// --- first snapshot ----------------------------------------------------

	wantFiles, wantBytes := treeTotals(t, srcDir)

	first, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source:      src,
		Description: "spike first",
		Tags:        map[string]string{"spike": "790"},
	})
	if err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}

	if first.ID == "" {
		t.Fatal("first Snapshot returned empty SnapshotID")
	}

	if first.Incomplete != "" {
		t.Fatalf("first Snapshot incomplete: %q", first.Incomplete)
	}

	if first.Files != wantFiles || first.Bytes != wantBytes {
		t.Errorf("first Snapshot counted %d files / %d bytes, source holds %d / %d",
			first.Files, first.Bytes, wantFiles, wantBytes)
	}

	if first.ReusedFiles != 0 {
		t.Errorf("first Snapshot reused %d files, nothing existed to reuse", first.ReusedFiles)
	}

	if first.NewFiles != wantFiles {
		t.Errorf("first Snapshot hashed %d files, want all %d", first.NewFiles, wantFiles)
	}

	if first.End.Before(first.Start) {
		t.Errorf("first Snapshot ended (%v) before it started (%v)", first.End, first.Start)
	}

	firstGrowth := dirBytes(t, blobDir)

	if firstGrowth < bigFileSize {
		t.Fatalf("repository grew only %d bytes storing a %d byte incompressible file; "+
			"the reuse measurement below would be meaningless", firstGrowth, bigFileSize)
	}

	// --- second, incremental snapshot --------------------------------------

	// Change a small subset only: one existing small file rewritten, one new
	// small file added. The 8MiB file is untouched, so a correct incremental
	// snapshot must not store its bytes a second time.
	mustWrite(t, filepath.Join(srcDir, "nested", "small-b.txt"), []byte("small-b, revised\n"))
	mustWrite(t, filepath.Join(srcDir, "nested", "small-c.txt"), []byte("small-c, added in the second snapshot\n"))

	wantFiles2, wantBytes2 := treeTotals(t, srcDir)

	second, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source:      src,
		Description: "spike second",
	})
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}

	if second.ID == first.ID {
		t.Fatal("second Snapshot reused the first snapshot's ID; these are two distinct snapshots")
	}

	if second.Files != wantFiles2 || second.Bytes != wantBytes2 {
		t.Errorf("second Snapshot counted %d files / %d bytes, source holds %d / %d",
			second.Files, second.Bytes, wantFiles2, wantBytes2)
	}

	// The incrementality claim, from the engine's own accounting: everything
	// except the rewritten file and the added file came back unread.
	if wantUnchanged := wantFiles2 - 2; second.ReusedFiles != wantUnchanged {
		t.Errorf("second Snapshot reused %d files, want %d (all but the one rewritten and the one added)",
			second.ReusedFiles, wantUnchanged)
	}

	if second.NewFiles != 2 {
		t.Errorf("second Snapshot hashed %d files, want 2", second.NewFiles)
	}

	// And the same claim from the filesystem, which cannot be talked into
	// agreeing: a second physical copy of the 8MiB file would show up here.
	secondGrowth := dirBytes(t, blobDir) - firstGrowth

	if secondGrowth >= firstGrowth/4 {
		t.Errorf("repository grew %d bytes for the second snapshot after growing %d for the first; "+
			"that is not incremental storage", secondGrowth, firstGrowth)
	}

	t.Logf("physical growth: first snapshot %d bytes, second snapshot %d bytes (%.4f x)",
		firstGrowth, secondGrowth, float64(secondGrowth)/float64(firstGrowth))

	// --- list --------------------------------------------------------------

	snaps, err := rep.ListSnapshots(ctx, src)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 2 {
		t.Fatalf("ListSnapshots returned %d snapshots, want 2", len(snaps))
	}

	if snaps[0].ID != first.ID || snaps[1].ID != second.ID {
		t.Errorf("ListSnapshots returned %v then %v, want oldest first: %v then %v",
			snaps[0].ID, snaps[1].ID, first.ID, second.ID)
	}

	if snaps[0].Description != "spike first" {
		t.Errorf("first snapshot description round-tripped as %q", snaps[0].Description)
	}

	// --- verify ------------------------------------------------------------

	report, err := rep.Verify(ctx, second.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(report.Errors) != 0 {
		t.Fatalf("Verify found %d problems in a repository nothing has damaged: %v",
			len(report.Errors), report.Errors)
	}

	if report.FilesVerified != wantFiles2 {
		t.Errorf("Verify read %d files, snapshot holds %d", report.FilesVerified, wantFiles2)
	}

	if report.BytesVerified != wantBytes2 {
		t.Errorf("Verify read %d bytes, snapshot holds %d", report.BytesVerified, wantBytes2)
	}

	if _, err := rep.Verify(ctx, backupengine.SnapshotID("nonexistent")); !errors.Is(err, backupengine.ErrSnapshotNotFound) {
		t.Errorf("Verify of unknown snapshot: got %v, want ErrSnapshotNotFound", err)
	}

	// --- restore -----------------------------------------------------------

	restored, err := rep.Restore(ctx, second.ID, backupengine.RestoreRequest{
		TargetPath: restoreDir,
		Overwrite:  true,
		SkipOwners: true,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if restored.Files != wantFiles2 || restored.Bytes != wantBytes2 {
		t.Errorf("Restore wrote %d files / %d bytes, snapshot holds %d / %d",
			restored.Files, restored.Bytes, wantFiles2, wantBytes2)
	}

	// The only restore assertion that matters: the bytes came back.
	wantHashes := hashTree(t, srcDir)
	gotHashes := hashTree(t, restoreDir)

	for path, want := range wantHashes {
		got, ok := gotHashes[path]
		if !ok {
			t.Errorf("restore is missing %s", path)
			continue
		}

		if got != want {
			t.Errorf("restored %s hashes %s, source hashes %s", path, got, want)
		}
	}

	for path := range gotHashes {
		if _, ok := wantHashes[path]; !ok {
			t.Errorf("restore invented %s, which is not in the source", path)
		}
	}

	// --- delete snapshot ---------------------------------------------------

	if err := rep.DeleteSnapshot(ctx, first.ID); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	if err := rep.DeleteSnapshot(ctx, first.ID); !errors.Is(err, backupengine.ErrSnapshotNotFound) {
		t.Errorf("DeleteSnapshot on an already-deleted snapshot: got %v, want ErrSnapshotNotFound", err)
	}

	snaps, err = rep.ListSnapshots(ctx, src)
	if err != nil {
		t.Fatalf("ListSnapshots after delete: %v", err)
	}

	if len(snaps) != 1 || snaps[0].ID != second.ID {
		t.Fatalf("after deleting the first snapshot, ListSnapshots returned %d snapshots (%v); want just %v",
			len(snaps), idsOf(snaps), second.ID)
	}

	// --- maintenance -------------------------------------------------------

	maint, err := rep.Maintain(ctx, backupengine.MaintenanceFull)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	if !maint.Ran {
		t.Error("Maintain reported it did nothing on a repository with a just-deleted snapshot")
	}

	if maint.Mode != backupengine.MaintenanceFull {
		t.Errorf("Maintain reported mode %q, want %q", maint.Mode, backupengine.MaintenanceFull)
	}

	// The dangerous half of maintenance is that it deletes content. Prove it
	// did not delete content the surviving snapshot still needs, which is the
	// failure this whole product exists to not have.
	after, err := rep.Verify(ctx, second.ID)
	if err != nil {
		t.Fatalf("Verify after maintenance: %v", err)
	}

	if len(after.Errors) != 0 {
		t.Fatalf("maintenance damaged the surviving snapshot: %v", after.Errors)
	}

	if after.BytesVerified != wantBytes2 {
		t.Errorf("after maintenance the surviving snapshot reads back %d bytes, want %d",
			after.BytesVerified, wantBytes2)
	}
}

// TestNoSubprocess is the other half of the spike's claim.
//
// "In process" is not observable from the outside of a passing lifecycle
// test: a wrapper that shelled out to an installed CLI could pass every
// assertion above. What makes the claim checkable is that this package
// contains no way to start a process at all, so the check is a read of the
// package's own sources.
func TestNoSubprocess(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	var checked int

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}

		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}

		checked++

		for _, forbidden := range subprocessNeedles() {
			if bytes.Contains(src, []byte(forbidden)) {
				t.Errorf("%s references %s; the adapter must drive the engine in process, not as a subprocess",
					e.Name(), forbidden)
			}
		}
	}

	if checked == 0 {
		t.Fatal("found no .go files to check; this guard is looking in the wrong place")
	}
}

// subprocessNeedles assembles what TestNoSubprocess searches for.
//
// The fragments are concatenated at run time so this file scans itself
// without matching itself. The alternative, exempting *_test.go from the
// scan, would leave the obvious cheat available: a test helper that shells
// out to a CLI and feeds its output to the adapter.
func subprocessNeedles() []string {
	const (
		ex   = "ex"
		ec   = "ec"
		call = "Command"
	)

	return []string{
		`"os/` + ex + ec + `"`,
		ex + ec + "." + call,
		ex + ec + "." + call + "Context",
		"syscall." + ex + ec,
		"Start" + "Process",
	}
}

// writeSourceTree lays down the tree the lifecycle snapshots.
func writeSourceTree(t *testing.T, dir string) {
	t.Helper()

	big := make([]byte, bigFileSize)
	if _, err := rand.Read(big); err != nil {
		t.Fatalf("generating incompressible test data: %v", err)
	}

	mustWrite(t, filepath.Join(dir, "big.bin"), big)
	mustWrite(t, filepath.Join(dir, "small-a.txt"), []byte("small-a, first revision\n"))
	mustWrite(t, filepath.Join(dir, "nested", "small-b.txt"), []byte("small-b, first revision\n"))
	mustWrite(t, filepath.Join(dir, "nested", "deeper", "small-d.txt"), []byte("small-d\n"))
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// treeTotals counts regular files and their total size under dir.
func treeTotals(t *testing.T, dir string) (files, size int64) {
	t.Helper()

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		files++
		size += info.Size()

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}

	return files, size
}

// dirBytes is the physical size of everything under dir, which is how the
// test observes deduplication without asking the engine to self-report.
func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()

	var total int64

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		total += info.Size()

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}

	return total
}

// hashTree maps every regular file's slash-separated path relative to dir to
// the hex SHA-256 of its contents.
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}

		out[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))

		return nil
	})
	if err != nil {
		t.Fatalf("hashing %s: %v", dir, err)
	}

	return out
}

func idsOf(snaps []backupengine.SnapshotInfo) []backupengine.SnapshotID {
	out := make([]backupengine.SnapshotID, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, s.ID)
	}

	return out
}
