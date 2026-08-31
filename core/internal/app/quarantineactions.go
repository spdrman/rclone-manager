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
// internal/lifecycle's Transitions table, where QUARANTINED_LOST is the
// one state with no outgoing edge at all.
var ErrQuarantineIrrecoverable = errors.New("app: quarantined artifact has no remaining source to re-ingest")

// RevalidateQuarantined re-runs the durable-local-copy checks against one
// QUARANTINED or QUARANTINED_LOST artifact and reports the verdict,
// writing nothing.
//
// This is deliberately NOT ValidateArtifact with a wider eligible-state
// set. The two differ in what they are for, and in what they may do:
//
//   - ValidateArtifact checks a healthy restore point (COMMITTED,
//     REMOTE_DELETE_PENDING, COMPLETE) and, on a failure, quarantines it.
//     Its whole point is that a bad artifact stops being trusted.
//
//   - This checks an artifact that is ALREADY quarantined, and can move it
//     nowhere at all. The lifecycle graph has no edge from QUARANTINED to
//     any healthy state (its one exit is back to DISCOVERED, which is
//     RetryQuarantinedIngestion below, a re-ingest rather than a
//     rehabilitation), and QUARANTINED_LOST has no exit whatsoever. So a
//     PASS here cannot and must not silently restore the artifact: it is
//     evidence for an operator deciding whether re-ingesting is worth
//     trying, and nothing more.
//
// Writing nothing on either verdict is what makes that honest. A caller
// that wants the artifact back in the pipeline asks for that explicitly.
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
		return ValidateResult{}, fmt.Errorf("app: revalidate: %s has no configured backup set", id.Set)
	}

	checked, passed, reason, err := s.runValidationChecks(ctx, rec, bs.Validation)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("app: revalidate: %w", err)
	}
	return ValidateResult{Artifact: id, Checked: checked, Passed: passed, Reason: reason}, nil
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
// gives QUARANTINED_LOST no outgoing edge for exactly this reason, so the
// refusal here is naming that rule rather than adding a new one.
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
