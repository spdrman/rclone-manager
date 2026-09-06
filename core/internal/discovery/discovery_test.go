// These drive whole Discover passes, and they are deliberately split
// between two kinds of transport.
//
// The strategy and filtering tests run against the REAL rclone adapter over
// a local temp directory. That is the expensive choice and it is the right
// one: the completeness strategies are claims about what a listing looks
// like, and a hand-written double would let this package's beliefs about
// recursion, path shapes and modification times drift from what the adapter
// actually produces. TestDiscover_RenameStrategy_RecursesAndSkipsTempNames
// exists precisely because that drift once happened.
//
// The hostile-input and failure tests use fakeTransport, because a real
// backend will not hand back a name containing a backslash and will not fail
// one Stat out of two on request.
//
// Every test here injects a fixed clock. Only the stable strategy reads it
// for a decision, but every discovered row is stamped with it, so freezing
// it everywhere keeps a failure message from depending on when the suite
// ran.
package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// openJournal opens a real SQLite journal in a temp directory, one per
// test.
//
// It is the real thing rather than an in-memory fake because the property
// several of these tests turn on lives in the schema, not in this package:
// the UNIQUE(source, backup_set, artifact_name) constraint is what turns a
// basename collision into state.ErrAlreadyDiscovered, and a fake journal
// would have to reimplement that to be useful, at which point the test would
// be checking the fake.
func openJournal(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// mustSetID builds a validated backup set id, failing the test rather than
// returning an error: a fixture this package cannot construct is not a case
// under test.
func mustSetID(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

// backupSet assembles the config.BackupSet these tests hand to Discover.
//
// It sets ID explicitly, which is the field Discover refuses to run without.
// That is not incidental: config.Validate is what normally populates ID, so
// a test constructing a BackupSet literal is exactly the caller Discover's
// "run it through config.Validate first" guard is aimed at, and every test
// here has to satisfy that guard the same way the real pipeline does.
func backupSet(t *testing.T, completion config.Completion, include []string) config.BackupSet {
	t.Helper()
	return config.BackupSet{
		Name:       "postgres-primary",
		ID:         mustSetID(t, "production", "postgres-primary"),
		Include:    include,
		Completion: completion,
	}
}

// fixedNow freezes the clock Deps takes. See this file's header for why
// every test uses one even when nothing reads it for a decision.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// epoch is the single instant every test in this file treats as "now".
// Modification times in the stable-strategy tests are expressed as offsets
// from it, so the boundary between fresh and old is exact rather than a
// race against how long the suite took to get there.
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

// TestDiscover_BasenameCollisionAcrossDirectoriesIsReported is the hazard
// recursion created, staged exactly as a real producer would create it: one
// directory per run, the same filename in each.
//
// The assertions are written to be indifferent to WHICH path wins, and that
// is on purpose. Listing order is the adapter's business and could change,
// so pinning a winner would make this a test of rclone. What must hold
// either way is the relationship: exactly one row exists, the conflict names
// the other path, and the conflict's RecordedPath is the path the journal
// actually holds. An implementation that reported a conflict against a path
// it had not stored would pass a weaker version of this and leave an
// operator chasing a row that is not there.
//
// The journal read at the end is the part that would catch the worst
// outcome, which is not a missing conflict report but a row whose RemotePath
// was overwritten by the loser: that would leave the journal pointing a
// stale path at an artifact that was replaced, which is the quiet corruption
// the package doc names.
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

// TestDiscover_SecondCallOnTheSamePathIsAlreadyKnownNotAnError is the other
// side of the collision case, and the two have to be read together: the same
// journal constraint fires in both, and this pins that Discover tells them
// apart by comparing the stored path.
//
// Running twice over an unchanged remote is the normal case, once per
// scheduled pass for the life of the artifact, so getting this wrong would
// not be a rare bug. The explicit "not a conflict" assertion is what stops a
// future change from routing every repeat pass into Conflicts, which would
// bury the real collisions under one line per artifact per cycle.
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

// TestDiscover_MarkerStrategy stages all four cases in one directory tree,
// because the interesting assertions are about what does NOT appear.
//
// A marker file is a completion signal, not a payload, and it also matches
// nothing in the include patterns, so there are two independent reasons it
// should be skipped and only one of them is the one under test. The
// assertions therefore check that neither marker shows up in Discovered nor
// in Pending: reporting a marker as pending would be a line an operator sees
// on every pass for ever, about a file that is behaving correctly.
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

// TestDiscover_MarkerStrategy_ConfigurableManifestMarker is issue #291's
// literal acceptance criterion: "A directory without the marker still
// holds its artifacts back, proven by a test that adds the marker and
// watches the same artifacts become complete."
//
// The producer is the issue's own example: a Gitea backup on a read-only
// production host that writes one directory per run and signals it
// finished with SHA256SUMS, written last, after every artifact -- a name
// this manager cannot ask the producer to change to "_SUCCESS". Discover
// runs twice against the same directory and journal: once before
// SHA256SUMS exists, once after, with nothing else about the directory
// changing in between.
func TestDiscover_MarkerStrategy_ConfigurableManifestMarker(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "run", "gitea-dump.tar.gz"), "dump payload")
	mustWrite(t, filepath.Join(root, "run", "gitea-db.dump"), "db payload")

	source := transport.Source{ID: "gitea-backup", Type: "local", Root: root}
	set := backupSet(t, config.Completion{Strategy: "marker", ManifestMarker: "SHA256SUMS"}, []string{"*.tar.gz", "*.dump"})
	deps := Deps{Transport: rclone.New(), Journal: openJournal(t), Now: fixedNow(epoch)}

	// Before: the directory has no SHA256SUMS yet. Both artifacts are held
	// back as Pending, even though the config already names the marker it
	// is waiting for -- naming it is not the same as it existing.
	before, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover (before marker exists): %v", err)
	}
	if len(before.Discovered) != 0 {
		t.Fatalf("before the marker exists, Discovered = %+v, want none", before.Discovered)
	}
	pendingBefore := map[string]bool{}
	for _, p := range before.Pending {
		pendingBefore[p.RemotePath] = true
	}
	for _, want := range []string{"run/gitea-dump.tar.gz", "run/gitea-db.dump"} {
		if !pendingBefore[want] {
			t.Fatalf("%q was not Pending before the marker existed; Pending=%+v", want, before.Pending)
		}
	}

	// The producer finishes its run and writes its own completion signal.
	mustWrite(t, filepath.Join(root, "run", "SHA256SUMS"), "checksums")

	after, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover (after marker exists): %v", err)
	}
	discoveredAfter := pathSet(after.Discovered)
	for _, want := range []string{"run/gitea-dump.tar.gz", "run/gitea-db.dump"} {
		if !discoveredAfter[want] {
			t.Fatalf("%q was not Discovered after the marker appeared; Discovered=%+v", want, after.Discovered)
		}
	}
	for _, rec := range after.Discovered {
		if rec.RemotePath == "run/SHA256SUMS" {
			t.Fatalf("the marker file itself was discovered as an artifact: %+v", rec)
		}
	}
}

