package app

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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// The acceptance line #238 handed to #239, recorded on both issues: the
// move engine driven from the retention cycle, under max_moves_per_cycle,
// under FR-27's already-given consent.
//
// PR #395 landed the engine as a library with three seams behind test
// doubles and no production caller at all, and one of those seams,
// TierGuard, treats a nil value as a refusal, so until this file existed
// the engine physically could not delete a source. These tests are about
// the WIRING: which plans reach the engine, where they come from, and what
// bounds them. internal/placement's own suite already proves what the
// engine does with a plan once it has one.

// --- a medium a test can watch ---

// countingMedium is transport.MediumStore, small enough to read and
// complete enough to carry a whole move. It is deliberately not a
// generous fake: an unknown key is a NotFound-classified transport error,
// never a zero ObjectInfo, because a mover that cannot tell "not there"
// from "could not ask" deletes a local copy on the strength of a network
// failure.
type countingMedium struct {
	mu      sync.Mutex
	objects map[string][]byte
	uploads int
	deletes int
}

func newCountingMedium() *countingMedium {
	return &countingMedium{objects: map[string][]byte{}}
}

func (m *countingMedium) StatObject(_ context.Context, _ transport.Medium, key string) (transport.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return transport.ObjectInfo{}, &transport.Error{Category: transport.NotFound, Op: "stat", Cause: errors.New("no such key")}
	}
	return transport.ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (m *countingMedium) UploadFromLocal(_ context.Context, _ transport.Medium, localPath, key string, _ transport.UploadOptions) (transport.UploadResult, error) {
	b, err := os.ReadFile(localPath)
	if err != nil {
		return transport.UploadResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploads++
	m.objects[key] = b
	return transport.UploadResult{Key: key, BytesUploaded: int64(len(b))}, nil
}

func (m *countingMedium) OpenObject(_ context.Context, _ transport.Medium, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, &transport.Error{Category: transport.NotFound, Op: "open", Cause: errors.New("no such key")}
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

// ObjectChecksum refuses, exactly as rclone v1.75.0's s3 backend does:
// Fs.Hashes() returns only MD5, so no full-object SHA-256 attestation is
// obtainable. Every medium in these tests is therefore a readback medium,
// which is what a real s3 deployment gets.
func (m *countingMedium) ObjectChecksum(_ context.Context, _ transport.Medium, _ string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	return transport.ChecksumAttestation{}, &transport.Error{
		Category: transport.UnsupportedCapability, Op: "checksum",
		Cause: fmt.Errorf("this backend cannot attest a full-object %s", alg),
	}
}

func (m *countingMedium) DeleteObject(_ context.Context, _ transport.Medium, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	delete(m.objects, key)
	return nil
}

// ListObjects is on transport.MediumStore but deliberately NOT on
// placement.MediumStore: a move addresses exactly the key it planned, and
// a mover that can enumerate is a mover that can act on something it did
// not plan. It is here only because Service.MediumStore holds the wider
// interface, and it panics rather than returning an empty list, so a
// change that starts enumerating during a move is loud instead of
// plausible.
func (m *countingMedium) ListObjects(context.Context, transport.Medium, string) ([]transport.ObjectInfo, error) {
	panic("a move must never enumerate a medium")
}

func (m *countingMedium) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

// --- fixtures ---

const moveTestMedium = "cold_offsite"

// moveTestMediums is the storage_mediums block a chain naming
// moveTestMedium needs to be a real configuration rather than a tier
// pointing at nothing.
func moveTestMediums() []config.StorageMedium {
	return []config.StorageMedium{{
		ID:     moveTestMedium,
		Type:   config.StorageMediumTypeS3,
		Region: "us-east-1",
		Bucket: "nas-backups",
		Prefix: "rclone-manager",
	}}
}

// seedMovableArtifact writes a real local file and journals it to
// COMPLETE with the ACTIVE local placement a real ingestion writes
// (internal/lifecycle.Commit), so the artifact this cycle is asked to move
// is exactly the shape a live deployment holds.
func seedMovableArtifact(t *testing.T, ctx context.Context, journal *state.Journal, bs config.BackupSet, name string, at time.Time) model.ArtifactID {
	t.Helper()
	artifact, err := model.NewArtifactID(bs.ID, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%q): %v", name, err)
	}
	path := filepath.Join(bs.LocalPath, name)
	content := "the bytes of " + name
	mustWriteFile(t, path, content)

	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: "discover-" + name, From: "", To: string(lifecycle.Discovered),
		OccurredAt: at, RemotePath: "/backups/" + name,
	}); err != nil {
		t.Fatalf("RecordTransition(discover %s): %v", name, err)
	}
	lp := path
	size := int64(len(content))
	// The hash is not decoration. internal/lifecycle.Commit records it on
	// every local placement it writes, and the move engine refuses to
	// start a move whose source has none, because a destination copy could
	// never be content-verified against it. A fixture without one would be
	// testing a shape ingestion does not produce.
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: "complete-" + name,
		From: string(lifecycle.Discovered), To: string(lifecycle.Complete),
		OccurredAt: at, LocalPath: &lp,
		Transfer: &state.TransferResult{BytesTransferred: size, Checksummed: true},
		Placement: &state.PlacementUpdate{
			Medium: state.MediumLocal, Location: path, Size: &size,
			Hash: hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("RecordTransition(complete %s): %v", name, err)
	}
	return artifact
}

