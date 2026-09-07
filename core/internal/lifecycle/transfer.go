package lifecycle

// This file is FR-11 and FR-12's transfer step: DISCOVERED -> TRANSFERRING ->
// TRANSFERRED. Everything before DISCOVERED (discovery itself) and
// everything after TRANSFERRED (verification, durable commit, remote
// deletion) belongs to other steps; this one only owns getting bytes from
// the remote onto local disk under a name nothing can mistake for a
// finished restore point.
//
// # Why copy, never move
//
// transport.Transport has no Move method at all (see transport.go's package
// doc), so there is no shortcut to reach for here even by accident. Copy,
// verify, durable-commit and delete stay four separately owned steps, and
// this file only ever calls Transport.CopyToLocal. Source deletion is a
// later step's job, invoked only after REMOTE_DELETE_PENDING is durably
// recorded; it is not this file's concern at all.
//
// # Naming, FR-12
//
// The local destination this step writes to is always the artifact's final
// basename with ".partial" appended, e.g. backup-2026-08-27.dump.zst ->
// backup-2026-08-27.dump.zst.partial. That name is deliberately
// non-restorable: nothing downstream (retention, last-known-good, a restore
// operation) has any business treating a .partial file as a valid backup,
// and giving it a name a human or a script can recognise on sight is part
// of enforcing that, not just a cosmetic choice.
//
// # The final-name collision guard
//
// Before this step writes anything at all, it stats the artifact's final
// (non-.partial) path. If something is already there, the transfer refuses
// outright: it never starts the copy, never touches the existing file, and
// records the refusal as a loud, durable FAILED transition rather than a
// silent skip. See FinalNameCollisionError and TestFinalNameCollision* in
// transfer_test.go. This has to happen here, before TRANSFERRING is even
// recorded, precisely because by the time this step's own COMMITTING
// counterpart would rename .partial to that final name, the honest and
// safe answer is "refuse, don't overwrite a possible known-good backup".
// Checking early means a doomed transfer never spends bandwidth (and,
// because rclone's Go API talks straight to memory rather than a
// subprocess, no attacker-controlled shell string ever gets a chance to
// build a filename either).
//
// # Orphaned .partial on restart, a deliberate answer
//
// The crash matrix can kill this process at any point during the copy,
// leaving a .partial file on disk that is either empty, truncated, or (in
// principle) a fresh full copy that just hasn't been recorded yet. FR-12
// says a .partial can never be a restore point, which means it also carries
// no value worth preserving across a restart: this step always removes
// whatever is at the .partial path, if anything, immediately before
// starting a fresh copy into it (removePartial below). That is a deliberate
// choice, not an oversight, for two reasons:
//
//   - A stale .partial must never silently block a later attempt. If this
//     step instead treated "a .partial already exists" as "someone else is
//     already handling this" or "this must already be done", a crash mid-
//     copy would turn into a permanently stuck artifact: a quiet outage
//     that only surfaces the next time someone needs the restore that never
//     happened.
//   - rclone copy's own overwrite behaviour for a destination file is an
//     implementation detail of transport/rclone, which this package does
//     not import and must not rely on (see transport.go's package doc).
//     Clearing the destination here makes "start clean" true regardless of
//     what CopyToLocal would otherwise have done.
//
// This is safe to repeat on every retry: TRANSFERRING is idempotent per the
// caller's AttemptKey (see Transfer's doc), and a half-written .partial
// carries no commitment the journal has made to anyone.
//
// # Cancellation
//
// retry.Do already races a Transient backoff wait against ctx.Done() and
// reports a cancellation as transport.Cancelled (see retry.go). Transfer
// treats that case specially: it returns the error without ever recording
// TRANSFERRED, and without recording FAILED either. The journal is left
// exactly where it honestly is, TRANSFERRING, because a cancellation is an
// external stop request, not a verdict that this artifact is broken; a
// later call with the same AttemptKey resumes cleanly, discarding whatever
// the cancelled attempt left in the .partial file per the rule above.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// partialSuffix is FR-12's non-restorable temporary-name marker. Appended
// verbatim to the artifact's final basename, never inserted anywhere else,
// so a directory listing makes the in-flight files obvious at a glance:
// backup-2026-08-27.dump.zst.partial next to backup-2026-08-27.dump.zst.
const partialSuffix = ".partial"

