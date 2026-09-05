// This file is the FR-13 step: everything between an artifact reaching
// TRANSFERRED and it being allowed to become VERIFIED. Verify is the single
// entry point; it always does the mandatory transfer-verification checks,
// then layers on hash verification and application validation exactly as
// far as configuration and the backend allow, and finally makes exactly one
// Advance call to record whichever of VERIFIED, FAILED or QUARANTINED the
// result earns. Like Transfer (transfer.go), Verify reads the artifact's
// current state from d.Journal itself rather than trusting a caller-held
// copy, so a resumed call after a crash re-reads the local file and, for a
// deterministic check, reaches the same verdict and the same idempotency
// key it would have produced before the crash.
//
// # Three layers, three different failure shapes
//
// Transfer verification (always performed): the copy already returning
// success is established by the artifact reaching VERIFYING at all (that is
// what the FR-10 state machine's sequencing guarantees), so what is left to
// prove here is that the bytes are actually still on disk as recorded: the
// local file opens, reads to completion without an I/O error, and its size
// matches what the transfer step recorded. A failure here is "the copy
// didn't actually happen the way the journal says it did" -- an operational
// problem, not a verdict about content -- so it produces FAILED, the same
// category machine.go documents for a permanent, non-retryable error at the
// verification step.
//
// Hash verification (gated by config.Validation.Hash) either confirms a
// checksum the transfer step already trusted, or asks the backend for one
// directly. A definite mismatch is a positive finding that the content
// itself is bad, which is QUARANTINED's job, not FAILED's (see machine.go's
// package doc on that distinction). An inability to complete a *required*
// hash check is a different thing again: see "The capability-absence
// decision" below.
//
// Application validation (gated by config.Validation.Command) runs an
// external, untrusted program against the local file and treats its exit
// code, and only its exit code, as a pass/fail verdict. A required
// validator's "no", or its failure to answer at all within its timeout, is
// exactly the case FR-13 says must prevent source deletion, so both land in
// QUARANTINED: see TestFailingValidatorBlocksSourceDeletion for the proof
// that QUARANTINED actually forecloses reaching a delete-eligible state
// without the whole pipeline running again from DISCOVERED.
//
// # The capability-absence decision
//
// A hardened SFTP account, exactly the kind docs/ssh-setup.md recommends
// (ForceCommand internal-sftp, /sbin/nologin, no shell) cannot run the
// remote command rclone's sftp backend needs to compute a hash. f.Hashes()
// comes back empty and Transport.RemoteHash returns an explicit
// transport.UnsupportedCapability error. That is not a rare edge case for
// this project, it is the normal shape of its recommended deployment, so it
// gets a normal, deliberate, always-tested answer instead of an
// afterthought:
//
//   - config.Validation.Hash == "": the operator has explicitly chosen not
//     to require hash verification. Transfer verification is this backup
//     set's whole guarantee, and that is a legitimate, honest choice for a
//     backend that cannot do better. Capability absence is irrelevant in
//     this case, because nothing asked for the capability.
//   - config.Validation.Hash == "sha256": the operator has explicitly
//     required cryptographic confirmation. The manager honours that
//     literally: it asks the backend, via RemoteHash, every time. If the
//     backend cannot answer, capability absent, or any other error,
//     verification for this artifact FAILS explicitly, with a detail
//     naming the reason. It never falls back to treating the
//     already-passed transfer-verification checks as "good enough": that
//     would be exactly the silent downgrade to a size check FR-13 forbids.
//     An operator running the recommended hardened SFTP setup and wanting
//     a hard per-artifact content guarantee needs an application validator
//     instead (one that does not depend on a remote hash call), not this
//     switch.
//
//     "Every time" is #492's correction. This used to say it would take
//     the transfer step's own word for it when the record carried
//     state.TransferResult.Checksummed, which nothing ever set, so the
//     shortcut was unreachable. It came out rather than getting wired up,
//     because the hash rclone compares during a copy is the first type
//     both ends share and that is the weaker one everywhere this manager
//     copies from, and discharging a configured sha256 policy with it is
//     the same silent downgrade this paragraph already refuses.
//
// # A check that could not be completed (issue #419)
//
// The capability-absence decision above is about a backend that ANSWERED,
// and answered that it cannot do this. There is a third case, and it is
// the one #419 found: a backend that could not be asked at all. A connect
// timeout rclone imposed on itself is the shape it takes, and since #408
// it classifies, correctly, as transport.Transient.
//
// A retry-exhausted Transient failure is not evidence about the artifact.
// internal/revalidate had already written the rule down one package over,
// for the same situation on the other side of a move: "an unreachable
// bucket is not evidence that a backup is gone", so a placement it could
// not ask about is an error rather than a verdict. This file did the
// opposite, and recorded FAILED. That is wrong twice over. It contradicts
// FAILED's own documented meaning (machine.go: "a permanent, non-retryable
// error", where transport.Category.Retryable is this product's own answer
// to whether a failure is permanent), and it strands the artifact, because
// FAILED's declared exits are FR-22's retry policy and FR-22's retry
// policy has never been built, so nothing takes them.
//
// So a required check that could not be COMPLETED records no verdict. The
// artifact stays exactly where it honestly is, at VERIFYING, and Verify
// returns a *VerificationStalledError. That is the pre-#388 self-healing
// behaviour back, without the cancellation nobody asked for that #388 was
// about: the category is honest, the log line is honest, and the next
// cycle simply asks again.
//
// It is bounded rather than endless, and the bound is VerifyParams.
// StallBudget, counted on the journal row's own FR-22 RetryCount. Once it
// is spent the artifact goes to QUARANTINED, never to FAILED: see
// recordStall for why that is the honest destination and what it does and
// does not claim about the bytes.
//
// The existing config.Validation.Hash field is the explicit switch FR-13
// asks this decision to live in: which of the two honest postures above
// applies is the operator's choice, recorded in configuration, not a
// judgement call this file makes silently at runtime.
package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// maxValidatorOutput bounds how much of an application validator's combined
// stdout+stderr this package will keep, both while capturing it (so a
// runaway untrusted process cannot exhaust memory by writing without limit)
// and in the detail string persisted to the journal (so one validator can't
// bloat the journal without limit either). A pass/fail detail has no
// business being larger than this; a validator that wants to leave a full
// report should write one to a file of its own choosing.
const maxValidatorOutput = 16 << 10 // 16 KiB

