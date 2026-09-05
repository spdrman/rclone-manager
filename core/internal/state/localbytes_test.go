package state

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// This file covers LocalBytesInUse, the measurement issue #286's storage
// cap is enforced from: how much space this manager itself is occupying.
//
// It is a catalog read on purpose. The alternative, walking the backup
// root, is slow on a large tree and counts every file anything else put
// there; this journal already records what this manager transferred and how
// big each one was, so summing it is one aggregate query and it measures
// OUR consumption specifically.

// holdsLocalCopy is the state list core/internal/lifecycle owns. It is
// spelled out here rather than imported because lifecycle imports this
// package; lifecycle's own TestEveryStateIsClassifiedAsHoldingALocalCopyOrNot
// is what keeps the real list honest, and these tests only need A list to
// drive the query with.
var holdsLocalCopy = []string{
	"TRANSFERRING", "TRANSFERRED", "VERIFYING", "VERIFIED",
	"COMMITTING", "COMMITTED", "REMOTE_DELETE_PENDING", "COMPLETE",
	"REMOTE_RETAINED", "QUARANTINED", "QUARANTINED_LOST",
}

// discoverSized records an artifact whose REMOTE size is known and whose
// transfer has not happened. That is the interesting starting point for
// every test here: the two sizes LocalBytesInUse chooses between are set
// separately, so a fixture that populated both at once could not tell which
// one the query preferred.
func discoverSized(t *testing.T, j *Journal, name string, size int64) model.ArtifactID {
	t.Helper()
	set, err := model.NewBackupSetID("production", "postgres")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := j.Discover(context.Background(), artifact, "discover:"+name, "/incoming/"+name,
		RemoteIdentity{Size: &size}, time.Now()); err != nil {
		t.Fatalf("Discover(%s): %v", name, err)
	}
	return artifact
}

// advance moves an artifact one state and optionally attaches a transfer
// result. transfer is a pointer so a test can move an artifact WITHOUT
// recording bytes, which is the case where the query has to fall back to
// the remote size.
func advance(t *testing.T, j *Journal, artifact model.ArtifactID, from, to string, transfer *TransferResult) {
	t.Helper()
	if _, err := j.RecordTransition(context.Background(), Transition{
		Artifact:   artifact,
		Key:        artifact.Name + ":" + to,
		From:       from,
		To:         to,
		OccurredAt: time.Now(),
		Transfer:   transfer,
	}); err != nil {
		t.Fatalf("RecordTransition(%s -> %s): %v", from, to, err)
	}
}

// TestLocalBytesInUseIsZeroOnAnEmptyJournal is the fresh-install reading,
// and it has to be a real zero rather than an error: a deployment that has
// never transferred anything genuinely is using nothing.
func TestLocalBytesInUseIsZeroOnAnEmptyJournal(t *testing.T) {
	j, _ := openJournal(t)
	got, err := j.LocalBytesInUse(context.Background(), holdsLocalCopy)
	if err != nil {
		t.Fatalf("LocalBytesInUse: %v", err)
	}
	if got != 0 {
		t.Errorf("LocalBytesInUse = %d, want 0", got)
	}
}

// TestADiscoveredArtifactOccupiesNothing is the boundary the whole
// measurement turns on. An artifact noticed on the remote has a size, and
// counting that size would report space this manager has not used.
func TestADiscoveredArtifactOccupiesNothing(t *testing.T) {
	j, _ := openJournal(t)
	discoverSized(t, j, "pg-1.dump", 5_000_000_000)

	got, err := j.LocalBytesInUse(context.Background(), holdsLocalCopy)
	if err != nil {
		t.Fatalf("LocalBytesInUse: %v", err)
	}
	if got != 0 {
		t.Errorf("LocalBytesInUse = %d, want 0: DISCOVERED means noticed on the remote, not written to disk", got)
	}
}

// TestATransferredArtifactCountsItsTransferredBytes prefers what the copy
// actually wrote over what the remote listing claimed. The two differ when
// a listing was stale, and only one of them is on this disk.
func TestATransferredArtifactCountsItsTransferredBytes(t *testing.T) {
	j, _ := openJournal(t)
	a := discoverSized(t, j, "pg-1.dump", 900)
	advance(t, j, a, "DISCOVERED", "TRANSFERRING", nil)
	advance(t, j, a, "TRANSFERRING", "TRANSFERRED", &TransferResult{BytesTransferred: 1000})

	got, err := j.LocalBytesInUse(context.Background(), holdsLocalCopy)
	if err != nil {
		t.Fatalf("LocalBytesInUse: %v", err)
	}
	if got != 1000 {
		t.Errorf("LocalBytesInUse = %d, want 1000 (what the copy wrote, not the 900 the listing claimed)", got)
	}
}

