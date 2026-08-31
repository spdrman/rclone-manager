package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// ValidateResult is `backup-manager validate <artifact-id>`'s use case
// output.
type ValidateResult struct {
	Artifact model.ArtifactID

	// Checked is false when there was nothing to check at all: no local
	// final file recorded, or no local file left on disk (both of those
	// are still failures, not a "checked: false" no-op; see
	// ValidateArtifact's doc). It exists mainly so a caller can tell "the
	// recorded hash baseline was empty and no restore-test command is
	// configured, so the only thing checked was that the file still
	// exists at all" apart from a fuller check having actually run.
	Checked bool
	Passed  bool
	Reason  string

	// NewState is set only when a failed check routed the artifact into
	// quarantine (Quarantined or QuarantinedLost); it is the zero value on
	// a pass, since a pass never moves the artifact anywhere.
	NewState lifecycle.State
}

// ValidateArtifact is `backup-manager validate <artifact-id>`'s use case:
// an operator-triggered, on-demand re-check of one already-committed
// artifact's durable local copy, right now, regardless of Phase 4's
// scheduled-revalidation cadence (internal/revalidate.Run, which this does
// not call: that package's Run/SelectDue are gated behind
// config.Revalidation.Interval/MaxPerCycle being configured at all for the
// backup set, and are meant for a due-based background sweep, not an
// operator naming one specific artifact id on demand).
//
// # What it checks
//
// It mirrors the durable-local-copy check every other package that has
// ever needed one keeps its own small copy of (internal/lifecycle/verify.go's
// readAndHashLocal, internal/reconcile/localcheck.go's checkLocalFinal,
// internal/revalidate/checks.go's recomputeLocalHash): does the recorded
// local final file still exist, and if a local hash was recorded at
// VERIFIED, does it still match. That much runs unconditionally, and needs
// no config to be meaningful, unlike Phase 4's revalidation which is
// opt-in per backup set. On top of that, when the owning backup set's FR-13
// config.Validation.Command is configured, ValidateArtifact reruns it too
// (lifecycle.RunRestoreCheck, the same untrusted-subprocess contract FR-13
// already established), so `validate` can prove an artifact still actually
// restores, not merely that its bytes are unchanged.
//
// # Which artifacts this accepts
//
// Only COMMITTED, REMOTE_DELETE_PENDING or COMPLETE: the same "a durable
// local copy has actually landed" set internal/health's decideState,
// internal/retention's gfsIsManagedComplete and internal/revalidate's
// eligibleStates all already agree on. Anything else (still in flight, or
// already FAILED/QUARANTINED/QUARANTINED_LOST) is refused outright: there
// is either no durable copy yet to check, or the artifact has already been
// routed to wherever it needs to go.
//
// # No --dry-run, on purpose
//
// Unlike `fetch` and `retention`, `validate` is not gated behind a dry-run
// flag. A failed check has a real, but protective rather than destructive,
// consequence: it quarantines the artifact (the exact same
// COMMITTED/REMOTE_DELETE_PENDING -> QUARANTINED, COMPLETE ->
// QUARANTINED_LOST routing internal/reconcile and internal/revalidate
// already use for "the durable local copy was found invalid after the
// fact"), which preserves evidence and asks for human attention rather
// than deleting anything. A passing check writes nothing at all: unlike
// internal/revalidate.Run's same-state "pass" audit write (which exists to
// reset that package's own due-ness clock, a concept `validate` has no
// use for since it is not on any schedule), there is no due-ness clock
// here to reset, so a clean result has no side effect to record.
func (s *Service) ValidateArtifact(ctx context.Context, id model.ArtifactID) (ValidateResult, error) {
	rec, err := s.Journal.Get(ctx, id)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("app: validate: %w", err)
	}

	cur := lifecycle.State(rec.State)
	switch cur {
	case lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.Complete:
		// eligible
	default:
		return ValidateResult{Artifact: id}, fmt.Errorf(
			"app: validate: %s is %s, not a durable restore point (COMMITTED, REMOTE_DELETE_PENDING or COMPLETE)", id, cur)
	}

	_, bs, ok := s.backupSetConfigFor(id.Set)
	if !ok {
		return ValidateResult{}, fmt.Errorf("app: validate: %s has no configured backup set", id.Set)
	}

	checks, err := s.runValidationChecks(ctx, rec, bs.Validation)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("app: validate: %w", err)
	}

	result := ValidateResult{Artifact: id, Checked: checks.Checked, Passed: checks.Passed, Reason: checks.Reason}
	if checks.Passed {
		return result, nil
	}

	to := lifecycle.Quarantined
	if cur == lifecycle.Complete {
		to = lifecycle.QuarantinedLost
	}
	out, err := lifecycle.Advance(ctx, s.lifecycleDeps(), state.Transition{
		Artifact: id,
		Key:      fmt.Sprintf("app:validate:%s:%s", id, s.now().Format(time.RFC3339Nano)),
		From:     string(cur),
		To:       string(to),
		Detail:   "operator-triggered validate: " + checks.Reason,
	})
	if err != nil {
		return result, fmt.Errorf("app: validate: quarantining %s: %w", id, err)
	}
	result.NewState = lifecycle.State(out.Record.State)
	return result, nil
}