// remoteHashRetryPolicy bounds the retries Verify gives a RemoteHash call
// before treating it as failed. Only transport.Transient failures are
// retried at all (retry.DefaultIsTransient, driven by transport.CategoryOf),
// so a capability-absence error is never retried: retrying it cannot change
// a fixed property of the backend. MaxAttempts is set explicitly, rather
// than left at retry.DefaultPolicy's unbounded default, because Verify has
// no deadline of its own to bound an otherwise-unbounded retry loop against.
var remoteHashRetryPolicy = retry.Policy{MaxAttempts: 3}

// keyVerifySuffix is appended to VerifyParams.AttemptKey to build the one
// journal write Verify makes. Verify only ever records a single transition
// per call (unlike Transfer, which records TRANSFERRING and then
// TRANSFERRED), so a single, fixed suffix is enough to keep it distinct
// from another lifecycle step sharing the same AttemptKey base.
const keyVerifySuffix = ":verify"

// keyVerifyStallSuffix is appended to VerifyParams.AttemptKey to build the
// one write a stalled attempt makes (issue #419).
//
// A fixed suffix, like keyVerifySuffix, and it stays correct across
// successive stalls for a reason worth stating rather than assuming: the
// stall write is what increments state.Record.RetryCount, and
// internal/app's attemptKey derives the base from RetryCount, so the next
// attempt against a still-unreachable backend arrives here with a base
// that has already moved. Replaying the SAME attempt (a crash between the
// write landing and this call returning) reuses the same base and the same
// key, so the journal replays it instead of spending a second attempt on
// one outage.
const keyVerifyStallSuffix = ":verify-stall"

