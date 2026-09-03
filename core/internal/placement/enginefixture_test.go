package placement_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file builds the world an engine test runs in: a real SQLite
// journal with a real artifact in COMPLETE, a real local backup root with
// real bytes in it, and a medium store double that can be told to
// misbehave in each of the specific ways a real one can.
//
// # The invariant guard, and why it wraps what it wraps
//
// FR-30's standing invariant is "at every instant, at least one ACTIVE
// placement at read-back class or better", and the acceptance criterion
// asks for it to be asserted continuously rather than sampled. A polling
// goroutine is sampling however fast it polls, so this does something
// else.
//
// The invariant is a property that only an EVENT can falsify: time passing
// does not remove a copy. There are exactly two events in this engine that
// remove one, the source delete and the destination delete, and both go
// through the seams below. So the guard sits on those two calls and
// re-reads the DURABLE journal before either of them, plus on every phase
// write, which is the only other way the journal's own account of the
// copies changes. Checking every event that can falsify a property is a
// complete check over the whole run, which is a stronger claim than any
// polling frequency and is the one I can actually defend.
//
// A violation is recorded AND the delete is refused, so a mutation that
// breaks the ordering fails the assertion rather than destroying the
// fixture's own evidence.

const (
	testSource = "production"
	testSet    = "postgres-primary"
	testMedium = "offsite_s3"
)

var testNow2 = time.Date(2026, 9, 2, 9, 30, 0, 0, time.UTC)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- the medium store double ------------------------------------------

// fakeMedium is a MediumStore that keeps objects in memory and can be told
// to lie in each of the ways a real endpoint can: store the wrong bytes,
// refuse to attest, fail to answer a stat, or fail an upload outright.
type fakeMedium struct {
	mu      sync.Mutex
	objects map[string][]byte

	// corrupt, when set, is what UploadFromLocal actually stores, whatever
	// it was handed. This is the hostile destination: an endpoint that
	// accepts an upload and keeps something else.
	corrupt []byte

	// truncate, when > 0, stores only that many bytes of the upload.
	truncate int

	// attests reports whether ObjectChecksum can produce a full-object
	// SHA-256. Against the rclone this product embeds, an s3 medium never
	// can, so false is the realistic default for s3.
	attests bool

	uploadErr error
	statErr   error
	openErr   error
	deleteErr error

	uploads, opens, stats, deletes int
	uploadedKeys                   []string
}

func newFakeMedium() *fakeMedium {
	return &fakeMedium{objects: map[string][]byte{}}
}

func (f *fakeMedium) StatObject(_ context.Context, _ transport.Medium, key string) (transport.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats++
	if f.statErr != nil {
		return transport.ObjectInfo{}, f.statErr
	}
	b, ok := f.objects[key]
	if !ok {
		return transport.ObjectInfo{}, &transport.Error{Category: transport.NotFound, Op: "stat", Cause: errors.New("no such key")}
	}
	return transport.ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (f *fakeMedium) UploadFromLocal(_ context.Context, _ transport.Medium, localPath, key string, _ transport.UploadOptions) (transport.UploadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads++
	f.uploadedKeys = append(f.uploadedKeys, key)
	if f.uploadErr != nil {
		return transport.UploadResult{}, f.uploadErr
	}
	b, err := os.ReadFile(localPath)
	if err != nil {
		return transport.UploadResult{}, err
	}
	switch {
	case f.corrupt != nil:
		b = append([]byte(nil), f.corrupt...)
	case f.truncate > 0 && f.truncate < len(b):
		b = b[:f.truncate]
	}
	f.objects[key] = b
	return transport.UploadResult{Key: key, BytesUploaded: int64(len(b))}, nil
}

func (f *fakeMedium) OpenObject(_ context.Context, _ transport.Medium, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if f.openErr != nil {
		return nil, f.openErr
	}
	b, ok := f.objects[key]
	if !ok {
		return nil, &transport.Error{Category: transport.NotFound, Op: "open", Cause: errors.New("no such key")}
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

func (f *fakeMedium) ObjectChecksum(_ context.Context, _ transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.attests {
		// Exactly what rclone v1.75.0's s3 backend forces: it exposes MD5
		// from the ETag and refuses every other algorithm, so no S3
		// endpoint reachable through this build can attest a full-object
		// SHA-256.
		return transport.ChecksumAttestation{}, &transport.Error{
			Category: transport.UnsupportedCapability, Op: "checksum",
			Cause: fmt.Errorf("this backend cannot attest a full-object %s", alg),
		}
	}
	b, ok := f.objects[key]
	if !ok {
		return transport.ChecksumAttestation{}, &transport.Error{Category: transport.NotFound, Op: "checksum", Cause: errors.New("no such key")}
	}
	return transport.ChecksumAttestation{Algorithm: alg, Value: sha256Hex(b)}, nil
}

func (f *fakeMedium) DeleteObject(_ context.Context, _ transport.Medium, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.objects, key)
	return nil
}

func (f *fakeMedium) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeMedium) bytesAt(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.objects[key]...)
}

