package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// discoverOne runs internal/discovery.Discover for one backup set,
// retrying the whole call under Service.retryPolicy when it fails with a
// transport.Transient-classified error.
//
// Retrying the whole call is safe for the same reason reconcileOne's
// retry is: a systemic Discover failure (its List call failing) happens
// before any candidate is ever journaled, so there is nothing partially
// done for a retry to disagree with. A per-candidate problem inside a
// successful List call never reaches this retry at all; it is isolated
// into discovery.Result's own Pending/Rejected/Conflicts/Errors buckets,
// exactly as internal/discovery's package doc describes, and naturally
// gets another chance the next time Discover runs (next cycle, or the
// next `fetch`), since Discover itself is safe to call repeatedly.
func (s *Service) discoverOne(ctx context.Context, source transport.Source, bs config.BackupSet) (discovery.Result, error) {
	deps := discovery.Deps{Transport: s.Transport, Journal: s.Journal, Now: s.Now}
	var res discovery.Result
	err := retry.Do(ctx, s.retryPolicy(), nil, func(ctx context.Context) error {
		var err error
		res, err = discovery.Discover(ctx, deps, source, bs)
		return err
	})
	return res, err
}

// attemptKey derives the idempotency-key base every lifecycle step this
// package calls for rec shares. It is built from the artifact's own
// identity (globally required: internal/state's idempotency-key replay is
// keyed across the whole state_transitions table, not scoped per artifact,
// so an unqualified key would risk colliding across two different
// artifacts) plus rec.RetryCount.
//
// RetryCount is what makes this the right "attempt" boundary. Nothing in
// today's codebase increments it while an artifact moves forward through
// DISCOVERED -> ... -> COMPLETE (see internal/lifecycle/quarantine.go's
// package doc: only QUARANTINED's release back to DISCOVERED bumps it
// today, and FR-22's own FAILED -> DISCOVERED retry policy, which is
// expected to share this same counter, has not been built yet). So calling
// this again for the same rec, after a crash mid-cycle left it exactly
// where it was, reproduces exactly the same key, and every lifecycle step
// below can resume the same logical attempt it left off on. The moment an
// artifact is genuinely sent back to DISCOVERED (a quarantine release, and
// eventually a FAILED retry), RetryCount has already advanced by then, so
// the very next attemptKey call computes a fresh base for what is,
// correctly, a new logical attempt.
func attemptKey(rec state.Record) string {
	return fmt.Sprintf("app:%s:attempt-%d", rec.Artifact, rec.RetryCount)
}

