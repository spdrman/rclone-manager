package placement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is FR-30's move engine, and it is the code in this project
// that deletes a copy of a backup. internal/lifecycle/remotedelete.go's
// package doc calls itself "the most dangerous line in the project on
// purpose" and internal/retention/prune.go calls itself the second most
// dangerous. This one deletes the copy those two exist to protect, so read
// the ordering argument before changing anything here.
//
// # The ordering, and where it is enforced
//
// A move is copy, verify, delete source, and the source is deleted only
// after VERIFIED is durably recorded. That sentence is enforced in four
// independent places, deliberately, because it is the only thing standing
// between this package and losing a backup:
//
//  1. phases.go's table has no edge from anywhere but SOURCE_DELETE_PENDING
//     into a state where a delete happens, and no edge into
//     SOURCE_DELETE_PENDING except from VERIFIED.
//  2. state.AdvanceMove compares the phase in the UPDATE's own WHERE
//     clause, so a write against a row that has moved on affects no rows.
//  3. deleteSource re-verifies the destination from scratch, immediately
//     before the delete, whether it arrived there fresh or after a crash.
//     FR-30 asks for that on the restart path; doing it on both paths
//     means there is only one path.
//  4. guardSourceDelete re-derives every precondition from the DURABLE
//     journal and the real filesystem at the moment of the delete, and
//     refuses on anything it cannot prove. It is this file's
//     pruneVerifySafeToDelete, and it is written the same way and for the
//     same reason.
//  5. The same guard ends by asking internal/archive whether a copy that
//     SURVIVES the delete can actually be read right now
//     (archive.CheckSourceDelete), which is a fact about the present that
//     no journal row carries: a placement can say ACTIVE and
//     content-verified and describe bytes that are hours away from
//     anybody, because nothing rewrites verification_class when a bucket
//     lifecycle rule transitions an object or a restore window expires.
//     archive's own composition guard fails the build if this package
//     stops making that call.
//
// That is redundant on purpose. internal/retention's own package doc makes
// the argument at length: a safety check worth having is worth re-running
// at the point of the dangerous action, not merely upstream of it.
//
// # Resume is not a separate path
//
// RunCycle reconciles every non-terminal move and plans new ones, and both
// end up in the same advance loop. There is no "resume" function and no
// second driver, which is a direct response to #372: a crash suite that
// drives an artifact through a state machine of its own proves things
// about that machine, and the product's own driver can be missing a case
// for a state without anyone noticing. Here there is one driver, its
// switch is checked against phases.go's own list of non-terminal phases by
// a test, and the crash harness cannot spell a phase at all.
//
// # What this file does not do
//
// It does not decide WHICH artifacts should move. FR-27's home-medium rule
// is a retention question and belongs to the retention pass (#239), which
// hands RunCycle a list of plans. This file's only opinion about a plan is
// whether executing it is safe, which it re-forms from the journal rather
// than trusting the caller.

// MoveJournal is the durable surface the engine needs. It is stated here,
// narrower than *state.Journal, so a crash harness can decorate it and
// self-destruct the instant a phase write commits without this package
// knowing anything about that.
type MoveJournal interface {
	Get(ctx context.Context, artifact model.ArtifactID) (state.Record, error)
	PlanMove(ctx context.Context, p state.MovePlan) (state.Move, error)
	AdvanceMove(ctx context.Context, a state.MoveAdvance) (state.Move, error)
	ListMoves(ctx context.Context, phases ...string) ([]state.Move, error)
}

// MediumStore is the slice of transport.MediumStore a move needs. Stating
// it narrowly is what lets a test substitute a double, and it is also why
// ListObjects is absent: a move addresses exactly the key it planned, and
// a mover that can enumerate is a mover that can act on something it did
// not plan.
type MediumStore interface {
	StatObject(ctx context.Context, medium transport.Medium, key string) (transport.ObjectInfo, error)
	UploadFromLocal(ctx context.Context, medium transport.Medium, localPath, key string, opts transport.UploadOptions) (transport.UploadResult, error)
	OpenObject(ctx context.Context, medium transport.Medium, key string) (io.ReadCloser, error)
	ObjectChecksum(ctx context.Context, medium transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error)
	DeleteObject(ctx context.Context, medium transport.Medium, key string) error

	// RestoreStatus is what the medium says about a restore of the object
	// at key, or nil when it reports none, which is what S3 returns for an
	// object nobody has asked to restore. It is the one read that turns a
	// storage class into an access state honestly: an archive-class copy
	// is unreadable UNLESS a restore is in effect, and the journal cannot
	// know that. It addresses exactly the key it is given, which is the
	// property this interface exists to keep, and it initiates nothing.
	// The engine asks it only for a copy on an archive class; see observe.
	RestoreStatus(ctx context.Context, medium transport.Medium, key string) (*transport.RestoreState, error)
}

// LocalStore is the slice of artifactstore.Store the local end of a move
// needs. artifactstore's package doc describes exactly this caller: "a
// mover, when one is written, composes them in one place, in one auditable
// order". This is that mover.
type LocalStore interface {
	Stat(ctx context.Context, locator string) (artifactstore.Stat, error)
	Open(ctx context.Context, locator string) (io.ReadCloser, error)
	Put(ctx context.Context, locator string, r io.Reader) error
	Remove(ctx context.Context, locator string) error
}

// MediumResolver answers how to reach a configured medium and what a move
// to it has to achieve before a source may be deleted.
//
// Returning the required Class rather than the raw upload_verification
// string keeps FR-31's mapping in one place, and it is what makes
// "attested fails loudly" structural: Verify never falls back, so a medium
// whose required class is Attested against an endpoint that cannot attest
// produces ErrClassUnavailable and the move refuses, instead of quietly
// verifying less.
type MediumResolver interface {
	Resolve(id string) (transport.Medium, Class, error)
}

