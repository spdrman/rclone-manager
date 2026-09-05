package placement_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is issue #429: a chain with two medium tiers produces a move
// whose source and destination are both mediums, and the engine used to
// refuse it outright.
//
// The refusal was load-bearing rather than conservative policy. Removing
// the guard on its own does not produce a medium-to-medium move, it
// produces an incoherent one: copyToMedium hands the SOURCE placement's
// location straight to UploadFromLocal, and for a medium-resident source
// that is an object key, not a path on disk. So the work is the staging
// copy the refusal's own comment named, and the failure modes that come
// with it.
//
// # What the standing invariant rests on while three copies exist
//
// FR-30's invariant is "a managed-complete artifact has at least one
// ACTIVE placement at a sufficient verification class, at every instant",
// and a staged move genuinely has three copies of the bytes in the world
// at once: the source object, the staging file, and the destination
// object. The decision this suite pins is that the invariant rests on the
// SOURCE for the whole of the copy phase, and on the destination only
// once the VERIFIED write has recorded it, exactly as it does for an
// ordinary local-to-medium move.
//
// The staging copy is deliberately not a placement and never becomes one.
// It is not durable in the sense a placement claims (it is removed at the
// end of the copy), it is not verified against anything a later reader
// could check, and giving it a row would mean the engine's own
// re-eligibility check found two ACTIVE placements and refused the move it
// is in the middle of. So it is a file in a directory this engine owns,
// and the checks below assert it never appears in the journal and never
// lands on the artifact's own local path.

// mediumB is the second medium in this file's world: the annual rung of a
// two-medium chain. mediumA is the fixture's own testMedium.
const mediumB = "annual_s3"

// --- a two-medium world -------------------------------------------------

// twoMediums resolves both ends of a medium-to-medium move.
//
// The mediums are held by pointer so a cell can change one AFTER the
// artifact has been moved onto it, which is how the archive-source cell
// builds a world the engine could not have been driven into: no tier may
// deliver to an archive class, so a copy that sits on one got there
// before the class did, which is exactly what a bucket lifecycle rule
// does.
type twoMediums struct {
	a, b  *transport.Medium
	class placement.Class
}

func (m twoMediums) Resolve(id string) (transport.Medium, placement.Class, error) {
	switch id {
	case m.a.ID:
		return *m.a, m.class, nil
	case m.b.ID:
		return *m.b, m.class, nil
	}
	return transport.Medium{}, "", fmt.Errorf("no medium %q is configured", id)
}

// recordingStore counts OpenObject per key.
//
// The fixture's own openCount cannot answer the question these cells ask.
// A completed move opens the DESTINATION twice, once to verify it and
// once to re-verify it immediately before the source delete, so a
// whole-store counter cannot tell "the source was downloaded to stage it"
// from "the destination was read back to verify it", and a cell asserting
// no download would pass or fail on which phase the move reached.
type recordingStore struct {
	placement.MediumStore

	mu    sync.Mutex
	opens map[string]int
}

func (s *recordingStore) OpenObject(ctx context.Context, medium transport.Medium, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	s.opens[key]++
	s.mu.Unlock()
	return s.MediumStore.OpenObject(ctx, medium, key)
}

func (s *recordingStore) opensOf(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens[key]
}

// recordingLocal remembers every locator the engine wrote to or removed
// from local disk, which is how the cells below assert that the staging
// copy never touched the artifact's own path.
type recordingLocal struct {
	placement.LocalStore

	mu   sync.Mutex
	puts []string
}

func (l *recordingLocal) Put(ctx context.Context, locator string, r io.Reader) error {
	l.mu.Lock()
	l.puts = append(l.puts, locator)
	l.mu.Unlock()
	return l.LocalStore.Put(ctx, locator, r)
}

func (l *recordingLocal) putLog() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.puts...)
}