// processArtifact drives one journal record as far forward through
// DISCOVERED -> TRANSFERRING -> TRANSFERRED -> VERIFYING -> VERIFIED ->
// COMMITTING -> COMMITTED -> REMOTE_DELETE_PENDING -> COMPLETE as it
// safely can in this one call, entering wherever rec.State currently is
// (not only DISCOVERED): an artifact already sitting at COMMITTED or
// REMOTE_DELETE_PENDING when this call starts, for example because a
// previous cycle's delete attempt was refused or interrupted, skips
// straight to the delete step rather than being silently left behind
// forever. It stops early at whichever of these happens first:
//
//   - ctx is done (a shutdown was requested);
//   - a step reports a business outcome other than success (FAILED or
//     QUARANTINED): the artifact needs a human or a future retry pass, not
//     more of this cycle's own forward motion;
//   - a step reports an infrastructure error: logged, and this call
//     returns, leaving the artifact exactly where the failed step left it
//     (every step here reads the journal's current state itself before
//     acting, so the next cycle resumes cleanly).
//
// # Why a shutdown here can never leave a remote deleted without a committed local copy
//
// This is the property FR-1 requires ("shut down without initiating unsafe
// source deletion"). It holds for two independent reasons, one structural
// (true regardless of what this function does) and one that this function
// adds on top of it:
//
//  1. Structural, from internal/lifecycle itself: DeleteRemote is the only
//     call site in the whole project allowed to invoke
//     transport.Transport.DeleteRemote (see remotedelete.go's package
//     doc), and it may only ever be reached from COMMITTED or
//     REMOTE_DELETE_PENDING (machine.go's Transitions table: Committed is
//     RemoteDeletePending's only predecessor, proven by
//     TestOnlyCommittedPrecedesRemoteDeletePending). Since Commit's own
//     COMMITTING -> COMMITTED write already durably fsynced the local
//     file and recorded it before this function ever calls
//     deleteRemoteOne below, there is no possible interleaving, crash or
//     not, in which the remote gets deleted while the local file is not
//     yet durably committed. This is true even if this function had no
//     ctx checks in it at all.
//  2. What this function adds: it checks ctx.Err() before starting every
//     step below, including, critically, the one immediately before
//     deleteRemoteOne. That check is what turns "a shutdown signal
//     arrived" into "this artifact simply stops advancing this cycle"
//     rather than "every remaining step runs anyway because nothing
//     noticed". Concretely: if ctx is cancelled at any point up to and
//     including immediately after Commit returns, deleteRemoteOne is
//     never called at all this cycle; the artifact is left at COMMITTED
//     (commit.go's own crash-safety proof already covers a real process
//     kill at any point during the fsync/rename/fsync sequence, so a mere
//     ctx-cancellation observed cleanly between two function calls is a
//     strictly easier case), and a later cycle picks the delete step back
//     up exactly where the pipeline left off, safely, because
//     DeleteRemote is documented and tested as safe to call more than once
//     for the same logical attempt.
//
// See internal/app's test suite (pipeline_test.go) for a test that proves
// this with a real cancellation injected at exactly that boundary: it
// asserts the fake transport's DeleteRemote is never invoked, the journal
// still reads COMMITTED, the local final file still exists, and the fake
// remote object was never removed.
//
// # Return value
//
// final is the artifact's lifecycle.State exactly as this call leaves it,
// read back from the journal rather than from whatever this call happened
// to observe last. processArtifacts below uses it to tell a business
// outcome (FAILED, QUARANTINED) from everything else.
//
// Reading it back is load-bearing, and issue #361 is what it cost not to.
// A step can record a terminal state and still return an error: both of
// lifecycle.Transfer's refusal paths (failCopy for a copy that exhausted
// its retry budget, failCollision for a final-name collision) record
// FAILED and then return the underlying error. Every error path in this
// function returns without reassigning rec, because only a successful
// Advance ever does, so what this function used to report back for a
// refused transfer was DISCOVERED -- the state the record carried before
// the step ran -- while the journal already said FAILED. The cycle
// counted no failed artifact, and `run` exited 0 for a backup set whose
// own journal said its only artifact had failed. Asking the journal is
// the answer that cannot be reintroduced by a future step recording a
// verdict on a path this function treats as an error.
//
// The read deliberately outlives ctx (context.WithoutCancel): a shutdown
// arriving mid-cycle must not turn "what does the journal say" into "no
// idea". If the read fails anyway, the state this call last observed is
// the honest fallback.
//
// It costs one indexed point read per artifact per cycle, against a
// journal the same cycle has already listed in full. That is real and it
// is small, and it buys a property that does not depend on every future
// caller remembering which of its own early returns left rec behind. I
// considered reading back only where a step reported an error and decided
// against it: it is the same cost on every cycle that has anything wrong
// with it, which is the cycle that matters, and it puts the correctness
// back in the hands of whoever adds the next early return.
func (s *Service) processArtifact(ctx context.Context, source transport.Source, bs config.BackupSet, rec state.Record) (final lifecycle.State) {
	artifact := rec.Artifact
	base := attemptKey(rec)
	defer func() {
		final = lifecycle.State(rec.State)
		current, err := s.Journal.Get(context.WithoutCancel(ctx), artifact)
		if err != nil {
			s.logger().Error(ctx, "artifact-state", fmt.Errorf("reading back %s's state after this cycle's own work: %w", artifact, err))
			return
		}
		final = lifecycle.State(current.State)
	}()

	// Live progress (progress.go). Each stage is announced immediately
	// before the step that performs it, so an observer learns what is
	// happening while it happens rather than after it has finished, and
	// the artifact is counted as done however this function returns.
	// prog is nil for a cycle nobody is observing, and every method on it
	// is nil-safe.
	prog := progressFrom(ctx)
	defer prog.finishArtifact()

	if st := lifecycle.State(rec.State); st == lifecycle.Discovered || st == lifecycle.Transferring {
		if ctx.Err() != nil {
			return
		}
		if !s.admitCapacity(ctx, bs, rec) {
			return
		}
		prog.enterStage(StageTransferring, artifact.Name)
		out, err := s.transferOne(ctx, source, bs, rec, base)
		if err != nil {
			s.logger().Error(ctx, "transfer", err)
			return
		}
		s.logger().LifecycleTransition(ctx, artifact.String(), rec.State, out.Record.State, out.Detail)
		if out.Record.Transfer != nil {
			s.logger().TransferStats(ctx, artifact.String(), out.Record.Transfer.BytesTransferred, 0, out.Record.Transfer.Checksummed)
		}
		rec = out.Record
	}

	if lifecycle.State(rec.State) == lifecycle.Transferred {
		if ctx.Err() != nil {
			return
		}
		advanced, err := lifecycle.Advance(ctx, s.lifecycleDeps(), state.Transition{
			Artifact: artifact,
			Key:      base + ":verifying",
			From:     string(lifecycle.Transferred),
			To:       string(lifecycle.Verifying),
		})
		if err != nil {
			s.logger().Error(ctx, "advance-to-verifying", err)
			return
		}
		rec = advanced.Record
	}

	if lifecycle.State(rec.State) == lifecycle.Verifying {
		if ctx.Err() != nil {
			return
		}
		prog.enterStage(StageVerifying, artifact.Name)
		out, err := s.verifyOne(ctx, source, bs, rec, base)
		if err != nil {
			s.logger().Error(ctx, "verify", err)
			return
		}
		s.logger().LifecycleTransition(ctx, artifact.String(), rec.State, out.Record.State, out.Detail)
		if out.Record.ValidationDetail != "" || out.Record.ValidationPassed != nil {
			passed := out.Record.ValidationPassed != nil && *out.Record.ValidationPassed
			s.logger().Validation(ctx, artifact.String(), passed, out.Record.ValidationDetail)
		}
		rec = out.Record
	}

	// COMMITTING as well as VERIFIED, and that is issue #372. Every other
	// in-progress state is already an entry point here: the transfer step
	// above is reached from TRANSFERRING as well as DISCOVERED, and the
	// verify step from VERIFYING. COMMITTING was the one that was not, so
	// an artifact whose process died between lifecycle.Commit's
	// VERIFIED -> COMMITTING journal write and the COMMITTED one that
	// closes it was never handed back to the function that knows how to
	// finish it. lifecycle.Commit has handled being called again in that
	// state since it was written, with a comment saying so; nothing called
	// it. Reconciliation deliberately leaves these rows alone and says it
	// is letting normal processing resume from wherever the journal says
	// (reconcile.go), which is exactly right and exactly what did not
	// happen. So the row sat at COMMITTING, every cycle, forever, with its
	// bytes in a .partial file that may well have been renamed into place
	// already.
	//
	// attemptKey is what makes this safe rather than a guess: the key
	// lifecycle.Commit gets here is the same one the dead process used, so
	// the first Advance replays instead of re-applying and Commit takes
	// its own documented resume branch. See attemptKey's doc for why
	// RetryCount is the right attempt boundary.
	if st := lifecycle.State(rec.State); st == lifecycle.Verified || st == lifecycle.Committing {
		if ctx.Err() != nil {
			return
		}
		prog.enterStage(StageCommitting, artifact.Name)
		committed, err := s.commitOne(ctx, bs, rec, base)
		if err != nil {
			s.logger().Error(ctx, "commit", err)
			return
		}
		s.logger().Commit(ctx, artifact.String(), committed.Record.LocalPath)
		rec = committed.Record
	}

	// Reached from two different places: an artifact this call just
	// committed above, and an artifact that was already sitting at
	// COMMITTED or REMOTE_DELETE_PENDING when this call started, for
	// example because a previous cycle's own delete attempt was refused
	// (FR-16's identity re-check found only weak confidence, the common,
	// expected outcome against a hardened SFTP account; see
	// remotedelete.go's package doc) or was interrupted by a shutdown at
	// exactly the boundary this function's own doc describes. Either way,
	// DeleteRemote's documented idempotency under a reused AttemptKey
	// (attemptKey is derived from rec.RetryCount, which does not change
	// while an artifact sits at COMMITTED or REMOTE_DELETE_PENDING) makes
	// retrying it here, on every cycle, safe.
	//
	// Anything else reaching here (FAILED, QUARANTINED, QUARANTINED_LOST,
	// or COMPLETE, which has nothing further to do) stops: an operator
	// (ReleaseFromQuarantine) or a future FR-22 retry policy is what moves
	// FAILED/QUARANTINED again, not more of this cycle's own forward
	// motion.
	switch lifecycle.State(rec.State) {
	case lifecycle.Committed, lifecycle.RemoteDeletePending:
		// proceed to the delete step below.
	default:
		return
	}

	// The one boundary this whole function's safety property rests on: see
	// the doc comment above. Nothing after this point may run once ctx is
	// done.
	if ctx.Err() != nil {
		return
	}

	// Issue #282: a backup set declared read-only never reaches the delete
	// step at all. This branch, not a check inside DeleteRemote itself, is
	// what makes "no code path can reach DeleteRemote for that set" true:
	// s.deleteRemoteOne, and therefore lifecycle.DeleteRemote and
	// therefore transport.Transport.DeleteRemote, is simply never called
	// in this branch, structurally, not refused after being asked. See
	// readonly_test.go's TestProcessArtifact_ReadOnlyBackupSet_* for the
	// proof, driven with a transport double that fails the test the
	// instant DeleteRemote is invoked.
	if bs.ReadOnly {
		prog.enterStage(StageCleaningRemote, artifact.Name)
		out, err := s.retainRemoteOne(ctx, rec, base)
		if err != nil {
			s.logger().Error(ctx, "remote-retain", err)
			return
		}
		s.logger().LifecycleTransition(ctx, artifact.String(), rec.State, out.Record.State,
			"read-only backup set: the remote source is retained by policy, never offered for deletion")
		return
	}

	prog.enterStage(StageCleaningRemote, artifact.Name)
	out, err := s.deleteRemoteOne(ctx, source, bs, rec, base)
	if err != nil {
		var refusal *lifecycle.RemoteDeleteRefusalError
		if errors.As(err, &refusal) {
			s.logger().RemoteDelete(ctx, artifact.String(), rec.RemotePath, refusal)
		} else {
			s.logger().Error(ctx, "remote-delete", err)
		}
		return
	}
	s.logger().RemoteDelete(ctx, artifact.String(), rec.RemotePath, nil)
	s.logger().LifecycleTransition(ctx, artifact.String(), rec.State, out.Record.State, out.Detail)
	return
}

