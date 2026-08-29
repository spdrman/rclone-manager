package reconcile

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// idempotenceFixture is one artifact, already journaled through driveTo,
// plus the Stat response its remote path should give both times Reconcile
// runs against it. Every fixture below is built to be answerable by a
// fixed, replayable Stat function: idempotence has to hold for a clock and
// a transport that never change between the two calls, or the test would
// not actually be proving anything about this package.
type idempotenceFixture struct {
	name      string
	artifact  model.ArtifactID
	stat      func() (transport.RemoteArtifact, error)
	wantAfter lifecycle.State
}

func buildIdempotenceFixtures(t *testing.T, j *state.Journal) []idempotenceFixture {
	t.Helper()

	discovered := testArtifact(t, "idem-discovered.dump")
	size := int64(20)
	driveTo(t, j, driveParams{artifact: discovered, remote: state.RemoteIdentity{Size: &size}, stopAt: lifecycle.Discovered})

	transferring := testArtifact(t, "idem-transferring.dump")
	driveTo(t, j, driveParams{
		artifact: transferring, remote: state.RemoteIdentity{Size: &size}, localPath: writeLocalFile(t, size),
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Transferring,
	})

	committedValid := testArtifact(t, "idem-committed-valid.dump")
	driveTo(t, j, driveParams{
		artifact: committedValid, remote: state.RemoteIdentity{Size: &size}, localPath: writeLocalFile(t, size),
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Committed,
	})

	committedInvalid := testArtifact(t, "idem-committed-invalid.dump")
	driveTo(t, j, driveParams{
		artifact: committedInvalid, remote: state.RemoteIdentity{Size: &size}, localPath: t.TempDir() + "/missing.final",
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Committed,
	})

	pendingAbsentValid := testArtifact(t, "idem-pending-absent-valid.dump")
	driveTo(t, j, driveParams{
		artifact: pendingAbsentValid, remote: state.RemoteIdentity{Size: &size}, localPath: writeLocalFile(t, size),
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteDeletePending,
	})

	pendingAbsentInvalid := testArtifact(t, "idem-pending-absent-invalid.dump")
	driveTo(t, j, driveParams{
		artifact: pendingAbsentInvalid, remote: state.RemoteIdentity{Size: &size}, localPath: t.TempDir() + "/missing.final",
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteDeletePending,
	})

	pendingPresentInvalid := testArtifact(t, "idem-pending-present-invalid.dump")
	driveTo(t, j, driveParams{
		artifact: pendingPresentInvalid, remote: state.RemoteIdentity{Size: &size}, localPath: t.TempDir() + "/missing.final",
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteDeletePending,
	})

	pendingChanged := testArtifact(t, "idem-pending-changed.dump")
	driveTo(t, j, driveParams{
		artifact: pendingChanged, remote: state.RemoteIdentity{Size: &size, Hash: "aaaa", HashAlg: "sha256"},
		localPath: writeLocalFile(t, size), transfer: &state.TransferResult{BytesTransferred: size},
		stopAt: lifecycle.RemoteDeletePending,
	})

	pendingUnchanged := testArtifact(t, "idem-pending-unchanged.dump")
	driveTo(t, j, driveParams{
		artifact: pendingUnchanged, remote: state.RemoteIdentity{Size: &size, Hash: "cccc", HashAlg: "sha256"},
		localPath: writeLocalFile(t, size), transfer: &state.TransferResult{BytesTransferred: size},
		stopAt: lifecycle.RemoteDeletePending,
	})

	completeValid := testArtifact(t, "idem-complete-valid.dump")
	driveTo(t, j, driveParams{
		artifact: completeValid, remote: state.RemoteIdentity{Size: &size}, localPath: writeLocalFile(t, size),
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Complete,
	})

	completeInvalid := testArtifact(t, "idem-complete-invalid.dump")
	driveTo(t, j, driveParams{
		artifact: completeInvalid, remote: state.RemoteIdentity{Size: &size}, localPath: t.TempDir() + "/missing.final",
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Complete,
	})

	quarantined := testArtifact(t, "idem-quarantined.dump")
	driveTo(t, j, driveParams{
		artifact: quarantined, remote: state.RemoteIdentity{Size: &size}, localPath: writeLocalFile(t, size),
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Quarantined,
	})

	quarantinedLost := testArtifact(t, "idem-quarantined-lost.dump")
	driveTo(t, j, driveParams{
		artifact: quarantinedLost, remote: state.RemoteIdentity{Size: &size}, localPath: writeLocalFile(t, size),
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.QuarantinedLost,
	})

	failed := driveToFailed(t, j, testArtifact(t, "idem-failed.dump"))

	return []idempotenceFixture{
		{"discovered", discovered, nil, lifecycle.Discovered},
		{"transferring", transferring, nil, lifecycle.Transferring},
		{"committed-valid", committedValid, nil, lifecycle.Committed},
		{"committed-invalid", committedInvalid, nil, lifecycle.Quarantined}, // converges on run 1
		{"pending-absent-valid", pendingAbsentValid, statNotFound, lifecycle.Complete},
		{"pending-absent-invalid", pendingAbsentInvalid, statNotFound, lifecycle.QuarantinedLost},
		{"pending-present-invalid", pendingPresentInvalid, func() (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{Path: remotePathFor(pendingPresentInvalid), Size: size}, nil
		}, lifecycle.Quarantined},
		{"pending-changed", pendingChanged, func() (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{Path: remotePathFor(pendingChanged), Size: size, Hash: "bbbb", HashAlg: transport.SHA256}, nil
		}, lifecycle.RemoteDeletePending},
		{"pending-unchanged", pendingUnchanged, func() (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{Path: remotePathFor(pendingUnchanged), Size: size, Hash: "cccc", HashAlg: transport.SHA256}, nil
		}, lifecycle.RemoteDeletePending},
		{"complete-valid", completeValid, nil, lifecycle.Complete},
		{"complete-invalid", completeInvalid, nil, lifecycle.QuarantinedLost}, // converges on run 1
		{"quarantined", quarantined, nil, lifecycle.Quarantined},
		{"quarantined-lost", quarantinedLost, nil, lifecycle.QuarantinedLost},
		{"failed", failed.Artifact, nil, lifecycle.Failed},
	}
}

