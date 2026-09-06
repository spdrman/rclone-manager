package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The four things an operator may do about a backup this manager stopped
// trusting.
//
// Quarantine is deliberately a dead end for automation: nothing in a cycle
// takes an artifact out of it, because every route out is a judgement about
// whether a backup can be believed and that judgement is a person's. This
// file is where those judgements are spelled out, and the useful way to read
// it is as four different answers to "and then what".
//
// Revalidate checks and writes nothing, so an operator can look before
// deciding. Retry ingestion throws the local copy away and fetches again from
// a source that still exists. Retry a FAILED ingestion is the same door for
// an artifact that never got far enough to be quarantined. Reinstate keeps
// the local copy and restores the artifact's standing, and it is the only one
// of the four available to a QUARANTINED_LOST artifact, because that state is
// reached only from COMPLETE, which is the state that confirms the remote
// source was already deleted.
//
// The refusals get their own sentinels rather than a shared generic error
// because the layers above turn each into a different named response, and
// because they say genuinely different things: "this backup is not waiting
// for your judgement" is not "this backup is not stuck", and neither is "there
// is nothing left anywhere to re-ingest".
//
// An artifact whose backup set the configuration no longer names is refused
// by all of them, through unconfiguredSet, and that is not an oversight about
// removed sets. Every action here needs the set's policy to act under: the
// checks run under its validation config, and a retry hands the row back to a
// pipeline that only walks configured sets.

// ErrNotQuarantined is returned when an artifact named for one of the two
// quarantine actions below is not in QUARANTINED or QUARANTINED_LOST. It
// is a distinct error, not a generic failure, because the API layer turns
// it into a typed refusal rather than a 500.
var ErrNotQuarantined = errors.New("app: artifact is not quarantined")

// ErrQuarantineIrrecoverable is returned when a QUARANTINED_LOST artifact
// is asked to re-enter the pipeline. It is not a transient failure and no
// retry can change it: QUARANTINED_LOST is reached only from COMPLETE,
// which is the one state that confirms the remote source is already
// deleted, so there is nothing left anywhere to re-ingest. See
// internal/lifecycle's Transitions table, where QUARANTINED_LOST has no
// edge into the pipeline at all. ReinstateQuarantined is the action that
// does serve it, by keeping the local copy rather than re-fetching.
var ErrQuarantineIrrecoverable = errors.New("app: quarantined artifact has no remaining source to re-ingest")

// ErrNotFailed is returned when an artifact named for RetryFailedIngestion
// is not in FAILED. It is its own sentinel rather than a reuse of
// ErrNotQuarantined, because the two say different things to an operator
// and the API layer gives them different codes: one is "this backup is not
// waiting for your judgement", the other is "this backup is not stuck".
var ErrNotFailed = errors.New("app: artifact is not failed")

// unconfiguredSet is the refusal all three actions below share for an
// artifact whose backup set the configuration no longer names (issue
// #391): the same *NotFoundError ListArtifacts returns for a filter over
// such a set, so the layers above turn it into the same named 404
// rather than a 500. The row itself still exists, which is why this is
// not state.ErrArtifactNotFound.
func unconfiguredSet(set model.BackupSetID) error {
	return &NotFoundError{Kind: "backup set", Name: set.String()}
}

