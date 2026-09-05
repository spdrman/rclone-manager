package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is issue #435: what `backup-manager validate <id>` does about
// an artifact whose durable copies are all on storage mediums.
//
// # Why it is here at all
//
// Issue #434 fixed a Critical in which the first successful move of an
// artifact to a medium got it marked QUARANTINED_LOST on the next cycle,
// because three of FR-29's four swept callers read "no readable local
// path" as "no durable copy". The operator door was one of the three, and
// the fix there was a refusal: `validate` said where the copy was, said
// that scheduled revalidation existence-checks it, and left the artifact
// alone. That was the right stop-gap and it was a stop-gap, because an
// operator who ran `validate` by hand against a moved artifact got told to
// go and look somewhere else.
//
// This checks it instead. FR-31 makes a content check of a medium copy
// operator-initiated because it costs egress, and `validate` is the
// operator-initiated command, so this is the one place in the product
// where a content check of a medium copy is legitimate without a schedule.
// Existence and attested cost nothing and run without ceremony.
//
// # The one rule everything below is arranged around
//
// "I checked and it is gone" is a fact about the artifact and quarantines
// it. "I could not check" is a fact about the endpoint and must not. An
// unreachable bucket is not evidence that a backup is gone, and a
// deployment that answered "the object is missing" every time a network
// call failed would destroy a journal's worth of good artifacts over one
// afternoon of DNS trouble. internal/revalidate states the same rule in
// its own words and routes it the same way; this is the operator-triggered
// twin of that code, not a second opinion about it.

// ValidateOptions is what an operator asked for beyond `validate`'s
// defaults.
//
// It is a struct rather than a bare bool because the answer to "what may
// this command spend" is going to grow (a per-medium bound, a timeout),
// and because a bare bool at a call site says nothing about which of the
// two costs it authorises.
type ValidateOptions struct {
	// Content asks for placement.Content against a copy on a storage
	// medium: the object is downloaded in full and re-hashed against the
	// hash the journal recorded at ingestion.
	//
	// It is off by default, and the default is not timidity. A download is
	// egress, egress is a bill, and FR-31's rule is that anything costing
	// egress is operator-initiated rather than something a command does
	// because it was convenient. The flag IS the operator initiating it.
	//
	// It changes nothing about a LOCAL copy, which `validate` has always
	// read and re-hashed unconditionally, because reading a file off the
	// disk it is already on costs nothing.
	//
	// Where the copy cannot be read at all, an archived object nobody has
	// asked to restore being the case that exists today, this produces a
	// refusal rather than a weaker check wearing the same name. An
	// operator who asked for the bytes and cannot have them needs to hear
	// that, not a green tick from a HEAD request.
	Content bool
}

