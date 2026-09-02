package state_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

func openJournal(t *testing.T) (*state.Journal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	j, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j, path
}

func artifactID(t *testing.T, name string) model.ArtifactID {
	t.Helper()
	set, err := model.NewBackupSetID("nas", "daily")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	id, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return id
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// The invariant this whole issue is about, stated as a check on the
// database rather than on a Record: there is no moment, at any point in an
// artifact's life, when the journal holds an artifact row with no
// placement.
//
// The migration's backfill covers every artifact that existed when a
// deployment upgraded. This covers everything discovered since, which is
// the half a migration can never guarantee on its own.
func TestJournalNeverHoldsAnArtifactWithNoPlacement(t *testing.T) {
	ctx := context.Background()
	j, path := openJournal(t)

	partial := "/backups/daily/db.dump.partial"
	final := "/backups/daily/db.dump"
	id := artifactID(t, "db.dump")

	steps := []state.Transition{
		{Key: "k1", From: "", To: "DISCOVERED", RemotePath: "/remote/db.dump", OccurredAt: at(t, "2026-03-01T00:00:00Z")},
		{Key: "k2", From: "DISCOVERED", To: "TRANSFERRING", LocalPath: &partial, OccurredAt: at(t, "2026-03-01T00:01:00Z")},
		{Key: "k3", From: "TRANSFERRING", To: "TRANSFERRED", Transfer: &state.TransferResult{BytesTransferred: 8192, Checksummed: true}, OccurredAt: at(t, "2026-03-01T00:02:00Z")},
		{Key: "k4", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: at(t, "2026-03-01T00:03:00Z")},
		{Key: "k5", From: "VERIFYING", To: "VERIFIED", Hashes: &state.HashUpdate{Hash: "cafe", Alg: "sha256"}, OccurredAt: at(t, "2026-03-01T00:04:00Z")},
		{Key: "k6", From: "VERIFIED", To: "COMMITTING", OccurredAt: at(t, "2026-03-01T00:05:00Z")},
		{Key: "k7", From: "COMMITTING", To: "COMMITTED", LocalPath: &final, OccurredAt: at(t, "2026-03-01T00:06:00Z")},
		{Key: "k8", From: "COMMITTED", To: "REMOTE_DELETE_PENDING", OccurredAt: at(t, "2026-03-01T00:07:00Z")},
		{Key: "k9", From: "REMOTE_DELETE_PENDING", To: "COMPLETE", OccurredAt: at(t, "2026-03-01T00:08:00Z")},
	}

	for _, step := range steps {
		step.Artifact = id
		out, err := j.RecordTransition(ctx, step)
		if err != nil {
			t.Fatalf("transition to %s: %v", step.To, err)
		}
		if len(out.Record.Placements) != 1 {
			t.Fatalf("after reaching %s the artifact has %d placements, want exactly 1", step.To, len(out.Record.Placements))
		}
		p := out.Record.Placements[0]
		if p.Medium != state.MediumLocal || !p.IsActive() {
			t.Fatalf("after reaching %s the placement is %s/%s, want %s/%s", step.To, p.Medium, p.Status, state.MediumLocal, state.PlacementActive)
		}
		if p.Location != out.Record.LocalPath {
			t.Fatalf("after reaching %s the placement location is %q but local_path is %q", step.To, p.Location, out.Record.LocalPath)
		}
	}

	// And the same read off the database directly, so this is a statement
	// about what is stored rather than about what the Go surface assembles.
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	var orphans int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM artifacts a WHERE NOT EXISTS (SELECT 1 FROM placements p WHERE p.artifact_id = a.id)`,
	).Scan(&orphans); err != nil {
		t.Fatalf("count artifacts without placements: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d artifact rows have no placement", orphans)
	}
}

// The behaviour-neutrality claim, as a check. For as long as every
// placement is local, LocalLocation and LocalPath give the same answer for
// every artifact, so swapping one for the other at the "can I read this"
// call sites cannot change what any of them decides. This is what makes
// the sweep provable rather than merely argued.
func TestLocalLocationAgreesWithLocalPathThroughout(t *testing.T) {
	ctx := context.Background()
	j, _ := openJournal(t)

	partial := "/backups/daily/db.dump.partial"
	final := "/backups/daily/db.dump"
	id := artifactID(t, "db.dump")

	for _, step := range []state.Transition{
		{Key: "k1", From: "", To: "DISCOVERED", RemotePath: "/remote/db.dump", OccurredAt: at(t, "2026-03-01T00:00:00Z")},
		{Key: "k2", From: "DISCOVERED", To: "TRANSFERRING", LocalPath: &partial, OccurredAt: at(t, "2026-03-01T00:01:00Z")},
		{Key: "k3", From: "TRANSFERRING", To: "COMMITTED", LocalPath: &final, OccurredAt: at(t, "2026-03-01T00:06:00Z")},
	} {
		step.Artifact = id
		if _, err := j.RecordTransition(ctx, step); err != nil {
			t.Fatalf("transition to %s: %v", step.To, err)
		}
		rec, err := j.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if rec.LocalLocation() != rec.LocalPath {
			t.Fatalf("at %s: LocalLocation() = %q but LocalPath = %q", step.To, rec.LocalLocation(), rec.LocalPath)
		}
	}

	// The positive control for that agreement: it is a property of the
	// placement being ACTIVE and local, not a method that echoes LocalPath.
	// Retire the local placement, the way the move engine will once it has
	// migrated an artifact off local disk, and the two must part company:
	// LocalPath still records where the artifact landed, and LocalLocation
	// correctly says there is nothing there to open.
	rec, err := j.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rec.Placements[0].Status = state.PlacementGone
	if rec.LocalLocation() != "" {
		t.Fatalf("with the local placement GONE, LocalLocation() = %q, want empty", rec.LocalLocation())
	}
	if rec.LocalPath != final {
		t.Fatalf("LocalPath = %q, want the landing path %q to survive unchanged", rec.LocalPath, final)
	}
}

// A verification class is only recorded for the transition that actually
// verified something. Catalog rebuild hands the journal the very same
// HashUpdate, copied out of a sidecar manifest without reading one byte of
// the artifact, and a class recorded for that would be this product
// telling itself a copy is content-verified when nobody has looked at it.
func TestPlacementClassIsOnlyRecordedForATransitionThatVerified(t *testing.T) {
	ctx := context.Background()
	j, _ := openJournal(t)

	verified := artifactID(t, "verified.dump")
	rebuilt := artifactID(t, "rebuilt.dump")

	// The real path: lifecycle's own read-back, named through the state
	// machine's constant so this cannot silently stop applying if that
	// vocabulary is ever renamed.
	for _, step := range []state.Transition{
		{Artifact: verified, Key: "v1", From: "", To: string(lifecycle.Discovered), RemotePath: "/remote/verified.dump", OccurredAt: at(t, "2026-03-01T00:00:00Z")},
		{Artifact: verified, Key: "v2", From: string(lifecycle.Discovered), To: string(lifecycle.Verifying), OccurredAt: at(t, "2026-03-01T00:01:00Z")},
		{Artifact: verified, Key: "v3", From: string(lifecycle.Verifying), To: string(lifecycle.Verified),
			Hashes: &state.HashUpdate{Hash: "cafe", Alg: "sha256"}, OccurredAt: at(t, "2026-03-01T00:02:00Z")},
	} {
		if _, err := j.RecordTransition(ctx, step); err != nil {
			t.Fatalf("transition to %s: %v", step.To, err)
		}
	}
	rec, err := j.Get(ctx, verified)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p, ok := rec.LocalPlacement()
	if !ok {
		t.Fatal("no local placement")
	}
	if p.VerificationClass != state.VerificationContent {
		t.Errorf("after a real read-back the class is %q, want %q", p.VerificationClass, state.VerificationContent)
	}
	if p.VerifiedAt == nil || !p.VerifiedAt.Equal(at(t, "2026-03-01T00:02:00Z")) {
		t.Errorf("verified_at = %v, want the moment of the VERIFIED transition", p.VerifiedAt)
	}
	if p.Hash != "cafe" || p.HashAlg != "sha256" {
		t.Errorf("placement hash %q/%q, want cafe/sha256", p.Hash, p.HashAlg)
	}

	// Catalog rebuild's shape: the same hash, attached to a same-state
	// transition that read nothing.
	for _, step := range []state.Transition{
		{Artifact: rebuilt, Key: "r1", From: "", To: string(lifecycle.RemoteDeletePending), RemotePath: "/remote/rebuilt.dump", OccurredAt: at(t, "2026-03-01T00:00:00Z")},
		{Artifact: rebuilt, Key: "r2", From: string(lifecycle.RemoteDeletePending), To: string(lifecycle.RemoteDeletePending),
			Hashes: &state.HashUpdate{Hash: "beef", Alg: "sha256"}, OccurredAt: at(t, "2026-03-01T00:01:00Z")},
	} {
		if _, err := j.RecordTransition(ctx, step); err != nil {
			t.Fatalf("transition to %s: %v", step.To, err)
		}
	}
	rec, err = j.Get(ctx, rebuilt)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p, ok = rec.LocalPlacement()
	if !ok {
		t.Fatal("no local placement")
	}
	if p.VerificationClass != state.VerificationNone {
		t.Errorf("a rebuilt row's class is %q, want no class at all: nothing read the bytes", p.VerificationClass)
	}
	if p.VerifiedAt != nil {
		t.Errorf("a rebuilt row's verified_at is %v, want nil", p.VerifiedAt)
	}
	if p.Hash != "beef" {
		t.Errorf("placement hash %q, want the manifest's own beef: the hash is real evidence even though the moment is not", p.Hash)
	}
}

// Every list read carries placements too, not only the single-record ones.
// Retention, health and the API surface all read through the list paths,
// and a Record whose Placements are empty because of which query loaded it
// would be the worst kind of wrong: correct in the tests that use Get and
// silently empty everywhere that matters.
func TestListReadsCarryPlacements(t *testing.T) {
	ctx := context.Background()
	j, _ := openJournal(t)

	set, err := model.NewBackupSetID("nas", "daily")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	for _, name := range []string{"a.dump", "b.dump", "c.dump"} {
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact: artifactID(t, name), Key: "k-" + name, From: "", To: "DISCOVERED",
			RemotePath: "/remote/" + name, OccurredAt: at(t, "2026-03-01T00:00:00Z"),
		}); err != nil {
			t.Fatalf("discover %s: %v", name, err)
		}
	}

	for _, tc := range []struct {
		name string
		read func() ([]state.Record, error)
	}{
		{"ListByState", func() ([]state.Record, error) { return j.ListByState(ctx, "DISCOVERED") }},
		{"ListByBackupSet", func() ([]state.Record, error) { return j.ListByBackupSet(ctx, set) }},
	} {
		recs, err := tc.read()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(recs) != 3 {
			t.Fatalf("%s returned %d records, want 3", tc.name, len(recs))
		}
		for _, rec := range recs {
			if len(rec.Placements) != 1 {
				t.Errorf("%s: %s has %d placements, want 1", tc.name, rec.Artifact, len(rec.Placements))
			}
		}
	}
}

// A placement recorded under one spelling of "local" and a store resolved
// under another is an artifact nothing can find. config already pins its
// own constant against artifactstore's; this pins the third one, the
// journal's, against both.
func TestLocalMediumIdIsSpelledTheSameEverywhere(t *testing.T) {
	if state.MediumLocal != config.MediumLocal {
		t.Errorf("state.MediumLocal = %q but config.MediumLocal = %q", state.MediumLocal, config.MediumLocal)
	}
	if state.MediumLocal != string(artifactstore.KindLocal) {
		t.Errorf("state.MediumLocal = %q but artifactstore.KindLocal = %q", state.MediumLocal, artifactstore.KindLocal)
	}
}
