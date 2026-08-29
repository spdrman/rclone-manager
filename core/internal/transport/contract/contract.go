package contract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Fixtures builds everything one transport.Transport implementation needs in
// order to run Suite: an isolated Source to operate against, and the ability
// to place, replace and restrict objects within it. Suite itself knows
// nothing backend-specific; a Fixtures implementation is where that knowledge
// lives, one per backend (local disk, an SFTP server, and so on).
type Fixtures interface {
	// NewSource returns a fresh, isolated transport.Source for a single test.
	// Nothing written under a previous NewSource call may be visible through
	// this one, and t.Cleanup should tear down whatever it created.
	NewSource(t *testing.T) transport.Source

	// Put writes content at remotePath under source, creating parent
	// directories as needed and overwriting remotePath if it already exists.
	Put(t *testing.T, source transport.Source, remotePath string, content []byte)

	// Deny arranges for remotePath (already Put) to exist but be unreadable
	// to the transport under test, and returns a cleanup func that restores
	// access. Deny should call t.Skip if the fixture cannot express
	// permission denial in the current environment (for example, running as
	// root defeats a Unix chmod).
	Deny(t *testing.T, source transport.Source, remotePath string) (cleanup func())

	// SupportedHash returns a hash algorithm the fixture's backend can
	// compute for objects it creates.
	SupportedHash() transport.HashAlgorithm

	// UnsupportedHash returns a hash algorithm identifier the backend is
	// guaranteed not to be able to compute.
	UnsupportedHash() transport.HashAlgorithm
}

// ModTimeSetter is an optional Fixtures capability. When a Fixtures value
// also implements it, the changed-object-detection case additionally proves
// detection holds even when path, size and modification time are all
// identical between the discovered and the replacement object, and only the
// content differs, which a size/modtime-only comparison cannot see.
type ModTimeSetter interface {
	SetModTime(t *testing.T, source transport.Source, remotePath string, modTime int64)
}

// Run executes the transport contract suite against tr, using fx to build
// fixtures. Every subtest is independent and may run in parallel with a
// future backend's Run call, but subtests within one Run share nothing across
// each other's NewSource-provided Source.
func Run(t *testing.T, tr transport.Transport, fx Fixtures) {
	t.Run("list", func(t *testing.T) { testList(t, tr, fx) })
	t.Run("stat", func(t *testing.T) { testStat(t, tr, fx) })
	t.Run("copy_to_local", func(t *testing.T) { testCopyToLocal(t, tr, fx) })
	t.Run("hash_capability", func(t *testing.T) { testHashCapability(t, tr, fx) })
	t.Run("delete", func(t *testing.T) { testDelete(t, tr, fx) })
	t.Run("cancel", func(t *testing.T) { testCancel(t, tr, fx) })
	t.Run("not_found", func(t *testing.T) { testNotFound(t, tr, fx) })
	t.Run("permission_denied", func(t *testing.T) { testPermissionDenied(t, tr, fx) })
	t.Run("changed_object_detection", func(t *testing.T) { testChangedObjectDetection(t, tr, fx) })
}

func testList(t *testing.T, tr transport.Transport, fx Fixtures) {
	ctx := context.Background()
	source := fx.NewSource(t)

	// Flat files only: the transport.Transport interface documents no
	// recursion guarantee for List, and the rclone adapter's current
	// implementation does not recurse into subdirectories (see
	// TestRcloneAdapter_List_DoesNotRecurseIntoSubdirectories in
	// transport_test.go for that discovered gap). A generic contract that
	// every future backend must satisfy should not bake in behavior the
	// interface itself does not promise.
	want := map[string]int64{
		"one.txt": 5,
		"two.txt": 7,
	}
	for path, size := range want {
		fx.Put(t, source, path, bytes.Repeat([]byte("x"), int(size)))
	}

	got, err := tr.List(ctx, source)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	gotByPath := make(map[string]transport.RemoteArtifact, len(got))
	for _, a := range got {
		gotByPath[a.Path] = a
	}
	for path, size := range want {
		a, ok := gotByPath[path]
		if !ok {
			t.Errorf("List did not report %q; got paths %v", path, keysOf(gotByPath))
			continue
		}
		if a.Size != size {
			t.Errorf("List reported %q with size %d, want %d", path, a.Size, size)
		}
	}
}