// movingService builds the Service a move cycle runs on: a real journal, a
// medium it can reach, a chain whose monthly tier is offsite, and the
// clock every decision is taken at.
func movingService(t *testing.T, medium transport.MediumStore, bound *int) (*Service, config.BackupSet, *state.Journal) {
	t.Helper()
	dir := t.TempDir()
	journal := openJournal(t)
	bs := testBackupSet(t, dir)
	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = chainWithOffsiteMonthly()
	cfg.StorageMediums = moveTestMediums()
	cfg.MaxMovesPerCycle = bound
	resolveTestRetention(cfg)

	// A real transport, because these tests drive the REAL RunCycle rather
	// than the move pass on its own. A cycle whose reconcile step cannot
	// reach a source stops before it ever computes a retention preview, so
	// a nil one would make every assertion below pass or fail for a reason
	// that has nothing to do with moves.
	svc := New(cfg, journal, newFakeTransport(), nil)
	svc.MediumStore = medium
	svc.Now = fixedNow(retentionTestNow)
	return svc, bs, journal
}

// --- the tests ---

// TestRunCycle_MovesTheArtifactTheChainSaysBelongsElsewhere is the
// acceptance line itself. Nothing here asks for a move: the retention pass
// works out that the monthly tier is the first tier selecting this
// artifact, the monthly tier's medium is not where the artifact is, and
// the cycle executes that under the consent the operator already gave by
// writing the tier.
func TestRunCycle_MovesTheArtifactTheChainSaysBelongsElsewhere(t *testing.T) {
	ctx := context.Background()
	medium := newCountingMedium()
	svc, bs, journal := movingService(t, medium, nil)

	// 40 days old: past the 7-day daily window, inside the monthly one, so
	// the first tier that selects it is monthly and its home is offsite.
	artifact := seedMovableArtifact(t, ctx, journal, bs, "monthly-only.dump", retentionTestNow.AddDate(0, 0, -40))

	report := svc.RunCycle(ctx)

	if report.Moves.Planned != 1 {
		t.Fatalf("Moves = %+v, want exactly one planned move; the chain says this artifact belongs on %q and it is on local", report.Moves, moveTestMedium)
	}
	if report.Moves.Completed != 1 {
		t.Fatalf("Moves = %+v, want the planned move to have completed", report.Moves)
	}

	key, err := transport.MediumKey("rclone-manager", artifact)
	if err != nil {
		t.Fatalf("MediumKey: %v", err)
	}
	if !medium.has(key) {
		t.Errorf("the medium holds no object at %q after a completed move", key)
	}
	if _, err := os.Stat(filepath.Join(bs.LocalPath, "monthly-only.dump")); !os.IsNotExist(err) {
		t.Errorf("the local copy is still there after a completed move (Stat err = %v); a move is copy, verify, THEN delete the source", err)
	}
}

