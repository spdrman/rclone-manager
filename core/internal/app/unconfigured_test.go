package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is issue #418: the artifacts of a backup set the journal
// remembers and the configuration no longer names.
//
// Every test here is about the category itself rather than about one
// operation on it, because the defect the issue reports is that the
// category had no definition at all: retention never saw it, reconcile
// never saw it, and the one read that did (ListArtifacts) showed rows
// nothing would ever advance.

// setNamed builds a backup set fixture under an arbitrary source and
// name, which testBackupSet cannot do (it is fixed at
// production/postgres-primary).
func setNamed(t *testing.T, source, name, localDir string) config.BackupSet {
	t.Helper()
	bs := testBackupSet(t, localDir)
	bs.Name = name
	bs.ID = mustSetID(t, source, name)
	return bs
}

// strandedRow drives one freshly-discovered artifact to TRANSFERRING with
// a .partial file on disk, which is exactly the shape a cycle stopped
// mid-flight by a removal hold leaves behind (#410's own "what I am not
// sure about" note).
func strandedRow(t *testing.T, ctx context.Context, journal Journal, rec state.Record, partial string) state.Record {
	t.Helper()
	mustWriteFile(t, partial, "half a payload")
	lp := partial
	out, err := lifecycle.Advance(ctx, lifecycle.Deps{Journal: journal, Now: fixedNow(epoch)}, state.Transition{
		Artifact:  rec.Artifact,
		Key:       "test:transferring:" + rec.Artifact.String(),
		From:      string(lifecycle.Discovered),
		To:        string(lifecycle.Transferring),
		LocalPath: &lp,
	})
	if err != nil {
		t.Fatalf("advancing %s to TRANSFERRING: %v", rec.Artifact, err)
	}
	return out.Record
}

// discoverAll is discoverOneRecord for a backup set with more than one
// object waiting on the remote, ordered by artifact name so a test can
// name the rows it just made rather than guessing at map order.
func discoverAll(t *testing.T, ctx context.Context, journal Journal, tr transport.Transport, source transport.Source, bs config.BackupSet, want int) []state.Record {
	t.Helper()
	res, err := discovery.Discover(ctx, discovery.Deps{Transport: tr, Journal: journal, Now: fixedNow(epoch)}, source, bs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Discovered) != want {
		t.Fatalf("Discover found %d artifact(s), want %d (result=%+v)", len(res.Discovered), want, res)
	}
	out := append([]state.Record(nil), res.Discovered...)
	sort.Slice(out, func(i, j int) bool { return out[i].Artifact.Name < out[j].Artifact.Name })
	return out
}

// driveToComplete puts one discovered row into COMPLETE with a durable
// local placement, which is what a finished backup looks like: the shape
// the removal dialog promises stays on storage and stays listed.
func driveToComplete(t *testing.T, ctx context.Context, journal Journal, rec state.Record, final string) {
	t.Helper()
	// The file has to be exactly the size discovery recorded on the
	// remote, or the next reconciliation pass finds a mismatch and
	// quarantines the artifact. A fixture whose "finished backup" is one
	// reconcile away from being a loss is not a finished backup.
	if rec.Remote.Size == nil {
		t.Fatalf("%s was discovered with no recorded size; this fixture cannot build a matching local copy", rec.Artifact)
	}
	size := *rec.Remote.Size
	mustWriteFile(t, final, strings.Repeat("x", int(size)))
	lp := final
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   rec.Artifact,
		Key:        "test:complete:" + rec.Artifact.String(),
		From:       string(lifecycle.Discovered),
		To:         string(lifecycle.Complete),
		OccurredAt: epoch,
		LocalPath:  &lp,
		Placement: &state.PlacementUpdate{
			Medium: state.MediumLocal, Location: final, Size: &size,
			VerificationClass: state.VerificationContent, Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("driving %s to COMPLETE: %v", rec.Artifact, err)
	}
}

func mustGet(t *testing.T, ctx context.Context, journal Journal, id model.ArtifactID) state.Record {
	t.Helper()
	rec, err := journal.Get(ctx, id)
	if err != nil {
		t.Fatalf("journal.Get(%s): %v", id, err)
	}
	return rec
}

