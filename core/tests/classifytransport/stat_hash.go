package classifytransport

import (
	"context"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// A Transport decorator that attaches a best-effort hash to whatever Stat
// returns, and the reasoning for why it is still here after the defect it
// worked around was fixed.
//
// It is kept deliberately rather than left behind. WithStatHash's own doc
// carries the full history, and the part worth knowing before reading the
// code is that its existence is not evidence of a live bug: the rclone
// adapter's Stat now carries enough identity for FR-16 on its own, and this
// only still earns its place against a Transport that genuinely does not
// hash in Stat.

// WithStatHash decorates tr so its Stat method also attaches a best-effort
// sha256 hash to whatever it returns, exactly mirroring the pattern
// internal/discovery.go's captureRemoteIdentity already uses at discovery
// time: try Transport.RemoteHash, and if it succeeds, carry the result
// along; if it fails (an unsupported-capability backend, most notably a
// hardened, shell-less SFTP account), leave the artifact exactly as the
// real Stat reported it, hash-less.
//
// HISTORY, and why this is now belt and braces rather than a workaround.
// It was written to work around a real defect: internal/lifecycle.DeleteRemote's FR-16 re-identification builds
// its "current" identity from a bare Transport.Stat call, which
// internal/transport/rclone.Adapter's toArtifact never populates with a
// hash or backend id (only Path/Size/ModTime), and DeleteRemote never
// calls RemoteHash itself to fill that gap in.
//
// That defect is FIXED. Adapter.Stat now asks the backend for a hash and a
// stable id, so a real Stat carries enough identity for FR-16 to reach
// ConfidenceStrong on its own. This decorator is therefore redundant against
// the rclone adapter today. I left it in place because it still does
// something useful for a Transport that genuinely does not hash in Stat, and
// because ripping it out would churn several test files for no behavioural
// gain. Do not read its existence as evidence the defect is still there. See
// internal/discovery/a213_defect_test.go
// (TestRealPipeline_DeleteRemote_NeverConfirmsIdentityStrongly_KnownDefect)
// for the full proof and the PR description for the recommended fix,
// which touches internal/lifecycle/remotedelete.go and so is out of this
// PR's file scope to apply directly.
//
// Without this, a real delete can never reach model.ConfidenceStrong
// against any backend registered in this binary, which would make most of
// this PR's crash-matrix coverage past COMMITTED unable to exercise a
// genuine, successful remote deletion at all: every attempt would refuse
// for the same already-diagnosed reason, regardless of which crash point
// is actually under test. Composing this decorator is not a production
// code change: RemoteHash is already public, already-tested API of
// transport.Transport; this file only calls it from outside
// internal/lifecycle, the same way any other caller could, and does
// exactly what DeleteRemote's own re-identification should already be
// doing internally.
func WithStatHash(tr transport.Transport) transport.Transport {
	return statHashing{tr: tr}
}

type statHashing struct{ tr transport.Transport }

var _ transport.Transport = statHashing{}

func (s statHashing) List(ctx context.Context, source transport.Source) ([]transport.RemoteArtifact, error) {
	return s.tr.List(ctx, source)
}

func (s statHashing) Stat(ctx context.Context, source transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	art, err := s.tr.Stat(ctx, source, remotePath)
	if err != nil {
		return art, err
	}
	if h, hashErr := s.tr.RemoteHash(ctx, source, remotePath, transport.SHA256); hashErr == nil && h != "" {
		art.Hash = h
		art.HashAlg = transport.SHA256
	}
	return art, nil
}

func (s statHashing) CopyToLocal(ctx context.Context, source transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	return s.tr.CopyToLocal(ctx, source, remotePath, localPartialPath)
}

func (s statHashing) RemoteHash(ctx context.Context, source transport.Source, remotePath string, algorithm transport.HashAlgorithm) (string, error) {
	return s.tr.RemoteHash(ctx, source, remotePath, algorithm)
}

func (s statHashing) DeleteRemote(ctx context.Context, source transport.Source, remotePath string) error {
	return s.tr.DeleteRemote(ctx, source, remotePath)
}