// TestAnInFlightTransferFallsBackToTheRemoteSize: at TRANSFERRING there is
// no transferred-bytes figure yet, and the artifact's eventual footprint is
// the best available answer. Falling back to zero would let a cap be
// breached by exactly the transfers in flight.
func TestAnInFlightTransferFallsBackToTheRemoteSize(t *testing.T) {
	j, _ := openJournal(t)
	a := discoverSized(t, j, "pg-1.dump", 4096)
	advance(t, j, a, "DISCOVERED", "TRANSFERRING", nil)

	got, err := j.LocalBytesInUse(context.Background(), holdsLocalCopy)
	if err != nil {
		t.Fatalf("LocalBytesInUse: %v", err)
	}
	if got != 4096 {
		t.Errorf("LocalBytesInUse = %d, want 4096 (the size the .partial is heading for)", got)
	}
}

// TestSizesSumAcrossArtifactsAndStates is the ordinary case: several
// artifacts, in several of the states that hold a local copy, add up.
func TestSizesSumAcrossArtifactsAndStates(t *testing.T) {
	j, _ := openJournal(t)

	a := discoverSized(t, j, "a.dump", 100)
	advance(t, j, a, "DISCOVERED", "TRANSFERRING", nil)
	advance(t, j, a, "TRANSFERRING", "TRANSFERRED", &TransferResult{BytesTransferred: 100})
	advance(t, j, a, "TRANSFERRED", "VERIFYING", nil)
	advance(t, j, a, "VERIFYING", "VERIFIED", nil)
	advance(t, j, a, "VERIFIED", "COMMITTING", nil)
	advance(t, j, a, "COMMITTING", "COMMITTED", nil)

	b := discoverSized(t, j, "b.dump", 20)
	advance(t, j, b, "DISCOVERED", "TRANSFERRING", nil)
	advance(t, j, b, "TRANSFERRING", "TRANSFERRED", &TransferResult{BytesTransferred: 20})
	advance(t, j, b, "TRANSFERRED", "VERIFYING", nil)
	advance(t, j, b, "VERIFYING", "QUARANTINED", nil)

	c := discoverSized(t, j, "c.dump", 7)

	got, err := j.LocalBytesInUse(context.Background(), holdsLocalCopy)
	if err != nil {
		t.Fatalf("LocalBytesInUse: %v", err)
	}
	if got != 120 {
		t.Errorf("LocalBytesInUse = %d, want 120 (100 committed + 20 quarantined; %s is still only DISCOVERED)", got, c.Name)
	}
}

// TestAnArtifactWithNoRecordedSizeCountsAsZeroRatherThanFailing keeps one
// backend that reports no size from making the whole measurement
// unavailable. RemoteIdentity's fields are all optional by contract.
func TestAnArtifactWithNoRecordedSizeCountsAsZeroRatherThanFailing(t *testing.T) {
	j, _ := openJournal(t)
	set, err := model.NewBackupSetID("production", "postgres")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	a, err := model.NewArtifactID(set, "sizeless.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := j.Discover(context.Background(), a, "d", "/incoming/sizeless.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	advance(t, j, a, "DISCOVERED", "TRANSFERRING", nil)

	got, err := j.LocalBytesInUse(context.Background(), holdsLocalCopy)
	if err != nil {
		t.Fatalf("LocalBytesInUse: %v", err)
	}
	if got != 0 {
		t.Errorf("LocalBytesInUse = %d, want 0", got)
	}
}

// TestAnEmptyStateListIsRefused: a query with no states matches nothing and
// would report a confident zero, which reads as "this manager is using no
// space at all" and hands a caller the whole cap as headroom. A caller that
// meant "nothing holds a local copy" has to say so somewhere other than by
// passing an empty slice.
func TestAnEmptyStateListIsRefused(t *testing.T) {
	j, _ := openJournal(t)
	if _, err := j.LocalBytesInUse(context.Background(), nil); err == nil {
		t.Fatal("LocalBytesInUse with no states = nil error, want a refusal rather than a confident zero")
	}
}