// BackupSets resolves a backup set's configuration, which is where a local
// copy's root comes from. FR-20's containment proof is against the
// CONFIGURED root, re-read at the moment of the delete, never against a
// directory derived from the path being deleted.
type BackupSets interface {
	Set(id model.BackupSetID) (config.BackupSet, error)
}

// TierGuard answers FR-30's last question before a source delete: does any
// retention tier whose medium is the source still select this artifact?
//
// It is injected rather than computed here because the answer is retention
// arithmetic (#239 owns the home-medium rule and the chain evaluation),
// and because a nil guard has to mean "refuse", not "allow". An engine
// with no guard cannot prove the source is unwanted, and uncertainty
// preserves the source.
type TierGuard interface {
	SourceStillSelected(ctx context.Context, rec state.Record, medium string) (bool, string, error)
}

// Engine executes FR-30's journaled three-phase moves.
type Engine struct {
	Journal MoveJournal
	Store   MediumStore
	Local   LocalStore
	Mediums MediumResolver
	Sets    BackupSets
	Tiers   TierGuard

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// MaxMovesPerCycle bounds how many moves one cycle touches, resumed
	// ones included, the same shape revalidation's max_per_cycle has. Zero
	// or negative means the engine does nothing at all, which is the same
	// fail-safe direction revalidate.SelectDue takes for the same field.
	//
	// It is a field rather than a config key here on purpose. FR-30 names
	// a max_moves_per_cycle guard, and the place that reads it is the
	// retention cycle that calls RunCycle, which is #239's. Adding the
	// schema key in this change would put a key in config.yaml that
	// nothing reads, and FR-35's round-trip rule is specifically about not
	// doing that.
	MaxMovesPerCycle int

	// MaxCopyAttempts bounds how many times one cycle will copy a
	// destination that keeps failing verification before abandoning the
	// move. Zero means defaultMaxCopyAttempts.
	MaxCopyAttempts int
}

const defaultMaxCopyAttempts = 2

// maxPhaseStepsPerMove bounds one move's walk through the phase machine in
// a single cycle. A nominal move takes six steps; this is generous enough
// to absorb a re-copy and small enough that a cycle cannot spin.
const maxPhaseStepsPerMove = 32

// Plan is one requested move: this artifact belongs on that medium.
//
// The engine does not check whether the caller was right about the home
// medium; it checks whether acting on it is safe. See the file comment.
type Plan struct {
	Artifact model.ArtifactID

	// DestinationMedium is config.MediumLocal for a move back to the
	// backup set's own local_path, or a configured medium id.
	DestinationMedium string
}

// Outcome is what happened to one move.
type Outcome struct {
	Artifact model.ArtifactID
	MoveID   int64
	Phase    Phase

	// Resumed is true when this move was found in the journal rather than
	// planned by this cycle.
	Resumed bool

	// Refused carries the reason a plan never became a move, or a move
	// stopped short of a terminal phase. It is empty on a move that
	// reached DONE.
	Refused string
}

// CycleReport is what one RunCycle did.
type CycleReport struct {
	Resumed   int
	Planned   int
	Completed int
	Abandoned int
	Refused   int
	Outcomes  []Outcome
}

// ErrNotEligible is what a plan the engine refuses to start is refused
// with. It is a distinct error because "this artifact must not move" is a
// routine, expected answer that a caller reports and carries on from,
// while a storage failure is not.
var ErrNotEligible = errors.New("placement: this artifact is not eligible to move")

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) maxCopyAttempts() int {
	if e.MaxCopyAttempts > 0 {
		return e.MaxCopyAttempts
	}
	return defaultMaxCopyAttempts
}

// RunCycle is the engine's one entry point. It reconciles every
// non-terminal move it finds, then plans and executes as many of plans as
// the per-cycle bound still allows.
//
// Reconciliation comes first and is not optional: a move left in flight by
// a crash is holding a source placement at DELETE_PENDING or an unverified
// object on a medium, and planning new work before finishing that is how a
// backlog of half-moves accumulates.
func (e *Engine) RunCycle(ctx context.Context, plans []Plan) (CycleReport, error) {
	var report CycleReport
	if e.Journal == nil {
		return report, fmt.Errorf("placement: the move engine needs a journal")
	}
	if e.MaxMovesPerCycle <= 0 {
		return report, nil
	}

	live, err := e.Journal.ListMoves(ctx, NonTerminalPhaseStrings()...)
	if err != nil {
		return report, err
	}

	budget := e.MaxMovesPerCycle
	resuming := map[string]bool{}
	for _, mv := range live {
		if budget == 0 {
			break
		}
		budget--
		report.Resumed++
		resuming[mv.Artifact.String()] = true
		report.record(e.advance(ctx, mv, true))
	}

	for _, p := range plans {
		if budget == 0 {
			break
		}
		if resuming[p.Artifact.String()] {
			// This artifact already had a move in flight, which this cycle
			// has just driven. Planning a second one is refused by
			// state.PlanMove anyway; not asking is the difference between
			// an expected outcome and a reported error.
			continue
		}
		mv, err := e.plan(ctx, p)
		if err != nil {
			budget--
			report.Refused++
			report.Outcomes = append(report.Outcomes, Outcome{
				Artifact: p.Artifact,
				Refused:  err.Error(),
			})
			continue
		}
		budget--
		report.Planned++
		report.record(e.advance(ctx, mv, false))
	}
	return report, nil
}

func (r *CycleReport) record(o Outcome) {
	switch {
	case o.Phase == Done:
		r.Completed++
	case o.Phase == Abandoned:
		r.Abandoned++
	case o.Refused != "":
		r.Refused++
	}
	r.Outcomes = append(r.Outcomes, o)
}

