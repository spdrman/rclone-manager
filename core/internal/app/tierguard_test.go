package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The production placement.TierGuard (issue #239, inherited from #238's
// acceptance line). It is the sharpest of the three seams #238 left open:
// guardSourceDelete treats a nil guard as a refusal, so until this exists
// the move engine physically cannot delete a source copy.
//
// Every test here is written from the delete's point of view. A false
// answer authorises removing a copy of a backup, so the ones that must
// come first are the ones that prove it says true, or errors, whenever it
// cannot see the whole picture.

func guardFixture(t *testing.T) (TierGuard, []state.Record) {
	t.Helper()
	records := gfsRecordsForHomeTest(t)
	for i := range records {
		records[i].Placements = []state.Placement{{Medium: state.MediumLocal, Status: state.PlacementActive}}
	}
	bs := testBackupSet(t, t.TempDir())
	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = chainWithOffsiteMonthly()
	resolveTestRetention(cfg)

	svc := New(cfg, recordsJournal{records: records}, nil, nil)
	svc.Now = fixedNow(retentionTestNow)
	return TierGuard{Service: svc}, records
}

func recordNamed(t *testing.T, records []state.Record, name string) state.Record {
	t.Helper()
	for _, r := range records {
		if r.Artifact.Name == name {
			return r
		}
	}
	t.Fatalf("no record named %q", name)
	return state.Record{}
}

// TestTierGuard_PreservesASourceATierStillWants is the refusal, and it
// comes first. recent.dump is inside the daily window, daily is local, so
// nothing may delete its local copy however verified a destination is.
func TestTierGuard_PreservesASourceATierStillWants(t *testing.T) {
	guard, records := guardFixture(t)
	rec := recordNamed(t, records, "recent.dump")

	selected, why, err := guard.SourceStillSelected(context.Background(), rec, state.MediumLocal)
	if err != nil {
		t.Fatalf("SourceStillSelected: %v", err)
	}
	if !selected {
		t.Fatal("the guard said no tier wants recent.dump on local; the daily tier is local and selects it, and a false answer here deletes a backup that is still in the retention window")
	}
	if !strings.Contains(why, "daily") {
		t.Errorf("the explanation %q does not name the tier that preserved the source", why)
	}
}

// TestTierGuard_ReleasesASourceNothingWantsAnyMore is the positive
// control. Without it the refusal above would also pass against a guard
// that answered true unconditionally, which is safe and useless: no move
// would ever complete.
func TestTierGuard_ReleasesASourceNothingWantsAnyMore(t *testing.T) {
	guard, records := guardFixture(t)
	rec := recordNamed(t, records, "monthly-only.dump")

	selected, why, err := guard.SourceStillSelected(context.Background(), rec, state.MediumLocal)
	if err != nil {
		t.Fatalf("SourceStillSelected: %v", err)
	}
	if selected {
		t.Fatalf("the guard preserved monthly-only.dump's local copy (%s); it has aged out of the daily window and only the offsite monthly tier selects it, so no move off local could ever finish", why)
	}
}

// TestTierGuard_AsksAboutTheMediumItWasGiven pins that the answer is per
// medium and not a global "is this artifact kept". The same artifact, the
// same instant: local is free and the offsite medium is not.
func TestTierGuard_AsksAboutTheMediumItWasGiven(t *testing.T) {
	guard, records := guardFixture(t)
	rec := recordNamed(t, records, "monthly-only.dump")

	selected, _, err := guard.SourceStillSelected(context.Background(), rec, "cold_offsite")
	if err != nil {
		t.Fatalf("SourceStillSelected: %v", err)
	}
	if !selected {
		t.Fatal("the guard released a copy on cold_offsite, which is exactly the medium the monthly tier that selects this artifact names")
	}
}

// TestTierGuard_RefusesAnArtifactRetentionHasNoOpinionAbout is the
// uncertainty case. GFSDecide classifies only managed-complete artifacts,
// so an artifact with no verdict is one whose retention standing this
// guard cannot establish, and FR-30 says uncertainty preserves the source.
// An error rather than a plain true, because guardSourceDelete renders it
// into the refusal an operator reads.
func TestTierGuard_RefusesAnArtifactRetentionHasNoOpinionAbout(t *testing.T) {
	guard, records := guardFixture(t)
	rec := recordNamed(t, records, "recent.dump")
	rec.State = "QUARANTINED" // no longer managed-complete, so no verdict

	guardWithout := guard
	guardWithout.Service = New(guard.Service.Config, recordsJournal{records: []state.Record{rec}}, nil, nil)
	guardWithout.Service.Now = fixedNow(retentionTestNow)

	if _, _, err := guardWithout.SourceStillSelected(context.Background(), rec, state.MediumLocal); err == nil {
		t.Fatal("the guard answered about an artifact GFS has no verdict for; it cannot prove nothing wants the source, and an unproven release is a deleted copy")
	}
}

// TestTierGuard_RefusesWhenTheJournalCannotBeRead is the other
// uncertainty shape, and the one a live deployment actually hits. A
// database that will not answer must never read as "no tier wants it".
func TestTierGuard_RefusesWhenTheJournalCannotBeRead(t *testing.T) {
	guard, records := guardFixture(t)
	rec := recordNamed(t, records, "monthly-only.dump")

	broken := guard
	broken.Service = New(guard.Service.Config, failingJournal{}, nil, nil)
	broken.Service.Now = fixedNow(retentionTestNow)

	selected, _, err := broken.SourceStillSelected(context.Background(), rec, state.MediumLocal)
	if err == nil {
		t.Fatalf("the guard answered %v off a journal that returned an error", selected)
	}
	if !strings.Contains(err.Error(), "the disk fell over") {
		t.Errorf("the refusal %q loses the underlying cause, so an operator reading a preserved source cannot tell a policy answer from a broken database", err)
	}
}

// TestTierGuard_RefusesABackupSetConfigNoLongerNames covers the set the
// journal remembers and the config does not. There is no chain to
// evaluate, so there is no way to prove the source is unwanted.
func TestTierGuard_RefusesABackupSetConfigNoLongerNames(t *testing.T) {
	guard, records := guardFixture(t)
	rec := recordNamed(t, records, "monthly-only.dump")

	other, err := model.NewBackupSetID("production", "a-set-nobody-configures")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	id, err := model.NewArtifactID(other, rec.Artifact.Name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	rec.Artifact = id

	if _, _, err := guard.SourceStillSelected(context.Background(), rec, state.MediumLocal); err == nil {
		t.Fatal("the guard answered about a backup set this configuration does not name; there is no chain, so there is nothing that could have said no")
	}
}

// TestTierGuard_SatisfiesTheEngineSeam is the compile-time proof this is
// the thing #238 left a hole for.
func TestTierGuard_SatisfiesTheEngineSeam(t *testing.T) {
	var _ placement.TierGuard = TierGuard{}
}

// failingJournal answers every list with the same error, so a test can
// prove a refusal survives a database that will not talk.
type failingJournal struct{ Journal }

func (failingJournal) ListByBackupSet(context.Context, model.BackupSetID) ([]state.Record, error) {
	return nil, errors.New("the disk fell over")
}