// Key suffixes for the transitions Transfer records. Appending a fixed,
// distinct suffix to the caller's AttemptKey for each transition means a
// resumed call (same AttemptKey, because the caller is retrying the same
// logical attempt) reproduces the same keys and gets the journal's
// exactly-once guarantee on each one individually, while the different
// suffixes keep a FAILED-via-collision key from ever colliding with a
// FAILED-via-copy-error key for the same attempt.
const (
	keyTransferringSuffix    = ":transferring"
	keyTransferredSuffix     = ":transferred"
	keyFailedCollisionSuffix = ":failed:collision"
	keyFailedCopySuffix      = ":failed:copy"
)

// TransferParams is what Transfer needs beyond Deps: the artifact being
// moved, where its bytes come from, and where they land.
type TransferParams struct {
	// Artifact is the artifact to transfer. It must already have a
	// DISCOVERED (or later, for a resumed call) journal row.
	Artifact model.ArtifactID

	// Source is the remote this artifact's bytes are copied from.
	Source transport.Source

	// LocalDir is the backup set's configured local destination directory
	// (config.BackupSet.LocalPath). The final path is LocalDir joined with
	// the artifact's own basename; Transfer never uses any other directory
	// and never lets the artifact name escape it, because
	// model.NewArtifactID already refuses a name containing a path
	// separator or a ".." segment.
	LocalDir string

	// AttemptKey is the caller's idempotency key base for this logical
	// attempt (see state.Transition.Key's doc for what "logical attempt"
	// means). Transfer derives each transition's own key by appending a
	// fixed suffix to this. A caller resuming the same attempt after a
	// crash, using the same AttemptKey it used before, gets the journal's
	// exactly-once guarantee on every transition this step records; a
	// caller starting a genuinely new attempt (for example after a
	// FAILED -> DISCOVERED retry) must pass a new one, or the journal will
	// refuse it as a reused key against a different logical move
	// (ErrIdempotencyKeyReused).
	AttemptKey string

	// Policy governs retry.Do's bounded backoff over the copy itself.
	// The zero value uses retry.DefaultPolicy().
	Policy retry.Policy
}

// FinalNameCollisionError reports that Transfer refused to run because a
// file already exists at the artifact's final (non-.partial) local path.
//
// This is never resolved by overwriting whatever is there: FR-12 requires a
// final-name collision to fail safely rather than risk clobbering a
// known-good backup, and this package has no way to tell a stray file
// apart from the real thing. Only an operator, looking at the collision by
// hand, can decide what the existing file actually is.
type FinalNameCollisionError struct {
	// Artifact is which transfer was refused, and Path is the file already
	// sitting where its final copy would go. Both are needed: the artifact
	// says what did not happen, and the path is what an operator has to go
	// and look at, since deciding what that file actually is is the whole
	// reason this refuses rather than resolving it.
	Artifact model.ArtifactID
	Path     string
}

// Error names the path, which is the actionable half. This refusal is
// resolved by a person looking at a specific file and deciding whether it is
// a real backup or residue, so a message that named only the artifact would
// leave them searching for it.
func (e *FinalNameCollisionError) Error() string {
	return fmt.Sprintf(
		"lifecycle: transfer: refusing to overwrite an existing final-name file for %s at %s",
		e.Artifact, e.Path,
	)
}

// finalPath and partialPath compute FR-12's two destination names for one
// artifact under one backup set's local directory.
//
// # This asks a Store, and that is issue #334's deferred conversion
//
// internal/artifactstore landed the Store seam with no production caller at
// all, deliberately, so the contract could be argued in review before
// anything depended on it. Its package doc named the two call sites that
// would convert to it, and this is one of them (internal/retention's
// pruneFinalPath is the other). Both used the package-level LocalLocator
// function, which takes a filesystem path and therefore hard-codes the
// assumption the seam exists to remove.
//
// So this now builds the backup set's Local store and asks it where the
// artifact belongs. The store is constructed per call rather than held,
// because a Local is one string and constructing it is free; what matters
// is that the ANSWER comes from the store rather than from a join composed
// here.
//
// # Why this grew an error, and what that changed
//
// Because the conversion is not a no-op, and #334 said so in advance:
// keeping LocalLocator was what let that change be a pure refactor, and
// the behaviour it was deferring is exactly this. NewLocal refuses an
// empty root, and Locator refuses an artifact it cannot address.
//
// Before, finalPath("", artifact) returned the artifact's bare name, which
// is a path relative to the process working directory: a backup set with no
// configured local_path would have written its artifact into whatever
// directory the daemon happened to start in, silently, and nothing would
// have been backing that directory up. config.Validate refuses an empty
// local_path so no configuration that got as far as running a cycle could
// reach it, which is why this was safe to defer and not safe to leave.
//
// Artifact.Name is already validated as a plain basename
// (model.NewArtifactID refuses "/", "\\", and "." / ".."), so the join the
// store performs on the far side is safe.
func finalPath(localDir string, artifact model.ArtifactID) (string, error) {
	store, err := artifactstore.NewLocal(localDir)
	if err != nil {
		return "", err
	}
	return store.Locator(artifact)
}

