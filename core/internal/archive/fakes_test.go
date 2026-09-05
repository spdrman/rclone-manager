package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file holds the doubles the rest of this package's tests are built
// on, in one place rather than one copy per file.
//
// That is not tidiness. The counters below are how this package proves the
// things that must NOT happen, and a counter is only worth having if every
// test that reads it agrees about what increments it. Two near-identical
// doubles in two files is how "a read never starts a restore" ends up true
// in one suite and quietly untested in the other.

func glacierMedium() transport.Medium {
	return transport.Medium{ID: "cold-store", Type: transport.MediumTypeS3, Bucket: "b", StorageClass: config.StorageClassGlacier}
}

// fakeMedium is one object on one pretend medium, plus a call counter for
// every method a test might want to prove was NOT reached.
//
// The counters are the point of it. Half of what this package promises is
// about things that must not happen: a read must not start a restore, and
// a refusal must be decided from held facts rather than from a request
// nobody should have spent. A double that only returns canned answers
// cannot show either, because "returned the right refusal" and "returned
// the right refusal after doing the expensive thing anyway" look
// identical from the outside.
type fakeMedium struct {
	mu sync.Mutex

	// content is the object's bytes.
	content []byte

	// restore is what the medium reports about a restore of it, or nil.
	restore *RestoreState

	// statErr / openErr / checksumErr / restoreStatusErr / initiateErr
	// each force one method to fail.
	statErr          error
	openErr          error
	checksumErr      error
	restoreStatusErr error
	initiateErr      error

	// attestation is what ObjectChecksum returns when it does not fail.
	attestation string

	stats           int
	opens           int
	checksums       int
	restoreStatuses int
	initiates       int

	// initiatedWindows records the window of every restore actually
	// asked for, so a test can prove a second one did not happen.
	initiatedWindows []int
}

// counts reports every call this double has taken.
//
// Only initiates is read today, and it is what "a read never starts a
// restore" comes down to: an absence of error cannot say that and a
// counter can. The five are separated rather than summed because they are
// not equivalent. Asking whether a restore is running moves no bytes,
// starts nothing and is the one call this package's own refusals are
// allowed to spend, so folding it in with stats, opens and checksums
// would make "nothing went to the medium about the object" unaskable.
func (f *fakeMedium) counts() (stats, opens, checksums, restoreStatuses, initiates int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, f.opens, f.checksums, f.restoreStatuses, f.initiates
}

func (f *fakeMedium) StatObject(_ context.Context, _ transport.Medium, _ string) (transport.ObjectInfo, error) {
	f.mu.Lock()
	f.stats++
	f.mu.Unlock()
	if f.statErr != nil {
		return transport.ObjectInfo{}, f.statErr
	}
	return transport.ObjectInfo{Size: int64(len(f.content))}, nil
}

func (f *fakeMedium) OpenObject(_ context.Context, _ transport.Medium, _ string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	if f.openErr != nil {
		return nil, f.openErr
	}
	return io.NopCloser(bytes.NewReader(f.content)), nil
}

func (f *fakeMedium) ObjectChecksum(_ context.Context, _ transport.Medium, _ string, _ transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	f.mu.Lock()
	f.checksums++
	f.mu.Unlock()
	if f.checksumErr != nil {
		return transport.ChecksumAttestation{}, f.checksumErr
	}
	return transport.ChecksumAttestation{Algorithm: transport.SHA256, Value: f.attestation}, nil
}

func (f *fakeMedium) RestoreStatus(_ context.Context, _ transport.Medium, _ string) (*RestoreState, error) {
	f.mu.Lock()
	f.restoreStatuses++
	current := f.restore
	f.mu.Unlock()
	if f.restoreStatusErr != nil {
		return nil, f.restoreStatusErr
	}
	return current, nil
}

func (f *fakeMedium) InitiateRestore(_ context.Context, _ transport.Medium, _ string, windowDays int) error {
	f.mu.Lock()
	f.initiates++
	f.initiatedWindows = append(f.initiatedWindows, windowDays)
	f.mu.Unlock()
	if f.initiateErr != nil {
		return f.initiateErr
	}
	f.mu.Lock()
	f.restore = &RestoreState{InProgress: true}
	f.mu.Unlock()
	return nil
}

// hashOf is the sha256 a placement records for content.
func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mediumPlacement is a placement row on a storage medium, verified at
// class, which is the shape almost every test here starts from.
func mediumPlacement(medium, key string, size int64, hash, class string) state.Placement {
	sz := size
	return state.Placement{
		Medium:            medium,
		Location:          key,
		Size:              &sz,
		Hash:              hash,
		HashAlg:           string(transport.SHA256),
		VerificationClass: class,
		VerifiedAt:        ptrTime(testNow.Add(-24 * time.Hour)),
		Status:            state.PlacementActive,
		CreatedAt:         testNow.Add(-48 * time.Hour),
		UpdatedAt:         testNow.Add(-24 * time.Hour),
	}
}

// localPlacement is the ordinary local copy, content-verified, which is
// what migration 0007 backfills for every artifact in every existing
// deployment.
func localPlacement(path string, size int64, hash string) state.Placement {
	p := mediumPlacement(state.MediumLocal, path, size, hash, state.VerificationContent)
	return p
}
