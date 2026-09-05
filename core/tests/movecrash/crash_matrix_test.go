// Package movecrash_test drives FR-30's move engine against a real,
// disposable subprocess (tests/movecrash/harness), kills it with a real,
// uncatchable SIGKILL at each of the move's phase boundaries, and then
// restarts the engine in this test process against the same on-disk
// journal and filesystem the crashed process left behind.
//
// # What each cell asserts
//
// Three things, every time:
//
//  1. At the instant of the crash, FR-30's standing invariant held: the
//     artifact has at least one ACTIVE placement at content class, read
//     from the journal the dead process left on disk.
//  2. The restart converges. The move reaches DONE or ABANDONED, from one
//     RunCycle, without the test telling it where it was.
//  3. The bytes are real. Whatever the surviving ACTIVE placement points
//     at is read and hashed, and it has to be the artifact.
//
// # Why the invariant is not sampled
//
// The invariant can only be falsified by an event: time passing does not
// remove a copy of anything. There are exactly two events in this engine
// that remove one, the source delete and the destination delete, and both
// run through a guard (in the harness for the crashed half of a cell, and
// in this process for the restarted half) that re-reads the durable
// journal immediately before the call and refuses if the invariant does
// not hold. Guarding every event that can falsify a property is a complete
// check over the whole run, which is a stronger claim than any polling
// interval.
//
// # Why the harness cannot spell a phase
//
// See TestTheCrashHarnessHasNoPhaseMachineOfItsOwn, and harness/main.go's
// own package doc, for #372 and why this suite is built the way it is.
package movecrash_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

const (
	crashSource   = "production"
	crashSet      = "postgres-primary"
	crashMedium   = "offsite_local"
	crashArtifact = "2026-09-02T00-00-00Z.dump"

	// crashSecondMedium is the annual rung of a two-medium chain, and it
	// is what makes a hop whose two ends are both mediums drivable here
	// (#429). Nothing in the original eleven cells names it; it is a
	// second directory standing in for a second bucket, on the same
	// backend, exactly as crashMedium is.
	crashSecondMedium = "annual_local"
)

var crashNow = time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)

// --- building and running the harness ---------------------------------

var (
	harnessOnce   sync.Once
	harnessBinary string
	harnessErr    error
)

func buildHarness(t *testing.T) string {
	t.Helper()
	harnessOnce.Do(func() {
		dir, err := os.MkdirTemp("", "movecrash-harness-bin")
		if err != nil {
			harnessErr = err
			return
		}
		bin := filepath.Join(dir, "movecrash-harness")
		out, err := exec.Command("go", "build", "-o", bin, "./harness").CombinedOutput()
		if err != nil {
			harnessErr = fmt.Errorf("building tests/movecrash/harness: %v\n%s", err, out)
			return
		}
		harnessBinary = bin
	})
	if harnessErr != nil {
		t.Fatalf("%v", harnessErr)
	}
	return harnessBinary
}

type harnessResult struct {
	stdout, stderr string
	signal         syscall.Signal
	err            error
}

func (r harnessResult) killed() bool { return r.signal == syscall.SIGKILL }

