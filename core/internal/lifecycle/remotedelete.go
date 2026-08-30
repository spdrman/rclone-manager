package lifecycle

// This file is FR-15: the one call site in this whole codebase that is
// allowed to invoke transport.Transport.DeleteRemote. Everything else this
// package does exists to make committing a local file safe; this is the
// step that makes deleting the remote copy of it safe too, and it is the
// most dangerous line in the project on purpose, it destroys data.
//
// DeleteRemote owns exactly the transition FR-15 names:
//
//	COMMITTED -> REMOTE_DELETE_PENDING -> COMPLETE
//
// and, per FR-15, it revalidates four things immediately before issuing the
// delete, from scratch, every single time, never trusting that an earlier
// pass already checked them:
//
//  1. the journal artifact is COMMITTED or REMOTE_DELETE_PENDING (the only
//     two states this transition may legally start from, see machine.go);
//  2. the expected local final file exists;
//  3. its local identity/size is consistent with what the journal recorded;
//  4. the remote object still corresponds to what was captured at
//     discovery, via model.CompareIdentity (FR-16). This package does not
//     reimplement that comparison; it only supplies the two RemoteIdentity
//     values to compare and honours model.IdentityComparison.Preserve().
//
// Any one of these failing refuses the delete. Nothing here ever calls
// transport.Transport.DeleteRemote except after all four hold.
//
// # Why this usually refuses, and why that is not a bug
//
// model.CompareIdentity can only reach ConfidenceStrong on an unchanged
// verdict through a hash match, a backend stable-identifier match, or an
// outright mismatch on path/size/mtime. Every other outcome, including
// "size and modification time both agree, nothing else was available",
// only reaches ConfidenceWeak or ConfidenceNone, and
// IdentityComparison.Preserve() is true for both.
//
// docs/ssh-setup.md recommends a hardened, shell-less SFTP account (a
// forced internal-sftp subsystem, no login shell). rclone's sftp backend
// computes remote hashes by running a hash command (sha1sum, md5sum, ...)
// over the SSH session, which requires exactly the shell access that
// account posture removes. Against the project's own recommended
// deployment, then, the remote side of every comparison this function runs
// routinely carries no hash and no backend stable identifier, so the best
// it can usually do is ConfidenceWeak on an Unconfirmed verdict, size and
// mtime agree, nothing rules out a same-second replacement, and
// Preserve() comes back true.
//
// That means the common, expected, correctly-functioning outcome in that
// deployment is DeleteRemote refusing every delete it is asked to perform.
// This is not a defect to route around; it is FR-16's stated policy
// ("identity cannot be established with sufficient confidence: preserve
// the remote object") doing exactly its job. But it has a real operational
// consequence this package cannot paper over: an archive that never prunes
// its remote side will fill the source disk, eventually, on a long enough
// timeline, in every deployment that follows the project's own hardening
// advice. That failure has to be loud, not discovered when the source
// volume is full. So every refusal here is returned as a typed
// *RemoteDeleteRefusalError a caller can distinguish with errors.As (never
// a bare error a caller might log-and-ignore), and every refusal that
// happens after intent to delete was already durably recorded is also
// written back into the journal's remote_delete_error column via a
// same-state Deletion update, so it shows up in a direct query against the
// journal, not only in whatever log line happened to be emitted the moment
// it occurred. Turning that persisted signal into an actual alert (a count
// of artifacts stuck in REMOTE_DELETE_PENDING past some age, surfaced
// through FR-24's health reporting) is future work this package does not
// own; what it owns is making sure the signal exists to be alerted on.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// DeleteRemoteRequest is everything one attempt at FR-15's delete
// transition needs.
type DeleteRemoteRequest struct {
	// Source is the remote the artifact was discovered on.
	Source transport.Source

	// Artifact identifies the journal row this attempt is acting on.
	Artifact model.ArtifactID

	// AttemptKey is this attempt's idempotency seed. DeleteRemote may make
	// up to three durable journal writes in one call (record intent,
	// record a refusal or delete failure, record COMPLETE); it derives
	// each one's own state.Transition.Key by suffixing AttemptKey with a
	// fixed, distinct tag, so a single value covers all of them.
	//
	// Per state.Transition's own Key contract, AttemptKey must be derived
	// deterministically per logical attempt (artifact id plus a
	// persisted, monotonically increasing attempt counter is the pattern
	// the journal package itself documents), never generated fresh on
	// every call. A fresh key on a retry defeats the idempotency guarantee
	// this whole design depends on: it would let a crash-and-restart
	// re-issue a delete that already landed. A caller that wants a new
	// attempt to actually re-run every check (for example after fixing
	// whatever caused a prior refusal) mints a new AttemptKey on purpose;
	// a caller retrying purely because the process restarted reuses the
	// same one it used before.
	AttemptKey string

	// Completion is the backup set's FR-8 completion configuration
	// (WP3.2, docs/EPIC-B-multi-nas.md §26 Step 3, §71 Work Package 3.2).
	// DeleteRemote reads only Completion.Strategy and
	// Completion.DeleteSafetyDelay from it, and only to decide whether an
	// extra deletion-safety delay gates this attempt: see the "stable
	// completion safety delay" revalidation below.
	//
	// The zero value (Strategy == "") is treated exactly like "rename" or
	// "marker": no extra delay is required. That is deliberate, not an
	// oversight -- a caller that does not populate this field is opting
	// out of a dimension of behavior it was never asking DeleteRemote to
	// have an opinion about, exactly the same convention this package
	// already uses for config.Validation's zero value in verify.go. In
	// production this can never silently hide a real "stable" backup set:
	// config.Validate refuses to let Completion.Strategy ever be empty in
	// a validated config, so the only caller in this codebase that
	// reaches DeleteRemote in production (internal/app/pipeline.go)
	// always has a real, non-empty strategy to hand it.
	Completion config.Completion
}