// TestMoveEngine_IsNotBuiltAtAllWithNoMediumDeclared is the fail-safe,
// stated where it is decidable.
//
// A deployment that declares no storage medium has nowhere to move
// anything to, so there is no engine, and that is not a refusal: it is the
// ordinary state of every deployment written before EPIC E. It is checked
// here rather than only through RunCycle because a medium-free deployment
// also plans no moves, so the cycle-level outcome is identical whether an
// engine was built or not, and a test that could not tell those apart
// would pass against a build that constructed one on every cycle.
func TestMoveEngine_IsNotBuiltAtAllWithNoMediumDeclared(t *testing.T) {
	svc, _, _ := movingService(t, newCountingMedium(), nil)
	svc.Config.StorageMediums = nil

	engine, err := svc.moveEngine()
	if err != nil {
		t.Fatalf("moveEngine = %v; a deployment with no medium is not misconfigured, it simply has nothing to move", err)
	}
	if engine != nil {
		t.Errorf("moveEngine built an engine (%+v) for a deployment that declares no storage medium", engine)
	}
}

// TestRunCycle_MovesNothingInADeploymentWithNoMedium is FR-35's
// compatibility claim for this feature, end to end: a deployment that
// declares no storage medium runs exactly as it did before EPIC E.
//
// It is a regression control rather than a discriminating test, and the
// test above is the discriminating half. What this one is here to catch is
// the shape a unit test cannot: some later change making the cycle touch a
// local file, or reach a medium, on a configuration that names neither.
func TestRunCycle_MovesNothingInADeploymentWithNoMedium(t *testing.T) {
	ctx := context.Background()
	medium := newCountingMedium()
	svc, bs, journal := movingService(t, medium, nil)
	svc.Config.StorageMediums = nil
	// The chain still has to be one a medium-free deployment could write,
	// so the monthly tier loses its medium too. A tier naming a medium
	// that is not declared is a configuration config.Validate refuses.
	svc.Config.Retention = testRetention()
	resolveTestRetention(svc.Config)

	seedMovableArtifact(t, ctx, journal, bs, "monthly-only.dump", retentionTestNow.AddDate(0, 0, -40))

	report := svc.RunCycle(ctx)

	if report.Moves.Planned != 0 || report.Moves.Resumed != 0 || len(report.Moves.Outcomes) != 0 {
		t.Errorf("Moves = %+v, want nothing at all in a deployment with no storage medium", report.Moves)
	}
	if report.MovesErr != nil {
		t.Errorf("MovesErr = %v; a deployment with no medium is not misconfigured", report.MovesErr)
	}
	if medium.uploads != 0 {
		t.Errorf("%d uploads reached a medium this deployment does not declare", medium.uploads)
	}
	if _, err := os.Stat(filepath.Join(bs.LocalPath, "monthly-only.dump")); err != nil {
		t.Errorf("the local copy is gone (%v) in a deployment that moves nothing", err)
	}
}

// TestRunCycle_HonoursMaxMovesPerCycle is FR-30's per-cycle bound, read
// off the configuration key rather than off a struct field nothing sets.
//
// Three artifacts want to move and the bound is one, so exactly one moves
// and two local files survive the cycle. The surviving count is the
// assertion that matters: a bound that was read but not applied still
// reports Planned == 1 if the engine happens to stop early for another
// reason.
//
// The three ages are in three different MONTHS, and that is load bearing
// rather than tidy. Three artifacts inside one month are one monthly
// bucket, so GFS keeps exactly one of them and the other two are selected
// by nothing at all: no home, no move, and a test that would report one
// planned move whatever the bound said. The first draft of this test did
// exactly that, and a mutation replacing the configured bound with the
// built-in default stayed green.
func TestRunCycle_HonoursMaxMovesPerCycle(t *testing.T) {
	ctx := context.Background()
	medium := newCountingMedium()
	one := 1
	svc, bs, journal := movingService(t, medium, &one)

	names := []string{"a.dump", "b.dump", "c.dump"}
	for i, name := range names {
		seedMovableArtifact(t, ctx, journal, bs, name, retentionTestNow.AddDate(0, -(i+1), -10))
	}

	// The control: without three artifacts the monthly tier actually
	// selects, the bound below is being asserted against a plan of one.
	preview, err := svc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview: %v", err)
	}
	if len(preview.HomePlan.Moves) != 3 {
		t.Fatalf("the retention pass plans %+v, want all three artifacts moving; a bound cannot be tested against a plan smaller than it", preview.HomePlan.Moves)
	}

	report := svc.RunCycle(ctx)

	if report.Moves.Planned != 1 {
		t.Fatalf("Moves = %+v, want exactly one planned move under max_moves_per_cycle = 1", report.Moves)
	}
	survivors := 0
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(bs.LocalPath, name)); err == nil {
			survivors++
		}
	}
	if survivors != 2 {
		t.Errorf("%d of the three local copies survived the cycle, want 2: a bound that is read but not applied moves all three", survivors)
	}
	if medium.uploads != 1 {
		t.Errorf("%d uploads reached the medium, want 1", medium.uploads)
	}
}

