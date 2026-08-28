// Package rclone is the only package in this repository that imports rclone.
//
// Exactly two backends are registered. Importing all of them for convenience
// would cost binary size, dependency surface, initialization complexity and
// accidental configuration exposure, so a third backend is an architecture
// decision rather than an import line (FR-4).
package rclone

import (
	"context"
	"fmt"

	// local and sftp are the two backends FR-4 requires. Importing them,
	// together with fs/operations below, also registers crypt transitively.
	// See backends.go for the traced cause, why it's accepted rather than
	// removed, and the test that keeps this exact set enforced.
	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/sftp"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"

	"github.com/spdrman/rclone-manager/internal/transport"
)

// Adapter implements transport.Transport over embedded rclone packages.
type Adapter struct{}

// New returns an adapter. It takes no rclone types, by design.
func New() *Adapter { return &Adapter{} }

var _ transport.Transport = (*Adapter)(nil)

// fsFor builds an rclone Fs for a source without touching any on-disk rclone
// config file. Everything comes from the manager's own configuration, so there
// is no ambient rclone state to leak in.
//
// sftp options are built by sftpConfig in ssh.go, which owns the SSH
// authentication and host-key verification posture required by FR-6.
func (a *Adapter) fsFor(ctx context.Context, src transport.Source) (fs.Fs, error) {
	info, err := fs.Find(src.Type)
	if err != nil {
		return nil, fmt.Errorf("backend %q is not registered in this binary: %w", src.Type, err)
	}

	cfg := configmap.Simple{}
	if src.Type == "sftp" {
		sftpCfg, err := sftpConfig(src)
		if err != nil {
			return nil, err
		}
		cfg = sftpCfg
	}

	f, err := info.NewFs(ctx, src.ID, src.Root, cfg)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", src.ID, err)
	}
	return f, nil
}

func toArtifact(o fs.Object) transport.RemoteArtifact {
	return transport.RemoteArtifact{
		Path:    o.Remote(),
		Size:    o.Size(),
		ModTime: o.ModTime(context.Background()).Unix(),
	}
}

func (a *Adapter) List(ctx context.Context, src transport.Source) ([]transport.RemoteArtifact, error) {
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return nil, err
	}
	entries, err := f.List(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]transport.RemoteArtifact, 0, len(entries))
	for _, e := range entries {
		if o, ok := e.(fs.Object); ok {
			out = append(out, toArtifact(o))
		}
	}
	return out, nil
}

func (a *Adapter) Stat(ctx context.Context, src transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return transport.RemoteArtifact{}, err
	}
	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return transport.RemoteArtifact{}, err
	}
	return toArtifact(o), nil
}

func (a *Adapter) CopyToLocal(ctx context.Context, src transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	srcFs, err := a.fsFor(ctx, src)
	if err != nil {
		return transport.TransferResult{}, err
	}
	o, err := srcFs.NewObject(ctx, remotePath)
	if err != nil {
		return transport.TransferResult{}, err
	}
	dstDir, dstName := splitPath(localPartialPath)
	dstFs, err := fs.NewFs(ctx, dstDir)
	if err != nil {
		return transport.TransferResult{}, err
	}
	// Copy, never Move. The remote source is deleted later, by the lifecycle
	// manager, and only after a durable commit (FR-11, FR-15).
	dst, err := operations.Copy(ctx, dstFs, nil, dstName, o)
	if err != nil {
		return transport.TransferResult{}, err
	}
	return transport.TransferResult{BytesTransferred: dst.Size()}, nil
}

func (a *Adapter) RemoteHash(ctx context.Context, src transport.Source, remotePath string, alg transport.HashAlgorithm) (string, error) {
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return "", err
	}
	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return "", err
	}
	var ht hash.Type
	switch alg {
	case transport.SHA256:
		ht = hash.SHA256
	default:
		return "", fmt.Errorf("unsupported hash %q", alg)
	}
	// An unsupported remote hash must surface as an explicit capability result,
	// never as a silent downgrade of configured verification (FR-13).
	if !f.Hashes().Contains(ht) {
		return "", fmt.Errorf("backend %q cannot compute %s", src.Type, alg)
	}
	return o.Hash(ctx, ht)
}

func (a *Adapter) DeleteRemote(ctx context.Context, src transport.Source, remotePath string) error {
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return err
	}
	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return err
	}
	return o.Remove(ctx)
}

func splitPath(p string) (dir, name string) {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i], p[i+1:]
		}
	}
	return ".", p
}
