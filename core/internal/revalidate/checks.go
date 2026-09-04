package revalidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// automaticMediumClass is the strongest verification class a scheduled,
// unattended pass will ever run against a copy on a storage medium.
//
// It was a const inside checkMediumPlacements and it is here so a test can
// read it, because placement.AutomaticClass is the same rule stated a
// second time and the two had no way of being held together. That function
// derives the ceiling from Class.CostsEgress rather than naming a rung, so
// that raising the automatic ceiling means changing the class whose cost is
// consulted rather than editing a constant and finding out from a bill.
// This IS that constant, and nothing consulted that function.
//
// It is not simply replaced by a call to it, because
// placement.AutomaticClass takes an archive access state and this pass does
// not have one: deriving it means a restore-status probe per archive-class
// copy per cycle, which is a request this pass deliberately does not spend
// on an answer that cannot change what it runs (a HEAD works on an
// archived object). So the two are pinned together by a test instead, and
// the delete-or-wire decision for AutomaticClass is filed rather than taken
// here.
const automaticMediumClass = placement.Existence

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
	// The medium copies are only consulted when there is no active LOCAL
	// placement, which is the ordering FR-31 asks for: local placements
	// keep today's behaviour exactly, and an artifact mid-move that still
	// has its local copy is checked the way it was checked yesterday.
	//
	// state.Record.ActiveMediumPlacements used to be a private loop here,
	// and this package was the only one of FR-29's four swept callers
	// that asked it. The other three read "no readable local path" as "no
	// durable copy" and quarantined every moved artifact; the loop now
	// lives beside ReadableLocalPath so every caller of the one is held
	// to asking the other.
	if _, local := rec.LocalPlacement(); !local {
		if mediums := rec.ActiveMediumPlacements(); len(mediums) > 0 {
			return checkMediumPlacements(ctx, deps, cfg, mediums)
		}
	}
	return checkLocalCopy(ctx, deps, cfg, rec)
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
//
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
			"this artifact's durable copies are on storage mediums (%s), and this deployment has no way to reach one, so nothing was checked", mediumIDs(ps)), nil
	}

	const automatic = automaticMediumClass
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
				"this artifact's durable copies are on storage mediums (%s), and none of them is in the configuration, so nothing was checked", mediumIDs(ps)), nil
		}
	}

	detail := strings.Join(details, "; ")
	// A pass that another copy carried has to say which copies it did not
	// hear from, or an operator reads a green tick and never learns that
	// one of their two buckets went quiet. The pass itself stands: a copy
	// is there and was asked. What it must not do is imply that every copy
	// was.
	if didNotAnswer != nil {
		detail += "; " + didNotAnswer.Error() + ", so that copy was not checked"
	}
	if notConfigured != "" {
		detail += fmt.Sprintf("; the copy on storage medium %q was not checked, because that medium is not in the configuration", notConfigured)
	}
	if cfg.Command != nil {
		detail += "; the restore-test hook did not run, because opening this artifact means downloading it and FR-31 makes anything that costs egress operator-initiated"
	}
	return true, anyPassed, automatic, detail, nil
}

// mediumIDs names the mediums a set of placements sits on, for the two
// messages an operator reads when nothing could be checked at all. It
// names every one of them rather than the first, because "your backup is
// on a medium I cannot reach" is a sentence somebody has to act on and the
// medium's id is the only part of it that says where to look.
func mediumIDs(ps []state.Placement) string {
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, strconv.Quote(p.Medium))
	}
	return strings.Join(ids, ", ")
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
