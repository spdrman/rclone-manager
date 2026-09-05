// This file holds the one hand-written double in this package's tests.
//
// Everything else in discovery_test.go runs against the real rclone
// adapter over a local directory, which is the right default: it catches
// the disagreements between what this package believes a listing looks like
// and what one actually looks like. The cost is that a real backend will
// not fail on request, and two of FR-8's branches are entirely about
// failure (a Stat that errors mid-batch, a RemoteHash a hardened SFTP
// account cannot answer). Those are what this exists for.
//
// It is a test double for the transport, never for this package's own
// logic. Nothing here decides whether an artifact is complete or what its
// identity is; it only decides what the remote says, which is exactly the
// boundary a double should sit on.
package discovery

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// fakeTransport is a hand-rolled transport.Transport for tests that need
// precise control over what List/Stat/RemoteHash return, independent of any
// real filesystem or rclone backend. The rclone-backed tests in
// discovery_test.go cover the real adapter end to end; this one exists so
// error paths (a Stat that fails, a RemoteHash that fails) can be provoked
// on demand.
type fakeTransport struct {
	artifacts []transport.RemoteArtifact
	listErr   error

	statErr map[string]error
	hashes  map[string]string
	hashErr map[string]error
}

// A compile-time check that this double still satisfies the interface.
// Without it, a method added to transport.Transport would make every test
// here fail to build with an error about an assignment somewhere else; with
// it, the failure names this file.
var _ transport.Transport = (*fakeTransport)(nil)

// List hands back the fixture, or the configured failure. It ignores the
// Source entirely: these tests are about what discovery does with a
// listing, not about the listing being addressed correctly, and the real
// adapter's tests already cover the latter.
func (f *fakeTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.artifacts, nil
}

// Stat answers from the same fixture List does, so the identity captured
// after a completeness proof describes the same object the listing
// reported. The per-path statErr map is what lets a test fail exactly one
// candidate in a batch, which is the case
// TestDiscover_StatFailureIsPerCandidateNotFatal needs and which a
// transport that fails wholesale could not produce.
func (f *fakeTransport) Stat(_ context.Context, _ transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	if err, ok := f.statErr[remotePath]; ok {
		return transport.RemoteArtifact{}, err
	}
	for _, a := range f.artifacts {
		if a.Path == remotePath {
			return a, nil
		}
	}
	return transport.RemoteArtifact{}, fmt.Errorf("fakeTransport: no such object %q", remotePath)
}

// CopyToLocal always fails, because discovery must never call it. Nothing
// in FR-8 transfers bytes, and a double that quietly succeeded here would
// let a future change start copying during a discovery pass without any
// test noticing.
func (f *fakeTransport) CopyToLocal(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	return transport.TransferResult{}, fmt.Errorf("fakeTransport: CopyToLocal not implemented")
}

// RemoteHash reports a configured hash, a configured failure, or "no hash
// configured", and the last of those is the interesting default. It is the
// hardened, shell-less SFTP account FR-6 recommends: a backend that simply
// cannot compute one. captureRemoteIdentity is supposed to treat that as a
// degraded identity rather than a discovery failure, so the double's
// unconfigured state is the case that has to keep working.
//
// The algorithm argument is ignored: this package only ever asks for
// SHA-256, and honouring the parameter would mean inventing a second answer
// no caller can request.
func (f *fakeTransport) RemoteHash(_ context.Context, _ transport.Source, remotePath string, _ transport.HashAlgorithm) (string, error) {
	if err, ok := f.hashErr[remotePath]; ok {
		return "", err
	}
	if h, ok := f.hashes[remotePath]; ok {
		return h, nil
	}
	return "", fmt.Errorf("fakeTransport: no hash configured for %q", remotePath)
}

// DeleteRemote always fails, for the same reason CopyToLocal does and with
// more at stake. Deleting the producer's original is FR-17's decision made
// long after discovery, and a discovery pass that reached this method at
// all would be a serious bug; a double that returned nil would hide it.
func (f *fakeTransport) DeleteRemote(context.Context, transport.Source, string) error {
	return fmt.Errorf("fakeTransport: DeleteRemote not implemented")
}
