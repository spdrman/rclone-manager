package conformance_test

import (
	"context"
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
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// This file is #242's other half of job one: "the full crash matrix from
// E2.1 runs against this composed scenario, not only against unit
// fixtures".
//
// tests/movecrash runs the same boundaries against rclone's local backend,
// with a directory standing in for a bucket. That proves the ENGINE
// survives a real SIGKILL at each point. It cannot prove anything about
// what a real S3 endpoint leaves behind, and the differences are exactly
// where a crash matrix earns its keep: a multipart upload interrupted
// mid-flight, an object that exists at a key with no journal row naming
// it, a second PUT to the same key after a crash, and a HEAD of an object
// a dead process was halfway through writing.
//
// So the same cells run here against MinIO, through the same harness
// binary. The harness grew four flags (medium type, endpoint, region,
// credentials file) whose zero values are what a directory-backed medium
// already wanted, so tests/movecrash's own invocations mean exactly what
// they meant before and both suites drive one harness. Two harnesses would
// be two crash matrices, and #372 is open because a suite proved things
// about its harness rather than about the product.
//
// The restart half is judged by this package's continuous watcher rather
// than by movecrash's guard, which is the one deliberate difference: the
// watcher also subtracts copies it has watched being destroyed, so a
// restart that removed bytes while the journal still called them ACTIVE
// would be caught here and is not caught there.

// crashArtifactName is the one artifact each cell moves. It is dated so
// that a chain would put it on a medium, but no chain runs here: a crash
// cell is about the engine's own convergence, and bringing retention into
// it would make a red cell ambiguous between the two.
const crashArtifactName = "2026-07-15T02-00-00Z.dump"

const crashMediumID = "offsite_s3"

var crashNow = time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)

