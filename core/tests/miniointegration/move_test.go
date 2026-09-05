// This file is E2.1's integration half: the move engine driven end to end
// against a real S3 API, out and back.
//
// The engine's own suite runs against a MediumStore double and against
// rclone's local backend, which proves the engine. This proves the engine
// over the backend an operator actually has, and the reverse leg proves
// direction-agnosticism, which is the property that lets an operator undo
// a tier-to-medium mapping without a restore procedure.
package miniointegration_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// TestMoveToS3AndBackAgain moves one artifact off local disk onto a real
// MinIO bucket, verified by read-back, and then moves it home.
//
// The read-back is what makes this test worth its container: the engine
// downloads the object from S3 and re-hashes it against the SHA-256 the
// journal recorded at ingestion, and the local copy is deleted only after
// that write lands. Nothing about that path is exercised by a double.
func TestMoveToS3AndBackAgain(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.Medium()
	ctx := context.Background()

	root := t.TempDir()
	content := []byte("a real artifact, uploaded to a real S3 API and read back from it")
	hash := sha256HexOf(content)

	set := model.BackupSetID{Source: "production", Set: "postgres-primary"}
	artifact := model.ArtifactID{Set: set, Name: "2026-09-02T03-00-00Z.dump"}
	localPath := filepath.Join(root, artifact.Name)
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatalf("writing the local artifact: %v", err)
	}

	journal := openJournal(t)
	seedArtifactOnLocal(t, journal, artifact, localPath, content, time.Now().UTC())

	local, err := artifactstore.NewLocal(root)
	if err != nil {
		t.Fatalf("building the local store: %v", err)
	}
	engine := &placement.Engine{
		Journal:          journal,
		Store:            rclone.New(),
		Local:            local,
		Mediums:          minioResolver{medium: medium},
		Sets:             minioSets{set: config.BackupSet{Name: set.Set, ID: set, LocalPath: root}},
		Tiers:            nothingSelectsIt{},
		MaxMovesPerCycle: 4,
	}

	key, err := transport.MediumKey(medium.Prefix, artifact)
	if err != nil {
		t.Fatalf("computing the key: %v", err)
	}

	// --- out to S3 ---

	report, err := engine.RunCycle(ctx, []placement.Plan{{Artifact: artifact, DestinationMedium: medium.ID}})
	if err != nil {
		t.Fatalf("the outward cycle failed: %v", err)
	}
	if report.Completed != 1 {
		t.Fatalf("the outward move did not complete: %+v", report.Outcomes)
	}
	if _, err := os.Lstat(localPath); err == nil {
		t.Error("the local copy is still on disk after a completed move to S3")
	}

	info, err := rclone.New().StatObject(ctx, medium, key)
	if err != nil {
		t.Fatalf("the object is not on the bucket at %q: %v", key, err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("the object is %d bytes, want %d", info.Size, len(content))
	}
	assertPlacement(t, journal, artifact, medium.ID, state.PlacementActive, state.VerificationContent)
	assertPlacement(t, journal, artifact, state.MediumLocal, state.PlacementGone, "")

	if err := placement.CheckInvariant(mustGet(t, journal, artifact)); err != nil {
		t.Fatalf("FR-30's standing invariant does not hold after the outward move: %v", err)
	}

	// --- and home again, the same engine, the same phases ---

	report, err = engine.RunCycle(ctx, []placement.Plan{{Artifact: artifact, DestinationMedium: state.MediumLocal}})
	if err != nil {
		t.Fatalf("the reverse cycle failed: %v", err)
	}
	if report.Completed != 1 {
		t.Fatalf("the reverse move did not complete: %+v", report.Outcomes)
	}

	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("the artifact did not come back to local disk: %v", err)
	}
	if sha256HexOf(got) != hash {
		t.Error("the artifact came back with different bytes")
	}
	if _, err := rclone.New().StatObject(ctx, medium, key); err == nil {
		t.Error("the object is still on the bucket after a completed move off it")
	}
	assertPlacement(t, journal, artifact, state.MediumLocal, state.PlacementActive, state.VerificationContent)
	assertPlacement(t, journal, artifact, medium.ID, state.PlacementGone, "")

	if err := placement.CheckInvariant(mustGet(t, journal, artifact)); err != nil {
		t.Fatalf("FR-30's standing invariant does not hold after the reverse move: %v", err)
	}

	moves, err := journal.MovesForArtifact(ctx, artifact)
	if err != nil {
		t.Fatalf("reading the move journal: %v", err)
	}
	if len(moves) != 2 {
		t.Fatalf("expected two moves in the journal, got %d", len(moves))
	}
	for _, mv := range moves {
		if placement.Phase(mv.Phase) != placement.Done {
			t.Errorf("move %d ended at %s, want DONE (error: %q)", mv.ID, mv.Phase, mv.Error)
		}
	}
}

