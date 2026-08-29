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

	"github.com/spdrman/rclone-manager/internal/config"
	"github.com/spdrman/rclone-manager/internal/lifecycle"
	"github.com/spdrman/rclone-manager/internal/model"
	"github.com/spdrman/rclone-manager/internal/state"
	"github.com/spdrman/rclone-manager/internal/transport"
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

	checked, passed, reason, err := s.runValidationChecks(ctx, rec, bs.Validation.Command)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("app: validate: %w", err)
	}

	result := ValidateResult{Artifact: id, Checked: checked, Passed: passed, Reason: reason}
	if passed {
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
		Detail:   "operator-triggered validate: " + reason,
	})
	if err != nil {
		return result, fmt.Errorf("app: validate: quarantining %s: %w", id, err)
	}
	result.NewState = lifecycle.State(out.Record.State)
	return result, nil
}

// runValidationChecks runs the local-file check (always) and cmd (only
// when non-nil) against rec, and reports a combined verdict.
func (s *Service) runValidationChecks(ctx context.Context, rec state.Record, cmd *config.Command) (checked, passed bool, reason string, err error) {
	var reasons []string
	passed = true

	if rec.LocalPath == "" {
		return true, false, "no local final path is recorded in the journal", nil
	}
	info, statErr := os.Stat(rec.LocalPath)
	if statErr != nil {
		return true, false, fmt.Sprintf("local final file %s: %v", rec.LocalPath, statErr), nil
	}
	checked = true

	if rec.LocalHashAlg != "" {
		if !strings.EqualFold(rec.LocalHashAlg, string(transport.SHA256)) {
			return true, false, fmt.Sprintf("cannot verify local identity: unsupported recorded hash algorithm %q", rec.LocalHashAlg), nil
		}
		sum, hashErr := sha256File(rec.LocalPath)
		if hashErr != nil {
			return true, false, fmt.Sprintf("hashing %s: %v", rec.LocalPath, hashErr), nil
		}
		if !strings.EqualFold(sum, rec.LocalHash) {
			passed = false
			reasons = append(reasons, fmt.Sprintf(
				"local final file %s now hashes to %s, but the %s hash recorded at verification was %s",
				rec.LocalPath, sum, rec.LocalHashAlg, rec.LocalHash))
		} else {
			reasons = append(reasons, "recomputed hash still matches the hash recorded at verification")
		}
	} else {
		reasons = append(reasons, fmt.Sprintf("local final file present, %d bytes (no recorded hash to compare against)", info.Size()))
	}

	if cmd != nil {
		result, hookErr := lifecycle.RunRestoreCheck(ctx, *cmd, rec.LocalPath)
		if hookErr != nil {
			return checked, false, "", fmt.Errorf("restore-test hook: %w", hookErr)
		}
		if !result.Passed {
			passed = false
			reasons = append(reasons, "restore-test hook failed: "+result.Detail)
		} else {
			reasons = append(reasons, "restore-test hook passed")
		}
	}

	return checked, passed, strings.Join(reasons, "; "), nil
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