// TestTheCrashMatrixAgainstARealS3Endpoint is the matrix.
//
// Every cell kills a real process at one real boundary and then restarts
// the real engine, in this process, against the journal and the bucket the
// dead process left behind.
func TestTheCrashMatrixAgainstARealS3Endpoint(t *testing.T) {
	fixture := machines.Start(t).Medium(t)

	for _, cell := range []struct {
		// name is the boundary, in the words tests/movecrash uses for the
		// same one, so the two matrices line up cell for cell.
		name string
		// kill is the harness flag that produces it.
		kill []string
		// proves is what this cell is here for, printed on failure so a
		// red cell says what was lost rather than only which flag was
		// passed.
		proves string
		// want is the phase the restart must converge to.
		want placement.Phase
		// sourceSurvives says whether the local copy must still be there
		// after the restart.
		sourceSurvives bool
		// setup runs before the crash.
		setup func(t *testing.T, w *crashWorld)
		// corruptOnRestart makes the restarting process's own endpoint
		// keep something other than what it was handed, so a bad
		// destination stays bad across the restart.
		corruptOnRestart bool
	}{
		{
			name:   "C1 after the intent to move is durably recorded",
			kill:   []string{"-plan", "-kill-after-plan"},
			proves: "recorded intent on its own uploads nothing and deletes nothing, and a plan alone converges",
			want:   placement.Done,
		},
		{
			name:   "C2 after the copy phase is durably recorded, before the upload",
			kill:   []string{"-plan", "-kill-after-phase=COPYING"},
			proves: "a copy-phase row with no object at the key converges instead of stalling",
			want:   placement.Done,
		},
		{
			name:   "C3 the instant the upload returns, before anything is journaled",
			kill:   []string{"-plan", "-kill-after-copy"},
			proves: "an object on the bucket with no journal entry naming it is found by the move row and re-used, not duplicated",
			want:   placement.Done,
		},
		{
			name: "C3b a copy-phase row with a half-written object already at the key",
			kill: []string{"-plan", "-kill-after-phase=COPYING"},
			setup: func(t *testing.T, w *crashWorld) {
				w.plantPartialObject(t, "half an artifact and no more")
			},
			proves: "the deterministic key means a resumed upload replaces the interrupted object rather than leaving a second one on the bucket",
			want:   placement.Done,
		},
		{
			name:   "C4 after the upload is journaled but before anything looks at it",
			kill:   []string{"-plan", "-kill-after-phase=COPIED"},
			proves: "an unverified destination has no placement row, so nothing can rely on it, and the restart verifies before it deletes",
			want:   placement.Done,
		},
		{
			name:   "C5 mid-verification",
			kill:   []string{"-plan", "-kill-after-phase=VERIFYING"},
			proves: "the read-back is redone from scratch against the real endpoint and never inferred from the phase",
			want:   placement.Done,
		},
		{
			name:   "C6 after the write that authorises a source delete",
			kill:   []string{"-plan", "-kill-after-phase=VERIFIED"},
			proves: "even that write is not trusted on its own: the restart re-downloads and re-hashes before it removes anything",
			want:   placement.Done,
		},
		{
			name:   "C7 after the source delete is intended, before it happens",
			kill:   []string{"-plan", "-kill-after-phase=SOURCE_DELETE_PENDING"},
			proves: "intent is recorded before the delete, so a crash in that window leaves a journal saying what was about to happen",
			want:   placement.Done,
		},
		{
			name:   "C8 the instant the source is removed, before the move is closed out",
			kill:   []string{"-plan", "-kill-after-source-delete"},
			proves: "the last window converges: the delete is idempotent and the row reaches its terminal phase on the next cycle",
			want:   placement.Done,
		},
		{
			name:   "C9 an endpoint that kept the wrong bytes, crashed once journaled",
			kill:   []string{"-plan", "-corrupt-after-copy", "-kill-after-phase=COPIED"},
			proves: "a destination the journal never verified is never what the artifact ends up relying on: the restart re-verifies, throws the bad object away and uploads again, and the surviving copy really hashes to the recorded SHA-256",
			want:   placement.Done,
		},
		{
			name:             "C10 an endpoint that is persistently wrong",
			kill:             []string{"-plan", "-corrupt-after-copy", "-kill-after-phase=COPIED"},
			corruptOnRestart: true,
			proves:           "the destination is the disposable copy: an endpoint that is always wrong ends with the move abandoned and the local copy still on disk",
			want:             placement.Abandoned,
			sourceSurvives:   true,
		},
	} {
		t.Run(cell.name, func(t *testing.T) {
			w := newCrashWorld(t, fixture)
			if cell.setup != nil {
				cell.setup(t, w)
			}

			res := w.crash(cell.kill...)
			if !res.killed() {
				t.Fatalf("the harness was not killed by SIGKILL (err=%v)\nstdout:\n%s\nstderr:\n%s",
					res.err, res.stdout, res.stderr)
			}
			if v := res.violations(); len(v) > 0 {
				t.Fatalf("the crashed process reported invariant violations: %v", v)
			}

			// 1. The invariant held at the instant of the crash, judged
			// from the journal the dead process left on disk.
			j := w.journalNow()
			rec, err := j.Get(w.ctx, w.artifact)
			if err != nil {
				t.Fatalf("reading the journal the crashed process left: %v", err)
			}
			if err := placement.CheckInvariant(rec); err != nil {
				t.Fatalf("the standing invariant did not hold at the instant of the crash: %v", err)
			}

			// 2. The restart converges, driven by the same RunCycle an
			// operator runs, told nothing about where the move was, and
			// watched continuously while it does.
			wa := newWatcher(t, j, []model.ArtifactID{w.artifact})
			wa.observe("at the instant of the crash")
			engine := w.restartEngine(j, wa, cell.corruptOnRestart)
			report, err := engine.RunCycle(w.ctx, nil)
			if err != nil {
				t.Fatalf("the restart failed: %v", err)
			}
			wa.report()
			if report.Resumed != 1 {
				t.Fatalf("the restart did not pick the move up: %+v\nthis cell proves: %s", report, cell.proves)
			}

			// 3. The move is terminal where it should be, and the bytes
			// that survive really are the artifact.
			moves, err := j.MovesForArtifact(w.ctx, w.artifact)
			if err != nil {
				t.Fatalf("reading the move journal: %v", err)
			}
			if len(moves) != 1 {
				t.Fatalf("expected exactly one move, got %d", len(moves))
			}
			if got := placement.Phase(moves[0].Phase); got != cell.want {
				t.Fatalf("the move converged to %s, want %s (error: %q)\nthis cell proves: %s",
					got, cell.want, moves[0].Error, cell.proves)
			}
			w.assertSurvivingBytesAreTheArtifact(t, j)

			if cell.sourceSurvives && !w.localExists() {
				t.Fatalf("THE SOURCE WAS DELETED. This cell proves: %s", cell.proves)
			}
			if !cell.sourceSurvives {
				if w.localExists() {
					t.Errorf("the local copy is still on disk after a completed move")
				}
				if !hasActiveOn(mustRecord(t, j, w.artifact), crashMediumID) {
					t.Errorf("no ACTIVE placement records the destination copy after a completed move")
				}
			}
			if n := w.objectCount(t); n > 1 {
				t.Errorf("the bucket holds %d objects; a resumed upload must converge on one key", n)
			}
		})
	}
}

