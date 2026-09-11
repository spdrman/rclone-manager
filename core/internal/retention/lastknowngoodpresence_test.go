package retention

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/lifecycle"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// FR-19 protects a RECORD, and FR-30 is about a COPY. This file is the
// place those two meet (issue #602).
//
// LastKnownGoodDecide picks the newest managed-complete row in the journal
// and never touches a filesystem, deliberately: gfs.go and lastknowngood.go
// decide and prune.go acts, and FR-32 keeps anything a medium reported out
// of a retention decision entirely. The consequence nobody had written down
// is that the protection is only as good as the row. If the file behind
// that row is gone (an operator's rm, a failed disk, a restore that went
// sideways, anything FR-17 reconciliation exists to notice), FR-19 goes on
// protecting a name, every OTHER artifact in the set is still selected for
// deletion on its age alone, and an apply empties a backup set whose last
// readable restore point it just deleted while reporting that it kept one.
//
// That is FR-30's invariant broken by the one code path in this product
// that removes a local restore point: at the instant the last DELETE lands
// there is no confirmed readable copy of anything in the set, and the
// preview an operator confirmed said "kept by the LAST_KNOWN_GOOD tier"
// about a file that was not there.
//
// So the confirmation is re-derived here, at the point of the dangerous
// action, in the same spirit as pruneVerifySafeToDelete's second call: not
// "the journal names a last known good" but "a copy of it is where this
// backup set says it is". Where it is not, every deletion in the pass is
// refused rather than a subset of them being carried out.

// lkgPresenceChain is a chain nothing this file dates can fall inside, with
// FR-19's protection ON. Every artifact is therefore a GFS delete candidate
// and the only thing standing between this backup set and an empty
// directory is the protection under test.
func lkgPresenceChain(protect bool) config.Retention {
	p := protect
	return config.Retention{
		Timezone:     "UTC",
		WeekStartsOn: "monday",
		Tiers: []config.RetentionTier{
			{Name: "daily", Granularity: config.GranularityDay, Keep: 1},
		},
		ProtectLastKnownGood: &p,
	}
}

// TestPruneRefusesEveryDeleteWhenTheLastKnownGoodCopyIsNotThere is the
// claim: with FR-19 on and the protected artifact's own file missing,
// nothing in the backup set may be deleted.
//
// The older artifact is the only readable copy this deployment has left.
// Deleting it is not "retention working": it is the last restore point
// going, under a plan that says a restore point was kept.
func TestPruneRefusesEveryDeleteWhenTheLastKnownGoodCopyIsNotThere(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "lkg-presence", "set")

	newest := gfsMustArtifact(t, set, "newest.zst")
	newestPath := filepath.Join(root, "newest.zst")

	older := gfsMustArtifact(t, set, "older.zst")
	olderPath := filepath.Join(root, "older.zst")
	pruneWriteFile(t, olderPath, "the only copy of anything this backup set still has")

	// newest.zst has a journal row and no file: exactly what an out-of-band
	// loss looks like from in here. Nothing writes newestPath at all.
	records := []state.Record{
		pruneRecord(newest, lifecycle.Complete, pruneNow.Add(-40*24*time.Hour), newestPath),
		pruneRecord(older, lifecycle.Complete, pruneNow.Add(-90*24*time.Hour), olderPath),
	}
	bs := pruneBackupSet(set, root)

	verdicts, err := PruneDecide(pruneNow, lkgPresenceChain(true), bs, records, AllLocal)
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}

	for _, v := range verdicts {
		if v.Action == PruneDelete {
			t.Errorf("PruneDecide marked %s DELETE while this backup set's last known good (%s) has no readable copy at %s. Carrying that out removes the only file anything could be restored from, under a plan that reports a restore point was kept.",
				v.Artifact.Name, newest.Name, newestPath)
		}
	}

	older_ := pruneFindVerdict(t, verdicts, "older.zst")
	if older_.Action != PruneRefuse {
		t.Fatalf("older.zst: Action = %s, want %s (reason: %s)", older_.Action, PruneRefuse, older_.Reason)
	}
	if !strings.Contains(older_.Reason, "last known good") {
		t.Errorf("older.zst was refused without naming why, so an operator cannot act on it. Reason: %q", older_.Reason)
	}

	// And the apply, which is the half that costs something if it is wrong.
	applied, err := PruneApply(context.Background(), pruneNow, lkgPresenceChain(true), bs, records, AllLocal, nil)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	for _, v := range applied {
		if v.Action == PruneDelete {
			t.Errorf("PruneApply deleted %s in the same situation", v.Artifact.Name)
		}
	}
	pruneMustExist(t, olderPath)
}

