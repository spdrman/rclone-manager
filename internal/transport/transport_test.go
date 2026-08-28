// This file drives the reusable transport contract suite (internal/transport/contract)
// against the embedded-rclone adapter's local backend, and carries the pure unit
// tests from docs/EPIC.md's "Unit" testing section that are in scope without the
// lifecycle/retention/state packages: rclone error translation shape and path
// safety. See the PR description for which EPIC unit tests are deferred and why.
package transport_test

import (
	"context"
	"errors"
	"fmt"
	stdfs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rclonefs "github.com/rclone/rclone/fs"

	"github.com/spdrman/rclone-manager/internal/transport"
	"github.com/spdrman/rclone-manager/internal/transport/contract"
	"github.com/spdrman/rclone-manager/internal/transport/rclone"
)

// TestRcloneAdapter_LocalBackend_ContractSuite is the deliverable this issue
// asks for: the rclone adapter's local backend, run through the full
// transport contract suite. It needs no network and no Docker, so it is the
// baseline every future backend (an rclone upgrade, an SFTP fixture from
// issue #2, or eventually a non-rclone transport) must also pass.
func TestRcloneAdapter_LocalBackend_ContractSuite(t *testing.T) {
	contract.Run(t, rclone.New(), localFixtures{})
}

// localFixtures implements contract.Fixtures over plain files on the local
// disk, which the rclone adapter's "local" backend reads directly. It
// implements contract.ModTimeSetter too, so the changed-object-detection case
// can pin an identical modification time on a replacement object and prove
// detection still catches it via hash alone.
type localFixtures struct{}

var _ contract.Fixtures = localFixtures{}
var _ contract.ModTimeSetter = localFixtures{}

var sourceCounter int64

func (localFixtures) NewSource(t *testing.T) transport.Source {
	root := t.TempDir()
	id := fmt.Sprintf("contract-local-%d", atomic.AddInt64(&sourceCounter, 1))
	return transport.Source{ID: id, Type: "local", Root: root}
}

func (localFixtures) Put(t *testing.T, source transport.Source, remotePath string, content []byte) {
	t.Helper()
	full := filepath.Join(source.Root, filepath.FromSlash(remotePath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", full, err)
	}
}

func (localFixtures) Deny(t *testing.T, source transport.Source, remotePath string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod-based permission denial is not enforced")
	}
	full := filepath.Join(source.Root, filepath.FromSlash(remotePath))
	if err := os.Chmod(full, 0o000); err != nil {
		t.Fatalf("Chmod(%q): %v", full, err)
	}
	return func() {
		_ = os.Chmod(full, 0o644)
	}
}

func (localFixtures) SupportedHash() transport.HashAlgorithm { return transport.SHA256 }

func (localFixtures) UnsupportedHash() transport.HashAlgorithm {
	return transport.HashAlgorithm("crc32-this-fixture-never-supports-it")
}

func (localFixtures) SetModTime(t *testing.T, source transport.Source, remotePath string, modTime int64) {
	t.Helper()
	full := filepath.Join(source.Root, filepath.FromSlash(remotePath))
	mt := time.Unix(modTime, 0)
	if err := os.Chtimes(full, mt, mt); err != nil {
		t.Fatalf("Chtimes(%q): %v", full, err)
	}
}

// Pure unit tests in scope now: rclone error translation shape.

// TestRcloneErrorTranslationShape_NotFound documents what actually crosses the
// transport boundary today for a missing object. FR-22 asks the adapter to
// translate rclone-specific errors into manager-owned categories (NotFound
// among them); no such translation exists yet (there is no errors.go, and
// adapter.go returns rclone's own error values directly). What does survive
// intact is rclone's fs.ErrorObjectNotFound sentinel, unwrapped, so a future
// FR-22 layer has something reliable to classify against. See the PR
// description for this gap.
func TestRcloneErrorTranslationShape_NotFound(t *testing.T) {
	ctx := context.Background()
	adapter := rclone.New()
	source := transport.Source{ID: "shape-notfound", Type: "local", Root: t.TempDir()}

	cases := map[string]func() error{
		"Stat": func() error {
			_, err := adapter.Stat(ctx, source, "missing.txt")
			return err
		},
		"CopyToLocal": func() error {
			_, err := adapter.CopyToLocal(ctx, source, "missing.txt", filepath.Join(t.TempDir(), "out"))
			return err
		},
		"RemoteHash": func() error {
			_, err := adapter.RemoteHash(ctx, source, "missing.txt", transport.SHA256)
			return err
		},
		"DeleteRemote": func() error {
			return adapter.DeleteRemote(ctx, source, "missing.txt")
		},
	}

	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatalf("%s succeeded against a missing object", name)
			}
			if !errors.Is(err, rclonefs.ErrorObjectNotFound) {
				t.Errorf("%s error does not satisfy errors.Is(err, rclonefs.ErrorObjectNotFound): %v (%T)", name, err, err)
			}
		})
	}
}

