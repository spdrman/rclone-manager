package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Issue #227's end-to-end proof: reinstatements driven through the real
// operator entry point (ReinstateQuarantined) show up as a real number in
// the FR-24 report the `status` command and GET /api/v1/system/health both
// render, and the number is the population FR-15's delete gate actually
// refuses.
//
// The count is deliberately driven several times. A count asserted at zero
// or one is satisfied by a boolean, by a stale read, and by an off-by-one,
// and this one has to be right at three.

type reinstatementFixture struct {
	svc      *Service
	journal  *state.Journal
	set      model.BackupSetID
	localDir string
}

// stageArtifact walks one artifact down the real pipeline into the state
// stopAt, writing a real durable local file and recording the sha256 the
// reinstatement evidence rule requires, so nothing here is a hand-poked
// row that merely reads like a committed backup.
func (f reinstatementFixture) stageArtifact(t *testing.T, name string, stopAt lifecycle.State) model.ArtifactID {
	t.Helper()
	ctx := context.Background()

	artifact, err := model.NewArtifactID(f.set, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%s): %v", name, err)
	}

	payload := "payload for " + name
	local := filepath.Join(f.localDir, name)
	mustWriteFile(t, local, payload)
	sum := sha256.Sum256([]byte(payload))
	hashes := &state.HashUpdate{Alg: "sha256", Hash: hex.EncodeToString(sum[:])}

	if _, err := f.journal.Discover(ctx, artifact, name+"-discover", "/backups/"+name, state.RemoteIdentity{}, epoch); err != nil {
		t.Fatalf("Discover %s: %v", name, err)
	}

	deleted := epoch.Add(time.Hour)
	steps := []struct {
		from, to  lifecycle.State
		localPath *string
		hashes    *state.HashUpdate
		deletion  *state.DeletionUpdate
	}{
		{from: lifecycle.Discovered, to: lifecycle.Transferring},
		{from: lifecycle.Transferring, to: lifecycle.Transferred},
		{from: lifecycle.Transferred, to: lifecycle.Verifying},
		{from: lifecycle.Verifying, to: lifecycle.Verified, hashes: hashes},
		{from: lifecycle.Verified, to: lifecycle.Committing},
		{from: lifecycle.Committing, to: lifecycle.Committed, localPath: &local},
		{from: lifecycle.Committed, to: lifecycle.Quarantined},
		{from: lifecycle.Committed, to: lifecycle.RemoteDeletePending},
		// The remote delete really happened, so the journal records when.
		// That is the fact that makes a later QUARANTINED_LOST
		// reinstatement a restore point with no remote source left to
		// hold.
		{from: lifecycle.RemoteDeletePending, to: lifecycle.Complete, deletion: &state.DeletionUpdate{DeletedAt: &deleted}},
		{from: lifecycle.Complete, to: lifecycle.QuarantinedLost},
	}

	for i, s := range steps {
		if s.from == lifecycle.Committed && s.to == lifecycle.Quarantined && stopAt != lifecycle.Quarantined {
			continue // the branch into quarantine, only for a set-up that wants it
		}
		if _, err := f.journal.RecordTransition(ctx, state.Transition{
			Artifact:   artifact,
			Key:        fmt.Sprintf("%s-%d-%s", name, i, s.to),
			From:       string(s.from),
			To:         string(s.to),
			LocalPath:  s.localPath,
			Hashes:     s.hashes,
			Deletion:   s.deletion,
			OccurredAt: epoch.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("%s: %s -> %s: %v", name, s.from, s.to, err)
		}
		if s.to == stopAt {
			return artifact
		}
	}
	t.Fatalf("%s never reached %s", name, stopAt)
	return artifact
}

func newReinstatementFixture(t *testing.T) reinstatementFixture {
	t.Helper()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.StaleAfter = mustParseDuration(t, "24h")

	tr := newFakeTransport()
	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch.Add(2 * time.Hour))
	return reinstatementFixture{svc: svc, journal: journal, set: bs.ID, localDir: localDir}
}

func (f reinstatementFixture) reportedCount(t *testing.T) int {
	t.Helper()
	report, err := f.svc.BuildHealthReport(context.Background(), VersionInfo{})
	if err != nil {
		t.Fatalf("BuildHealthReport: %v", err)
	}
	if len(report.BackupSets) != 1 {
		t.Fatalf("BackupSets = %d, want 1", len(report.BackupSets))
	}
	return report.BackupSets[0].ReinstatedRemoteRetainedCount
}

