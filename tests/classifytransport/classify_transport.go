// Package classifytransport decorates a transport.Transport so every error
// it returns has already been through internal/transport/rclone's own
// Classify/Wrap.
//
// It exists to work around a real defect this issue's test suites found:
// internal/transport/rclone.Adapter never calls Wrap on anything it
// returns (see internal/transport/rclone/error_classification_gap_a213_test.go
// and internal/reconcile/a213_defect_test.go for the two tests that prove
// it, and the PR description for the full writeup and the fix this
// package recommends but does not itself apply, since adapter.go is
// production code outside this PR's file scope of tests/ and new
// _test.go files). Left unwrapped, most of this PR's crash-matrix and
// SFTP-integration coverage of reconciliation's absent-object detection
// and of FR-22's retry-on-transient behaviour would be blocked by that one
// already-diagnosed, already-reported gap rather than proving anything
// about the lifecycle/reconcile logic those suites actually exist to
// exercise.
//
// This is not a production-code change: Classify and Wrap are already
// public, already-tested API of internal/transport/rclone (see
// errors.go/errors_test.go there). This file only calls that public API
// from outside the package, exactly as any other caller of
// internal/transport/rclone could, and does exactly what
// internal/transport/rclone.Adapter's own methods should already be doing
// internally.
package classifytransport

import (
	"context"

	"github.com/spdrman/rclone-manager/internal/transport"
	"github.com/spdrman/rclone-manager/internal/transport/rclone"
)

// Wrap decorates tr so every error it returns carries the category
// rclone.Classify assigns it, the same as if tr's own methods called
// rclone.Wrap internally.
func Wrap(tr transport.Transport) transport.Transport {
	return classifying{tr: tr}
}

type classifying struct{ tr transport.Transport }

var _ transport.Transport = classifying{}

func (c classifying) List(ctx context.Context, source transport.Source) ([]transport.RemoteArtifact, error) {
	out, err := c.tr.List(ctx, source)
	return out, rclone.Wrap("list", err)
}

func (c classifying) Stat(ctx context.Context, source transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	out, err := c.tr.Stat(ctx, source, remotePath)
	return out, rclone.Wrap("stat", err)
}

func (c classifying) CopyToLocal(ctx context.Context, source transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	out, err := c.tr.CopyToLocal(ctx, source, remotePath, localPartialPath)
	return out, rclone.Wrap("copy_to_local", err)
}

func (c classifying) RemoteHash(ctx context.Context, source transport.Source, remotePath string, algorithm transport.HashAlgorithm) (string, error) {
	out, err := c.tr.RemoteHash(ctx, source, remotePath, algorithm)
	return out, rclone.Wrap("remote_hash", err)
}

func (c classifying) DeleteRemote(ctx context.Context, source transport.Source, remotePath string) error {
	err := c.tr.DeleteRemote(ctx, source, remotePath)
	return rclone.Wrap("delete_remote", err)
}
