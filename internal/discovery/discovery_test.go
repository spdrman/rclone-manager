package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/internal/config"
	"github.com/spdrman/rclone-manager/internal/model"
	"github.com/spdrman/rclone-manager/internal/state"
	"github.com/spdrman/rclone-manager/internal/transport"
	"github.com/spdrman/rclone-manager/internal/transport/rclone"
)

func openJournal(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func mustSetID(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

func backupSet(t *testing.T, completion config.Completion, include []string) config.BackupSet {
	t.Helper()
	return config.BackupSet{
		Name:       "postgres-primary",
		ID:         mustSetID(t, "production", "postgres-primary"),
		Include:    include,
		Completion: completion,
	}
}

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

var epoch = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// --- rename strategy, against the real rclone local adapter -------------

// TestDiscover_RenameStrategy_RecursesAndSkipsTempNames is the end-to-end
// proof this issue exists for: a producer writing one directory per run
// (gitea-runs/<RUN_ID>/*.dump), using the preferred atomic-rename strategy,
// discovered through the real rclone adapter whose List recursion this PR
// also fixes.
func TestDiscover_RenameStrategy_RecursesAndSkipsTempNames(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "gitea-runs", "run-1", "backup.dump"), "run 1 payload")
	mustWrite(t, filepath.Join(root, "gitea-runs", "run-2", "backup.dump"), "run 2 payload, different name collision case handled elsewhere")
	mustWrite(t, filepath.Join(root, "gitea-runs", "run-3", "backup.dump.tmp"), "still being written")

	source := transport.Source{ID: "rename-strategy", Type: "local", Root: root}
	set := backupSet(t, config.Completion{Strategy: "rename"}, []string{"*.dump"})

	deps := Deps{Transport: rclone.New(), Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// run-1 and run-2 both discover fine as long as they don't collide; the
	// collision case (both actually named backup.dump) is exercised in
	// TestDiscover_BasenameCollisionAcrossDirectoriesIsReported below, so
	// here run-2's file is intentionally the same basename to prove the
	// happy, non-colliding path is unaffected when only one of the two
	// candidates is being asserted on: check run-1 explicitly.
	var sawRun1 bool
	for _, rec := range res.Discovered {
		if rec.RemotePath == "gitea-runs/run-1/backup.dump" {
			sawRun1 = true
			if rec.Artifact.Name != "backup.dump" {
				t.Errorf("Artifact.Name = %q, want %q", rec.Artifact.Name, "backup.dump")
			}
		}
	}
	if !sawRun1 {
		t.Fatalf("run-1's nested backup.dump was not discovered; got Discovered=%+v Rejected=%+v Pending=%+v Conflicts=%+v Errors=%+v",
			res.Discovered, res.Rejected, res.Pending, res.Conflicts, res.Errors)
	}

	for _, rec := range res.Discovered {
		if rec.RemotePath == "gitea-runs/run-3/backup.dump.tmp" {
			t.Fatalf("the .tmp in-progress file was discovered: %+v", rec)
		}
	}
	for _, p := range res.Pending {
		if p.RemotePath == "gitea-runs/run-3/backup.dump.tmp" {
			t.Fatalf("the .tmp in-progress file was reported Pending; it should be silently excluded, not reported at all: %+v", p)
		}
	}
}

func TestDiscover_BasenameCollisionAcrossDirectoriesIsReported(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "gitea-runs", "run-1", "backup.dump"), "first")
	mustWrite(t, filepath.Join(root, "gitea-runs", "run-2", "backup.dump"), "second, different content, same basename")

	source := transport.Source{ID: "collision", Type: "local", Root: root}
	set := backupSet(t, config.Completion{Strategy: "rename"}, []string{"*.dump"})

	deps := Deps{Transport: rclone.New(), Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(res.Discovered) != 1 {
		t.Fatalf("Discovered = %+v, want exactly one winner", res.Discovered)
	}
	winner := res.Discovered[0].RemotePath

	if len(res.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one reported collision", res.Conflicts)
	}
	conflict := res.Conflicts[0]
	if conflict.RecordedPath != winner {
		t.Errorf("Conflict.RecordedPath = %q, want the winner %q", conflict.RecordedPath, winner)
	}
	if conflict.RemotePath == winner {
		t.Errorf("Conflict.RemotePath should be the losing path, not the winner")
	}
	wantLoser := map[string]bool{"gitea-runs/run-1/backup.dump": true, "gitea-runs/run-2/backup.dump": true}
	if !wantLoser[conflict.RemotePath] || !wantLoser[winner] || conflict.RemotePath == winner {
		t.Errorf("winner=%q loser=%q, want the two distinct run paths", winner, conflict.RemotePath)
	}
	if conflict.Artifact.Name != "backup.dump" {
		t.Errorf("Conflict.Artifact.Name = %q, want %q", conflict.Artifact.Name, "backup.dump")
	}

	// The collision must not have corrupted the journal: the winner's
	// recorded row must still point at whichever path actually won, and
	// there must be exactly one row for this artifact identity.
	rec, err := deps.Journal.Get(context.Background(), conflict.Artifact)
	if err != nil {
		t.Fatalf("Journal.Get: %v", err)
	}
	if rec.RemotePath != winner {
		t.Errorf("journal RemotePath = %q, want the winner %q", rec.RemotePath, winner)
	}
}

