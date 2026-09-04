package conformance_test

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/tests/miniofixture"
)

// This file is the honest half of #242's archive question, and it is
// written as checks rather than as prose because a paragraph saying "we
// cannot test this" ages into a paragraph nobody reads.
//
// #242 asks for the annual tier of the three-tier chain to be an `s3`
// COLD class, end to end, against MinIO. That cannot be done, for two
// independent reasons, and both of them are established below by running
// the real thing and looking at what comes back:
//
//  1. MinIO does not accept an archive storage class at all. A PUT
//     carrying GLACIER is refused with InvalidStorageClass, so the object
//     never exists and there is nothing to restore.
//  2. Even against an endpoint that took the object, this build could not
//     complete the move, and that has nothing to do with the fixture.
//     The class a move must achieve comes from the medium's
//     upload_verification, which has exactly two spellings, readback and
//     attested, mapping to content and attested. An archived copy's
//     ceiling is existence. Neither configurable class is reachable, so
//     the move cannot reach VERIFIED. And if it somehow did, FR-30's
//     standing invariant requires an ACTIVE placement at content class,
//     which an archived copy cannot hold either.
//
// The second one is a property of the product, derivable from four
// functions this repository already exports, so it is proved from them
// rather than emulated. That is the difference between a check and a
// mock: nothing below stands in for S3, and the conclusion does not
// depend on any endpoint behaving a particular way.
//
// What follows from all of it is that "annual on a cold class" is a
// configuration this build accepts and cannot execute. The conformance
// matrix records that as a BLOCKED row with an issue against it, rather
// than as a pass with a footnote.

// TestThisFixtureRefusesAnArchiveStorageClass establishes fact one against
// the real server, on every run.
//
// It is a check and not a comment because the interesting direction is the
// day it changes. If a later MinIO grows archive-class support, this test
// fails, and the failure is the notification that the composed scenario
// can be extended to cover a rung it currently cannot.
func TestThisFixtureRefusesAnArchiveStorageClass(t *testing.T) {
	fixture := miniofixture.Start(t)
	medium := fixture.NewBucket(t)
	medium.StorageClass = config.StorageClassGlacier

	root := t.TempDir()
	local := filepath.Join(root, "probe.dump")
	if err := writeFile(local, []byte("a probe object nobody expects to survive")); err != nil {
		t.Fatalf("writing the probe: %v", err)
	}

	_, err := adapter().UploadFromLocal(t.Context(), medium, local, "probe/glacier.dump", transport.UploadOptions{})
	if err == nil {
		t.Fatalf("this MinIO accepted an upload on the %s class. That is a CHANGE: the composed scenario "+
			"was written on the fact that it does not, and the archive rung of the chain should now be "+
			"reconsidered rather than left blocked", config.StorageClassGlacier)
	}
	if !strings.Contains(err.Error(), "InvalidStorageClass") {
		t.Errorf("the upload failed for something other than the storage class, so this check is not "+
			"establishing what it says it is: %v", err)
	}
	t.Logf("the fixture refuses %s with: %v", config.StorageClassGlacier, err)
}

// TestWhichStorageClassesThisFixtureWillTake maps the boundary rather than
// asserting one point on it.
//
// Every class the product's own config accepts is tried against the real
// server, and the two the product calls Archive have to be the refused
// ones. Doing the whole table matters: a check on GLACIER alone would
// still pass on a fixture that had started refusing every class, which is
// a broken fixture reading as a proved boundary.
func TestWhichStorageClassesThisFixtureWillTake(t *testing.T) {
	fixture := miniofixture.Start(t)
	root := t.TempDir()
	local := filepath.Join(root, "probe.dump")
	if err := writeFile(local, []byte("a probe object nobody expects to survive")); err != nil {
		t.Fatalf("writing the probe: %v", err)
	}

	classes := config.StorageClasses()
	sort.Strings(classes)

	accepted, refused := map[string]bool{}, map[string]string{}
	for _, class := range classes {
		medium := fixture.NewBucket(t)
		medium.StorageClass = class
		_, err := adapter().UploadFromLocal(t.Context(), medium, local, "probe/"+class+".dump", transport.UploadOptions{})
		if err == nil {
			accepted[class] = true
			continue
		}
		refused[class] = err.Error()
	}
	t.Logf("this fixture accepts %d of %d configurable storage classes", len(accepted), len(classes))

	if len(accepted) == 0 {
		t.Fatal("the fixture refused EVERY storage class, so the refusals below prove nothing about " +
			"archive classes in particular; something is wrong with the fixture or the credentials")
	}
	for _, class := range classes {
		isArchive := archive.IsArchive(class)
		switch {
		case isArchive && accepted[class]:
			t.Errorf("%s is an archive class and this fixture took it. The composed scenario assumes "+
				"it cannot, so that assumption has to be revisited rather than quietly outlived", class)
		case isArchive:
			t.Logf("%s: refused, as the scenario assumes (%s)", class, firstLine(refused[class]))
		case !isArchive && !accepted[class]:
			t.Logf("%s: refused too, although it is not an archive class (%s)", class, firstLine(refused[class]))
		}
	}
}

