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
	// no configured backup set matches nothing, which is not an error: a
	// filter is a filter, and a caller asking about a set that has since
	// been removed gets an empty list rather than a refusal.
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
			// backup set, so it selects nothing. Same reasoning as an id
			// that is well-formed but unknown.
			return nil, nil
		}
		appFilter.Source, appFilter.Set = source, set
	}

	records, err := st.inner.ListArtifacts(ctx, appFilter)
	if err != nil {
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
