package kopia_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
)

// TestVerifyReportsCancellationNotJustFindings is the regression for the
// worst way a verification can lie.
//
// A tree walk that is torn down by a cancelled context records the
// cancellation as a per-object finding AND returns an error. An adapter that
// decides "did verification run" by counting findings therefore reports a
// cancelled verification as a completed one that happened to find damage:
// the operator sees findings, retries, and never learns that nothing was
// read. The findings and the error are independent, and both have to reach
// the caller.
//
// The cancellation is made deterministic rather than raced: one clean verify
// first, so the snapshot manifest is already resolvable from cache and the
// cancelled run gets past loading it and into the walk, which is where the
// masking happened.
func TestVerifyReportsCancellationNotJustFindings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep, _, snap := singleSnapshotRepository(t, 8<<20)

	if _, err := rep.Verify(ctx, snap.ID); err != nil {
		t.Fatalf("warm-up Verify of an undamaged snapshot: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	report, err := rep.Verify(cancelled, snap.ID)
	if err == nil {
		t.Fatalf("Verify returned a nil error for a cancelled verification that read %d of %d bytes "+
			"and recorded %d finding(s); a cancelled walk is not a completed one",
			report.BytesVerified, snap.Bytes, len(report.Errors))
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Verify returned %v; want an error wrapping context.Canceled", err)
	}

	// The partial report still comes back with the error, because what was
	// read before the cancellation is a real, if incomplete, observation.
	if report.BytesVerified >= snap.Bytes {
		t.Errorf("Verify reported %d of %d bytes verified after cancellation; that is not a partial result",
			report.BytesVerified, snap.Bytes)
	}
}

// TestVerifyReportsDamageAsBothErrorAndFindings is the other half of the
// same contract, and the reason the cancellation fix cannot be "return an
// error only when nothing was read".
//
// A repository missing a pack blob is damaged, not unreadable: the walk
// completes, one object fails, and the operator needs the finding (what is
// broken) and a non-nil error (this verification did not pass). Reporting
// damage with a nil error is how a monitoring system learns nothing is
// wrong.
func TestVerifyReportsDamageAsBothErrorAndFindings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep, loc, snap := singleSnapshotRepository(t, 8<<20)

	if _, err := rep.Verify(ctx, snap.ID); err != nil {
		t.Fatalf("Verify before damage: %v", err)
	}

	// Closed first so the damage is not hidden behind an open repository's
	// in-memory content cache.
	if err := rep.Close(ctx); err != nil {
		t.Fatalf("closing before damage: %v", err)
	}

	removeLargestBlob(t, repoDir(t, loc))

	damaged, err := kopia.New().OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("reopening the damaged repository: %v", err)
	}

	t.Cleanup(func() {
		if err := damaged.Close(context.Background()); err != nil {
			t.Errorf("closing the damaged repository: %v", err)
		}
	})

	report, err := damaged.Verify(ctx, snap.ID)
	if err == nil {
		t.Fatal("Verify passed a snapshot whose data blob was deleted")
	}

	if errors.Is(err, context.Canceled) {
		t.Errorf("Verify reported damage as a cancellation: %v", err)
	}

	if len(report.Errors) == 0 {
		t.Errorf("Verify returned %v with no findings; the error says it failed and the report says what failed", err)
	}

	if report.BytesVerified >= snap.Bytes {
		t.Errorf("Verify claims %d of %d bytes read back out of a repository missing the blob holding them",
			report.BytesVerified, snap.Bytes)
	}
}

