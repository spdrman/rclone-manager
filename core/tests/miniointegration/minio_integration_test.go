// Package miniointegration_test is E1.3's integration half: the MediumStore
// contract suite run against a real S3 API, plus the facts about that API
// which only a real endpoint can establish.
//
// The in-tree run of the same suite (against rclone's local backend, in
// internal/transport/rclone) proves the suite itself is sound. This one
// proves the s3 backend behind it is, and it is where every claim this
// adapter makes about S3 gets checked against something that can disagree.
package miniointegration_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/contract"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/miniofixture"
)

// minioFixtures adapts the container fixture to the contract suite.
//
// Every case gets its own BUCKET on the one server. The suite composes
// whole keys itself and deliberately reuses the same ones between cases, so
// its isolation requirement ("nothing written under a previous NewMedium may
// be visible through this one") cannot be met by a prefix it never sees.
type minioFixtures struct {
	fixture *miniofixture.Fixture
}

func (f *minioFixtures) NewMedium(t *testing.T) transport.Medium {
	t.Helper()
	return f.fixture.NewBucket(t)
}

// AttestsSHA256 is false, and it is not a fixture limitation: rclone
// v1.75.0's s3 backend reports exactly hash.MD5, so no S3 endpoint
// reachable through this build can attest a full-object SHA-256. This is
// therefore the run that exercises FR-31's REFUSAL branch, against a real
// endpoint. The local-backend run in internal/transport/rclone exercises
// the success branch.
func (f *minioFixtures) AttestsSHA256() bool { return false }

// TestMinioMediumContractSuite runs the whole MediumStore contract against
// a real S3 endpoint.
func TestMinioMediumContractSuite(t *testing.T) {
	fixture := miniofixture.Start(t)
	contract.RunMedium(t, rclone.New(), &minioFixtures{fixture: fixture})
}

// TestMinioAttestationIsRefused is FR-31's capability rule proved against
// the endpoint rather than asserted about it.
//
// rclone v1.75.0's s3 backend reports exactly hash.MD5 and refuses every
// other algorithm, so no S3 endpoint reachable through this build can
// attest a full-object SHA-256. The right behaviour is an explicit
// capability refusal, and the wrong one, the one FR-13 was written about,
// is a weaker answer wearing the same name.
//
// This is also the test that will notice if a future rclone upgrade makes
// attestation possible: it will start failing, and the fix will be to
// teach the fixture that s3 attests now, which is a decision somebody
// should make deliberately rather than discover in production.
func TestMinioAttestationIsRefused(t *testing.T) {
	fixture := miniofixture.Start(t)
	adapter := rclone.New()
	ctx := context.Background()
	medium := fixture.Medium()

	local := filepath.Join(t.TempDir(), "artifact.dump")
	if err := os.WriteFile(local, []byte("bytes nobody can attest"), 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}
	const key = "attestation/production/pg/artifact.dump"
	if _, err := adapter.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}

	attestation, err := adapter.ObjectChecksum(ctx, medium, key, transport.SHA256)
	if err == nil {
		t.Fatalf("ObjectChecksum attested %+v against a real S3 endpoint; if a newer rclone can genuinely do this, that is a capability change to adopt deliberately, not to discover here", attestation)
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.UnsupportedCapability {
		t.Errorf("the refusal classified as %v (recognised=%v), want %s", category, ok, transport.UnsupportedCapability)
	}
	if attestation != (transport.ChecksumAttestation{}) {
		t.Errorf("ObjectChecksum returned %+v alongside its refusal", attestation)
	}
}

// TestMinioErrorClassification is FR-28's error table, checked against
// errors a real endpoint actually produced rather than against errors this
// repository wrote for itself.
//
// The unit table in internal/transport/rclone/medium_test.go can only ever
// prove the MAPPING. This proves the codes: that a wrong credential really
// does arrive as something this table has an entry for, and that a missing
// bucket really does say NoSuchBucket where the manager can see it.
func TestMinioErrorClassification(t *testing.T) {
	fixture := miniofixture.Start(t)
	adapter := rclone.New()
	ctx := context.Background()

	local := filepath.Join(t.TempDir(), "artifact.dump")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}

	t.Run("a wrong credential is an authentication failure", func(t *testing.T) {
		// Through the env source, so nothing writes a second credential
		// file: the value never leaves this process except into rclone.
		t.Setenv("MINIO_WRONG_CREDENTIALS", "[default]\naws_access_key_id = wronguser\naws_secret_access_key = wrongpasswordwrongpassword\n")
		medium := fixture.Medium()
		medium.Credentials = transport.MediumCredentials{Env: "MINIO_WRONG_CREDENTIALS"}

		_, err := adapter.StatObject(ctx, medium, "anything/at/all.dump")
		if err == nil {
			t.Fatal("StatObject succeeded with a wrong credential")
		}
		category, ok := transport.CategoryOf(err)
		if !ok || category != transport.Authentication {
			t.Errorf("classified as %v (recognised=%v), want %s. The error was: %v", category, ok, transport.Authentication, err)
		}
	})

	t.Run("a bucket that does not exist is a configuration failure", func(t *testing.T) {
		medium := fixture.Medium()
		medium.Bucket = "no-such-bucket-anywhere"

		_, err := adapter.UploadFromLocal(ctx, medium, local, "production/pg/artifact.dump", transport.UploadOptions{})
		if err == nil {
			t.Fatal("UploadFromLocal succeeded against a bucket that does not exist; this adapter must never create one")
		}
		category, ok := transport.CategoryOf(err)
		if !ok || category != transport.Configuration {
			t.Errorf("classified as %v (recognised=%v), want %s. The error was: %v", category, ok, transport.Configuration, err)
		}
	})

	t.Run("an unreachable endpoint is not mistaken for an absence", func(t *testing.T) {
		medium := fixture.Medium()
		medium.Endpoint = "http://127.0.0.1:1"

		_, err := adapter.StatObject(ctx, medium, "production/pg/artifact.dump")
		if err == nil {
			t.Fatal("StatObject succeeded against a closed port")
		}
		category, _ := transport.CategoryOf(err)
		if category == transport.NotFound {
			t.Error("a medium that could not be reached classified as NotFound; a mover that believes that deletes a local copy on the strength of a network failure")
		}
	})
}

// TestMinioNeverCreatesABucket is the other half of the refusal above, and
// the one that matters for an operator who typed a bucket name wrong: after
// the failed upload, the bucket must still not exist.
//
// It is checked from INSIDE the container, on the drive MinIO reads its
// buckets off, rather than through the S3 API, so a permissions quirk in
// the API cannot make an existing bucket look absent.
func TestMinioNeverCreatesABucket(t *testing.T) {
	fixture := miniofixture.Start(t)
	adapter := rclone.New()
	ctx := context.Background()

	local := filepath.Join(t.TempDir(), "artifact.dump")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}

	medium := fixture.Medium()
	medium.Bucket = "typo-in-the-config"
	_, _ = adapter.UploadFromLocal(ctx, medium, local, "production/pg/artifact.dump", transport.UploadOptions{})

	if dirExistsInContainer(t, fixture.ContainerID(), "/data/typo-in-the-config") {
		t.Error("the failed upload created the bucket; a backup manager that provisions the bucket it was pointed at turns a typo into a silent second home for artifacts nobody looks in again")
	}
	// A positive control, because the assertion above is an absence and an
	// absence assertion that cannot fail is not an assertion.
	if !dirExistsInContainer(t, fixture.ContainerID(), "/data/"+fixture.Bucket) {
		t.Fatal("the probe cannot see the bucket the fixture definitely created, so its verdict about the other one means nothing")
	}
}
