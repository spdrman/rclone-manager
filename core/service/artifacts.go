package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// ErrArtifactNotFound is returned when no journal row exists for the
// artifact id a caller named.
var ErrArtifactNotFound = errors.New("service: artifact not found")

// ErrArtifactNotQuarantined is returned by RevalidateArtifact and
// RetryArtifactIngestion when the artifact named is not in a quarantine
// state. It exists so the API layer can answer a typed 409 rather than a
// 500 for what is an ordinary, expected refusal.
var ErrArtifactNotQuarantined = errors.New("service: artifact is not quarantined")

// ErrArtifactIrrecoverable is returned by RetryArtifactIngestion for an
// artifact whose remote source is already confirmed deleted, so nothing
// remains anywhere to re-ingest.
var ErrArtifactIrrecoverable = errors.New("service: artifact has no remaining source to re-ingest")

// ErrReinstatementRefused is returned by ReinstateArtifact when the checks
// it ran are not enough to trust the artifact again, or when the artifact
// never held the state it would be returned to.
//
// It is deliberately distinct from a failing check. A failing check says
// the durable local copy is bad, which no configuration change fixes; this
// says the copy may well be fine but what could be proved about it does
// not justify re-trusting it, which an operator CAN act on (repair the
// validator the backup set names, so it runs and passes, or re-ingest
// instead). Collapsing the two would leave an operator with no way to tell
// "your backup is gone" from "run the validator".
var ErrReinstatementRefused = errors.New("service: the evidence is not enough to trust this artifact again")

// Artifact is the plain, provider-agnostic shape of one journal row: what
// this backup is, where it came from, whether it is trustworthy, and
// whether the remote source has been released.
//
// It mirrors internal/state.Record the way Operation mirrors
// state.Operation, and it deliberately does not carry every column that
// record has: retry bookkeeping, the next scheduled retry and the
// backend's own object identity are internal scheduling detail a caller
// outside core/ has no use for and should not be able to key on.
type Artifact struct {
	// ID is "source/set/name", model.ArtifactID.String().
	ID string
	// BackupSetID is "source/set", the same id GetBackupSet takes.
	BackupSetID string
	SourceName  string
	SetName     string
	// Name is the artifact's own filename within its backup set.
	Name string

	RemotePath string
	LocalPath  string

	// State is the FR-10 lifecycle state string this artifact is
	// currently recorded in ("DISCOVERED", "COMMITTED", "COMPLETE",
	// "QUARANTINED", ...).
	State string

	DiscoveredAt time.Time
	UpdatedAt    time.Time

	// SizeBytes is what the remote reported at discovery, or 0 when the
	// remote never reported a size at all.
	SizeBytes int64

	// Checksum is the hash recorded for the durable local copy at
	// verification, with the algorithm that produced it. Both are empty
	// when nothing has been hashed yet, or when the backup set is
	// configured for transfer verification alone.
	Checksum          string
	ChecksumAlgorithm string

	// Validation is "passed", "failed" or "pending": the tri-state
	// state.Record models as a *bool, spelled out so a caller never has
	// to decide what a nil means.
	Validation       string
	ValidationDetail string

	// RemoteSourceRemovedAt is the moment the remote source object was
	// deleted, or the zero time while it is still there. Remote deletion
	// is a lifecycle fact, never something a caller asks for.
	RemoteSourceRemovedAt time.Time

	// Quarantined is true for the two quarantine states.
	// QuarantineIrrecoverable narrows that to QUARANTINED_LOST, the one
	// case with no remote source left to re-ingest from.
	Quarantined             bool
	QuarantineIrrecoverable bool
	// QuarantineReason is the last error recorded against this artifact,
	// which is what routed it into quarantine. Empty when it is not
	// quarantined.
	QuarantineReason string

	// RetentionTier is the tier that most recently selected this artifact
	// for retention, or empty.
	RetentionTier string
}

// ArtifactFilter narrows ListArtifacts. An empty BackupSetID matches every
// backup set; QuarantinedOnly restricts the result to the two quarantine
// states.
type ArtifactFilter struct {
	// BackupSetID is a "source/set" id, matched exactly. An id that names
	// no configured backup set is REFUSED with ErrBackupSetNotFound
	// rather than answered with an empty list, following the rule issue
	// #187 established for the same filter on the CLI side: an empty list
	// has to keep meaning one thing, "this backup set exists and has no
	// backups yet". If it also meant "there is no such backup set", a
	// renamed set would read to an operator as "your backups are gone",
	// and those two call for opposite responses.
	BackupSetID     string
	QuarantinedOnly bool
}