// --- idempotency across two Discover calls -------------------------------

func TestDiscover_SecondCallOnTheSamePathIsAlreadyKnownNotAnError(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "backup.dump"), "payload")

	source := transport.Source{ID: "idempotent", Type: "local", Root: root}
	set := backupSet(t, config.Completion{Strategy: "rename"}, []string{"*.dump"})
	deps := Deps{Transport: rclone.New(), Journal: openJournal(t), Now: fixedNow(epoch)}

	first, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("first Discover: %v", err)
	}
	if len(first.Discovered) != 1 {
		t.Fatalf("first call: Discovered = %+v, want exactly one", first.Discovered)
	}

	second, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	if len(second.Discovered) != 0 {
		t.Fatalf("second call re-discovered an already-known artifact: %+v", second.Discovered)
	}
	if len(second.AlreadyKnown) != 1 {
		t.Fatalf("second call: AlreadyKnown = %+v, want exactly one", second.AlreadyKnown)
	}
	if len(second.Conflicts) != 0 {
		t.Fatalf("second call on the exact same path was reported as a conflict: %+v", second.Conflicts)
	}
}

// --- marker strategy ------------------------------------------------------

func TestDiscover_MarkerStrategy(t *testing.T) {
	root := t.TempDir()
	// Sibling per-artifact marker.
	mustWrite(t, filepath.Join(root, "with-marker.dump"), "done")
	mustWrite(t, filepath.Join(root, "with-marker.dump.complete"), "")
	// Directory-level manifest marker, covering everything in that dir.
	mustWrite(t, filepath.Join(root, "run", "grouped-a.dump"), "a")
	mustWrite(t, filepath.Join(root, "run", "grouped-b.dump"), "b")
	mustWrite(t, filepath.Join(root, "run", "_SUCCESS"), "")
	// No marker at all: still in flight as far as this strategy is concerned.
	mustWrite(t, filepath.Join(root, "no-marker.dump"), "not yet")

	source := transport.Source{ID: "marker-strategy", Type: "local", Root: root}
	set := backupSet(t, config.Completion{Strategy: "marker"}, []string{"*.dump"})
	deps := Deps{Transport: rclone.New(), Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	discovered := pathSet(res.Discovered)
	for _, want := range []string{"with-marker.dump", "run/grouped-a.dump", "run/grouped-b.dump"} {
		if !discovered[want] {
			t.Errorf("%q was not discovered; Discovered=%+v", want, res.Discovered)
		}
	}

	var sawPendingNoMarker bool
	for _, p := range res.Pending {
		if p.RemotePath == "no-marker.dump" {
			sawPendingNoMarker = true
			if p.Reason == "" {
				t.Errorf("Pending entry for no-marker.dump has no reason")
			}
		}
		if p.RemotePath == "with-marker.dump.complete" || p.RemotePath == "run/_SUCCESS" {
			t.Errorf("a marker file itself was reported as Pending: %+v", p)
		}
	}
	if !sawPendingNoMarker {
		t.Fatalf("no-marker.dump should be Pending; got Pending=%+v", res.Pending)
	}

	for _, rec := range res.Discovered {
		if rec.RemotePath == "with-marker.dump.complete" || rec.RemotePath == "run/_SUCCESS" {
			t.Fatalf("a marker file itself was discovered as an artifact: %+v", rec)
		}
	}
}

// --- stable strategy -------------------------------------------------------

func TestDiscover_StableStrategy(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "old.dump"), "old enough")
	mustWrite(t, filepath.Join(root, "fresh.dump"), "just written")

	oldTime := epoch.Add(-time.Hour)
	freshTime := epoch.Add(-time.Second)
	if err := os.Chtimes(filepath.Join(root, "old.dump"), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := os.Chtimes(filepath.Join(root, "fresh.dump"), freshTime, freshTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	source := transport.Source{ID: "stable-strategy", Type: "local", Root: root}
	set := backupSet(t, config.Completion{Strategy: "stable", StableFor: config.Duration(10 * time.Minute)}, nil)
	deps := Deps{Transport: rclone.New(), Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if !pathSet(res.Discovered)["old.dump"] {
		t.Errorf("old.dump (modified %s before now) should be Discovered; got %+v", epoch.Sub(oldTime), res.Discovered)
	}
	var sawFreshPending bool
	for _, p := range res.Pending {
		if p.RemotePath == "fresh.dump" {
			sawFreshPending = true
		}
	}
	if !sawFreshPending {
		t.Errorf("fresh.dump (modified 1s before now, needs 10m) should be Pending; got Pending=%+v Discovered=%+v", res.Pending, res.Discovered)
	}
}

// --- include filtering -----------------------------------------------------

func TestDiscover_IncludeFiltersNonMatchingNamesSilently(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "backup.dump"), "payload")
	mustWrite(t, filepath.Join(root, "notes.txt"), "irrelevant")

	source := transport.Source{ID: "include-filter", Type: "local", Root: root}
	set := backupSet(t, config.Completion{Strategy: "rename"}, []string{"*.dump"})
	deps := Deps{Transport: rclone.New(), Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if !pathSet(res.Discovered)["backup.dump"] {
		t.Errorf("backup.dump should be Discovered; got %+v", res.Discovered)
	}
	for _, rec := range res.Discovered {
		if rec.RemotePath == "notes.txt" {
			t.Fatalf("notes.txt matched no include pattern but was Discovered anyway")
		}
	}
	for _, p := range res.Pending {
		if p.RemotePath == "notes.txt" {
			t.Fatalf("notes.txt should be silently excluded by include, not reported Pending: %+v", p)
		}
	}
	for _, r := range res.Rejected {
		if r.RemotePath == "notes.txt" {
			t.Fatalf("notes.txt should be silently excluded by include, not Rejected: %+v", r)
		}
	}
}

func TestDiscover_EmptyIncludeMatchesEverything(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "anything.bin"), "payload")

	source := transport.Source{ID: "no-include", Type: "local", Root: root}
	set := backupSet(t, config.Completion{Strategy: "rename"}, nil)
	deps := Deps{Transport: rclone.New(), Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !pathSet(res.Discovered)["anything.bin"] {
		t.Errorf("with no include configured, anything.bin should be Discovered; got %+v", res.Discovered)
	}
}