// TestNoConfigurableVerificationCanBeAchievedOnAnArchiveClass is fact two,
// proved from the product's own definitions with no endpoint involved.
//
// Four facts, each read from the code that defines it:
//
//	a. config offers exactly readback and attested, and nothing else.
//	b. those map to content and attested (the mapping the daemon's
//	   resolver will make, and the one scenarioResolver makes here).
//	c. placement.Ceiling caps an archived copy at existence.
//	d. placement.CheckClass therefore refuses both configurable classes.
//
// Together they say a move to an archive-class medium can never reach
// VERIFIED. That is not a bug in any one of them: (c) is exactly right,
// because reading an archived object fails and calling that a failed
// verification would quarantine a good backup. It is a hole between them,
// which is the kind of thing only a composed check finds.
func TestNoConfigurableVerificationCanBeAchievedOnAnArchiveClass(t *testing.T) {
	modes := config.UploadVerificationModes()
	if len(modes) == 0 {
		t.Fatal("config reports no upload_verification modes at all, so this check inspected nothing")
	}

	// (a) and (b): the complete mapping, stated here so a third mode
	// added to config without a class for it fails this rather than
	// silently narrowing the check.
	classFor := map[string]placement.Class{
		config.UploadVerificationReadback: placement.Content,
		config.UploadVerificationAttested: placement.Attested,
	}
	for _, mode := range modes {
		if _, ok := classFor[mode]; !ok {
			t.Fatalf("config accepts upload_verification %q and this check has no verification class for it; "+
				"the new mode may well be the one that fixes the hole below, so decide rather than skip", mode)
		}
	}

	for _, class := range config.StorageClasses() {
		if !archive.IsArchive(class) {
			continue
		}
		// (c): what an archived copy tops out at, whether it has been
		// asked for or is mid-restore.
		for _, s := range []archive.State{archive.RequiresRestore, archive.Restoring} {
			if got := placement.Ceiling(s); got != placement.Existence {
				t.Fatalf("an archived copy in state %q now has a ceiling of %q; this check was written "+
					"when it was %q and needs rereading", s, got, placement.Existence)
			}
			// (d): every configurable mode refused.
			for mode, want := range classFor {
				err := placement.CheckClass(s, want)
				if err == nil {
					t.Errorf("%s on class %s: %s verification is no longer refused against an archived copy. "+
						"If that is deliberate, the composed scenario's annual tier can be unblocked", mode, class, want)
					continue
				}
				if !errors.Is(err, placement.ErrClassUnavailable) {
					t.Errorf("%s on class %s: refused, but not as a capability result: %v", mode, class, err)
				}
			}
		}
	}
	t.Logf("every upload_verification mode (%s) is refused against every archive class (%s)",
		strings.Join(modes, ", "), strings.Join(archiveClasses(), ", "))
}

