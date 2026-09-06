package reconcile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Every fixture this package's tests run on is built here, and sharing them
// is not just about repetition. Reconciliation's inputs are expensive to
// state honestly: an artifact at REMOTE_DELETE_PENDING is not a struct
// literal, it is a journal row that arrived there through every transition
// the state machine required, and driveTo walks it rather than writing the
// destination state in directly.
//
// That distinction is load-bearing. A record assembled by hand can hold a
// combination the machine would have refused, and a test built on one is
// proving something about a journal this project cannot actually produce,
// while looking exactly like a test that is not.

// --- journal and identity fixtures ---

func openTestJournal(t *testing.T) *state.Journal {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	j, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func testSet(t *testing.T) model.BackupSetID {
	t.Helper()
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return set
}

func testArtifact(t *testing.T, name string) model.ArtifactID {
	t.Helper()
	id, err := model.NewArtifactID(testSet(t), name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return id
}

var testSource = transport.Source{ID: "primary", Type: "sftp", Host: "nas.example", Root: "/backups"}

// writeLocalFile writes size deterministic bytes to a fresh temp file and
// returns its path.
func writeLocalFile(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.final")
	data := bytes.Repeat([]byte{0xAB}, int(size))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// --- fixture driver ---
//
// driveTo walks artifact through the real FR-11 sequence, one legal
// RecordTransition at a time, up to (and including) stopAt, so every
// fixture this test file builds is a journal row that could really have
// been produced by the pipeline. It mirrors
// internal/lifecycle/remotedelete_test.go's discoverAndAdvance, extended to
// reach COMPLETE, QUARANTINED and QUARANTINED_LOST too, since reconciliation
// needs fixtures the delete step never had to build.

type driveParams struct {
	artifact  model.ArtifactID
	remote    state.RemoteIdentity
	localPath string
	transfer  *state.TransferResult
	stopAt    lifecycle.State
}

func driveTo(t *testing.T, j *state.Journal, p driveParams) state.Record {
	t.Helper()
	ctx := context.Background()
	seq := 0
	nextKey := func() string {
		seq++
		return fmt.Sprintf("%s#%d", p.artifact, seq)
	}
	now := func() time.Time { return time.Now().UTC() }

	remotePath := "backups/" + p.artifact.Name

	out, err := j.Discover(ctx, p.artifact, nextKey(), remotePath, p.remote, now())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if p.stopAt == lifecycle.Discovered {
		return out.Record
	}

	partial := p.localPath + ".partial"
	out, err = j.RecordTransition(ctx, state.Transition{
		Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Discovered), To: string(lifecycle.Transferring),
		OccurredAt: now(), LocalPath: &partial,
	})
	if err != nil {
		t.Fatalf("-> TRANSFERRING: %v", err)
	}
	if p.stopAt == lifecycle.Transferring {
		return out.Record
	}

	out, err = j.RecordTransition(ctx, state.Transition{
		Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Transferring), To: string(lifecycle.Transferred),
		OccurredAt: now(), Transfer: p.transfer,
	})
	if err != nil {
		t.Fatalf("-> TRANSFERRED: %v", err)
	}
	if p.stopAt == lifecycle.Transferred {
		return out.Record
	}

	out, err = j.RecordTransition(ctx, state.Transition{
		Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Transferred), To: string(lifecycle.Verifying),
		OccurredAt: now(),
	})
	if err != nil {
		t.Fatalf("-> VERIFYING: %v", err)
	}
	if p.stopAt == lifecycle.Verifying {
		return out.Record
	}

	out, err = j.RecordTransition(ctx, state.Transition{
		Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Verifying), To: string(lifecycle.Verified),
		OccurredAt: now(),
	})
	if err != nil {
		t.Fatalf("-> VERIFIED: %v", err)
	}
	if p.stopAt == lifecycle.Verified {
		return out.Record
	}

	out, err = j.RecordTransition(ctx, state.Transition{
		Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Verified), To: string(lifecycle.Committing),
		OccurredAt: now(),
	})
	if err != nil {
		t.Fatalf("-> COMMITTING: %v", err)
	}
	if p.stopAt == lifecycle.Committing {
		return out.Record
	}

	localPath := p.localPath
	out, err = j.RecordTransition(ctx, state.Transition{
		Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Committing), To: string(lifecycle.Committed),
		OccurredAt: now(), LocalPath: &localPath,
	})
	if err != nil {
		t.Fatalf("-> COMMITTED: %v", err)
	}
	if p.stopAt == lifecycle.Committed {
		return out.Record
	}

	if p.stopAt == lifecycle.Quarantined {
		out, err = j.RecordTransition(ctx, state.Transition{
			Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Committed), To: string(lifecycle.Quarantined),
			OccurredAt: now(),
		})
		if err != nil {
			t.Fatalf("-> QUARANTINED: %v", err)
		}
		return out.Record
	}

	if p.stopAt == lifecycle.RemoteRetained {
		out, err = j.RecordTransition(ctx, state.Transition{
			Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Committed), To: string(lifecycle.RemoteRetained),
			OccurredAt: now(),
		})
		if err != nil {
			t.Fatalf("-> REMOTE_RETAINED: %v", err)
		}
		return out.Record
	}

	out, err = j.RecordTransition(ctx, state.Transition{
		Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Committed), To: string(lifecycle.RemoteDeletePending),
		OccurredAt: now(),
	})
	if err != nil {
		t.Fatalf("-> REMOTE_DELETE_PENDING: %v", err)
	}
	if p.stopAt == lifecycle.RemoteDeletePending {
		return out.Record
	}

	if p.stopAt == lifecycle.Complete || p.stopAt == lifecycle.QuarantinedLost {
		deletedAt := now()
		out, err = j.RecordTransition(ctx, state.Transition{
			Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.RemoteDeletePending), To: string(lifecycle.Complete),
			OccurredAt: now(), Deletion: &state.DeletionUpdate{DeletedAt: &deletedAt},
		})
		if err != nil {
			t.Fatalf("-> COMPLETE: %v", err)
		}
		if p.stopAt == lifecycle.Complete {
			return out.Record
		}

		out, err = j.RecordTransition(ctx, state.Transition{
			Artifact: p.artifact, Key: nextKey(), From: string(lifecycle.Complete), To: string(lifecycle.QuarantinedLost),
			OccurredAt: now(),
		})
		if err != nil {
			t.Fatalf("-> QUARANTINED_LOST: %v", err)
		}
		return out.Record
	}

	t.Fatalf("driveTo: unsupported stopAt %s", p.stopAt)
	return state.Record{}
}