// VerifyParams is what Verify needs beyond Deps.
type VerifyParams struct {
	// Artifact is the artifact to verify. It must currently have a
	// VERIFYING journal row (Verify reads the current record itself, so a
	// resumed call after a crash works the same as a first call).
	Artifact model.ArtifactID

	// Source is the remote this artifact's bytes were copied from, needed
	// only for the hash-comparison path (Transport.RemoteHash).
	Source transport.Source

	// Validation is the backup set's FR-13 policy: whether a hash is
	// required, and the optional application validator.
	Validation config.Validation

	// AttemptKey is this logical attempt's idempotency key base, exactly
	// like TransferParams.AttemptKey: the same value across a crash-and-
	// resume, a new one for a genuinely new attempt (see TransferParams's
	// doc for the full contract, which Verify shares).
	AttemptKey string

	// StallBudget is how many consecutive attempts this artifact's
	// verification may STALL, on a check that could not be completed at
	// all, before Verify stops leaving it in progress and hands it to a
	// human (issue #419). See "A check that could not be completed" in
	// this file's package doc for what a stall is and why it is not a
	// verdict.
	//
	// It is counted against state.Record.RetryCount, FR-22's own "how many
	// times has this artifact been sent back to try again from an
	// exceptional state" counter, which internal/lifecycle/quarantine.go
	// already designed to be shared rather than duplicated. So an artifact
	// an operator has already released from quarantine twice reaches a
	// human two stalls sooner, which is the right direction: an artifact
	// that keeps needing attention should get it earlier, not later.
	//
	// Zero or negative means the caller granted no tolerance at all and
	// the first exhausted attempt is the outcome. internal/app derives the
	// real value from the same retry policy that already bounds one
	// attempt's own retries; see verifyOne there.
	StallBudget int
}

// VerificationStalledError reports that a REQUIRED check could not be
// completed, as opposed to completing and producing a verdict.
//
// It is its own type rather than a wrapped transport error because the two
// answer different questions. transport.Category says what went wrong on
// the wire; this says what it means for the artifact, which is nothing:
// the check never ran, so there is no finding, and the journal row is
// exactly where it honestly was. A caller reads Attempt/Budget to say how
// close this artifact is to being handed to a human.
//
// Unwrap keeps the underlying classified failure reachable, so
// transport.CategoryOf and errors.Is still answer for it.
type VerificationStalledError struct {
	Artifact model.ArtifactID

	// Attempt is how many consecutive stalls this artifact has now
	// recorded, including this one.
	Attempt int

	// Budget is the StallBudget this attempt was measured against.
	Budget int

	// Err is the classified failure that stopped the check.
	Err error
}

// Error puts the attempt count and the budget in the message rather than
// leaving them to a caller that might not print them. This error is the one
// an operator sees repeatedly for the same artifact, once per cycle, and
// "attempt 3 of 5" is what turns a wall of identical lines into something
// with a visible end: it says both that the artifact is not being ignored
// and how long it has before somebody has to look at it.
func (e *VerificationStalledError) Error() string {
	return fmt.Sprintf(
		"lifecycle: verify: %s could not be checked (attempt %d of %d before this is handed to an operator): %v",
		e.Artifact, e.Attempt, e.Budget, e.Err)
}

// Unwrap keeps the classified failure reachable, so a caller can still ask
// errors.Is what actually went wrong. The stall bookkeeping is a fact about
// this artifact's history; the wrapped error is the fact about the world,
// and losing the second to report the first would make an unreachable NAS
// indistinguishable from a backend that cannot hash.
func (e *VerificationStalledError) Unwrap() error { return e.Err }

// AsVerificationStalled reports whether err is, or wraps, a
// *VerificationStalledError.
func AsVerificationStalled(err error) (*VerificationStalledError, bool) {
	var e *VerificationStalledError
	return e, errors.As(err, &e)
}

// verifyOutcome is the terminal disposition decide has reached, before
// Verify packages it into exactly one state.Transition and hands it to
// Advance.
type verifyOutcome struct {
	to         State
	detail     string
	hashes     *state.HashUpdate
	validation *state.ValidationUpdate
}

