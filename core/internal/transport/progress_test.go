package transport_test

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// TestProgressReporter_AbsentByDefault is the property every existing
// caller depends on without knowing it: a context nobody attached a
// reporter to reports nothing, so an adapter that looks for one finds
// nothing and behaves exactly as it did before progress existed.
func TestProgressReporter_AbsentByDefault(t *testing.T) {
	if r := transport.ProgressReporterFrom(context.Background()); r != nil {
		t.Fatalf("ProgressReporterFrom(context.Background()) = %#v, want nil", r)
	}
}

// TestProgressReporter_RoundTrips proves the one thing the ctx-carried
// design has to get right: what an adapter pulls off the context is the
// reporter the caller put there, and calling it reaches the caller.
func TestProgressReporter_RoundTrips(t *testing.T) {
	var got []transport.ByteProgress
	ctx := transport.WithProgressReporter(context.Background(),
		transport.ProgressReporterFunc(func(p transport.ByteProgress) { got = append(got, p) }))

	r := transport.ProgressReporterFrom(ctx)
	if r == nil {
		t.Fatal("ProgressReporterFrom returned nil for a context that carries a reporter")
	}
	r.CopyProgress(transport.ByteProgress{BytesTransferred: 7, BytesTotal: 11, BytesPerSecond: 3})

	if len(got) != 1 {
		t.Fatalf("reporter received %d samples, want 1", len(got))
	}
	if want := (transport.ByteProgress{BytesTransferred: 7, BytesTotal: 11, BytesPerSecond: 3}); got[0] != want {
		t.Errorf("sample = %+v, want %+v", got[0], want)
	}
}

// TestProgressReporter_NilReporterIsNotAttached keeps WithProgressReporter
// from turning "no reporter" into "a typed nil that panics on the first
// call": a caller handing over a nil reporter must leave the context
// exactly as it found it.
func TestProgressReporter_NilReporterIsNotAttached(t *testing.T) {
	ctx := transport.WithProgressReporter(context.Background(), nil)
	if r := transport.ProgressReporterFrom(ctx); r != nil {
		t.Fatalf("ProgressReporterFrom = %#v after attaching a nil reporter, want nil", r)
	}
}
