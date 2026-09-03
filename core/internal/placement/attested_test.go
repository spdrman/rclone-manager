package placement_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// TestTheAttestedRungPassesWhereTheBackendCanAttest is the positive control
// for the refusal the MinIO suite records, and it is what makes that
// refusal mean anything.
//
// "No S3 endpoint reachable through this build can attest a full-object
// SHA-256" and "this ladder cannot attest at all" produce the identical
// red in the MinIO run. Only one of them is true, and telling them apart
// needs a backend that CAN attest, driven through the same Verify call and
// the same real adapter. rclone's local backend is that: it hashes a file
// it can read, so ObjectChecksum answers, and the Attested rung passes.
//
// So the refusal on s3 is the endpoint's capability speaking through this
// ladder, rather than a rung nobody has ever seen work.
func TestTheAttestedRungPassesWhereTheBackendCanAttest(t *testing.T) {
	ctx := context.Background()
	adapter := rclone.New()

	// MediumTypeLocalDir is not configurable, by design: core/internal/config
	// closes the medium type set to s3 alone, so the only way to build one
	// is by hand, in a test. See transport.MediumTypeLocalDir.
	medium := transport.Medium{ID: "attesting_medium", Type: transport.MediumTypeLocalDir, Bucket: t.TempDir()}

	content := []byte("bytes a backend that hashes can vouch for")
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
		Hash: sha256Of(content), HashAlg: string(transport.SHA256), Status: state.PlacementActive,
	}
	now := time.Now().UTC()

	for _, class := range placement.Classes {
		got, err := placement.Verify(ctx, adapter, medium, p, class, now)
		if err != nil {
			t.Fatalf("Verify(%s) against a backend that can attest: %v", class, err)
		}
		if !got.Passed {
			t.Fatalf("Verify(%s) did not pass against a correct object: %s", class, got.Detail)
		}
		if got.Class != class {
			t.Errorf("Verify(%s) reported class %s", class, got.Class)
		}
	}
}

// TestTheAttestedRungCatchesAWrongObjectWhereItCanAttest is the other half:
// the rung has to be able to FAIL where it can run, or "it passes" only
// says it returns true.
func TestTheAttestedRungCatchesAWrongObjectWhereItCanAttest(t *testing.T) {
	ctx := context.Background()
	adapter := rclone.New()
	medium := transport.Medium{ID: "attesting_medium", Type: transport.MediumTypeLocalDir, Bucket: t.TempDir()}

	stored := []byte("what is actually in the bucket")
	local := filepath.Join(t.TempDir(), "backup.dump")
	if err := os.WriteFile(local, stored, 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}
	const key = "rclone-manager/production/postgres-primary/backup.dump"
	if _, err := adapter.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}

	// The journal recorded a different artifact's hash entirely.
	size := int64(len(stored))
	p := state.Placement{
		Medium: medium.ID, Location: key, Size: &size,
		Hash: sha256Of([]byte("what the journal says was ingested")), HashAlg: string(transport.SHA256),
		Status: state.PlacementActive,
	}

	got, err := placement.Verify(ctx, adapter, medium, p, placement.Attested, time.Now().UTC())
	if err != nil {
		t.Fatalf("Verify(attested) reported a capability refusal for a check it actually ran: %v", err)
	}
	if got.Passed {
		t.Fatalf("Verify(attested) passed an object the endpoint attests as something else: %s", got.Detail)
	}
	if got.Class != placement.Attested {
		t.Errorf("Class = %s, want %s", got.Class, placement.Attested)
	}
}