// plan checks eligibility and writes the durable PLANNED row.
func (e *Engine) plan(ctx context.Context, p Plan) (state.Move, error) {
	rec, err := e.Journal.Get(ctx, p.Artifact)
	if err != nil {
		return state.Move{}, err
	}
	src, err := e.eligibleSource(rec, p.DestinationMedium)
	if err != nil {
		return state.Move{}, err
	}
	key, err := e.destinationLocator(rec, p.DestinationMedium)
	if err != nil {
		return state.Move{}, err
	}
	return e.Journal.PlanMove(ctx, state.MovePlan{
		Artifact:          p.Artifact,
		SourceMedium:      src.Medium,
		DestinationMedium: p.DestinationMedium,
		DestinationKey:    key,
		OccurredAt:        e.now(),
	})
}

// eligibleSource applies FR-30's move-eligibility rules and returns the
// placement a move would copy FROM.
//
// Every refusal here preserves the source, which is the whole point: the
// engine declines rather than guessing, and declining costs a cycle.
func (e *Engine) eligibleSource(rec state.Record, destination string) (state.Placement, error) {
	if lifecycle.State(rec.State) != lifecycle.Complete {
		// FR-30: only COMPLETE is move-eligible. COMMITTED and
		// REMOTE_DELETE_PENDING still owe FR-15 its pre-delete local-file
		// checks, and a move racing those checks is a bug this rule makes
		// unrepresentable.
		return state.Placement{}, fmt.Errorf(
			"%w: %s is %s, and only COMPLETE artifacts may move (FR-15 still owes the others its pre-delete checks)",
			ErrNotEligible, rec.Artifact, rec.State)
	}

	var active []state.Placement
	for _, p := range rec.Placements {
		if p.Status == state.PlacementActive {
			active = append(active, p)
		}
	}
	if len(active) == 0 {
		// The conservative failure the whole design turns on. A placement
		// row means a durable copy; a still-transferring artifact's
		// .partial deliberately has no row. If there is no row, this
		// engine cannot say where the bytes are, and it must decline
		// rather than infer a placement from LocalPath.
		return state.Placement{}, fmt.Errorf(
			"%w: %s has no ACTIVE placement, so nothing here can say where its bytes are; refusing to infer one",
			ErrNotEligible, rec.Artifact)
	}
	for _, p := range active {
		if p.Medium == destination {
			return state.Placement{}, fmt.Errorf(
				"%w: %s already has an ACTIVE copy on %q", ErrNotEligible, rec.Artifact, destination)
		}
	}
	if len(active) > 1 {
		return state.Placement{}, fmt.Errorf(
			"%w: %s has %d ACTIVE placements, and this engine will not choose which of them is disposable",
			ErrNotEligible, rec.Artifact, len(active))
	}

	src := active[0]
	if src.Location == "" {
		return state.Placement{}, fmt.Errorf(
			"%w: %s's placement on %q records no location", ErrNotEligible, rec.Artifact, src.Medium)
	}
	if src.Hash == "" || !strings.EqualFold(src.HashAlg, string(transport.SHA256)) {
		// Verification of the destination compares against the hash the
		// journal recorded for this artifact. With no recorded SHA-256
		// there is nothing to compare against, so the move could never
		// reach VERIFIED and would have copied bytes for nothing. Refusing
		// here rather than at VERIFYING is the difference between a
		// declined plan and an abandoned move with an orphan to clean up.
		return state.Placement{}, fmt.Errorf(
			"%w: %s's placement on %q records no %s hash, so a destination copy could never be content-verified against it",
			ErrNotEligible, rec.Artifact, src.Medium, transport.SHA256)
	}
	if src.Medium != config.MediumLocal && destination != config.MediumLocal {
		// Medium to medium would need a local staging copy and a second
		// set of failure modes. FR-27's home rule only ever produces
		// local-to-medium and medium-to-local, so this is refused rather
		// than half-built.
		return state.Placement{}, fmt.Errorf(
			"%w: moving %s from %q to %q is medium-to-medium, which this engine does not do",
			ErrNotEligible, rec.Artifact, src.Medium, destination)
	}
	return src, nil
}

// destinationLocator computes where the destination copy goes: FR-28's
// deterministic key on a medium, or the backup set's own final path for a
// move back to local.
//
// Deterministic is the load-bearing word. Nothing here carries a timestamp
// or a random component, so an interrupted copy that resumes targets the
// same object and converges instead of leaving a second one behind.
func (e *Engine) destinationLocator(rec state.Record, destination string) (string, error) {
	if destination == config.MediumLocal {
		bs, err := e.backupSet(rec.Artifact.Set)
		if err != nil {
			return "", err
		}
		return localArtifactPath(bs, rec.Artifact)
	}
	medium, _, err := e.resolve(destination)
	if err != nil {
		return "", err
	}
	return transport.MediumKey(medium.Prefix, rec.Artifact)
}

// localArtifactPath asks the backup set's own Local store where an
// artifact belongs, and it is the only way this package computes a local
// path. Both callers are one step from a delete: destinationLocator names
// the object a move back to local lands on, and proveLocalSourceSafe names
// the path the source delete is allowed to consider.
//
// It asks a store rather than joining a root onto a name because issue
// #390 removed artifactstore.LocalLocator, the exported free function this
// package originally called. That removal is the point of the seam: a
// caller that composes the path itself is a second implementation that can
// drift from the store's, and this package is the last one that should
// have one. #334's package doc named the two conversions #390 did, this is
// the third, and there is now no way to address a local artifact except
// through a Local.
//
// The error is real rather than ceremony. NewLocal refuses an empty root,
// so a backup set with no configured local_path now fails here instead of
// producing the artifact's bare name, which is a path relative to whatever
// directory the daemon started in. config.Validate refuses that
// configuration, so no cycle can reach it, but a move engine that would
// have deleted a source against a relative path should say so rather than
// rely on validation upstream.
func localArtifactPath(bs config.BackupSet, artifact model.ArtifactID) (string, error) {
	store, err := artifactstore.NewLocal(bs.LocalPath)
	if err != nil {
		return "", fmt.Errorf("placement: backup set %s: %w", bs.ID, err)
	}
	return store.Locator(artifact)
}