// TestOpenRepositoryConnectsTheRequestedStorage is the regression for
// writing a snapshot into the wrong repository.
//
// A config file records which storage it is connected to. Reusing an
// existing one because it happens to be there means a caller that asked for
// repository B gets a handle on repository A, and the failure is silent in
// the worst direction: the snapshot succeeds, it is just in the repository
// nobody asked about, and B stays empty while looking configured.
//
// The way to reach that state now that the config path is derived from the
// repository's id is the case an operator actually produces: the backup
// root moved -- a new mount, a migrated NAS, a restored volume -- and the
// repository domain id, which is what names the config file, did not.
// Both locations then resolve to one config file, and the first one's
// contents are sitting there when the second one is opened.
func TestOpenRepositoryConnectsTheRequestedStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()

	// One state directory, deliberately shared, which is what production
	// looks like: /var/lib/backupd holds the connection config for every
	// repository this manager knows about, keyed by domain id.
	stateDir := filepath.Join(t.TempDir(), "state")

	first := localLocation(t, filepath.Join(root, "old-backup-root"), "production")
	first.StateDir = stateDir

	second := first
	second.Root = filepath.Join(root, "new-backup-root")

	eng := kopia.New()

	for _, loc := range []backupengine.RepositoryLocation{first, second} {
		if err := eng.CreateRepository(ctx, loc); err != nil {
			t.Fatalf("CreateRepository under %s: %v", loc.Root, err)
		}
	}

	srcDir := filepath.Join(root, "source")
	writeSourceTree(t, srcDir)

	src := backupengine.Source{Host: "spike-host", User: "spike-user", Path: srcDir}

	// Open and snapshot into A, which is what writes the config file.
	repA, err := eng.OpenRepository(ctx, first)
	if err != nil {
		t.Fatalf("OpenRepository(A): %v", err)
	}

	if err := eng.CreateRepository(ctx, first); !errors.Is(err, backupengine.ErrRepositoryExists) {
		t.Fatalf("CreateRepository over A: got %v, want ErrRepositoryExists", err)
	}

	if _, err := repA.Snapshot(ctx, backupengine.SnapshotRequest{Source: src}); err != nil {
		t.Fatalf("Snapshot into A: %v", err)
	}

	if err := repA.Close(ctx); err != nil {
		t.Fatalf("closing A: %v", err)
	}

	sizeA := dirBytes(t, repoDir(t, first))

	// Now ask for B with A's config file still sitting there.
	repB, err := eng.OpenRepository(ctx, second)
	if err != nil {
		t.Fatalf("OpenRepository(B) with a config file pointing at A: %v", err)
	}

	t.Cleanup(func() {
		if err := repB.Close(context.Background()); err != nil {
			t.Errorf("closing B: %v", err)
		}
	})

	if snaps, err := repB.ListSnapshots(ctx, src); err != nil || len(snaps) != 0 {
		t.Fatalf("the handle for B lists %d snapshot(s) (err %v); it is connected to A, which holds 1",
			len(snaps), err)
	}

	if _, err := repB.Snapshot(ctx, backupengine.SnapshotRequest{Source: src}); err != nil {
		t.Fatalf("Snapshot into B: %v", err)
	}

	if after := dirBytes(t, repoDir(t, first)); after != sizeA {
		t.Errorf("repository A grew from %d to %d bytes while the caller was writing to B", sizeA, after)
	}

	snaps, err := repB.ListSnapshots(ctx, src)
	if err != nil {
		t.Fatalf("ListSnapshots(B): %v", err)
	}

	if len(snaps) != 1 {
		t.Errorf("B holds %d snapshot(s) after one snapshot was written to it, want 1", len(snaps))
	}
}

// singleSnapshotRepository opens a repository holding exactly one snapshot
// of a tree containing one file of `size` bytes of incompressible data, and
// returns the open handle, the location it was opened from, and the
// snapshot.
func singleSnapshotRepository(
	t *testing.T,
	size int64,
) (backupengine.Repository, backupengine.RepositoryLocation, backupengine.SnapshotInfo) {
	t.Helper()

	ctx := context.Background()
	root := t.TempDir()
	srcDir := filepath.Join(root, "source")

	if err := os.MkdirAll(srcDir, 0o750); err != nil {
		t.Fatalf("creating source dir: %v", err)
	}

	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generating payload: %v", err)
	}

	mustWrite(t, filepath.Join(srcDir, "payload.bin"), payload)

	loc := localLocation(t, root, "production")

	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
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

	snap, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{Source: src})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	return rep, loc, snap
}

// removeLargestBlob deletes the biggest file under a repository directory,
// which for a repository holding one large incompressible file is the pack
// blob its content lives in. It is the cheapest available way to produce a
// repository that is damaged rather than merely absent.
func removeLargestBlob(t *testing.T, repoDir string) {
	t.Helper()

	var (
		biggest string
		size    int64
	)

	err := filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, err error) error {
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

		if info.Mode().IsRegular() && info.Size() > size {
			biggest, size = path, info.Size()
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", repoDir, err)
	}

	if biggest == "" {
		t.Fatalf("no blobs found under %s", repoDir)
	}

	if err := os.Remove(biggest); err != nil {
		t.Fatalf("removing %s: %v", biggest, err)
	}
}