// ListArtifacts returns every journal row the filter selects, in config
// order (source order, then backup-set order, then journal order within
// each set), which is the same deterministic order the `artifacts` CLI
// command renders.
func (b *BackupService) ListArtifacts(ctx context.Context, filter ArtifactFilter) ([]Artifact, error) {
	st := b.state.Load()

	appFilter := app.ArtifactFilter{}
	if filter.BackupSetID != "" {
		source, set, ok := splitBackupSetID(filter.BackupSetID)
		if !ok {
			// A syntactically impossible id cannot name a configured
			// backup set. Refused for the same reason a well-formed but
			// unknown one is, and with the same sentinel, so a caller
			// never has to tell the two apart.
			return nil, fmt.Errorf("%w: %s", ErrBackupSetNotFound, filter.BackupSetID)
		}
		appFilter.Source, appFilter.Set = source, set
	}

	records, err := st.inner.ListArtifacts(ctx, appFilter)
	if err != nil {
		// internal/app refuses a filter naming nothing (#187). Translated
		// to this package's own sentinel rather than passed through: a
		// caller outside core/ cannot name *app.NotFoundError, and the
		// distinction between "no such source" and "no such backup set"
		// is not one an API client can act on differently.
		var notFound *app.NotFoundError
		if errors.As(err, &notFound) {
			return nil, fmt.Errorf("%w: %s", ErrBackupSetNotFound, filter.BackupSetID)
		}
		return nil, fmt.Errorf("service: listing artifacts: %w", err)
	}

	out := make([]Artifact, 0, len(records))
	for _, rec := range records {
		a := toServiceArtifact(rec)
		if filter.QuarantinedOnly && !a.Quarantined {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// GetArtifact returns one artifact by its "source/set/name" id, or
// ErrArtifactNotFound.
func (b *BackupService) GetArtifact(ctx context.Context, id string) (Artifact, error) {
	artifactID, err := app.ParseArtifactID(id)
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, id)
	}
	rec, err := b.journal.Get(ctx, artifactID)
	if err != nil {
		if errors.Is(err, state.ErrArtifactNotFound) {
			return Artifact{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, id)
		}
		return Artifact{}, fmt.Errorf("service: loading artifact %s: %w", id, err)
	}
	return toServiceArtifact(rec), nil
}

// RevalidateArtifact re-runs the durable-local-copy checks against one
// quarantined artifact and reports the verdict. It writes nothing: see
// internal/app.RevalidateQuarantined for why a passing check may not
// rehabilitate a quarantined artifact by itself.
func (b *BackupService) RevalidateArtifact(ctx context.Context, id string) (ArtifactCheck, error) {
	artifactID, err := app.ParseArtifactID(id)
	if err != nil {
		return ArtifactCheck{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, id)
	}

	result, err := b.state.Load().inner.RevalidateQuarantined(ctx, artifactID)
	switch {
	case errors.Is(err, state.ErrArtifactNotFound):
		return ArtifactCheck{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, id)
	case errors.Is(err, app.ErrNotQuarantined):
		return ArtifactCheck{}, fmt.Errorf("%w: %s", ErrArtifactNotQuarantined, id)
	case err != nil:
		return ArtifactCheck{}, fmt.Errorf("service: revalidating %s: %w", id, err)
	}
	return ArtifactCheck{Checked: result.Checked, Passed: result.Passed, Reason: result.Reason}, nil
}

// ArtifactCheck is RevalidateArtifact's verdict. Checked is false when
// there was nothing to examine at all, which is still not a pass.
type ArtifactCheck struct {
	Checked bool
	Passed  bool
	Reason  string
}

// ArtifactReinstatement is ReinstateArtifact's outcome.
//
// Reinstated and Passed are separate on purpose. Passed is the verdict of
// the checks that ran; Reinstated is whether the artifact actually moved.
// They agree in the ordinary cases and a caller still must not conflate
// them, because "the checks passed and it moved" and "the checks failed
// and it did not" are not the only two outcomes this can have.
type ArtifactReinstatement struct {
	Checked bool
	Passed  bool
	Reason  string

	// Reinstated is true only when the artifact was actually returned to
	// a trusted state.
	Reinstated bool

	// State is the lifecycle state the artifact was returned to
	// ("COMMITTED" or "COMPLETE"), empty when nothing moved.
	State string
}

// ReinstateArtifact re-checks one quarantined artifact's durable local copy
// and, when what it finds is enough, returns it to the state it already
// held so it counts as a restore point again.
//
// This is the answer for the two cases RetryArtifactIngestion cannot
// serve: the local copy is fine and the quarantine was the mistake, or the
// remote source is gone while the local copy is intact, so there is
// nothing left to re-ingest from. Before it existed the only remaining
// option was to leave the artifact quarantined forever.
//
// A reinstated artifact never authorises a remote delete again. That
// forfeiture is permanent and is what makes the whole action safe to
// offer; see core/internal/lifecycle's DeleteRemote for where it is
// enforced and why it is enforced there.
func (b *BackupService) ReinstateArtifact(ctx context.Context, id, note string) (ArtifactReinstatement, error) {
	artifactID, err := app.ParseArtifactID(id)
	if err != nil {
		return ArtifactReinstatement{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, id)
	}

	result, err := b.state.Load().inner.ReinstateQuarantined(ctx, artifactID, note)
	switch {
	case errors.Is(err, state.ErrArtifactNotFound):
		return ArtifactReinstatement{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, id)
	case errors.Is(err, app.ErrNotQuarantined):
		return ArtifactReinstatement{}, fmt.Errorf("%w: %s", ErrArtifactNotQuarantined, id)
	case err != nil:
		// internal/lifecycle's own two refusals are business outcomes an
		// operator reads and acts on, not infrastructure failures, so they
		// are translated rather than passed through as a generic error.
		// A caller outside core/ cannot name those types, and the
		// distinction between "the evidence was inconclusive" and "this
		// artifact never held that state" is not one an API client acts on
		// differently: both mean "not on this evidence", and both carry
		// their own explanation in the message.
		if _, ok := lifecycle.AsInsufficientEvidence(err); ok {
			return ArtifactReinstatement{}, fmt.Errorf("%w: %v", ErrReinstatementRefused, err)
		}
		if _, ok := lifecycle.AsNeverHeldTargetState(err); ok {
			return ArtifactReinstatement{}, fmt.Errorf("%w: %v", ErrReinstatementRefused, err)
		}
		if _, ok := lifecycle.AsNotQuarantined(err); ok {
			return ArtifactReinstatement{}, fmt.Errorf("%w: %s", ErrArtifactNotQuarantined, id)
		}
		return ArtifactReinstatement{}, fmt.Errorf("service: reinstating %s: %w", id, err)
	}

	return ArtifactReinstatement{
		Checked:    result.Checked,
		Passed:     result.Passed,
		Reason:     result.Reason,
		Reinstated: result.Reinstated,
		State:      string(result.NewState),
	}, nil
}

// RetryArtifactIngestion returns one QUARANTINED artifact to DISCOVERED so
// the ordinary pipeline attempts it again, or refuses with
// ErrArtifactIrrecoverable for a QUARANTINED_LOST one.
func (b *BackupService) RetryArtifactIngestion(ctx context.Context, id string) error {
	artifactID, err := app.ParseArtifactID(id)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrArtifactNotFound, id)
	}

	err = b.state.Load().inner.RetryQuarantinedIngestion(ctx, artifactID)
	switch {
	case errors.Is(err, state.ErrArtifactNotFound):
		return fmt.Errorf("%w: %s", ErrArtifactNotFound, id)
	case errors.Is(err, app.ErrQuarantineIrrecoverable):
		return fmt.Errorf("%w: %s", ErrArtifactIrrecoverable, id)
	case errors.Is(err, app.ErrNotQuarantined):
		return fmt.Errorf("%w: %s", ErrArtifactNotQuarantined, id)
	case err != nil:
		return fmt.Errorf("service: retrying ingestion of %s: %w", id, err)
	}
	return nil
}