// IsPartialPath reports whether path carries FR-12's non-restorable
// temporary-name marker.
//
// It is exported because the rule "a .partial is never a restore point"
// has to be checkable by a caller that is about to remove a file, and the
// suffix itself must not be spelled a second time anywhere: a constant
// copied into another package is a constant that drifts, and the failure
// mode of that drift here is a final-name backup deleted by something
// that thought it was clearing residue. internal/app's #418 sweep is the
// caller (unconfigured.go's clearOne).
func IsPartialPath(path string) bool {
	return strings.HasSuffix(path, partialSuffix)
}

// partialPath is the final path with FR-12's non-restorable suffix on the
// end.
//
// It is derived from finalPath rather than composed independently, which is
// the same reason recovery's ManifestObjectKey derives a sidecar key from
// the artifact's own key: the two names have to be siblings in the same
// directory for the commit step's hard link to work at all, and two
// independent joins are one refactor away from disagreeing about which
// directory that is.
func partialPath(localDir string, artifact model.ArtifactID) (string, error) {
	final, err := finalPath(localDir, artifact)
	if err != nil {
		return "", err
	}
	return final + partialSuffix, nil
}

// FinalArtifactPath is finalPath exported for callers outside this
// package that need to agree on exactly where Commit will have placed an
// artifact's durable local copy, without duplicating the join logic. The
// one caller today is internal/app's catalog-rebuild use case (issue
// #102), which has to compute the same LocalPath a normal commit would
// have recorded for a journal row it is reconstructing from a sidecar
// recovery manifest rather than from a live Commit call.
func FinalArtifactPath(localDir string, artifact model.ArtifactID) (string, error) {
	return finalPath(localDir, artifact)
}