func (e *Engine) resolve(id string) (transport.Medium, Class, error) {
	if e.Mediums == nil {
		return transport.Medium{}, "", fmt.Errorf("placement: no medium resolver is configured, so %q cannot be reached", id)
	}
	return e.Mediums.Resolve(id)
}

func (e *Engine) backupSet(id model.BackupSetID) (config.BackupSet, error) {
	if e.Sets == nil {
		return config.BackupSet{}, fmt.Errorf("placement: no backup-set resolver is configured, so %s has no known local root", id)
	}
	return e.Sets.Set(id)
}

// advance drives one move from wherever it is to a terminal phase.
//
// This is the whole driver. A move found at COPYING by a restart and a
// move that reached COPYING a microsecond ago take the same branch, which
// is what makes the crash suite a test of the product rather than of a
// harness. See the file comment on #372.
func (e *Engine) advance(ctx context.Context, mv state.Move, resumed bool) Outcome {
	out := Outcome{Artifact: mv.Artifact, MoveID: mv.ID, Resumed: resumed}
	attempts := 0

	for step := 0; step < maxPhaseStepsPerMove; step++ {
		phase := Phase(mv.Phase)
		out.Phase = phase
		if IsTerminal(phase) {
			if phase == Abandoned {
				out.Refused = mv.Error
			}
			return out
		}

		var next state.Move
		var err error
		switch phase {
		case Planned:
			next, err = e.startCopy(ctx, mv)
		case Copying:
			attempts++
			if attempts > e.maxCopyAttempts() {
				// The reason the last attempt failed is carried forward
				// rather than replaced. "It was tried twice" explains the
				// budget and nothing else, and an operator reading an
				// ABANDONED row needs to know WHAT went wrong, which is
				// the sentence the verification wrote.
				next, err = e.abandon(ctx, mv, fmt.Sprintf(
					"%s; giving up after %d copy attempts in one cycle, with the source untouched",
					strings.TrimSuffix(mv.Error, "."), attempts-1))
				break
			}
			next, err = e.copy(ctx, mv)
		case Copied:
			next, err = e.startVerify(ctx, mv)
		case Verifying:
			next, err = e.verifyDestination(ctx, mv)
		case Verified:
			next, err = e.intendSourceDelete(ctx, mv)
		case SourceDeletePending:
			next, err = e.deleteSource(ctx, mv)
		default:
			// Unreachable while the switch covers NonTerminalPhases, which
			// TestEveryNonTerminalPhaseHasAResumeCase proves it does.
			// Reaching it anyway is a phase nothing can move, which is the
			// #372 shape, so it stops loudly instead of quietly.
			out.Refused = fmt.Sprintf("placement: move %d is at phase %q, which this engine has no case for", mv.ID, mv.Phase)
			return out
		}
		if err != nil {
			out.Refused = err.Error()
			return out
		}
		mv = next
	}

	out.Phase = Phase(mv.Phase)
	out.Refused = fmt.Sprintf("placement: move %d took more than %d phase steps in one cycle and was left at %s", mv.ID, maxPhaseStepsPerMove, mv.Phase)
	return out
}

// startCopy records the intent to copy, before a byte moves.
func (e *Engine) startCopy(ctx context.Context, mv state.Move) (state.Move, error) {
	rec, err := e.Journal.Get(ctx, mv.Artifact)
	if err != nil {
		return mv, err
	}
	// Eligibility is re-checked here, not only at plan time, because a
	// resumed move was planned by a process that is gone and whose reasons
	// cannot be inspected. If the artifact stopped being movable in the
	// meantime, the move is abandoned with nothing copied.
	if _, err := e.eligibleSource(rec, mv.DestinationMedium); err != nil {
		return e.abandon(ctx, mv, err.Error())
	}
	return e.step(ctx, mv, Planned, Copying, "")
}

// copy performs the one side effect of the copy phase, having already
// durably recorded that it was going to.
func (e *Engine) copy(ctx context.Context, mv state.Move) (state.Move, error) {
	rec, err := e.Journal.Get(ctx, mv.Artifact)
	if err != nil {
		return mv, err
	}
	src, ok := placementOn(rec, mv.SourceMedium)
	if !ok || src.Location == "" {
		return e.abandon(ctx, mv, fmt.Sprintf(
			"%s no longer has a placement on %q, so there is nothing to copy from", mv.Artifact, mv.SourceMedium))
	}

	var bytes int64
	switch mv.DestinationMedium {
	case config.MediumLocal:
		bytes, err = e.copyToLocal(ctx, mv, src)
	default:
		bytes, err = e.copyToMedium(ctx, mv, src)
	}
	if err != nil {
		return mv, fmt.Errorf("placement: copying %s to %q: %w", mv.Artifact, mv.DestinationMedium, err)
	}

	advance := state.MoveAdvance{
		MoveID: mv.ID, From: state.MoveCopying, To: state.MoveCopied,
		OccurredAt: e.now(), BytesCopied: &bytes,
	}
	return e.Journal.AdvanceMove(ctx, e.checked(advance))
}

