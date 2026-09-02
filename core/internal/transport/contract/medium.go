package contract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// MediumFixtures is everything one transport.MediumStore implementation
// needs in order to run RunMedium: an isolated medium to operate against,
// and an honest answer about whether the backend can attest a full-object
// digest.
//
// It is deliberately smaller than Fixtures. The Transport suite needs a
// Put, because listing and copying are about objects some producer wrote;
// a MediumStore writes its own objects through UploadFromLocal, so the
// suite places everything it needs through the interface under test and a
// fixture has nothing to arrange.
type MediumFixtures interface {
	// NewMedium returns a fresh, isolated transport.Medium for a single
	// test. Nothing written under a previous NewMedium call may be visible
	// through this one, and t.Cleanup should tear down whatever it
	// created.
	NewMedium(t *testing.T) transport.Medium

	// AttestsSHA256 reports whether this backend can produce a
	// FULL-OBJECT SHA-256 attestation through ObjectChecksum.
	//
	// Both answers are contract-checked, and that is the point of asking
	// rather than probing: a backend that says yes has to produce the
	// right digest, and a backend that says no has to REFUSE with an
	// UnsupportedCapability error rather than return something weaker
	// wearing the same method's name. FR-31's rule only means anything if
	// the "no" branch is tested as hard as the "yes" branch, and the two
	// real backends this suite runs against sit on opposite sides of it:
	// rclone's local backend hashes a file it can read, and rclone's s3
	// backend (v1.75.0) exposes MD5 from the ETag and nothing else.
	AttestsSHA256() bool
}

// RunMedium executes the MediumStore contract suite against store, using
// fx to build mediums.
func RunMedium(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	t.Run("upload_stat_and_read_back", func(t *testing.T) { testMediumRoundTrip(t, store, fx) })
	t.Run("upload_converges_on_the_same_key", func(t *testing.T) { testMediumUploadConverges(t, store, fx) })
	t.Run("checksum_attests_or_refuses", func(t *testing.T) { testMediumChecksum(t, store, fx) })
	t.Run("checksum_refuses_an_algorithm_it_cannot_speak", func(t *testing.T) { testMediumChecksumAlgorithm(t, store, fx) })
	t.Run("list", func(t *testing.T) { testMediumList(t, store, fx) })
	t.Run("not_found", func(t *testing.T) { testMediumNotFound(t, store, fx) })
	t.Run("delete_removes_only_its_own_key", func(t *testing.T) { testMediumDelete(t, store, fx) })
	t.Run("delete_of_an_absent_key_is_not_an_error", func(t *testing.T) { testMediumDeleteAbsent(t, store, fx) })
	t.Run("cancel", func(t *testing.T) { testMediumCancel(t, store, fx) })
}

// putLocal writes content to a file under t.TempDir and returns its path,
// which is what UploadFromLocal is addressed by.
func putLocal(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing the local source file: %v", err)
	}
	return path
}

func testMediumRoundTrip(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	content := []byte("the durable bytes of an artifact")
	local := putLocal(t, "artifact.dump", content)
	const key = "prefix/production/postgres-primary/artifact.dump"

	result, err := store.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{})
	if err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}
	if result.Key != key {
		t.Errorf("UploadFromLocal reported key %q, want %q; an upload never chooses its own destination", result.Key, key)
	}
	if result.BytesUploaded != int64(len(content)) {
		t.Errorf("UploadFromLocal reported %d bytes, want %d", result.BytesUploaded, len(content))
	}

	info, err := store.StatObject(ctx, medium, key)
	if err != nil {
		t.Fatalf("StatObject after an upload: %v", err)
	}
	if info.Key != key {
		t.Errorf("StatObject reported key %q, want %q", info.Key, key)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("StatObject reported size %d, want %d", info.Size, len(content))
	}

	rc, err := store.OpenObject(ctx, medium, key)
	if err != nil {
		t.Fatalf("OpenObject: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading the object back: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("the object read back as %q, want %q", got, content)
	}
}