func snapshot(t *testing.T, j *state.Journal, fixtures []idempotenceFixture) map[string]state.Record {
	t.Helper()
	out := make(map[string]state.Record, len(fixtures))
	for _, f := range fixtures {
		rec, err := j.Get(context.Background(), f.artifact)
		if err != nil {
			t.Fatalf("Get(%s): %v", f.artifact, err)
		}
		out[f.name] = rec
	}
	return out
}

// TestReconcile_IsIdempotentAcrossTheWholeTable runs Reconcile twice, back
// to back, over one journal holding an instance of every reachable
// scenario in FR-17's table, the two rows this package adds, and every
// exceptional/terminal state left outside its scope. It is the direct test
// of the property the issue calls out as the one to test hardest: running
// reconciliation twice in a row must produce the same result as running it
// once.
func TestReconcile_IsIdempotentAcrossTheWholeTable(t *testing.T) {
	j := openTestJournal(t)
	fixtures := buildIdempotenceFixtures(t, j)

	stat := make(map[string]func() (transport.RemoteArtifact, error), len(fixtures))
	for _, f := range fixtures {
		if f.stat != nil {
			stat[remotePathFor(f.artifact)] = f.stat
		}
	}

	// A frozen clock: if a second run somehow performed a write it should
	// not have, freezing time means it could not slip past the snapshot
	// comparison below by coincidentally landing on a different, but still
	// plausible-looking, timestamp.
	frozen := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	deps := Deps{Journal: j, Transport: &fakeTransport{stat: stat}, Now: func() time.Time { return frozen }}

	firstReport, err := Reconcile(context.Background(), deps, testSource, testSet(t))
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	requireNoErrors(t, firstReport)

	for _, f := range fixtures {
		rec, err := j.Get(context.Background(), f.artifact)
		if err != nil {
			t.Fatalf("Get(%s) after first run: %v", f.artifact, err)
		}
		if rec.State != string(f.wantAfter) {
			t.Errorf("%s: state after first Reconcile = %s, want %s", f.name, rec.State, f.wantAfter)
		}
	}

	afterFirst := snapshot(t, j, fixtures)

	secondReport, err := Reconcile(context.Background(), deps, testSource, testSet(t))
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	requireNoErrors(t, secondReport)

	for _, f := range secondReport.Findings {
		if f.Changed() {
			t.Errorf("second Reconcile: %s changed From=%s To=%s, want no change on a repeat run", f.Artifact, f.From, f.To)
		}
	}

	afterSecond := snapshot(t, j, fixtures)
	for _, f := range fixtures {
		if !reflect.DeepEqual(afterFirst[f.name], afterSecond[f.name]) {
			t.Errorf("%s: journal record changed between the first and second Reconcile call\n  after first:  %+v\n  after second: %+v",
				f.name, afterFirst[f.name], afterSecond[f.name])
		}
	}
}

