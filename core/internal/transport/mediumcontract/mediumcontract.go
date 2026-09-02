// Package mediumcontract is the reusable transport.MediumStore contract
// suite, the FR-28 counterpart to internal/transport/contract.
//
// It exercises any MediumStore implementation against a Fixtures factory
// that knows how to stand up one concrete medium, so a second medium type,
// or an rclone upgrade, or a non-rclone implementation entirely, runs the
// identical assertions without anyone rewriting them.
//
// # What this suite is for, and what it deliberately is not
//
// It is for the OBLIGATIONS the interface states, in the words the
// interface states them. UploadFromLocal must refuse an occupied key.
// StatObject must distinguish "the medium answered and it is not there"
// from "the medium could not be asked". DeleteObject must delete exactly
// one object and treat an absent one as success. ObjectChecksum must either
// attest honestly or refuse explicitly, never weaken silently. Those are
// the properties an implementation can get wrong in a way that costs an
// artifact, and every one of them is asserted here rather than described.
//
// It is NOT a place for S3 trivia. Nothing here knows what a bucket is, and
// a case that could only be written against S3 belongs in the S3 fixture's
// own tests, not in the shared suite: the moment this file learns about one
// backend it stops being evidence that the boundary is a boundary.
//
// # Two implementations run it, on purpose
//
// A contract suite with one implementation is a second copy of that
// implementation's tests. So this package ships its own reference
// implementation over the local filesystem (see filesystem.go), which runs
// on every gate with no container anywhere, and the real rclone s3 adapter
// runs the same suite against a MinIO container in
// core/tests/miniointegration. When those two disagree, one of them is
// wrong and the suite says which case.
package mediumcontract

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Fixtures builds what one MediumStore implementation needs in order to run
// Suite: an isolated medium to operate against, and an honest statement of
// the one capability that is allowed to be absent.
type Fixtures interface {
	// NewMedium returns a fresh, isolated transport.Medium for a single
	// test. Nothing written under a previous NewMedium call may be visible
	// through this one, and t.Cleanup should tear down whatever it
	// created.
	NewMedium(t *testing.T) transport.Medium

	// AttestsChecksums reports whether this backend can produce a
	// full-object SHA-256 attestation.
	//
	// It exists because the honest answer for S3 through rclone v1.75 is
	// no, and FR-31 says an absent attestation capability must surface as
	// an explicit capability result rather than as something weaker. A
	// fixture that answers false is asserting a refusal; one that answers
	// true is asserting a digest that actually matches the bytes. Either
	// way the case runs, which is the point: "we could not check" is not
	// one of the outcomes.
	AttestsChecksums() bool
}

// Run executes the MediumStore contract suite against store, using fx to
// build fixtures.
func Run(t *testing.T, store transport.MediumStore, fx Fixtures) {
	t.Run("upload_and_stat", func(t *testing.T) { testUploadAndStat(t, store, fx) })
	t.Run("upload_refuses_an_occupied_key", func(t *testing.T) { testUploadRefusesOccupied(t, store, fx) })
	t.Run("upload_refuses_a_missing_local_file", func(t *testing.T) { testUploadRefusesMissingLocal(t, store, fx) })
	t.Run("stat_absent_is_not_found", func(t *testing.T) { testStatAbsent(t, store, fx) })
	t.Run("open_round_trips_the_bytes", func(t *testing.T) { testOpenRoundTrip(t, store, fx) })
	t.Run("open_absent_is_not_found", func(t *testing.T) { testOpenAbsent(t, store, fx) })
	t.Run("delete_removes_exactly_one_object", func(t *testing.T) { testDeleteIsNarrow(t, store, fx) })
	t.Run("delete_absent_is_success", func(t *testing.T) { testDeleteAbsent(t, store, fx) })
	t.Run("list_objects", func(t *testing.T) { testListObjects(t, store, fx) })
	t.Run("list_of_an_empty_prefix_is_empty_not_an_error", func(t *testing.T) { testListEmpty(t, store, fx) })
	t.Run("checksum_attests_or_refuses", func(t *testing.T) { testChecksum(t, store, fx) })
	t.Run("cancel", func(t *testing.T) { testCancel(t, store, fx) })
}