// Verify runs the FR-13 checks for p.Artifact and Advances it to exactly
// one of Verified, Failed or Quarantined. Like Advance, the returned error
// is non-nil only for an infrastructure problem (a missing dependency, an
// illegal transition, a journal write failure, or context cancellation): a
// business outcome of "this artifact failed verification" or "this
// artifact is quarantined" is a successful call, reported through
// Outcome.Record.State, not through error.
//
// Cancellation is treated the way Transfer treats it (see transfer.go's
// package doc): it is a stop request, not a verdict, so it is returned
// without ever calling Advance. The journal is left exactly where it
// honestly is (VERIFYING), and a later call with the same AttemptKey
// resumes cleanly.
func Verify(ctx context.Context, d Deps, p VerifyParams) (state.Outcome, error) {
	if d.Journal == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: verify needs a Journal")
	}
	if d.Transport == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: verify needs a Transport")
	}
	if p.AttemptKey == "" {
		return state.Outcome{}, fmt.Errorf("lifecycle: verify needs an AttemptKey")
	}
	if err := ctx.Err(); err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: verify: %w", transport.NewError(transport.Cancelled, "verify", err))
	}

	rec, err := d.Journal.Get(ctx, p.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: verify: looking up %s: %w", p.Artifact, err)
	}

	out, halted := decide(ctx, d, p.Source, rec, p.Validation)
	if halted != nil {
		// Two different reasons decide declines to produce a verdict, and
		// they are routed differently. A cancellation is the caller's own
		// decision and is propagated untouched, leaving the journal
		// exactly where it is. A check that could not be COMPLETED is not
		// anybody's decision: it is recorded, bounded and eventually
		// handed to an operator (see recordStall).
		var unfinished *unfinishedCheck
		if errors.As(halted, &unfinished) {
			return recordStall(ctx, d, p, rec, unfinished.err)
		}
		return state.Outcome{}, fmt.Errorf("lifecycle: verify: %w", halted)
	}

	result, err := Advance(ctx, d, state.Transition{
		Artifact:   rec.Artifact,
		Key:        p.AttemptKey + keyVerifySuffix,
		From:       rec.State,
		To:         string(out.to),
		Detail:     out.detail,
		Hashes:     out.hashes,
		Validation: out.validation,
	})
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: verify: recording %s: %w", out.to, err)
	}
	return result, nil
}

// unfinishedCheck is decide's way of saying "a required check could not be
// completed", which is neither a verdict nor a cancellation and so needs a
// third answer rather than being folded into either.
//
// It is unexported because it is a signal between two functions in one
// file. What a CALLER sees is VerificationStalledError, which carries the
// attempt bookkeeping this type has no way to know.
type unfinishedCheck struct{ err error }

// Error passes the underlying message straight through, adding no prefix of
// its own. This type is a marker rather than a wrapper: it exists so decide
// can return a third kind of answer, and any text it added would end up
// duplicated inside the VerificationStalledError that a caller actually
// sees.
func (u *unfinishedCheck) Error() string { return u.err.Error() }

// Unwrap keeps the cause reachable, which is what lets Verify classify the
// failure after unwrapping the marker.
func (u *unfinishedCheck) Unwrap() error { return u.err }