// splitBackupSetID splits a "source/set" id into its two halves. A
// model.BackupSetID's own two parts may not themselves contain "/"
// (model.NewBackupSetID refuses it), so exactly one separator is expected.
func splitBackupSetID(id string) (source, set string, ok bool) {
	source, set, found := strings.Cut(id, "/")
	if !found || source == "" || set == "" || strings.Contains(set, "/") {
		return "", "", false
	}
	return source, set, true
}

func toServiceArtifact(rec state.Record) Artifact {
	a := Artifact{
		ID:                rec.Artifact.String(),
		BackupSetID:       rec.Artifact.Set.String(),
		SourceName:        rec.Artifact.Set.Source,
		SetName:           rec.Artifact.Set.Set,
		Name:              rec.Artifact.Name,
		RemotePath:        rec.RemotePath,
		LocalPath:         rec.LocalPath,
		State:             rec.State,
		DiscoveredAt:      rec.DiscoveredAt,
		UpdatedAt:         rec.UpdatedAt,
		Checksum:          rec.LocalHash,
		ChecksumAlgorithm: rec.LocalHashAlg,
		ValidationDetail:  rec.ValidationDetail,
		RetentionTier:     rec.RetentionTier,
	}

	if rec.Remote.Size != nil {
		a.SizeBytes = *rec.Remote.Size
	}
	if rec.RemoteDeletedAt != nil {
		a.RemoteSourceRemovedAt = *rec.RemoteDeletedAt
	}

	switch {
	case rec.ValidationPassed == nil:
		a.Validation = "pending"
	case *rec.ValidationPassed:
		a.Validation = "passed"
	default:
		a.Validation = "failed"
	}

	switch lifecycle.State(rec.State) {
	case lifecycle.Quarantined:
		a.Quarantined = true
	case lifecycle.QuarantinedLost:
		a.Quarantined = true
		a.QuarantineIrrecoverable = true
	}
	if a.Quarantined {
		// LastError is what the transition into quarantine recorded, and
		// ValidationDetail is what a failed validator said; either can be
		// the reason, and neither is guaranteed to be set.
		a.QuarantineReason = rec.LastError
		if a.QuarantineReason == "" {
			a.QuarantineReason = rec.ValidationDetail
		}
	}
	return a
}
