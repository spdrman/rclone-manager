package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// The fixtures every other test file in this package is built on.
//
// Reading this file before writing a test here saves rediscovering two
// things the hard way. fakeTransport is source-blind: it is keyed on remote
// path alone, so two backup sets sharing one instance each discover the
// other's objects, and a test with more than one set needs one fake per set.
// And the doubles are deliberately narrow rather than one general failure
// switch, because a test that wants a transfer to fail almost always wants
// discovery to have succeeded first, and a broad switch silently gives it a
// perfectly clean cycle instead.
//
// The layout mirrors the idiom internal/discovery's and internal/reconcile's
// own tests already use, so a reader moving between packages finds the same
// shapes in the same places.

// --- shared fixtures, mirroring the idiom internal/discovery/discovery_test.go
// and internal/reconcile's own tests already use in this codebase ---

func openJournal(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func mustSetID(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func mustWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// --- fakeTransport: a small, fully-controlled transport.Transport used
// instead of the real rclone adapter for tests that need FR-16's identity
// comparison to reach ConfidenceStrong (see this package's introducing PR
// description: the real internal/transport/rclone.Adapter's Stat never
// populates Hash/HashAlg/ID for any backend, so DeleteRemote's own
// re-check can never reach that confidence level through it today). This
// fake exists purely to prove this package's own sequencing is correct on
// its own terms, independent of that separately-reported gap.

// fakeObject is one remote object: its bytes, and the modification time
// discovery's completion strategies read. The hash is derived from the bytes
// on demand rather than stored, so a test cannot construct an object whose
// recorded hash disagrees with its content by accident.
type fakeObject struct {
	data    []byte
	modTime int64
}

// fakeTransport is keyed on remote path and knows nothing about which source
// asked, which is the one thing to remember about it.
//
// Everything below is addressed by path alone, so two backup sets pointed at
// one instance see the same objects, and a test covering more than one set
// wants one fake each. failForSourceID is the single exception, and it exists
// precisely because there was no other way to make ONE set's remote go away
// without taking the rest of a shared fixture down with it.
//
// The other fields are narrow on purpose: one switch per method rather than
// one switch for the transport. That is not fussiness. A test that wants a
// transfer to fail wants discovery to have worked first, and a broad failure
// switch hands it a cycle where nothing was ever discovered, which passes for
// the wrong reason.
type fakeTransport struct {
	objects map[string]*fakeObject

	deleteRemoteCalls int32 // atomic
	deleteErr         error

	copyToLocalCallsCount int32 // atomic

	// failForSourceID, when non-empty, makes every method fail with
	// failErr for calls whose transport.Source.ID matches it, leaving
	// every other source unaffected. This is what lets a test simulate one
	// specific configured backup set's remote being systemically
	// unreachable without one shared fake transport instance breaking
	// every backup set that happens to use it.
	failForSourceID string
	failErr         error

	// beforeCopy, when set, runs at the very start of every CopyToLocal,
	// before any byte is written. It is what lets a test act at the exact
	// moment a transfer is in flight (issue #350's "Edit pressed while a
	// transfer is running") without racing a sleep against real work.
	beforeCopy func()

	// poison, when set, makes DeleteRemote fail the test the instant it is
	// called, rather than merely counting the call for a later assertion.
	// Issue #282's own acceptance criterion asks for proof "not by
	// asserting a refusal": a double that fails as soon as it is invoked
	// is the strongest form of that this package can build, stronger than
	// a post-hoc deleteCallCount() check.
	poison *testing.T

	// copyToLocalErr, when non-nil, is what CopyToLocal returns instead of
	// writing the bytes, for every remote path.
	//
	// It is its own field rather than a use of failForSourceID above,
	// which reads as though it covers this and does not: that field is
	// consulted by List and Stat only, so a test setting it and expecting
	// a failed COPY gets a perfectly successful cycle. This is the same
	// narrow, one-method shape remoteHashErr already has, for the same
	// reason: a test that wants a transfer to fail wants discovery to have
	// worked.
	copyToLocalErr error

	// remoteHashErr, when non-nil, is what RemoteHash returns instead of
	// a computed hash, for every remote path. Unlike failForSourceID
	// above (which fails every method, breaking transfer along with
	// verification), this targets only the hash-comparison call, so a
	// test can drive a real FR-13 layer-2 capability-absence failure
	// (issue #284's own reproduction: a hardened SFTP account that
	// cannot compute a hash) through an otherwise-successful transfer.
	remoteHashErr error

	// afterCopyToLocal, when non-nil, runs at the end of a successful
	// CopyToLocal with the .partial path it just wrote. It exists so a
	// test can change the local directory between lifecycle.Transfer's
	// final-name collision guard (which runs before anything else, every
	// time, per transfer.go) and lifecycle.Commit's own rename. That is
	// the only window in which a cycle can be made to fail its commit
	// rather than its transfer. See
	// TestRunCycle_ResumesAnArtifactLeftAtCommitting.
	afterCopyToLocal func(localPartialPath string)
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{objects: map[string]*fakeObject{}}
}

func (f *fakeTransport) put(path string, content string, modTime int64) {
	f.objects[path] = &fakeObject{data: []byte(content), modTime: modTime}
}

func (f *fakeTransport) hash(obj *fakeObject) string {
	sum := sha256.Sum256(obj.data)
	return hex.EncodeToString(sum[:])
}

func (f *fakeTransport) failsFor(source transport.Source) bool {
	return f.failForSourceID != "" && source.ID == f.failForSourceID
}

func (f *fakeTransport) List(ctx context.Context, source transport.Source) ([]transport.RemoteArtifact, error) {
	if f.failsFor(source) {
		return nil, f.failErr
	}
	out := make([]transport.RemoteArtifact, 0, len(f.objects))
	for path, obj := range f.objects {
		out = append(out, transport.RemoteArtifact{
			Path: path, Size: int64(len(obj.data)), ModTime: obj.modTime,
			Hash: f.hash(obj), HashAlg: transport.SHA256, ID: "fake:" + path,
		})
	}
	return out, nil
}

func (f *fakeTransport) Stat(ctx context.Context, source transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	if f.failsFor(source) {
		return transport.RemoteArtifact{}, f.failErr
	}
	obj, ok := f.objects[remotePath]
	if !ok {
		return transport.RemoteArtifact{}, transport.NewError(transport.NotFound, "stat", errors.New("not found"))
	}
	return transport.RemoteArtifact{
		Path: remotePath, Size: int64(len(obj.data)), ModTime: obj.modTime,
		Hash: f.hash(obj), HashAlg: transport.SHA256, ID: "fake:" + remotePath,
	}, nil
}

func (f *fakeTransport) CopyToLocal(ctx context.Context, source transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	atomic.AddInt32(&f.copyToLocalCallsCount, 1)
	if f.beforeCopy != nil {
		f.beforeCopy()
	}
	if f.copyToLocalErr != nil {
		return transport.TransferResult{}, f.copyToLocalErr
	}
	obj, ok := f.objects[remotePath]
	if !ok {
		return transport.TransferResult{}, transport.NewError(transport.NotFound, "copy_to_local", errors.New("not found"))
	}
	if err := os.MkdirAll(filepath.Dir(localPartialPath), 0o755); err != nil {
		return transport.TransferResult{}, err
	}
	if err := os.WriteFile(localPartialPath, obj.data, 0o644); err != nil {
		return transport.TransferResult{}, err
	}
	if f.afterCopyToLocal != nil {
		f.afterCopyToLocal(localPartialPath)
	}
	return transport.TransferResult{BytesTransferred: int64(len(obj.data))}, nil
}

func (f *fakeTransport) RemoteHash(ctx context.Context, source transport.Source, remotePath string, algorithm transport.HashAlgorithm) (string, error) {
	if f.remoteHashErr != nil {
		return "", f.remoteHashErr
	}
	obj, ok := f.objects[remotePath]
	if !ok {
		return "", transport.NewError(transport.NotFound, "remote_hash", errors.New("not found"))
	}
	return f.hash(obj), nil
}

func (f *fakeTransport) DeleteRemote(ctx context.Context, source transport.Source, remotePath string) error {
	atomic.AddInt32(&f.deleteRemoteCalls, 1)
	if f.poison != nil {
		f.poison.Fatalf("fakeTransport.DeleteRemote(%q) was called; this test's backup set must never reach the transport's delete", remotePath)
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.objects, remotePath)
	return nil
}

func (f *fakeTransport) deleteCallCount() int {
	return int(atomic.LoadInt32(&f.deleteRemoteCalls))
}

func (f *fakeTransport) copyToLocalCalls() int {
	return int(atomic.LoadInt32(&f.copyToLocalCallsCount))
}

var _ transport.Transport = (*fakeTransport)(nil)

// --- a Journal wrapper that can run a hook on every RecordTransition,
// used by the shutdown-safety test to inject a cancellation at an exact
// boundary without needing any change to internal/lifecycle or
// internal/state.

// hookJournal wraps the real journal so a test can act at the instant a
// specific transition lands, which is what makes the shutdown-safety proof
// possible without touching internal/lifecycle or internal/state.
//
// The hook runs AFTER the write and only when it succeeded, so a test
// cancelling from inside it cancels at a boundary the product has genuinely
// reached. Running it before the write would let a test prove something about
// a state the journal never held.
type hookJournal struct {
	*state.Journal
	onRecordTransition func(t state.Transition, out state.Outcome)
}

func (h *hookJournal) RecordTransition(ctx context.Context, t state.Transition) (state.Outcome, error) {
	out, err := h.Journal.RecordTransition(ctx, t)
	if err == nil && h.onRecordTransition != nil {
		h.onRecordTransition(t, out)
	}
	return out, err
}

var _ Journal = (*hookJournal)(nil)