// recordStall is what Verify does with a check that could not be
// completed: it counts the attempt on the journal row, and either leaves
// the artifact exactly where it honestly is or, once the budget is spent,
// hands it to an operator.
//
// # Why the exhausted case is QUARANTINED and not FAILED
//
// FAILED is documented, in machine.go, as "a permanent, non-retryable
// error", and transport.Category.Retryable is this product's own answer to
// whether a failure is permanent. A transient failure recorded as FAILED
// contradicts both. It also strands the artifact: FAILED's two declared
// exits, back to DISCOVERED and into QUARANTINED, are the FR-22 retry
// policy that has never been built, so nothing in this product takes
// either one and the row stops being worked on permanently, on the
// strength of a network condition that has very likely already cleared
// (issue #419).
//
// QUARANTINED is where an artifact waits for a human, and it already has
// three operator actions wired to it end to end, one of which
// (RetryQuarantinedIngestion) is exactly the route back into the pipeline
// this case needs. Sending an artifact there says something true about it,
// as long as the words are right: what is being held is not "these bytes
// are suspect" but "nobody has been able to prove anything about these
// bytes, and until somebody does they must not be committed and must not
// authorise deleting the source". QUARANTINED guarantees precisely that,
// which is why no new state was invented for it. See machine.go's package
// doc, which now says so beside the table, and QuarantineReason, which
// reports this shape as what it is rather than as a content check that
// failed.
//
// # What is deliberately NOT recorded
//
// No hash update. Layer 1 computed the local file's SHA-256 as a side
// effect of reading it, and attaching it here would leave the row carrying
// exactly the evidence QuarantineReason reads to decide whether to say "a
// content check failed", about a comparison that never happened.
func recordStall(ctx context.Context, d Deps, p VerifyParams, rec state.Record, cause error) (state.Outcome, error) {
	attempt := rec.RetryCount + 1
	// The category, once. transport.Error renders it itself, so prefixing
	// unconditionally produced "transient: remote_hash: transient: ..." on
	// the one path that always has one; this names it only where the
	// message does not already.
	reason := cause.Error()
	if category, ok := transport.CategoryOf(cause); ok && !strings.Contains(reason, category.String()) {
		reason = fmt.Sprintf("%s: %s", category, reason)
	}
	lastError := "verification could not be completed: " + reason

	if attempt >= p.StallBudget {
		out, err := Advance(ctx, d, state.Transition{
			Artifact: rec.Artifact,
			Key:      p.AttemptKey + keyVerifyStallSuffix,
			From:     rec.State,
			To:       string(Quarantined),
			Detail: fmt.Sprintf(
				"verification could not be completed on %d of %d attempts, so this artifact is held for an operator rather than left in progress or reported as a verdict nobody measured: %s",
				attempt, max(attempt, p.StallBudget), reason),
			Retry: &state.RetryUpdate{Count: attempt, LastError: lastError},
		})
		if err != nil {
			return state.Outcome{}, fmt.Errorf("lifecycle: verify: recording %s after %d unfinished checks: %w", Quarantined, attempt, err)
		}
		return out, nil
	}

	if _, err := Advance(ctx, d, state.Transition{
		Artifact: rec.Artifact,
		Key:      p.AttemptKey + keyVerifyStallSuffix,
		From:     rec.State,
		To:       rec.State,
		Detail: fmt.Sprintf(
			"verification could not be completed (attempt %d of %d): %s", attempt, p.StallBudget, reason),
		Retry: &state.RetryUpdate{Count: attempt, LastError: lastError},
	}); err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: verify: recording an unfinished check for %s: %w", rec.Artifact, err)
	}

	return state.Outcome{}, &VerificationStalledError{
		Artifact: rec.Artifact,
		Attempt:  attempt,
		Budget:   p.StallBudget,
		Err:      cause,
	}
}