// RevalidateQuarantined re-runs the durable-local-copy checks against one
// QUARANTINED or QUARANTINED_LOST artifact and reports the verdict,
// writing nothing.
//
// This is deliberately NOT ValidateArtifact with a wider eligible-state
// set. The two differ in what they are for, and in what they may do:
//
//   - ValidateArtifact checks a healthy restore point (COMMITTED,
//     REMOTE_DELETE_PENDING, COMPLETE or REMOTE_RETAINED) and, on a
//     failure, quarantines it.
//     Its whole point is that a bad artifact stops being trusted.
//
//   - This checks an artifact that is ALREADY quarantined, and moves it
//     nowhere at all, on either verdict. A pass here is evidence for an
//     operator deciding what to do next, and nothing more.
//
// Writing nothing on either verdict is what makes that honest, and it is
// still the right shape now that a pass CAN lead somewhere (issue #220
// added ReinstateQuarantined below). A verdict this call produced and
// reported is a fact about the moment it was measured, and re-trusting an
// artifact on the strength of a measurement taken in an earlier request
// would be writing on a fact that may already be stale. So this reports,
// ReinstateQuarantined re-measures and writes in one operation, and
// neither one borrows the other's verdict.
func (s *Service) RevalidateQuarantined(ctx context.Context, id model.ArtifactID) (ValidateResult, error) {
	rec, err := s.Journal.Get(ctx, id)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("app: revalidate: %w", err)
	}

	cur := lifecycle.State(rec.State)
	if cur != lifecycle.Quarantined && cur != lifecycle.QuarantinedLost {
		return ValidateResult{Artifact: id}, fmt.Errorf("%w: %s is %s", ErrNotQuarantined, id, cur)
	}

	_, bs, ok := s.backupSetConfigFor(id.Set)
	if !ok {
		return ValidateResult{}, fmt.Errorf("app: revalidate: %w", unconfiguredSet(id.Set))
	}

	checks, err := s.runValidationChecks(ctx, rec, bs.Validation, ValidateOptions{})
	if err != nil {
		return ValidateResult{}, fmt.Errorf("app: revalidate: %w", err)
	}
	return ValidateResult{Artifact: id, Checked: checks.Checked, Passed: checks.Passed, Reason: checks.Reason}, nil
}

// RetryQuarantinedIngestion puts one QUARANTINED artifact back into
// DISCOVERED so the ordinary pipeline can attempt it again.
//
// QUARANTINED -> DISCOVERED is the lifecycle graph's own recovery edge,
// and the reason it is safe is recorded there: QUARANTINED is only ever
// reached from VERIFYING, COMMITTED, REMOTE_DELETE_PENDING or FAILED, and
// none of those has issued a remote delete, so the source is presumptively
// still there to re-fetch from.
//
// QUARANTINED_LOST is refused with ErrQuarantineIrrecoverable rather than
// attempted. That state is reached only from COMPLETE, which confirms the
// remote source is gone; sending it to DISCOVERED would rediscover
// nothing, fail, land in FAILED, and FAILED -> DISCOVERED would send it
// straight back around. The lifecycle package calls that a livelock and
// gives QUARANTINED_LOST no edge into the pipeline for exactly this reason,
// so the refusal here is naming that rule rather than adding a new one. An
// operator whose local copy is provably intact wants ReinstateQuarantined
// instead, which keeps that copy rather than asking for a source that is
// not there.
func (s *Service) RetryQuarantinedIngestion(ctx context.Context, id model.ArtifactID) error {
	rec, err := s.Journal.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("app: retry ingestion: %w", err)
	}

	switch lifecycle.State(rec.State) {
	case lifecycle.Quarantined:
		// the one recoverable case
	case lifecycle.QuarantinedLost:
		return fmt.Errorf("%w: %s", ErrQuarantineIrrecoverable, id)
	default:
		return fmt.Errorf("%w: %s is %s", ErrNotQuarantined, id, rec.State)
	}

	// The set has to be configured, and this is the one action of the
	// three that did not check (issue #391). A retry hands the row back
	// to the ordinary pipeline, and the pipeline walks configured sets:
	// a row sent to DISCOVERED under a set the configuration no longer
	// has is one no cycle will ever pick up, no longer quarantined so
	// off that screen, and unreachable by every recovery path. It is
	// refused before the write, like the other two.
	if _, _, ok := s.backupSetConfigFor(id.Set); !ok {
		return fmt.Errorf("app: retry ingestion: %w", unconfiguredSet(id.Set))
	}

	_, err = lifecycle.Advance(ctx, s.lifecycleDeps(), state.Transition{
		Artifact: id,
		Key:      fmt.Sprintf("app:retry-ingestion:%s:%s", id, s.now().Format(time.RFC3339Nano)),
		From:     string(lifecycle.Quarantined),
		To:       string(lifecycle.Discovered),
		Detail:   "operator-triggered retry: re-entering the pipeline from quarantine",
	})
	if err != nil {
		return fmt.Errorf("app: retry ingestion: %s: %w", id, err)
	}
	return nil
}