func (f *fakeMedium) keyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objects)
}

// --- resolvers --------------------------------------------------------

type fixedMediums struct {
	medium transport.Medium
	class  placement.Class
	err    error
}

func (m fixedMediums) Resolve(id string) (transport.Medium, placement.Class, error) {
	if m.err != nil {
		return transport.Medium{}, "", m.err
	}
	if id != m.medium.ID {
		return transport.Medium{}, "", fmt.Errorf("no medium %q is configured", id)
	}
	return m.medium, m.class, nil
}

type fixedSets struct{ set config.BackupSet }

func (s fixedSets) Set(id model.BackupSetID) (config.BackupSet, error) {
	if id != s.set.ID {
		return config.BackupSet{}, fmt.Errorf("no backup set %s is configured", id)
	}
	return s.set, nil
}

// tierGuard answers FR-30's last pre-delete question with whatever the
// test told it to say.
type tierGuard struct {
	selected bool
	why      string
	err      error
	asked    int
}

func (g *tierGuard) SourceStillSelected(_ context.Context, _ state.Record, _ string) (bool, string, error) {
	g.asked++
	return g.selected, g.why, g.err
}

// --- the invariant guard ----------------------------------------------

// guard wraps the journal, the medium store and the local store, and
// asserts FR-30's invariant at every event that could break it. See this
// file's own comment for why those events are the complete set.
type guard struct {
	t          *testing.T
	journal    *state.Journal
	artifact   model.ArtifactID
	sufficient []placement.Class

	mu         sync.Mutex
	violations []string
	deletes    []string
}

func (g *guard) check(what string) error {
	return g.checkSurviving(what, "")
}

// checkSurviving asserts FR-30's invariant, and, when a locator is given,
// asserts it will STILL hold once the copy at that locator is gone.
//
// The second half is the one that matters and it was added after a planted
// mutation walked past the first. A guard that only re-reads the journal
// cannot see a delete nobody journaled: at the instant an early, unrecorded
// delete happens, the journal still says the copy it is about to destroy is
// ACTIVE and content-verified, so the invariant "holds" and the guard waves
// it through. Requiring a surviving copy that is NOT the one being deleted
// is what closes that, and it is also the honest reading of the invariant:
// the point was never that a row exists, it was that a copy does.
func (g *guard) checkSurviving(what, locator string) error {
	rec, err := g.journal.Get(context.Background(), g.artifact)
	if err != nil {
		return fmt.Errorf("reading the journal to check the invariant before %s: %w", what, err)
	}
	if err := placement.CheckInvariant(rec, g.sufficient...); err != nil {
		return g.violation(what, err)
	}
	if locator == "" {
		return nil
	}
	surviving := rec
	surviving.Placements = nil
	for _, p := range rec.Placements {
		if samePlace(p.Location, locator) {
			continue
		}
		surviving.Placements = append(surviving.Placements, p)
	}
	if err := placement.CheckInvariant(surviving, g.sufficient...); err != nil {
		return g.violation(what, fmt.Errorf("once the copy at %q is gone, %w", locator, err))
	}
	return nil
}

func (g *guard) violation(what string, err error) error {
	g.mu.Lock()
	g.violations = append(g.violations, fmt.Sprintf("%s: %v", what, err))
	g.mu.Unlock()
	return fmt.Errorf("the standing invariant does not hold, so %s is refused: %w", what, err)
}

// beforeDelete is the guard on the two destructive calls: the durable
// journal has to say another good copy, one that is not this one, exists
// before either of them may proceed.
func (g *guard) beforeDelete(what, locator string) error {
	g.mu.Lock()
	g.deletes = append(g.deletes, what+":"+locator)
	g.mu.Unlock()
	return g.checkSurviving(what+" "+locator, locator)
}

func (g *guard) fail() {
	g.t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, v := range g.violations {
		g.t.Errorf("FR-30's standing invariant was broken: %s", v)
	}
}

func (g *guard) deleteLog() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.deletes...)
}

// guardedJournal asserts the invariant after every durable phase write,
// which is the only way the journal's own account of the copies changes.
type guardedJournal struct {
	*state.Journal
	guard *guard

	mu       sync.Mutex
	advances []string
}

func (j *guardedJournal) AdvanceMove(ctx context.Context, a state.MoveAdvance) (state.Move, error) {
	mv, err := j.Journal.AdvanceMove(ctx, a)
	if err != nil {
		return mv, err
	}
	j.mu.Lock()
	j.advances = append(j.advances, a.From+"->"+a.To)
	j.mu.Unlock()
	if cerr := j.guard.check("the phase write " + a.From + " -> " + a.To); cerr != nil {
		return mv, nil
	}
	return mv, nil
}