// processArtifacts drives every one of records forward via processArtifact
// and reports two different things about the walk.
//
// failed is how many ended this call in FAILED, QUARANTINED or
// QUARANTINED_LOST: a business outcome, not the systemic reconcile/
// discover failure a caller's own Err field already tracks separately.
//
// progress is issue #361's arithmetic: how many of these records this
// cycle was still trying to turn into a durable backup, and how many of
// those moved. It exists because failed alone cannot tell a cycle that
// had nothing to do from one where nothing got through. A refused
// transfer that leaves an artifact pre-durable for the next cycle is
// correctly not a failure, and a cycle in which every artifact was
// refused that way is correctly not a success either, and only the
// second count can see the difference. See CycleProgress and acquiring
// for what is counted and, just as deliberately, what is not.
//
// records is always listed fresh from the journal after this cycle's own
// FR-17 reconcile pass has already run and written whatever it decided
// (processBackupSet in cycle.go, and Fetch in fetch.go, both list after
// reconciling), so a record reconciliation itself just moved to
// QUARANTINED or QUARANTINED_LOST -- a previously-durable artifact whose
// local copy turned out corrupted or missing, discovered by a
// reconciliation pass that otherwise succeeded -- already carries that
// state by the time it reaches processArtifact here. processArtifact's
// own switch has no case for an already-terminal state, so it takes no
// further action and simply reports the state back; this function's own
// switch is what turns that into a counted failure. This is issue #283's
// second half (a High finding from PR #303's own adversarial review): a
// cycle where reconciliation alone discovered an irrecoverable loss, with
// no new transfer/verify/commit failure of its own, must count as failed
// too, since a successful reconciliation pass finding rot is a stronger
// case for a non-zero exit than a single artifact this cycle's own
// pipeline quarantined.
//
// RunCycle (processBackupSet, below) and Fetch (fetch.go) both walk their
// backup set's in-flight journal rows this exact same way and both need
// this exact same count to decide whether their cycle actually succeeded
// (issue #283: before this existed, neither did, and a cycle where every
// artifact discovered fine and then failed verification exited 0). Pulling
// the walk-and-count into one place, rather than each of them keeping its
// own copy, is what makes "run and fetch agree on what a failed cycle is"
// a structural property instead of two definitions that happen to match
// today.
func (s *Service) processArtifacts(ctx context.Context, source transport.Source, bs config.BackupSet, records []state.Record) artifactWalk {
	walk := artifactWalk{coveredPaths: map[string]bool{}}
	for _, rec := range records {
		if ctx.Err() != nil {
			break
		}
		before := lifecycle.State(rec.State)
		after := s.processArtifact(ctx, source, bs, rec)
		if terminalFailure(after) {
			walk.Failed++
		}
		// Every row's remote path, not only the ones counted below: the
		// question a discovery error asks is "is this object already
		// under management", and a COMPLETE row at that path answers yes
		// just as firmly as a DISCOVERED one does.
		walk.coveredPaths[rec.RemotePath] = true
		if !acquiring(before) {
			continue
		}
		walk.Progress.Walked++
		if durable(after) {
			walk.Progress.Durable++
		}
	}
	return walk
}