// --- hostile / malformed names ---------------------------------------------

func TestDiscover_HostileBasenameIsRejectedNotIngested(t *testing.T) {
	fake := &fakeTransport{
		artifacts: []transport.RemoteArtifact{
			{Path: "sneaky\\attempt.dump", Size: 1, ModTime: epoch.Unix()},
		},
	}
	source := transport.Source{ID: "hostile", Type: "local", Root: "/unused"}
	set := backupSet(t, config.Completion{Strategy: "rename"}, nil)
	deps := Deps{Transport: fake, Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Discovered) != 0 {
		t.Fatalf("a hostile basename was discovered: %+v", res.Discovered)
	}
	if len(res.Rejected) != 1 {
		t.Fatalf("Rejected = %+v, want exactly one entry", res.Rejected)
	}
	if res.Rejected[0].RemotePath != "sneaky\\attempt.dump" {
		t.Errorf("Rejected path = %q, want the hostile name", res.Rejected[0].RemotePath)
	}
}

func TestDiscover_TraversalShapedPathIsRejectedNotIngested(t *testing.T) {
	fake := &fakeTransport{
		artifacts: []transport.RemoteArtifact{
			{Path: "../escape.dump", Size: 1, ModTime: epoch.Unix()},
			{Path: "a/../../escape2.dump", Size: 1, ModTime: epoch.Unix()},
		},
	}
	source := transport.Source{ID: "traversal", Type: "local", Root: "/unused"}
	set := backupSet(t, config.Completion{Strategy: "rename"}, nil)
	deps := Deps{Transport: fake, Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Discovered) != 0 {
		t.Fatalf("a traversal-shaped path was discovered: %+v", res.Discovered)
	}
	if len(res.Rejected) != 2 {
		t.Fatalf("Rejected = %+v, want exactly two entries", res.Rejected)
	}
}

