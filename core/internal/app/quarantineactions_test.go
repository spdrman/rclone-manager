package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// quarantinedFixture is one discovered artifact walked to a quarantine
// state through the journal's own transition log.
//
// The route (DISCOVERED -> TRANSFERRING -> TRANSFERRED -> VERIFYING ->
// QUARANTINED) is every edge internal/lifecycle's Transitions table
// declares, in order, so this fixture stands the artifact somewhere the
// real pipeline can actually put it rather than somewhere only a test can.
type quarantinedFixture struct {
	svc      *Service
	journal  Journal
	artifact model.ArtifactID
	localDir string
}

func newQuarantinedFixture(t *testing.T, target lifecycle.State) quarantinedFixture {
	t.Helper()
	ctx := context.Background()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "quarantine-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload for quarantine", epoch.Unix())

	journal := openJournal(t)
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	var route []lifecycle.State
	switch target {
	case lifecycle.Quarantined:
		route = []lifecycle.State{lifecycle.Transferring, lifecycle.Transferred, lifecycle.Verifying, lifecycle.Quarantined}
	case lifecycle.QuarantinedLost:
		route = []lifecycle.State{
			lifecycle.Transferring, lifecycle.Transferred, lifecycle.Verifying, lifecycle.Verified,
			lifecycle.Committing, lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.Complete,
			lifecycle.QuarantinedLost,
		}
	default:
		t.Fatalf("newQuarantinedFixture: %s is not a quarantine state", target)
	}

	from := lifecycle.Discovered
	for i, to := range route {
		if _, err := journal.RecordTransition(ctx, state.Transition{
			Artifact:   rec.Artifact,
			Key:        "fixture-" + string(to),
			From:       string(from),
			To:         string(to),
			OccurredAt: epoch.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("RecordTransition(%s -> %s): %v", from, to, err)
		}
		from = to
	}

	got, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != string(target) {
		t.Fatalf("precondition failed: state = %q, want %q", got.State, target)
	}

	return quarantinedFixture{svc: svc, journal: journal, artifact: rec.Artifact, localDir: localDir}
}