func (r harnessResult) violations() []string {
	var out []string
	for _, line := range strings.Split(r.stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "VIOLATION "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// --- the world one cell runs in ---------------------------------------

type world struct {
	t        *testing.T
	ctx      context.Context
	dir      string
	root     string
	bucket   string
	journal  string
	artifact model.ArtifactID
	content  []byte
	hash     string
	key      string

	// store is the restarting process's own medium store, kept so a cell
	// can ask it what it actually read.
	store *guardedStore

	// secondBucket is crashSecondMedium's directory. Both mediums write
	// with no prefix, so FR-28 gives the artifact the SAME key on both,
	// and every lookup below is by medium id rather than by key. That is
	// the collision the conformance watcher was keyed wrongly against,
	// and it is worth having here too: a helper that told the two copies
	// apart by locator would be telling this suite the same lie.
	secondBucket string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "backups")
	bucket := filepath.Join(dir, "bucket")
	secondBucket := filepath.Join(dir, "second-bucket")
	for _, d := range []string{root, bucket, secondBucket} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	content := []byte(strings.Repeat("the durable bytes of one backup artifact. ", 64))
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	set := model.BackupSetID{Source: crashSource, Set: crashSet}
	artifact := model.ArtifactID{Set: set, Name: crashArtifact}
	if err := os.WriteFile(filepath.Join(root, crashArtifact), content, 0o600); err != nil {
		t.Fatalf("writing the artifact: %v", err)
	}

	w := &world{
		t: t, ctx: context.Background(), dir: dir, root: root, bucket: bucket,
		secondBucket: secondBucket,
		journal:      filepath.Join(dir, "journal.db"), artifact: artifact,
		content: content, hash: hash,
	}
	key, err := transport.MediumKey("", artifact)
	if err != nil {
		t.Fatalf("computing the key: %v", err)
	}
	w.key = key

	seed(t, w)
	return w
}

// seed walks a real artifact to COMPLETE through the real journal, with
// the local placement recorded at COMMITTED exactly where lifecycle.Commit
// records it.
func seed(t *testing.T, w *world) {
	t.Helper()
	j, err := state.Open(w.ctx, w.journal)
	if err != nil {
		t.Fatalf("opening the journal to seed: %v", err)
	}
	defer func() { _ = j.Close() }()

	now := crashNow
	size := int64(len(w.content))
	path := filepath.Join(w.root, crashArtifact)
	partial := path + ".partial"

	step := func(from, to string, mutate func(*state.Transition)) {
		t.Helper()
		now = now.Add(time.Minute)
		tr := state.Transition{
			Artifact: w.artifact, Key: fmt.Sprintf("seed|%s", to),
			From: from, To: to, OccurredAt: now,
		}
		if from == "" {
			tr.RemotePath = "/srv/" + crashArtifact
			tr.Remote = &state.RemoteIdentity{Size: &size, Hash: w.hash, HashAlg: "sha256", BackendID: "sftp"}
		}
		if mutate != nil {
			mutate(&tr)
		}
		if _, err := j.RecordTransition(w.ctx, tr); err != nil {
			t.Fatalf("seeding %s -> %s: %v", from, to, err)
		}
	}

	step("", "DISCOVERED", nil)
	step("DISCOVERED", "TRANSFERRING", func(tr *state.Transition) { tr.LocalPath = &partial })
	step("TRANSFERRING", "TRANSFERRED", func(tr *state.Transition) {
		tr.Transfer = &state.TransferResult{BytesTransferred: size, Checksummed: true}
	})
	step("TRANSFERRED", "VERIFYING", nil)
	step("VERIFYING", "VERIFIED", func(tr *state.Transition) {
		tr.Hashes = &state.HashUpdate{Hash: w.hash, Alg: "sha256"}
		tr.Validation = &state.ValidationUpdate{Passed: true, Detail: "seeded"}
	})
	step("VERIFIED", "COMMITTING", nil)
	step("COMMITTING", "COMMITTED", func(tr *state.Transition) {
		tr.LocalPath = &path
		verified := now
		tr.Placement = &state.PlacementUpdate{
			Medium: state.MediumLocal, Location: path, Size: &size,
			Hash: w.hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, VerifiedAt: &verified,
		}
	})
	step("COMMITTED", "REMOTE_DELETE_PENDING", nil)
	step("REMOTE_DELETE_PENDING", "COMPLETE", nil)
}

// crash runs the harness with the given kill plan and reports what
// happened to the process.
func (w *world) crash(extra ...string) harnessResult {
	w.t.Helper()
	args := append([]string{
		"-journal", w.journal, "-root", w.root, "-bucket", w.bucket,
		"-medium", crashMedium, "-source", crashSource, "-set", crashSet,
		"-artifact", crashArtifact, "-destination", crashMedium,
		"-second-medium", crashSecondMedium, "-second-bucket", w.secondBucket,
	}, extra...)

	return w.runHarness(args)
}

// crashTo is crash with the hop's destination named, for a cell whose move
// does not end on the first medium.
func (w *world) crashTo(destination string, extra ...string) harnessResult {
	w.t.Helper()
	args := append([]string{
		"-journal", w.journal, "-root", w.root, "-bucket", w.bucket,
		"-medium", crashMedium, "-source", crashSource, "-set", crashSet,
		"-artifact", crashArtifact, "-destination", destination,
		"-second-medium", crashSecondMedium, "-second-bucket", w.secondBucket,
	}, extra...)

	return w.runHarness(args)
}

// firstHop runs the harness with no kill plan at all, to put the artifact
// on the first medium for real before a cell crashes the hop to the
// second one.
//
// It is the product's own move rather than a seeded placement row, for the
// reason every fixture in this repository gives for the same choice:
// seeding the state a cell is about is seeding the premise, and a placement
// row a test wrote is not evidence that the engine can produce one.
func (w *world) firstHop() {
	w.t.Helper()
	res := w.crashTo(crashMedium, "-plan")
	if res.killed() {
		w.t.Fatalf("the uninterrupted first hop was killed:\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "FINISHED") {
		w.t.Fatalf("the first hop did not finish:\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	}
	if w.localExists() {
		w.t.Fatalf("the local copy survived the first hop, so the hop a staged cell crashes is not medium-to-medium")
	}
}

func (w *world) runHarness(args []string) harnessResult {
	w.t.Helper()

	cmd := exec.Command(buildHarness(w.t), args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := harnessResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.signal = ws.Signal()
		}
	}
	return res
}

// journalNow opens the journal the crashed process left behind.
func (w *world) journalNow() *state.Journal {
	w.t.Helper()
	j, err := state.Open(w.ctx, w.journal)
	if err != nil {
		w.t.Fatalf("reopening the journal after the crash: %v", err)
	}
	w.t.Cleanup(func() { _ = j.Close() })
	return j
}

// restartEngine builds the engine this process resumes with. It is the
// same Engine the harness builds, with the same one entry point, and that
// is the whole design: resume is not a separate path.
func (w *world) restartEngine(j *state.Journal, corruptOnUpload bool) (*placement.Engine, *restartGuard) {
	w.t.Helper()
	local, err := artifactstore.NewLocal(w.root)
	if err != nil {
		w.t.Fatalf("building the local store: %v", err)
	}
	g := &restartGuard{t: w.t, journal: j, artifact: w.artifact}
	w.store = &guardedStore{
		MediumStore: rclone.New(), guard: g, corruptOnUpload: corruptOnUpload,
		bucketOf: w.bucketOf, opens: map[string]int{},
	}
	return &placement.Engine{
		Journal: j,
		Store:   w.store,
		Local:   &guardedLocal{Local: local, guard: g},
		Mediums: crashResolver{mediums: w.mediums()},
		Sets: crashSets{set: config.BackupSet{
			Name: crashSet, ID: w.artifact.Set, LocalPath: w.root,
		}},
		Tiers:            noTier{},
		MaxMovesPerCycle: 4,
	}, g
}

// mediums is both configured destinations, as the engine resolves them.
func (w *world) mediums() []transport.Medium {
	return []transport.Medium{
		{ID: crashMedium, Type: transport.MediumTypeLocalDir, Bucket: w.bucket},
		{ID: crashSecondMedium, Type: transport.MediumTypeLocalDir, Bucket: w.secondBucket},
	}
}

// bucketOf is which directory a medium's objects are in. Every lookup that
// reaches for bytes on a medium goes through this rather than through the
// key, because both mediums give one artifact the same key. See world's
// own comment.
func (w *world) bucketOf(medium string) string {
	switch medium {
	case crashMedium:
		return w.bucket
	case crashSecondMedium:
		return w.secondBucket
	}
	w.t.Fatalf("no bucket is configured for medium %q", medium)
	return ""
}

// stagingLeftovers is everything still sitting in the staging area a hop
// between two mediums writes through, temporary files included.
//
// The temporary files are the point of listing rather than statting one
// path. artifactstore.Local.Put writes through a temp file in the same
// directory and links it into place, so a process killed part-way through
// the read leaves a temp there and no file at the staging name at all. A
// check that only looked at the staging name would call that clean.
func (w *world) stagingLeftovers() []string {
	w.t.Helper()
	entries, err := os.ReadDir(filepath.Join(w.root, placement.StagingDirName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		w.t.Fatalf("reading the staging area: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// checkInvariantNow reads the journal exactly as it stands and asserts
// FR-30's standing invariant. It is what the crashed half of every cell is
// judged by.
func (w *world) checkInvariantNow(j *state.Journal, when string) {
	w.t.Helper()
	rec, err := j.Get(w.ctx, w.artifact)
	if err != nil {
		w.t.Fatalf("reading the journal %s: %v", when, err)
	}
	if err := placement.CheckInvariant(rec); err != nil {
		w.t.Fatalf("FR-30's standing invariant did not hold %s: %v", when, err)
	}
}

// assertConverged is the after-restart assertion: a terminal move, a good
// copy that really hashes right, and no orphan.
func (w *world) assertConverged(j *state.Journal, wantPhase placement.Phase) state.Record {
	w.t.Helper()
	moves, err := j.MovesForArtifact(w.ctx, w.artifact)
	if err != nil {
		w.t.Fatalf("reading the move journal: %v", err)
	}
	if len(moves) != 1 {
		w.t.Fatalf("expected exactly one move, got %d", len(moves))
	}
	mv := moves[0]
	if placement.Phase(mv.Phase) != wantPhase {
		w.t.Fatalf("the move converged to %s, want %s (error: %q)", mv.Phase, wantPhase, mv.Error)
	}

	rec, err := j.Get(w.ctx, w.artifact)
	if err != nil {
		w.t.Fatalf("reading the artifact: %v", err)
	}
	if err := placement.CheckInvariant(rec); err != nil {
		w.t.Fatalf("FR-30's standing invariant did not hold after the restart: %v", err)
	}

	// And the copy the journal is relying on has to be real: read the
	// bytes wherever it says they are and hash them.
	var checked int
	for _, p := range rec.Placements {
		if p.Status != state.PlacementActive {
			continue
		}
		checked++
		got := w.readPlacement(p)
		if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != w.hash {
			w.t.Fatalf("the ACTIVE placement on %q does not hold the artifact's bytes", p.Medium)
		}
	}
	if checked == 0 {
		w.t.Fatal("no ACTIVE placement survived, so the artifact has no copy at all")
	}
	return rec
}

// assertStagedConverged is assertConverged for a cell whose world has
// already completed one move.
//
// It reads the LAST move rather than requiring exactly one, because the
// hop under test is the second: the first put the artifact on a medium and
// is finished. Requiring one row would have made every staged cell fail on
// the fixture rather than on the engine, and relaxing the original
// assertion instead would have weakened the eleven cells that legitimately
// only ever have one.
func (w *world) assertStagedConverged(j *state.Journal, wantPhase placement.Phase) state.Record {
	w.t.Helper()
	moves, err := j.MovesForArtifact(w.ctx, w.artifact)
	if err != nil {
		w.t.Fatalf("reading the move journal: %v", err)
	}
	if len(moves) != 2 {
		w.t.Fatalf("expected two moves (the hop onto the first medium, and the staged hop this cell crashed), got %d: %+v", len(moves), moves)
	}
	first, staged := moves[0], moves[1]
	if first.DestinationMedium != crashMedium || placement.Phase(first.Phase) != placement.Done {
		first, staged = staged, first
	}
	if first.DestinationMedium != crashMedium || placement.Phase(first.Phase) != placement.Done {
		w.t.Fatalf("neither move is the completed hop onto %q, so this cell is not looking at a staged hop: %+v", crashMedium, moves)
	}
	if staged.SourceMedium != crashMedium || staged.DestinationMedium != crashSecondMedium {
		w.t.Fatalf("the hop under test runs %q -> %q, which is not medium-to-medium", staged.SourceMedium, staged.DestinationMedium)
	}
	if got := placement.Phase(staged.Phase); got != wantPhase {
		w.t.Fatalf("the staged hop converged to %s, want %s (error: %q)", got, wantPhase, staged.Error)
	}

	rec, err := j.Get(w.ctx, w.artifact)
	if err != nil {
		w.t.Fatalf("reading the artifact: %v", err)
	}
	if err := placement.CheckInvariant(rec); err != nil {
		w.t.Fatalf("FR-30's standing invariant did not hold after the restart: %v", err)
	}
	var checked int
	for _, p := range rec.Placements {
		if p.Status != state.PlacementActive {
			continue
		}
		checked++
		got := w.readPlacement(p)
		if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != w.hash {
			w.t.Fatalf("the ACTIVE placement on %q does not hold the artifact's bytes", p.Medium)
		}
	}
	if checked == 0 {
		w.t.Fatal("no ACTIVE placement survived, so the artifact has no copy at all")
	}
	return rec
}

func (w *world) readPlacement(p state.Placement) []byte {
	w.t.Helper()
	if p.Medium == state.MediumLocal {
		b, err := os.ReadFile(p.Location)
		if err != nil {
			w.t.Fatalf("reading the local copy at %q: %v", p.Location, err)
		}
		return b
	}
	b, err := os.ReadFile(filepath.Join(w.bucketOf(p.Medium), filepath.FromSlash(p.Location)))
	if err != nil {
		w.t.Fatalf("reading %s's copy at %q: %v", p.Medium, p.Location, err)
	}
	return b
}

func (w *world) localPath() string { return filepath.Join(w.root, crashArtifact) }

func (w *world) localExists() bool {
	_, err := os.Lstat(w.localPath())
	return err == nil
}

// --- the guards this process's own restart runs under ------------------

type restartGuard struct {
	t          *testing.T
	journal    *state.Journal
	artifact   model.ArtifactID
	violations []string
}

// before is the harness's own guard, in the restarting process. A copy is
// a MEDIUM and a locator; see the harness's copy of this function for why
// the locator alone will not do, and what it cost when it was.
func (g *restartGuard) before(what, medium, locator string) error {
	rec, err := g.journal.Get(context.Background(), g.artifact)
	if err != nil {
		return err
	}
	if err := placement.CheckInvariant(rec); err != nil {
		return g.violation(what, err)
	}
	surviving := rec
	surviving.Placements = nil
	for _, p := range rec.Placements {
		if p.Medium == medium && samePlace(p.Location, locator) {
			continue
		}
		surviving.Placements = append(surviving.Placements, p)
	}
	if err := placement.CheckInvariant(surviving); err != nil {
		return g.violation(what, fmt.Errorf("once the copy at %q on %q is gone, %w", locator, medium, err))
	}
	return nil
}

func (g *restartGuard) violation(what string, err error) error {
	g.violations = append(g.violations, fmt.Sprintf("%s: %v", what, err))
	return fmt.Errorf("the standing invariant does not hold, so %s is refused: %w", what, err)
}

func (g *restartGuard) fail() {
	g.t.Helper()
	for _, v := range g.violations {
		g.t.Errorf("FR-30's standing invariant was broken during the restart: %s", v)
	}
}

type guardedStore struct {
	transport.MediumStore
	guard *restartGuard

	// corruptOnUpload makes this store keep something other than what it
	// was handed, every time, which is the persistently hostile endpoint
	// FR-31's trust discussion is about. One bad upload is recoverable by
	// copying again; an endpoint that is always wrong is what has to end
	// with the source still where it was.
	corruptOnUpload bool
	bucketOf        func(string) string

	// opens counts reads, keyed by MEDIUM and key rather than by key
	// alone. Both mediums give one artifact the same key, and a staged
	// hop reads the source to stage it and the destination to verify it,
	// so a counter that could not tell those apart could not answer the
	// one question the reuse cell asks.
	opens map[string]int
}

func (s *guardedStore) UploadFromLocal(ctx context.Context, medium transport.Medium, localPath, key string, opts transport.UploadOptions) (transport.UploadResult, error) {
	res, err := s.MediumStore.UploadFromLocal(ctx, medium, localPath, key, opts)
	if err != nil || !s.corruptOnUpload {
		return res, err
	}
	target := filepath.Join(s.bucketOf(medium.ID), filepath.FromSlash(key))
	if werr := os.WriteFile(target, []byte("this endpoint kept something else"), 0o600); werr != nil {
		return res, werr
	}
	return res, nil
}

func (s *guardedStore) OpenObject(ctx context.Context, medium transport.Medium, key string) (io.ReadCloser, error) {
	s.opens[medium.ID+"\x00"+key]++
	return s.MediumStore.OpenObject(ctx, medium, key)
}

func (s *guardedStore) opensOf(medium, key string) int { return s.opens[medium+"\x00"+key] }

func (s *guardedStore) DeleteObject(ctx context.Context, medium transport.Medium, key string) error {
	if err := s.guard.before("deleting an object on "+medium.ID, medium.ID, key); err != nil {
		return err
	}
	return s.MediumStore.DeleteObject(ctx, medium, key)
}

type guardedLocal struct {
	artifactstore.Local
	guard *restartGuard
}

func (l *guardedLocal) Remove(ctx context.Context, locator string) error {
	if err := l.guard.before("removing the local copy", state.MediumLocal, locator); err != nil {
		return err
	}
	return l.Local.Remove(ctx, locator)
}

type crashResolver struct{ mediums []transport.Medium }

func (r crashResolver) Resolve(id string) (transport.Medium, placement.Class, error) {
	for _, m := range r.mediums {
		if m.ID == id {
			return m, placement.Content, nil
		}
	}
	return transport.Medium{}, "", fmt.Errorf("no medium %q", id)
}

type crashSets struct{ set config.BackupSet }

func (s crashSets) Set(model.BackupSetID) (config.BackupSet, error) { return s.set, nil }

type noTier struct{}

func (noTier) SourceStillSelected(context.Context, state.Record, string) (bool, string, error) {
	return false, "", nil
}

// plantPartialObject writes a half-written object at the destination key,
// which is what an upload interrupted in flight leaves on a medium.
func (w *world) plantPartialObject(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(w.bucket, filepath.FromSlash(w.key))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating the object's directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("planting the partial object: %v", err)
	}
}

// objectCount counts what the medium actually holds, so a cell can prove a
// resumed copy converged on one key rather than leaving an orphan.
func (w *world) objectCount(t *testing.T) int {
	t.Helper()
	return w.objectCountOn(t, w.bucket)
}

func (w *world) objectCountOn(t *testing.T, bucket string) int {
	t.Helper()
	var n int
	err := filepath.Walk(bucket, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("counting the medium's objects: %v", err)
	}
	return n
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
