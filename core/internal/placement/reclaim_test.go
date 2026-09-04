package placement_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// FR-30's prune half (issue #239): deleting an expired artifact whose
// durable copy is an object goes through MediumStore.DeleteObject, and is
// preceded by an FR-16-style identity re-check, "stat the object; compare
// size and, where available, checksum against the placement record; refuse
// on mismatch and require reconciliation".
//
// The spec's own planted-violation table names this one directly: "a
// fixture that swaps the object behind a key before prune; the delete must
// be refused". TestReclaim_RefusesAnObjectThatWasSwappedBehindTheKey is
// that fixture.
//
// Ordering here is the same as internal/retention's own medium prune
// suite: every refusal first, the successful delete last. What this code
// destroys is a copy of a backup, and a suite that establishes the happy
// path first grows its refusals as afterthoughts around it.

const reclaimKey = "rclone-manager/production/postgres-primary/expired.dump"

var reclaimNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// reclaimFixture is one expired artifact with one ACTIVE placement on a
// medium, and a matching object behind it. Size AND hash are both recorded
// on purpose: FR-16 asks for the strongest practical attributes, and a
// fixture that left either out would stop exercising the comparison it
// stands for the moment the other one was enough.
func reclaimFixture(t *testing.T) (*placement.Reclaimer, *fakeMedium, state.Record) {
	t.Helper()
	content := []byte("expired backup bytes")
	store := newFakeMedium()
	store.objects[reclaimKey] = append([]byte(nil), content...)

	size := int64(len(content))
	rec := state.Record{
		Artifact:  reclaimArtifact(t),
		State:     string(lifecycle.Complete),
		LocalPath: "/backups/production/postgres-primary/expired.dump",
		Placements: []state.Placement{{
			Medium:            "offsite_s3",
			Location:          reclaimKey,
			Size:              &size,
			Hash:              sha256Hex(content),
			HashAlg:           "sha256",
			VerificationClass: string(placement.Content),
			Status:            state.PlacementActive,
		}},
	}

	r := &placement.Reclaimer{
		Store:   store,
		Mediums: fixedMediums{medium: transport.Medium{ID: "offsite_s3", Type: transport.MediumTypeS3, Bucket: "nas-backups"}},
		Now:     func() time.Time { return reclaimNow },
	}
	return r, store, rec
}

func reclaimArtifact(t *testing.T) model.ArtifactID {
	t.Helper()
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	id, err := model.NewArtifactID(set, "expired.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return id
}

// --- the refusals ---

// TestReclaim_RefusesAnObjectThatWasSwappedBehindTheKey is FR-35's planted
// violation for this gate, run as a test rather than described. Something
// else is at the key now: same key, different bytes, different length. The
// delete must not happen, because the object there is not the one this
// journal is expiring.
func TestReclaim_RefusesAnObjectThatWasSwappedBehindTheKey(t *testing.T) {
	r, store, rec := reclaimFixture(t)
	store.objects[reclaimKey] = []byte("something else entirely, and a different length")

	err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3")
	if err == nil {
		t.Fatal("the object behind the key was replaced and the delete went ahead anyway; FR-16 exists to stop exactly this")
	}
	if store.deletes != 0 {
		t.Errorf("DeleteObject was called %d times on a refused delete", store.deletes)
	}
	if !store.has(reclaimKey) {
		t.Error("the object is gone after a refusal")
	}
}

// TestReclaim_RefusesAnObjectSwappedForOneOfTheSameLength is the sharper
// half of the same attack, and the reason the re-check does not stop at a
// size comparison. An object replaced by different bytes of identical
// length passes every size check there is.
func TestReclaim_RefusesAnObjectSwappedForOneOfTheSameLength(t *testing.T) {
	r, store, rec := reclaimFixture(t)
	store.attests = true // this endpoint CAN produce a full-object SHA-256
	same := []byte("EXPIRED BACKUP BYTES")
	if len(same) != len(store.objects[reclaimKey]) {
		t.Fatalf("the fixture's replacement is %d bytes and the original is %d; this test proves nothing unless they match", len(same), len(store.objects[reclaimKey]))
	}
	store.objects[reclaimKey] = same

	if err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3"); err == nil {
		t.Fatal("an object of the same length but different bytes was deleted; the size check alone cannot see this, which is why FR-16 says checksum where available")
	}
	if store.deletes != 0 {
		t.Errorf("DeleteObject was called %d times on a refused delete", store.deletes)
	}
}