// twoMediumFixture is the fixture plus a second medium and the artifact
// already moved onto the first one.
type twoMediumFixture struct {
	*fixture
	a, b  *transport.Medium
	keyB  string
	local *recordingLocal
	store *recordingStore
}

// newTwoMediumFixture builds the world every cell here runs in: an
// artifact that this engine really moved from local disk onto medium A,
// so the medium-to-medium move that follows starts from a placement the
// product wrote rather than one a test invented.
func newTwoMediumFixture(t *testing.T, opts fixtureOpts) *twoMediumFixture {
	t.Helper()
	f := newFixture(t, opts)

	a := &transport.Medium{
		ID: testMedium, Type: transport.MediumTypeS3, Bucket: "nas-backups",
		Prefix: "rclone-manager", StorageClass: opts.storageClass,
	}
	b := &transport.Medium{
		ID: mediumB, Type: transport.MediumTypeS3, Bucket: "nas-annual",
		Prefix: "annual", StorageClass: "",
	}
	class := opts.class
	if class == "" {
		class = placement.Content
	}
	f.engine.Mediums = twoMediums{a: a, b: b, class: class}

	local := &recordingLocal{LocalStore: f.engine.Local}
	f.engine.Local = local
	store := &recordingStore{MediumStore: f.engine.Store, opens: map[string]int{}}
	f.engine.Store = store

	keyB, err := transport.MediumKey(b.Prefix, f.artifact)
	if err != nil {
		t.Fatalf("computing the second medium's key: %v", err)
	}

	tf := &twoMediumFixture{fixture: f, a: a, b: b, keyB: keyB, local: local, store: store}

	// The first hop, run for real. Everything below is about what happens
	// to an artifact whose one ACTIVE copy is on a medium, and seeding
	// that placement by hand would be seeding the premise.
	report := f.runCycle()
	if report.Completed != 1 {
		t.Fatalf("the local -> %s hop this world is built on did not complete: %+v", testMedium, report)
	}
	if f.localExists() {
		t.Fatalf("the local copy survived the first hop, so the second one is not medium-to-medium")
	}
	return tf
}

// moveToB runs one cycle planning the medium-to-medium hop.
func (f *twoMediumFixture) moveToB() placement.CycleReport {
	f.t.Helper()
	report, err := f.engine.RunCycle(f.ctx, []placement.Plan{{Artifact: f.artifact, DestinationMedium: mediumB}})
	if err != nil {
		f.t.Fatalf("RunCycle: %v", err)
	}
	return report
}

// stagingDir is where a staged move puts its local copy, and stagingPath
// is the file itself.
func (f *twoMediumFixture) stagingDir() string {
	return filepath.Join(f.localDir, placement.StagingDirName)
}

func (f *twoMediumFixture) stagingPath() string {
	return filepath.Join(f.stagingDir(), f.artifact.Name)
}