func testStat(t *testing.T, tr transport.Transport, fx Fixtures) {
	ctx := context.Background()
	source := fx.NewSource(t)

	content := []byte("hello world")
	before := time.Now().Add(-time.Minute).Unix()
	fx.Put(t, source, "file.txt", content)

	a, err := tr.Stat(ctx, source, "file.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if a.Path != "file.txt" {
		t.Errorf("Stat Path = %q, want %q", a.Path, "file.txt")
	}
	if a.Size != int64(len(content)) {
		t.Errorf("Stat Size = %d, want %d", a.Size, len(content))
	}
	after := time.Now().Add(time.Minute).Unix()
	if a.ModTime != 0 && (a.ModTime < before || a.ModTime > after) {
		t.Errorf("Stat ModTime = %d, want between %d and %d", a.ModTime, before, after)
	}
}

func testCopyToLocal(t *testing.T, tr transport.Transport, fx Fixtures) {
	ctx := context.Background()
	source := fx.NewSource(t)

	content := bytes.Repeat([]byte("payload-"), 512) // 4096 bytes
	fx.Put(t, source, "artifact.bin", content)

	dest := filepath.Join(t.TempDir(), "artifact.bin.partial")
	result, err := tr.CopyToLocal(ctx, source, "artifact.bin", dest)
	if err != nil {
		t.Fatalf("CopyToLocal: %v", err)
	}
	if result.BytesTransferred != int64(len(content)) {
		t.Errorf("BytesTransferred = %d, want %d", result.BytesTransferred, len(content))
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading copied file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("copied content does not match source (got %d bytes, want %d)", len(got), len(content))
	}
}

// testHashCapability is the capability-reporting case: an unsupported remote
// hash must produce an explicit error, never a silent downgrade of configured
// verification (a nil error with an empty/zero hash would let a caller
// mistake "cannot verify" for "verified"). It proves the difference by
// exercising both a supported and an unsupported algorithm against the same
// object in the same test.
func testHashCapability(t *testing.T, tr transport.Transport, fx Fixtures) {
	ctx := context.Background()
	source := fx.NewSource(t)

	content := []byte("hash me please, this is the fixture content")
	fx.Put(t, source, "hashed.txt", content)

	wantHash := sha256Hex(content)
	supported := fx.SupportedHash()
	unsupported := fx.UnsupportedHash()
	if supported == unsupported {
		t.Fatalf("fixture bug: SupportedHash and UnsupportedHash are both %q", supported)
	}

	t.Run("supported_hash_succeeds_with_a_real_value", func(t *testing.T) {
		got, err := tr.RemoteHash(ctx, source, "hashed.txt", supported)
		if err != nil {
			t.Fatalf("RemoteHash(%s): %v", supported, err)
		}
		if got == "" {
			t.Fatalf("RemoteHash(%s) returned an empty hash with no error; a caller could mistake this for a verified match", supported)
		}
		if supported == transport.SHA256 && got != wantHash {
			t.Errorf("RemoteHash(sha256) = %q, want %q (independently computed)", got, wantHash)
		}
	})

	t.Run("unsupported_hash_fails_explicitly", func(t *testing.T) {
		got, err := tr.RemoteHash(ctx, source, "hashed.txt", unsupported)
		if err == nil {
			t.Fatalf("RemoteHash(%s) succeeded with %q; an unsupported algorithm must return an explicit error rather than silently downgrading verification", unsupported, got)
		}
		if got != "" {
			t.Errorf("RemoteHash(%s) returned a non-empty hash (%q) alongside an error", unsupported, got)
		}
	})
}