// TestUnconfiguredSets_NamesEverySetTheJournalRemembersAndConfigDoesNot is
// the read the whole issue turns on: until it existed, "a set the journal
// remembers" was a category exactly one call could see, and no operator
// could ask for it by name.
func TestUnconfiguredSets_NamesEverySetTheJournalRemembersAndConfigDoesNot(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)

	keptDir, goneDir := t.TempDir(), t.TempDir()
	kept := setNamed(t, "production", "alpha", keptDir)
	gone := setNamed(t, "production", "beta", goneDir)

	keptTr := newFakeTransport()
	keptTr.put("alpha.dump", "alpha payload", epoch.Unix())
	goneTr := newFakeTransport()
	goneTr.put("beta.dump", "beta payload", epoch.Unix())

	discoverOneRecord(t, ctx, journal, keptTr, transport.Source{ID: "kept"}, kept)
	discoverOneRecord(t, ctx, journal, goneTr, transport.Source{ID: "gone"}, gone)

	// Only alpha is configured, which is the state a removal leaves.
	svc := New(testConfig(t, testSource("production", kept)), journal, keptTr, nil)

	sets, err := svc.UnconfiguredSets(ctx)
	if err != nil {
		t.Fatalf("UnconfiguredSets: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("UnconfiguredSets returned %d set(s) (%+v), want exactly production/beta", len(sets), sets)
	}
	if got := sets[0].Set.String(); got != "production/beta" {
		t.Errorf("UnconfiguredSets named %q, want production/beta; a configured set has a policy and belongs nowhere near this list", got)
	}
	if sets[0].Artifacts != 1 {
		t.Errorf("production/beta reports %d artifact(s), want 1", sets[0].Artifacts)
	}
}

// TestUnconfiguredSets_SaysWhatIsRetainedAndWhatIsStranded is acceptance
// criterion one: the lifecycle is defined, and the two halves of it are
// counted apart. A retained backup is what the removal dialog promised
// would stay; a stranded row is residue no promise covers.
func TestUnconfiguredSets_SaysWhatIsRetainedAndWhatIsStranded(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	gone := setNamed(t, "production", "beta", localDir)
	tr := newFakeTransport()
	tr.put("done.dump", "done payload", epoch.Unix())
	tr.put("stuck.dump", "stuck payload", epoch.Unix())

	res := discoverAll(t, ctx, journal, tr, transport.Source{ID: "gone"}, gone, 2)
	done, stuck := res[0], res[1]
	if done.Artifact.Name != "done.dump" {
		done, stuck = stuck, done
	}

	driveToComplete(t, ctx, journal, done, filepath.Join(localDir, "done.dump"))
	strandedRow(t, ctx, journal, stuck, filepath.Join(localDir, "stuck.dump.partial"))

	svc := New(testConfig(t, testSource("production")), journal, tr, nil)

	sets, err := svc.UnconfiguredSets(ctx)
	if err != nil {
		t.Fatalf("UnconfiguredSets: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("UnconfiguredSets returned %d set(s), want 1", len(sets))
	}
	got := sets[0]
	if got.Retained != 1 {
		t.Errorf("Retained = %d, want 1; the one COMPLETE row is the backup the removal dialog promised stays", got.Retained)
	}
	if got.Stranded != 1 {
		t.Errorf("Stranded = %d, want 1; the TRANSFERRING row is what a removal caught mid-flight and nothing will ever advance", got.Stranded)
	}
	if got.Bytes <= 0 {
		t.Errorf("Bytes = %d, want the recorded size of what is pinned on disk; a report that cannot say how much space this costs is not the report this issue asks for", got.Bytes)
	}
}

// TestStrandedArtifacts_PreviewsWithoutWriting keeps the preview honest:
// this product previews before it changes anything, and a preview that
// quietly did the work would make the confirmation meaningless.
func TestStrandedArtifacts_PreviewsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	gone := setNamed(t, "production", "beta", localDir)
	tr := newFakeTransport()
	tr.put("stuck.dump", "stuck payload", epoch.Unix())
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{ID: "gone"}, gone)
	partial := filepath.Join(localDir, "stuck.dump.partial")
	strandedRow(t, ctx, journal, rec, partial)

	svc := New(testConfig(t, testSource("production")), journal, tr, nil)

	found, err := svc.StrandedArtifacts(ctx, gone.ID)
	if err != nil {
		t.Fatalf("StrandedArtifacts: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("StrandedArtifacts returned %d row(s), want 1", len(found))
	}
	if found[0].PartialPath != partial {
		t.Errorf("PartialPath = %q, want %q", found[0].PartialPath, partial)
	}
	if found[0].PartialBytes <= 0 {
		t.Errorf("PartialBytes = %d, want the size of the residue on disk", found[0].PartialBytes)
	}
	if found[0].Cleared {
		t.Error("a preview reported Cleared; nothing has been cleared")
	}
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("the preview removed %s: %v", partial, err)
	}
	if st := mustGet(t, ctx, journal, rec.Artifact).State; st != string(lifecycle.Transferring) {
		t.Errorf("the preview moved the row to %s; a preview writes nothing", st)
	}
}