// decide runs every configured check and returns the single terminal
// disposition Verify should record. It never touches the journal itself;
// Verify is the only place that does, so there is exactly one write per
// call regardless of how many checks ran.
//
// A non-nil second return means the attempt was cancelled: the caller must
// propagate it without recording anything, exactly like Transfer's own
// cancellation handling.
func decide(ctx context.Context, d Deps, source transport.Source, rec state.Record, cfg config.Validation) (verifyOutcome, error) {
	// Layer 1: transfer verification, always. "The copy returned success"
	// is already established by rec reaching Verifying at all; what remains
	// is proving the bytes on disk right now still match that record.
	bytesRead, localHash, err := readAndHashLocal(rec.LocalPath)
	if err != nil {
		return verifyOutcome{to: Failed, detail: fmt.Sprintf("transfer verification: %v", err)}, nil
	}

	expected, known := expectedSize(rec)
	if !known {
		return verifyOutcome{to: Failed, detail: "transfer verification: no recorded size (neither transfer result nor remote identity) to check the local file against"}, nil
	}
	if bytesRead != expected {
		return verifyOutcome{
			to:     Failed,
			detail: fmt.Sprintf("transfer verification: expected %d bytes, local file %q has %d", expected, rec.LocalPath, bytesRead),
		}, nil
	}

	// This is always computed as a side effect of the mandatory full read
	// above, so recording it costs nothing extra even when cfg.Hash == "":
	// it is forensically useful on its own, and it does not, by itself,
	// claim anything was confirmed against the remote. Only the hash-compare
	// path below can claim that.
	hashes := &state.HashUpdate{Hash: localHash, Alg: string(transport.SHA256)}

	// Layer 2: hash verification, gated by policy. See the package doc's
	// "capability-absence decision" for why an unsupported or otherwise
	// failing RemoteHash call ends this attempt in Failed rather than
	// silently accepting layer 1 as sufficient.
	switch cfg.Hash {
	case "":
		// The operator has explicitly not asked for this; transfer
		// verification above is the whole guarantee for this backup set.
	case string(transport.SHA256):
		// No shortcut here, deliberately. There used to be one: a record
		// carrying state.TransferResult.Checksummed skipped the call
		// below, on the reading that the copy had already compared a hash
		// and would have failed outright on a mismatch. #492 removed it.
		// Nothing ever set the field, so the branch was dead, and it was
		// dead in the direction that hid what it would have cost: the
		// hash rclone's copy compares is the first type both ends share,
		// which is the weaker one on every backend this manager can
		// reach, so a live version of that branch would have discharged a
		// configured sha256 policy with a comparison the operator did not
		// ask for. See transport.TransferResult's own doc.
		remoteHash, hashErr := remoteHashWithRetry(ctx, d.Transport, source, rec.RemotePath)
		if isCancellation(hashErr) {
			return verifyOutcome{}, hashErr
		}
		if hashErr == nil && remoteHash == "" {
			hashErr = errors.New("backend returned an empty hash with no error")
		}
		if hashErr != nil {
			category, classified := transport.CategoryOf(hashErr)
			if classified && category.Retryable() {
				// The backend was not able to ANSWER. See "A check that
				// could not be completed" in this file's package doc: a
				// retryable failure that outlasted its own retry budget
				// says the network is down, not that this artifact is
				// bad, so no verdict is recorded here at all and no hash
				// is attached to a row nothing compared.
				return verifyOutcome{}, &unfinishedCheck{err: hashErr}
			}
			reason := hashErr.Error()
			if classified {
				reason = fmt.Sprintf("%s: %v", category, hashErr)
			}
			return verifyOutcome{
				to:     Failed,
				detail: fmt.Sprintf("hash verification required (sha256) but the backend could not supply a comparable remote hash: %s", reason),
				hashes: hashes,
			}, nil
		}

		if !strings.EqualFold(remoteHash, localHash) {
			return verifyOutcome{
				to:     Quarantined,
				detail: fmt.Sprintf("sha256 mismatch: local file hashes to %s, remote reports %s", localHash, remoteHash),
				hashes: hashes,
			}, nil
		}
	default:
		// config.Validate restricts Hash to "" or "sha256"; a caller that
		// hands Verify an unvalidated config.Validation is a bug, and this
		// refuses to guess rather than silently skip the check.
		return verifyOutcome{
			to:     Failed,
			detail: fmt.Sprintf("unsupported hash policy %q; config.Validate should have rejected this", cfg.Hash),
			hashes: hashes,
		}, nil
	}

	// Layer 3: application validation, gated by an optional validator.
	//
	// ResolvedCommand, never cfg.Command directly: a ValidatorID with no
	// Command is the one combination that must not read as "no validator
	// was configured, carry on", and that rule belongs to config.Validation
	// so both this path and internal/app's operator-triggered revalidation
	// get it from the same place (see the accessor's own doc).
	//
	// Failed, not Quarantined, for the same reason runValidator's own
	// "could not be run at all" branch below is: this is an infrastructure
	// condition, not the validator forming an opinion about the artifact's
	// content. Neither state can reach Committed (machine.go's Transitions
	// table), so remote deletion is blocked either way.
	command, err := cfg.ResolvedCommand()
	if err != nil {
		return verifyOutcome{
			to:     Failed,
			detail: fmt.Sprintf("%v; refusing to treat a configured validator as absent", err),
			hashes: hashes,
		}, nil
	}
	if command == nil {
		return verifyOutcome{to: Verified, detail: "transfer and configured checks passed", hashes: hashes}, nil
	}

	passed, detail, err := runValidator(ctx, *command, rec.LocalPath)
	if err != nil {
		if isCancellation(err) {
			return verifyOutcome{}, err
		}
		return verifyOutcome{
			to:     Failed,
			detail: fmt.Sprintf("application validator %q: %v", command.Executable, err),
			hashes: hashes,
		}, nil
	}
	validation := &state.ValidationUpdate{Passed: passed, Detail: detail}
	if !passed {
		// FR-13: a required validator's failure must prevent source
		// deletion. Quarantined is the state whose only exit is back to
		// Discovered (see machine.go's Transitions table), so this
		// artifact cannot reach Committed/RemoteDeletePending, and
		// therefore Transport.DeleteRemote, without the whole pipeline
		// running again from scratch.
		return verifyOutcome{
			to:         Quarantined,
			detail:     "application validator rejected the artifact",
			hashes:     hashes,
			validation: validation,
		}, nil
	}
	return verifyOutcome{to: Verified, detail: "transfer and configured checks passed", hashes: hashes, validation: validation}, nil
}