// TestAttestedAgainstS3RefusesRatherThanDeletingALocalCopy is the settled
// fact of this EPIC, proved against a real endpoint by the code that would
// act on it.
//
// rclone v1.75.0's s3 backend reports exactly hash.MD5, so no S3 endpoint
// reachable through this build can attest a full-object SHA-256. A medium
// configured for upload_verification: attested therefore cannot reach
// VERIFIED at all, and what must happen is a loud refusal with the local
// copy still on disk, never a quiet fall back to something cheaper.
func TestAttestedAgainstS3RefusesRatherThanDeletingALocalCopy(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.Medium()
	ctx := context.Background()

	root := t.TempDir()
	content := []byte("this copy must still be here when the test ends")
	set := model.BackupSetID{Source: "production", Set: "postgres-primary"}
	artifact := model.ArtifactID{Set: set, Name: "2026-09-02T04-00-00Z.dump"}
	localPath := filepath.Join(root, artifact.Name)
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatalf("writing the local artifact: %v", err)
	}

	journal := openJournal(t)
	seedArtifactOnLocal(t, journal, artifact, localPath, content, time.Now().UTC())

	local, err := artifactstore.NewLocal(root)
	if err != nil {
		t.Fatalf("building the local store: %v", err)
	}
	engine := &placement.Engine{
		Journal:          journal,
		Store:            rclone.New(),
		Local:            local,
		Mediums:          minioResolver{medium: medium, class: placement.Attested},
		Sets:             minioSets{set: config.BackupSet{Name: set.Set, ID: set, LocalPath: root}},
		Tiers:            nothingSelectsIt{},
		MaxMovesPerCycle: 4,
	}

	if _, err := engine.RunCycle(ctx, []placement.Plan{{Artifact: artifact, DestinationMedium: medium.ID}}); err != nil {
		t.Fatalf("the cycle failed: %v", err)
	}

	if _, err := os.Lstat(localPath); err != nil {
		t.Fatalf("THE LOCAL COPY WAS DELETED against a medium that cannot attest anything: %v", err)
	}
	moves, err := journal.MovesForArtifact(ctx, artifact)
	if err != nil {
		t.Fatalf("reading the move journal: %v", err)
	}
	if len(moves) != 1 || placement.Phase(moves[0].Phase) != placement.Abandoned {
		t.Fatalf("expected one ABANDONED move, got %+v", moves)
	}
	assertNoPlacement(t, journal, artifact, medium.ID)
}

// --- helpers -----------------------------------------------------------

type minioResolver struct {
	medium transport.Medium
	class  placement.Class
}

func (r minioResolver) Resolve(id string) (transport.Medium, placement.Class, error) {
	class := r.class
	if class == "" {
		class = placement.Content
	}
	if id != r.medium.ID {
		return transport.Medium{}, "", errNoSuchMedium
	}
	return r.medium, class, nil
}

type minioSets struct{ set config.BackupSet }

func (s minioSets) Set(model.BackupSetID) (config.BackupSet, error) { return s.set, nil }

type nothingSelectsIt struct{}

func (nothingSelectsIt) SourceStillSelected(context.Context, state.Record, string) (bool, string, error) {
	return false, "", nil
}

func mustGet(t *testing.T, j *state.Journal, artifact model.ArtifactID) state.Record {
	t.Helper()
	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("reading the artifact: %v", err)
	}
	return rec
}