// TestTheS3CrashCellsAreReallyKilled is the same vacuity control
// tests/movecrash keeps, re-run here because this suite passes different
// flags to the same binary and a suppressed kill would turn every cell
// above into an ordinary uninterrupted run that still went green.
func TestTheS3CrashCellsAreReallyKilled(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	w := newCrashWorld(t, fixture)
	res := w.crash("-plan", "-kill-after-phase=VERIFIED", "-suppress-kill")
	if res.killed() {
		t.Fatal("the harness died even with -suppress-kill set, so the flag does not do what the matrix relies on")
	}
	if !strings.Contains(res.stderr, "MOVECRASH_SELF_KILL_SUPPRESSED") {
		t.Fatalf("the harness never reached the kill point, so this proved nothing:\n%s", res.stderr)
	}
	if !strings.Contains(res.stdout, "FINISHED") {
		t.Fatalf("the suppressed run did not finish:\n%s\n%s", res.stdout, res.stderr)
	}
}

// --- the world one crash cell runs in ---------------------------------

type crashWorld struct {
	t   *testing.T
	ctx context.Context

	dir     string
	root    string
	journal string
	medium  transport.Medium

	artifact model.ArtifactID
	content  []byte
	hash     string
	key      string
}

func newCrashWorld(t *testing.T, fixture *machines.Medium) *crashWorld {
	t.Helper()

	medium := fixture.NewBucket(t)
	medium.ID = crashMediumID

	dir := t.TempDir()
	root := filepath.Join(dir, "backups")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("creating the backup set's local_path: %v", err)
	}

	content := []byte(strings.Repeat("the durable bytes of one backup artifact. ", 64))
	id := model.ArtifactID{Set: setID, Name: crashArtifactName}

	w := &crashWorld{
		t: t, ctx: context.Background(),
		dir: dir, root: root, journal: filepath.Join(dir, "journal.db"),
		medium: medium, artifact: id, content: content, hash: sha256Hex(content),
	}
	key, err := transport.MediumKey(medium.Prefix, id)
	if err != nil {
		t.Fatalf("computing the key: %v", err)
	}
	w.key = key

	j, err := state.Open(w.ctx, w.journal)
	if err != nil {
		t.Fatalf("opening the journal to seed: %v", err)
	}
	seedOnLocal(t, w.ctx, j, root, id, content, crashNow)
	if err := j.Close(); err != nil {
		t.Fatalf("closing the seeding journal: %v", err)
	}
	return w
}

// crash runs the harness with the given kill plan, pointed at the real
// bucket, and reports what happened to the process.
func (w *crashWorld) crash(extra ...string) crashResult {
	w.t.Helper()
	args := append([]string{
		"-journal", w.journal, "-root", w.root,
		"-medium", w.medium.ID, "-medium-type", string(w.medium.Type),
		"-bucket", w.medium.Bucket, "-endpoint", w.medium.Endpoint, "-region", w.medium.Region,
		"-credentials-file", w.medium.Credentials.File,
		"-source", scenarioSource, "-set", scenarioSet,
		"-artifact", w.artifact.Name, "-destination", w.medium.ID,
	}, extra...)

	cmd := exec.Command(buildCrashHarness(w.t), args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := crashResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.signal = ws.Signal()
		}
	}
	return res
}