// stagingLeftovers is every file still sitting in the staging area.
func (f *twoMediumFixture) stagingLeftovers() []string {
	f.t.Helper()
	entries, err := os.ReadDir(f.stagingDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("reading the staging area: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// --- the move itself ----------------------------------------------------

// TestAMediumToMediumMoveStagesThroughLocalAndCompletes is #429's
// acceptance line.
//
// The chain's second hop is the whole subject: an artifact that reached
// the monthly medium ages into the annual tier, whose home is a different
// medium. It has to arrive, the source has to go, and the standing
// invariant has to hold at every instant on the way, which the fixture's
// guard asserts at every journal write and before every delete.
func TestAMediumToMediumMoveStagesThroughLocalAndCompletes(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})

	report := f.moveToB()
	f.guard.fail()

	if report.Planned != 1 || report.Completed != 1 {
		t.Fatalf("expected one planned, completed medium-to-medium move; got %+v", report)
	}

	// The bytes are on the second medium and they are the artifact's.
	if !f.medium.has(f.keyB) {
		t.Fatalf("nothing was written to %q at %q", mediumB, f.keyB)
	}
	if got := string(f.medium.bytesAt(f.keyB)); got != string(f.content) {
		t.Errorf("the copy on %q does not hold the artifact's bytes", mediumB)
	}

	// The source object is gone from the first medium, which is what
	// makes this a move rather than a second copy.
	if f.medium.has(f.key) {
		t.Errorf("the source object is still on %q after a completed move", testMedium)
	}

	// The journal says both.
	dst, ok := f.placement(mediumB)
	if !ok {
		t.Fatal("no placement records the copy on the second medium")
	}
	if dst.Status != state.PlacementActive || dst.VerificationClass != state.VerificationContent {
		t.Errorf("the destination placement is %s/%s, want ACTIVE/%s", dst.Status, dst.VerificationClass, state.VerificationContent)
	}
	src, ok := f.placement(testMedium)
	if !ok {
		t.Fatal("the source placement row disappeared; a deleted copy is recorded as GONE, never removed")
	}
	if src.Status != state.PlacementGone {
		t.Errorf("the source placement on %q is %s after the delete, want GONE", testMedium, src.Status)
	}

	// The phase machine is untouched. A staged move is the same six
	// steps; the staging is inside the copy phase and is not a phase of
	// its own, which is what keeps the crash matrix's coverage honest.
	want := []string{
		"PLANNED->COPYING", "COPYING->COPIED", "COPIED->VERIFYING",
		"VERIFYING->VERIFIED", "VERIFIED->SOURCE_DELETE_PENDING", "SOURCE_DELETE_PENDING->DONE",
	}
	got := f.guarded.phaseWrites()
	// The first hop wrote the same six, so only the tail belongs to this
	// move.
	if len(got) < len(want) {
		t.Fatalf("the two hops together wrote %v, which is fewer writes than one move takes", got)
	}
	got = got[len(got)-len(want):]
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the medium-to-medium move walked %v\nwant %v", got, want)
	}

	// And nothing is left in the staging area.
	if leftovers := f.stagingLeftovers(); len(leftovers) != 0 {
		t.Errorf("the staging area still holds %v after a completed move; a staging copy is not a copy anything believes in and must not survive the move that made it", leftovers)
	}
}

// TestTheStagingCopyIsNeverTheCopyTheInvariantRestsOn is the claim the
// issue asks to be decided rather than assumed: with three copies in
// existence, which one is FR-30's invariant satisfied by at each instant?
//
// The answer is the source, until the destination earns a placements row.
// The staging file is never in the journal at all, so it can never be the
// answer, and this asserts that structurally rather than by inspecting a
// moment: no placement row ever names the local medium as ACTIVE during
// this move, and nothing was ever written to the artifact's own local
// path, which is the one local location a placement could point at.
func TestTheStagingCopyIsNeverTheCopyTheInvariantRestsOn(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})

	// The local placement is GONE after the first hop, which is the state
	// this cell needs: if the staging copy were quietly recorded, this row
	// would come back to life.
	before, ok := f.placement(state.MediumLocal)
	if !ok || before.Status != state.PlacementGone {
		t.Fatalf("the local placement is %+v before the second hop; this cell needs it GONE or it proves nothing", before)
	}

	f.moveToB()
	f.guard.fail()

	after, ok := f.placement(state.MediumLocal)
	if !ok {
		t.Fatal("the local placement row disappeared")
	}
	if after.Status != state.PlacementGone {
		t.Errorf("the local placement is %s after a staged move; the staging copy became a placement, and the journal now claims a durable local copy that the copy phase deletes", after.Status)
	}
	if after.Location != f.localPath() {
		t.Errorf("the local placement's location moved to %q; the staging copy overwrote the row that describes the artifact's own path", after.Location)
	}

	// Nothing was ever written to the artifact's own local path. That is
	// the location FR-20's containment proof accepts and the one a
	// placement row would name, so a staging copy that landed there would
	// be indistinguishable from a real local copy to every guard
	// downstream.
	for _, put := range f.local.putLog() {
		if samePlace(put, f.localPath()) {
			t.Errorf("the engine wrote to the artifact's own local path %q while staging; the staging copy must live somewhere no placement can point at", put)
		}
	}

	// And it did stage somewhere, so the check above is not passing
	// because nothing was written at all.
	staged := false
	for _, put := range f.local.putLog() {
		if put == f.stagingPath() {
			staged = true
		}
	}
	if !staged {
		t.Fatalf("nothing was written to the staging path %q, so this cell watched a move that did not stage: puts were %v", f.stagingPath(), f.local.putLog())
	}
}