// Transfer runs FR-11's copy step for one artifact: it records TRANSFERRING
// with the .partial destination, copies the remote object to that
// destination through Transport.CopyToLocal (retried under Policy for
// Transient failures per FR-22), and records TRANSFERRED on success.
//
// Transfer reads the artifact's current state from d.Journal rather than
// assuming DISCOVERED, so it is safe to call again after a crash: a call
// that finds the artifact already at TRANSFERRING resumes from there
// (discarding whatever an interrupted prior attempt left in the .partial
// file, see the package doc above), and a call that finds the artifact
// somewhere Advance does not allow this step to reach from (VERIFYING,
// COMMITTED, ...) is refused by the same state-machine validation every
// other step goes through, never silently skipped or repeated.
//
// Every transition Transfer records goes through Advance, never
// state.RecordTransition directly, so an illegal move here is refused
// before the journal is touched, exactly like every other lifecycle step.
//
// One outcome is neither a success nor a failure of this artifact, and it
// is TransferSupersededError: this attempt found that the artifact is not
// one it speaks for any more. It is reachable from all three points where
// that can be discovered, which is the whole of issue #570 and is why the
// same type covers a copy failure and a refused collision alike:
//
//   - before the copy, when this attempt's own TRANSFERRING write replays
//     over a row somebody else has already moved on;
//   - at the collision guard, when another attempt has already finished
//     the artifact and renamed its bytes to the final name (failCollision);
//   - after a failed copy, either because this attempt never held the
//     TRANSFERRING claim it would be stamping a verdict from or because the
//     row moved while the copy ran (failCopy).
//
// Nothing is written on any of them. The artifact already has a state,
// another attempt put it there, and that state is the answer both `status`
// and `artifacts` will be giving an operator, so this one names it rather
// than claiming a verdict of its own. Issue #570 is what it cost not to.
func Transfer(ctx context.Context, d Deps, p TransferParams) (state.Outcome, error) {
	if d.Journal == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer needs a Journal")
	}
	if d.Transport == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer needs a Transport")
	}
	if p.LocalDir == "" {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer needs a LocalDir")
	}
	if p.AttemptKey == "" {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer needs an AttemptKey")
	}
	if err := ctx.Err(); err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: %w", transport.NewError(transport.Cancelled, "transfer", err))
	}

	rec, err := d.Journal.Get(ctx, p.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: looking up %s: %w", p.Artifact, err)
	}

	final, err := finalPath(p.LocalDir, p.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: resolving where %s belongs: %w", p.Artifact, err)
	}
	partial, err := partialPath(p.LocalDir, p.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: resolving the .partial destination for %s: %w", p.Artifact, err)
	}

	// The collision guard runs before anything else, every time, including
	// on a resumed call: it is cheap, and re-checking on every attempt
	// catches a file that appears at the final path between attempts, not
	// only one that was already there on the first.
	if _, statErr := os.Stat(final); statErr == nil {
		return failCollision(ctx, d, p, rec.State, final)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: checking for a final-name collision at %s: %w", final, statErr)
	}

	lp := partial
	started, err := Advance(ctx, d, state.Transition{
		Artifact:  p.Artifact,
		Key:       p.AttemptKey + keyTransferringSuffix,
		From:      rec.State,
		To:        string(Transferring),
		LocalPath: &lp,
	})
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: recording TRANSFERRING: %w", err)
	}

	// Recording TRANSFERRING is not the same as the artifact BEING at
	// TRANSFERRING, and issue #570 is the gap between the two.
	//
	// The Get above and this write are two separate journal calls, and an
	// artifact that has already moved on by the time this step looks is
	// refused between them by machine.go's table, loudly and with nothing
	// written (TestTransferRefusesAnArtifactPastItsOwnStage). What is left
	// is the interleaving: a second attempt moving the row in the window
	// between the two calls. If this attempt's key is new the write itself
	// catches that, because the journal compares From against the row. If
	// the key has been seen before it does not, because the journal's
	// exactly-once guarantee makes that a replay, and a replay reports the
	// row as it stands and validates nothing, there being nothing to
	// validate about a write it is not making.
	//
	// Two attempts sharing a key is not exotic. internal/app's attemptKey
	// is the artifact plus its retry count and nothing in it distinguishes
	// two live attempts, and nothing stops two of them existing: service's
	// runOnce is an in-process lock, and `backup-manager fetch` and `run`
	// open the same journal from another process behind a SHARED one.
	//
	// Without this check the copy below would start anyway and write into
	// the directory of an artifact this attempt has no claim on any more.
	// Reading the state off the outcome rather than asking again is
	// deliberate: Outcome.Record is the row as the transaction that just
	// ran saw it, so there is no second read to be stale.
	if State(started.Record.State) != Transferring {
		return state.Outcome{}, &TransferSupersededError{Artifact: p.Artifact, Current: State(started.Record.State)}
	}

	// Deliberate cleanup of whatever a prior, interrupted attempt left
	// behind. See the package doc's "Orphaned .partial on restart" section
	// for why this always runs rather than trusting or resuming it.
	if err := os.Remove(partial); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: clearing a stale .partial at %s: %w", partial, err)
	}

	var result transport.TransferResult
	copyErr := retry.Do(ctx, p.Policy, nil, func(ctx context.Context) error {
		r, err := d.Transport.CopyToLocal(ctx, p.Source, rec.RemotePath, partial)
		if err == nil {
			result = r
		}
		return err
	})

	if copyErr != nil {
		if category, ok := transport.CategoryOf(copyErr); ok && category == transport.Cancelled {
			// Cancellation must not claim TRANSFERRED, and it is not a
			// verdict that this artifact is broken either: leave the
			// journal exactly where it honestly is (TRANSFERRING) for a
			// later retry to resume from. See the package doc.
			return state.Outcome{}, fmt.Errorf("lifecycle: transfer: cancelled: %w", copyErr)
		}
		return failCopy(ctx, d, p, copyErr, started)
	}

	// The copy itself succeeded, but ctx may have been cancelled in the
	// narrow window between CopyToLocal returning and this check. Treat
	// that exactly like a cancellation detected mid-copy: the bytes may be
	// on disk, but nothing has verified or committed them, so leaving the
	// journal at TRANSFERRING and letting a later attempt clear and redo
	// the .partial is the safe, honest outcome.
	if err := ctx.Err(); err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: cancelled: %w", transport.NewError(transport.Cancelled, "transfer", err))
	}

	out, err := Advance(ctx, d, state.Transition{
		Artifact: p.Artifact,
		Key:      p.AttemptKey + keyTransferredSuffix,
		From:     string(Transferring),
		To:       string(Transferred),
		// Checksummed is not set, and there is nothing left to set it
		// from: #492 removed the field from transport.TransferResult
		// because the only hash a copy could have compared is not the one
		// a hash policy asks for. See state.TransferResult's own doc for
		// why the column is still there.
		Transfer: &state.TransferResult{
			BytesTransferred: result.BytesTransferred,
		},
	})
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: recording TRANSFERRED: %w", err)
	}
	return out, nil
}

