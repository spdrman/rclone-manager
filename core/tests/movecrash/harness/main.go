// Command movecrash-harness is a real, disposable OS process that runs one
// cycle of the FR-30 move engine and can be told to kill itself, for real,
// at a named point.
//
// It exists so core/tests/movecrash can terminate a REAL process at each
// of the move's phase boundaries and inspect what a real crash actually
// leaves in the journal and on disk, rather than reasoning about it from a
// simulated failure inside the test binary.
//
// # This harness has no state machine of its own, and that is the point
//
// Issue #372 is open because the lifecycle crash matrix proves convergence
// about its own harness: crashmatrix/harness carries a second state
// machine that handles COMMITTING, while processArtifact, the code an
// operator actually runs, does not. The suite proves lifecycle.Commit
// resumes; nothing proves the pipeline gives it the chance.
//
// So this file cannot spell a phase. It calls placement.Engine.RunCycle
// and nothing else; the phase to die after arrives as an opaque string on
// the command line and is compared, never interpreted. There is no switch
// here, no phase constant, and no phase literal, and
// TestTheCrashHarnessHasNoPhaseMachineOfItsOwn parses this source and
// fails if one appears. A harness that cannot name a phase cannot drive
// one, so the only thing a crash cell can be proving is the engine.
//
// # What a real SIGKILL does and does not prove
//
// The same trust boundary crashmatrix/harness states: this machine's
// disks, kernel and Go runtime are trusted to have genuinely issued the
// syscalls the code before the kill point called. What a real SIGKILL adds
// over an in-process simulated failure is that nothing downstream of the
// kill point runs, no deferred cleanup, no buffered flush, no rollback,
// and the SQLite transaction that just committed is the last thing this
// process ever did.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "movecrash-harness: %v\n", err)
		os.Exit(1)
	}
}

var suppressKill bool