// artifactWalk is what one walk over a backup set's journal rows tells the
// cycle around it. Nothing else builds one.
type artifactWalk struct {
	// Failed is how many rows ended the walk in FAILED, QUARANTINED or
	// QUARANTINED_LOST (issue #283).
	Failed int

	// Progress is issue #361's denominator and numerator over these rows
	// alone. The caller adds the discovery candidates that never became
	// rows.
	Progress CycleProgress

	// coveredPaths is every remote path this walk saw a journal row for,
	// in any state, so a caller folding in discovery's own per-candidate
	// errors can tell an object nothing here knows about from one that is
	// already under management.
	//
	// Both halves of that matter. An object with a row in flight would
	// otherwise be counted twice, once by the walk and once by discovery,
	// which changes no verdict but puts a number in front of an operator
	// that does not match what is on the remote. An object with a
	// FINISHED row is worse than double counting: a read-only backup set
	// keeps its remote objects forever by design, so discovery re-reads
	// them every cycle, and one transient identity-capture failure
	// against an artifact that was safely backed up weeks ago must not
	// read as work this cycle failed to do.
	coveredPaths map[string]bool
}

// terminalFailure names the three states an artifact ends in when it did
// not get through: this cycle's own transfer/verify/commit failure, or a
// loss reconciliation found on its own.
func terminalFailure(st lifecycle.State) bool {
	switch st {
	case lifecycle.Failed, lifecycle.Quarantined, lifecycle.QuarantinedLost:
		return true
	}
	return false
}