// testMediumUploadConverges is the property FR-30's restart semantics rest
// on: a move interrupted after the upload started restarts the upload to
// the same deterministic key, and that has to converge on one object with
// the right bytes rather than fail or leave a second copy.
func testMediumUploadConverges(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	content := []byte("re-uploaded after an interruption")
	local := putLocal(t, "artifact.dump", content)
	const key = "production/pg/artifact.dump"

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := store.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
			t.Fatalf("UploadFromLocal attempt %d: %v", attempt, err)
		}
	}

	objects, err := store.ListObjects(ctx, medium, "production")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("after two uploads to the same key the medium holds %d objects (%v), want exactly 1", len(objects), keysOfObjects(objects))
	}

	rc, err := store.OpenObject(ctx, medium, key)
	if err != nil {
		t.Fatalf("OpenObject: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading the object back: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("after a second upload the object reads back as %q, want %q", got, content)
	}
}

func testMediumChecksum(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	content := []byte("bytes whose digest somebody may or may not be able to attest")
	local := putLocal(t, "artifact.dump", content)
	const key = "production/pg/artifact.dump"
	if _, err := store.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}

	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	attestation, err := store.ObjectChecksum(ctx, medium, key, transport.SHA256)
	if fx.AttestsSHA256() {
		if err != nil {
			t.Fatalf("ObjectChecksum: the fixture says this backend attests SHA-256, and it returned %v", err)
		}
		if attestation.Algorithm != transport.SHA256 {
			t.Errorf("ObjectChecksum attested algorithm %q, want %q", attestation.Algorithm, transport.SHA256)
		}
		if attestation.Value != want {
			t.Errorf("ObjectChecksum attested %q, want %q", attestation.Value, want)
		}
		return
	}

	// The refusal branch, and the one FR-31 exists for. A backend that
	// cannot attest must say so in a way a caller can act on, and must
	// not hand back a value at all: an empty-but-nil-error answer is how
	// a weaker check gets recorded under a stronger name.
	if err == nil {
		t.Fatalf("ObjectChecksum returned %+v with no error, but this backend cannot attest a full-object SHA-256; FR-31 says that is an explicit capability refusal, never a silent downgrade", attestation)
	}
	category, ok := transport.CategoryOf(err)
	if !ok {
		t.Fatalf("ObjectChecksum's refusal (%v) carries no transport category, so a caller cannot tell a capability absence from an outage", err)
	}
	if category != transport.UnsupportedCapability {
		t.Errorf("ObjectChecksum's refusal classified as %s, want %s", category, transport.UnsupportedCapability)
	}
	if attestation != (transport.ChecksumAttestation{}) {
		t.Errorf("ObjectChecksum returned attestation %+v alongside its refusal; a refused attestation must be empty so a caller cannot use it by accident", attestation)
	}
}

// testMediumChecksumAlgorithm pins the other half of FR-32's "an ETag is
// never a content hash": this boundary speaks exactly one algorithm, so
// there is no way to ask a medium for the MD5 an S3 ETag would hand back.
func testMediumChecksumAlgorithm(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	local := putLocal(t, "artifact.dump", []byte("anything"))
	const key = "production/pg/artifact.dump"
	if _, err := store.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}

	attestation, err := store.ObjectChecksum(ctx, medium, key, transport.HashAlgorithm("md5"))
	if err == nil {
		t.Fatalf("ObjectChecksum answered an md5 request with %+v; the recorded hash is a SHA-256 and an MD5 from an ETag is exactly what FR-32 forbids comparing to it", attestation)
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.UnsupportedCapability {
		t.Errorf("ObjectChecksum's md5 refusal classified as %v (recognised=%v), want %s", category, ok, transport.UnsupportedCapability)
	}
}