// --- the failure modes staging brings with it ---------------------------

// TestAMediumToMediumMoveWithNoRoomToStageIsRefused is the size check the
// issue names.
//
// A staged move needs the artifact's whole size on the backup set's own
// disk, and it needs it before it starts: a download that runs out of
// space partway leaves a truncated file and a wasted egress bill, and on
// a full disk it can take the backup set's own local root down with it.
// So the arithmetic happens first, from the size the journal already
// recorded, and the refusal costs nothing.
func TestAMediumToMediumMoveWithNoRoomToStageIsRefused(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})

	opensBefore := f.store.opensOf(f.key)
	uploadsBefore := f.medium.uploadCount()
	f.engine.StagingFreeBytes = func(string) (int64, error) { return int64(len(f.content)) - 1, nil }

	f.moveToB()
	f.guard.fail()

	if got := f.store.opensOf(f.key) - opensBefore; got != 0 {
		t.Errorf("the engine read the source object %d time(s) after deciding it had nowhere to put it", got)
	}
	if f.medium.uploadCount() != uploadsBefore {
		t.Errorf("the engine uploaded %d time(s) with no room to stage", f.medium.uploadCount()-uploadsBefore)
	}
	if f.medium.has(f.keyB) {
		t.Error("an object reached the second medium with no room to stage the copy it was made from")
	}

	// The artifact is exactly where it was, and the reason is on the row.
	if !f.medium.has(f.key) {
		t.Fatal("THE SOURCE OBJECT WAS DELETED for a move that never copied anything")
	}
	src, _ := f.placement(testMedium)
	if src.Status != state.PlacementActive {
		t.Errorf("the source placement is %s after a refused copy, want ACTIVE", src.Status)
	}
	mv := f.moves()[len(f.moves())-1]
	for _, want := range []string{"stag", "bytes"} {
		if !strings.Contains(mv.Error, want) {
			t.Errorf("the move row's reason does not carry %q, so it does not say what ran out: %q", want, mv.Error)
		}
	}
	if leftovers := f.stagingLeftovers(); len(leftovers) != 0 {
		t.Errorf("the staging area holds %v after a move that never staged", leftovers)
	}
}

