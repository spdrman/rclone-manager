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
	"io"
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
		bucket      = flag.String("bucket", "", "the medium's bucket: a directory for a local_dir medium, a real bucket name for an s3 one")
		mediumID    = flag.String("medium", "", "the medium id")
		mediumType  = flag.String("medium-type", string(transport.MediumTypeLocalDir), "the medium's transport type")

		// A second medium, so a cell can drive a hop whose two ends are
		// both mediums. It shares the type, endpoint, region and
		// credentials of the first, because both suites that run this
		// binary put their two mediums on one endpoint; only the id and
		// the bucket differ. Left empty, nothing about this harness
		// changes and every existing invocation means what it meant.
		secondMediumID = flag.String("second-medium", "", "a second medium id, for a hop between two mediums")
		secondBucket   = flag.String("second-bucket", "", "the second medium's bucket")
		endpoint       = flag.String("endpoint", "", "the medium's endpoint, for an s3 medium")
		region         = flag.String("region", "", "the medium's region, for an s3 medium")
		credentials    = flag.String("credentials-file", "", "a shared-credentials file, for an s3 medium")
		source         = flag.String("source", "", "the backup set's source name")
		set            = flag.String("set", "", "the backup set's name")
		artifact       = flag.String("artifact", "", "the artifact's name")
		destination    = flag.String("destination", "", "the medium the artifact should move to")
		plan           = flag.Bool("plan", false, "plan a move for the artifact as well as resuming")

		killAfterPhase       = flag.String("kill-after-phase", "", "die the instant a phase write with this target commits")
		killAfterPlan        = flag.Bool("kill-after-plan", false, "die the instant the durable intent to move is recorded, before any phase write")
		killAfterCopy        = flag.Bool("kill-after-copy", false, "die the instant a copy to the medium returns, before anything is journaled")
		killAfterSourceWipe  = flag.Bool("kill-after-source-delete", false, "die the instant a local file is removed, before anything is journaled")
		killAfterObjectWipe  = flag.Bool("kill-after-object-delete", false, "die the instant an object is removed from a medium, before anything is journaled")
		killDuringStage      = flag.Bool("kill-during-stage", false, "die part-way through reading a medium object back onto local disk")
		killAfterStage       = flag.Bool("kill-after-stage", false, "die the instant a local file is written, before anything is done with it")
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

	// The medium is described entirely by flags so the same harness, and
	// therefore the same crash cells, run against a directory standing in
	// for a bucket and against a real S3 endpoint. The zero values are
	// what a local_dir medium wants, so every existing invocation means
	// exactly what it meant before.
	medium := transport.Medium{
		ID:          *mediumID,
		Type:        transport.MediumType(*mediumType),
		Bucket:      *bucket,
		Endpoint:    *endpoint,
		Region:      *region,
		Credentials: transport.MediumCredentials{File: *credentials},
	}
	mediums := []transport.Medium{medium}
	if *secondMediumID != "" {
		second := medium
		second.ID = *secondMediumID
		second.Bucket = *secondBucket
		mediums = append(mediums, second)
	}
	adapter := rclone.New()

	g := &invariantGuard{journal: journal, artifact: id}

	engine := &placement.Engine{
		Journal: &killerJournal{Journal: journal, killAfter: *killAfterPhase, killAfterPlan: *killAfterPlan, guard: g},
		Store: &killerStore{
			MediumStore: adapter, guard: g,
			killAfterCopy: *killAfterCopy, corruptAfterCopy: *corruptAfterCopy,
			killAfterDelete: *killAfterObjectWipe, killDuringRead: *killDuringStage,
		},
		Local: &killerLocal{
			Local: local, guard: g,
			killAfterRemove: *killAfterSourceWipe, killAfterPut: *killAfterStage,
		},
		Mediums:          resolver{mediums: mediums},
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

// before evaluates the invariant, and then evaluates it again as it WILL
// be once the copy on medium at locator is gone.
//
// A copy is a MEDIUM and a locator, never a locator alone. FR-28's key is
// <prefix>/<source>/<set>/<artifact-name>, so two mediums that declare no
// prefix give one artifact the same key on both, and nothing had two
// medium copies at once until a hop between two mediums did (#429). Keyed
// by locator, deleting the source subtracted the destination as well and
// this guard refused a delete that was perfectly safe, which parks the
// move at the phase before it for ever. The same mistake the other way
// round is worse: a filter that removes the wrong copy can leave the one
// being deleted in the surviving set, and then a real breach reads clean.
func (g *invariantGuard) before(what, medium, locator string) error {
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
	killAfterDelete  bool
	killDuringRead   bool
}

// OpenObject can hand back a reader that dies part-way through, which is
// the one crash point a hop between two mediums has that no other move
// does: the bytes come down onto local disk before they go anywhere, and
// a process that dies during that read leaves whatever the local store's
// write was in the middle of.
//
// Killing inside the read rather than after it is the point. The local
// store writes through a temporary file and links it into place, so a
// process killed DURING the read and one killed AFTER the write leave
// two genuinely different things behind, and only one of them is a file
// at the name anything will look for.
func (s *killerStore) OpenObject(ctx context.Context, medium transport.Medium, key string) (io.ReadCloser, error) {
	rc, err := s.MediumStore.OpenObject(ctx, medium, key)
	if err != nil || !s.killDuringRead {
		return rc, err
	}
	return &dyingReader{ReadCloser: rc, what: key}, nil
}

// dyingReader serves one read and then kills the process, so the write
// consuming it is interrupted in flight rather than completed.
type dyingReader struct {
	io.ReadCloser
	what string
	read bool
}

func (r *dyingReader) Read(p []byte) (int, error) {
	if r.read {
		selfDestruct("the read of " + r.what + " was interrupted in flight")
	}
	n, err := r.ReadCloser.Read(p)
	r.read = true
	return n, err
}

func (s *killerStore) UploadFromLocal(ctx context.Context, medium transport.Medium, localPath, key string, opts transport.UploadOptions) (transport.UploadResult, error) {
	res, err := s.MediumStore.UploadFromLocal(ctx, medium, localPath, key, opts)
	if err != nil {
		return res, err
	}
	if s.corruptAfterCopy {
		if werr := s.corrupt(ctx, medium, key); werr != nil {
			return res, werr
		}
	}
	if s.killAfterCopy {
		selfDestruct("the copy to " + key + " returned, before anything was journaled")
	}
	return res, nil
}

// corrupt replaces what the copy just wrote with something else, which is
// the only way to reach "the object really is there and really is the
// wrong bytes" through a real backend without a real hostile endpoint.
//
// It has two implementations because the two backend kinds this harness
// runs against are reached differently, and neither may go through the
// manager in a way that would journal the substitution: a directory bucket
// is written on disk, and a real endpoint is written through a second
// upload to the same key, which is what an endpoint keeping the wrong
// bytes looks like from every side the manager can see.
//
// The directory it writes into comes from the MEDIUM it was handed, not
// from a field on this struct. It used to be the latter, which was
// indistinguishable from correct while there was one medium and became a
// real defect the moment there were two: a hop between two mediums
// corrupted the SOURCE object rather than the destination, and the cell
// that found it read as the engine refusing a source delete for a size
// mismatch. Both mediums give one artifact the same key, so nothing about
// the key could have caught it.
func (s *killerStore) corrupt(ctx context.Context, medium transport.Medium, key string) error {
	const wrong = "this endpoint kept something else"
	if medium.Type == transport.MediumTypeLocalDir {
		return os.WriteFile(filepath.Join(medium.Bucket, filepath.FromSlash(key)), []byte(wrong), 0o600)
	}
	f, err := os.CreateTemp("", "movecrash-wrong-bytes")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(wrong); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = s.MediumStore.UploadFromLocal(ctx, medium, f.Name(), key, transport.UploadOptions{})
	return err
}

func (s *killerStore) DeleteObject(ctx context.Context, medium transport.Medium, key string) error {
	if err := s.guard.before("deleting an object on "+medium.ID, medium.ID, key); err != nil {
		return err
	}
	if err := s.MediumStore.DeleteObject(ctx, medium, key); err != nil {
		return err
	}
	if s.killAfterDelete {
		selfDestruct("the object " + key + " on " + medium.ID + " was removed, before anything was journaled")
	}
	return nil
}

// killerLocal guards the local delete, and can die the instant it lands.
type killerLocal struct {
	artifactstore.Local
	guard           *invariantGuard
	killAfterRemove bool
	killAfterPut    bool
}

// Put can die the instant a local file is written and linked into place,
// which is the second crash point a hop between two mediums adds: the
// bytes are down, complete and correct, and nothing has been uploaded or
// journaled about them.
//
// The kill is after the underlying call returns, so the local store's
// temporary-file-then-link has finished and the file is at the name a
// resumed move will look for. That is the whole difference from
// -kill-during-stage, and the pair is what says which of the two things a
// crash can leave behind is the one the engine has to cope with.
func (l *killerLocal) Put(ctx context.Context, locator string, r io.Reader) error {
	if err := l.Local.Put(ctx, locator, r); err != nil {
		return err
	}
	if l.killAfterPut {
		selfDestruct("the local file at " + locator + " was written, before anything was done with it")
	}
	return nil
}

// Remove guards, and can die on, every local delete this engine makes.
//
// It says "a local file" rather than "the source copy" because it is no
// longer only the source: a hop between two mediums removes its staging
// copy through this same call, and a guard message that named the wrong
// thing would send somebody looking for a lost backup that is not lost.
// The guard's arithmetic is unaffected either way, since it filters on the
// locator and a staging path is no placement's location.
func (l *killerLocal) Remove(ctx context.Context, locator string) error {
	if err := l.guard.before("removing a local file", state.MediumLocal, locator); err != nil {
		return err
	}
	if err := l.Local.Remove(ctx, locator); err != nil {
		return err
	}
	if l.killAfterRemove {
		selfDestruct("the local file at " + locator + " was removed, before anything was journaled")
	}
	return nil
}

// --- the small resolvers ----------------------------------------------

type resolver struct{ mediums []transport.Medium }

func (r resolver) Resolve(id string) (transport.Medium, placement.Class, error) {
	for _, m := range r.mediums {
		if m.ID == id {
			return m, placement.Content, nil
		}
	}
	return transport.Medium{}, "", fmt.Errorf("no medium %q is configured", id)
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