// RetryFailedIngestion puts one FAILED artifact back into DISCOVERED so
// the ordinary pipeline attempts it again (issue #419).
//
// It is the FAILED counterpart of RetryQuarantinedIngestion above and is
// deliberately a separate method rather than a widened one. The two refuse
// different things, report different named errors, and answer different
// questions for an operator: quarantine means a human has to decide
// whether a backup is trustworthy, FAILED means an attempt did not finish.
// Folding them together would give one entry point two vocabularies and
// make the refusal an operator gets depend on a state they cannot see from
// where they clicked.
//
// # Why the reason is read here
//
// internal/lifecycle cannot read it. FR-10 gives FAILED no reason field,
// so the sentence lives only in the journal's state_transitions log, which
// internal/state owns and lifecycle.Journal does not expose. This package
// already reads it for issue #284's CLI detail view, so it reads it once
// more here and hands it down, rather than lifecycle growing a second read
// path or reconstructing something less true.
//
// A read that fails is not a reason to refuse the retry: the operator's
// decision does not depend on this manager being able to quote the
// previous failure back at them, so the retry proceeds with an empty
// RecoveringFrom and the row simply carries nothing where it would have
// carried the old sentence.
func (s *Service) RetryFailedIngestion(ctx context.Context, id model.ArtifactID, note string) error {
	rec, err := s.Journal.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("app: retry failed ingestion: %w", err)
	}
	if cur := lifecycle.State(rec.State); cur != lifecycle.Failed {
		return fmt.Errorf("%w: %s is %s", ErrNotFailed, id, cur)
	}

	// The same refusal RetryQuarantinedIngestion makes, for the same
	// reason (issue #391): a row sent to DISCOVERED under a backup set the
	// configuration no longer names is one no cycle will ever walk, no
	// longer FAILED so off that list, and unreachable by every recovery
	// path there is.
	if _, _, ok := s.backupSetConfigFor(id.Set); !ok {
		return fmt.Errorf("app: retry failed ingestion: %w", unconfiguredSet(id.Set))
	}

	var recoveringFrom string
	if detail, err := s.GetArtifactDetail(ctx, id); err == nil {
		recoveringFrom = detail.FailureReason
	} else {
		s.logger().Error(ctx, "retry-failed", fmt.Errorf("reading %s's recorded failure reason: %w", id, err))
	}

	if _, err := lifecycle.RetryFailed(ctx, s.lifecycleDeps(), lifecycle.RetryFailedParams{
		Artifact:       id,
		AttemptKey:     fmt.Sprintf("app:retry-failed:%s:%s", id, s.now().Format(time.RFC3339Nano)),
		RecoveringFrom: recoveringFrom,
		Note:           note,
	}); err != nil {
		return fmt.Errorf("app: retry failed ingestion: %s: %w", id, err)
	}
	return nil
}

// ReinstateResult is ReinstateQuarantined's outcome.
type ReinstateResult struct {
	Artifact model.ArtifactID

	// Checked, Passed and Reason are the verdict of the checks this call
	// ran, reported the same way RevalidateQuarantined reports them, so an
	// operator who asked to reinstate and got a refusal sees exactly what
	// was found rather than only that it did not work.
	Checked bool
	Passed  bool
	Reason  string

	// Reinstated is true only when the artifact actually moved. It is
	// false, with a nil error, when the checks did not pass: that is an
	// ordinary verdict about the artifact, not a failure of the request.
	Reinstated bool

	// NewState is the state the artifact was returned to, set only when
	// Reinstated is true.
	NewState lifecycle.State
}