// TestReconcile_IsIdempotentOnAnAlreadyConsistentJournal is the narrower
// claim the issue calls out by name: it must be safe to run reconciliation
// against a journal that already agrees with local files and remote state,
// with nothing at all to fix, on the very first call, not just the second.
func TestReconcile_IsIdempotentOnAnAlreadyConsistentJournal(t *testing.T) {
	j := openTestJournal(t)
	size := int64(40)

	discovered := testArtifact(t, "consistent-discovered.dump")
	driveTo(t, j, driveParams{artifact: discovered, remote: state.RemoteIdentity{Size: &size}, stopAt: lifecycle.Discovered})

	committed := testArtifact(t, "consistent-committed.dump")
	driveTo(t, j, driveParams{
		artifact: committed, remote: state.RemoteIdentity{Size: &size}, localPath: writeLocalFile(t, size),
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Committed,
	})

	complete := testArtifact(t, "consistent-complete.dump")
	driveTo(t, j, driveParams{
		artifact: complete, remote: state.RemoteIdentity{Size: &size}, localPath: writeLocalFile(t, size),
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Complete,
	})

	deps := Deps{Journal: j, Transport: &fakeTransport{}}

	for run := 1; run <= 2; run++ {
		report, err := Reconcile(context.Background(), deps, testSource, testSet(t))
		if err != nil {
			t.Fatalf("run %d: Reconcile: %v", run, err)
		}
		requireNoErrors(t, report)
		for _, f := range report.Findings {
			if f.Changed() {
				t.Errorf("run %d: %s changed From=%s To=%s, want no change against an already-consistent journal", run, f.Artifact, f.From, f.To)
			}
		}
	}
}

// TestReconcileToComplete_KeyReplayIsSafeUnderARace exercises the
// mechanism idempotence actually rests on, not just its externally
// observable effect. It calls the same reconciliation-driven transition
// twice against the exact same, stale journal snapshot, the shape of a
// crash between two concurrent or restarted attempts that both read the
// journal before either one wrote to it, and confirms the journal's own
// idempotency-key replay (not a check this package performs itself) is
// what keeps the second call from either failing or double-applying.
func TestReconcileToComplete_KeyReplayIsSafeUnderARace(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "race.dump")
	size := int64(50)
	localPath := writeLocalFile(t, size)
	rec := driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.RemoteDeletePending,
	})

	deps := Deps{Journal: j, Transport: &fakeTransport{}, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}

	first, err := reconcileToComplete(context.Background(), deps, rec)
	if err != nil {
		t.Fatalf("first reconcileToComplete: %v", err)
	}
	if first.State != string(lifecycle.Complete) {
		t.Fatalf("first call state = %s, want COMPLETE", first.State)
	}

	// Reuse the exact same stale rec, as a second caller who read the
	// journal before the first call's write would. reconcileKey derives
	// the same key from rec.UpdatedAt both times, so this must replay
	// rather than fail with a stale From, or worse, apply the deletion
	// bookkeeping a second time.
	second, err := reconcileToComplete(context.Background(), deps, rec)
	if err != nil {
		t.Fatalf("second reconcileToComplete (replay): %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("replayed call returned a different record:\n  first:  %+v\n  second: %+v", first, second)
	}
}

func TestReconcileKey_IsDeterministicAndDistinctPerEdge(t *testing.T) {
	artifact := testArtifact(t, "key.dump")
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	k1 := reconcileKey(artifact, lifecycle.RemoteDeletePending, lifecycle.Complete, when)
	k2 := reconcileKey(artifact, lifecycle.RemoteDeletePending, lifecycle.Complete, when)
	if k1 != k2 {
		t.Errorf("reconcileKey is not deterministic: %q != %q", k1, k2)
	}

	k3 := reconcileKey(artifact, lifecycle.Complete, lifecycle.QuarantinedLost, when)
	if k1 == k3 {
		t.Errorf("reconcileKey collided across different edges: %q", k1)
	}

	k4 := reconcileKey(artifact, lifecycle.RemoteDeletePending, lifecycle.Complete, when.Add(time.Second))
	if k1 == k4 {
		t.Errorf("reconcileKey collided across different UpdatedAt values: %q", k1)
	}
}