// TestClearStranded_RemovesTheResidueAndEndsTheRow is acceptance criterion
// two: the .partial and the non-terminal row a removal strands have a way
// to be cleared.
//
// FAILED is the honest end for these rows and not an arbitrary one:
// internal/lifecycle's own localfootprint.go says FAILED is a state with
// NO local bytes, so ending the row there and leaving the .partial behind
// would make FR-21's capacity arithmetic under-count, which is the one
// direction that file says a bias must never point.
func TestClearStranded_RemovesTheResidueAndEndsTheRow(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	gone := setNamed(t, "production", "beta", localDir)
	tr := newFakeTransport()
	tr.put("stuck.dump", "stuck payload", epoch.Unix())
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{ID: "gone"}, gone)
	partial := filepath.Join(localDir, "stuck.dump.partial")
	strandedRow(t, ctx, journal, rec, partial)

	svc := New(testConfig(t, testSource("production")), journal, tr, nil)

	cleared, err := svc.ClearStranded(ctx, gone.ID)
	if err != nil {
		t.Fatalf("ClearStranded: %v", err)
	}
	if len(cleared) != 1 || !cleared[0].Cleared {
		t.Fatalf("ClearStranded reported %+v, want one cleared row", cleared)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%s) = %v, want the .partial gone; a row ended at FAILED with its residue still on disk is space FR-21 stops counting", partial, err)
	}
	if st := mustGet(t, ctx, journal, rec.Artifact).State; st != string(lifecycle.Failed) {
		t.Errorf("the row is %s, want FAILED; nothing will ever advance it and a row that says otherwise is a claim this manager cannot keep", st)
	}
}

// TestClearStranded_RefusesASetTheConfigurationStillHas draws the line
// this operation only makes sense on one side of: a configured set's
// in-flight rows belong to the processing cycle, which will resume them.
// Sweeping those would be this command racing the pipeline and destroying
// a transfer in progress.
func TestClearStranded_RefusesASetTheConfigurationStillHas(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	kept := setNamed(t, "production", "alpha", localDir)
	tr := newFakeTransport()
	tr.put("alpha.dump", "alpha payload", epoch.Unix())
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{ID: "kept"}, kept)
	partial := filepath.Join(localDir, "alpha.dump.partial")
	strandedRow(t, ctx, journal, rec, partial)

	svc := New(testConfig(t, testSource("production", kept)), journal, tr, nil)

	if _, err := svc.ClearStranded(ctx, kept.ID); !errors.Is(err, ErrBackupSetConfigured) {
		t.Fatalf("ClearStranded on a configured set = %v, want ErrBackupSetConfigured", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("the refused sweep removed %s anyway: %v", partial, err)
	}
	if st := mustGet(t, ctx, journal, rec.Artifact).State; st != string(lifecycle.Transferring) {
		t.Errorf("the refused sweep moved the row to %s, want it left at TRANSFERRING for the cycle to resume", st)
	}
}