func assertPlacement(t *testing.T, j *state.Journal, artifact model.ArtifactID, medium, status, class string) {
	t.Helper()
	for _, p := range mustGet(t, j, artifact).Placements {
		if p.Medium != medium {
			continue
		}
		if p.Status != status {
			t.Errorf("the placement on %q is %s, want %s", medium, p.Status, status)
		}
		if class != "" && p.VerificationClass != class {
			t.Errorf("the placement on %q records class %q, want %q", medium, p.VerificationClass, class)
		}
		return
	}
	t.Errorf("no placement records a copy on %q", medium)
}

func assertNoPlacement(t *testing.T, j *state.Journal, artifact model.ArtifactID, medium string) {
	t.Helper()
	for _, p := range mustGet(t, j, artifact).Placements {
		if p.Medium == medium {
			t.Errorf("a placement was written for %q, which nothing verified: %+v", medium, p)
		}
	}
}

// seedArtifactOnLocal drives an artifact to COMPLETE with its only ACTIVE
// placement on local disk, which is where every artifact is before the
// move engine has ever looked at it.
func seedArtifactOnLocal(t *testing.T, j *state.Journal, artifact model.ArtifactID, localPath string, content []byte, at time.Time) {
	t.Helper()
	ctx := context.Background()
	size := int64(len(content))
	hash := sha256HexOf(content)
	partial := localPath + ".partial"

	if _, err := j.Discover(ctx, artifact, artifact.String()+":discover", "backups/"+artifact.Name,
		state.RemoteIdentity{Size: &size, Hash: hash, HashAlg: "sha256"}, at); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, tr := range []state.Transition{
		{Artifact: artifact, Key: artifact.String() + ":transferring", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: at, LocalPath: &partial},
		{Artifact: artifact, Key: artifact.String() + ":transferred", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: at, Transfer: &state.TransferResult{BytesTransferred: size}},
		{Artifact: artifact, Key: artifact.String() + ":verifying", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: at},
		{Artifact: artifact, Key: artifact.String() + ":verified", From: "VERIFYING", To: "VERIFIED", OccurredAt: at, Hashes: &state.HashUpdate{Hash: hash, Alg: "sha256"}},
		{Artifact: artifact, Key: artifact.String() + ":committing", From: "VERIFIED", To: "COMMITTING", OccurredAt: at},
		{Artifact: artifact, Key: artifact.String() + ":committed", From: "COMMITTING", To: "COMMITTED", OccurredAt: at, LocalPath: &localPath,
			Placement: &state.PlacementUpdate{Medium: state.MediumLocal, Location: localPath, Size: &size,
				Hash: hash, HashAlg: "sha256", VerificationClass: state.VerificationContent, VerifiedAt: &at, Status: state.PlacementActive}},
		{Artifact: artifact, Key: artifact.String() + ":pending", From: "COMMITTED", To: "REMOTE_DELETE_PENDING", OccurredAt: at},
		{Artifact: artifact, Key: artifact.String() + ":complete", From: "REMOTE_DELETE_PENDING", To: "COMPLETE", OccurredAt: at},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s (%s): %v", tr.To, tr.Key, err)
		}
	}
}

// errNoSuchMedium is the resolver's own refusal, a value rather than a
// formatted string so a caller can recognise it.
var errNoSuchMedium = errors.New("no medium with that id is configured")

// countedStore records what a move actually asks a real endpoint for.
//
// It sits between the engine and the rclone adapter rather than inside
// MinIO, which is the honest place for a cost assertion: what has to be
// true is that the engine never ASKS for the bytes twice, and this records
// every ask, including one a backend might have answered from a cache.
type countedStore struct {
	inner placement.MediumStore

	stats, opens, uploads, deletes, restores atomic.Int64
}

func (s *countedStore) StatObject(ctx context.Context, m transport.Medium, key string) (transport.ObjectInfo, error) {
	s.stats.Add(1)
	return s.inner.StatObject(ctx, m, key)
}

func (s *countedStore) UploadFromLocal(ctx context.Context, m transport.Medium, localPath, key string, o transport.UploadOptions) (transport.UploadResult, error) {
	s.uploads.Add(1)
	return s.inner.UploadFromLocal(ctx, m, localPath, key, o)
}