const (
	deleteAttemptTagIntent   = "remote-delete-pending"
	deleteAttemptTagRefused  = "remote-delete-refused"
	deleteAttemptTagComplete = "remote-delete-complete"
)

func (r DeleteRemoteRequest) key(tag string) string {
	return r.AttemptKey + ":" + tag
}

// RemoteDeleteRefusalError reports that DeleteRemote declined to delete a
// remote object because one of FR-15's revalidation checks did not pass.
// This is the type every caller of DeleteRemote should look for with
// errors.As (AsRemoteDeleteRefusal does exactly that) to tell "the delete
// was refused on purpose because a safety check did not clear" apart from
// an operational failure (a journal write error, a network error talking
// to the remote). See this file's package doc for why the remote-identity
// check specifically is expected to produce this refusal routinely against
// a hardened SFTP account, not just during a genuine incident.
type RemoteDeleteRefusalError struct {
	Artifact model.ArtifactID

	// Check names which FR-15 revalidation check refused the delete:
	// "journal state", "local file", "remote identity", or "remote
	// delete" (the transport call itself failed after every check
	// upstream of it passed).
	Check string

	// Reason is a short, human-readable explanation suitable for a log
	// line or an audit trail. For a "remote identity" refusal this is
	// model.IdentityComparison.Reason verbatim.
	Reason string

	// HasComparison, Verdict and Confidence carry the exact
	// model.CompareIdentity outcome that produced this refusal. They are
	// only meaningful when Check == "remote identity" and HasComparison
	// is true; a caller that wants to tell "confirmed changed" (a possible
	// incident: something wrote where this artifact's remote copy used to
	// be) apart from "could not confirm" (routinely expected against a
	// hardened SFTP account, see the package doc) reads Verdict rather
	// than parsing Reason.
	HasComparison bool
	Verdict       model.Verdict
	Confidence    model.Confidence
}

func (e *RemoteDeleteRefusalError) Error() string {
	if e.HasComparison {
		return fmt.Sprintf(
			"lifecycle: refusing to delete the remote object for %s: %s check failed (verdict=%s, confidence=%s): %s",
			e.Artifact, e.Check, e.Verdict, e.Confidence, e.Reason,
		)
	}
	return fmt.Sprintf("lifecycle: refusing to delete the remote object for %s: %s check failed: %s", e.Artifact, e.Check, e.Reason)
}

