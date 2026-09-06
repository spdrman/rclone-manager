package rclone

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is two findings that arrived together, kept together because
// each is the other's counterweight.
//
// The first is that Stat has to carry a hash. FR-15's pre-delete recheck
// compares the identity captured at discovery against the one observed
// now, and model.CompareIdentity cannot reach ConfidenceStrong without a
// hash or a backend-supplied stable id. Stat returning only path, size and
// modification time did not weaken that check, it made a successful delete
// UNREACHABLE, on every backend, including ones that hash perfectly well.
//
// The second is that the fix is not "always hash". A backend that cannot
// answer still has to refuse the delete rather than acquire confidence it
// does not have, so the second case pins the other side: identical
// identities with no hash on either side still preserve. Without it the
// obvious over-fix (treat agreement as sufficient) would pass.
//
// The third case is the one that is easy to forget to write at all: every
// error leaving this adapter carries a manager-owned category. Wrap
// existed before anything called it, so the classification vocabulary was
// complete and unreachable, and lifecycle code switching on Category saw
// Unclassified for everything.

// Stat feeds FR-15's pre-delete recheck. If it does not carry a hash, then
// model.CompareIdentity can never reach ConfidenceStrong, Preserve() is always
// true, and a delete is unreachable on every backend, including ones that hash
// perfectly well. That was the state of things until this test existed: an
// untouched, byte-identical local object could never be deleted.
func TestStatCarriesEnoughIdentityToPermitADelete(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.dump"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New()
	src := transport.Source{ID: "p", Type: "local", Root: root}

	st, err := a.Stat(context.Background(), src, "a.dump")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Hash == "" || st.HashAlg == "" {
		t.Fatalf("Stat returned no hash on a hash-capable backend: %+v", st)
	}

	id := model.RemoteIdentity{Path: st.Path, Size: st.Size, ModTime: st.ModTime, Hash: st.Hash, HashAlg: string(st.HashAlg)}
	cmp := model.CompareIdentity(id, id)
	if cmp.Preserve() {
		t.Fatalf("an unchanged object still refuses deletion: verdict=%v confidence=%v reason=%s",
			cmp.Verdict, cmp.Confidence, cmp.Reason)
	}
}

// The counterpart, and the reason the fix above is not simply "always hash".
// A backend that cannot hash must still refuse, rather than inventing
// confidence it does not have.
func TestUnchangedButUnhashableStillRefusesDeletion(t *testing.T) {
	id := model.RemoteIdentity{Path: "a.dump", Size: 7, ModTime: 1756400000}
	cmp := model.CompareIdentity(id, id)
	if !cmp.Preserve() {
		t.Fatal("an object with no hash on either side was cleared for deletion")
	}
}

// Every error leaving the adapter has to carry a manager-owned category, or
// FR-22's retry-on-transient and FR-17's remote-confirmed-absent path have
// nothing to switch on. Wrap existed but nothing called it.
func TestAdapterErrorsCarryACategory(t *testing.T) {
	a := New()
	src := transport.Source{ID: "p", Type: "local", Root: t.TempDir()}

	_, err := a.Stat(context.Background(), src, "does-not-exist.dump")
	if err == nil {
		t.Fatal("Stat on a missing object returned no error")
	}
	cat, ok := transport.CategoryOf(err)
	if !ok {
		t.Fatalf("Stat's error carries no category at all: %v", err)
	}
	if cat != transport.NotFound {
		t.Fatalf("category = %v, want NotFound (err=%v)", cat, err)
	}

	if err := a.DeleteRemote(context.Background(), src, "does-not-exist.dump"); err != nil {
		if _, ok := transport.CategoryOf(err); !ok {
			t.Fatalf("DeleteRemote's error carries no category: %v", err)
		}
	}
}