// --- stable strategy -------------------------------------------------------

// TestDiscover_StableStrategy sets the modification times explicitly with
// Chtimes rather than relying on when the files happened to be created.
//
// That is what makes the two cases mean something: "an hour old" and "one
// second old" against a ten-minute window are both far from the boundary, so
// the test cannot flip on a slow machine, and both are compared against the
// same frozen now the code sees.
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

// TestDiscover_IncludeFiltersNonMatchingNamesSilently pins that a
// non-matching name is not reported anywhere at all.
//
// The word doing the work is silently. A backup root routinely contains
// files nobody asked this manager to back up, so an unmatched name reported
// as Pending or Rejected would produce steady noise on every pass, and noise
// on every pass is how a real rejection gets missed. Both buckets are
// asserted empty for notes.txt because either one alone would leave the
// other as an available place to put it.
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

// TestDiscover_EmptyIncludeMatchesEverything is the end-to-end version of
// includeMatches' nil case, and it uses a name no plausible pattern would
// match ("anything.bin") so it cannot pass by coincidence. The failure it
// guards against is a backup set with no include configured discovering
// nothing while looking perfectly healthy, which is the shape of outage that
// is only noticed when a restore is needed.
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

// TestDiscover_HostileBasenameIsRejectedNotIngested uses a backslash,
// which is the case that needs a fake transport: the local adapter would
// have to be running on a filesystem willing to create such a name.
//
// A backslash is not a path separator on the platforms this runs on, so it
// survives isCleanRelativePath and is caught one step later by
// model.NewArtifactID. That is the point of the case: it proves the second
// gate is real rather than redundant, since a name can be a clean relative
// path and still be unusable as an identity.
//
// Rejected rather than dropped is asserted explicitly. A hostile name is
// exactly the thing an operator needs told about, so disappearing quietly
// would be the worst available outcome.
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

// TestDiscover_TraversalShapedPathIsRejectedNotIngested covers the first
// gate with two different shapes: a leading "..", and one buried mid-path
// that only resolves to an escape after cleaning. The second is there
// because a check that only looked at the start of the string would pass the
// first case and let the second through.
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

// TestDiscover_StatFailureIsPerCandidateNotFatal stages the real race:
// an object listed a moment ago and gone by the time its identity is
// captured, which is what a producer's own cleanup does routinely.
//
// The good artifact is asserted to have been discovered anyway, and that
// half is the whole point. Returning early on the first candidate error
// would make one vanishing file hide every other artifact in the listing, so
// a busy source with steady producer churn could stop discovering anything
// at all while reporting a single unremarkable error.
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

// TestDiscover_HashFailureDoesNotBlockDiscovery pins the degrade-honestly
// rule for the one identity field a backend is allowed not to have.
//
// The hardened, shell-less SFTP account FR-6 recommends cannot compute a
// SHA-256, and that is the recommended posture rather than a
// misconfiguration, so a hash failure must not surface as a candidate error.
// The last assertion is the honest half: the artifact is discovered with an
// EMPTY hash rather than with something invented, so every later stage can
// see that this identity has no hash to compare against instead of
// comparing against a placeholder.
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

// mustWrite creates a file and every directory above it. The MkdirAll is
// what lets the nested-producer fixtures be written as one path each, which
// is how the run-per-directory layouts in this file stay readable.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// pathSet indexes records by remote path so a test can ask "was this
// discovered" without caring about order. Discover's output order follows
// the listing, which is the adapter's business rather than this package's,
// so asserting on a slice position would be pinning something no contract
// promises.
func pathSet(recs []state.Record) map[string]bool {
	m := make(map[string]bool, len(recs))
	for _, r := range recs {
		m[r.RemotePath] = true
	}
	return m
}