// TestReclaim_RefusesWhenTheMediumCannotBeAsked is the fact-about-the-
// endpoint case. A medium that could not be reached and a medium that
// answered "not there" are different facts, and a delete recorded on the
// strength of a network failure is the confusion StatObject's own doc
// warns about.
func TestReclaim_RefusesWhenTheMediumCannotBeAsked(t *testing.T) {
	r, store, rec := reclaimFixture(t)
	store.statErr = errors.New("dial tcp: connection refused")

	err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3")
	if err == nil {
		t.Fatal("the delete went ahead against a medium that could not be asked about the object")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the refusal %q loses the underlying cause", err)
	}
	if store.deletes != 0 {
		t.Errorf("DeleteObject was called %d times", store.deletes)
	}
}

// TestReclaim_RefusesAnObjectThatIsNotThere mirrors what the local path
// already does with a file that has vanished: refuse, name it, and change
// nothing. This is deliberately NOT the mid-move convergence
// errSourceAlreadyGone describes, and the difference is what that case
// has and this one does not: a durably recorded, verified destination copy
// that proves the artifact still exists somewhere.
func TestReclaim_RefusesAnObjectThatIsNotThere(t *testing.T) {
	r, store, rec := reclaimFixture(t)
	delete(store.objects, reclaimKey)

	if err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3"); err == nil {
		t.Fatal("the delete reported success against a key holding no object; nothing was re-checked and nothing was removed")
	}
}

// TestReclaim_RefusesAPlacementWithNothingToCompare is FR-16's own
// closing line, "if identity cannot be established with sufficient
// confidence, preserve". A placement carrying neither a size nor a usable
// checksum offers no evidence beyond "an object exists at this key", and
// that is not identity.
func TestReclaim_RefusesAPlacementWithNothingToCompare(t *testing.T) {
	r, store, rec := reclaimFixture(t)
	rec.Placements[0].Size = nil
	rec.Placements[0].Hash = ""
	rec.Placements[0].HashAlg = ""

	err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3")
	if err == nil {
		t.Fatal("an object was deleted on the strength of existing at a key; the placement recorded neither a size nor a checksum, so nothing about its identity was established")
	}
	if store.deletes != 0 {
		t.Errorf("DeleteObject was called %d times", store.deletes)
	}
}

// TestReclaim_RefusesAPlacementThatIsNotTheOneAndOnlyActiveOne re-derives
// the "where is this artifact" reading at the point of the delete rather
// than trusting the locator that got us here. Two ACTIVE placements is a
// move in flight, and deleting one of them from under it is the race
// FR-30's journal exists to make unrepresentable.
func TestReclaim_RefusesAPlacementThatIsNotTheOneAndOnlyActiveOne(t *testing.T) {
	for _, tc := range []struct {
		name       string
		placements func(state.Record) []state.Placement
	}{
		{"a second ACTIVE placement (a move in flight)", func(rec state.Record) []state.Placement {
			return append(rec.Placements, state.Placement{Medium: config.MediumLocal, Location: rec.LocalPath, Status: state.PlacementActive})
		}},
		{"the placement is on its way out", func(rec state.Record) []state.Placement {
			p := rec.Placements[0]
			p.Status = state.PlacementDeletePending
			return []state.Placement{p}
		}},
		{"no placement on this medium at all", func(state.Record) []state.Placement { return nil }},
		{"the placement records no location", func(rec state.Record) []state.Placement {
			p := rec.Placements[0]
			p.Location = ""
			return []state.Placement{p}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, store, rec := reclaimFixture(t)
			rec.Placements = tc.placements(rec)

			if err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3"); err == nil {
				t.Fatal("the delete went ahead")
			}
			if store.deletes != 0 {
				t.Errorf("DeleteObject was called %d times", store.deletes)
			}
		})
	}
}