// --- per-candidate error resilience ----------------------------------------

func TestDiscover_StatFailureIsPerCandidateNotFatal(t *testing.T) {
	fake := &fakeTransport{
		artifacts: []transport.RemoteArtifact{
			{Path: "good.dump", Size: 1, ModTime: epoch.Unix()},
			{Path: "vanished.dump", Size: 1, ModTime: epoch.Unix()},
		},
		statErr: map[string]error{"vanished.dump": errors.New("object vanished between list and stat")},
		hashes:  map[string]string{"good.dump": "deadbeef"},
	}
	source := transport.Source{ID: "stat-failure", Type: "local", Root: "/unused"}
	set := backupSet(t, config.Completion{Strategy: "rename"}, nil)
	deps := Deps{Transport: fake, Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover returned a fatal error for a per-candidate problem: %v", err)
	}
	if !pathSet(res.Discovered)["good.dump"] {
		t.Errorf("good.dump should still be Discovered despite vanished.dump failing; got %+v", res.Discovered)
	}
	if len(res.Errors) != 1 || res.Errors[0].RemotePath != "vanished.dump" {
		t.Fatalf("Errors = %+v, want exactly one entry for vanished.dump", res.Errors)
	}
}

func TestDiscover_HashFailureDoesNotBlockDiscovery(t *testing.T) {
	fake := &fakeTransport{
		artifacts: []transport.RemoteArtifact{
			{Path: "no-hash-support.dump", Size: 1, ModTime: epoch.Unix()},
		},
		hashErr: map[string]error{"no-hash-support.dump": errors.New("backend cannot compute sha256")},
	}
	source := transport.Source{ID: "hash-failure", Type: "local", Root: "/unused"}
	set := backupSet(t, config.Completion{Strategy: "rename"}, nil)
	deps := Deps{Transport: fake, Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("a hash failure should degrade honestly, not surface as a candidate error: %+v", res.Errors)
	}
	if len(res.Discovered) != 1 {
		t.Fatalf("Discovered = %+v, want exactly one (hash-less) artifact", res.Discovered)
	}
	if res.Discovered[0].Remote.Hash != "" {
		t.Errorf("Remote.Hash = %q, want empty (backend could not compute it)", res.Discovered[0].Remote.Hash)
	}
}

// --- helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func pathSet(recs []state.Record) map[string]bool {
	m := make(map[string]bool, len(recs))
	for _, r := range recs {
		m[r.RemotePath] = true
	}
	return m
}
