package app

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// pruneDailyOnlyRetention narrows the daily window to exactly "today" and
// disables the weekly/monthly tiers, so a managed-complete artifact
// discovered outside that one-day window is a GFS delete candidate purely
// because of its own DiscoveredAt date, not because of anything this file
// has to fake at the retention-policy layer. Last-known-good protection
// stays on, exactly like testRetention()'s own default.
func pruneDailyOnlyRetention() config.Retention {
	protect := true
	return config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		DailyDays: 1, WeeklyMonths: 0, MonthlyMonths: 0,
		ProtectLastKnownGood: &protect,
	}
}

// discoverAt is discoverOneRecord (pipeline_test.go), except it takes an
// explicit DiscoveredAt instant instead of hardcoding epoch: this file's
// whole point is proving GFS's calendar-bucket math over two artifacts
// discovered on two different dates, which discoverOneRecord's fixed
// clock cannot produce.
func discoverAt(t *testing.T, ctx context.Context, journal Journal, tr transport.Transport, source transport.Source, bs config.BackupSet, when time.Time) state.Record {
	t.Helper()
	res, err := discovery.Discover(ctx, discovery.Deps{Transport: tr, Journal: journal, Now: fixedNow(when)}, source, bs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Discovered) != 1 {
		t.Fatalf("Discover: got %d newly-discovered artifacts, want 1 (result=%+v)", len(res.Discovered), res)
	}
	return res.Discovered[0]
}

// TestPrunePreviewAndApply_KeepsNewestDropsOld drives two artifacts all
// the way to COMPLETE, one "old" (outside the daily window, not
// last-known-good) and one "new" (inside it, and last-known-good since it
// is the newest eligible artifact), then checks that:
//
//   - PrunePreview reports the old one DELETE and the new one KEEP,
//     without touching either file (PruneDecide's own no-mutation
//     contract);
//   - PruneApply actually removes the old artifact's local file and
//     leaves the new one in place — the real internal/retention decision
//     path (PruneDecide feeding PruneApply, FR-20's own safety checks
//     included), not a mock of it.
func TestPrunePreviewAndApply_KeepsNewestDropsOld(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""
	source := transport.Source{ID: "prune-test"}

	tr := newFakeTransport()
	journal := openJournal(t)
	ctx := context.Background()

	oldDay := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	newDay := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	tr.put("old.dump", "old payload", oldDay.Unix())
	oldRec := discoverAt(t, ctx, journal, tr, source, bs, oldDay)

	tr.put("new.dump", "new payload", newDay.Unix())
	newRec := discoverAt(t, ctx, journal, tr, source, bs, newDay)

	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = pruneDailyOnlyRetention()
	resolveTestRetention(cfg)
	svc := New(cfg, journal, tr, nil)

	svc.Now = fixedNow(oldDay)
	svc.processArtifact(ctx, source, bs, oldRec)
	svc.Now = fixedNow(newDay)
	svc.processArtifact(ctx, source, bs, newRec)

	oldFinal, err := journal.Get(ctx, oldRec.Artifact)
	if err != nil || oldFinal.State != string(lifecycle.Complete) {
		t.Fatalf("precondition failed: old artifact state = %q, err = %v, want %q", oldFinal.State, err, lifecycle.Complete)
	}
	newFinal, err := journal.Get(ctx, newRec.Artifact)
	if err != nil || newFinal.State != string(lifecycle.Complete) {
		t.Fatalf("precondition failed: new artifact state = %q, err = %v, want %q", newFinal.State, err, lifecycle.Complete)
	}

	svc.Now = fixedNow(newDay)

	preview, err := svc.PrunePreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("PrunePreview: %v", err)
	}
	if len(preview.Verdicts) != 2 {
		t.Fatalf("PrunePreview: len(Verdicts) = %d, want 2 (verdicts=%+v)", len(preview.Verdicts), preview.Verdicts)
	}
	verdictFor := func(vs []retention.PruneVerdict, name string) retention.PruneVerdict {
		t.Helper()
		for _, v := range vs {
			if v.Artifact.Name == name {
				return v
			}
		}
		t.Fatalf("no verdict for artifact %q in %+v", name, vs)
		return retention.PruneVerdict{}
	}
	if v := verdictFor(preview.Verdicts, "old.dump"); v.Action != retention.PruneDelete {
		t.Errorf("preview: old.dump Action = %v, want %v (reason: %s)", v.Action, retention.PruneDelete, v.Reason)
	}
	if v := verdictFor(preview.Verdicts, "new.dump"); v.Action != retention.PruneKeep {
		t.Errorf("preview: new.dump Action = %v, want %v (reason: %s)", v.Action, retention.PruneKeep, v.Reason)
	}

	// Preview must never touch the filesystem: both files still exist.
	if _, err := os.Lstat(oldFinal.LocalPath); err != nil {
		t.Errorf("PrunePreview deleted or moved %s, want it untouched: %v", oldFinal.LocalPath, err)
	}
	if _, err := os.Lstat(newFinal.LocalPath); err != nil {
		t.Errorf("PrunePreview deleted or moved %s, want it untouched: %v", newFinal.LocalPath, err)
	}

	applied, err := svc.PruneApply(ctx, bs.ID)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if v := verdictFor(applied.Verdicts, "old.dump"); v.Action != retention.PruneDelete {
		t.Errorf("apply: old.dump Action = %v, want %v (reason: %s)", v.Action, retention.PruneDelete, v.Reason)
	}
	if v := verdictFor(applied.Verdicts, "new.dump"); v.Action != retention.PruneKeep {
		t.Errorf("apply: new.dump Action = %v, want %v (reason: %s)", v.Action, retention.PruneKeep, v.Reason)
	}

	if _, err := os.Lstat(oldFinal.LocalPath); !os.IsNotExist(err) {
		t.Errorf("PruneApply did not remove %s (err=%v), want it gone", oldFinal.LocalPath, err)
	}
	if _, err := os.Lstat(newFinal.LocalPath); err != nil {
		t.Errorf("PruneApply removed or moved %s, want it kept: %v", newFinal.LocalPath, err)
	}
}