// --- the cases ---

func testUploadAndStat(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)
	body := randomBytes(t, 4096)
	local := writeLocal(t, "backup.dump", body)
	key := keyFor(t, medium, "backup.dump")

	result, err := store.UploadFromLocal(ctx, medium, local, key)
	if err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}
	if result.BytesUploaded != int64(len(body)) {
		t.Errorf("UploadResult.BytesUploaded = %d, want %d; the size must be read off the STORED object, so an endpoint that accepted a truncated upload is visible here rather than three steps later",
			result.BytesUploaded, len(body))
	}

	info, err := store.StatObject(ctx, medium, key)
	if err != nil {
		t.Fatalf("StatObject after a successful upload: %v", err)
	}
	if info.Key != key {
		t.Errorf("ObjectInfo.Key = %q, want %q", info.Key, key)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("ObjectInfo.Size = %d, want %d", info.Size, len(body))
	}
}

// testUploadRefusesOccupied is the obligation with the sharpest teeth. An
// upload that replaced what was there could destroy an artifact's only
// remaining copy, and no other method on this interface can do that.
func testUploadRefusesOccupied(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)
	first := randomBytes(t, 1024)
	second := randomBytes(t, 2048)
	key := keyFor(t, medium, "backup.dump")

	if _, err := store.UploadFromLocal(ctx, medium, writeLocal(t, "first.dump", first), key); err != nil {
		t.Fatalf("first UploadFromLocal: %v", err)
	}

	_, err := store.UploadFromLocal(ctx, medium, writeLocal(t, "second.dump", second), key)
	if err == nil {
		t.Fatal("UploadFromLocal replaced an object that was already at this key; overwriting an artifact is not recoverable and is a case a person decides")
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.Conflict {
		t.Errorf("refusing an occupied key gave category %v (classified: %t), want %s; the caller's next move is confirm-then-continue, which it can only pick if the category says resumable",
			category, ok, transport.Conflict)
	}

	// And the refusal has to leave the original intact. A refusal that
	// truncated first would be worse than the overwrite it was avoiding.
	rc, err := store.OpenObject(ctx, medium, key)
	if err != nil {
		t.Fatalf("OpenObject after a refused overwrite: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading back after a refused overwrite: %v", err)
	}
	if !bytes.Equal(got, first) {
		t.Error("the object changed even though the second upload was refused")
	}
}

func testUploadRefusesMissingLocal(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)
	missing := filepath.Join(t.TempDir(), "not-there.dump")
	if _, err := store.UploadFromLocal(ctx, medium, missing, keyFor(t, medium, "not-there.dump")); err == nil {
		t.Fatal("UploadFromLocal accepted a local path with no file at it")
	}
}

// testStatAbsent pins the distinction artifactstore.ErrNotPresent's doc
// calls out: "the medium answered, and the object is not there" and "the
// medium could not be reached to ask" are different facts, and a mover that
// confuses them can delete an origin copy on the strength of a network
// failure.
func testStatAbsent(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	info, err := store.StatObject(ctx, medium, keyFor(t, medium, "never-uploaded.dump"))
	if err == nil {
		t.Fatalf("StatObject reported %+v for a key nothing was ever written to", info)
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.NotFound {
		t.Errorf("StatObject of an absent object gave category %v (classified: %t), want %s", category, ok, transport.NotFound)
	}
	if info != (transport.ObjectInfo{}) {
		t.Errorf("StatObject returned %+v alongside its error; a refused stat must carry no data a caller could use by accident", info)
	}
}

func testOpenRoundTrip(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)
	// Deliberately bigger than a single read, so a partial implementation
	// that returns the first chunk is caught.
	body := randomBytes(t, 1<<20)
	key := keyFor(t, medium, "large.dump")

	if _, err := store.UploadFromLocal(ctx, medium, writeLocal(t, "large.dump", body), key); err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}
	rc, err := store.OpenObject(ctx, medium, key)
	if err != nil {
		t.Fatalf("OpenObject: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading the object: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("the bytes read back are not the bytes uploaded (%d bytes out, %d back)", len(body), len(got))
	}
}

