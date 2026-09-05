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
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// minioFixtures adapts the container fixture to the contract suite.
//
// Every case gets its own BUCKET on the one server. The suite composes
// whole keys itself and deliberately reuses the same ones between cases, so
// its isolation requirement ("nothing written under a previous NewMedium may
// be visible through this one") cannot be met by a prefix it never sees.
type minioFixtures struct {
	fixture *machines.Medium
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
	fixture := machines.Start(t).Medium(t)
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
	fixture := machines.Start(t).Medium(t)
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
	fixture := machines.Start(t).Medium(t)
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
	fixture := machines.Start(t).Medium(t)
	adapter := rclone.New()
	ctx := context.Background()

	local := filepath.Join(t.TempDir(), "artifact.dump")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}

	medium := fixture.Medium()
	medium.Bucket = "typo-in-the-config"
	_, _ = adapter.UploadFromLocal(ctx, medium, local, "production/pg/artifact.dump", transport.UploadOptions{})

	if fixture.HasBucket(t, "typo-in-the-config") {
		t.Error("the failed upload created the bucket; a backup manager that provisions the bucket it was pointed at turns a typo into a silent second home for artifacts nobody looks in again")
	}
	// A positive control, because the assertion above is an absence and an
	// absence assertion that cannot fail is not an assertion.
	if !fixture.HasBucket(t, fixture.Bucket) {
		t.Fatal("the probe cannot see the bucket the fixture definitely created, so its verdict about the other one means nothing")
	}
}

// TestMinioNeverReportsAMissingBucketAsAMissingObject is confirmBucket's
// proof against the endpoint the bug was found on.
//
// rclone's s3 backend turns a 404 into fs.ErrorObjectNotFound or
// fs.ErrorDirNotFound before the adapter sees it and discards the S3 code
// that is the only thing separating NoSuchKey from NoSuchBucket. Measured
// here before the fix, against this same MinIO: DeleteObject against a
// mistyped bucket returned nil, ListObjects returned an empty listing and
// no error, and StatObject and OpenObject reported NotFound. Every one of
// those is a data-integrity failure under FR-30's prune or a catalog
// rebuild.
//
// The in-tree run of the same assertions (internal/transport/rclone's
// TestAMissingContainerIsNeverReportedAsAMissingObject, against rclone's
// local backend) proves the disambiguation on every gate. This one proves
// the theory it rests on: that an existing bucket answers a listing
// however empty it is, and only a missing one 404s.
func TestMinioNeverReportsAMissingBucketAsAMissingObject(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	adapter := rclone.New()
	ctx := context.Background()

	absent := fixture.MediumForBucket("a-bucket-nobody-created")
	const key = "production/pg/nothing-was-ever-written-here.dump"

	assertNotAnAbsence := func(t *testing.T, op string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s against a bucket that does not exist succeeded", op)
		}
		category, ok := transport.CategoryOf(err)
		if !ok || category != transport.Configuration {
			t.Errorf("%s classified as %v (recognised=%v), want %s. A reconciler reads %s as the medium having LOST the "+
				"artifact. The error was: %v", op, category, ok, transport.Configuration, transport.NotFound, err)
		}
	}

	t.Run("StatObject", func(t *testing.T) {
		_, err := adapter.StatObject(ctx, absent, key)
		assertNotAnAbsence(t, "StatObject", err)
	})

	t.Run("OpenObject", func(t *testing.T) {
		rc, err := adapter.OpenObject(ctx, absent, key)
		if rc != nil {
			_ = rc.Close()
		}
		assertNotAnAbsence(t, "OpenObject", err)
	})

	t.Run("DeleteObject", func(t *testing.T) {
		err := adapter.DeleteObject(ctx, absent, key)
		if err == nil {
			t.Fatal("DeleteObject against a bucket that does not exist reported SUCCESS. Under FR-30's medium-aware prune " +
				"that marks every placement on the medium GONE for artifacts nobody deleted")
		}
		assertNotAnAbsence(t, "DeleteObject", err)
	})

	t.Run("ListObjects", func(t *testing.T) {
		objects, err := adapter.ListObjects(ctx, absent, "")
		if err == nil {
			t.Fatalf("ListObjects against a bucket that does not exist returned %d objects and no error; a catalog rebuild "+
				"reading that concludes the medium holds nothing", len(objects))
		}
		assertNotAnAbsence(t, "ListObjects", err)
	})

	// The controls, and they are the half that matters: confirmBucket
	// decides bucket-absent by listing the bucket ROOT, on the theory that
	// an existing bucket answers a listing however empty it is. If that
	// theory were wrong this fix would turn the FIRST operation against a
	// brand new medium into "your bucket does not exist", which is worse
	// than the bug it fixed and would only ever show up on a fresh
	// deployment. So the fixture creates a bucket it deliberately never
	// writes to, and asks a real endpoint.
	t.Run("an empty bucket is not a missing bucket", func(t *testing.T) {
		empty := fixture.NewBucket(t)

		objects, err := adapter.ListObjects(ctx, empty, "")
		if err != nil {
			t.Fatalf("ListObjects against an existing but empty bucket failed: %v", err)
		}
		if len(objects) != 0 {
			t.Fatalf("a bucket nothing was written to listed %d objects", len(objects))
		}

		if _, err := adapter.StatObject(ctx, empty, key); err == nil {
			t.Fatal("StatObject found an object in a bucket nothing was written to")
		} else if category, _ := transport.CategoryOf(err); category != transport.NotFound {
			t.Errorf("StatObject in an EMPTY bucket classified as %s, want %s: the bucket-absent check has swallowed a "+
				"genuine absence, which is the opposite mistake and just as bad. The error was: %v", category, transport.NotFound, err)
		}

		if err := adapter.DeleteObject(ctx, empty, key); err != nil {
			t.Errorf("DeleteObject of an absent key in an EXISTING empty bucket failed: %v", err)
		}

		local := filepath.Join(t.TempDir(), "artifact.dump")
		if err := os.WriteFile(local, []byte("the first bytes this bucket ever held"), 0o600); err != nil {
			t.Fatalf("writing the source file: %v", err)
		}
		if _, err := adapter.UploadFromLocal(ctx, empty, local, key, transport.UploadOptions{}); err != nil {
			t.Fatalf("the first upload into a brand new bucket failed: %v. That is the deployment-day failure this control "+
				"exists to catch", err)
		}
	})
}