// durable names the states in which an artifact's bytes are on local disk,
// verified and fsynced, which is what a backup is (see internal/lifecycle's
// state doc). Everything before COMMITTED is either still in flight or not
// yet proven; everything after it is either finished or a failure.
func durable(st lifecycle.State) bool {
	switch st {
	case lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.RemoteRetained, lifecycle.Complete:
		return true
	}
	return false
}

// acquiring names the states an artifact is still trying to get out of on
// its way to being a durable local backup, which is exactly the work
// issue #361 asks "did any of it land". It is deliberately narrower than
// "not terminal", in both directions:
//
//   - COMMITTING is in, since issue #372. It used to be left out on the
//     grounds that processArtifact could not act on it, so counting it
//     would report a stall the cycle had no move for. processArtifact
//     acts on it now, which turns that reasoning around completely: a
//     COMMITTING row is work this cycle tried and did not land, exactly
//     like a TRANSFERRED one, and leaving it out would mean a set whose
//     commits keep failing reports a clean cycle every time. That is the
//     invisibility issue #372 is about, one layer up from the resume
//     itself.
//   - COMMITTED and REMOTE_DELETE_PENDING are left out because by then
//     the bytes are durably on local disk and the backup has already
//     succeeded. What is left is the remote cleanup, and FR-16's
//     identity re-check refusing that is the documented, expected steady
//     state against a hardened source (see remotedelete.go) rather than
//     a backup that did not happen. A set sitting there forever is a
//     healthy set, and calling it a failed cycle every poll interval
//     would be exactly the false alarm this count exists to avoid.
func acquiring(st lifecycle.State) bool {
	switch st {
	case lifecycle.Discovered, lifecycle.Transferring, lifecycle.Transferred,
		lifecycle.Verifying, lifecycle.Verified, lifecycle.Committing:
		return true
	}
	return false
}