// TestClearStranded_RefusesAnIDNothingHasEverHeard names the third case,
// following issue #187's rule: an id that is neither configured nor on
// record is a mistake, and answering "nothing to clear" would let a typo
// read as success on an operation that removes files.
func TestClearStranded_RefusesAnIDNothingHasEverHeard(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production")), journal, newFakeTransport(), nil)

	_, err := svc.ClearStranded(ctx, mustSetID(t, "production", "never-existed"))
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("ClearStranded on an unknown id = %v (%T), want a *NotFoundError", err, err)
	}
	if notFound.Kind != "backup set" || notFound.Name != "production/never-existed" {
		t.Errorf("refused %s %q, want backup set \"production/never-existed\"", notFound.Kind, notFound.Name)
	}
}

// TestClearStranded_LeavesEveryDurableCopyExactlyWhereItIs is the safety
// property this whole operation stands on. The removal dialog promises
// the retained backups stay, and a sweep that reached one would be this
// product destroying a backup nobody asked it to destroy.
func TestClearStranded_LeavesEveryDurableCopyExactlyWhereItIs(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	gone := setNamed(t, "production", "beta", localDir)
	tr := newFakeTransport()
	tr.put("done.dump", "done payload", epoch.Unix())
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{ID: "gone"}, gone)
	final := filepath.Join(localDir, "done.dump")
	driveToComplete(t, ctx, journal, rec, final)

	svc := New(testConfig(t, testSource("production")), journal, tr, nil)

	cleared, err := svc.ClearStranded(ctx, gone.ID)
	if err != nil {
		t.Fatalf("ClearStranded: %v", err)
	}
	if len(cleared) != 0 {
		t.Fatalf("ClearStranded reported %+v for a set holding only a finished backup, want nothing to clear", cleared)
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("os.Stat(%s) = %v; the sweep destroyed a retained backup", final, err)
	}
	if st := mustGet(t, ctx, journal, rec.Artifact).State; st != string(lifecycle.Complete) {
		t.Errorf("the finished backup is now %s, want COMPLETE", st)
	}
}

// TestClearStranded_RefusesARowPointingAtSomethingThatIsNotAPartial is
// the structural half of the same guarantee. Rather than trusting that
// only .partial paths can reach here, the sweep asserts it: FR-12 gives
// the temporary name a suffix precisely so that nothing can mistake one
// for a restore point, and this is that rule enforced at the moment a
// file would be removed.
func TestClearStranded_RefusesARowPointingAtSomethingThatIsNotAPartial(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	gone := setNamed(t, "production", "beta", localDir)
	tr := newFakeTransport()
	tr.put("stuck.dump", "stuck payload", epoch.Unix())
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{ID: "gone"}, gone)

	// A TRANSFERRING row whose recorded local path is a FINAL name. No
	// production path writes this, which is the point: the guard has to
	// hold whatever put the row there.
	final := filepath.Join(localDir, "stuck.dump")
	mustWriteFile(t, final, "a whole payload")
	lp := final
	if _, err := lifecycle.Advance(ctx, lifecycle.Deps{Journal: journal, Now: fixedNow(epoch)}, state.Transition{
		Artifact:  rec.Artifact,
		Key:       "test:transferring-final",
		From:      string(lifecycle.Discovered),
		To:        string(lifecycle.Transferring),
		LocalPath: &lp,
	}); err != nil {
		t.Fatalf("planting the row: %v", err)
	}

	svc := New(testConfig(t, testSource("production")), journal, tr, nil)

	cleared, err := svc.ClearStranded(ctx, gone.ID)
	if err != nil {
		t.Fatalf("ClearStranded: %v", err)
	}
	if len(cleared) != 1 {
		t.Fatalf("ClearStranded reported %d row(s), want 1", len(cleared))
	}
	if cleared[0].Cleared || cleared[0].Err == nil {
		t.Errorf("row reported Cleared=%v Err=%v, want refused with a reason", cleared[0].Cleared, cleared[0].Err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("os.Stat(%s) = %v; the sweep removed a file that is not a .partial", final, err)
	}
	if st := mustGet(t, ctx, journal, rec.Artifact).State; st != string(lifecycle.Transferring) {
		t.Errorf("the refused row moved to %s, want it left where it was", st)
	}
}