// TestRevalidateQuarantined_ReportsAVerdictAndWritesNothing is the whole
// contract of the operator-facing "Revalidate" action. The write assertion
// is the load-bearing half: the lifecycle graph has no edge from
// QUARANTINED to any healthy state, so a pass that quietly moved the
// artifact would be moving it somewhere illegal, and a pass that moved it
// nowhere but SAID it had would be worse.
func TestRevalidateQuarantined_ReportsAVerdictAndWritesNothing(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.Quarantined)
	ctx := context.Background()

	// The artifact never reached COMMITTED, so no local final file was
	// recorded; give it one so the check has something real to look at.
	local := fx.localDir + "/backup.dump"
	if err := os.WriteFile(local, []byte("payload for quarantine"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := fx.journal.RecordTransition(ctx, state.Transition{
		Artifact:   fx.artifact,
		Key:        "fixture-local-path",
		From:       string(lifecycle.Quarantined),
		To:         string(lifecycle.Quarantined),
		LocalPath:  &local,
		OccurredAt: epoch,
	}); err != nil {
		t.Fatalf("recording the local path: %v", err)
	}

	before, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	result, err := fx.svc.RevalidateQuarantined(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("RevalidateQuarantined: %v", err)
	}
	if !result.Checked {
		t.Errorf("Checked = false, so nothing was actually examined: %+v", result)
	}
	if !result.Passed {
		t.Errorf("Passed = false for an intact local file: %+v", result)
	}
	if result.Reason == "" {
		t.Error("Reason is empty, so an operator is given a verdict with no evidence")
	}

	after, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != string(lifecycle.Quarantined) {
		t.Errorf("State = %q, want it left in %q: a pass may not rehabilitate a quarantined artifact", after.State, lifecycle.Quarantined)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("UpdatedAt moved from %s to %s: revalidate writes nothing", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestRevalidateQuarantined_FailsWhenTheLocalCopyIsGone is the negative
// half of the pair above: without it, "Passed" could be a constant.
func TestRevalidateQuarantined_FailsWhenTheLocalCopyIsGone(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.Quarantined)
	ctx := context.Background()

	missing := fx.localDir + "/never-written.dump"
	if _, err := fx.journal.RecordTransition(ctx, state.Transition{
		Artifact:   fx.artifact,
		Key:        "fixture-missing-local-path",
		From:       string(lifecycle.Quarantined),
		To:         string(lifecycle.Quarantined),
		LocalPath:  &missing,
		OccurredAt: epoch,
	}); err != nil {
		t.Fatalf("recording the local path: %v", err)
	}

	result, err := fx.svc.RevalidateQuarantined(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("RevalidateQuarantined: %v", err)
	}
	if result.Passed {
		t.Errorf("Passed = true for a local file that is not there: %+v", result)
	}
}

func TestRevalidateQuarantined_RefusesAnArtifactThatIsNotQuarantined(t *testing.T) {
	fx := newCommittedFixture(t)

	_, err := fx.svc.RevalidateQuarantined(context.Background(), fx.artifact)
	if !errors.Is(err, ErrNotQuarantined) {
		t.Fatalf("error = %v, want ErrNotQuarantined", err)
	}
}

// TestRetryQuarantinedIngestion_ReturnsTheArtifactToDiscovered drives the
// one recovery edge the lifecycle graph gives QUARANTINED.
func TestRetryQuarantinedIngestion_ReturnsTheArtifactToDiscovered(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.Quarantined)
	ctx := context.Background()

	if err := fx.svc.RetryQuarantinedIngestion(ctx, fx.artifact); err != nil {
		t.Fatalf("RetryQuarantinedIngestion: %v", err)
	}

	got, err := fx.journal.Get(ctx, fx.artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != string(lifecycle.Discovered) {
		t.Fatalf("State = %q, want %q", got.State, lifecycle.Discovered)
	}

	// The move is durable and auditable, not only in memory: it shows up
	// in the append-only transition log the activity feed reads.
	activity, err := fx.journal.(*state.Journal).RecentActivity(ctx, 5)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(activity) == 0 || activity[0].To != string(lifecycle.Discovered) {
		t.Fatalf("newest transition = %+v, want one into DISCOVERED", activity)
	}
	if !strings.Contains(activity[0].Detail, "operator-triggered retry") {
		t.Errorf("Detail = %q, want it to say who asked for this", activity[0].Detail)
	}
}

// TestRetryQuarantinedIngestion_RefusesAnIrrecoverableLoss. QUARANTINED_LOST
// is reached only from COMPLETE, which confirms the remote source is gone,
// so there is nothing to re-ingest and the graph gives that state no
// outgoing edge at all.
func TestRetryQuarantinedIngestion_RefusesAnIrrecoverableLoss(t *testing.T) {
	fx := newQuarantinedFixture(t, lifecycle.QuarantinedLost)
	ctx := context.Background()

	err := fx.svc.RetryQuarantinedIngestion(ctx, fx.artifact)
	if !errors.Is(err, ErrQuarantineIrrecoverable) {
		t.Fatalf("error = %v, want ErrQuarantineIrrecoverable", err)
	}

	got, gotErr := fx.journal.Get(ctx, fx.artifact)
	if gotErr != nil {
		t.Fatalf("Get: %v", gotErr)
	}
	if got.State != string(lifecycle.QuarantinedLost) {
		t.Errorf("State = %q, want it left in %q", got.State, lifecycle.QuarantinedLost)
	}
}

func TestRetryQuarantinedIngestion_RefusesAnArtifactThatIsNotQuarantined(t *testing.T) {
	fx := newCommittedFixture(t)

	err := fx.svc.RetryQuarantinedIngestion(context.Background(), fx.artifact)
	if !errors.Is(err, ErrNotQuarantined) {
		t.Fatalf("error = %v, want ErrNotQuarantined", err)
	}
}