// TestPruneStillDeletesWhenTheLastKnownGoodCopyIsThere is the negative
// control for the test above, and it is the reason that one means
// anything.
//
// Identical in every respect except that newest.zst's file exists. A
// refusal that fired in both worlds would be a retention engine that had
// simply stopped deleting, which passes the test above perfectly.
func TestPruneStillDeletesWhenTheLastKnownGoodCopyIsThere(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "lkg-presence-control", "set")

	newest := gfsMustArtifact(t, set, "newest.zst")
	newestPath := filepath.Join(root, "newest.zst")
	pruneWriteFile(t, newestPath, "the last known good, and it is really here")

	older := gfsMustArtifact(t, set, "older.zst")
	olderPath := filepath.Join(root, "older.zst")
	pruneWriteFile(t, olderPath, "aged out, and nothing protects it")

	records := []state.Record{
		pruneRecord(newest, lifecycle.Complete, pruneNow.Add(-40*24*time.Hour), newestPath),
		pruneRecord(older, lifecycle.Complete, pruneNow.Add(-90*24*time.Hour), olderPath),
	}
	bs := pruneBackupSet(set, root)

	applied, err := PruneApply(context.Background(), pruneNow, lkgPresenceChain(true), bs, records, AllLocal, nil)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if v := pruneFindVerdict(t, applied, "older.zst"); v.Action != PruneDelete {
		t.Fatalf("older.zst: Action = %s, want %s. Nothing protects it and the last known good is right there, so a refusal here means the guard stopped this engine deleting at all (reason: %s)", v.Action, PruneDelete, v.Reason)
	}
	pruneMustNotExist(t, olderPath)
	pruneMustExist(t, newestPath)
	if v := pruneFindVerdict(t, applied, "newest.zst"); v.Action != PruneKeep {
		t.Errorf("newest.zst: Action = %s, want %s", v.Action, PruneKeep)
	}
}

// TestPruneDeletesWhenTheOperatorTurnedTheProtectionOff is the other
// control, and it is about consent rather than about mechanism.
//
// protect_last_known_good: false is an operator saying out loud that this
// deployment's retention may empty a backup set. The guard above must not
// quietly override that, or a documented configuration silently stops
// working the moment a file goes missing.
func TestPruneDeletesWhenTheOperatorTurnedTheProtectionOff(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "lkg-presence-off", "set")

	newest := gfsMustArtifact(t, set, "newest.zst")
	newestPath := filepath.Join(root, "newest.zst")

	older := gfsMustArtifact(t, set, "older.zst")
	olderPath := filepath.Join(root, "older.zst")
	pruneWriteFile(t, olderPath, "aged out, and this deployment consented to losing it")

	records := []state.Record{
		pruneRecord(newest, lifecycle.Complete, pruneNow.Add(-40*24*time.Hour), newestPath),
		pruneRecord(older, lifecycle.Complete, pruneNow.Add(-90*24*time.Hour), olderPath),
	}
	bs := pruneBackupSet(set, root)

	applied, err := PruneApply(context.Background(), pruneNow, lkgPresenceChain(false), bs, records, AllLocal, nil)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if v := pruneFindVerdict(t, applied, "older.zst"); v.Action != PruneDelete {
		t.Errorf("older.zst: Action = %s, want %s: with the protection explicitly off there is no last known good for this guard to be about (reason: %s)", v.Action, PruneDelete, v.Reason)
	}
	pruneMustNotExist(t, olderPath)
}