func testMediumList(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	want := map[string]int64{
		"production/pg/one.dump":    5,
		"production/pg/two.dump":    7,
		"staging/pg/elsewhere.dump": 3,
	}
	for key, size := range want {
		local := putLocal(t, filepath.Base(key), bytes.Repeat([]byte("x"), int(size)))
		if _, err := store.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
			t.Fatalf("UploadFromLocal(%s): %v", key, err)
		}
	}

	got, err := store.ListObjects(ctx, medium, "production")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	bySize := make(map[string]int64, len(got))
	for _, o := range got {
		bySize[o.Key] = o.Size
	}
	for _, key := range []string{"production/pg/one.dump", "production/pg/two.dump"} {
		size, ok := bySize[key]
		if !ok {
			t.Errorf("ListObjects(%q) did not report %q; it reported %v", "production", key, keysOfObjects(got))
			continue
		}
		if size != want[key] {
			t.Errorf("ListObjects reported %q with size %d, want %d", key, size, want[key])
		}
	}
	if _, leaked := bySize["staging/pg/elsewhere.dump"]; leaked {
		t.Errorf("ListObjects(%q) reported an object outside that prefix; it reported %v", "production", keysOfObjects(got))
	}

	empty, err := store.ListObjects(ctx, medium, "nothing-is-here")
	if err != nil {
		t.Fatalf("ListObjects over an empty prefix: %v, want an empty result and no error", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListObjects over an empty prefix reported %v", keysOfObjects(empty))
	}
}

func testMediumNotFound(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	const key = "production/pg/never-uploaded.dump"

	if info, err := store.StatObject(ctx, medium, key); err == nil {
		t.Errorf("StatObject on an absent key returned %+v, want a refusal", info)
	} else if category, ok := transport.CategoryOf(err); !ok || category != transport.NotFound {
		t.Errorf("StatObject on an absent key classified as %v (recognised=%v), want %s", category, ok, transport.NotFound)
	}

	if rc, err := store.OpenObject(ctx, medium, key); err == nil {
		rc.Close()
		t.Error("OpenObject on an absent key returned a reader, want a refusal")
	} else if category, ok := transport.CategoryOf(err); !ok || category != transport.NotFound {
		t.Errorf("OpenObject on an absent key classified as %v (recognised=%v), want %s", category, ok, transport.NotFound)
	}
}

func testMediumDelete(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	const doomed = "production/pg/doomed.dump"
	const sibling = "production/pg/sibling.dump"
	for _, key := range []string{doomed, sibling} {
		local := putLocal(t, filepath.Base(key), []byte(key))
		if _, err := store.UploadFromLocal(ctx, medium, local, key, transport.UploadOptions{}); err != nil {
			t.Fatalf("UploadFromLocal(%s): %v", key, err)
		}
	}

	if err := store.DeleteObject(ctx, medium, doomed); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if info, err := store.StatObject(ctx, medium, doomed); err == nil {
		t.Errorf("StatObject still reports the deleted key: %+v", info)
	} else if category, _ := transport.CategoryOf(err); category != transport.NotFound {
		t.Errorf("StatObject on the deleted key classified as %s, want %s", category, transport.NotFound)
	}

	if _, err := store.StatObject(ctx, medium, sibling); err != nil {
		t.Errorf("deleting %q also removed %q: %v; DeleteObject deletes exactly the key it was given", doomed, sibling, err)
	}
}

func testMediumDeleteAbsent(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	if err := store.DeleteObject(ctx, medium, "production/pg/was-never-there.dump"); err != nil {
		t.Errorf("DeleteObject on an absent key returned %v; the caller's intent, that these bytes not be on this medium, is already satisfied", err)
	}
}

func testMediumCancel(t *testing.T, store transport.MediumStore, fx MediumFixtures) {
	medium := fx.NewMedium(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	local := putLocal(t, "artifact.dump", []byte("never uploaded"))
	_, err := store.UploadFromLocal(ctx, medium, local, "production/pg/artifact.dump", transport.UploadOptions{})
	if err == nil {
		t.Fatal("UploadFromLocal under a cancelled context succeeded, want a refusal")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("UploadFromLocal's cancellation error does not unwrap to context.Canceled: %v", err)
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.Cancelled {
		t.Errorf("UploadFromLocal's cancellation classified as %v (recognised=%v), want %s", category, ok, transport.Cancelled)
	}
}

func keysOfObjects(objects []transport.ObjectInfo) []string {
	out := make([]string, 0, len(objects))
	for _, o := range objects {
		out = append(out, o.Key)
	}
	return out
}
