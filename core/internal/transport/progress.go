package transport

import "context"

// ByteProgress is one sample of a copy that is happening right now,
// expressed entirely in this package's own vocabulary.
//
// It exists so live transfer progress can leave the adapter that computes
// it without the rclone types that computed it coming along. Invariant 13
// ("rclone API details must not leak outside the adapter") is the reason
// this struct is here and not in transport/rclone: every package upstream
// of the adapter reads progress through this type, and none of them can
// import rclone at all.
//
// A sample is a reading, not an event log. Nothing durable is built from
// one, nothing decides anything from one, and a caller that misses one
// has lost nothing but a redraw.
type ByteProgress struct {
	// BytesTransferred is how much of THIS copy has landed so far.
	BytesTransferred int64

	// BytesTotal is the size of the object being copied, or 0 when the
	// backend could not say. Zero means "unknown", so a reader must check
	// it before dividing; it never means "an empty object", because an
	// empty object is never copied through a progress-reporting path in
	// the first place.
	BytesTotal int64

	// BytesPerSecond is this copy's average rate so far, or 0 when too
	// little has happened to measure one. Zero is "no rate yet", and a
	// reader must not render it as "stalled at 0 B/s": those are
	// different claims and only one of them is supported by a sample this
	// early.
	BytesPerSecond int64
}

// ProgressReporter receives ByteProgress samples while a copy runs.
//
// The transport calls this from a goroutine of its own while the copy is
// in flight, so an implementation must be safe to call concurrently with
// whatever else the caller is doing, and must not block: a slow reporter
// slows the sampler, and a reporter that blocks forever holds the copy's
// own goroutine at shutdown.
type ProgressReporter interface {
	CopyProgress(ByteProgress)
}

// ProgressReporterFunc adapts a plain function to ProgressReporter.
type ProgressReporterFunc func(ByteProgress)

// CopyProgress calls f.
func (f ProgressReporterFunc) CopyProgress(p ByteProgress) { f(p) }

type progressReporterKey struct{}

// WithProgressReporter returns a context that asks the transport to report
// copy progress to r while it works.
//
// It travels on the context rather than as a parameter on
// Transport.CopyToLocal deliberately. Progress is scoped to the operation
// in flight, which is exactly what a context already is; the alternative
// is a parameter threaded through every intermediate layer (internal/app,
// internal/lifecycle) that has no interest in it, and a Transport
// interface change that every fake transport in the repository would have
// to absorb for the sake of a value most of them will never produce. The
// engine this adapter wraps carries its own transfer statistics on the
// context for the same reason.
//
// A nil r is the same as no reporter at all.
func WithProgressReporter(ctx context.Context, r ProgressReporter) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, progressReporterKey{}, r)
}

// ProgressReporterFrom returns the reporter WithProgressReporter put on
// ctx, or nil when there is none. Nil is the ordinary case: nothing in
// this repository requires progress reporting to work, and an adapter
// that finds no reporter does exactly what it did before this existed.
func ProgressReporterFrom(ctx context.Context) ProgressReporter {
	r, _ := ctx.Value(progressReporterKey{}).(ProgressReporter)
	return r
}