func (w *crashWorld) journalNow() *state.Journal {
	w.t.Helper()
	j, err := state.Open(w.ctx, w.journal)
	if err != nil {
		w.t.Fatalf("reopening the journal after the crash: %v", err)
	}
	w.t.Cleanup(func() { _ = j.Close() })
	return j
}

// restartEngine is the engine this process resumes with: the same Engine
// the harness builds, with the same single entry point, because resume is
// not a separate path.
func (w *crashWorld) restartEngine(j *state.Journal, wa *watcher, corruptOnUpload bool) *placement.Engine {
	w.t.Helper()
	local, err := artifactstore.NewLocal(w.root)
	if err != nil {
		w.t.Fatalf("building the local store: %v", err)
	}
	var store transport.MediumStore = adapter()
	if corruptOnUpload {
		store = &alwaysWrongEndpoint{MediumStore: store}
	}
	return &placement.Engine{
		Journal:          &watchedJournal{inner: j, w: wa},
		Store:            &watchedStore{MediumStore: store, w: wa},
		Local:            &watchedLocal{Local: local, w: wa},
		Mediums:          fixedResolver{medium: w.medium},
		Sets:             scenarioSets{set: config.BackupSet{Name: scenarioSet, ID: setID, LocalPath: w.root}},
		Tiers:            nothingWantsTheSource{},
		MaxMovesPerCycle: 4,
	}
}

func (w *crashWorld) localPath() string { return filepath.Join(w.root, w.artifact.Name) }

func (w *crashWorld) localExists() bool {
	_, err := os.Lstat(w.localPath())
	return err == nil
}

// plantPartialObject puts a half-written object at the destination key,
// which is what an upload interrupted in flight leaves on a bucket.
func (w *crashWorld) plantPartialObject(t *testing.T, body string) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "partial")
	if err := writeFile(tmp, []byte(body)); err != nil {
		t.Fatalf("writing the partial object's body: %v", err)
	}
	if _, err := adapter().UploadFromLocal(w.ctx, w.medium, tmp, w.key, transport.UploadOptions{}); err != nil {
		t.Fatalf("planting the partial object: %v", err)
	}
}

// objectCount is what the bucket actually holds, so a cell can prove a
// resumed upload converged on one key rather than leaving an orphan.
func (w *crashWorld) objectCount(t *testing.T) int {
	t.Helper()
	objects, err := adapter().ListObjects(w.ctx, w.medium, "")
	if err != nil {
		t.Fatalf("listing the bucket: %v", err)
	}
	return len(objects)
}

// assertSurvivingBytesAreTheArtifact reads every ACTIVE placement back
// from wherever the journal says it is and hashes it. A converged move row
// with no readable bytes behind it is the failure this whole matrix is
// about.
func (w *crashWorld) assertSurvivingBytesAreTheArtifact(t *testing.T, j *state.Journal) {
	t.Helper()
	rec := mustRecord(t, j, w.artifact)
	var checked int
	for _, p := range rec.Placements {
		if p.Status != state.PlacementActive {
			continue
		}
		checked++
		var got []byte
		if p.Medium == state.MediumLocal {
			b, err := os.ReadFile(p.Location)
			if err != nil {
				t.Fatalf("reading the local copy at %q: %v", p.Location, err)
			}
			got = b
		} else {
			got = readObject(t, w.ctx, w.medium, p.Location)
		}
		if sha256Hex(got) != w.hash {
			t.Errorf("the ACTIVE placement on %q does not hold the artifact's bytes", p.Medium)
		}
	}
	if checked == 0 {
		t.Fatalf("no ACTIVE placement survived, so the artifact has no copy at all: %s", describe(rec))
	}
}