// TestPruneRefusesEveryDeleteWhenTheLastKnownGoodPathIsASymlink is the same
// guard against the other way a copy can fail to be one.
//
// A symlink at a final path is already refused as a DELETE candidate
// (TestPruneRefusesSymlinkAtFinalPath), and this is the other side of that
// same fact: FR-20 refuses to treat it as positively identified, so nothing
// may treat it as the confirmed readable copy that lets everything else in
// the set be deleted either. Both readings of a symlink cannot be right.
func TestPruneRefusesEveryDeleteWhenTheLastKnownGoodPathIsASymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	set := gfsMustSet(t, "lkg-presence-symlink", "set")

	target := filepath.Join(outside, "somewhere-else.zst")
	pruneWriteFile(t, target, "not this backup set's file")

	newest := gfsMustArtifact(t, set, "newest.zst")
	newestPath := filepath.Join(root, "newest.zst")
	if err := os.Symlink(target, newestPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	older := gfsMustArtifact(t, set, "older.zst")
	olderPath := filepath.Join(root, "older.zst")
	pruneWriteFile(t, olderPath, "the only genuine file in here")

	records := []state.Record{
		pruneRecord(newest, lifecycle.Complete, pruneNow.Add(-40*24*time.Hour), newestPath),
		pruneRecord(older, lifecycle.Complete, pruneNow.Add(-90*24*time.Hour), olderPath),
	}
	bs := pruneBackupSet(set, root)

	applied, err := PruneApply(context.Background(), pruneNow, lkgPresenceChain(true), bs, records, AllLocal, nil)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if v := pruneFindVerdict(t, applied, "older.zst"); v.Action == PruneDelete {
		t.Errorf("older.zst was deleted on the strength of a last known good whose final path is a symlink, which FR-20 refuses to treat as a positively identified managed artifact anywhere else in this file")
	}
	pruneMustExist(t, olderPath)
	pruneMustExist(t, target)
}

// TestPruneConfirmsALastKnownGoodOnAMediumFromItsPlacementRow is the seam
// FR-32 draws, stated as a test.
//
// An artifact whose durable copy is confirmed on a storage medium has no
// local file and is not supposed to. Its confirmation is the ACTIVE
// placement row the locator already read, one package up, where reading it
// is not a retention decision; nothing here asks the medium anything. So
// the guard must not fire for it, or every deployment that moves its oldest
// tiers to a bucket stops pruning entirely the moment the newest artifact
// gets there.
func TestPruneConfirmsALastKnownGoodOnAMediumFromItsPlacementRow(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "lkg-presence-medium", "set")

	newest := gfsMustArtifact(t, set, "newest.zst")
	newestPath := filepath.Join(root, "newest.zst")

	older := gfsMustArtifact(t, set, "older.zst")
	olderPath := filepath.Join(root, "older.zst")
	pruneWriteFile(t, olderPath, "still local, still aged out")

	records := []state.Record{
		pruneRecord(newest, lifecycle.Complete, pruneNow.Add(-40*24*time.Hour), newestPath),
		pruneRecord(older, lifecycle.Complete, pruneNow.Add(-90*24*time.Hour), olderPath),
	}
	bs := pruneBackupSet(set, root)

	where := func(a model.ArtifactID) Location {
		if a == newest {
			return Location{Medium: "cold", Status: LocationConfirmed}
		}
		return Location{Medium: config.MediumLocal, Status: LocationConfirmed}
	}

	applied, err := PruneApply(context.Background(), pruneNow, lkgPresenceChain(true), bs, records, where, nil)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if v := pruneFindVerdict(t, applied, "older.zst"); v.Action != PruneDelete {
		t.Errorf("older.zst: Action = %s, want %s: the last known good's copy is confirmed on the medium %q by its own placement row, so there is a readable copy and this guard has nothing to say (reason: %s)", v.Action, PruneDelete, "cold", v.Reason)
	}
	pruneMustNotExist(t, olderPath)
}