func (e *Engine) copyToMedium(ctx context.Context, mv state.Move, src state.Placement) (int64, error) {
	if e.Store == nil {
		return 0, fmt.Errorf("no medium store is configured")
	}
	medium, _, err := e.resolve(mv.DestinationMedium)
	if err != nil {
		return 0, err
	}
	res, err := e.Store.UploadFromLocal(ctx, medium, src.Location, mv.DestinationKey, transport.UploadOptions{})
	if err != nil {
		return 0, err
	}
	return res.BytesUploaded, nil
}

// copyToLocal is the reverse direction: a medium placement coming back to
// the backup set's own root.
//
// artifactstore.Local.Put is what makes it durable and atomic (temp file,
// hard link that refuses to clobber, directory fsync). ErrAlreadyPresent
// is convergence rather than a failure: an interrupted copy that already
// linked the final name has done its job, and the verify phase is what
// decides whether those bytes are the artifact.
func (e *Engine) copyToLocal(ctx context.Context, mv state.Move, src state.Placement) (int64, error) {
	if e.Local == nil {
		return 0, fmt.Errorf("no local store is configured")
	}
	if e.Store == nil {
		return 0, fmt.Errorf("no medium store is configured")
	}
	medium, _, err := e.resolve(src.Medium)
	if err != nil {
		return 0, err
	}
	rc, err := e.Store.OpenObject(ctx, medium, src.Location)
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	if err := e.Local.Put(ctx, mv.DestinationKey, rc); err != nil && !errors.Is(err, artifactstore.ErrAlreadyPresent) {
		return 0, err
	}
	st, err := e.Local.Stat(ctx, mv.DestinationKey)
	if err != nil {
		return 0, err
	}
	if st.Size == nil {
		return 0, fmt.Errorf("the local store reported no size for %q", mv.DestinationKey)
	}
	return *st.Size, nil
}

// startVerify records the intent to verify, before the egress it costs.
func (e *Engine) startVerify(ctx context.Context, mv state.Move) (state.Move, error) {
	return e.step(ctx, mv, Copied, Verifying, "")
}

// verifyDestination runs the class the destination requires and, on a
// pass, writes VERIFIED together with the destination's placements row in
// one transaction.
//
// On a failure or a capability refusal it goes the other way: the
// destination object is deleted and the move returns to COPYING with the
// source untouched. FR-31's rule is verbatim here, and it is the reason
// Verify is called with exactly one class and never with a fallback: a
// medium configured for attested against an endpoint that cannot attest
// gets ErrClassUnavailable and this move refuses, rather than quietly
// verifying less than the operator asked for.
func (e *Engine) verifyDestination(ctx context.Context, mv state.Move) (state.Move, error) {
	rec, err := e.Journal.Get(ctx, mv.Artifact)
	if err != nil {
		return mv, err
	}
	src, ok := placementOn(rec, mv.SourceMedium)
	if !ok {
		return e.abandon(ctx, mv, fmt.Sprintf(
			"%s no longer has a placement on %q, so there is no recorded hash to verify the destination against", mv.Artifact, mv.SourceMedium))
	}

	result, want, err := e.verifyCopy(ctx, mv, src)
	if err != nil {
		return e.recopyOrAbandon(ctx, mv, Verifying, fmt.Sprintf(
			"the destination copy on %q could not be verified at %s class: %v", mv.DestinationMedium, want, err))
	}
	if !result.Passed {
		return e.recopyOrAbandon(ctx, mv, Verifying, fmt.Sprintf(
			"the destination copy on %q failed %s verification: %s", mv.DestinationMedium, result.Class, result.Detail))
	}

	return e.Journal.AdvanceMove(ctx, e.checked(state.MoveAdvance{
		MoveID: mv.ID, From: state.MoveVerifying, To: state.MoveVerified,
		OccurredAt: e.now(),
		Placements: []state.PlacementUpdate{e.destinationPlacement(mv, src, result)},
	}))
}

// verifyCopy verifies whatever is at the destination against the hash the
// journal recorded for the source, at the class the destination requires.
func (e *Engine) verifyCopy(ctx context.Context, mv state.Move, src state.Placement) (Result, Class, error) {
	candidate := state.Placement{
		Medium:   mv.DestinationMedium,
		Location: mv.DestinationKey,
		Size:     src.Size,
		Hash:     src.Hash,
		HashAlg:  src.HashAlg,
	}
	if mv.DestinationMedium == config.MediumLocal {
		res, err := e.verifyLocalCopy(ctx, candidate)
		return res, Content, err
	}
	medium, want, err := e.resolve(mv.DestinationMedium)
	if err != nil {
		return Result{}, want, err
	}
	if e.Store == nil {
		return Result{}, want, fmt.Errorf("no medium store is configured")
	}
	res, err := Verify(ctx, e.Store, medium, candidate, want, e.now())
	return res, want, err
}