func (s *countedStore) OpenObject(ctx context.Context, m transport.Medium, key string) (io.ReadCloser, error) {
	s.opens.Add(1)
	return s.inner.OpenObject(ctx, m, key)
}

func (s *countedStore) ObjectChecksum(ctx context.Context, m transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	return s.inner.ObjectChecksum(ctx, m, key, alg)
}

func (s *countedStore) DeleteObject(ctx context.Context, m transport.Medium, key string) error {
	s.deletes.Add(1)
	return s.inner.DeleteObject(ctx, m, key)
}

func (s *countedStore) RestoreStatus(ctx context.Context, m transport.Medium, key string) (*transport.RestoreState, error) {
	s.restores.Add(1)
	return s.inner.RestoreStatus(ctx, m, key)
}

// TestAMoveToS3ReadsTheObjectBackOnce is issue #439's measurement, taken
// against a real S3 API rather than against a double.
//
// The issue reported uploads=1 opens=2: the whole artifact downloaded
// twice, once to reach VERIFIED and once again in deleteSource immediately
// before the source delete. On a 100 GB artifact that is 200 GB of egress
// to move one file.
//
// It has to be measured here as well as in the engine's own suite, because
// the saving depends on something only a real endpoint can supply. The
// pre-delete proof stands only while the medium reports the same size, mod
// time and storage class for the object as it did immediately before the
// read, and a backend that reports no mod time at all gives that check
// nothing to work with and falls back to the full read, correctly and
// silently. A double can be told to report one. MinIO has to actually do
// it, and if it stopped, this is the test that would say so rather than a
// bill that would.
func TestAMoveToS3ReadsTheObjectBackOnce(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.Medium()
	ctx := context.Background()

	root := t.TempDir()
	content := []byte("one artifact, uploaded to a real S3 API and read back from it exactly once")

	set := model.BackupSetID{Source: "production", Set: "postgres-primary"}
	artifact := model.ArtifactID{Set: set, Name: "2026-09-03T03-00-00Z.dump"}
	localPath := filepath.Join(root, artifact.Name)
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatalf("writing the local artifact: %v", err)
	}

	journal := openJournal(t)
	seedArtifactOnLocal(t, journal, artifact, localPath, content, time.Now().UTC())

	local, err := artifactstore.NewLocal(root)
	if err != nil {
		t.Fatalf("building the local store: %v", err)
	}
	store := &countedStore{inner: rclone.New()}
	engine := &placement.Engine{
		Journal:          journal,
		Store:            store,
		Local:            local,
		Mediums:          minioResolver{medium: medium},
		Sets:             minioSets{set: config.BackupSet{Name: set.Set, ID: set, LocalPath: root}},
		Tiers:            nothingSelectsIt{},
		MaxMovesPerCycle: 4,
	}

	report, err := engine.RunCycle(ctx, []placement.Plan{{Artifact: artifact, DestinationMedium: medium.ID}})
	if err != nil {
		t.Fatalf("the cycle failed: %v", err)
	}
	if report.Completed != 1 {
		t.Fatalf("the move did not complete, so these counts are of something else: %+v", report.Outcomes)
	}
	if _, err := os.Lstat(localPath); err == nil {
		t.Fatal("the local copy is still on disk after a completed move, so nothing here measured a finished move")
	}

	if got := store.uploads.Load(); got != 1 {
		t.Errorf("the move uploaded %d times, want 1", got)
	}
	if got := store.opens.Load(); got != 1 {
		t.Errorf("the move downloaded the object %d times, want 1. This is #439's number and it was 2. "+
			"If it is 2 again, the pre-delete proof is not standing against a real endpoint, and the most likely reason "+
			"is that the backend has stopped reporting a mod time for the object", got)
	}
	if got := store.stats.Load(); got != 2 {
		t.Errorf("the move spent %d metadata calls on the destination, want 2: one immediately before the read the proof is "+
			"about, one immediately before the delete", got)
	}
	if got := store.restores.Load(); got != 0 {
		t.Errorf("the move asked about a restore %d times for an object on a class that reads on demand", got)
	}
}