// transferOne runs lifecycle.Transfer with a bounded retry policy (see
// DefaultRetryPolicy's doc) so a persistently unreachable source
// eventually surfaces as FAILED, this cycle, rather than retrying forever
// and starving every other artifact and backup set behind it.
func (s *Service) transferOne(ctx context.Context, source transport.Source, bs config.BackupSet, rec state.Record, base string) (state.Outcome, error) {
	// The one place the transport's own progress reporting is switched
	// on. It is scoped to the copy, not to the cycle, because that is what
	// the counters describe: an artifact's bytes belong to the artifact
	// being copied, and nothing else in the pipeline copies anything.
	ctx = progressFrom(ctx).reportingCtx(ctx)
	return lifecycle.Transfer(ctx, s.lifecycleDeps(), lifecycle.TransferParams{
		Artifact:   rec.Artifact,
		Source:     source,
		LocalDir:   bs.LocalPath,
		AttemptKey: base + ":transfer",
		Policy:     s.retryPolicy(),
	})
}

// verifyOne runs lifecycle.Verify. VerifyParams has no caller-configurable
// retry policy (its one network-facing call, a remote hash lookup, is
// already internally bounded by lifecycle's own hardcoded policy; see
// verify.go's remoteHashRetryPolicy), so there is nothing for this package
// to configure here.
func (s *Service) verifyOne(ctx context.Context, source transport.Source, bs config.BackupSet, rec state.Record, base string) (state.Outcome, error) {
	return lifecycle.Verify(ctx, s.lifecycleDeps(), lifecycle.VerifyParams{
		Artifact:   rec.Artifact,
		Source:     source,
		Validation: bs.Validation,
		AttemptKey: base + ":verify",
	})
}

// commitOne runs lifecycle.Commit. It touches only the local filesystem
// (see commit.go's package doc), so there is no transient-network case to
// retry here at all.
func (s *Service) commitOne(ctx context.Context, bs config.BackupSet, rec state.Record, base string) (state.Outcome, error) {
	return lifecycle.Commit(ctx, s.lifecycleDeps(), lifecycle.CommitInput{
		Artifact:      rec.Artifact,
		LocalDir:      bs.LocalPath,
		CommittingKey: base + ":committing",
		CommittedKey:  base + ":committed",
	})
}

// deleteRemoteOne runs lifecycle.DeleteRemote exactly once (no retry
// wrapper here: unlike Discover/Reconcile, a transient failure inside
// DeleteRemote is not silently absorbed into a side report, it is returned
// straight to processArtifact as this call's own error, and is left for
// the next cycle to retry, which DeleteRemote's own documented, tested
// idempotency under a reused AttemptKey makes safe to do).
func (s *Service) deleteRemoteOne(ctx context.Context, source transport.Source, bs config.BackupSet, rec state.Record, base string) (state.Outcome, error) {
	return lifecycle.DeleteRemote(ctx, s.lifecycleDeps(), lifecycle.DeleteRemoteRequest{
		Source:     source,
		Artifact:   rec.Artifact,
		AttemptKey: base + ":delete",
		// WP3.2: these two are what let DeleteRemote tell a "stable"
		// backup set apart from "rename"/"marker" and gate it behind an
		// extra deletion-safety delay. DeleteRemote refuses an empty or
		// unrecognised strategy outright, so passing them is not optional;
		// see DeleteRemoteRequest.CompletionStrategy and remotedelete.go's
		// own doc for the full reasoning.
		CompletionStrategy: bs.Completion.Strategy,
		DeleteSafetyDelay:  bs.Completion.DeleteSafetyDelay.Duration(),
	})
}