func testOpenAbsent(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)
	rc, err := store.OpenObject(ctx, medium, keyFor(t, medium, "never-uploaded.dump"))
	if err == nil {
		rc.Close()
		t.Fatal("OpenObject opened a key nothing was ever written to")
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.NotFound {
		t.Errorf("OpenObject of an absent object gave category %v (classified: %t), want %s", category, ok, transport.NotFound)
	}
}

// testDeleteIsNarrow is the interface's "delete the one object key names
// and nothing else, never widen the target". The sibling keys share a
// prefix on purpose: a prefix delete, or a recursive one, takes them too.
func testDeleteIsNarrow(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	target := keyFor(t, medium, "2026-09-01.dump")
	siblings := []string{
		keyFor(t, medium, "2026-09-02.dump"),
		keyFor(t, medium, "2026-09-03.dump"),
	}
	for _, key := range append([]string{target}, siblings...) {
		if _, err := store.UploadFromLocal(ctx, medium, writeLocal(t, filepath.Base(key), randomBytes(t, 256)), key); err != nil {
			t.Fatalf("UploadFromLocal(%s): %v", key, err)
		}
	}

	if err := store.DeleteObject(ctx, medium, target); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, err := store.StatObject(ctx, medium, target); err == nil {
		t.Error("the object DeleteObject was asked to remove is still there")
	}
	for _, key := range siblings {
		if _, err := store.StatObject(ctx, medium, key); err != nil {
			t.Errorf("DeleteObject also removed %s, which it was not asked about: %v", key, err)
		}
	}
}

func testDeleteAbsent(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)
	if err := store.DeleteObject(ctx, medium, keyFor(t, medium, "never-uploaded.dump")); err != nil {
		t.Errorf("DeleteObject of an absent object returned %v; the caller's intent, that these bytes not be on this medium, is already true, and a re-run of an interrupted delete has to be safe", err)
	}
}

func testListObjects(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)

	want := []string{
		keyFor(t, medium, "2026-09-01.dump"),
		keyFor(t, medium, "2026-09-02.dump"),
		keyFor(t, medium, "2026-09-03.dump"),
	}
	for _, key := range want {
		if _, err := store.UploadFromLocal(ctx, medium, writeLocal(t, filepath.Base(key), randomBytes(t, 128)), key); err != nil {
			t.Fatalf("UploadFromLocal(%s): %v", key, err)
		}
	}

	got, err := store.ListObjects(ctx, medium, prefixFor(t, medium))
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListObjects returned %d objects, want %d: %+v", len(got), len(want), got)
	}
	for i, info := range got {
		if info.Key != want[i] {
			t.Errorf("ListObjects[%d].Key = %q, want %q (the listing must be ordered, or two runs over the same content disagree)", i, info.Key, want[i])
		}
		if info.Size != 128 {
			t.Errorf("ListObjects[%d].Size = %d, want 128", i, info.Size)
		}
	}
}

// testListEmpty is the case an object store gets wrong by inheriting a
// filesystem's idea of a directory. Nothing is stored under this prefix,
// which is an ordinary answer on the first upload to a new backup set, not
// a missing thing.
func testListEmpty(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)
	got, err := store.ListObjects(ctx, medium, prefixFor(t, medium))
	if err != nil {
		t.Fatalf("ListObjects over a prefix with nothing under it returned %v; a first upload to a new backup set would look like a broken medium", err)
	}
	if len(got) != 0 {
		t.Errorf("ListObjects returned %d objects for an empty prefix: %+v", len(got), got)
	}
}