// driveToFailed builds a FAILED fixture directly from DISCOVERED, since
// FAILED sits off to the side of the nominal chain driveTo walks.
func driveToFailed(t *testing.T, j *state.Journal, artifact model.ArtifactID) state.Record {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	size := int64(10)

	if _, err := j.Discover(ctx, artifact, artifact.String()+"#discover", "backups/"+artifact.Name,
		state.RemoteIdentity{Size: &size}, now); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: artifact.String() + "#failed", From: string(lifecycle.Discovered), To: string(lifecycle.Failed),
		OccurredAt: now,
	})
	if err != nil {
		t.Fatalf("-> FAILED: %v", err)
	}
	return out.Record
}

// --- fake transport ---

// fakeTransport is a hand-rolled transport.Transport, following the same
// "every method not configured returns an explicit, distinguishable error"
// convention internal/lifecycle/remotedelete_test.go's deleteTransport and
// internal/discovery/fake_transport_test.go's fakeTransport already use, so
// a test that unexpectedly reaches further than it meant to fails on an
// unambiguous error rather than a zero value.
type fakeTransport struct {
	stat map[string]func() (transport.RemoteArtifact, error)

	statCalls int
}

var _ transport.Transport = (*fakeTransport)(nil)

func (f *fakeTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	return nil, errors.New("fakeTransport: List not used")
}

func (f *fakeTransport) Stat(_ context.Context, _ transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	f.statCalls++
	fn, ok := f.stat[remotePath]
	if !ok {
		return transport.RemoteArtifact{}, fmt.Errorf("fakeTransport: Stat not configured for %q", remotePath)
	}
	return fn()
}

func (f *fakeTransport) CopyToLocal(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	return transport.TransferResult{}, errors.New("fakeTransport: CopyToLocal not used")
}

func (f *fakeTransport) RemoteHash(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
	return "", errors.New("fakeTransport: RemoteHash not used")
}

func (f *fakeTransport) DeleteRemote(context.Context, transport.Source, string) error {
	return errors.New("fakeTransport: DeleteRemote not used")
}

// statNotFound is a ready-made Stat responder for "confirmed absent".
func statNotFound() (transport.RemoteArtifact, error) {
	return transport.RemoteArtifact{}, transport.NewError(transport.NotFound, "stat", errors.New("no such object"))
}

// statAmbiguousErr is a ready-made Stat responder for "could not tell",
// which must never be treated as confirmed absence.
func statAmbiguousErr() (transport.RemoteArtifact, error) {
	return transport.RemoteArtifact{}, transport.NewError(transport.Transient, "stat", errors.New("connection reset"))
}