func testDelete(t *testing.T, tr transport.Transport, fx Fixtures) {
	ctx := context.Background()
	source := fx.NewSource(t)

	fx.Put(t, source, "to-delete.txt", []byte("bye"))

	if err := tr.DeleteRemote(ctx, source, "to-delete.txt"); err != nil {
		t.Fatalf("DeleteRemote: %v", err)
	}

	if _, err := tr.Stat(ctx, source, "to-delete.txt"); err == nil {
		t.Fatalf("Stat succeeded after DeleteRemote; object should be gone")
	}

	entries, err := tr.List(ctx, source)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	for _, a := range entries {
		if a.Path == "to-delete.txt" {
			t.Fatalf("List still reports %q after DeleteRemote", a.Path)
		}
	}
}

// testCancel proves cancellation propagates through Go contexts (FR-22): a
// context that is already cancelled before the call starts must cause the
// operation to fail rather than complete as if nothing were wrong.
func testCancel(t *testing.T, tr transport.Transport, fx Fixtures) {
	source := fx.NewSource(t)
	content := bytes.Repeat([]byte("cancel-me-"), 4096)
	fx.Put(t, source, "big.bin", content)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dest := filepath.Join(t.TempDir(), "big.bin.partial")
	_, err := tr.CopyToLocal(ctx, source, "big.bin", dest)
	if err == nil {
		t.Fatalf("CopyToLocal succeeded against an already-cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CopyToLocal error = %v, want it to satisfy errors.Is(err, context.Canceled)", err)
	}
}

func testNotFound(t *testing.T, tr transport.Transport, fx Fixtures) {
	ctx := context.Background()
	source := fx.NewSource(t)

	const missing = "does-not-exist.txt"

	if _, err := tr.Stat(ctx, source, missing); err == nil {
		t.Errorf("Stat(%q) succeeded, want a not-found error", missing)
	}
	if _, err := tr.CopyToLocal(ctx, source, missing, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Errorf("CopyToLocal(%q) succeeded, want a not-found error", missing)
	}
	if _, err := tr.RemoteHash(ctx, source, missing, fx.SupportedHash()); err == nil {
		t.Errorf("RemoteHash(%q) succeeded, want a not-found error", missing)
	}
	if err := tr.DeleteRemote(ctx, source, missing); err == nil {
		t.Errorf("DeleteRemote(%q) succeeded, want a not-found error", missing)
	}
}

func testPermissionDenied(t *testing.T, tr transport.Transport, fx Fixtures) {
	ctx := context.Background()
	source := fx.NewSource(t)

	fx.Put(t, source, "secret.txt", []byte("shh"))
	cleanup := fx.Deny(t, source, "secret.txt")
	defer cleanup()

	if _, err := tr.CopyToLocal(ctx, source, "secret.txt", filepath.Join(t.TempDir(), "out")); err == nil {
		t.Errorf("CopyToLocal succeeded reading a denied object, want a permission error")
	}
	if _, err := tr.RemoteHash(ctx, source, "secret.txt", fx.SupportedHash()); err == nil {
		t.Errorf("RemoteHash succeeded reading a denied object, want a permission error")
	}
}