// isCancellation reports whether err represents this call being stopped
// externally (a caller's context being cancelled or timing out) rather than
// the operation itself failing. Capability-absence and every other
// classified failure must still fall through to a real Failed/Quarantined
// verdict, so this must never match anything but an actual cancellation.
//
// A classified error answers for itself and the raw check never runs on it.
// That ordering is the whole point rather than a tidiness preference:
// transport.Error keeps its cause reachable through Unwrap, so an error a
// transport already looked at and called Transient still answers
// errors.Is(err, context.DeadlineExceeded) if a deadline is what it was
// underneath. A connect timeout rclone imposed on itself is exactly that
// shape (see transport/rclone.ClassifyCtx and issue #388), and reading it
// here as a stop request would leave the journal at VERIFYING with no
// verdict at all, for a failure nobody asked for and everybody wants
// recorded.
//
// The raw check is still the right answer for an error nothing classified,
// which is what a transport implementation returning ctx.Err() unwrapped
// produces, and what runValidator returns when the context it was given
// expires.
func isCancellation(err error) bool {
	if err == nil {
		return false
	}
	if category, ok := transport.CategoryOf(err); ok {
		return category == transport.Cancelled
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// readAndHashLocal opens path, reads it to completion, and returns how many
// bytes were read and their SHA-256, in hex. A single pass serves both "the
// local file opens and reads" and the hash computation layer 2 may need,
// rather than reading the file twice.
func readAndHashLocal(path string) (bytesRead int64, sha256Hex string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("local file %q: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return n, "", fmt.Errorf("reading local file %q failed after %d bytes: %w", path, n, err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// expectedSize returns the size the local file is supposed to have, drawn
// from whichever of the two records that carry one is available: the
// transfer step's own report of what it wrote, preferred because it is
// closer in time to what is on disk right now, or failing that the remote
// identity captured at discovery.
func expectedSize(rec state.Record) (size int64, known bool) {
	if rec.Transfer != nil {
		return rec.Transfer.BytesTransferred, true
	}
	if rec.Remote.Size != nil {
		return *rec.Remote.Size, true
	}
	return 0, false
}

// remoteHashWithRetry asks the backend for remotePath's SHA-256, retrying a
// bounded number of times if the failure is transport.Transient (a network
// blip talking to the backend), and giving up immediately for anything
// else, including transport.UnsupportedCapability: retrying a fixed
// property of the backend cannot change the answer.
func remoteHashWithRetry(ctx context.Context, tr transport.Transport, source transport.Source, remotePath string) (string, error) {
	var hash string
	err := retry.Do(ctx, remoteHashRetryPolicy, nil, func(ctx context.Context) error {
		h, err := tr.RemoteHash(ctx, source, remotePath, transport.SHA256)
		if err != nil {
			return err
		}
		hash = h
		return nil
	})
	return hash, err
}

// runValidator runs cmd against localPath and reports its verdict.
//
// The contract with decide, the only caller: err is non-nil only when the
// validator could not be run at all (bad executable, permission denied to
// exec it, ...) or when the call was cancelled by the outer context, both
// infrastructure conditions distinct from the validator having an opinion.
// Once the process actually starts, whatever happens to it next, a clean
// non-zero exit or being killed for exceeding its own timeout, is the
// validator's verdict, reported through passed/detail with a nil error:
// FR-13 treats a required validator's failure to answer at all the same as
// an explicit "no" (fail closed), never as license to proceed.
//
// The validator is treated as untrusted throughout: it gets a minimal,
// fixed environment rather than this process's own (which could carry
// secrets this manager holds, such as the sftp private key path or ambient
// credentials), it runs in its own process group so the timeout can kill
// any helper it spawned along with it, and its output is captured only as
// an opaque, size-bounded detail string, never parsed as anything but bytes
// to show a human.
func runValidator(ctx context.Context, cmd config.Command, localPath string) (passed bool, detail string, err error) {
	timeout := cmd.Timeout.Duration()
	if timeout <= 0 {
		return false, "", fmt.Errorf("timeout must be positive, got %s", cmd.Timeout)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := exec.CommandContext(runCtx, cmd.Executable, localPath)

	// A fixed, minimal environment: never the ambient os.Environ() this
	// process is running with. RCLONE_MANAGER_ARTIFACT_PATH duplicates
	// argv[1] for a validator that prefers reading its target from the
	// environment; the config schema (FR-13) does not specify an argument
	// convention, so this file picks one and offers both.
	c.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"RCLONE_MANAGER_ARTIFACT_PATH=" + localPath,
	}

	// A fresh process group, so the timeout below can kill whatever the
	// validator spawned along with it, not just the validator itself.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		// Negative pid: signal the whole group Setpgid created above.
		if killErr := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			return killErr
		}
		return nil
	}
	// Bounds how long Wait waits for stdio to drain after the group is
	// killed, in case a grandchild somehow escaped the group and is still
	// holding a pipe end open.
	c.WaitDelay = 5 * time.Second

	out := &boundedWriter{limit: maxValidatorOutput}
	c.Stdout = out
	c.Stderr = out

	runErr := c.Run()
	detail = out.String()

	// Classification reads the contexts' own Err() directly rather than
	// inspecting runErr's type or wrapped chain. That is deliberate: once
	// c.Cancel (above) kills the process promptly, Go's exec package is not
	// guaranteed to surface ctx.Err() as the returned error at all (it may
	// report a plain *exec.ExitError for the signal-kill instead, depending
	// on exactly how quickly the process died relative to WaitDelay), so
	// asking the contexts directly is the only reliable signal.
	switch {
	case runErr == nil:
		return true, detail, nil
	case ctx.Err() != nil:
		// The OUTER context ended this call, an explicit cancellation or a
		// pipeline-level deadline, not this validator's own configured
		// timeout. A stop request, not a verdict: propagate it so the
		// caller treats this exactly like Transfer treats cancellation.
		return false, "", fmt.Errorf("cancelled: %w", ctx.Err())
	case runCtx.Err() != nil:
		// Only runCtx, derived from cmd.Timeout, is done: this validator's
		// own timeout fired while the outer context was still fine. It did
		// not answer in time. Fail closed rather than treat "we don't
		// know" as a pass.
		return false, detail + fmt.Sprintf("\n(rclone-manager: validator killed after exceeding its %s timeout)", timeout), nil
	default:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// The process started and ran to some conclusion (a clean
			// non-zero exit): a verdict, not an infrastructure failure.
			return false, detail, nil
		}
		// The process never started at all.
		return false, "", fmt.Errorf("could not start: %w", runErr)
	}
}

// boundedWriter caps how many bytes it retains, while still reporting every
// write as fully accepted so an untrusted process that writes past the
// limit never blocks or errors on stdout/stderr; the excess is simply
// discarded rather than kept.
type boundedWriter struct {
	buf   bytes.Buffer
	limit int
}

// Write always reports the full length as accepted, even for the bytes it
// throws away.
//
// That is the whole point of the type. The writer is on the far end of a
// pipe from an untrusted process, and reporting a short write or an error
// would surface to that process as a failed write or a blocked pipe, which
// changes its behaviour: a validator that logs verbosely would start failing
// for reasons that have nothing to do with the artifact it was asked about.
// So the excess is discarded silently and the process is never told.
func (w *boundedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		w.buf.Write(p[:room])
	}
	return len(p), nil
}

// String marks a truncated buffer as truncated.
//
// Without the marker, output that was cut off is indistinguishable from
// output that simply ended, and the difference matters exactly when it is
// least obvious: a validator whose real error message arrived after the
// limit would leave a journal detail that reads as a complete but
// unhelpful explanation.
func (w *boundedWriter) String() string {
	if w.buf.Len() >= w.limit {
		return w.buf.String() + "... (truncated)"
	}
	return w.buf.String()
}
