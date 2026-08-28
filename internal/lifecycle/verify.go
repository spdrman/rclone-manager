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
//     literally: it trusts an already-verified producer-supplied checksum
//     when the transfer step recorded one (state.TransferResult.Checksummed,
//     rclone's own copy-time hash comparison, which would already have
//     failed the copy outright on a mismatch), and otherwise asks the
//     backend directly via RemoteHash. If the backend cannot answer,
//     capability absent, or any other error, verification for this
//     artifact FAILS explicitly, with a detail naming the reason. It never
//     falls back to treating the already-passed transfer-verification
//     checks as "good enough": that would be exactly the silent downgrade
//     to a size check FR-13 forbids. An operator running the recommended
//     hardened SFTP setup and wanting a hard per-artifact content guarantee
//     needs an application validator instead (one that does not depend on
//     a remote hash call), not this switch.
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

	"github.com/spdrman/rclone-manager/internal/config"
	"github.com/spdrman/rclone-manager/internal/model"
	"github.com/spdrman/rclone-manager/internal/state"
	"github.com/spdrman/rclone-manager/internal/transport"
	"github.com/spdrman/rclone-manager/internal/transport/retry"
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

	out, cancelErr := decide(ctx, d, p.Source, rec, p.Validation)
	if cancelErr != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: verify: %w", cancelErr)
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
		if rec.Transfer != nil && rec.Transfer.Checksummed {
			// A producer-supplied checksum was already verified as an
			// intrinsic part of the copy (rclone's own Copy compares a
			// common hash type and fails the transfer outright on a
			// mismatch), so there is nothing left to prove.
			break
		}

		remoteHash, hashErr := remoteHashWithRetry(ctx, d.Transport, source, rec.RemotePath)
		if isCancellation(hashErr) {
			return verifyOutcome{}, hashErr
		}
		if hashErr == nil && remoteHash == "" {
			hashErr = errors.New("backend returned an empty hash with no error")
		}
		if hashErr != nil {
			reason := hashErr.Error()
			if category, ok := transport.CategoryOf(hashErr); ok {
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
	if cfg.Command == nil {
		return verifyOutcome{to: Verified, detail: "transfer and configured checks passed", hashes: hashes}, nil
	}

	passed, detail, err := runValidator(ctx, *cfg.Command, rec.LocalPath)
	if err != nil {
		if isCancellation(err) {
			return verifyOutcome{}, err
		}
		return verifyOutcome{
			to:     Failed,
			detail: fmt.Sprintf("application validator %q: %v", cfg.Command.Executable, err),
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
// the operation itself failing. It checks transport.CategoryOf first
// (retry.Do wraps a cancellation it observes as transport.Cancelled), and
// falls back to a raw context error check for a transport implementation
// that returns ctx.Err() unwrapped: capability-absence and every other
// classified failure must still fall through to a real Failed/Quarantined
// verdict, so this must never match anything but an actual cancellation.
func isCancellation(err error) bool {
	if err == nil {
		return false
	}
	if category, ok := transport.CategoryOf(err); ok && category == transport.Cancelled {
		return true
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

func (w *boundedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		w.buf.Write(p[:room])
	}
	return len(p), nil
}

func (w *boundedWriter) String() string {
	if w.buf.Len() >= w.limit {
		return w.buf.String() + "... (truncated)"
	}
	return w.buf.String()
}