// TestRunCycle_RefusesToMoveWithNoWayToReachAMedium is the other
// fail-safe, and it is the direction that costs nothing to get right and a
// backup to get wrong. A deployment that DECLARES a medium and has no
// store to reach it with is misconfigured, and the answer is a refusal
// that says so, never a quiet cycle that looks like a deployment with
// nothing to move.
func TestRunCycle_RefusesToMoveWithNoWayToReachAMedium(t *testing.T) {
	ctx := context.Background()
	svc, bs, journal := movingService(t, nil, nil)
	svc.MediumStore = nil

	seedMovableArtifact(t, ctx, journal, bs, "monthly-only.dump", retentionTestNow.AddDate(0, 0, -40))

	report := svc.RunCycle(ctx)

	if report.MovesErr == nil {
		t.Fatalf("a cycle with a declared medium and no medium store reported no error; Moves = %+v", report.Moves)
	}
	if report.Moves.Planned != 0 {
		t.Errorf("Moves = %+v, want nothing planned", report.Moves)
	}
	if _, err := os.Stat(filepath.Join(bs.LocalPath, "monthly-only.dump")); err != nil {
		t.Errorf("the local copy is gone (%v) after a cycle that could not reach a medium at all", err)
	}
}

// TestRunCycle_MovesNothingForAnArtifactWhoseLocationIsContested is FR-27
// consent's other edge, at the level that executes rather than the level
// that plans. Two ACTIVE placements is a move already in flight, so "where
// is this" has two answers, and a second move planned on top of one
// already running is the race FR-30's journal exists to make
// unrepresentable.
//
// The artifact is RECENT, so its home is the daily tier's local medium
// while its second placement is offsite. That is deliberate and it is what
// makes the test discriminate. The first draft used a month-old artifact,
// whose home is offsite, and placements come back ordered by medium, so
// "cold_offsite" is the first of the two: a build that took the first
// ACTIVE placement instead of refusing would have read the artifact as
// already at home and planned nothing, and the test would have passed
// against exactly the bug it exists to catch. With the home at local, the
// same wrong build plans a move.
func TestRunCycle_MovesNothingForAnArtifactWhoseLocationIsContested(t *testing.T) {
	ctx := context.Background()
	medium := newCountingMedium()
	svc, bs, journal := movingService(t, medium, nil)

	artifact := seedMovableArtifact(t, ctx, journal, bs, "recent.dump", retentionTestNow.AddDate(0, 0, -1))
	size := int64(1)
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: "second-placement",
		From: string(lifecycle.Complete), To: string(lifecycle.Complete),
		OccurredAt: retentionTestNow,
		Placement: &state.PlacementUpdate{
			Medium: moveTestMedium, Location: "artifacts/recent.dump", Size: &size,
			VerificationClass: state.VerificationExistence, Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("RecordTransition(second placement): %v", err)
	}

	report := svc.RunCycle(ctx)

	if report.Moves.Planned != 0 || len(report.Moves.Outcomes) != 0 {
		t.Errorf("Moves = %+v, want nothing planned for an artifact whose location cannot be confirmed", report.Moves)
	}
	if medium.uploads != 0 {
		t.Errorf("%d uploads were started for an artifact with a move already in flight", medium.uploads)
	}
	if _, err := os.Stat(filepath.Join(bs.LocalPath, "recent.dump")); err != nil {
		t.Errorf("the local copy is gone (%v); nothing should have touched it", err)
	}
}

// --- FR-32: bucketing invariance, across a move that really happened ---