// checkMediumCopies verifies every ACTIVE copy an artifact has on a
// storage medium and reports one verdict for the artifact.
//
// ps is the artifact's ACTIVE medium placements, already asked for by the
// caller (state.Record.ActiveMediumPlacements), and it is never empty:
// this is only reached when there is no readable local copy and at least
// one medium copy is recorded.
//
// # What "the strongest class the medium can give" means here
//
// Without ValidateOptions.Content, this walks the free half of FR-31's
// ladder: attested first, existence if the endpoint cannot attest. The
// step-down is written here, in the caller, rather than inside
// internal/placement, because that package's Verify never falls back by
// design ("a caller that wants the fallback asks for the weaker class
// itself, in its own code, where the decision is visible"). This is that
// code, and the decision is visible in it.
//
// The step-down is not a corner case. Measured against rclone v1.75.0,
// backend/s3's Fs.Hashes() returns hash.MD5 and nothing else, so NO s3
// medium reachable through this build can attest a full-object SHA-256,
// so every default run against the only medium type there is lands on
// existence. That is exactly the situation FR-31 forbids handling
// quietly, so the reason string names the class that was attempted, why
// it could not run, and what the class that did run actually proves.
//
// # When this quarantines, and when it refuses
//
// A failing check is a verdict about one COPY; quarantine is a verdict
// about the ARTIFACT. FR-31 keeps them apart, and so does this: a pass
// needs one copy to pass, and a failure needs every copy to have been
// ASKED and to have failed. A copy that could not be asked leaves the
// question open, and an open question is returned as an error, never as a
// verdict, for the reason at the top of this file.
//
// # The restore-test hook does not run here, and the pass says so
//
// FR-13's validator opens the artifact, so running it against a copy on a
// medium means downloading the artifact whether or not ValidateOptions
// asked for a download. It is skipped, and hasValidator is why the pass
// names the tier that did not run: an operator who configured a restore
// test and reads back a green tick has been told less than they asked
// for, and a check that quietly stops running is how a safety feature
// becomes decorative. internal/revalidate makes the same skip for the
// same reason and says it in the same place.
func (s *Service) checkMediumCopies(ctx context.Context, rec state.Record, ps []state.Placement, opts ValidateOptions, hasValidator bool) (checkOutcome, error) {
	if s.MediumStore == nil {
		// Issue #434's refusal, kept exactly where it belongs: a
		// deployment with no way to reach a medium at all. Everything
		// below needs a store, and inventing a verdict without one is the
		// fail-open this whole file exists to avoid.
		return checkOutcome{}, fmt.Errorf(
			"%s has no local copy to check: its durable copy is on storage medium %s, and this deployment has no way to reach a storage medium, so the artifact is left as it is",
			rec.Artifact, describePlacements(ps))
	}

	var (
		details       []string
		anyPassed     bool
		contentPassed bool
		unasked       []string
		didNotAnswer  error
	)
	now := s.now()
	for _, p := range ps {
		medium, _, err := MediumResolver(s.Config.StorageMediums).Resolve(p.Medium)
		if err != nil {
			// A placement row outlives the configuration that created it,
			// so a medium the config no longer declares is a state an
			// operator reaches normally. It is still an unanswered
			// question rather than a missing backup: the object may well
			// be sitting in that bucket, and nothing here can look.
			unasked = append(unasked, fmt.Sprintf(
				"the copy on storage medium %q was not checked, because this deployment cannot resolve that medium: %v", p.Medium, err))
			continue
		}

		result, verifyErr := s.verifyMediumCopy(ctx, medium, p, opts, now)
		if verifyErr != nil {
			if isCancelledErr(verifyErr) {
				// A cancelled pass has not measured anything, and a
				// partial answer from one is worse than none: it would
				// report a pass carried by whichever copy happened to be
				// asked first.
				return checkOutcome{}, verifyErr
			}
			if didNotAnswer == nil {
				didNotAnswer = fmt.Errorf("the copy on storage medium %q could not be checked: %w", p.Medium, verifyErr)
			}
			continue
		}
		details = append(details, result.Detail)
		if result.Passed {
			anyPassed = true
			if result.Class == placement.Content {
				contentPassed = true
			}
		}
	}

	// Nothing carried the artifact, so the two ways a copy can go unasked
	// stop being footnotes. Either of them leaves "no verified copy
	// remains" unproven, and that sentence is the ONLY thing a failing
	// verdict from here may mean, so a single unasked copy is enough to
	// turn the whole answer into an open question.
	//
	// This is the subtle half, and I wrote the fail-open version of it
	// first. It is easy to see that an unreachable medium beside no other
	// evidence must not quarantine. It is easier to miss that one copy
	// answering "not there" beside one copy nobody could ask is the same
	// situation: the artifact may be sitting perfectly safely in the
	// bucket that was never looked in, and quarantining it says the
	// opposite. Quarantine needs every copy ASKED and every copy failed.
	if !anyPassed && (didNotAnswer != nil || len(unasked) > 0) {
		open := append([]string(nil), details...)
		if didNotAnswer != nil {
			open = append(open, didNotAnswer.Error())
		}
		open = append(open, unasked...)
		return checkOutcome{}, fmt.Errorf(
			"%s: no copy of this artifact could be verified, and not every copy could be asked, so it is not established that no verified copy remains: %s; the artifact is left as it is",
			rec.Artifact, strings.Join(open, "; "))
	}

	// A pass another copy carried has to say which copies it did not hear
	// from, or an operator reads a green tick and never learns that one of
	// their two buckets went quiet. The pass stands: a copy is there and
	// was asked. What it must not do is imply that every copy was.
	reasons := append([]string(nil), details...)
	if didNotAnswer != nil {
		reasons = append(reasons, didNotAnswer.Error()+", so that copy was not checked")
	}
	reasons = append(reasons, unasked...)
	if hasValidator {
		reasons = append(reasons,
			"the restore-test hook did not run, because opening this artifact means downloading it and this command checks the copy where it is")
	}

	return checkOutcome{
		Checked: true,
		Passed:  anyPassed,
		Reason:  strings.Join(reasons, "; "),
		// Only a content check of the medium copy is evidence that could
		// have failed on the bytes, which is what lifecycle's
		// reinstatement rule asks for. An existence or attested pass sets
		// nothing here on purpose: an object of the right size at the
		// right key is not a reason to trust a quarantined artifact
		// again, and an attestation is the endpoint's own word rather
		// than a hash this manager computed.
		HashMatched: contentPassed,
	}, nil
}