// AsRemoteDeleteRefusal reports whether err is, or wraps, a
// *RemoteDeleteRefusalError, and returns it. Use this rather than a type
// switch so a caller keeps working even if DeleteRemote starts wrapping the
// refusal inside additional context in the future.
func AsRemoteDeleteRefusal(err error) (*RemoteDeleteRefusalError, bool) {
	var refusal *RemoteDeleteRefusalError
	ok := errors.As(err, &refusal)
	return refusal, ok
}

// DeleteRemote is the only call this package makes to
// transport.Transport.DeleteRemote, and the only place FR-15's revalidation
// runs. See the package doc above for the full policy and its operational
// consequence against a hardened SFTP account.
//
// On success it returns the Outcome of the COMPLETE transition. On refusal
// it returns a *RemoteDeleteRefusalError (see AsRemoteDeleteRefusal) and
// leaves the journal at whatever state correctly reflects what actually
// happened: unchanged if the refusal was caught before any write (a wrong
// journal state, a bad local file), or at REMOTE_DELETE_PENDING with the
// refusal durably recorded in remote_delete_error if it was caught after
// intent to delete was already recorded. It never leaves an artifact
// reachable at REMOTE_DELETE_PENDING without COMMITTED as its
// (transitive) predecessor, because Advance enforces that regardless of
// what this function does.
func DeleteRemote(ctx context.Context, d Deps, req DeleteRemoteRequest) (state.Outcome, error) {
	if d.Journal == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: DeleteRemote needs a Journal")
	}
	if d.Transport == nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: DeleteRemote needs a Transport")
	}
	if req.AttemptKey == "" {
		return state.Outcome{}, fmt.Errorf("lifecycle: DeleteRemote requires a non-empty AttemptKey")
	}

	rec, err := d.Journal.Get(ctx, req.Artifact)
	if err != nil {
		return state.Outcome{}, fmt.Errorf("lifecycle: DeleteRemote: load journal record for %s: %w", req.Artifact, err)
	}

	// --- revalidation 1: the journal artifact is COMMITTED or
	// REMOTE_DELETE_PENDING. Nothing else may ever reach this transition
	// (see machine.go: RemoteDeletePending's only predecessor besides
	// itself is Committed), and this check catches a caller bug or a
	// corrupted row before it ever touches the journal again.
	current := State(rec.State)
	if current != Committed && current != RemoteDeletePending {
		return state.Outcome{}, &RemoteDeleteRefusalError{
			Artifact: req.Artifact,
			Check:    "journal state",
			Reason:   fmt.Sprintf("journal records %s, which is not COMMITTED or REMOTE_DELETE_PENDING", rec.State),
		}
	}

	// --- revalidation 2 & 3: the expected local final file exists and its
	// size/identity is consistent with what the journal recorded. This is
	// a pure, side-effect-free filesystem check, so it runs before any
	// journal write: there is no reason to record intent to delete the
	// remote copy of a local file that may not even be there.
	if err := verifyLocalFinal(rec); err != nil {
		return state.Outcome{}, &RemoteDeleteRefusalError{
			Artifact: req.Artifact,
			Check:    "local file",
			Reason:   err.Error(),
		}
	}

	// --- revalidation (WP3.2): a "stable" completion strategy only ever
	// confirmed a size/mtime heuristic, never a producer completion
	// signal the way "rename"/"marker" do (see internal/discovery/
	// complete.go), so it may not be treated as equivalent to them at this
	// gate without an additional deletion-safety delay having elapsed
	// since this artifact last reached a confirmed-good journal state
	// (req.Completion.DeleteSafetyDelay; config.Validate requires this to
	// be set whenever Strategy == "stable"). rec.UpdatedAt is the closest
	// thing this journal row has to "when was this record last confirmed
	// good": it is set by the COMMITTED write itself and, for an artifact
	// still sitting at COMMITTED, is left untouched by anything else
	// except a later same-state pass (internal/revalidate's own scheduled
	// re-check, or this very check refusing again).
	//
	// Every other strategy, and a zero-value Completion a caller did not
	// populate (see DeleteRemoteRequest.Completion's own doc), skips this
	// entirely.
	//
	// Like the local-file check just above, this is a pure,
	// side-effect-free comparison that runs before any journal write:
	// refusing here leaves rec.UpdatedAt exactly where it was, so a
	// stable-strategy artifact that keeps getting refused on every cycle
	// never has its own safety clock reset by the refusal itself, only by
	// a genuine journal event. The practical effect of that refusal is
	// exactly what WP3.2's behavioral contract asks for: the remote
	// source is preserved (nothing here calls the transport), and the
	// artifact is left exactly where internal/revalidate's own SelectDue
	// already treats COMMITTED/REMOTE_DELETE_PENDING artifacts as
	// eligible for a scheduled re-check, so an operator with revalidation
	// configured for this backup set gets it re-examined without this
	// package having to teach internal/revalidate anything new about
	// WP3.2 at all.
	if req.Completion.Strategy == "stable" {
		delay := req.Completion.DeleteSafetyDelay.Duration()
		elapsed := d.now().Sub(rec.UpdatedAt)
		if elapsed < delay {
			return state.Outcome{}, &RemoteDeleteRefusalError{
				Artifact: req.Artifact,
				Check:    "stable completion safety delay",
				Reason: fmt.Sprintf(
					"completion strategy \"stable\" only confirms size/mtime stability, not producer-confirmed completion; only %s of the required %s deletion-safety delay has elapsed since this artifact last reached a confirmed-good state",
					elapsed.Round(time.Second), delay,
				),
			}
		}
	}

	// --- record intent, strictly before any delete call, exactly as
	// crash_safety.go's COMMITTED -> REMOTE_DELETE_PENDING walkthrough
	// requires. If this call is itself a retry that starts already at
	// REMOTE_DELETE_PENDING (the process crashed after a previous
	// attempt recorded intent), there is nothing to (re)record: Advance
	// would treat RemoteDeletePending -> RemoteDeletePending as the
	// idempotent no-op it is, and doing so here would only spend an
	// idempotency key for no reason.
	if current == Committed {
		if _, err := Advance(ctx, d, state.Transition{
			Artifact: req.Artifact,
			Key:      req.key(deleteAttemptTagIntent),
			From:     rec.State,
			To:       string(RemoteDeletePending),
			Detail:   "FR-15: recording intent to delete the remote object before revalidating its identity",
		}); err != nil {
			return state.Outcome{}, fmt.Errorf("lifecycle: DeleteRemote: record intent: %w", err)
		}
	}

	// --- revalidation 4: the remote object still corresponds to what was
	// captured at discovery (FR-16). This is the check crash_safety.go
	// says must be re-run on every attempt, including a restart that
	// finds the journal already at REMOTE_DELETE_PENDING, never skipped
	// just because an earlier attempt already got this far.
	discovered, err := discoveredIdentity(rec)
	if err != nil {
		return state.Outcome{}, d.refuseRemoteIdentity(ctx, req, "remote identity", err.Error(), false, model.IdentityComparison{})
	}

	remote, err := d.Transport.Stat(ctx, req.Source, rec.RemotePath)
	if err != nil {
		return state.Outcome{}, d.refuseRemoteIdentity(ctx, req, "remote identity",
			fmt.Sprintf("could not re-confirm the remote object before deleting: %v", err), false, model.IdentityComparison{})
	}

	comparison := model.CompareIdentity(discovered, currentIdentity(remote))
	if comparison.Preserve() {
		return state.Outcome{}, d.refuseRemoteIdentity(ctx, req, "remote identity", comparison.Reason, true, comparison)
	}

	// Every revalidation cleared. This is the one call site in the whole
	// project allowed to reach here.
	if err := d.Transport.DeleteRemote(ctx, req.Source, rec.RemotePath); err != nil {
		refusalErr := fmt.Errorf("lifecycle: DeleteRemote: transport delete failed for %s: %w", req.Artifact, err)
		if persistErr := persistDeleteOutcome(ctx, d, req, req.key(deleteAttemptTagRefused),
			"FR-15: the remote delete call itself failed", err.Error(), nil); persistErr != nil {
			return state.Outcome{}, fmt.Errorf("%w (and failed to record the failure: %v)", refusalErr, persistErr)
		}
		return state.Outcome{}, refusalErr
	}

	now := d.now()
	outcome, err := Advance(ctx, d, state.Transition{
		Artifact: req.Artifact,
		Key:      req.key(deleteAttemptTagComplete),
		From:     string(RemoteDeletePending),
		To:       string(Complete),
		Detail:   "FR-15: remote object confirmed unchanged since discovery and deleted",
		Deletion: &state.DeletionUpdate{DeletedAt: &now},
	})
	if err != nil {
		// The remote delete has already happened at this point (see this
		// file's package doc and crash_safety.go's REMOTE_DELETE_PENDING
		// -> COMPLETE walkthrough): a crash or journal error here leaves
		// the artifact at REMOTE_DELETE_PENDING with the remote object
		// already gone. That is FR-17's reconciliation problem to close
		// out ("absent, final, REMOTE_DELETE_PENDING -> reconcile
		// COMPLETE", issue #18), not something this function can safely
		// paper over by guessing.
		return state.Outcome{}, fmt.Errorf("lifecycle: DeleteRemote: the remote object was deleted but recording COMPLETE failed, reconciliation must confirm it independently: %w", err)
	}
	return outcome, nil
}