// verifyLocalCopy is the content check for a copy that came back to local
// disk: read it and hash it.
//
// It is spelled here rather than routed through Verify because Verify's
// Store is a medium store and a local placement is not on one. The class
// it produces is Content and can be nothing else, which is why it takes no
// class argument: there is no cheaper way to look at a local file that
// would prove anything about its bytes.
func (e *Engine) verifyLocalCopy(ctx context.Context, p state.Placement) (Result, error) {
	if e.Local == nil {
		return Result{}, fmt.Errorf("%w: no local store is configured", ErrClassUnavailable)
	}
	rc, err := e.Local.Open(ctx, p.Location)
	if err != nil {
		return Result{}, fmt.Errorf("%w: reading %q back: %w", ErrClassUnavailable, p.Location, err)
	}
	defer rc.Close()

	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return Result{}, fmt.Errorf("%w: reading %q back: %w", ErrClassUnavailable, p.Location, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	now := e.now()
	if !strings.EqualFold(sum, p.Hash) {
		return Result{Class: Content, Passed: false, At: now, Detail: fmt.Sprintf(
			"%s hashes to %s, but the hash recorded at ingestion was %s", p.Location, sum, p.Hash)}, nil
	}
	return Result{Class: Content, Passed: true, At: now, Detail: fmt.Sprintf(
		"%s was read back and still hashes to the %s recorded at ingestion", p.Location, p.HashAlg)}, nil
}

func (e *Engine) destinationPlacement(mv state.Move, src state.Placement, result Result) state.PlacementUpdate {
	verified := result.At
	size := src.Size
	if mv.BytesCopied != nil {
		copied := *mv.BytesCopied
		size = &copied
	}
	return state.PlacementUpdate{
		Medium:            mv.DestinationMedium,
		Location:          mv.DestinationKey,
		Size:              size,
		Hash:              src.Hash,
		HashAlg:           src.HashAlg,
		VerificationClass: string(result.Class),
		VerifiedAt:        &verified,
		Status:            state.PlacementActive,
	}
}

// intendSourceDelete records the decision to delete the source, and marks
// the source placement DELETE_PENDING, before anything is deleted.
//
// This is the transposition of COMMITTED -> REMOTE_DELETE_PENDING, and it
// is the same promise: a crash between this write and the delete leaves a
// journal that says outright "a delete was decided here", which
// reconciliation can act on, instead of a source that is missing for no
// recorded reason.
func (e *Engine) intendSourceDelete(ctx context.Context, mv state.Move) (state.Move, error) {
	rec, err := e.Journal.Get(ctx, mv.Artifact)
	if err != nil {
		return mv, err
	}
	src, ok := placementOn(rec, mv.SourceMedium)
	if !ok {
		return mv, fmt.Errorf("placement: %s has no placement on %q to delete", mv.Artifact, mv.SourceMedium)
	}
	return e.Journal.AdvanceMove(ctx, e.checked(state.MoveAdvance{
		MoveID: mv.ID, From: state.MoveVerified, To: state.MoveSourceDeletePending,
		OccurredAt: e.now(),
		Placements: []state.PlacementUpdate{src.Update().WithStatus(state.PlacementDeletePending)},
	}))
}

// deleteSource is the dangerous one.
//
// It re-verifies the destination from scratch, does NOT write that fresh
// result anywhere, re-derives every precondition from the durable journal
// and the real filesystem, and only then removes the source copy.
//
// That "does NOT" was the wrong way round here until now: this comment used
// to say the fresh result is written into the destination's placement "so
// the guard below reads a journal fact newer than the check that justifies
// it", which is the exact mistake the comment in the body of the function
// exists to argue against, and it is the one a reader hits first. Two
// structural tests in destructive_test.go now hold the code to the body's
// version, because a rule that lives only in a comment is a rule two
// comments can disagree about.
//
// On a destination that fails re-verification it goes back to COPYING with
// the source placement restored to ACTIVE, which is FR-30's own restart
// answer, and it restores the source BEFORE it touches the destination, so
// there is no instant at which the journal says both copies are
// disposable.
func (e *Engine) deleteSource(ctx context.Context, mv state.Move) (state.Move, error) {
	rec, err := e.Journal.Get(ctx, mv.Artifact)
	if err != nil {
		return mv, err
	}
	src, ok := placementOn(rec, mv.SourceMedium)
	if !ok {
		return mv, fmt.Errorf("placement: %s has no placement on %q to delete", mv.Artifact, mv.SourceMedium)
	}

	result, want, err := e.verifyCopy(ctx, mv, src)
	switch {
	case err != nil:
		return e.recopyOrAbandon(ctx, mv, SourceDeletePending, fmt.Sprintf(
			"the destination copy on %q could not be re-verified at %s class immediately before the source delete: %v",
			mv.DestinationMedium, want, err))
	case result.Class != want:
		// Unreachable through Verify, which has no path returning a class
		// it did not run. Kept as its own refusal because the one thing
		// this engine must never do is delete a source on the strength of
		// a check that was not the one the medium requires.
		return e.recopyOrAbandon(ctx, mv, SourceDeletePending, fmt.Sprintf(
			"the re-verification before the source delete ran at %s class and this medium requires %s", result.Class, want))
	case !result.Passed:
		return e.recopyOrAbandon(ctx, mv, SourceDeletePending, fmt.Sprintf(
			"the destination copy on %q failed %s re-verification immediately before the source delete: %s",
			mv.DestinationMedium, result.Class, result.Detail))
	}

	// The fresh result is deliberately NOT written back over the
	// destination's placement here.
	//
	// That would be the natural thing to do and it would quietly destroy
	// the guard below. The guard's job is to require DURABLE evidence, the
	// placement the VERIFIED write recorded, and if this function refreshed
	// that row from the check it just ran then every one of the guard's
	// destination clauses would be satisfied by construction and none of
	// them could ever fire. Two independent conditions are the point: what
	// the journal durably recorded when it authorised this delete, and what
	// is true about the destination right now. Both have to hold.
	target, err := e.guardSourceDelete(ctx, mv, rec, want)
	switch {
	case errors.Is(err, errSourceAlreadyGone):
		// The delete already happened and the process died before it could
		// say so. Recording DONE below is the whole remaining work.
	case err != nil:
		return mv, err
	default:
		if err := e.remove(ctx, target); err != nil {
			return mv, fmt.Errorf("placement: deleting %s's source copy on %q: %w", mv.Artifact, mv.SourceMedium, err)
		}
	}

	src, _ = placementOn(rec, mv.SourceMedium)
	return e.Journal.AdvanceMove(ctx, e.checked(state.MoveAdvance{
		MoveID: mv.ID, From: state.MoveSourceDeletePending, To: state.MoveDone,
		OccurredAt: e.now(),
		Placements: []state.PlacementUpdate{src.Update().WithStatus(state.PlacementGone)},
	}))
}

// recopyOrAbandon takes the destination away and either copies again or
// gives up, with the source restored to ACTIVE first in either case.
func (e *Engine) recopyOrAbandon(ctx context.Context, mv state.Move, from Phase, why string) (state.Move, error) {
	rec, err := e.Journal.Get(ctx, mv.Artifact)
	if err != nil {
		return mv, err
	}

	// Restore the source before anything else. If this write is the last
	// one before a crash, the journal says the source is the good copy,
	// which is the direction every uncertainty in this engine falls.
	var restore []state.PlacementUpdate
	if src, ok := placementOn(rec, mv.SourceMedium); ok && src.Status != state.PlacementActive {
		restore = append(restore, src.Update().WithStatus(state.PlacementActive))
	}
	if dst, ok := placementOn(rec, mv.DestinationMedium); ok && dst.Status == state.PlacementActive {
		restore = append(restore, dst.Update().WithStatus(state.PlacementDeletePending))
	}

	mv, err = e.Journal.AdvanceMove(ctx, e.checked(state.MoveAdvance{
		MoveID: mv.ID, From: string(from), To: state.MoveCopying,
		OccurredAt: e.now(), Error: why, Placements: restore,
	}))
	if err != nil {
		return mv, err
	}
	if err := e.discardDestination(ctx, mv); err != nil {
		return mv, err
	}
	return mv, nil
}

// abandon cleans up the destination and leaves the source exactly where it
// was. It is the only terminal outcome other than DONE.
func (e *Engine) abandon(ctx context.Context, mv state.Move, why string) (state.Move, error) {
	rec, err := e.Journal.Get(ctx, mv.Artifact)
	if err != nil {
		return mv, err
	}
	var restore []state.PlacementUpdate
	if src, ok := placementOn(rec, mv.SourceMedium); ok && src.Status != state.PlacementActive {
		restore = append(restore, src.Update().WithStatus(state.PlacementActive))
	}

	mv, err = e.Journal.AdvanceMove(ctx, e.checked(state.MoveAdvance{
		MoveID: mv.ID, From: mv.Phase, To: state.MoveAbandoned,
		OccurredAt: e.now(), Error: why, Placements: restore,
	}))
	if err != nil {
		return mv, err
	}
	if err := e.discardDestination(ctx, mv); err != nil {
		return mv, err
	}
	return mv, nil
}

// discardDestination removes whatever the move put at the destination, and
// marks its placement GONE if it had earned one.
//
// It never touches the source, and it is the only delete in this file that
// is not guarded by guardSourceDelete, because the thing it deletes is the
// object this move itself created at a key this move itself computed.
func (e *Engine) discardDestination(ctx context.Context, mv state.Move) error {
	if mv.DestinationKey == "" {
		return nil
	}
	if mv.DestinationMedium == config.MediumLocal {
		if e.Local == nil {
			return nil
		}
		if err := e.Local.Remove(ctx, mv.DestinationKey); err != nil {
			return fmt.Errorf("placement: discarding %s's destination copy at %q: %w", mv.Artifact, mv.DestinationKey, err)
		}
	} else {
		medium, _, err := e.resolve(mv.DestinationMedium)
		if err != nil {
			return err
		}
		if e.Store == nil {
			return nil
		}
		if err := e.Store.DeleteObject(ctx, medium, mv.DestinationKey); err != nil {
			return fmt.Errorf("placement: discarding %s's destination copy on %q: %w", mv.Artifact, mv.DestinationMedium, err)
		}
	}

	rec, err := e.Journal.Get(ctx, mv.Artifact)
	if err != nil {
		return err
	}
	dst, ok := placementOn(rec, mv.DestinationMedium)
	if !ok || dst.Status == state.PlacementGone {
		return nil
	}
	_, err = e.Journal.AdvanceMove(ctx, e.checked(state.MoveAdvance{
		MoveID: mv.ID, From: mv.Phase, To: mv.Phase, OccurredAt: e.now(), Error: mv.Error,
		Placements: []state.PlacementUpdate{dst.Update().WithStatus(state.PlacementGone)},
	}))
	return err
}

// step is a phase write with no placement change.
func (e *Engine) step(ctx context.Context, mv state.Move, from, to Phase, why string) (state.Move, error) {
	return e.Journal.AdvanceMove(ctx, e.checked(state.MoveAdvance{
		MoveID: mv.ID, From: string(from), To: string(to), OccurredAt: e.now(), Error: why,
	}))
}

// checked runs every advance past phases.go's table before it is written.
//
// The table is the readable statement of the ordering and
// state.AdvanceMove's WHERE clause is the wall underneath it; this is the
// third place, and it is the one that turns an illegal change into a
// refusal at the call site rather than a silent no-op at the database. An
// advance the table refuses is turned into an impossible From, so the
// write cannot land.
func (e *Engine) checked(a state.MoveAdvance) state.MoveAdvance {
	if err := ValidatePhaseChange(Phase(a.From), Phase(a.To)); err != nil {
		a.From = "!" + a.From
		a.Error = err.Error()
	}
	return a
}

// deleteTarget names exactly what a source delete will remove, after the
// guard has proved it safe. Exactly one of its two halves is set.
type deleteTarget struct {
	localPath string

	medium transport.Medium
	key    string
}

func (e *Engine) remove(ctx context.Context, t deleteTarget) error {
	if t.localPath != "" {
		if e.Local == nil {
			return fmt.Errorf("no local store is configured")
		}
		return e.Local.Remove(ctx, t.localPath)
	}
	if e.Store == nil {
		return fmt.Errorf("no medium store is configured")
	}
	return e.Store.DeleteObject(ctx, t.medium, t.key)
}

// copiesOf is every placement the journal holds for this artifact, as
// internal/archive's delete decision sees them: the row, the storage class
// of the medium it names, and what can be done with it RIGHT NOW.
//
// The third field is the whole point and the reason this cannot be a
// projection of the record. A row says what was true when it was written;
// whether the bytes it describes can be read today depends on the class
// the medium writes with and on whether a restore is in effect, and the
// second of those lives at the endpoint. So an ACTIVE copy on an archive
// class is asked about, through observe, before it is allowed to count.
//
// A copy on a medium the configuration no longer declares is unreachable:
// there is no bucket, no endpoint and no credential to reach it with, so
// nothing here can say it is readable, and archive's decision will not let
// it stand in for anything. That is deliberately a state and not a
// refusal, because the copy being DELETED may be the one on the forgotten
// medium, and its own reachability is not the question. A class the table
// does not know IS a refusal: config validation refuses it at load, so it
// is drift between two lists, and the copy it describes might be the one
// that has to stand in.
func (e *Engine) copiesOf(ctx context.Context, rec state.Record) ([]archive.Copy, error) {
	now := e.now()
	out := make([]archive.Copy, 0, len(rec.Placements))
	for _, p := range rec.Placements {
		c := archive.Copy{Placement: p}
		var obs archive.Observation
		if !p.IsLocal() {
			medium, _, err := e.resolve(p.Medium)
			if err != nil {
				c.Access = archive.Unreachable
				out = append(out, c)
				continue
			}
			c.Class = medium.StorageClass
			if p.Status == state.PlacementActive {
				// Only a copy the journal still believes in can stand in
				// for another, so only such a copy is worth a request.
				obs = e.observe(ctx, medium, p.Location, c.Class)
			}
		}
		access, err := archive.Access(p.Medium, c.Class, obs, now)
		if err != nil {
			return nil, fmt.Errorf("the copy on %q is on storage class %q, which this build does not recognise, so nothing here can say whether it is readable: %w", p.Medium, c.Class, err)
		}
		c.Access = access
		out = append(out, c)
	}
	return out, nil
}

// observe asks the medium whether a restore of one copy is in effect,
// when and only when the copy's class needs one to be readable.
//
// A non-archive class serves objects on demand, and archive.Access does
// not read the restore status for one; asking anyway would add a failure
// mode (a status endpoint that did not answer) to a copy whose
// readability does not depend on the answer. A class the table does not
// know is treated as archive, which is archive.IsArchive's own safe
// direction, and archive.Access then refuses it by name.
//
// A store that cannot be asked, or that did not answer, is reported as
// exactly that. Nothing here guesses: NotAsked reads as requires_restore
// and DidNotAnswer reads as unreachable, and neither can stand in for a
// copy that is about to be deleted.
func (e *Engine) observe(ctx context.Context, medium transport.Medium, key, class string) archive.Observation {
	if !archive.IsArchive(class) || e.Store == nil {
		return archive.Observation{}
	}
	restore, err := e.Store.RestoreStatus(ctx, medium, key)
	if err != nil {
		return archive.Observation{Probe: archive.DidNotAnswer}
	}
	return archive.Observation{Probe: archive.Answered, Restore: restore}
}

// placementOn returns the artifact's placement on one medium, whatever its
// status.
func placementOn(rec state.Record, medium string) (state.Placement, bool) {
	for _, p := range rec.Placements {
		if p.Medium == medium {
			return p, true
		}
	}
	return state.Placement{}, false
}

// CheckInvariant is FR-30's standing invariant, evaluated against one
// record: a managed-complete artifact has at least one ACTIVE placement at
// a sufficient verification class, at every instant.
//
// sufficient defaults to Content alone, which is what FR-30 means by
// "read-back or better". A caller passes something else only for a medium
// whose operator opted into attested through upload_verification, and that
// is the one place FR-30's invariant and FR-31's opt-in genuinely pull
// against each other: FR-31 says an operator may accept the endpoint's own
// checksum, and FR-31 also says outright what that costs ("an endpoint
// that lies about checksums can then cause the local copy to be deleted
// against a bad upload"). Making the relaxation an explicit argument means
// a surface that reports the invariant can say which standard it was held
// to, instead of quietly holding a weaker one.
//
// An artifact that is not a managed-complete restore point is not in
// scope: an artifact still being transferred has no durable copy yet, and
// that is a fact about its lifecycle rather than a broken invariant.
func CheckInvariant(rec state.Record, sufficient ...Class) error {
	if !lifecycle.IsDurableRestorePoint(lifecycle.State(rec.State)) {
		return nil
	}
	if len(sufficient) == 0 {
		sufficient = []Class{Content}
	}
	ok := map[string]bool{}
	for _, c := range sufficient {
		ok[string(c)] = true
	}
	for _, p := range rec.Placements {
		if p.Status == state.PlacementActive && ok[p.VerificationClass] {
			return nil
		}
	}
	names := make([]string, 0, len(sufficient))
	for _, c := range sufficient {
		names = append(names, string(c))
	}
	return fmt.Errorf(
		"placement: %s is %s and has no ACTIVE placement at %s class: %s",
		rec.Artifact, rec.State, strings.Join(names, " or "), describePlacements(rec))
}

func describePlacements(rec state.Record) string {
	if len(rec.Placements) == 0 {
		return "it has no placements at all"
	}
	parts := make([]string, 0, len(rec.Placements))
	for _, p := range rec.Placements {
		class := p.VerificationClass
		if class == "" {
			class = "unverified"
		}
		parts = append(parts, fmt.Sprintf("%s=%s/%s", p.Medium, p.Status, class))
	}
	return strings.Join(parts, ", ")
}