// ReinstateQuarantined re-checks one quarantined artifact's durable local
// copy and, if what it finds is enough to trust the artifact again,
// returns it to the state it already held: QUARANTINED to COMMITTED, or
// QUARANTINED_LOST to COMPLETE.
//
// This is the answer to the case RetryQuarantinedIngestion cannot serve.
// A retry throws the local copy away and re-fetches from the remote, which
// is right when the local copy is bad and useless when the local copy is
// fine or the remote is gone. Before this existed the operator's only
// remaining option in those cases was to leave the artifact quarantined
// forever, where FR-24 keeps reporting it and FR-19's last-known-good
// protection keeps skipping it.
//
// # The checks and the write are one operation, deliberately
//
// It re-runs the checks itself rather than taking a verdict from an
// earlier RevalidateQuarantined call. A verdict is a fact about the moment
// it was measured; a design where the operator revalidates, reads a pass,
// and then asks for the reinstatement in a second request would write on
// the strength of a measurement that may already be stale, and the window
// between the two is exactly when a failing disk keeps failing.
//
// # What it refuses, and why the refusals are typed
//
// internal/lifecycle owns every rule about whether the evidence justifies
// re-trusting the artifact (see ReinstateFromQuarantine: the evidence has
// to be something that could have failed, it has to answer a validator's
// own rejection when there is one, and the append-only transition log has
// to show the artifact really did hold the state it is being returned to).
// This function's job is to gather the evidence and pass it along, not to
// re-decide any of that. Its refusals come back as
// lifecycle.InsufficientEvidenceError, lifecycle.NeverHeldTargetStateError
// and lifecycle.NotQuarantinedError, all of which the API layer turns into
// named refusals rather than 500s.
//
// # What it costs the artifact
//
// A reinstated artifact never authorises a remote delete again, ever:
// lifecycle.DeleteRemote refuses one outright, reading the fact out of the
// append-only transition log. That forfeiture is what makes the whole edge
// safe, and it is worth saying plainly to whoever calls this: the remote
// source is preserved from here on, and releasing it becomes an operator's
// job outside this manager.
func (s *Service) ReinstateQuarantined(ctx context.Context, id model.ArtifactID, note string) (ReinstateResult, error) {
	rec, err := s.Journal.Get(ctx, id)
	if err != nil {
		return ReinstateResult{}, fmt.Errorf("app: reinstate: %w", err)
	}

	cur := lifecycle.State(rec.State)
	if !lifecycle.HasReinstatementExit(cur) {
		return ReinstateResult{Artifact: id}, fmt.Errorf("%w: %s is %s", ErrNotQuarantined, id, cur)
	}

	_, bs, ok := s.backupSetConfigFor(id.Set)
	if !ok {
		return ReinstateResult{}, fmt.Errorf("app: reinstate: %w", unconfiguredSet(id.Set))
	}

	checks, err := s.runValidationChecks(ctx, rec, bs.Validation, ValidateOptions{})
	if err != nil {
		return ReinstateResult{}, fmt.Errorf("app: reinstate: %w", err)
	}

	result := ReinstateResult{Artifact: id, Checked: checks.Checked, Passed: checks.Passed, Reason: checks.Reason}
	if !checks.Passed {
		// The copy is genuinely bad, or at least not provably good. That
		// is a verdict about the artifact, not a failed request, so it is
		// reported rather than returned as an error, exactly like
		// ValidateArtifact reports a failure.
		return result, nil
	}

	out, err := lifecycle.ReinstateFromQuarantine(ctx, s.lifecycleDeps(), lifecycle.QuarantineReinstateParams{
		Artifact:   id,
		AttemptKey: fmt.Sprintf("app:reinstate:%s:%s", id, s.now().Format(time.RFC3339Nano)),
		Evidence:   checks.evidence(),
		Note:       note,
	})
	if err != nil {
		return result, err
	}

	result.Reinstated = true
	result.NewState = lifecycle.State(out.Record.State)
	return result, nil
}