// testChecksum is FR-31's honesty rule as a test. There are exactly two
// acceptable outcomes and "something weaker, quietly" is not one of them.
func testChecksum(t *testing.T, store transport.MediumStore, fx Fixtures) {
	ctx := context.Background()
	medium := fx.NewMedium(t)
	body := randomBytes(t, 8192)
	key := keyFor(t, medium, "attested.dump")
	if _, err := store.UploadFromLocal(ctx, medium, writeLocal(t, "attested.dump", body), key); err != nil {
		t.Fatalf("UploadFromLocal: %v", err)
	}

	attestation, err := store.ObjectChecksum(ctx, medium, key, transport.SHA256)

	if !fx.AttestsChecksums() {
		if err == nil {
			t.Fatalf("this fixture says it cannot attest checksums, but ObjectChecksum returned %+v; a capability that appears out of nowhere is worse news than one that is absent", attestation)
		}
		category, ok := transport.CategoryOf(err)
		if !ok || category != transport.UnsupportedCapability {
			t.Errorf("an absent attestation capability gave category %v (classified: %t), want %s; FR-13 says an unsupported verification is an explicit capability result, never a silent weakening",
				category, ok, transport.UnsupportedCapability)
		}
		if attestation != (transport.ChecksumAttestation{}) {
			t.Errorf("ObjectChecksum returned %+v alongside its refusal; an empty digest compared against a recorded one reads as corruption, which is the wrong verdict for an absent capability", attestation)
		}
		return
	}

	if err != nil {
		t.Fatalf("this fixture says it attests checksums, but ObjectChecksum failed: %v", err)
	}
	if attestation.Algorithm != transport.SHA256 {
		t.Errorf("ChecksumAttestation.Algorithm = %q, want %q; comparing digests across algorithms is how a mismatch reads as corruption", attestation.Algorithm, transport.SHA256)
	}
	sum := sha256.Sum256(body)
	if want := hex.EncodeToString(sum[:]); attestation.Value != want {
		t.Errorf("ChecksumAttestation.Value = %q, want %q (the real digest of the bytes uploaded)", attestation.Value, want)
	}
}

// testCancel pins FR-22's "cancellation SHALL propagate through Go
// contexts" for this interface. An already-cancelled context is used rather
// than a race against a live transfer, because the assertion is about the
// category, not about timing.
func testCancel(t *testing.T, store transport.MediumStore, fx Fixtures) {
	medium := fx.NewMedium(t)
	body := randomBytes(t, 4096)
	local := writeLocal(t, "cancelled.dump", body)
	key := keyFor(t, medium, "cancelled.dump")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.UploadFromLocal(ctx, medium, local, key)
	if err == nil {
		t.Fatal("UploadFromLocal succeeded under an already-cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the error from a cancelled upload does not unwrap to context.Canceled: %v", err)
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.Cancelled {
		t.Errorf("a cancelled upload gave category %v (classified: %t), want %s; cancellation is a decision the caller already made, not a judgement about the failure",
			category, ok, transport.Cancelled)
	}
}

// --- helpers ---

// keyFor builds the key for name under medium, through the same
// transport.MediumKey every production caller uses. The suite deliberately
// does not compose keys itself: a contract suite that spelled its own
// layout would keep passing while the product wrote somewhere else.
func keyFor(t *testing.T, medium transport.Medium, name string) string {
	t.Helper()
	set, err := model.NewBackupSetID("contract-source", "contract-set")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%q): %v", name, err)
	}
	key, err := transport.MediumKey(medium.Prefix, artifact)
	if err != nil {
		t.Fatalf("MediumKey: %v", err)
	}
	return key
}

// prefixFor is keyFor's containing namespace: everything the suite writes
// for one medium sits under it, so a listing can be asserted exactly.
func prefixFor(t *testing.T, medium transport.Medium) string {
	t.Helper()
	key := keyFor(t, medium, "placeholder")
	return key[:len(key)-len("/placeholder")]
}

func writeLocal(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}