// retainRemoteOne runs lifecycle.RetainRemote: issue #282's read-only path,
// taken instead of deleteRemoteOne whenever bs.ReadOnly is true. Unlike
// that function, this one never touches transport.Transport at all -- it
// does not even receive one -- so there is nothing in its call graph that
// could reach transport.Transport.DeleteRemote.
func (s *Service) retainRemoteOne(ctx context.Context, rec state.Record, base string) (state.Outcome, error) {
	return lifecycle.RetainRemote(ctx, s.lifecycleDeps(), lifecycle.RetainRemoteRequest{
		Artifact:   rec.Artifact,
		AttemptKey: base + ":retain",
	})
}

// admitCapacity is FR-21's gate, consulted immediately before every
// transfer begins (both a fresh DISCOVERED start and a crash-resumed
// TRANSFERRING restart: transfer.go always redoes the whole copy from
// scratch in either case, so the same headroom is required either way).
//
// It reports false, meaning "do not transfer this artifact right now", on
// any refusal or error; the artifact is left exactly where it is (no
// journal write happens here at all) for a later cycle to retry once space
// is available. See Service.Capacity's own doc for where its thresholds
// come from, and internal/capacity's "Two different questions" section for
// how the operator's cap and the filesystem's free space combine into the
// one headroom figure this gate is decided from.
func (s *Service) admitCapacity(ctx context.Context, bs config.BackupSet, rec state.Record) bool {
	// capacity.StatPath needs an existing directory to statfs; nothing
	// upstream of this call (config.Validate only checks the configured
	// path is absolute and traversal-free, never that it exists yet) has
	// created bs.LocalPath. A fresh deployment's very first cycle would
	// otherwise fail every single transfer with a capacity error before
	// ever reaching Transfer, which would have created the directory
	// itself as a side effect of the copy. MkdirAll is idempotent and
	// cheap, so doing it here, immediately before the one thing that
	// actually needs the directory to exist, is simpler and safer than
	// trying to guess the one right moment to create it up front.
	if err := os.MkdirAll(bs.LocalPath, 0o755); err != nil {
		s.logger().Error(ctx, "capacity", fmt.Errorf("ensuring local destination directory %s exists: %w", bs.LocalPath, err))
		return false
	}

	var size int64
	if rec.Remote.Size != nil {
		size = *rec.Remote.Size
	}

	// The cap's own input (issue #286). A statfs reading answers "does the
	// disk have room"; enforcing an operator's ceiling additionally needs
	// "how much of the allowance have we spent", and only the catalog
	// knows that. A failure to measure it is a refusal, not a zero: with a
	// cap configured, capacity.Assess will not guess at an unmeasured
	// usage, and with no cap configured the value is never consulted, so
	// this costs a deployment without a cap nothing but one aggregate
	// query.
	usage, err := s.LocalUsage(ctx)
	if err != nil {
		s.logger().Error(ctx, "capacity", err)
		return false
	}

	assessment, err := capacity.CheckBeforeTransfer(bs.LocalPath, usage, size, s.Capacity)
	var insufficient *capacity.InsufficientCapacityError
	switch {
	case errors.As(err, &insufficient):
		s.logger().DiskPressure(ctx, bs.LocalPath, int64(assessment.Stat.FreeBytes), int64(assessment.Stat.TotalBytes), "critical")
		return false
	case err != nil:
		s.logger().Error(ctx, "capacity", err)
		return false
	case assessment.Level == capacity.Warning:
		s.logger().DiskPressure(ctx, bs.LocalPath, int64(assessment.Stat.FreeBytes), int64(assessment.Stat.TotalBytes), "warning")
	}
	return true
}

// eventDiscoveryCounts is a small helper so cycle.go's call into
// s.logger().Discovery reads as one line instead of six repeated len()
// calls at the call site.
func eventDiscoveryCounts(res discovery.Result) (discovered, alreadyKnown, pending, rejected, conflicts, errored int) {
	return len(res.Discovered), len(res.AlreadyKnown), len(res.Pending), len(res.Rejected), len(res.Conflicts), len(res.Errors)
}