// failCollision records the FR-12 final-name collision as a loud, durable
// FAILED transition and returns a FinalNameCollisionError regardless of
// whether that recording succeeded. A journal that cannot even record the
// refusal is a second problem, not a reason to hide the first: both are
// folded into the returned error rather than one masking the other.
//
// From is the row's own state rather than a pinned one, and it has to be:
// this runs before anything is recorded, so there is no claim of this
// attempt's to write from and the honest predecessor is wherever the
// artifact actually is. That has a consequence worth knowing about, because
// it is the likeliest path in the whole file. A winning attempt renames
// .partial to the final name, so a loser arriving after it finished lands
// here rather than at failCopy, and for a row another attempt has carried
// to REMOTE_RETAINED the FAILED write is refused by machine.go's TABLE
// rather than by the journal's CAS. supersededBy classifies both, which is
// what stops the refusal itself being handed to an operator as the outcome
// (issue #570's own sentence, over an artifact that is durable and
// retained). The write is still made and still refused; only what the
// caller is told about it changed.
func failCollision(ctx context.Context, d Deps, p TransferParams, from, path string) (state.Outcome, error) {
	collision := &FinalNameCollisionError{Artifact: p.Artifact, Path: path}
	if _, err := Advance(ctx, d, state.Transition{
		Artifact: p.Artifact,
		Key:      p.AttemptKey + keyFailedCollisionSuffix,
		From:     from,
		To:       string(Failed),
		Detail:   collision.Error(),
	}); err != nil {
		if superseded := supersededBy(ctx, d, p.Artifact, err, collision); superseded != nil {
			return state.Outcome{}, superseded
		}
		return state.Outcome{}, fmt.Errorf("%w (and recording FAILED also failed: %v)", collision, err)
	}
	return state.Outcome{}, collision
}