// TestAnUnreadableArchiveSourceIsRefusedWithoutSpendingAGet is the second
// failure mode, and it is the expensive one.
//
// A staged move has to READ the source, which is a content-class
// capability, and an archived object cannot serve one until somebody has
// paid for a restore. The engine already knows how to ask that question
// cheaply for a destination it is about to write; this is the same
// question asked about the source it is about to read, and asking it
// before the GET is what stops the endpoint answering InvalidObjectState
// and the engine reading that as a failed copy worth retrying.
//
// The world is built by moving the artifact onto an ordinary medium and
// then changing that medium's class, because that is the only way this
// state arises: no tier may deliver to an archive class, so a copy on one
// got there before the class did, which is what a bucket lifecycle rule
// does.
func TestAnUnreadableArchiveSourceIsRefusedWithoutSpendingAGet(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})
	f.medium.archiveRefusesReads = true
	f.a.StorageClass = config.StorageClassDeepArchive

	opensBefore := f.store.opensOf(f.key)
	f.moveToB()
	f.guard.fail()

	if got := f.store.opensOf(f.key) - opensBefore; got != 0 {
		t.Errorf("the engine spent %d GET(s) on an object its own class table says cannot be read", got)
	}
	if f.medium.has(f.keyB) {
		t.Error("bytes reached the second medium from a source nothing could read")
	}
	if !f.medium.has(f.key) {
		t.Fatal("THE SOURCE OBJECT WAS DELETED against a move that could not even read it")
	}

	mv := f.moves()[len(f.moves())-1]
	if placement.Phase(mv.Phase) != placement.Abandoned {
		t.Errorf("the move is at %s; a refusal no retry can change must not be retried until the attempt budget runs out", mv.Phase)
	}
	for _, want := range []string{config.StorageClassDeepArchive, "restore"} {
		if !strings.Contains(mv.Error, want) {
			t.Errorf("the move row's reason does not carry %q, so an operator cannot tell why: %q", want, mv.Error)
		}
	}
	if leftovers := f.stagingLeftovers(); len(leftovers) != 0 {
		t.Errorf("the staging area holds %v after a move that could not read its source", leftovers)
	}
}

// TestAStaleStagingCopyIsReplacedRatherThanUploaded is the crash case that
// makes a deterministic staging path safe.
//
// The path is deterministic so an interrupted move converges on the same
// file instead of leaving a second one behind. That is only safe if what
// is already there is checked rather than trusted: a staging file left by
// a crash could be a truncated download, and uploading it would put bytes
// on the destination that the source hash will then refuse, twice, before
// the move gives up.
func TestAStaleStagingCopyIsReplacedRatherThanUploaded(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})

	if err := os.MkdirAll(f.stagingDir(), 0o750); err != nil {
		t.Fatalf("creating the staging area: %v", err)
	}
	if err := os.WriteFile(f.stagingPath(), []byte("half a download"), 0o600); err != nil {
		t.Fatalf("planting a stale staging copy: %v", err)
	}

	f.moveToB()
	f.guard.fail()

	if got := string(f.medium.bytesAt(f.keyB)); got != string(f.content) {
		t.Fatalf("the copy on %q holds %q; a stale staging file was uploaded as the artifact", mediumB, got)
	}
	if leftovers := f.stagingLeftovers(); len(leftovers) != 0 {
		t.Errorf("the staging area holds %v after the move that replaced its contents", leftovers)
	}
}

// TestAGoodStagingCopyIsReusedRatherThanDownloadedAgain is the other half
// of the same rule, and it is the half that pays for itself.
//
// A move interrupted between the download and the upload has already
// spent the egress. Re-downloading on the next cycle spends it again for
// nothing, so a staging file that hashes to what the journal recorded is
// the artifact and is used.
func TestAGoodStagingCopyIsReusedRatherThanDownloadedAgain(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})

	if err := os.MkdirAll(f.stagingDir(), 0o750); err != nil {
		t.Fatalf("creating the staging area: %v", err)
	}
	if err := os.WriteFile(f.stagingPath(), f.content, 0o600); err != nil {
		t.Fatalf("planting a good staging copy: %v", err)
	}

	opensBefore := f.store.opensOf(f.key)
	f.moveToB()
	f.guard.fail()

	if got := f.store.opensOf(f.key) - opensBefore; got != 0 {
		t.Errorf("the engine downloaded the source %d time(s) over a staging copy that already hashed correctly", got)
	}
	if got := string(f.medium.bytesAt(f.keyB)); got != string(f.content) {
		t.Errorf("the copy on %q does not hold the artifact's bytes", mediumB)
	}
}

