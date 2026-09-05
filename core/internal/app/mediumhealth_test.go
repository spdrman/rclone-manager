package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/health"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Issue #444. The story the issue tells is an operator opening a status
// page on a deployment nobody has run `backup-manager run` in front of,
// whose moves have been failing for a week, and being told everything is
// fine. These tests are that story end to end: a real cycle, the real
// move engine, a real medium that refuses the upload, and the verdict
// `backup-manager status` would print a week later.
//
// Nothing here plants a move row by hand. A test that writes its own
// journal rows proves the reader can read what the test wrote; the whole
// question is whether the shape production actually leaves behind is
// visible, so the failure is planted in the transport and the engine
// writes whatever it writes.

// healthMovingService is movingService plus a freshness window, because
// FR-24's verdict is decided against stale_after and the move fixtures do
// not set one. A set whose window is zero is STALE the instant it is
// looked at, which would hide the exact verdict change these tests are
// about behind a worse one.
func healthMovingService(t *testing.T, medium transport.MediumStore) (*Service, config.BackupSet, *state.Journal) {
	t.Helper()
	dir := t.TempDir()
	journal := openJournal(t)
	bs := testBackupSet(t, dir)
	bs.StaleAfter = config.Duration(24 * time.Hour)
	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = chainWithOffsiteMonthly()
	cfg.StorageMediums = moveTestMediums()
	resolveTestRetention(cfg)

	svc := New(cfg, journal, newFakeTransport(), nil)
	svc.MediumStore = medium
	svc.Now = fixedNow(retentionTestNow)
	return svc, bs, journal
}

// seedForMoveHealth gives the set one artifact that is fresh (so its
// freshness verdict is HEALTHY and cannot mask anything) and one old
// enough that the monthly tier claims it, which is the artifact whose
// home is offsite and which therefore has to move.
func seedForMoveHealth(t *testing.T, ctx context.Context, journal *state.Journal, bs config.BackupSet) model.ArtifactID {
	t.Helper()
	seedMovableArtifact(t, ctx, journal, bs, "fresh.dump", retentionTestNow.Add(-time.Hour))
	return seedMovableArtifact(t, ctx, journal, bs, "monthly-only.dump", retentionTestNow.AddDate(0, 0, -40))
}

func healthFor(t *testing.T, ctx context.Context, svc *Service, set model.BackupSetID) health.BackupSetHealth {
	t.Helper()
	report, err := svc.BuildHealthReport(ctx, VersionInfo{})
	if err != nil {
		t.Fatalf("BuildHealthReport: %v", err)
	}
	for _, bs := range report.BackupSets {
		if bs.Set == set {
			return bs
		}
	}
	t.Fatalf("no health for %s in %+v", set, report.BackupSets)
	return health.BackupSetHealth{}
}

// TestBuildHealthReport_AWeekOfFailingMovesReadsDegraded is the issue's
// acceptance line. The control runs first, against a medium that works,
// because a DEGRADED verdict from the failing run proves nothing unless
// the identical deployment reads HEALTHY when the move lands.
func TestBuildHealthReport_AWeekOfFailingMovesReadsDegraded(t *testing.T) {
	ctx := context.Background()

	working, bs, journal := healthMovingService(t, newCountingMedium())
	seedForMoveHealth(t, ctx, journal, bs)
	if report := working.RunCycle(ctx); report.Moves.Completed != 1 {
		t.Fatalf("control: Moves = %+v, want the one planned move to have completed; without a working control the assertion below is about nothing", report.Moves)
	}
	control := healthFor(t, ctx, working, bs.ID)
	if control.State != health.Healthy {
		t.Fatalf("control State = %s (%s), want HEALTHY: this deployment's backups are fresh and everything is where the chain says it belongs",
			control.State, control.Reason)
	}
	if control.Placement != (health.PlacementHealth{}) {
		t.Fatalf("control Placement = %+v, want the zero value once the move has landed", control.Placement)
	}

	// The same deployment, one bucket policy away.
	svc, bs, journal := healthMovingService(t, newRefusingMedium())
	artifact := seedForMoveHealth(t, ctx, journal, bs)
	if report := svc.RunCycle(ctx); report.Moves.Planned != 1 {
		t.Fatalf("Moves = %+v, want the chain's move to have been planned", report.Moves)
	}

	// A week goes by. Nobody runs anything in front of a terminal, the
	// backups keep landing exactly as they should, and the cycle keeps
	// failing to move the one artifact whose home is offsite.
	//
	// The fresh artifact is re-seeded at the later clock deliberately: a
	// deployment whose backups had ALSO stopped would read STALE, which is
	// a worse verdict and would hide the one this test is about. The
	// defect being fixed is precisely that a deployment where everything
	// else works reports itself healthy.
	weekLater := retentionTestNow.AddDate(0, 0, 7)
	svc.Now = fixedNow(weekLater)
	seedMovableArtifact(t, ctx, journal, bs, "still-landing.dump", weekLater.Add(-time.Hour))
	for i := 0; i < 6; i++ {
		svc.RunCycle(ctx)
	}

	got := healthFor(t, ctx, svc, bs.ID)
	if got.State != health.Degraded {
		t.Fatalf("State = %s (%s), want DEGRADED: %s has been failing to reach %s for a week and the status page reads green",
			got.State, got.Reason, artifact, moveTestMedium)
	}
	if got.Placement.FailedMoves == 0 {
		t.Errorf("Placement.FailedMoves = 0, want at least one; Placement = %+v", got.Placement)
	}
	if got.Placement.OldestFailedMoveAge == nil || *got.Placement.OldestFailedMoveAge < 7*24*time.Hour {
		t.Errorf("Placement.OldestFailedMoveAge = %v, want at least a week: the age is what separates a blip from a wedge",
			got.Placement.OldestFailedMoveAge)
	}
	if got.Placement.AwayFromHome < 1 {
		t.Errorf("Placement.AwayFromHome = %d, want at least one (%s is still on local)", got.Placement.AwayFromHome, artifact)
	}
	if got.Placement.FailedMoveReason == "" {
		t.Error("Placement.FailedMoveReason is empty; the reason the engine recorded on the row is the only account of this failure that outlives the cycle")
	}
	if !strings.Contains(got.Reason, "relocation") {
		t.Errorf("Reason = %q, want it to say what is wrong; colour alone is not an explanation", got.Reason)
	}
}

