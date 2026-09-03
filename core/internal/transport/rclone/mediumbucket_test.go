package rclone_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// TestAMissingContainerIsNeverReportedAsAMissingObject is confirmBucket's
// guard, and it is the reason it exists.
//
// rclone's s3 backend translates a 404 into fs.ErrorObjectNotFound or
// fs.ErrorDirNotFound before this adapter sees it, discarding the S3 code
// that is the only thing separating NoSuchKey from NoSuchBucket. Measured
// against a real MinIO before confirmBucket existed: DeleteObject against
// a mistyped bucket returned nil, ListObjects returned an empty listing
// and no error, and StatObject and OpenObject reported NotFound.
//
// This runs against rclone's LOCAL backend, on every gate, with no
// container: a local Fs rooted at a directory that does not exist produces
// the identical pair of sentinels, so the disambiguation this test is
// about is exercised even when Docker is not available. The MinIO run
// (core/tests/miniointegration) proves the same thing against the endpoint
// the bug was found on.
//
// Every case carries its own positive control against an EXISTING
// container, because "a missing container is refused" would also pass if
// the adapter had simply started refusing everything, which would be a
// worse bug than the one being fixed.
func TestAMissingContainerIsNeverReportedAsAMissingObject(t *testing.T) {
	ctx := context.Background()
	adapter := rclone.New()

	// The absent one is a path under a temp dir that is never created, so
	// nothing on this machine can make it exist by accident.
	absent := transport.Medium{
		ID:     "typo_in_the_config",
		Type:   transport.MediumTypeLocalDir,
		Bucket: filepath.Join(t.TempDir(), "no-such-container"),
	}
	present := transport.Medium{
		ID:     "correctly_configured",
		Type:   transport.MediumTypeLocalDir,
		Bucket: t.TempDir(),
	}

	const key = "production/pg/nothing-was-ever-written-here.dump"

	t.Run("StatObject", func(t *testing.T) {
		_, err := adapter.StatObject(ctx, absent, key)
		requireConfigurationNotNotFound(t, err, "StatObject", "stat_object")

		// Positive control: the same key in a container that DOES exist
		// still has to come back NotFound, or this adapter has stopped
		// being able to say "the object is not there" at all.
		_, err = adapter.StatObject(ctx, present, key)
		requireNotFound(t, err, "StatObject against an existing container")
	})

	t.Run("OpenObject", func(t *testing.T) {
		rc, err := adapter.OpenObject(ctx, absent, key)
		if rc != nil {
			_ = rc.Close()
		}
		requireConfigurationNotNotFound(t, err, "OpenObject", "open_object")

		rc, err = adapter.OpenObject(ctx, present, key)
		if rc != nil {
			_ = rc.Close()
		}
		requireNotFound(t, err, "OpenObject against an existing container")
	})

	t.Run("ListObjects", func(t *testing.T) {
		objects, err := adapter.ListObjects(ctx, absent, "")
		if err == nil {
			t.Fatalf("ListObjects against a container that does not exist returned %d objects and no error; "+
				"a catalog rebuild reading that concludes the medium holds nothing", len(objects))
		}
		requireConfigurationNotNotFound(t, err, "ListObjects", "list_objects")

		// Positive control, and the one that matters most: an EMPTY
		// container must still list empty with no error, or this fix has
		// turned the first operation against a brand new medium into
		// "your bucket does not exist".
		objects, err = adapter.ListObjects(ctx, present, "")
		if err != nil {
			t.Fatalf("ListObjects against an existing but empty container failed: %v. An empty container is not a missing one, "+
				"and reporting it as one would break every freshly created medium", err)
		}
		if len(objects) != 0 {
			t.Fatalf("ListObjects against an empty container returned %d objects", len(objects))
		}
	})

	t.Run("DeleteObject", func(t *testing.T) {
		err := adapter.DeleteObject(ctx, absent, key)
		if err == nil {
			t.Fatal("DeleteObject against a container that does not exist reported SUCCESS. " +
				"Under FR-30's medium-aware prune that marks every placement on the medium GONE for artifacts nobody deleted")
		}
		requireConfigurationNotNotFound(t, err, "DeleteObject", "delete_object")

		// Positive control: deleting something already absent from a
		// container that exists is still success, because the caller's
		// intent is already true.
		if err := adapter.DeleteObject(ctx, present, key); err != nil {
			t.Fatalf("DeleteObject of an absent key in an EXISTING container failed: %v. "+
				"A mover resuming after a crash has to be able to finish a delete it may already have completed", err)
		}
	})

	t.Run("a container that exists still serves a real round trip", func(t *testing.T) {
		// The broadest positive control. If confirmBucket were probing
		// the wrong thing, or probing on a healthy path, this is what
		// would notice.
		local := filepath.Join(t.TempDir(), "artifact.dump")
		if err := os.WriteFile(local, []byte("bytes that go somewhere real"), 0o600); err != nil {
			t.Fatalf("writing the source file: %v", err)
		}
		if _, err := adapter.UploadFromLocal(ctx, present, local, "production/pg/artifact.dump", transport.UploadOptions{}); err != nil {
			t.Fatalf("UploadFromLocal into an existing container failed: %v", err)
		}
		info, err := adapter.StatObject(ctx, present, "production/pg/artifact.dump")
		if err != nil {
			t.Fatalf("StatObject after a successful upload failed: %v", err)
		}
		if info.Size != int64(len("bytes that go somewhere real")) {
			t.Fatalf("StatObject reported size %d", info.Size)
		}
		rc, err := adapter.OpenObject(ctx, present, "production/pg/artifact.dump")
		if err != nil {
			t.Fatalf("OpenObject after a successful upload failed: %v", err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("reading the object back: %v", err)
		}
		if string(got) != "bytes that go somewhere real" {
			t.Fatalf("read back %q", got)
		}
	})
}

func requireConfigurationNotNotFound(t *testing.T, err error, op, wantOp string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s against a container that does not exist succeeded", op)
	}
	category, ok := transport.CategoryOf(err)
	if !ok {
		t.Fatalf("%s: the failure carried no category at all: %v", op, err)
	}
	if category == transport.NotFound {
		t.Fatalf("%s against a container that does not exist classified as %s. "+
			"A reconciler reads that as the medium having LOST the artifact, and a mover reads it as permission to delete "+
			"the local copy. The error was: %v", op, transport.NotFound, err)
	}
	if category != transport.Configuration {
		t.Fatalf("%s classified as %s, want %s (a bucket somebody mistyped is one line to fix, and %s is the label for "+
			"a failure nobody can act on). The error was: %v", op, category, transport.Configuration, category, err)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("%s: the refusal does not say the container is missing, so an operator cannot act on it: %v", op, err)
	}
	// The verdict carries the operation the CALLER was performing, not the
	// name of the probe that produced it. An operator reading "the medium's
	// bucket does not exist" needs to know which operation hit it.
	if !strings.HasPrefix(err.Error(), wantOp+":") {
		t.Errorf("%s: the refusal is reported under a different operation than the one that failed (want %q): %v", op, wantOp, err)
	}
}

func requireNotFound(t *testing.T, err error, op string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: an object that was never written was reported as present", op)
	}
	category, ok := transport.CategoryOf(err)
	if !ok || category != transport.NotFound {
		t.Fatalf("%s: classified as %v (recognised=%v), want %s. The bucket-absent check has swallowed a genuine absence, "+
			"which is the opposite mistake and just as bad: %v", op, category, ok, transport.NotFound, err)
	}
}
