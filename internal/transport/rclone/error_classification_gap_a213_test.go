// This file documents a real defect A2.13 found while building the crash
// matrix and destructive-safety suites for issue #31: Adapter never calls
// this package's own Classify/Wrap (errors.go) on anything it returns. See
// the PR description ("Defect: the adapter never classifies its own
// errors") for the full writeup, the recommended fix, and why fixing
// adapter.go itself is out of this PR's file scope (it is production code
// outside tests/ and outside the "new _test.go" allowance).
//
// errors.go and errors_test.go are thorough and correct in isolation:
// Classify(err) correctly turns a real rclone.ErrorObjectNotFound,
// os.ErrPermission, a knownhosts.KeyError, "unable to authenticate", and
// this package's own ErrUnsupportedHash into the right transport.Category.
// The gap is that nothing in Adapter's five methods (List, Stat,
// CopyToLocal, RemoteHash, DeleteRemote) ever calls Wrap on what it
// returns; every one of them hands back the underlying rclone/os/ssh error
// completely unclassified. transport.CategoryOf walks an error's chain for
// a *transport.Error (the type Wrap/NewError produce); an error that never
// passed through Wrap can never satisfy that, no matter what it wraps
// underneath, so CategoryOf always reports (Unclassified, false) for
// anything the real Adapter returns today.
//
// This matters beyond cosmetics: transport/retry.DefaultIsTransient and
// internal/reconcile's statRemote (the FR-17 "is the remote object
// confirmed absent" check) both branch on transport.CategoryOf. Driven
// through the real Adapter, DefaultIsTransient can never see Transient
// (so a genuinely transient network blip during a real transfer is never
// retried, contrary to FR-22) and statRemote can never see NotFound (so
// reconciliation can never conclude "the remote is confirmed gone" and
// close an artifact out to COMPLETE, contrary to FR-17). See
// internal/reconcile's own new test in this PR
// (TestReconcile_RealAdapter_CannotConvergeRemoteDeletePendingToComplete_KnownDefect)
// for the second half of this proof: an artifact stuck at
// REMOTE_DELETE_PENDING forever even though its remote object is
// genuinely, confirmably gone.
//
// It fails safe, not unsafe: reconcileDeletePending never calls
// DeleteRemote itself (it only ever advances toward COMPLETE or
// QUARANTINED, or leaves the row where reconciliation found it), so this
// bug cannot cause an unauthorized deletion. What it breaks is
// convergence: "no artifact may be stuck with no way forward" is exactly
// the safety property docs/EPIC.md's crash matrix asks this issue to
// prove, and against the real adapter, an artifact whose remote copy was
// actually deleted just before a crash cannot reconcile forward on its
// own.
package rclone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	rclonefs "github.com/rclone/rclone/fs"

	"github.com/spdrman/rclone-manager/internal/transport"
)

// TestAdapter_KnownDefect_NeverClassifiesItsOwnErrors documents, with firm
// assertions (not a soft log), that every one of Adapter's methods hands
// back an error transport.CategoryOf cannot classify, even for a case
// Classify itself handles correctly in isolation (errors_test.go proves
// that half already). If this ever starts failing because someone wired
// Wrap into adapter.go, that is the fix described in the PR body landing;
// update or remove this test as part of that change, and check the
// downstream tests this defect blocks (see this file's package doc) can be
// simplified once it does.
func TestAdapter_KnownDefect_NeverClassifiesItsOwnErrors(t *testing.T) {
	root := t.TempDir()
	a := New()
	src := transport.Source{ID: "known-defect-probe", Type: "local", Root: root}
	ctx := context.Background()

	assertUnclassified := func(t *testing.T, op string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected an error", op)
		}
		if _, ok := transport.CategoryOf(err); ok {
			t.Fatalf("%s: transport.CategoryOf now resolves a category for %v; "+
				"this test documents a known defect (adapter.go never calls Wrap) that "+
				"appears to be fixed now, see this file's package doc for what to update", op, err)
		}
	}

	t.Run("Stat_NotFound", func(t *testing.T) {
		_, err := a.Stat(ctx, src, "missing.txt")
		if !errors.Is(err, rclonefs.ErrorObjectNotFound) {
			t.Fatalf("Stat(missing) = %v, want it to still satisfy errors.Is(_, rclonefs.ErrorObjectNotFound) (Classify handles this correctly, see errors_test.go; only the wiring into Adapter is missing)", err)
		}
		assertUnclassified(t, "Stat", err)
	})

	t.Run("CopyToLocal_NotFound", func(t *testing.T) {
		_, err := a.CopyToLocal(ctx, src, "missing.txt", filepath.Join(root, "out.partial"))
		assertUnclassified(t, "CopyToLocal", err)
	})

	t.Run("DeleteRemote_NotFound", func(t *testing.T) {
		err := a.DeleteRemote(ctx, src, "missing.txt")
		assertUnclassified(t, "DeleteRemote", err)
	})

	t.Run("RemoteHash_UnsupportedAlgorithm", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "hashme.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, err := a.RemoteHash(ctx, src, "hashme.txt", transport.HashAlgorithm("crc32-not-a-real-choice"))
		if !errors.Is(err, ErrUnsupportedHash) {
			t.Fatalf("RemoteHash(unsupported alg) = %v, want it to still satisfy errors.Is(_, ErrUnsupportedHash)", err)
		}
		assertUnclassified(t, "RemoteHash", err)
	})

	if os.Geteuid() != 0 {
		t.Run("RemoteHash_PermissionDenied", func(t *testing.T) {
			denied := filepath.Join(root, "denied.txt")
			if err := os.WriteFile(denied, []byte("secret"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if err := os.Chmod(denied, 0o000); err != nil {
				t.Fatalf("Chmod: %v", err)
			}
			defer os.Chmod(denied, 0o644)

			_, err := a.RemoteHash(ctx, src, "denied.txt", transport.SHA256)
			assertUnclassified(t, "RemoteHash", err)
		})
	}
}
