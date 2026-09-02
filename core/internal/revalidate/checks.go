package revalidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// runChecks runs whatever cfg enables against rec's local final file and
// reports a combined verdict.
//
// checked is false only when nothing cfg enables could actually produce a
// verdict for this specific artifact: cfg.Command is nil, and cfg.Hash is
// set but rec has no recorded hash baseline to compare a fresh read
// against (it was originally verified without hash: sha256, so
// rec.LocalHash is empty). That is not itself a failure and not a pass;
// see checkArtifact, the only caller, for why it must never be turned into
// a same-state "passed" journal write, which would silently reset
// SelectDue's due-ness clock for an artifact nothing here actually
// re-verified.
//
// err is non-nil only for an infrastructure problem this function cannot
// safely turn into a business verdict for this one artifact: the outer
// context being cancelled or timing out (isCancelled), or the restore-test
// hook failing to even start (a broken or missing executable, not the
// hook's own opinion about the file's contents). Both are the caller's
// (Run's) responsibility to route: a cancellation stops the whole pass,
// an ordinary infrastructure error is recorded in Report.Errors without
// touching this artifact's journal row. Neither is grounds to quarantine a
// possibly-perfectly-good backup over what might be a misconfigured
// operator hook, not a corrupt artifact.
func runChecks(ctx context.Context, cfg config.Revalidation, rec state.Record) (checked, passed bool, reason string, err error) {
	var reasons []string
	passed = true

	// FR-29: where the copy actually is comes from the placement, not from
	// LocalPath, which records where it landed and keeps saying so
	// afterwards. Every placement is local today, so this reads exactly as
	// it did; when the move engine can retire a local copy, an empty
	// answer here is the honest one and turning it into a read of the old
	// landing path would be a re-hash of whatever else ended up at that
	// name.
	local := rec.LocalLocation()

	if cfg.Hash && rec.LocalHashAlg == string(transport.SHA256) && rec.LocalHash != "" {
		checked = true
		sum, readErr := recomputeLocalHash(local)
		switch {
		case local == "":
			passed = false
			reasons = append(reasons, "the journal records no active local copy of this artifact to re-read")
		case readErr != nil:
			passed = false
			reasons = append(reasons, fmt.Sprintf("local final file %s could not be read: %v", local, readErr))
		case !strings.EqualFold(sum, rec.LocalHash):
			passed = false
			reasons = append(reasons, fmt.Sprintf(
				"local final file %s now hashes to %s, but the %s hash recorded at verification was %s",
				local, sum, rec.LocalHashAlg, rec.LocalHash,
			))
		default:
			reasons = append(reasons, "recomputed hash still matches the hash recorded at verification")
		}
	}

	if cfg.Command != nil {
		result, hookErr := lifecycle.RunRestoreCheck(ctx, *cfg.Command, local)
		if hookErr != nil {
			if isCancelled(hookErr) {
				return false, false, "", hookErr
			}
			return false, false, "", fmt.Errorf("restore-test hook: %w", hookErr)
		}
		checked = true
		if !result.Passed {
			passed = false
			reasons = append(reasons, "restore-test hook failed: "+result.Detail)
		} else {
			reasons = append(reasons, "restore-test hook passed")
		}
	}

	if !checked {
		return false, true, fmt.Sprintf(
			"nothing to check for this artifact: no recorded hash baseline (local_hash_alg=%q) and no restore-test hook configured",
			rec.LocalHashAlg,
		), nil
	}

	return true, passed, strings.Join(reasons, "; "), nil
}

// recomputeLocalHash reads path in full and returns its SHA-256, in hex.
//
// This duplicates a small (open, io.Copy into a hasher, hex-encode)
// utility that already exists, independently, in both
// internal/lifecycle's verify.go (readAndHashLocal) and
// internal/reconcile's localcheck.go (sha256File): each package that has
// ever needed this has kept its own copy rather than reach across a
// package boundary for ten lines, and this package follows that same,
// already-established convention rather than introduce a new shared
// dependency for it.
func recomputeLocalHash(path string) (string, error) {
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
