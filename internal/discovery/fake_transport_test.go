package discovery

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/internal/transport"
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

var _ transport.Transport = (*fakeTransport)(nil)

func (f *fakeTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.artifacts, nil
}

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

func (f *fakeTransport) CopyToLocal(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	return transport.TransferResult{}, fmt.Errorf("fakeTransport: CopyToLocal not implemented")
}

func (f *fakeTransport) RemoteHash(_ context.Context, _ transport.Source, remotePath string, _ transport.HashAlgorithm) (string, error) {
	if err, ok := f.hashErr[remotePath]; ok {
		return "", err
	}
	if h, ok := f.hashes[remotePath]; ok {
		return h, nil
	}
	return "", fmt.Errorf("fakeTransport: no hash configured for %q", remotePath)
}

func (f *fakeTransport) DeleteRemote(context.Context, transport.Source, string) error {
	return fmt.Errorf("fakeTransport: DeleteRemote not implemented")
}