// failCopy records a non-retryable (or retry-exhausted) copy failure as a
// FAILED transition and returns the underlying error, wrapped so
// errors.Is/As against copyErr still work.
//
// # Only the attempt that made the claim gets to spend it
//
// started is this call's own TRANSFERRING outcome, and started.Applied is
// the question this function turns on: did the journal apply THIS attempt's
// TRANSFERRING write, or did it replay one that was already there? A
// verdict is only ever recorded for the first, because only the first can
// honestly say the TRANSFERRING it is writing FAILED from is describing its
// own copy.
//
// Two live attempts derive the same keys (internal/app's attemptKey is the
// artifact plus its retry count and nothing in it tells two of them apart),
// so the loser's TRANSFERRING write is a replay of the winner's, and a
// replay reports the row and validates nothing, there being nothing to
// validate about a write it is not making.
//
// # Why pinning From is not enough, which is issue #570's actual ordering
//
// From stays TRANSFERRING and that part is load-bearing: recording FAILED
// from wherever the row happened to get to would write a failure over an
// artifact that already has a durable local copy, which is the one move
// machine.go's table refuses outright.
//
// What the pin does not cover is the ordering that actually happens. The
// losing attempt fails in milliseconds, because the winner renamed its
// .partial out from under it, while the winner is still copying gigabytes.
// The row is therefore still at TRANSFERRING, TRANSFERRING -> FAILED is a
// legal edge, the journal's CAS matches, and the loser's FAILED is
// ACCEPTED over a winner mid-flight. The winner cannot then record
// TRANSFERRED, and what is left is an artifact reading FAILED with a
// complete good copy at .partial for the next attempt to delete. That is
// the sentence #570 was filed about, so the claim check is what closes it:
// between the pin and the check, a verdict is only ever written by the
// attempt whose own copy it describes.
//
// # What the refusals hand back, and what they cost
//
// Nothing is recorded on either refusal, on purpose. The artifact is not
// failed, its row already says what it is, and the record an operator acts
// on is that row plus this error in the FR-23 log. supersededBy and
// supersededClaim are what turn a refused write into a sentence naming the
// state the artifact is really in.
//
// The cost lands on the other side of the same coin and it is deliberate.
// An attempt resuming after a crash replays its own TRANSFERRING key too,
// because the key is stable across a resume and that is what makes resuming
// work at all, so it cannot stamp FAILED either: an artifact whose copy
// fails permanently after a mid-copy restart sits at TRANSFERRING, retried
// and logged every cycle, until something moves its retry count. The
// journal cannot tell that attempt apart from a second live one, nothing in
// this package can, and of the two ways of being wrong, retrying a copy
// somebody is watching the log for is the one that cannot lose a backup.
func failCopy(ctx context.Context, d Deps, p TransferParams, copyErr error, started state.Outcome) (state.Outcome, error) {
	if !started.Applied {
		return state.Outcome{}, supersededClaim(ctx, d, p.Artifact, State(started.Record.State), copyErr)
	}
	if _, err := Advance(ctx, d, state.Transition{
		Artifact: p.Artifact,
		Key:      p.AttemptKey + keyFailedCopySuffix,
		From:     string(Transferring),
		To:       string(Failed),
		Detail:   copyErr.Error(),
	}); err != nil {
		if superseded := supersededBy(ctx, d, p.Artifact, err, copyErr); superseded != nil {
			return state.Outcome{}, superseded
		}
		return state.Outcome{}, fmt.Errorf("lifecycle: transfer: copy failed (%v), and recording FAILED also failed: %w", copyErr, err)
	}
	return state.Outcome{}, fmt.Errorf("lifecycle: transfer: copy failed: %w", copyErr)
}

// TransferSupersededError reports that a transfer attempt stopped speaking
// for its artifact partway through: the journal has the artifact somewhere
// this attempt's own outcome, success or failure, would misdescribe.
//
// It exists because the alternative an operator was actually getting is
// worse than an unhandled case. Issue #570 caught the shape twice in one
// cycle on a live deployment: a copy failed, the FAILED transition that
// would have recorded it was refused because another attempt had already
// carried the same artifact to REMOTE_RETAINED, and the engine reported a
// verdict no row anywhere backed. `status` counted no failure for it and
// `artifacts` showed a durable, retained artifact, and the log said the
// backup had failed. All three were reading the same artifact.
//
// The state is on the error rather than only in its message so a caller can
// act on it, and because it is the thing an operator has to see: it is the
// answer to "so what IS this artifact", which a bare copy error does not
// give and which the journal's own ErrStateMismatch gives in the vocabulary
// of a refused write rather than of an artifact.
//
// Classify it with errors.As. There is deliberately no AsTransferSuperseded
// helper to go with it: errors.As is the one line a caller needs, and an
// exported wrapper with nothing calling it is a claim about how this error
// gets handled that nothing in the code keeps.
type TransferSupersededError struct {
	// Artifact is the artifact this attempt lost its claim on, and
	// Current is where the journal actually has it.
	Artifact model.ArtifactID
	Current  State

	// Cause is the failure this attempt observed before it found out, and
	// it is nil when the attempt was superseded before any bytes moved.
	// It is unwrapped, so errors.Is and errors.As against a transport
	// error still work through this.
	Cause error
}