// TestAnAbandonedStagedMoveLeavesNothingInTheStagingArea is the cleanup
// claim on the path nobody looks at.
//
// A staging area that only empties on the happy path fills up with one
// artifact-sized file per abandoned move, on the backup set's own disk,
// which is the disk the next move's size check is about.
//
// The world is the one where a leftover can actually exist. In a single
// process the copy phase removes its staging file on every exit, so the
// only way one survives is a crash between the download and the upload:
// the file is here, the move row still says COPYING, and the process that
// wrote both is gone. This plants exactly that, and then makes the resumed
// attempt abandon rather than finish, by taking the source out of reach
// the way a bucket lifecycle rule would.
func TestAnAbandonedStagedMoveLeavesNothingInTheStagingArea(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})

	if err := os.MkdirAll(f.stagingDir(), 0o750); err != nil {
		t.Fatalf("creating the staging area: %v", err)
	}
	if err := os.WriteFile(f.stagingPath(), f.content, 0o600); err != nil {
		t.Fatalf("planting the staging copy a crash would leave: %v", err)
	}
	f.medium.archiveRefusesReads = true
	f.a.StorageClass = config.StorageClassGlacier

	f.moveToB()
	f.guard.fail()

	mv := f.moves()[len(f.moves())-1]
	if placement.Phase(mv.Phase) != placement.Abandoned {
		t.Fatalf("the move is at %s, want ABANDONED once the source stopped being readable", mv.Phase)
	}
	if leftovers := f.stagingLeftovers(); len(leftovers) != 0 {
		t.Errorf("the staging area holds %v after an abandoned move", leftovers)
	}
	if !f.medium.has(f.key) {
		t.Error("THE SOURCE OBJECT WAS DELETED for a move that never landed")
	}
}

// TestAFailedUploadKeepsNothingStagedAndKeepsTheSource is the ordinary
// failure, which is not abandoned and must not be: the reason a copy fails
// is usually transient and the next cycle should try again.
//
// What it must not do is leave the staging copy behind while it waits.
func TestAFailedUploadKeepsNothingStagedAndKeepsTheSource(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})
	f.medium.uploadErr = errors.New("the endpoint refused the upload")

	f.moveToB()
	f.guard.fail()

	mv := f.moves()[len(f.moves())-1]
	if placement.Phase(mv.Phase) != placement.Copying {
		t.Fatalf("the move is at %s; a failed copy stays at %s so the next cycle retries it", mv.Phase, placement.Copying)
	}
	if mv.Error == "" {
		t.Error("the move row carries no reason, so an operator reading the move journal has no account of why nothing moved")
	}
	if leftovers := f.stagingLeftovers(); len(leftovers) != 0 {
		t.Errorf("the staging area holds %v after a failed upload; a move waiting to be retried must not hold the artifact's whole size on the backup set's disk while it waits", leftovers)
	}
	if !f.medium.has(f.key) {
		t.Error("THE SOURCE OBJECT WAS DELETED for a move whose upload never landed")
	}
}

// TestAMediumToMediumMoveWithNowhereToStageIsRefusedBeforeAnyRow is the
// one refusal that survives from the old behaviour, and it is the cheap
// one: a deployment whose backup set has no local root has nowhere to put
// the staging copy, and that is decidable from configuration alone.
//
// Refusing at plan time means no move row, no request and no bytes, which
// is the same shape the archive-destination refusal takes.
func TestAMediumToMediumMoveWithNowhereToStageIsRefusedBeforeAnyRow(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})
	movesBefore := len(f.moves())
	f.engine.Sets = rootlessSets{id: f.artifact.Set}

	report := f.moveToB()
	f.guard.fail()

	if report.Refused != 1 {
		t.Fatalf("the cycle reported %d refusal(s), want 1: %+v", report.Refused, report)
	}
	o := report.Outcomes[0]
	if o.Err == nil || !errors.Is(o.Err, placement.ErrNotEligible) {
		t.Errorf("the refusal did not come through ErrNotEligible, so a caller cannot tell it from a storage failure: %v", o.Err)
	}
	for _, want := range []string{"medium-to-medium", "stag"} {
		if !strings.Contains(o.Refused, want) {
			t.Errorf("the refusal does not carry %q: %s", want, o.Refused)
		}
	}
	if got := len(f.moves()); got != movesBefore {
		t.Errorf("the engine wrote %d move row(s) for a plan it refused before planning", got-movesBefore)
	}
}

