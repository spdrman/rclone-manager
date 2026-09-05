package contract

import (
	"context"
	"errors"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file unit-tests Capture and Changed, the two helpers the contract
// suite's changed-object case leans on, against an in-memory transport
// instead of a backend.
//
// The split is deliberate. The suite in contract.go proves these work
// against a REAL backend, which is what a backend author needs; it cannot
// easily produce the awkward inputs, because a real filesystem will not
// hand you two different objects with the same path, size and modification
// time on demand. So the table below builds those by hand, one attribute
// at a time, which is the only way to show that Changed reaches for a hash
// rather than stopping at a size and a timestamp that happen to agree.
//
// fakeTransport implements the whole Transport interface and panics in the
// three methods Capture has no business calling, on purpose. A stub that
// returned a zero value instead would let a future Capture start calling
// List or DeleteRemote without anybody noticing; a panic makes that a
// failure with a stack trace pointing straight at the new call.

// fakeTransport is a minimal, in-memory transport.Transport used to unit test
// Capture and Changed without touching a real backend. It only implements
// enough behavior for those two functions.
type fakeTransport struct {
	artifacts map[string]transport.RemoteArtifact
	hashes    map[string]string // remotePath -> hash, absent means "cannot hash"
}

var _ transport.Transport = (*fakeTransport)(nil)

func (f *fakeTransport) List(ctx context.Context, source transport.Source) ([]transport.RemoteArtifact, error) {
	panic("not needed")
}

func (f *fakeTransport) Stat(ctx context.Context, source transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	a, ok := f.artifacts[remotePath]
	if !ok {
		return transport.RemoteArtifact{}, errNotFound
	}
	return a, nil
}

func (f *fakeTransport) CopyToLocal(ctx context.Context, source transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	panic("not needed")
}

func (f *fakeTransport) RemoteHash(ctx context.Context, source transport.Source, remotePath string, algorithm transport.HashAlgorithm) (string, error) {
	h, ok := f.hashes[remotePath]
	if !ok {
		return "", errUnsupportedHash
	}
	return h, nil
}

func (f *fakeTransport) DeleteRemote(ctx context.Context, source transport.Source, remotePath string) error {
	panic("not needed")
}

var (
	errNotFound        = errors.New("fake: not found")
	errUnsupportedHash = errors.New("fake: hash unsupported")
)

func TestCapture_PopulatesHashWhenAvailable(t *testing.T) {
	tr := &fakeTransport{
		artifacts: map[string]transport.RemoteArtifact{
			"a.txt": {Path: "a.txt", Size: 10, ModTime: 100},
		},
		hashes: map[string]string{"a.txt": "deadbeef"},
	}

	id, err := Capture(context.Background(), tr, transport.Source{}, "a.txt", transport.SHA256)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !id.HasHash {
		t.Fatalf("expected HasHash true, hash was available")
	}
	if id.Artifact.Hash != "deadbeef" {
		t.Fatalf("expected hash %q, got %q", "deadbeef", id.Artifact.Hash)
	}
	if id.Artifact.HashAlg != transport.SHA256 {
		t.Fatalf("expected HashAlg %q, got %q", transport.SHA256, id.Artifact.HashAlg)
	}
}

func TestCapture_LeavesHashEmptyWhenUnavailable(t *testing.T) {
	tr := &fakeTransport{
		artifacts: map[string]transport.RemoteArtifact{
			"a.txt": {Path: "a.txt", Size: 10, ModTime: 100},
		},
		hashes: map[string]string{}, // no backend hash for this object
	}

	id, err := Capture(context.Background(), tr, transport.Source{}, "a.txt", transport.SHA256)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if id.HasHash {
		t.Fatalf("expected HasHash false when the backend cannot hash this object")
	}
	if id.Artifact.Hash != "" {
		t.Fatalf("expected empty hash, got %q", id.Artifact.Hash)
	}
}

func TestCapture_PropagatesStatError(t *testing.T) {
	tr := &fakeTransport{artifacts: map[string]transport.RemoteArtifact{}}

	_, err := Capture(context.Background(), tr, transport.Source{}, "missing.txt", transport.SHA256)
	if !errors.Is(err, errNotFound) {
		t.Fatalf("expected Stat's not-found error to propagate, got %v", err)
	}
}

// TestChanged is the core FR-16 proof: it must catch a remote replacement,
// including the awkward case where the replacement has the same path, the
// same size and the same modification time as what was discovered, and only
// the content differs. Only a hash comparison can catch that case, so this
// table exists specifically to prove Changed reaches for one instead of
// stopping at size/mtime agreement.
func TestChanged(t *testing.T) {
	base := transport.RemoteArtifact{Path: "backup.tar", Size: 1024, ModTime: 1_700_000_000}

	tests := []struct {
		name          string
		discovered    Identity
		current       Identity
		wantChanged   bool
		wantConfident bool
		note          string
	}{
		{
			name:          "identical, hash agrees",
			discovered:    Identity{Artifact: withHash(base, "hash-a"), HasHash: true},
			current:       Identity{Artifact: withHash(base, "hash-a"), HasHash: true},
			wantChanged:   false,
			wantConfident: true,
		},
		{
			name:          "different path",
			discovered:    Identity{Artifact: base},
			current:       Identity{Artifact: withPath(base, "other.tar")},
			wantChanged:   true,
			wantConfident: true,
		},
		{
			name:          "different size, no hash on either side",
			discovered:    Identity{Artifact: base},
			current:       Identity{Artifact: withSize(base, 2048)},
			wantChanged:   true,
			wantConfident: true,
		},
		{
			name:          "different modtime, no hash on either side",
			discovered:    Identity{Artifact: base},
			current:       Identity{Artifact: withModTime(base, base.ModTime+1)},
			wantChanged:   true,
			wantConfident: true,
		},
		{
			name: "the awkward case: same path, same size, same modtime, different content",
			discovered: Identity{
				Artifact: withHash(base, "hash-of-original-content"),
				HasHash:  true,
			},
			current: Identity{
				// Same Path, same Size, same ModTime as base/discovered: a
				// size+modtime-only comparison would call this unchanged.
				Artifact: withHash(base, "hash-of-replacement-content"),
				HasHash:  true,
			},
			wantChanged:   true,
			wantConfident: true,
			note:          "must be caught by the hash comparison alone; size and modtime are identical",
		},
		{
			name: "no hash available on either side, size and modtime agree",
			discovered: Identity{
				Artifact: transport.RemoteArtifact{Path: base.Path, Size: base.Size, ModTime: 0},
			},
			current: Identity{
				Artifact: transport.RemoteArtifact{Path: base.Path, Size: base.Size, ModTime: 0},
			},
			wantChanged:   false,
			wantConfident: false,
			note:          "FR-16: identity cannot be established with sufficient confidence here, so a caller must refuse rather than treat this as a safe match",
		},
		{
			name: "hash on discovered side only falls back to weak size/modtime evidence",
			discovered: Identity{
				Artifact: withHash(base, "hash-a"),
				HasHash:  true,
			},
			current: Identity{
				Artifact: base, // same size/modtime, no hash available now
			},
			// This used to assert wantConfident: true. That was the bug this
			// package's Changed used to have (now fixed by delegating to
			// model.CompareIdentity): a size+modtime agreement with no hash on
			// at least one side is not proof of anything, it is exactly the
			// same-second, same-size replacement FR-16 exists to catch. Do not
			// "fix" this back to true; true was the wrong answer.
			wantChanged:   false,
			wantConfident: false,
			note:          "no two-sided hash comparison was possible, so a size/modtime agreement is only weak evidence, not confirmation",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed, confident := Changed(tc.discovered, tc.current)
			if changed != tc.wantChanged || confident != tc.wantConfident {
				t.Fatalf("Changed() = (changed=%v, confident=%v), want (changed=%v, confident=%v)%s",
					changed, confident, tc.wantChanged, tc.wantConfident, noteSuffix(tc.note))
			}
		})
	}
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}

func withHash(a transport.RemoteArtifact, h string) transport.RemoteArtifact {
	a.Hash = h
	a.HashAlg = transport.SHA256
	return a
}

func withPath(a transport.RemoteArtifact, p string) transport.RemoteArtifact {
	a.Path = p
	return a
}

func withSize(a transport.RemoteArtifact, s int64) transport.RemoteArtifact {
	a.Size = s
	return a
}

func withModTime(a transport.RemoteArtifact, m int64) transport.RemoteArtifact {
	a.ModTime = m
	return a
}
