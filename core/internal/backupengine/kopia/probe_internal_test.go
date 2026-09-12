package kopia

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"

	"github.com/backupdproject/backupd/core/internal/backupengine"
)

// This file is the one that decides whether "S3-compatible" is taken at
// its word.
//
// The probe's whole job is to refuse a storage target that answers every
// request successfully while failing one of the properties a repository
// depends on. Proving that against a real endpoint is impossible by
// definition: the endpoints worth refusing are the ones nobody has in a
// test rig, and the MinIO run under core/tests/miniointegration proves the
// opposite case (a conforming endpoint passes). So the faults are injected
// into a real blob.Storage instead, one property at a time, which is the
// only way to exercise each refusal separately and to know the message
// names the right cause.
//
// Every wrapper below breaks exactly one property and keeps the rest
// working, because a storage that is broken in several ways would pass
// this test with the first check that happened to fire.

// probeFixture is a real filesystem storage over a temp directory, which
// is what the faults are injected in front of.
func probeFixture(t *testing.T) blob.Storage {
	t.Helper()

	ctx := context.Background()

	st, err := filesystem.New(ctx, &filesystem.Options{Path: t.TempDir()}, true)
	if err != nil {
		t.Fatalf("opening the fixture storage: %v", err)
	}

	t.Cleanup(func() {
		if err := st.Close(context.Background()); err != nil {
			t.Errorf("closing the fixture storage: %v", err)
		}
	})

	return st
}

// noRanges answers a range read with the whole blob, which is what an
// implementation that ignores the Range header does. Every request
// succeeds and the bytes are even correct -- there are just too many of
// them, at the wrong offset.
type noRanges struct{ blob.Storage }

func (s noRanges) GetBlob(ctx context.Context, id blob.ID, _, _ int64, out blob.OutputBuffer) error {
	return s.Storage.GetBlob(ctx, id, 0, -1, out)
}

// invisibleUntilLater lists everything except the blob that was written
// most recently, which is eventual consistency on the listing path: the
// blob is readable, it is simply not there yet as far as a listing is
// concerned.
type invisibleUntilLater struct{ blob.Storage }

func (s invisibleUntilLater) ListBlobs(ctx context.Context, prefix blob.ID, cb func(blob.Metadata) error) error {
	return s.Storage.ListBlobs(ctx, prefix, func(m blob.Metadata) error {
		if strings.HasPrefix(string(m.BlobID), probePrefix) {
			return nil
		}

		return cb(m)
	})
}

// deleteIsALie reports a successful delete and keeps the blob, which is
// what a bucket with an object lock or a broken lifecycle policy does.
type deleteIsALie struct{ blob.Storage }

func (s deleteIsALie) DeleteBlob(context.Context, blob.ID) error { return nil }

// noTimestamps stores blobs and reports no modification time for them,
// which leaves a repository with no basis for a lock expiry or a
// content-retention window.
//
// The vendor reads a written blob's time through PutOptions.GetModTime
// rather than by asking afterwards, so that is where the fault has to go:
// a wrapper that only overrode GetMetadata would look like it worked and
// change nothing the probe sees.
type noTimestamps struct{ blob.Storage }

func (s noTimestamps) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	err := s.Storage.PutBlob(ctx, id, data, opts)

	if opts.GetModTime != nil {
		*opts.GetModTime = time.Time{}
	}

	return err
}

func (s noTimestamps) GetMetadata(ctx context.Context, id blob.ID) (blob.Metadata, error) {
	m, err := s.Storage.GetMetadata(ctx, id)
	m.Timestamp = time.Time{}

	return m, err
}

// TestProbeRefusesStorageThatIsMissingARequiredProperty is the explicit
// rejection this issue asks for: an unsupported target is refused, and not
// left to be discovered as corruption later.
func TestProbeRefusesStorageThatIsMissingARequiredProperty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		wrap func(blob.Storage) blob.Storage
		says string
	}{
		{
			"a storage that ignores ranges",
			func(st blob.Storage) blob.Storage { return noRanges{st} },
			"not honouring ranges",
		},
		{
			"a storage whose listings are eventually consistent",
			func(st blob.Storage) blob.Storage { return invisibleUntilLater{st} },
			"eventually consistent",
		},
		{
			"a storage that does not really delete",
			func(st blob.Storage) blob.Storage { return deleteIsALie{st} },
			"still lists a blob it reported deleting",
		},
		{
			"a storage that reports no timestamps",
			func(st blob.Storage) blob.Storage { return noTimestamps{st} },
			"reports no timestamp",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := New().probeStorage(context.Background(), tc.wrap(probeFixture(t)))
			if err == nil {
				t.Fatalf("probeStorage accepted %s", tc.name)
			}

			if !errors.Is(err, backupengine.ErrStorageUnsupported) {
				t.Errorf("the refusal is %v; want one wrapping ErrStorageUnsupported so a caller can route on it", err)
			}

			// The message has to name the missing property. An operator
			// choosing another storage target cannot act on "unsupported".
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say %q: %v", tc.says, err)
			}
		})
	}
}

// TestProbeAcceptsAConformingStorage is the control. Without it the test
// above passes just as well against a probe that refuses everything, which
// would be a perfectly green way to make every repository uncreatable.
func TestProbeAcceptsAConformingStorage(t *testing.T) {
	t.Parallel()

	skew, err := New().probeStorage(context.Background(), probeFixture(t))
	if err != nil {
		t.Fatalf("probeStorage refused a real filesystem storage: %v", err)
	}

	// A local filesystem shares this machine's clock, so the measured skew
	// is the probe's own round trip and nothing else.
	if skew > time.Minute || skew < -time.Minute {
		t.Errorf("probeStorage measured %s of skew against this machine's own filesystem", skew)
	}
}

// TestProbeLeavesNothingBehind matters because the probe writes to storage
// an operator owns, including storage it is about to refuse. A probe blob
// left in a bucket is litter in a place nobody will come back to look.
func TestProbeLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := probeFixture(t)

	if _, err := New().probeStorage(ctx, st); err != nil {
		t.Fatalf("probeStorage: %v", err)
	}

	// And once more on a storage the probe refuses, because the cleanup on
	// the failure path is the one that gets forgotten.
	if _, err := New().probeStorage(ctx, noRanges{st}); err == nil {
		t.Fatalf("probeStorage accepted a storage that ignores ranges")
	}

	var left []blob.ID

	if err := st.ListBlobs(ctx, "", func(m blob.Metadata) error {
		left = append(left, m.BlobID)

		return nil
	}); err != nil {
		t.Fatalf("listing the fixture storage: %v", err)
	}

	if len(left) != 0 {
		t.Errorf("the probe left %v behind", left)
	}
}