// rootlessSets is a backup-set resolver whose set has no local_path, which
// is the configuration a staged move cannot run under.
type rootlessSets struct{ id model.BackupSetID }

func (s rootlessSets) Set(id model.BackupSetID) (config.BackupSet, error) {
	if id != s.id {
		return config.BackupSet{}, fmt.Errorf("no backup set %s is configured", id)
	}
	return config.BackupSet{Name: testSet, ID: s.id}, nil
}

// TestTheStagingAreaIsNotTheBackupRootItself is the containment claim,
// stated on its own because it is what keeps every FR-20 proof downstream
// true.
//
// proveLocalSourceSafe accepts a local source only when its canonical
// directory IS the canonical backup-set root, so a staging file directly
// in the root would be one rename away from looking like a managed
// artifact to the one function that authorises deleting one. A
// subdirectory is outside that rule by construction, and it also avoids
// the collision a suffix scheme would have: an artifact may legitimately
// be named "<anything>.staging", because model.NewArtifactID refuses only
// "", ".", "..", a separator, and padding whitespace.
func TestTheStagingAreaIsNotTheBackupRootItself(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})

	if filepath.Dir(f.stagingPath()) == f.localDir {
		t.Fatalf("the staging path %q sits directly in the backup-set root", f.stagingPath())
	}
	if f.stagingPath() == f.localPath() {
		t.Fatalf("the staging path and the artifact's own path are the same file: %q", f.stagingPath())
	}
	if !strings.HasPrefix(placement.StagingDirName, ".") {
		t.Errorf("the staging directory is named %q; a leading dot keeps it out of a casual listing of a backup root", placement.StagingDirName)
	}
}

// TestAStagingAreaThatCannotBeCreatedRefusesTheMove is the one collision a
// subdirectory does leave, named and pinned rather than argued away.
//
// An artifact whose own name is the staging directory's name computes the
// same path, so a backup set holding a local artifact called ".moves" has
// a FILE where the staging area's directory goes. That cannot corrupt
// anything (a directory and a file are not the same entry, so neither can
// be mistaken for the other), and what it must not do is be swallowed: the
// move refuses, the source object is untouched, and the reason is on the
// row.
func TestAStagingAreaThatCannotBeCreatedRefusesTheMove(t *testing.T) {
	f := newTwoMediumFixture(t, fixtureOpts{})

	// A local artifact named exactly like the staging directory. It is
	// a legal artifact name, which is the point.
	if err := os.WriteFile(f.stagingDir(), []byte("an artifact that is named like the staging area"), 0o600); err != nil {
		t.Fatalf("planting a file where the staging area goes: %v", err)
	}

	uploadsBefore := f.medium.uploadCount()
	f.moveToB()
	f.guard.fail()

	if f.medium.uploadCount() != uploadsBefore {
		t.Errorf("the engine uploaded %d time(s) with nowhere to stage", f.medium.uploadCount()-uploadsBefore)
	}
	if !f.medium.has(f.key) {
		t.Fatal("THE SOURCE OBJECT WAS DELETED for a move that could not even make its staging area")
	}
	mv := f.moves()[len(f.moves())-1]
	if mv.Error == "" {
		t.Error("the move row carries no reason, so an operator has no account of why nothing moved")
	}

	// And the file that was in the way is exactly as it was, because the
	// engine has no business deleting something it did not create.
	got, err := os.ReadFile(f.stagingDir())
	if err != nil || !strings.Contains(string(got), "named like the staging area") {
		t.Fatalf("the file where the staging area goes was changed or removed: %q, %v", got, err)
	}
}