// TestAnArchiveClassCopyCannotSatisfyTheStandingInvariant is the second
// half of fact two, and it is why the hole above is a design question
// rather than a patch.
//
// Suppose the move were allowed to land at existence class. FR-30's
// standing invariant asks for an ACTIVE placement at a SUFFICIENT class,
// and sufficient means content unless the operator opted into attested.
// An existence-class placement is not sufficient under either, so the
// instant the source went away the artifact would have no acceptable
// copy. Allowing the move without changing the invariant would move the
// breakage from "the move refuses" to "the invariant is broken", which is
// strictly worse.
func TestAnArchiveClassCopyCannotSatisfyTheStandingInvariant(t *testing.T) {
	id := model.ArtifactID{Set: setID, Name: "2024-06-15T02-00-00Z.dump"}
	rec := state.Record{
		Artifact: id,
		State:    "COMPLETE",
		Placements: []state.Placement{{
			Medium:            mediumDeepFreeze,
			Location:          "rclone-manager/production/postgres-primary/2024-06-15T02-00-00Z.dump",
			Status:            state.PlacementActive,
			VerificationClass: state.VerificationExistence,
		}},
	}

	// The control first: the same record at content class holds, so the
	// refusal below is about the class and not about the fixture.
	held := rec
	held.Placements = []state.Placement{{
		Medium: mediumDeepFreeze, Location: rec.Placements[0].Location,
		Status: state.PlacementActive, VerificationClass: state.VerificationContent,
	}}
	if err := placement.CheckInvariant(held); err != nil {
		t.Fatalf("the control record does not satisfy the invariant, so the refusal below means nothing: %v", err)
	}

	if err := placement.CheckInvariant(rec); err == nil {
		t.Error("an ACTIVE placement at existence class satisfies FR-30's standing invariant. " +
			"If that changed deliberately, the archive tier can be reconsidered; if not, an archived " +
			"copy has just become an acceptable sole copy")
	}
	// And under the attested relaxation an operator can actually
	// configure, it is refused too.
	if err := placement.CheckInvariant(rec, placement.Attested); err == nil {
		t.Error("an existence-class copy satisfies the invariant even under the attested relaxation")
	}
}

// TestThisFixtureImplementsNoRestore establishes the last fixture fact:
// there is no restore to drive here, so #242's "restore" leg of the
// composed scenario cannot be run against MinIO either.
//
// It asks about a real object, uploaded on a class the fixture does take,
// because the interesting answer is "no restore is in play", not "no such
// object". Conflating those two is exactly the mistake
// rclone/mediumrestore.go's own doc warns about, so this check makes sure
// it is not making it.
func TestThisFixtureImplementsNoRestore(t *testing.T) {
	fixture := miniofixture.Start(t)
	medium := fixture.NewBucket(t)

	root := t.TempDir()
	local := filepath.Join(root, "probe.dump")
	body := []byte("an ordinary object on an ordinary class")
	if err := writeFile(local, body); err != nil {
		t.Fatalf("writing the probe: %v", err)
	}
	const key = "probe/ordinary.dump"
	if _, err := adapter().UploadFromLocal(t.Context(), medium, local, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("the fixture would not take an ordinary object, so this check inspected nothing: %v", err)
	}
	if _, err := adapter().StatObject(t.Context(), medium, key); err != nil {
		t.Fatalf("the object is not there, so a restore answer about it would mean nothing: %v", err)
	}

	st, err := adapter().RestoreStatus(t.Context(), medium, key)
	switch {
	case err != nil:
		t.Logf("this fixture answers a restore status with: %v", err)
	case st == nil:
		t.Logf("this fixture reports no restore in play for an object that exists, which is what S3 " +
			"reports for an object nobody has asked to restore")
	default:
		t.Errorf("this fixture reports a restore state (%+v) for an object nothing asked to restore. "+
			"That is a CHANGE, and the restore leg of the composed scenario should be reconsidered", st)
	}

	// The medium the annual tier actually uses is on an archive class,
	// and the object never got there, so this is the whole of it: there
	// is no restore to observe because there is nothing to restore.
	access, err := archive.Access(mediumDeepFreeze, config.StorageClassGlacier,
		archive.Observation{Probe: archive.Answered}, time.Now())
	if err != nil {
		t.Fatalf("deriving the access state of an archived copy: %v", err)
	}
	if access != archive.RequiresRestore {
		t.Errorf("an archived copy with no restore in play reads as %q, want %q", access, archive.RequiresRestore)
	}
}

func archiveClasses() []string {
	var out []string
	for _, c := range config.StorageClasses() {
		if archive.IsArchive(c) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
