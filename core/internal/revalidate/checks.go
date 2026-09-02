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
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// runChecks runs whatever cfg enables against rec's durable copy and
// reports a combined verdict, plus the verification CLASS it achieved.
//
// # Which copy it checks, and why that is a fork rather than a parameter
//
// An artifact's durable copy used to be one thing: a local file. Since
// EPIC E it can be a local file, an object on a storage medium, or both
// at once during a move. The two are not the same check with a different
// path, they are different checks with different costs and different
// strengths, so this function forks on where the copy actually is rather
// than pretending one code path covers both.
//
//   - a local copy is re-read and re-hashed, exactly as before, which is
//     placement.Content;
//   - a medium copy is HEADed, which is placement.Existence, and that is
//     the CEILING for an automatic pass rather than a starting point.
//     Anything stronger costs egress, and silent egress is a surprise
//     bill; FR-31 makes content and attested re-verification of a medium
//     placement operator-initiated, and Class.CostsEgress is what this
//     function refuses on rather than a rule written in a comment.
//
// checked is false only when nothing enabled could actually produce a
// verdict for this specific artifact: cfg.Command is nil and cfg.Hash is
// unset or has no recorded baseline, or the artifact's copy is on a medium
// this deployment cannot reach. That is not a failure and not a pass; see
// checkArtifact, the only caller, for why it must never be turned into a
// same-state "passed" journal write, which would silently reset SelectDue's
// due-ness clock for an artifact nothing here actually re-verified.
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
func runChecks(ctx context.Context, deps Deps, cfg config.Revalidation, rec state.Record) (checked, passed bool, class placement.Class, reason string, err error) {
	if _, local := rec.LocalPlacement(); !local {
		if medium, ok := activeMediumPlacement(rec); ok {
			return checkMediumPlacement(ctx, deps, rec, medium)
		}
	}
	return checkLocalCopy(ctx, deps, cfg, rec)
}

// activeMediumPlacement returns the artifact's ACTIVE placement on a
// storage medium, if it has one.
//
// It is only consulted when there is no active LOCAL placement, which is
// the ordering FR-31 asks for: local placements keep today's behaviour
// exactly, and an artifact mid-move that still has its local copy is
// checked the way it was checked yesterday.
func activeMediumPlacement(rec state.Record) (state.Placement, bool) {
	for _, p := range rec.Placements {
		if !p.IsLocal() && p.Status == state.PlacementActive {
			return p, true
		}
	}
	return state.Placement{}, false
}

// checkMediumPlacement runs the automatic ceiling, placement.Existence,
// against a copy on a storage medium.
//
// The class is not configurable here and that is deliberate. cfg.Hash asks
// for a content check, and honouring it against a medium would download
// the artifact on a schedule the operator set for something that used to
// be free. FR-31 says so directly, and the assertion below turns it from a
// rule into a fact: whatever class this function is about to run, it
// refuses if that class costs egress.
func checkMediumPlacement(ctx context.Context, deps Deps, rec state.Record, p state.Placement) (checked, passed bool, class placement.Class, reason string, err error) {
	if deps.Store == nil || deps.Mediums == nil {
		return false, true, "", fmt.Sprintf(
			"this artifact's only durable copy is on storage medium %q, and this deployment has no way to reach one, so nothing was checked", p.Medium), nil
	}
	medium, ok := deps.Mediums.MediumFor(p.Medium)
	if !ok {
		return false, true, "", fmt.Sprintf(
			"this artifact's only durable copy is on storage medium %q, which is not in the configuration, so nothing was checked", p.Medium), nil
	}

	const automatic = placement.Existence
	if automatic.CostsEgress() {
		// Unreachable today, and here on purpose: this is the line that
		// has to fail if somebody ever raises the automatic ceiling, so
		// the decision is made by editing this refusal rather than by
		// changing a constant and discovering the bill later.
		return false, true, "", "", fmt.Errorf(
			"revalidate: the automatic class %s costs egress, and FR-31 makes anything that costs egress operator-initiated", automatic)
	}

	result, verifyErr := placement.Verify(ctx, deps.Store, medium, p, automatic, deps.now())
	if verifyErr != nil {
		if isCancelled(verifyErr) {
			return false, false, "", "", verifyErr
		}
		// A class that could not be attempted is not a verdict about the
		// artifact. Reporting it as unchecked leaves the due-ness clock
		// alone, so the artifact stays selectable next cycle instead of
		// looking freshly checked because a bucket was briefly
		// unreachable.
		return false, true, "", fmt.Sprintf("nothing could be checked on medium %q: %v", p.Medium, verifyErr), nil
	}

	return true, result.Passed, result.Class, result.Detail, nil
}

// checkLocalCopy is exactly the check this package always did, against the
// artifact's local copy, reported as the class it has always achieved.
func checkLocalCopy(ctx context.Context, deps Deps, cfg config.Revalidation, rec state.Record) (checked, passed bool, class placement.Class, reason string, err error) {
	var reasons []string
	passed = true

	localPath, hasLocal := rec.ReadableLocalPath()

	if cfg.Hash && rec.LocalHashAlg == string(transport.SHA256) && rec.LocalHash != "" {
		checked = true
		class = placement.Content
		switch {
		case !hasLocal:
			passed = false
			reasons = append(reasons, "no local copy of this artifact is recorded, so its content cannot be re-read")
		default:
			sum, readErr := recomputeLocalHash(localPath)
			switch {
			case readErr != nil:
				passed = false
				reasons = append(reasons, fmt.Sprintf("local final file %s could not be read: %v", localPath, readErr))
			case !strings.EqualFold(sum, rec.LocalHash):
				passed = false
				reasons = append(reasons, fmt.Sprintf(
					"local final file %s now hashes to %s, but the %s hash recorded at verification was %s",
					localPath, sum, rec.LocalHashAlg, rec.LocalHash,
				))
			default:
				reasons = append(reasons, "recomputed hash still matches the hash recorded at verification")
			}
		}
	}

	if cfg.Command != nil {
		result, hookErr := lifecycle.RunRestoreCheck(ctx, *cfg.Command, localPath)
		if hookErr != nil {
			if isCancelled(hookErr) {
				return false, false, "", "", hookErr
			}
			return false, false, "", "", fmt.Errorf("restore-test hook: %w", hookErr)
		}
		checked = true
		// A restore-test hook opens the artifact and proves it restores,
		// which is at least as strong a statement about the bytes as
		// re-hashing them. It does not upgrade a class it did not reach,
		// so it only sets one where the hash tier did not already.
		if class == "" {
			class = placement.Content
		}
		if !result.Passed {
			passed = false
			reasons = append(reasons, "restore-test hook failed: "+result.Detail)
		} else {
			reasons = append(reasons, "restore-test hook passed")
		}
	}

	if !checked {
		return false, true, "", fmt.Sprintf(
			"nothing to check for this artifact: no recorded hash baseline (local_hash_alg=%q) and no restore-test hook configured",
			rec.LocalHashAlg,
		), nil
	}

	return true, passed, class, strings.Join(reasons, "; "), nil
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