// verifyMediumCopy runs the strongest class this command may attempt
// against one copy, and says out loud when it had to settle for less.
//
// A nil error and a Result is "the class ran"; a non-nil error is "it
// could not be attempted", which the caller keeps well away from a
// verdict.
func (s *Service) verifyMediumCopy(
	ctx context.Context,
	medium transport.Medium,
	p state.Placement,
	opts ValidateOptions,
	now time.Time,
) (placement.Result, error) {
	obs := s.observeMediumCopy(ctx, medium, p.Location)

	if opts.Content {
		// No step-down at all, deliberately. An operator who asked to
		// download the bytes and cannot have them (an archived copy
		// nobody has restored) is told so; quietly running a HEAD request
		// instead and reporting a pass is the exact "weaker class wearing
		// a stronger name" FR-13 and FR-31 both forbid.
		return placement.VerifyWithAccess(ctx, s.MediumStore, medium, p, placement.Content, obs, now)
	}

	attested, err := placement.VerifyWithAccess(ctx, s.MediumStore, medium, p, placement.Attested, obs, now)
	if err == nil {
		return attested, nil
	}
	if isCancelledErr(err) {
		return placement.Result{}, err
	}
	if !errors.Is(err, placement.ErrClassUnavailable) {
		// Not a capability answer, so there is nothing to step down from:
		// the endpoint or the record failed in a way this function has no
		// opinion about, and the caller routes it as an unanswered
		// question.
		return placement.Result{}, err
	}

	existence, existenceErr := placement.VerifyWithAccess(ctx, s.MediumStore, medium, p, placement.Existence, obs, now)
	if existenceErr != nil {
		// Both rungs refused. Report both, because "it cannot attest" and
		// "it did not answer at all" send an operator to different places
		// and the second one is the one that matters.
		return placement.Result{}, fmt.Errorf("%w (the %s class was attempted first and refused too: %v)", existenceErr, placement.Attested, err)
	}
	existence.Detail += fmt.Sprintf(
		"; the %s class was attempted first and could not run (%v), so this ran at %s, which proves only that %s",
		placement.Attested, err, placement.Existence, placement.Existence.Proves())
	return existence, nil
}

// observeMediumCopy asks the medium whether a restore of one copy is in
// effect, when and only when the copy's storage class needs one for the
// bytes to be readable.
//
// A non-archive class serves objects on demand and archive.Access does not
// read a restore status for one, so asking anyway would add a failure mode
// to a copy whose readability does not depend on the answer. This mirrors
// internal/placement's Engine.observe, deliberately and not by accident:
// the two callers hold different stores and neither imports the other, and
// the shape is six lines whose alternative is an exported seam on the move
// engine that this command would be the only user of.
//
// Nothing here guesses. A store that could not be asked reads as
// requires_restore, one that did not answer reads as unreachable, and both
// of those refuse the classes that need the bytes rather than pretending
// to have them.
func (s *Service) observeMediumCopy(ctx context.Context, medium transport.Medium, key string) archive.Observation {
	if !archive.IsArchive(medium.StorageClass) || s.MediumStore == nil {
		return archive.Observation{}
	}
	restore, err := s.MediumStore.RestoreStatus(ctx, medium, key)
	if err != nil {
		return archive.Observation{Probe: archive.DidNotAnswer}
	}
	return archive.Observation{Probe: archive.Answered, Restore: restore}
}

// describePlacements names the mediums a set of copies sits on, and the
// class each copy last ACHIEVED, for the refusal an operator reads when
// nothing could be reached at all.
//
// It names every one of them rather than the first: "your backup is on a
// medium I cannot reach" is a sentence somebody has to act on, and the
// medium's id is the only part of it that says where to look.
func describePlacements(ps []state.Placement) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		class := p.VerificationClass
		if class == "" {
			class = "unverified"
		}
		out = append(out, fmt.Sprintf("%s (%s)", strconv.Quote(p.Medium), class))
	}
	return strings.Join(out, ", ")
}

// isCancelledErr reports whether err is the outer context giving up, which
// is never a verdict about an artifact.
//
// It mirrors internal/revalidate's isCancelled, including its ORDERING,
// which is the part that matters and the part I got wrong first.
// transport.Error keeps its cause reachable through Unwrap, so an error
// already classified as something other than Cancelled can still answer
// errors.Is(err, context.DeadlineExceeded): a connect timeout rclone
// imposed on itself is exactly that shape (issue #388). Asking the context
// first reads one slow bucket as the operator having cancelled the whole
// command, and abandons a check that another copy would have carried. So
// the transport's own classification is asked first, and the context
// question only where nothing classified the error at all.
//
// This package keeps its own copy for the reason internal/revalidate gives
// for keeping that one: it is a handful of lines, and neither package
// otherwise depends on the other.
func isCancelledErr(err error) bool {
	if err == nil {
		return false
	}
	if category, ok := transport.CategoryOf(err); ok {
		return category == transport.Cancelled
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