// testChangedObjectDetection is the FR-16/FR-15 proof: the manager persists
// remote identity at discovery and compares it again immediately before
// deletion, so a remote file replaced under a reused pathname is refused
// rather than destroyed. It must catch that replacement even in the awkward
// case where the replacement has the same size (and, where the fixture can
// arrange it, the same modification time) as the original.
func testChangedObjectDetection(t *testing.T, tr transport.Transport, fx Fixtures) {
	ctx := context.Background()
	alg := fx.SupportedHash()

	t.Run("unchanged_object_is_not_flagged", func(t *testing.T) {
		source := fx.NewSource(t)
		fx.Put(t, source, "steady.bin", []byte("same content, never touched"))

		discovered, err := Capture(ctx, tr, source, "steady.bin", alg)
		if err != nil {
			t.Fatalf("Capture (discovery): %v", err)
		}
		current, err := Capture(ctx, tr, source, "steady.bin", alg)
		if err != nil {
			t.Fatalf("Capture (pre-delete recheck): %v", err)
		}

		changed, confident := Changed(discovered, current)
		if changed || !confident {
			t.Fatalf("Changed() = (changed=%v, confident=%v) for an untouched object, want (false, true)", changed, confident)
		}
	})

	t.Run("different_size_replacement_is_caught", func(t *testing.T) {
		source := fx.NewSource(t)
		fx.Put(t, source, "grows.bin", []byte("short"))

		discovered, err := Capture(ctx, tr, source, "grows.bin", alg)
		if err != nil {
			t.Fatalf("Capture (discovery): %v", err)
		}

		fx.Put(t, source, "grows.bin", []byte("this replacement is much longer than the original"))

		current, err := Capture(ctx, tr, source, "grows.bin", alg)
		if err != nil {
			t.Fatalf("Capture (pre-delete recheck): %v", err)
		}

		changed, confident := Changed(discovered, current)
		if !changed || !confident {
			t.Fatalf("Changed() = (changed=%v, confident=%v) for a differently-sized replacement, want (true, true)", changed, confident)
		}
	})

	t.Run("same_size_same_name_different_content_is_caught", func(t *testing.T) {
		source := fx.NewSource(t)

		// Same length on both sides, deliberately: a comparison that only
		// looked at size (or size and a coarse modtime) would see no
		// difference at all here. Only the content changes.
		original := []byte("original-32-byte-payload-------")
		replacement := []byte("replaced-32-byte-payload-------")
		if len(original) != len(replacement) {
			t.Fatalf("fixture bug: original and replacement are not the same length (%d vs %d)", len(original), len(replacement))
		}

		fx.Put(t, source, "reused-name.bin", original)

		discovered, err := Capture(ctx, tr, source, "reused-name.bin", alg)
		if err != nil {
			t.Fatalf("Capture (discovery): %v", err)
		}

		fx.Put(t, source, "reused-name.bin", replacement)

		// When the fixture can pin the modification time, force it equal to
		// the discovered one too, so this proves detection survives even
		// when path, size AND modtime all agree and only the hash differs.
		if setter, ok := fx.(ModTimeSetter); ok && discovered.Artifact.ModTime != 0 {
			setter.SetModTime(t, source, "reused-name.bin", discovered.Artifact.ModTime)
		}

		current, err := Capture(ctx, tr, source, "reused-name.bin", alg)
		if err != nil {
			t.Fatalf("Capture (pre-delete recheck): %v", err)
		}

		// Sanity/positive-control check: prove this really is the awkward
		// case (size unchanged) and not an accidental easy case. If this
		// fails, the test below would pass for the wrong reason.
		if discovered.Artifact.Size != current.Artifact.Size {
			t.Fatalf("fixture bug: expected identical size, discovered=%d current=%d", discovered.Artifact.Size, current.Artifact.Size)
		}
		if !discovered.HasHash || !current.HasHash {
			t.Skipf("backend fixture did not produce a hash for both captures (discovered=%v current=%v); cannot prove hash-based detection of a same-size replacement", discovered.HasHash, current.HasHash)
		}
		if discovered.Artifact.Hash == current.Artifact.Hash {
			t.Fatalf("fixture bug: discovered and replacement hashed identically; the fixture did not actually change the content")
		}

		changed, confident := Changed(discovered, current)
		if !changed || !confident {
			t.Fatalf("Changed() = (changed=%v, confident=%v) for a same-size, same-name content replacement, want (true, true)", changed, confident)
		}
	})
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func keysOf(m map[string]transport.RemoteArtifact) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
