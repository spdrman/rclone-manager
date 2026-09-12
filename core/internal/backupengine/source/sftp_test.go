package source_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/source"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/transport/rclone"
	"github.com/backupdproject/backupd/core/tests/machines"
)

// An SFTP source, end to end, against a real SSH server in a container:
// real keys, a real host-key check, real pkg/sftp reads, this adapter,
// and a real Kopia repository.
//
// It is the SFTP acceptance criterion, and it is a BackupPaths run rather
// than a Backup one, which is the finding rather than a shortcut.
// bundled/sftp.json declares bounded_listing false - rclone's sftp
// backend reads a directory through pkg/sftp's ReadDir, which returns the
// whole directory as one slice with no resumable cursor - so a walk of an
// SFTP source is refused by the capability gate before anything is
// dialed, and the first half of this test proves that the refusal fires
// against the real backend rather than only against a synthetic profile.
// What SFTP DOES declare is streaming_open, so its objects stream, and
// the second half proves that too.
//
// It skips cleanly where docker is absent and fails loudly inside the
// gate, which is machines.Start's own contract: a skip there would delete
// the machine tier from a run that went on reporting ok.
func TestAnSFTPSourceStreamsThroughTheAdapter(t *testing.T) {
	fixture := machines.Start(t).Source(t)

	payload := patternBytes(4<<20, 0x5F7B)
	small := []byte("a second object, so the run is not one file wide\n")

	seed := map[string][]byte{
		"runs/2026/db.dump":   payload,
		"runs/2026/notes.txt": small,
	}

	for name, body := range seed {
		full := filepath.Join(fixture.UploadDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	adapter := rclone.New()
	src := fixture.TransportSource("sftp-source", "")

	repo, repoRoot := realRepository(t)

	sink := source.RepositorySink{
		Repo:        repo,
		Source:      backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/sftp-source"},
		Description: "sftp integration",
	}

	var (
		storedMu = make(chan struct{}, 1)
		stored   = map[string]string{}
	)

	storedMu <- struct{}{}

	a, err := source.New(source.Deps{
		Streamer:   adapter,
		Stater:     adapter,
		Enumerator: adapter,
	}, source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 2,
		OnResult: func(r source.Result) {
			if !r.Verified() {
				return
			}
			<-storedMu
			stored[r.Path] = r.StoredID
			storedMu <- struct{}{}
		},
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	ctx := fixture.Context()

	// The walk is refused by the matrix, before anything is dialed.
	if _, err := a.Backup(ctx, source.Request{Source: src, Sink: sink}); !errors.Is(err, backend.ErrUnboundedListing) {
		t.Fatalf("walking an SFTP source returned %v; bundled/sftp.json declares bounded_listing false, so it must be refused", err)
	}

	// The objects stream.
	rep, err := a.BackupPaths(ctx, source.Request{Source: src, Sink: sink},
		[]string{"runs/2026/db.dump", "runs/2026/notes.txt"})
	if err != nil {
		t.Fatalf("BackupPaths over SFTP: %v", err)
	}

	if !rep.Complete() || rep.Stored != 2 {
		t.Fatalf("the SFTP run stored %d of 2 objects: %+v", rep.Stored, rep)
	}

	if rep.Backend != "sftp" {
		t.Errorf("the run was judged against the %q matrix, want sftp", rep.Backend)
	}

	if rep.Bytes != int64(len(payload)+len(small)) {
		t.Errorf("streamed %d bytes; the source holds %d", rep.Bytes, len(payload)+len(small))
	}

	for name, body := range seed {
		id := stored[name]
		if id == "" {
			t.Errorf("%s was not stored", name)

			continue
		}

		rc, err := repo.OpenSnapshotStream(context.Background(), backupengine.SnapshotID(id))
		if err != nil {
			t.Errorf("OpenSnapshotStream(%s): %v", name, err)

			continue
		}

		got, err := io.ReadAll(rc)
		_ = rc.Close()

		if err != nil {
			t.Errorf("reading %s back: %v", name, err)

			continue
		}

		if sha256Of(got) != sha256Of(body) {
			t.Errorf("%s came back as %d bytes; the SFTP source holds %d", name, len(got), len(body))
		}
	}

	// Nothing staged the object anywhere outside the repository, which
	// over SFTP is the claim that matters most: the obvious
	// implementation is an sftp GET to a temp file followed by a local
	// read of it.
	assertNothingStaged(t, repoRoot, filepath.Join(repoRoot, "repo"))

	// And the SFTP source still holds everything it did.
	for name, body := range seed {
		got, err := os.ReadFile(filepath.Join(fixture.UploadDir, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("the backup removed %s from the SFTP source: %v", name, err)

			continue
		}
		if sha256Of(got) != sha256Of(body) {
			t.Errorf("the backup modified %s on the SFTP source", name)
		}
	}
}