// TestRcloneErrorTranslationShape_PermissionDenied documents the same gap for
// a permission failure. Stat itself cannot observe this on a POSIX
// filesystem (os.Lstat needs no read permission on the file, only execute
// permission on its containing directories), so this exercises RemoteHash,
// which must actually open and read the file. The underlying *fs.PathError
// survives two layers of fmt.Errorf("...: %w", err) wrapping (rclone's local
// backend, then rclone's hash package), so errors.Is against the standard
// library's io/fs.ErrPermission still works even though no manager-owned
// PermissionDenied category exists yet.
func TestRcloneErrorTranslationShape_PermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission checks are not enforced")
	}

	ctx := context.Background()
	root := t.TempDir()
	source := transport.Source{ID: "shape-permdenied", Type: "local", Root: root}
	adapter := rclone.New()

	full := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(full, []byte("shh"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(full, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer func() { _ = os.Chmod(full, 0o644) }()

	_, err := adapter.RemoteHash(ctx, source, "secret.txt", transport.SHA256)
	if err == nil {
		t.Fatalf("RemoteHash succeeded reading a chmod 000 file")
	}
	if !errors.Is(err, stdfs.ErrPermission) {
		t.Errorf("RemoteHash error does not satisfy errors.Is(err, stdfs.ErrPermission): %v (%T)", err, err)
	}
}

// TestRcloneAdapter_List_DoesNotRecurseIntoSubdirectories documents an
// adapter gap the contract suite surfaced: Adapter.List calls f.List(ctx, "")
// once, which is a single-directory listing, not a recursive walk (rclone
// itself distinguishes the two, exposing recursive listing separately as
// walk.ListR / "rclone lsf -R"). An artifact placed in a subdirectory of a
// source's root is therefore invisible to List, silently, with no error.
//
// Whether that is actually wrong depends on whether any backup set's
// remote_path ever nests artifacts in subdirectories; FR-8 discovery does not
// rule that out. Reported in the PR description as an adapter bug rather than
// fixed here, since adapter.go is outside this issue's file scope.
func TestRcloneAdapter_List_DoesNotRecurseIntoSubdirectories(t *testing.T) {
	ctx := context.Background()
	adapter := rclone.New()
	root := t.TempDir()
	source := transport.Source{ID: "list-recursion-gap", Type: "local", Root: root}

	if err := os.MkdirAll(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "top-level.txt"), []byte("visible"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "subdir", "nested.txt"), []byte("invisible"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := adapter.List(ctx, source)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var sawTopLevel, sawNested bool
	for _, a := range got {
		switch a.Path {
		case "top-level.txt":
			sawTopLevel = true
		case "subdir/nested.txt":
			sawNested = true
		}
	}
	if !sawTopLevel {
		t.Errorf("List did not report the top-level file; test setup or List itself is broken beyond the known gap")
	}
	if sawNested {
		t.Fatalf("List reported the nested file: the recursion gap this test documents appears to be fixed, update the comment above and fold subdirectories back into the generic contract suite's list case")
	}

	// Stat and CopyToLocal, unlike List, do reach the nested object directly
	// by path: the gap is specific to enumeration, not to access.
	if _, err := adapter.Stat(ctx, source, "subdir/nested.txt"); err != nil {
		t.Errorf("Stat(\"subdir/nested.txt\") failed even though the file exists: %v", err)
	}
}

// Pure unit test in scope now: path safety.

// TestPathSafety_RemotePathTraversalIsRejected proves a remotePath cannot
// walk a source out of its configured Root. Remote filenames are untrusted
// input (FR-8) and path traversal must be rejected (Security Requirement 8);
// this is the transport-level half of that guarantee, independent of the
// local-deletion-root confinement FR-20 describes for retention (which needs
// the not-yet-built state/retention packages and is out of scope here).
func TestPathSafety_RemotePathTraversalIsRejected(t *testing.T) {
	ctx := context.Background()
	adapter := rclone.New()

	root := t.TempDir()
	outside := t.TempDir()

	outsideSecret := filepath.Join(outside, "secret.txt")
	const sentinelContent = "do not read, copy, hash or delete me"
	if err := os.WriteFile(outsideSecret, []byte(sentinelContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rel, err := filepath.Rel(root, outsideSecret)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	traversal := filepath.ToSlash(rel)
	if !strings.HasPrefix(traversal, "../") {
		t.Fatalf("test setup bug: expected a traversal path (starting with \"../\"), got %q", traversal)
	}

	source := transport.Source{ID: "path-safety", Type: "local", Root: root}

	if _, err := adapter.Stat(ctx, source, traversal); err == nil {
		t.Errorf("Stat(%q) succeeded; a remote path must not escape the source root", traversal)
	}

	dest := filepath.Join(t.TempDir(), "escaped.txt")
	if _, err := adapter.CopyToLocal(ctx, source, traversal, dest); err == nil {
		t.Errorf("CopyToLocal(%q) succeeded; a remote path must not escape the source root", traversal)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("a local file exists at %q despite the traversal attempt, meaning content crossed the root boundary", dest)
	}

	if _, err := adapter.RemoteHash(ctx, source, traversal, transport.SHA256); err == nil {
		t.Errorf("RemoteHash(%q) succeeded; a remote path must not escape the source root", traversal)
	}

	if err := adapter.DeleteRemote(ctx, source, traversal); err == nil {
		t.Errorf("DeleteRemote(%q) succeeded; a remote path must not escape the source root", traversal)
	}

	// Positive control: a legitimate in-root path must still work, so the
	// failures above are proof of confinement and not just everything erroring.
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := adapter.Stat(ctx, source, "inside.txt"); err != nil {
		t.Fatalf("Stat(\"inside.txt\") failed for a legitimate in-root path: %v", err)
	}

	// Final proof: the file outside the root was never touched, however the
	// calls above behaved.
	content, err := os.ReadFile(outsideSecret)
	if err != nil {
		t.Fatalf("sentinel file outside the root is gone, meaning DeleteRemote reached it: %v", err)
	}
	if string(content) != sentinelContent {
		t.Fatalf("sentinel file outside the root was modified: got %q, want %q", content, sentinelContent)
	}
}