func (j *guardedJournal) phaseWrites() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.advances...)
}

// guardedMedium refuses a destination delete the invariant does not
// authorise.
type guardedMedium struct {
	*fakeMedium
	guard *guard
}

func (m *guardedMedium) DeleteObject(ctx context.Context, medium transport.Medium, key string) error {
	if err := m.guard.beforeDelete("deleting the destination object", key); err != nil {
		return err
	}
	return m.fakeMedium.DeleteObject(ctx, medium, key)
}

// guardedLocal refuses a local delete the invariant does not authorise.
// This is the one that catches a source delete issued too early.
type guardedLocal struct {
	artifactstore.Local
	guard *guard
}

func (l guardedLocal) Remove(ctx context.Context, locator string) error {
	if err := l.guard.beforeDelete("removing the local copy", locator); err != nil {
		return err
	}
	return l.Local.Remove(ctx, locator)
}

// --- the fixture -------------------------------------------------------

type fixture struct {
	t        *testing.T
	ctx      context.Context
	journal  *state.Journal
	guarded  *guardedJournal
	guard    *guard
	medium   *fakeMedium
	engine   *placement.Engine
	tiers    *tierGuard
	sets     fixedSets
	artifact model.ArtifactID
	content  []byte
	hash     string
	localDir string
	root     string
	key      string
	clock    time.Time
}

type fixtureOpts struct {
	// class is what a move to the medium has to achieve. Empty means
	// Content, which is upload_verification: readback.
	class placement.Class
	// attests tells the medium store whether it can produce a full-object
	// SHA-256 attestation.
	attests bool
	// artifactState is the lifecycle state the seeded artifact ends in.
	// Empty means COMPLETE.
	artifactState string
	// noPlacement seeds the artifact with no placement row at all.
	noPlacement bool
	// content overrides the artifact's bytes.
	content []byte
	// artifactName overrides the artifact's name.
	artifactName string
}

func newFixture(t *testing.T, opts fixtureOpts) *fixture {
	t.Helper()
	ctx := context.Background()

	root := t.TempDir()
	localDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(localDir, 0o750); err != nil {
		t.Fatalf("creating the backup root: %v", err)
	}

	journal, err := state.Open(ctx, filepath.Join(root, "journal.db"))
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	t.Cleanup(func() { _ = journal.Close() })

	name := opts.artifactName
	if name == "" {
		name = "2026-09-01T00-00-00Z.dump"
	}
	set := model.BackupSetID{Source: testSource, Set: testSet}
	artifact := model.ArtifactID{Set: set, Name: name}

	content := opts.content
	if content == nil {
		content = []byte("the durable bytes of one backup artifact, twice over, so a truncation is visible")
	}
	localPath := filepath.Join(localDir, name)
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatalf("writing the local artifact: %v", err)
	}

	targetState := opts.artifactState
	if targetState == "" {
		targetState = "COMPLETE"
	}
	seedArtifact(t, journal, artifact, localPath, content, targetState, !opts.noPlacement)

	class := opts.class
	if class == "" {
		class = placement.Content
	}
	g := &guard{t: t, journal: journal, artifact: artifact}
	if class != placement.Content {
		g.sufficient = []placement.Class{placement.Content, class}
	}

	medium := newFakeMedium()
	medium.attests = opts.attests

	bs := config.BackupSet{Name: testSet, ID: set, LocalPath: localDir}
	sets := fixedSets{set: bs}
	tiers := &tierGuard{}

	local, err := artifactstore.NewLocal(localDir)
	if err != nil {
		t.Fatalf("building the local store: %v", err)
	}

	guarded := &guardedJournal{Journal: journal, guard: g}
	clock := testNow2

	f := &fixture{
		t: t, ctx: ctx, journal: journal, guarded: guarded, guard: g,
		medium: medium, tiers: tiers, sets: sets,
		artifact: artifact, content: content, hash: sha256Hex(content),
		localDir: localDir, root: root, clock: clock,
	}
	f.key, err = transport.MediumKey("rclone-manager", artifact)
	if err != nil {
		t.Fatalf("computing the destination key: %v", err)
	}

	f.engine = &placement.Engine{
		Journal: guarded,
		Store:   &guardedMedium{fakeMedium: medium, guard: g},
		Local:   guardedLocal{Local: local, guard: g},
		Mediums: fixedMediums{
			medium: transport.Medium{ID: testMedium, Type: transport.MediumTypeS3, Bucket: "nas-backups", Prefix: "rclone-manager"},
			class:  class,
		},
		Sets:             sets,
		Tiers:            tiers,
		Now:              func() time.Time { f.clock = f.clock.Add(time.Second); return f.clock },
		MaxMovesPerCycle: 4,
	}
	return f
}