// refuseRemoteIdentity builds the *RemoteDeleteRefusalError for a
// remote-identity failure and, since intent to delete has already been
// durably recorded by the time this runs, persists the refusal into the
// journal's remote_delete_error column so it is visible to anyone
// inspecting the journal directly, not only to whatever caught this
// function's return value. See the package doc for why this observability
// matters: against a hardened SFTP account this is the expected, routine
// outcome, not a rare failure, and it must never be silent.
func (d Deps) refuseRemoteIdentity(ctx context.Context, req DeleteRemoteRequest, check, reason string, hasComparison bool, comparison model.IdentityComparison) error {
	refusal := &RemoteDeleteRefusalError{
		Artifact:      req.Artifact,
		Check:         check,
		Reason:        reason,
		HasComparison: hasComparison,
		Verdict:       comparison.Verdict,
		Confidence:    comparison.Confidence,
	}
	if err := persistDeleteOutcome(ctx, d, req, req.key(deleteAttemptTagRefused),
		"FR-15: refusing to delete, remote identity could not be reconfirmed", refusal.Error(), nil); err != nil {
		return fmt.Errorf("%w (and failed to record the refusal: %v)", refusal, err)
	}
	return refusal
}

// persistDeleteOutcome records deleteErr (and, on success, deletedAt) into
// the journal via a same-state transition on REMOTE_DELETE_PENDING. A
// same-state move is always legal (Validate treats current == target as an
// idempotent no-op); using it here, rather than inventing a new state,
// keeps this purely a durability/observability write; it changes no
// lifecycle state a future reconciliation pass needs to reason about.
func persistDeleteOutcome(ctx context.Context, d Deps, req DeleteRemoteRequest, key, detail, deleteErr string, deletedAt *time.Time) error {
	_, err := Advance(ctx, d, state.Transition{
		Artifact: req.Artifact,
		Key:      key,
		From:     string(RemoteDeletePending),
		To:       string(RemoteDeletePending),
		Detail:   detail,
		Deletion: &state.DeletionUpdate{Error: deleteErr, DeletedAt: deletedAt},
	})
	return err
}