// The whole issue in one test: three artifacts reinstated for real, one
// left quarantined as the control, one never distrusted, and the FR-24
// report says how many remote sources this manager is now holding forever.
func TestBuildHealthReport_CountsReinstatedArtifactsHoldingTheirRemoteSource(t *testing.T) {
	fx := newReinstatementFixture(t)
	ctx := context.Background()

	// Nothing reinstated yet. Asserted before anything is driven so the
	// number below is a change this test caused, not a constant.
	if got := fx.reportedCount(t); got != 0 {
		t.Fatalf("ReinstatedRemoteRetainedCount = %d before any reinstatement, want 0", got)
	}

	var reinstated []model.ArtifactID
	for _, name := range []string{"one.dump", "two.dump", "three.dump"} {
		a := fx.stageArtifact(t, name, lifecycle.Quarantined)
		res, err := fx.svc.ReinstateQuarantined(ctx, a, "the validator was misconfigured")
		if err != nil {
			t.Fatalf("ReinstateQuarantined(%s): %v", a, err)
		}
		if !res.Reinstated || res.NewState != lifecycle.Committed {
			t.Fatalf("ReinstateQuarantined(%s) = %+v, want an applied reinstatement into COMMITTED", a, res)
		}
		reinstated = append(reinstated, a)
	}

	// The control: quarantined, never reinstated. Present so the count
	// cannot pass by counting quarantine.
	stillQuarantined := fx.stageArtifact(t, "quarantined.dump", lifecycle.Quarantined)
	// And one that walked the happy path untouched.
	fx.stageArtifact(t, "untouched.dump", lifecycle.Committed)

	if got := fx.reportedCount(t); got != 3 {
		t.Fatalf("ReinstatedRemoteRetainedCount = %d, want 3: three artifacts were reinstated and none of their remote sources has been released", got)
	}

	// The count is the population FR-15's gate actually refuses. Every
	// reinstated artifact must be refused a remote delete by name, and the
	// quarantined control must not be counted.
	for _, a := range reinstated {
		_, err := lifecycle.DeleteRemote(ctx, fx.svc.lifecycleDeps(), lifecycle.DeleteRemoteRequest{
			Artifact:           a,
			AttemptKey:         "gate-check-" + a.Name,
			CompletionStrategy: "rename",
		})
		refusal, ok := lifecycle.AsRemoteDeleteRefusal(err)
		if !ok {
			t.Fatalf("DeleteRemote(%s) = %v, want a refusal: a counted artifact must be one the delete gate refuses", a, err)
		}
		if refusal.Check != "quarantine reinstatement" {
			t.Errorf("DeleteRemote(%s) refused on %q, want the reinstatement check: the report is counting a different population from the one the gate protects",
				a, refusal.Check)
		}
	}
	if _, err := fx.journal.Get(ctx, stillQuarantined); err != nil {
		t.Fatalf("Get(%s): %v", stillQuarantined, err)
	}
}

// A reinstatement out of QUARANTINED_LOST returns an artifact to COMPLETE,
// and COMPLETE is the state that says this manager already deleted the
// remote object. There is no source left to hold, so it must not be
// counted: an operator told to go and reclaim storage would find none.
//
// The first half is the positive control. Without it, "the lost one is not
// counted" would pass on a fixture where nothing at all was counted.
func TestBuildHealthReport_DoesNotCountAReinstatementWhoseRemoteIsAlreadyGone(t *testing.T) {
	fx := newReinstatementFixture(t)
	ctx := context.Background()

	held := fx.stageArtifact(t, "held.dump", lifecycle.Quarantined)
	if _, err := fx.svc.ReinstateQuarantined(ctx, held, "false positive"); err != nil {
		t.Fatalf("ReinstateQuarantined(%s): %v", held, err)
	}
	if got := fx.reportedCount(t); got != 1 {
		t.Fatalf("ReinstatedRemoteRetainedCount = %d after one reinstatement whose remote is still there, want 1", got)
	}

	lost := fx.stageArtifact(t, "lost.dump", lifecycle.QuarantinedLost)
	res, err := fx.svc.ReinstateQuarantined(ctx, lost, "the backup volume was not mounted when the check ran")
	if err != nil {
		t.Fatalf("ReinstateQuarantined(%s): %v", lost, err)
	}
	if !res.Reinstated || res.NewState != lifecycle.Complete {
		t.Fatalf("ReinstateQuarantined(%s) = %+v, want an applied reinstatement into COMPLETE", lost, res)
	}

	if got := fx.reportedCount(t); got != 1 {
		t.Fatalf("ReinstatedRemoteRetainedCount = %d after reinstating an artifact whose remote this manager already deleted, want 1: a remote that is gone is not a remote being held", got)
	}
}

// A journal that cannot answer the transition-log read must fail the
// report, not fill the count with a zero. Zero is the reassuring answer,
// and this whole issue exists because a permanently-growing population was
// being reported as nothing at all.
func TestBuildHealthReport_FailsRatherThanReportingZeroReinstatements(t *testing.T) {
	fx := newReinstatementFixture(t)
	boom := errors.New("database is locked")
	fx.svc.Journal = reinstatementReadFailingJournal{Journal: fx.svc.Journal, err: boom}

	if _, err := fx.svc.BuildHealthReport(context.Background(), VersionInfo{}); !errors.Is(err, boom) {
		t.Fatalf("BuildHealthReport error = %v, want it to wrap %v rather than report a count of zero", err, boom)
	}
}

type reinstatementReadFailingJournal struct {
	Journal
	err error
}

func (j reinstatementReadFailingJournal) ArtifactsWithAnyTransition(context.Context, model.BackupSetID, []state.TransitionEdge) ([]model.ArtifactID, error) {
	return nil, j.err
}
