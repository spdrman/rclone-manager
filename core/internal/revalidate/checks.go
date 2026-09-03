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
		if mediums := activeMediumPlacements(rec); len(mediums) > 0 {
			return checkMediumPlacements(ctx, deps, cfg, mediums)
		}
	}
	return checkLocalCopy(ctx, deps, cfg, rec)
}

// activeMediumPlacements returns every ACTIVE placement the artifact has
// on a storage medium.
//
// It is only consulted when there is no active LOCAL placement, which is
// the ordering FR-31 asks for: local placements keep today's behaviour
// exactly, and an artifact mid-move that still has its local copy is
// checked the way it was checked yesterday.
//
// It returns all of them rather than the first because "the first one" is
// an assumption rather than a decision, and FR-31 makes the decision
// explicitly: an artifact enters QUARANTINED only when no other ACTIVE
// verified placement remains. Nothing in Phase 1 can put an artifact on
// two mediums at once, so today this is always a slice of one; the point
// is that the day something can, the answer is already the right one
// instead of whichever medium sorted first.
func activeMediumPlacements(rec state.Record) []state.Placement {
	var out []state.Placement
	for _, p := range rec.Placements {
		if !p.IsLocal() && p.Status == state.PlacementActive {
			out = append(out, p)
		}
	}
	return out
}

// checkMediumPlacements runs the automatic ceiling, placement.Existence,
// against every ACTIVE copy the artifact has on a storage medium.
//
// The class is not configurable here and that is deliberate. cfg.Hash asks
// for a content check, and honouring it against a medium would download
// the artifact on a schedule the operator set for something that used to
// be free. FR-31 says so directly, and the assertion below turns it from a
// rule into a fact: whatever class this function is about to run, it
// refuses if that class costs egress.
//
// cfg.Command is skipped for the same reason and said out loud for a
// different one. A restore test opens the artifact, so running one against
// a bucket means downloading the artifact, which is the egress this
// function refuses. But an operator who configured a restore test and gets
// back a green pass has been told less than they asked for, and a check
// that quietly stops running is how a safety feature becomes decorative.
// So the pass names the tier that did not run.
// A failing check is a verdict about ONE placement, and quarantine is a
// verdict about the artifact. FR-31 keeps them apart: the artifact enters
// QUARANTINED only when no other ACTIVE verified placement remains. So a
// pass here needs one placement to pass, and a failure needs every one of
// them to have been asked AND to have failed. A placement that could not
// be asked leaves the question open, which is an error rather than a
// verdict: an unreachable bucket is not evidence that a backup is gone.
func checkMediumPlacements(ctx context.Context, deps Deps, cfg config.Revalidation, ps []state.Placement) (checked, passed bool, class placement.Class, reason string, err error) {
	if deps.Store == nil || deps.Mediums == nil {
		return false, true, "", fmt.Sprintf(
			"this artifact's only durable copy is on storage medium %q, and this deployment has no way to reach one, so nothing was checked", ps[0].Medium), nil
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

	var (
		details       []string
		anyPassed     bool
		didNotAnswer  error
		notConfigured string
	)
	for _, p := range ps {
		medium, ok := deps.Mediums.MediumFor(p.Medium)
		if !ok {
			if notConfigured == "" {
				notConfigured = p.Medium
			}
			continue
		}

		result, verifyErr := placement.Verify(ctx, deps.Store, medium, p, automatic, deps.now())
		if verifyErr != nil {
			if isCancelled(verifyErr) {
				return false, false, "", "", verifyErr
			}
			// A class that could not be attempted is not a verdict about
			// the artifact, and it is not a configuration fact either: the
			// medium was there to ask and did not answer. So it is routed
			// the way this package already routes a restore-test hook that
			// fails to start, as a per-artifact ERROR rather than as an
			// unchecked finding.
			//
			// The distinction is worth the extra branch. An unchecked
			// finding says "nothing here was configured to check", which an
			// operator reads past; an error says "this backup could not be
			// checked and somebody should find out why", which is the true
			// statement when a bucket does not answer. Either way the
			// journal is untouched and the due-ness clock does not move, so
			// the artifact stays selectable next cycle rather than looking
			// freshly verified.
			if didNotAnswer == nil {
				didNotAnswer = fmt.Errorf("medium %q: %w", p.Medium, verifyErr)
			}
			continue
		}
		details = append(details, result.Detail)
		if result.Passed {
			anyPassed = true
		}
	}

	// One good copy is enough to say the artifact is still there, and
	// saying so does not depend on the placements that could not be asked.
	// Everything below is the case where none passed, where the two ways a
	// placement can go unasked matter, because each of them leaves "no
	// verified copy remains" unproven and quarantine is what that would
	// otherwise mean.
	if !anyPassed {
		switch {
		case didNotAnswer != nil:
			// A medium that was there to ask and did not answer: an error,
			// because somebody should find out why.
			return false, false, "", "", didNotAnswer
		case notConfigured != "":
			// A medium this deployment was never configured to reach: a
			// configuration fact rather than a backup nobody could check,
			// and an unchecked finding rather than an error.
			return false, true, "", fmt.Sprintf(
				"this artifact's only durable copy is on storage medium %q, which is not in the configuration, so nothing was checked", notConfigured), nil
		}
	}

	detail := strings.Join(details, "; ")
	if cfg.Command != nil {
		detail += "; the restore-test hook did not run, because opening this artifact means downloading it and FR-31 makes anything that costs egress operator-initiated"
	}
	return true, anyPassed, automatic, detail, nil
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
