// Whether revalidation against a real S3 endpoint checks existence without
// ever paying to read the bytes back.
//
// The claim is about a cost, and a cost claim asserted from the outside is
// almost always asserted wrongly: a backend can answer a read from a cache,
// and a test watching the wire would call that a pass. So the accounting
// happens at the boundary the code under test actually calls, where every
// ASK is recorded whether or not it becomes a request.
//
// The cells around it are what stop the accounting passing for the wrong
// reason. An object that is genuinely gone has to be noticed rather than
// shrugged off, because a revalidator that asks for nothing at all also
// downloads nothing, and attesting a placement this endpoint cannot attest
// has to be refused rather than assumed.
package miniointegration_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/revalidate"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// accountingStore is the request accounting the issue's INTEGRATION step
// asks for, taken at the boundary the code under test actually uses.
//
// Counting here rather than inside MinIO is the honest place for this
// assertion: what has to be true is that revalidation never ASKS for the
// bytes, and this records every ask, including the ones a backend might
// answer from a cache and never turn into a request on the wire.
type accountingStore struct {
	inner   placement.Store
	stats   atomic.Int64
	opens   atomic.Int64
	digests atomic.Int64
}

func (s *accountingStore) StatObject(ctx context.Context, m transport.Medium, key string) (transport.ObjectInfo, error) {
	s.stats.Add(1)
	return s.inner.StatObject(ctx, m, key)
}

func (s *accountingStore) OpenObject(ctx context.Context, m transport.Medium, key string) (io.ReadCloser, error) {
	s.opens.Add(1)
	return s.inner.OpenObject(ctx, m, key)
}

func (s *accountingStore) ObjectChecksum(ctx context.Context, m transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	s.digests.Add(1)
	return s.inner.ObjectChecksum(ctx, m, key, alg)
}

type oneMedium struct{ medium transport.Medium }

func (o oneMedium) MediumFor(id string) (transport.Medium, bool) {
	if id != o.medium.ID {
		return transport.Medium{}, false
	}
	return o.medium, true
}

// TestRevalidationAgainstMinioExistenceChecksAndNeverDownloads is EPIC E's
// Phase 1 exit-gate line for revalidation, run against a real S3 endpoint:
// the placement is existence-checked, the class reported is existence, and
// no bytes are downloaded.
func TestRevalidationAgainstMinioExistenceChecksAndNeverDownloads(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	adapter := rclone.New()
	ctx := context.Background()
	medium := fixture.Medium()
	medium.ID = "offsite_s3"

	content := []byte("an artifact that now lives only in a bucket")
	local := filepath.Join(t.TempDir(), "backup.dump")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	key, err := transport.MediumKey("rclone-manager", artifact)
	if err != nil {
		t.Fatalf("MediumKey: %v", err)
	}
	if _, err := adapter.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}

	journal := openJournal(t)
	long := time.Now().UTC().Add(-90 * 24 * time.Hour)
	seedArtifactOnMedium(t, journal, artifact, key, content, medium.ID, long)

	store := &accountingStore{inner: adapter}
	report, err := revalidate.Run(ctx, revalidate.Deps{
		Journal: journal,
		Store:   store,
		Mediums: oneMedium{medium: medium},
	}, set, config.Revalidation{
		Interval:    config.Duration(24 * time.Hour),
		MaxPerCycle: 10,
		Hash:        true,
	})
	if err != nil {
		t.Fatalf("revalidate.Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one", report.Findings)
	}
	f := report.Findings[0]
	if !f.Checked || !f.Passed {
		t.Fatalf("the existence check did not pass against an object that is really there: %+v", f)
	}
	if f.Class != placement.Existence {
		t.Errorf("Class = %q, want %q", f.Class, placement.Existence)
	}
	if got := store.opens.Load(); got != 0 {
		t.Errorf("the automatic pass asked the medium for the object's bytes %d times; anything that costs egress is operator-initiated", got)
	}
	if got := store.digests.Load(); got != 0 {
		t.Errorf("the automatic pass asked for %d attestations; the ceiling is existence", got)
	}
	if got := store.stats.Load(); got != 1 {
		t.Errorf("the medium was statted %d times, want exactly 1", got)
	}
}