// selfDestruct terminates this process immediately and unrecoverably,
// exactly as an OOM killer or a power cut would. The trailing loop
// guarantees this goroutine makes no further journal or file write while
// the kernel finishes tearing the process down.
func selfDestruct(reason string) {
	fmt.Fprintf(os.Stderr, "MOVECRASH_SELF_KILL: %s\n", reason)
	_ = os.Stderr.Sync()
	if suppressKill {
		// The one path on which this returns. -suppress-kill exists so
		// the suite can prove, in the gate, that its own "the harness
		// must have been killed" assertion still fails when the kill
		// genuinely does not happen.
		fmt.Fprintln(os.Stderr, "MOVECRASH_SELF_KILL_SUPPRESSED")
		_ = os.Stderr.Sync()
		return
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	for {
	}
}

func run() error {
	var (
		journalPath = flag.String("journal", "", "path to the SQLite journal")
		root        = flag.String("root", "", "the backup set's local_path")
		bucket      = flag.String("bucket", "", "the directory standing in for the medium's bucket")
		mediumID    = flag.String("medium", "", "the medium id")
		source      = flag.String("source", "", "the backup set's source name")
		set         = flag.String("set", "", "the backup set's name")
		artifact    = flag.String("artifact", "", "the artifact's name")
		destination = flag.String("destination", "", "the medium the artifact should move to")
		plan        = flag.Bool("plan", false, "plan a move for the artifact as well as resuming")

		killAfterPhase       = flag.String("kill-after-phase", "", "die the instant a phase write with this target commits")
		killAfterPlan        = flag.Bool("kill-after-plan", false, "die the instant the durable intent to move is recorded, before any phase write")
		killAfterCopy        = flag.Bool("kill-after-copy", false, "die the instant a copy to the medium returns, before anything is journaled")
		killAfterSourceWipe  = flag.Bool("kill-after-source-delete", false, "die the instant the source copy is removed, before anything is journaled")
		corruptAfterCopy     = flag.Bool("corrupt-after-copy", false, "overwrite the object the copy just wrote, the way a hostile or broken endpoint would")
		suppressKillRequests = flag.Bool("suppress-kill", false, "turn every self-kill into a no-op, to prove the suite's own kill assertion is not vacuous")
	)
	flag.Parse()
	suppressKill = *suppressKillRequests

	ctx := context.Background()
	setID := model.BackupSetID{Source: *source, Set: *set}
	id := model.ArtifactID{Set: setID, Name: *artifact}

	journal, err := state.Open(ctx, *journalPath)
	if err != nil {
		return fmt.Errorf("opening the journal: %w", err)
	}
	defer func() { _ = journal.Close() }()

	local, err := artifactstore.NewLocal(*root)
	if err != nil {
		return fmt.Errorf("building the local store: %w", err)
	}

	medium := transport.Medium{ID: *mediumID, Type: transport.MediumTypeLocalDir, Bucket: *bucket}
	adapter := rclone.New()

	g := &invariantGuard{journal: journal, artifact: id}

	engine := &placement.Engine{
		Journal: &killerJournal{Journal: journal, killAfter: *killAfterPhase, killAfterPlan: *killAfterPlan, guard: g},
		Store: &killerStore{
			MediumStore: adapter, guard: g,
			killAfterCopy: *killAfterCopy, corruptAfterCopy: *corruptAfterCopy, bucket: *bucket,
		},
		Local:            &killerLocal{Local: local, guard: g, killAfterRemove: *killAfterSourceWipe},
		Mediums:          resolver{medium: medium},
		Sets:             sets{set: config.BackupSet{Name: *set, ID: setID, LocalPath: *root}},
		Tiers:            noTierWantsIt{},
		MaxMovesPerCycle: 4,
	}

	var plans []placement.Plan
	if *plan {
		plans = []placement.Plan{{Artifact: id, DestinationMedium: *destination}}
	}

	report, err := engine.RunCycle(ctx, plans)
	if err != nil {
		return fmt.Errorf("running a cycle: %w", err)
	}
	for _, o := range report.Outcomes {
		fmt.Printf("OUTCOME phase=%s resumed=%t refused=%q\n", o.Phase, o.Resumed, o.Refused)
	}
	for _, v := range g.violations {
		fmt.Printf("VIOLATION %s\n", v)
	}
	fmt.Println("FINISHED")
	return nil
}

// --- the invariant guard ----------------------------------------------

// invariantGuard re-reads the durable journal before either of the two
// events that can remove a copy, and refuses the delete if FR-30's
// standing invariant does not hold at that instant.
//
// Those two events are the complete set of things that can falsify the
// invariant, which is why guarding them is a continuous check rather than
// a sample: time passing does not remove a copy.
type invariantGuard struct {
	journal    *state.Journal
	artifact   model.ArtifactID
	violations []string
}

func (g *invariantGuard) before(what, locator string) error {
	rec, err := g.journal.Get(context.Background(), g.artifact)
	if err != nil {
		return fmt.Errorf("reading the journal before %s: %w", what, err)
	}
	if err := placement.CheckInvariant(rec); err != nil {
		return g.violation(what, err)
	}
	// And it has to still hold once this copy is gone. A guard that only
	// re-reads the journal cannot see a delete nobody journaled: at that
	// instant the journal still says the copy about to be destroyed is
	// ACTIVE and verified. Requiring a SURVIVING copy is what closes it.
	surviving := rec
	surviving.Placements = nil
	for _, p := range rec.Placements {
		if !samePlace(p.Location, locator) {
			surviving.Placements = append(surviving.Placements, p)
		}
	}
	if err := placement.CheckInvariant(surviving); err != nil {
		return g.violation(what, fmt.Errorf("once the copy at %q is gone, %w", locator, err))
	}
	return nil
}

func (g *invariantGuard) violation(what string, err error) error {
	g.violations = append(g.violations, fmt.Sprintf("%s: %v", what, err))
	fmt.Printf("VIOLATION %s: %v\n", what, err)
	_ = os.Stdout.Sync()
	return fmt.Errorf("the standing invariant does not hold, so %s is refused: %w", what, err)
}

// --- the kill seams ----------------------------------------------------

// killerJournal dies the instant a phase write whose target matches
// killAfter durably commits. The target arrives as an opaque flag value
// and is compared, never interpreted: see this file's package doc.
type killerJournal struct {
	*state.Journal
	killAfter     string
	killAfterPlan bool
	guard         *invariantGuard
	fired         bool
}

// PlanMove is the first durable write a move makes, and it is not a phase
// write, so it needs its own seam. The flag is a boolean rather than a
// phase name for this file's whole reason: this harness has no phase
// vocabulary.
func (j *killerJournal) PlanMove(ctx context.Context, p state.MovePlan) (state.Move, error) {
	mv, err := j.Journal.PlanMove(ctx, p)
	if err != nil {
		return mv, err
	}
	if !j.fired && j.killAfterPlan {
		j.fired = true
		selfDestruct("the durable intent to move " + p.Artifact.String() + " to " + p.DestinationMedium + " committed")
	}
	return mv, nil
}

func (j *killerJournal) AdvanceMove(ctx context.Context, a state.MoveAdvance) (state.Move, error) {
	mv, err := j.Journal.AdvanceMove(ctx, a)
	if err != nil {
		return mv, err
	}
	// The underlying call has returned, so the SQLite transaction has
	// committed and the write this crash point is named after has durably
	// happened.
	if !j.fired && j.killAfter != "" && a.To == j.killAfter && a.From != a.To {
		j.fired = true
		selfDestruct("the phase write " + a.From + " -> " + a.To + " committed")
	}
	return mv, nil
}

// killerStore can die the instant a real copy returns, and can overwrite
// what the copy wrote, which is what a hostile or simply broken endpoint
// looks like from this side.
type killerStore struct {
	transport.MediumStore
	guard            *invariantGuard
	killAfterCopy    bool
	corruptAfterCopy bool
	bucket           string
}

func (s *killerStore) UploadFromLocal(ctx context.Context, medium transport.Medium, localPath, key string, opts transport.UploadOptions) (transport.UploadResult, error) {
	res, err := s.MediumStore.UploadFromLocal(ctx, medium, localPath, key, opts)
	if err != nil {
		return res, err
	}
	if s.corruptAfterCopy {
		// The object really is there and really is the wrong bytes. This
		// is the only way to reach that state through a real backend
		// without a real hostile endpoint, and what it produces on disk is
		// exactly what one would produce.
		target := filepath.Join(s.bucket, filepath.FromSlash(key))
		if werr := os.WriteFile(target, []byte("this endpoint kept something else"), 0o600); werr != nil {
			return res, werr
		}
	}
	if s.killAfterCopy {
		selfDestruct("the copy to " + key + " returned, before anything was journaled")
	}
	return res, nil
}

func (s *killerStore) DeleteObject(ctx context.Context, medium transport.Medium, key string) error {
	if err := s.guard.before("deleting the destination object", key); err != nil {
		return err
	}
	return s.MediumStore.DeleteObject(ctx, medium, key)
}

// killerLocal guards the local delete, and can die the instant it lands.
type killerLocal struct {
	artifactstore.Local
	guard           *invariantGuard
	killAfterRemove bool
}

func (l *killerLocal) Remove(ctx context.Context, locator string) error {
	if err := l.guard.before("removing the local copy", locator); err != nil {
		return err
	}
	if err := l.Local.Remove(ctx, locator); err != nil {
		return err
	}
	if l.killAfterRemove {
		selfDestruct("the source copy at " + locator + " was removed, before anything was journaled")
	}
	return nil
}

// --- the small resolvers ----------------------------------------------

type resolver struct{ medium transport.Medium }

func (r resolver) Resolve(id string) (transport.Medium, placement.Class, error) {
	if id != r.medium.ID {
		return transport.Medium{}, "", fmt.Errorf("no medium %q is configured", id)
	}
	return r.medium, placement.Content, nil
}

type sets struct{ set config.BackupSet }

func (s sets) Set(id model.BackupSetID) (config.BackupSet, error) {
	if id != s.set.ID {
		return config.BackupSet{}, fmt.Errorf("no backup set %s is configured", id)
	}
	return s.set, nil
}

// noTierWantsIt is the retention answer this harness runs under: the
// artifact's home is the destination, so no tier on the source still
// selects it. The refusal path for the other answer is unit-tested.
type noTierWantsIt struct{}

func (noTierWantsIt) SourceStillSelected(context.Context, state.Record, string) (bool, string, error) {
	return false, "", nil
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
