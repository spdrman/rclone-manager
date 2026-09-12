package rclone

import (
	"context"
	"io"

	"github.com/backupdproject/backupd/core/internal/transport"
)

// OpenSourceStream opens one file on a backup SOURCE for a single forward
// read and hands back the stream itself.
//
// It is the read half CopyToLocal never exposed. CopyToLocal's contract is
// "this remote file is now a local file", which is the right contract for a
// pull-and-verify cycle and the wrong one for a backup engine that hashes
// and chunks as the bytes arrive: staging first means writing the file
// twice and needing room for it once. #791 needs the bytes, not a copy of
// them, so this method hands over rclone's own reader.
//
// What crosses the boundary is an io.ReadCloser and nothing else. No
// fs.Object, no fs.Fs, no rclone type at all -- callers get a stream whose
// backend they cannot name, which is the same promise every other method on
// this adapter makes. The reader is forward-only by construction: it is
// whatever the backend's Object.Open returned, unwrapped and unbuffered, so
// a caller that wants to re-read has to open again.
//
// Closing the reader is not optional. It is what releases the Fs, exactly
// as in OpenObject, because there is no way out of this method to defer a
// release on: the reader is still reading through it.
func (a *Adapter) OpenSourceStream(ctx context.Context, src transport.Source, remotePath string) (io.ReadCloser, error) {
	ctx = oneConnectionAtATime(ctx)

	f, err := a.fsFor(ctx, src)
	if err != nil {
		return nil, WrapCtx(ctx, "open_source_stream", err)
	}

	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		shutdownFs(ctx, f)

		return nil, WrapCtx(ctx, "open_source_stream", err)
	}

	rc, err := o.Open(ctx)
	if err != nil {
		shutdownFs(ctx, f)

		return nil, WrapCtx(ctx, "open_source_stream", err)
	}

	return &fsBoundReadCloser{ReadCloser: rc, fs: f, ctx: ctx}, nil
}