// TestRevalidationAgainstMinioNoticesAnObjectThatIsGone is the failing
// half against the real endpoint: a weak check that fails is still a real
// verdict, and it must route the artifact exactly where a failed local
// recheck routes it.
func TestRevalidationAgainstMinioNoticesAnObjectThatIsGone(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	adapter := rclone.New()
	ctx := context.Background()
	medium := fixture.Medium()
	medium.ID = "offsite_s3"

	content := []byte("an artifact somebody deleted out from under this manager")
	local := filepath.Join(t.TempDir(), "backup.dump")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	key, err := transport.MediumKey("rclone-manager", artifact)
	if err != nil {
		t.Fatalf("MediumKey: %v", err)
	}

	// Uploaded, then removed through the same boundary, so what the
	// revalidation pass meets is a genuinely absent object rather than a
	// key that was never written.
	if _, err := adapter.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}
	if err := adapter.DeleteObject(ctx, medium, key); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	journal := openJournal(t)
	long := time.Now().UTC().Add(-90 * 24 * time.Hour)
	seedArtifactOnMedium(t, journal, artifact, key, content, medium.ID, long)

	report, err := revalidate.Run(ctx, revalidate.Deps{
		Journal: journal,
		Store:   adapter,
		Mediums: oneMedium{medium: medium},
	}, set, config.Revalidation{
		Interval:    config.Duration(24 * time.Hour),
		MaxPerCycle: 10,
		Hash:        true,
	})
	if err != nil {
		t.Fatalf("revalidate.Run: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one", report.Findings)
	}
	f := report.Findings[0]
	if !f.Checked {
		t.Fatalf("an absent object was reported as unchecked: %s", f.Reason)
	}
	if f.Passed {
		t.Fatalf("an absent object passed its existence check: %s", f.Reason)
	}
	if f.Class != placement.Existence {
		t.Errorf("Class = %q, want %q", f.Class, placement.Existence)
	}

	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != "QUARANTINED_LOST" {
		t.Errorf("state = %q, want QUARANTINED_LOST", rec.State)
	}
}

// TestAttestingAMinioPlacementIsRefused runs the ladder's Attested rung
// against a real endpoint, which is where FR-13's rule actually bites: no
// S3 endpoint reachable through rclone v1.75.0 can attest a full-object
// SHA-256, so this must be an explicit capability refusal every time.
func TestAttestingAMinioPlacementIsRefused(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	adapter := rclone.New()
	ctx := context.Background()
	medium := fixture.Medium()
	medium.ID = "offsite_s3"

	content := []byte("bytes no endpoint here can attest")
	local := filepath.Join(t.TempDir(), "backup.dump")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}
	const key = "rclone-manager/production/postgres-primary/backup.dump"
	if _, err := adapter.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}

	size := int64(len(content))
	p := state.Placement{
		Medium: medium.ID, Location: key, Size: &size,
		Hash: sha256HexOf(content), HashAlg: "sha256", Status: state.PlacementActive,
	}

	// Existence and content both work against this endpoint, which is what
	// makes the attested refusal a statement about attestation rather than
	// about the fixture.
	for _, class := range []placement.Class{placement.Existence, placement.Content} {
		got, err := placement.Verify(ctx, adapter, medium, p, class, time.Now().UTC())
		if err != nil {
			t.Fatalf("Verify(%s): %v", class, err)
		}
		if !got.Passed || got.Class != class {
			t.Fatalf("Verify(%s) = %+v", class, got)
		}
	}

	got, err := placement.Verify(ctx, adapter, medium, p, placement.Attested, time.Now().UTC())
	if err == nil {
		t.Fatalf("Verify(attested) returned %+v against a real S3 endpoint; if a newer rclone can genuinely do this, that is a capability change to adopt deliberately", got)
	}
	// Logged rather than only asserted, because this is the measurement
	// FR-31 asks to be re-taken on every rclone upgrade, and the next
	// person to take it wants to read what the endpoint actually said
	// rather than re-derive it from a green tick.
	t.Logf("rclone's s3 backend refused a full-object SHA-256 attestation: %v", err)

	// The typed refusal, not just its prose. ErrClassUnavailable is what a
	// caller branches on to tell "we could not check" from "we checked and
	// it is wrong", and a refusal that only reads correctly is a refusal
	// nothing can act on.
	if !errors.Is(err, placement.ErrClassUnavailable) {
		t.Errorf("the refusal is not ErrClassUnavailable: %v; a caller cannot tell it apart from a failed verification, and a failed verification quarantines", err)
	}
	// And it is a capability refusal all the way down, rather than a
	// transient failure that would come back differently on a retry.
	if category, ok := transport.CategoryOf(err); !ok || category != transport.UnsupportedCapability {
		t.Errorf("the refusal is classified %v (recognised=%v), want %v: FR-13 asks for an explicit capability result", category, ok, transport.UnsupportedCapability)
	}
	if !strings.Contains(err.Error(), "attest") {
		t.Errorf("the refusal does not say what could not be done: %v", err)
	}
	if got != (placement.Result{}) {
		t.Errorf("Verify returned %+v alongside its refusal", got)
	}
}