// TestClearStranded_RemovesTheFileBeforeItEndsTheRow pins the ordering,
// which is the only crash-safety decision this operation has to make.
//
// Both orders leave residue if the process dies between the two steps,
// and they are not equally bad. File first, row still TRANSFERRING: the
// capacity guard counts bytes that are already gone, which over-states
// usage, and a second sweep finishes the job. Row first, file still
// there: the row says FAILED, which internal/lifecycle defines as holding
// NO local bytes, so those bytes stop being counted by anything and
// nothing on any screen can see them again. Under-counting a ceiling is
// how a ceiling stops being one.
func TestClearStranded_RemovesTheFileBeforeItEndsTheRow(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	gone := setNamed(t, "production", "beta", localDir)
	tr := newFakeTransport()
	tr.put("stuck.dump", "stuck payload", epoch.Unix())
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{ID: "gone"}, gone)
	partial := filepath.Join(localDir, "stuck.dump.partial")
	strandedRow(t, ctx, journal, rec, partial)

	boom := errors.New("the journal went away mid-sweep")
	failing := &refusingTransitionJournal{Journal: journal, to: string(lifecycle.Failed), err: boom}
	svc := New(testConfig(t, testSource("production")), failing, tr, nil)

	cleared, err := svc.ClearStranded(ctx, gone.ID)
	if err != nil {
		t.Fatalf("ClearStranded: %v", err)
	}
	if len(cleared) != 1 || cleared[0].Cleared || !errors.Is(cleared[0].Err, boom) {
		t.Fatalf("ClearStranded reported %+v, want one uncleared row carrying the journal's own error", cleared)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%s) = %v, want the file already gone; the removal has to happen first so a crash over-states usage rather than hiding it", partial, err)
	}
	if st := mustGet(t, ctx, journal, rec.Artifact).State; st != string(lifecycle.Transferring) {
		t.Errorf("the row is %s, want it still TRANSFERRING so a second sweep can finish what this one started", st)
	}

	// And a second sweep does finish it, which is what makes the
	// over-counting window bounded rather than permanent.
	again := New(testConfig(t, testSource("production")), journal, tr, nil)
	if _, err := again.ClearStranded(ctx, gone.ID); err != nil {
		t.Fatalf("second ClearStranded: %v", err)
	}
	if st := mustGet(t, ctx, journal, rec.Artifact).State; st != string(lifecycle.Failed) {
		t.Errorf("after a second sweep the row is %s, want FAILED", st)
	}
}

// refusingTransitionJournal fails exactly one edge, so a test can stop a
// sweep between its two steps without stopping anything else.
type refusingTransitionJournal struct {
	Journal
	to  string
	err error
}

func (r *refusingTransitionJournal) RecordTransition(ctx context.Context, t state.Transition) (state.Outcome, error) {
	if t.To == r.to {
		return state.Outcome{}, r.err
	}
	return r.Journal.RecordTransition(ctx, t)
}

// TestRunCycle_SaysWhatItIsHoldingOutsideEveryConfiguredSet is the half of
// this report that reaches a deployment nobody is typing commands at.
// `daemon` has no exit status and no operator at a prompt, so a NAS
// filling up with backups no policy governs would otherwise be visible
// only to somebody who thought to go and look.
func TestRunCycle_SaysWhatItIsHoldingOutsideEveryConfiguredSet(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	gone := setNamed(t, "production", "beta", localDir)
	tr := newFakeTransport()
	tr.put("done.dump", "done payload", epoch.Unix())
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{ID: "gone"}, gone)
	driveToComplete(t, ctx, journal, rec, filepath.Join(localDir, "done.dump"))

	var log bytes.Buffer
	svc := New(testConfig(t, testSource("production")), journal, tr, obs.New(&log, obs.LevelInfo))
	svc.RunCycle(ctx)

	line := log.String()
	if !strings.Contains(line, "artifacts_ungoverned") {
		t.Errorf("a cycle over a deployment holding a removed set's backups says nothing about them:\n%s", line)
	}
	if !strings.Contains(line, `"artifacts":1`) || !strings.Contains(line, `"backup_sets":1`) {
		t.Errorf("the event does not carry what is being held:\n%s", line)
	}
}