// runValidationChecks runs the local-file check (always) and the backup
// set's application validator (only when one is configured) against rec,
// and reports a combined verdict.
//
// It takes the whole config.Validation, not the *config.Command, so that
// the one combination that must never be read as "no validator
// configured" -- a ValidatorID that nothing resolved into a runnable
// Command -- is refused here too and not only in internal/lifecycle's
// verify path. FR-14's operator-triggered `validate` would otherwise
// report an artifact as passing without ever running the validator its
// backup set names, which is the same fail-open outcome FR-13 exists to
// prevent, reached through the other door. The rule itself lives on
// config.Validation (ResolvedCommand), so both consumers get it from one
// place rather than each re-implementing it.
//
// An unresolved validator is an error, not a failed verdict: it says
// nothing about the artifact, and quarantining a durable restore point
// over what is an infrastructure or wiring problem would be its own kind
// of damage. The caller gets a refusal and the artifact is left exactly
// as it was, which is how the restore-test hook's own error is already
// handled below.
func (s *Service) runValidationChecks(ctx context.Context, rec state.Record, validation config.Validation) (checkOutcome, error) {
	cmd, err := validation.ResolvedCommand()
	if err != nil {
		return checkOutcome{}, err
	}

	var reasons []string
	out := checkOutcome{Passed: true}

	if rec.LocalPath == "" {
		return checkOutcome{Checked: true, Reason: "no local final path is recorded in the journal"}, nil
	}
	info, statErr := os.Stat(rec.LocalPath)
	if statErr != nil {
		return checkOutcome{Checked: true, Reason: fmt.Sprintf("local final file %s: %v", rec.LocalPath, statErr)}, nil
	}
	out.Checked = true

	if rec.LocalHashAlg != "" {
		if !strings.EqualFold(rec.LocalHashAlg, string(transport.SHA256)) {
			return checkOutcome{Checked: true, Reason: fmt.Sprintf("cannot verify local identity: unsupported recorded hash algorithm %q", rec.LocalHashAlg)}, nil
		}
		sum, hashErr := sha256File(rec.LocalPath)
		if hashErr != nil {
			return checkOutcome{Checked: true, Reason: fmt.Sprintf("hashing %s: %v", rec.LocalPath, hashErr)}, nil
		}
		if !strings.EqualFold(sum, rec.LocalHash) {
			out.Passed = false
			reasons = append(reasons, fmt.Sprintf(
				"local final file %s now hashes to %s, but the %s hash recorded at verification was %s",
				rec.LocalPath, sum, rec.LocalHashAlg, rec.LocalHash))
		} else {
			out.HashMatched = true
			reasons = append(reasons, "recomputed hash still matches the hash recorded at verification")
		}
	} else {
		reasons = append(reasons, fmt.Sprintf("local final file present, %d bytes (no recorded hash to compare against)", info.Size()))
	}

	if cmd != nil {
		result, hookErr := lifecycle.RunRestoreCheck(ctx, *cmd, rec.LocalPath)
		if hookErr != nil {
			return checkOutcome{Checked: out.Checked}, fmt.Errorf("restore-test hook: %w", hookErr)
		}
		if !result.Passed {
			out.Passed = false
			reasons = append(reasons, "restore-test hook failed: "+result.Detail)
		} else {
			out.ValidatorPassed = true
			reasons = append(reasons, "restore-test hook passed")
		}
	}

	out.Reason = strings.Join(reasons, "; ")
	return out, nil
}

// checkOutcome is one runValidationChecks call's full result.
//
// Checked, Passed and Reason are the combined verdict every caller has
// always used. HashMatched and ValidatorPassed are the per-tier verdicts
// underneath it, and they exist because the combined Passed is not enough
// to decide whether an artifact may be trusted AGAIN after quarantine
// (issue #220): the local-file check runs unconditionally, so an artifact
// with no recorded hash baseline and no configured validator "passes" on
// nothing more than the file still being present, which is a pass that
// could not have failed on content. See
// lifecycle.ReinstatementEvidence, which is what these two feed.
type checkOutcome struct {
	Checked bool
	Passed  bool
	Reason  string

	// HashMatched is true when a hash baseline recorded at VERIFIED
	// existed, the durable local copy was re-hashed now, and the two
	// still agree.
	HashMatched bool

	// ValidatorPassed is true when the backup set's configured
	// application validator actually ran in this call and passed. It is
	// false both when no validator is configured and when one ran and
	// failed, which is exactly right for its one use: only a validator
	// that ran and passed is evidence.
	ValidatorPassed bool
}

// evidence renders o as what lifecycle.ReinstateFromQuarantine takes.
func (o checkOutcome) evidence() lifecycle.ReinstatementEvidence {
	return lifecycle.ReinstatementEvidence{
		HashMatched:     o.HashMatched,
		ValidatorPassed: o.ValidatorPassed,
		AnyCheckFailed:  !o.Passed,
		Summary:         o.Reason,
	}
}

// sha256File duplicates the small (open, io.Copy into a hasher, hex-encode)
// helper that already exists, independently, in internal/lifecycle's
// verify.go, internal/reconcile's localcheck.go and
// internal/revalidate's checks.go: each package that has ever needed this
// has kept its own copy rather than reach across a package boundary for
// ten lines (see revalidate's own recomputeLocalHash doc, which names this
// as the established convention), and this package follows the same,
// already-established one.
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

// ParseArtifactID parses the "source/set/name" rendering
// model.ArtifactID.String() produces back into an ArtifactID, the form
// `validate <artifact-id>` takes its one positional argument in.
func ParseArtifactID(s string) (model.ArtifactID, error) {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 3 {
		return model.ArtifactID{}, fmt.Errorf("app: artifact id %q must have the form source/backup-set/name", s)
	}
	set, err := model.NewBackupSetID(parts[0], parts[1])
	if err != nil {
		return model.ArtifactID{}, fmt.Errorf("app: artifact id %q: %w", s, err)
	}
	return model.NewArtifactID(set, parts[2])
}