// renderVerdicts is the canonical rendering the invariance test compares.
// It covers every field of every verdict, in artifact order, plus FR-19's
// own result, so a difference anywhere in the decision shows up as a
// different string rather than as a comparison nobody wrote.
func renderVerdicts(report RetentionSetReport) string {
	sorted := append([]retention.GFSVerdict(nil), report.Verdicts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Artifact.Name < sorted[j].Artifact.Name })

	var b strings.Builder
	for _, v := range sorted {
		fmt.Fprintf(&b, "%s keep=%v tiers=%v\n", v.Artifact.Name, v.Keep, v.Tiers)
		for _, line := range v.SiblingCollisionLines() {
			fmt.Fprintf(&b, "  ! %s\n", line)
		}
	}
	fmt.Fprintf(&b, "last-known-good: protected=%v artifact=%s reason=%s\n",
		report.LastKnownGood.Protected, report.LastKnownGood.Artifact, report.LastKnownGood.Reason)
	return b.String()
}

// TestBucketingIsInvariantUnderARealMove is FR-32's own sentence, pinned
// against a move that actually moved bytes: "moving an artifact never
// changes its retention verdicts, because placements come from the
// journal and never from the destination."
//
// #387 landed the decidable-today form of this, one fixture decided twice,
// with mediums and without. This is the form that needed #238's engine:
// decide, MOVE, decide again at the same instant, and require the two
// renderings to be byte-identical.
//
// # The planted violation, and where it has to be planted
//
// The spec names it as "rewriting the discovery timestamp from the
// destination during a move". It cannot be planted inside
// internal/retention, because placement.TestRetentionReadsNoMediumSupplied
// Value fails the build if that package so much as mentions a placement or
// a transport.ObjectInfo, and that scan has a positive control proving it
// visits the package. What CAN still do it is a caller one layer up
// deriving an input from a placement before handing it over, which is
// exactly what this test catches: rewriting DiscoveredAt from the ACTIVE
// placement's VerifiedAt in RetentionPreview is a one-line, entirely
// legal-looking change, and it turns this test red.
func TestBucketingIsInvariantUnderARealMove(t *testing.T) {
	ctx := context.Background()
	medium := newCountingMedium()
	svc, bs, journal := movingService(t, medium, nil)

	// A spread wide enough that the chain has real work to do: something
	// inside the daily window, something only monthly selects, something
	// nothing selects, and two in one month so a sibling collision is
	// decided too.
	for _, seed := range []struct {
		name string
		days int
	}{
		{"today.dump", 0},
		{"yesterday.dump", 1},
		{"month-old.dump", 40},
		{"same-month.dump", 44},
		{"ancient.dump", 800},
	} {
		seedMovableArtifact(t, ctx, journal, bs, seed.name, retentionTestNow.AddDate(0, 0, -seed.days))
	}

	before, err := svc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview (before): %v", err)
	}
	if len(before.HomePlan.Moves) == 0 {
		t.Fatal("nothing was going to move, so this test would compare a decision against itself")
	}

	moves, err := svc.RunHomeMoves(ctx, HomeMovePlans(before.HomePlan))
	if err != nil {
		t.Fatalf("RunHomeMoves: %v", err)
	}
	if moves.Completed == 0 {
		t.Fatalf("no move completed (%+v); the invariance below would hold trivially", moves.Outcomes)
	}

	after, err := svc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview (after): %v", err)
	}

	// The control: the move really did change where things are. Without
	// it, a build where nothing ever moves passes this test forever.
	movedName := before.HomePlan.Moves[0].Artifact.Name
	rec, err := journal.Get(ctx, before.HomePlan.Moves[0].Artifact)
	if err != nil {
		t.Fatalf("Get(%s): %v", movedName, err)
	}
	onMedium := false
	for _, p := range rec.Placements {
		if p.Medium == moveTestMedium && p.Status == state.PlacementActive {
			onMedium = true
		}
	}
	if !onMedium {
		t.Fatalf("%s has no ACTIVE placement on %q after a completed move: %+v", movedName, moveTestMedium, rec.Placements)
	}

	if got, want := renderVerdicts(after), renderVerdicts(before); got != want {
		t.Errorf("moving %s changed this backup set's retention verdicts.\nbefore:\n%s\nafter:\n%s", movedName, want, got)
	}
}