// TestReclaim_RefusesTheLocalMedium keeps the two delete disciplines
// apart. A local copy is FR-20's, with a canonicalized path proven beneath
// the backup-set root; nothing here has any of that, and answering about
// one would be answering with no proof at all.
func TestReclaim_RefusesTheLocalMedium(t *testing.T) {
	r, store, rec := reclaimFixture(t)
	rec.Placements[0].Medium = config.MediumLocal

	if err := r.DeleteFromMedium(context.Background(), rec, config.MediumLocal); err == nil {
		t.Fatal("the reclaimer accepted a local placement")
	}
	if store.deletes != 0 {
		t.Errorf("DeleteObject was called %d times", store.deletes)
	}
}

// TestReclaim_RefusesWithoutAStoreOrAResolver is the nil-dependency case,
// which is what a deployment with no medium configured looks like from
// here.
func TestReclaim_RefusesWithoutAStoreOrAResolver(t *testing.T) {
	_, _, rec := reclaimFixture(t)

	for _, tc := range []struct {
		name string
		r    *placement.Reclaimer
	}{
		{"no store", &placement.Reclaimer{Mediums: fixedMediums{}}},
		{"no resolver", &placement.Reclaimer{Store: newFakeMedium()}},
		{"nothing at all", &placement.Reclaimer{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.r.DeleteFromMedium(context.Background(), rec, "offsite_s3"); err == nil {
				t.Fatal("the delete went ahead")
			}
		})
	}
}

// --- and only now, the delete ---

// TestReclaim_DeletesTheObjectItJustReChecked is the success case, and the
// control that keeps every refusal above from passing against a placement.Reclaimer
// that refuses everything.
func TestReclaim_DeletesTheObjectItJustReChecked(t *testing.T) {
	r, store, rec := reclaimFixture(t)

	if err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3"); err != nil {
		t.Fatalf("DeleteFromMedium: %v", err)
	}
	if store.has(reclaimKey) {
		t.Error("the object is still there")
	}
	if store.deletes != 1 {
		t.Errorf("DeleteObject was called %d times, want exactly once: a delete addresses the one key it re-checked", store.deletes)
	}
	if store.stats == 0 {
		t.Error("nothing stat'd the object before deleting it, so no identity was re-checked")
	}
}

// TestReclaim_ComparesTheChecksumWhenTheEndpointCanProduceOne pins the
// "where available" half as something that actually runs, rather than a
// branch that is always skipped. Against the rclone this product embeds no
// s3 endpoint can attest at all, so without this the checksum comparison
// would be dead code that no test ever entered.
func TestReclaim_ComparesTheChecksumWhenTheEndpointCanProduceOne(t *testing.T) {
	r, store, rec := reclaimFixture(t)
	store.attests = true

	if err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3"); err != nil {
		t.Fatalf("DeleteFromMedium against an attesting endpoint: %v", err)
	}
	if store.has(reclaimKey) {
		t.Error("the object is still there")
	}
}

// TestReclaim_DeletesWhenNoChecksumIsAvailableButTheSizeMatches is the
// realistic s3 case, and the one that says what "where available" costs.
// The endpoint cannot attest, the size is all there is, and the delete
// goes ahead on it. That is a weaker proof than the test above and it is
// the honest one to make here: refusing every s3 delete for want of a
// checksum this build cannot obtain would make FR-20 unimplementable on
// the one backend FR-28 ships.
func TestReclaim_DeletesWhenNoChecksumIsAvailableButTheSizeMatches(t *testing.T) {
	r, store, rec := reclaimFixture(t)
	store.attests = false

	if err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3"); err != nil {
		t.Fatalf("DeleteFromMedium: %v", err)
	}
	if store.has(reclaimKey) {
		t.Error("the object is still there")
	}
}

// TestReclaim_RefusesOnASizeMismatchEvenWhenNoChecksumIsAvailable is the
// pair to the test above: the weaker proof is still a proof, and it still
// refuses.
func TestReclaim_RefusesOnASizeMismatchEvenWhenNoChecksumIsAvailable(t *testing.T) {
	r, store, rec := reclaimFixture(t)
	store.attests = false
	store.objects[reclaimKey] = []byte("short")

	if err := r.DeleteFromMedium(context.Background(), rec, "offsite_s3"); err == nil {
		t.Fatal("an object of the wrong size was deleted")
	}
	if store.deletes != 0 {
		t.Errorf("DeleteObject was called %d times", store.deletes)
	}
}
