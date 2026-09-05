package app

import (
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

var copiesNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// serviceWithMediums builds the minimum Service artifactCopies needs: a
// configuration that declares the mediums the placements name.
func serviceWithMediums(mediums ...config.StorageMedium) *Service {
	return &Service{Config: &config.Config{StorageMediums: mediums}, Now: func() time.Time { return copiesNow }}
}

func placementOn(medium, location, verifiedAs string) state.Placement {
	return state.Placement{
		Medium:            medium,
		Location:          location,
		VerificationClass: verifiedAs,
		Status:            state.PlacementActive,
		CreatedAt:         copiesNow,
		UpdatedAt:         copiesNow,
	}
}

// TestAnArchivedCopyReadsAsRequiresRestoreOnTheDetailSurface is FR-34's
// read half at the level an operator actually meets it.
//
// The local copy in the same record is the control: it has to keep reading
// as immediate, because every deployment that never configured a medium
// has exactly that and nothing else, and a change in what it says would be
// a change every existing install would see.
func TestAnArchivedCopyReadsAsRequiresRestoreOnTheDetailSurface(t *testing.T) {
	s := serviceWithMediums(config.StorageMedium{
		ID: "cold-store", Type: config.StorageMediumTypeS3,
		Bucket: "backups", StorageClass: config.StorageClassDeepArchive,
	})

	rec := state.Record{Placements: []state.Placement{
		placementOn(state.MediumLocal, "/srv/backups/dump.zst", state.VerificationContent),
		placementOn("cold-store", "prefix/dump.zst", state.VerificationContent),
	}}

	copies := s.artifactCopies(rec, copiesNow)
	if len(copies) != 2 {
		t.Fatalf("got %d copies, want 2", len(copies))
	}

	local, cold := copies[0], copies[1]
	if local.Access != archive.Immediate || !local.Retrievable() {
		t.Errorf("the local copy reads as %q; it has to keep reading as immediate", local.Access)
	}
	if local.RetrievalBilled {
		t.Error("the local copy claims the provider bills to read a file off local disk")
	}
	if cold.Access != archive.RequiresRestore {
		t.Errorf("the DEEP_ARCHIVE copy reads as %q, want %q", cold.Access, archive.RequiresRestore)
	}
	if cold.Retrievable() {
		t.Error("the DEEP_ARCHIVE copy claims its bytes can be read right now")
	}
	if cold.StorageClass != config.StorageClassDeepArchive {
		t.Errorf("storage class = %q, want %q", cold.StorageClass, config.StorageClassDeepArchive)
	}
	if !cold.RetrievalBilled {
		t.Error("the DEEP_ARCHIVE copy does not say retrieval is billed")
	}
	if cold.VerificationClass != state.VerificationContent {
		t.Errorf("verification class = %q; the record of a verification that really happened must not be rewritten by this view", cold.VerificationClass)
	}
	if cold.Detail == "" {
		t.Error("the DEEP_ARCHIVE copy says nothing about why it cannot be read")
	}

	// FR-31's archive rule ends with "the status surfaces say exactly
	// that", and this is the line that does it: the strongest thing
	// anybody can do to reassure themselves about this copy today is
	// confirm an object of the right size is at that key.
	if cold.CheckableAs != string(placement.Existence) {
		t.Errorf("the DEEP_ARCHIVE copy is checkable as %q, want %q", cold.CheckableAs, placement.Existence)
	}
	if local.CheckableAs != string(placement.Content) {
		t.Errorf("the local copy is checkable as %q, want %q", local.CheckableAs, placement.Content)
	}
	if cold.CheckableAs == cold.VerificationClass {
		t.Error("this test is not showing anything: what a copy was verified as and what it can be checked as have to be able to differ")
	}
}

// TestACopyOnAnUnwarmClassStillReadsAsImmediate covers the classes an
// operator picks for cost rather than for cold storage. Marking one of
// these as needing a restore would refuse content verification of a class
// perfectly capable of it.
func TestACopyOnAnUnwarmClassStillReadsAsImmediate(t *testing.T) {
	for _, class := range []string{
		config.StorageClassStandardIA,
		config.StorageClassOneZoneIA,
		config.StorageClassGlacierIR,
		config.StorageClassIntelligentTiering,
	} {
		s := serviceWithMediums(config.StorageMedium{
			ID: "store", Type: config.StorageMediumTypeS3, Bucket: "b", StorageClass: class,
		})
		rec := state.Record{Placements: []state.Placement{placementOn("store", "k", state.VerificationContent)}}
		copies := s.artifactCopies(rec, copiesNow)
		if len(copies) != 1 {
			t.Fatalf("%s: got %d copies, want 1", class, len(copies))
		}
		if copies[0].Access != archive.Immediate {
			t.Errorf("%s reads as %q, want %q", class, copies[0].Access, archive.Immediate)
		}
	}
}

// TestACopyOnAMediumTheConfigurationDroppedIsUnreachableAndNotGone keeps
// FR-34's two facts apart. Removing a medium from config.yaml does not
// delete the objects in its bucket, and a surface that said "gone" would
// be telling an operator their backups no longer exist because they edited
// a YAML file.
func TestACopyOnAMediumTheConfigurationDroppedIsUnreachableAndNotGone(t *testing.T) {
	s := serviceWithMediums()
	rec := state.Record{Placements: []state.Placement{placementOn("a-medium-nobody-declares", "k", state.VerificationContent)}}

	copies := s.artifactCopies(rec, copiesNow)
	if len(copies) != 1 {
		t.Fatalf("got %d copies, want 1", len(copies))
	}
	if copies[0].Access != archive.Unreachable {
		t.Fatalf("access = %q, want %q", copies[0].Access, archive.Unreachable)
	}
	if copies[0].Status != state.PlacementActive {
		t.Errorf("status = %q; nothing here may downgrade the journal's own record of the copy", copies[0].Status)
	}
	if copies[0].CheckableAs != "" {
		t.Errorf("checkable as %q; nothing can be checked against a medium nothing can reach", copies[0].CheckableAs)
	}
}

// TestAnArtifactWithNoPlacementsHasNoCopies, which is what a record built
// by hand has and what an artifact that has not been transferred yet has.
func TestAnArtifactWithNoPlacementsHasNoCopies(t *testing.T) {
	s := serviceWithMediums()
	if copies := s.artifactCopies(state.Record{}, copiesNow); copies != nil {
		t.Fatalf("got %d copies for a record with no placements, want none", len(copies))
	}
}