// TestRunCycle_SaysNothingWhenEverythingIsGoverned is the control. A line
// that appears on every cycle of every deployment is a line an operator
// stops seeing, which is the failure mode this whole issue is about one
// level up.
func TestRunCycle_SaysNothingWhenEverythingIsGoverned(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	kept := setNamed(t, "production", "alpha", localDir)
	tr := newFakeTransport()
	tr.put("done.dump", "done payload", epoch.Unix())
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{ID: "kept"}, kept)
	driveToComplete(t, ctx, journal, rec, filepath.Join(localDir, "done.dump"))

	var log bytes.Buffer
	svc := New(testConfig(t, testSource("production", kept)), journal, tr, obs.New(&log, obs.LevelInfo))
	svc.RunCycle(ctx)

	if line := log.String(); strings.Contains(line, "artifacts_ungoverned") {
		t.Errorf("a fully-configured deployment was told it is holding ungoverned backups:\n%s", line)
	}
}

// TestUnconfiguredSets_CountsEveryRowIntoExactlyOneColumn keeps the
// report's arithmetic answerable. An operator reading "5 artifact(s), 2
// retained, 1 stranded" has to be able to account for the other two
// without guessing, and a row that lands in no column at all is a row the
// report has quietly lost.
func TestUnconfiguredSets_CountsEveryRowIntoExactlyOneColumn(t *testing.T) {
	ctx := context.Background()
	journal := openJournal(t)
	localDir := t.TempDir()

	gone := setNamed(t, "production", "beta", localDir)
	tr := newFakeTransport()
	for _, name := range []string{"done.dump", "stuck.dump", "bad.dump", "gaveup.dump"} {
		tr.put(name, "payload for "+name, epoch.Unix())
	}
	rows := discoverAll(t, ctx, journal, tr, transport.Source{ID: "gone"}, gone, 4)
	byName := map[string]state.Record{}
	for _, r := range rows {
		byName[r.Artifact.Name] = r
	}

	driveToComplete(t, ctx, journal, byName["done.dump"], filepath.Join(localDir, "done.dump"))
	strandedRow(t, ctx, journal, byName["stuck.dump"], filepath.Join(localDir, "stuck.dump.partial"))
	advanceTo(t, ctx, journal, byName["bad.dump"], lifecycle.Discovered, lifecycle.Failed)
	advanceTo(t, ctx, journal, byName["bad.dump"], lifecycle.Failed, lifecycle.Quarantined)
	advanceTo(t, ctx, journal, byName["gaveup.dump"], lifecycle.Discovered, lifecycle.Failed)

	svc := New(testConfig(t, testSource("production")), journal, tr, nil)
	sets, err := svc.UnconfiguredSets(ctx)
	if err != nil {
		t.Fatalf("UnconfiguredSets: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("UnconfiguredSets = %+v, want one set", sets)
	}
	u := sets[0]
	if u.Artifacts != 4 {
		t.Fatalf("Artifacts = %d, want 4", u.Artifacts)
	}
	if got := u.Retained + u.Stranded + u.Quarantined + u.Failed; got != u.Artifacts {
		t.Errorf("the columns add to %d over %d artifact(s) (%+v); every row has to land in exactly one, or the report has lost some", got, u.Artifacts, u)
	}
	if u.Failed != 1 {
		t.Errorf("Failed = %d, want 1", u.Failed)
	}
}

// advanceTo takes one legal edge, for a fixture that needs a row in a
// state no ordinary pass would leave it in here.
func advanceTo(t *testing.T, ctx context.Context, journal Journal, rec state.Record, from, to lifecycle.State) {
	t.Helper()
	if _, err := lifecycle.Advance(ctx, lifecycle.Deps{Journal: journal, Now: fixedNow(epoch)}, state.Transition{
		Artifact: rec.Artifact,
		Key:      "test:" + rec.Artifact.String() + ":" + string(from) + "->" + string(to),
		From:     string(from),
		To:       string(to),
	}); err != nil {
		t.Fatalf("advancing %s from %s to %s: %v", rec.Artifact, from, to, err)
	}
}