// Error says what this attempt saw, what it did NOT record, and what the
// artifact is instead. All three are needed: the first is the only account
// of the failure that exists anywhere, the second is what stops a reader
// going looking for a FAILED row, and the third is the state both other
// surfaces will be showing them.
//
// One sentence covers both things that can be in Cause, a failed copy and a
// refused final-name collision, because the useful half is the same either
// way and each of them already says what it was. Naming the mechanism (a
// copy in flight, a winner that had already finished) would be guessing:
// this attempt only ever learns that the artifact is not where it left it.
func (e *TransferSupersededError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf(
			"lifecycle: transfer: refusing to copy %s: this attempt was already recorded and the journal has since moved the artifact to %q, so a copy now would write over an artifact this attempt no longer speaks for",
			e.Artifact, e.Current,
		)
	}
	return fmt.Sprintf(
		"lifecycle: transfer: this attempt no longer speaks for %s (%v), so nothing was recorded as FAILED: another attempt has the artifact, the journal holds it at %q, and that is what it is",
		e.Artifact, e.Cause, e.Current,
	)
}

// Unwrap exposes the copy or collision failure this attempt observed, so
// errors.Is against a transport category still answers about the thing that
// actually went wrong.
func (e *TransferSupersededError) Unwrap() error { return e.Cause }

// supersededBy turns a refused verdict into the reason for it, or reports
// that this was not that kind of refusal.
//
// TWO refusals stand between a losing attempt and an artifact somebody else
// finished, and this classifies both, because which one fires is decided by
// how the caller spelled From rather than by anything about the situation:
//
//   - The journal's CAS, ErrStateMismatch, when From no longer matches the
//     row. failCopy pins From to TRANSFERRING, so its machine-table check
//     always passes and the journal is what refuses.
//   - machine.go's transition table, IllegalTransitionError, when the move
//     is not an edge at all. failCollision declares From = whatever the row
//     said, so for an artifact another attempt already carried to
//     REMOTE_RETAINED (or COMMITTED, or COMPLETE) the table refuses first
//     and the journal is never asked.
//
// Classifying only the first is what left issue #570's own sentence on the
// collision path, which is the likelier of the two: a winning attempt
// renames .partial to the final name, so a loser arriving after it finished
// hits the collision guard before it ever reaches the TRANSFERRING write,
// and what it emitted was "(and recording FAILED also failed: ...
// REMOTE_RETAINED -> FAILED is not a legal transition)" over an artifact
// that is durable and retained.
//
// Both refusals stay exactly where they are: this runs AFTER Advance has
// already turned the write down, and turning the refusal into a better
// sentence is the only thing it changes. Nothing here writes anything.
//
// Everything else returns nil, which keeps a genuinely broken journal (a
// closed database, a missing row) reading as the infrastructure failure it
// is rather than as somebody else's attempt winning a race. A table refusal
// is re-confirmed against the row as it stands now for the same reason: if
// FAILED is a legal move from where the artifact currently is, then whatever
// that refusal was about, it was not this.
//
// The re-read is what supplies the state, and it is only ever done on this
// path: the write that just failed proved the row is not where this attempt
// left it, and the whole value of the error is naming where it actually is.
// A read that itself fails leaves the caller with its original message,
// which is worse than this one but is still the failure it observed.
func supersededBy(ctx context.Context, d Deps, artifact model.ArtifactID, recordErr, cause error) *TransferSupersededError {
	var illegal *IllegalTransitionError
	tableRefused := errors.As(recordErr, &illegal)
	if !tableRefused && !errors.Is(recordErr, state.ErrStateMismatch) {
		return nil
	}
	rec, err := d.Journal.Get(ctx, artifact)
	if err != nil {
		return nil
	}
	if tableRefused && Validate(State(rec.State), Failed) == nil {
		return nil
	}
	return &TransferSupersededError{Artifact: artifact, Current: State(rec.State), Cause: cause}
}

// supersededClaim is the same sentence for an attempt that never held the
// claim it would have to spend, rather than for one whose write was turned
// down. See failCopy: an attempt whose TRANSFERRING write replayed somebody
// else's is not the attempt the journal has at TRANSFERRING, so it does not
// ask, and there is no refusal to classify.
//
// known is where this attempt last saw the artifact, from the outcome of its
// own TRANSFERRING write, and it is only the fallback: the row is re-read
// first because the winner may well have finished by now, and naming where
// the artifact actually is now is the whole point of this error.
func supersededClaim(ctx context.Context, d Deps, artifact model.ArtifactID, known State, cause error) *TransferSupersededError {
	current := known
	if rec, err := d.Journal.Get(ctx, artifact); err == nil {
		current = State(rec.State)
	}
	return &TransferSupersededError{Artifact: artifact, Current: current, Cause: cause}
}