// verifyLocalFinal is FR-15's second and third revalidation: the expected
// local final file exists, and its size (and, when a local hash was
// recorded at VERIFIED, its content hash) is consistent with what the
// journal recorded. This defends against the local final copy having been
// truncated, overwritten, or lost to corruption sometime between COMMITTED
// and the moment a delete is finally attempted; COMMITTED only proves the
// file was correct at the moment it was durably renamed, not that it still
// is now.
func verifyLocalFinal(rec state.Record) error {
	if rec.LocalPath == "" {
		return fmt.Errorf("no local final path is recorded for this artifact")
	}

	info, err := os.Stat(rec.LocalPath)
	if err != nil {
		return fmt.Errorf("expected local final file %s: %w", rec.LocalPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("expected local final file %s is a directory, not a file", rec.LocalPath)
	}

	expected, source, err := expectedLocalSize(rec)
	if err != nil {
		return err
	}
	if info.Size() != expected {
		return fmt.Errorf("local final file %s is %d bytes, expected %d (from %s)", rec.LocalPath, info.Size(), expected, source)
	}

	if rec.LocalHashAlg != "" {
		if !strings.EqualFold(rec.LocalHashAlg, string(transport.SHA256)) {
			return fmt.Errorf("cannot revalidate local identity: unsupported recorded local hash algorithm %q", rec.LocalHashAlg)
		}
		sum, err := sha256File(rec.LocalPath)
		if err != nil {
			return fmt.Errorf("hashing local final file %s: %w", rec.LocalPath, err)
		}
		if !strings.EqualFold(sum, rec.LocalHash) {
			return fmt.Errorf("local final file %s hash %s does not match the %s hash recorded at verification, %s", rec.LocalPath, sum, rec.LocalHashAlg, rec.LocalHash)
		}
	}

	return nil
}

// expectedLocalSize picks the size the local final file must have, from
// whichever of the journal's two independent size records are present, and
// refuses outright if they disagree with each other rather than silently
// preferring one. A caller with neither recorded gets a refusal too: FR-15
// requires size consistency to be confirmed, not assumed.
func expectedLocalSize(rec state.Record) (size int64, source string, err error) {
	haveRemote := rec.Remote.Size != nil
	haveTransfer := rec.Transfer != nil

	switch {
	case haveRemote && haveTransfer:
		if *rec.Remote.Size != rec.Transfer.BytesTransferred {
			return 0, "", fmt.Errorf("recorded remote size %d disagrees with recorded transfer size %d", *rec.Remote.Size, rec.Transfer.BytesTransferred)
		}
		return *rec.Remote.Size, "recorded remote size", nil
	case haveRemote:
		return *rec.Remote.Size, "recorded remote size", nil
	case haveTransfer:
		return rec.Transfer.BytesTransferred, "recorded transfer size", nil
	default:
		return 0, "", fmt.Errorf("no recorded size, neither the remote identity captured at discovery nor a transfer result, to confirm the local file against")
	}
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// discoveredIdentity builds the model.RemoteIdentity FR-16 comparison needs
// for the "discovered" side out of what the journal persisted at
// discovery. It refuses when no size was captured, since
// model.RemoteIdentity.Size is not optional the way the journal's own
// RemoteIdentity.Size is, and comparing against a fabricated zero would be
// worse than refusing outright.
func discoveredIdentity(rec state.Record) (model.RemoteIdentity, error) {
	if rec.Remote.Size == nil {
		return model.RemoteIdentity{}, fmt.Errorf("no size was captured for this artifact at discovery; cannot reconfirm remote identity now")
	}

	id := model.RemoteIdentity{
		Path:     rec.RemotePath,
		Size:     *rec.Remote.Size,
		Hash:     rec.Remote.Hash,
		HashAlg:  rec.Remote.HashAlg,
		StableID: rec.Remote.BackendID,
	}
	if rec.Remote.ModTime != nil {
		id.ModTime = rec.Remote.ModTime.Unix()
	}
	return id, nil
}

// currentIdentity builds the model.RemoteIdentity FR-16 comparison needs
// for the "current" side out of a fresh transport.Stat call.
func currentIdentity(art transport.RemoteArtifact) model.RemoteIdentity {
	return model.RemoteIdentity{
		Path:     art.Path,
		Size:     art.Size,
		ModTime:  art.ModTime,
		Hash:     art.Hash,
		HashAlg:  string(art.HashAlg),
		StableID: art.ID,
	}
}