// refusingMoveJournal is a journal that answers every question except the
// move one, which it refuses outright.
//
// It is how "a deployment that declares no storage medium reports nothing
// new" gets asserted rather than asserted-about: a report that comes back
// clean through this journal cannot have consulted the move table, and a
// report that comes back with zeroes because the read failed quietly
// would fail here instead of looking identical to a healthy deployment.
type refusingMoveJournal struct{ *state.Journal }

func (refusingMoveJournal) ListMoves(context.Context, ...string) ([]state.Move, error) {
	return nil, errors.New("this test's journal refuses to be asked about moves")
}

// TestBuildHealthReport_ADeploymentWithNoMediumAsksNoPlacementQuestion is
// the issue's second acceptance line. Every deployment that predates EPIC
// E has to read exactly as it did before this field existed, and it must
// not acquire a new way for `backup-manager status` to fail.
func TestBuildHealthReport_ADeploymentWithNoMediumAsksNoPlacementQuestion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	journal := openJournal(t)
	bs := testBackupSet(t, dir)
	bs.StaleAfter = config.Duration(24 * time.Hour)
	cfg := testConfig(t, testSource("production", bs))
	resolveTestRetention(cfg)

	svc := New(cfg, refusingMoveJournal{journal}, newFakeTransport(), nil)
	svc.Now = fixedNow(retentionTestNow)
	seedMovableArtifact(t, ctx, journal, bs, "fresh.dump", retentionTestNow.Add(-time.Hour))

	got := healthFor(t, ctx, svc, bs.ID)
	if got.State != health.Healthy {
		t.Fatalf("State = %s (%s), want HEALTHY", got.State, got.Reason)
	}
	if got.Placement != (health.PlacementHealth{}) {
		t.Errorf("Placement = %+v, want the zero value: no tier names a medium and no artifact is on one, so there is no placement question to answer", got.Placement)
	}
}

// TestBuildHealthReport_AnArtifactStrandedOnARemovedMediumIsAwayFromHome
// is the positive control for the gate above. The gate skips the whole
// computation for a set with no medium in its chain, and the reason that
// is exact rather than convenient is that it ALSO reads the placements:
// an operator who takes a medium back out of a tier leaves whatever is
// already on it stranded, and those artifacts' home became local at that
// moment. A gate that only read the config would hide exactly the
// population that edit creates.
func TestBuildHealthReport_AnArtifactStrandedOnARemovedMediumIsAwayFromHome(t *testing.T) {
	ctx := context.Background()
	svc, bs, journal := healthMovingService(t, newCountingMedium())
	seedForMoveHealth(t, ctx, journal, bs)
	if report := svc.RunCycle(ctx); report.Moves.Completed != 1 {
		t.Fatalf("Moves = %+v, want the move to have landed before the medium is taken away", report.Moves)
	}

	// The operator changes their mind: the monthly tier goes back to
	// local and the medium declaration goes away. Nothing moves the bytes
	// back, and nothing ever will unless somebody is told.
	local := chainWithOffsiteMonthly()
	local.Tiers[1].Medium = ""
	svc.Config.Retention = local
	svc.Config.StorageMediums = nil
	resolveTestRetention(svc.Config)

	got := healthFor(t, ctx, svc, bs.ID)
	if got.Placement.AwayFromHome != 1 {
		t.Fatalf("Placement.AwayFromHome = %d, want 1: an artifact left on a medium no tier names any more is not at home, and the config alone no longer says so; Placement = %+v",
			got.Placement.AwayFromHome, got.Placement)
	}
	if got.Placement.OldestAwayFromHomeAge == nil {
		t.Error("Placement.OldestAwayFromHomeAge is nil, want the age of the copy that is stranded")
	}
}