func (f *fixture) localPath() string { return filepath.Join(f.localDir, f.artifact.Name) }

func (f *fixture) localExists() bool {
	_, err := os.Lstat(f.localPath())
	return err == nil
}

func (f *fixture) record() state.Record {
	f.t.Helper()
	rec, err := f.journal.Get(f.ctx, f.artifact)
	if err != nil {
		f.t.Fatalf("reading the journal: %v", err)
	}
	return rec
}

func (f *fixture) moves() []state.Move {
	f.t.Helper()
	moves, err := f.journal.MovesForArtifact(f.ctx, f.artifact)
	if err != nil {
		f.t.Fatalf("reading the move journal: %v", err)
	}
	return moves
}

func (f *fixture) onlyMove() state.Move {
	f.t.Helper()
	moves := f.moves()
	if len(moves) != 1 {
		f.t.Fatalf("expected exactly one move for %s, got %d", f.artifact, len(moves))
	}
	return moves[0]
}

func (f *fixture) placement(medium string) (state.Placement, bool) {
	for _, p := range f.record().Placements {
		if p.Medium == medium {
			return p, true
		}
	}
	return state.Placement{}, false
}

func (f *fixture) runCycle() placement.CycleReport {
	f.t.Helper()
	report, err := f.engine.RunCycle(f.ctx, []placement.Plan{{Artifact: f.artifact, DestinationMedium: testMedium}})
	if err != nil {
		f.t.Fatalf("RunCycle: %v", err)
	}
	return report
}

// seedArtifact walks a real artifact from DISCOVERED to targetState
// through the real journal, recording the local placement at COMMITTED
// exactly where lifecycle.Commit records it.
func seedArtifact(t *testing.T, j *state.Journal, artifact model.ArtifactID, localPath string, content []byte, targetState string, withPlacement bool) {
	t.Helper()
	ctx := context.Background()
	now := testNow2.Add(-time.Hour)
	size := int64(len(content))
	hash := sha256Hex(content)

	step := func(from, to string, mutate func(*state.Transition)) {
		t.Helper()
		now = now.Add(time.Minute)
		tr := state.Transition{
			Artifact: artifact, Key: fmt.Sprintf("%s|%s|seed", artifact, to),
			From: from, To: to, OccurredAt: now,
		}
		if from == "" {
			tr.RemotePath = "/srv/" + artifact.Name
			tr.Remote = &state.RemoteIdentity{Size: &size, Hash: hash, HashAlg: "sha256", BackendID: "sftp"}
		}
		if mutate != nil {
			mutate(&tr)
		}
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("seeding %s -> %s: %v", from, to, err)
		}
	}

	path := localPath
	partial := localPath + ".partial"

	step("", "DISCOVERED", nil)
	if targetState == "DISCOVERED" {
		return
	}
	step("DISCOVERED", "TRANSFERRING", func(tr *state.Transition) { tr.LocalPath = &partial })
	step("TRANSFERRING", "TRANSFERRED", func(tr *state.Transition) {
		tr.Transfer = &state.TransferResult{BytesTransferred: size, Checksummed: true}
	})
	step("TRANSFERRED", "VERIFYING", nil)
	step("VERIFYING", "VERIFIED", func(tr *state.Transition) {
		tr.Hashes = &state.HashUpdate{Hash: hash, Alg: "sha256"}
		tr.Validation = &state.ValidationUpdate{Passed: true, Detail: "seeded"}
	})
	step("VERIFIED", "COMMITTING", nil)
	step("COMMITTING", "COMMITTED", func(tr *state.Transition) {
		tr.LocalPath = &path
		if withPlacement {
			verified := now
			tr.Placement = &state.PlacementUpdate{
				Medium: state.MediumLocal, Location: path, Size: &size,
				Hash: hash, HashAlg: "sha256",
				VerificationClass: state.VerificationContent, VerifiedAt: &verified,
			}
		}
	})
	if targetState == "COMMITTED" {
		return
	}
	step("COMMITTED", "REMOTE_DELETE_PENDING", nil)
	if targetState == "REMOTE_DELETE_PENDING" {
		return
	}
	step("REMOTE_DELETE_PENDING", "COMPLETE", nil)
	if targetState != "COMPLETE" {
		t.Fatalf("seedArtifact does not know how to reach %q", targetState)
	}
}

// samePlace reports whether two locators name the same copy. It resolves
// both before comparing because the journal records the path the config
// computes and the guard is handed the path the FR-20 proof resolved, and
// on darwin those differ (/var is a symlink to /private/var). A filter that
// missed the match would leave the copy being deleted in the surviving set
// and quietly weaken this whole check.
func samePlace(a, b string) bool {
	if a == b {
		return true
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}
