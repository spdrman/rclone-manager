package rclone

import (
	"context"
	"io"

	"github.com/rclone/rclone/fs"

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

// StatSource reports what the source says about one object, reading its
// METADATA and nothing else.
//
// It exists because Stat cannot be used for this, and the reason is
// measurable rather than stylistic. Stat asks the object for a SHA-256
// when the backend advertises one, and rclone's local backend advertises
// one by computing it - which is a full read of the file. That is the
// right trade for the destination side, where a stat is how a copy is
// proven and the object was going to be read anyway. It is catastrophic
// on the source side of a backup: the streaming adapter stats every
// object again after reading it, to see whether it moved, so a hashing
// stat would read every byte of a 100 GB source a second time to answer
// a question about its size and timestamp. Doubling the I/O of a backup
// is the anti-pattern this whole path exists to avoid, arriving through
// the back door.
//
// Kind is deliberately left unset. rclone's object model has no answer:
// Fs.NewObject returns an object for a fifo (of size zero) and follows a
// symlink to its target, so "it resolved to an object" says nothing
// about what is at the path. The bounded local enumerator classifies
// what it walks, from the directory read it already performed, and that
// is where a kind comes from; a consumer that has no kind must not
// invent one.
func (a *Adapter) StatSource(ctx context.Context, src transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	ctx = oneConnectionAtATime(ctx)

	f, err := a.fsFor(ctx, src)
	if err != nil {
		return transport.RemoteArtifact{}, WrapCtx(ctx, "stat_source", err)
	}
	defer shutdownFs(ctx, f)

	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return transport.RemoteArtifact{}, WrapCtx(ctx, "stat_source", err)
	}

	art := toArtifact(o)
	if ider, ok := o.(fs.IDer); ok {
		art.ID = ider.ID()
	}

	return art, nil
}