// --- small pieces ------------------------------------------------------

type crashResult struct {
	stdout, stderr string
	signal         syscall.Signal
	err            error
}

func (r crashResult) killed() bool { return r.signal == syscall.SIGKILL }

func (r crashResult) violations() []string {
	var out []string
	for _, line := range strings.Split(r.stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "VIOLATION "); ok {
			out = append(out, rest)
		}
	}
	return out
}

var (
	crashHarnessOnce sync.Once
	crashHarnessBin  string
	crashHarnessErr  error
)

// buildCrashHarness builds tests/movecrash/harness, which is deliberately
// the SAME binary that suite kills. See this file's own comment for why
// there is not a second one.
func buildCrashHarness(t *testing.T) string {
	t.Helper()
	crashHarnessOnce.Do(func() {
		dir, err := os.MkdirTemp("", "conformance-crash-harness")
		if err != nil {
			crashHarnessErr = err
			return
		}
		bin := filepath.Join(dir, "movecrash-harness")
		out, err := exec.Command("go", "build", "-o", bin, "../movecrash/harness").CombinedOutput()
		if err != nil {
			crashHarnessErr = fmt.Errorf("building tests/movecrash/harness: %v\n%s", err, out)
			return
		}
		crashHarnessBin = bin
	})
	if crashHarnessErr != nil {
		t.Fatalf("%v", crashHarnessErr)
	}
	return crashHarnessBin
}

// alwaysWrongEndpoint keeps something other than what it was handed, every
// time. One bad upload is recoverable by uploading again; an endpoint that
// is always wrong is what has to end with the local copy still on disk.
type alwaysWrongEndpoint struct{ transport.MediumStore }

func (s *alwaysWrongEndpoint) UploadFromLocal(ctx context.Context, medium transport.Medium, localPath, key string, opts transport.UploadOptions) (transport.UploadResult, error) {
	res, err := s.MediumStore.UploadFromLocal(ctx, medium, localPath, key, opts)
	if err != nil {
		return res, err
	}
	tmp, err := os.CreateTemp("", "conformance-wrong-bytes")
	if err != nil {
		return res, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString("this endpoint kept something else"); err != nil {
		_ = tmp.Close()
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	if _, err := s.MediumStore.UploadFromLocal(ctx, medium, tmp.Name(), key, transport.UploadOptions{}); err != nil {
		return res, err
	}
	return res, nil
}

type fixedResolver struct {
	medium transport.Medium
	class  placement.Class
}

func (r fixedResolver) Resolve(id string) (transport.Medium, placement.Class, error) {
	if id != r.medium.ID {
		return transport.Medium{}, "", fmt.Errorf("no medium %q is configured", id)
	}
	class := r.class
	if class == "" {
		class = placement.Content
	}
	return r.medium, class, nil
}

// nothingWantsTheSource is the retention answer a crash cell runs under:
// the artifact's home is the destination, so no tier on the source still
// selects it. The refusal for the other answer is unit-tested in
// internal/placement, and bringing a real chain in here would make a red
// cell ambiguous between the engine and the arithmetic.
type nothingWantsTheSource struct{}

func (nothingWantsTheSource) SourceStillSelected(context.Context, state.Record, string) (bool, string, error) {
	return false, "", nil
}

func hasActiveOn(rec state.Record, medium string) bool {
	for _, p := range rec.Placements {
		if p.Medium == medium && p.Status == state.PlacementActive {
			return true
		}
	}
	return false
}

func mustRecord(t *testing.T, j *state.Journal, id model.ArtifactID) state.Record {
	t.Helper()
	rec, err := j.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("reading %s: %v", id.Name, err)
	}
	return rec
}

func readObject(t *testing.T, ctx context.Context, medium transport.Medium, key string) []byte {
	t.Helper()
	rc, err := adapter().OpenObject(ctx, medium, key)
	if err != nil {
		t.Fatalf("opening the object at %q: %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading the object at %q: %v", key, err)
	}
	return b
}
