// This file is issue #418's boundary read model: the backup sets the
// journal remembers and the configuration no longer names, in the
// provider-agnostic shape §7.2 requires of anything an apps/ package can
// see.
//
// internal/app/unconfigured.go holds the whole argument for why this
// category exists, what its lifecycle is, and why clearing a stranded
// .partial destroys nothing. Nothing is repeated here; this file is the
// translation and the refusals, and it exists because a category of
// backup that is retained, growing and governed by no policy has to be
// answerable to every surface, not only to a terminal.
//
// # What is deliberately not here
//
// No route deletes a retained backup of an unconfigured set, and there is
// no request shape that could ask for one. The removal that produced
// these artifacts is config-only by design (#391, backupsetremove.go),
// and adding a "and the backups too" operation on the far side of it
// would put the destructive option back one screen away from the
// non-destructive one it was deliberately separated from. An operator who
// wants those bytes aged out creates the set again and lets its own
// retention chain do it under FR-20's checks.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// ErrBackupSetConfigured refuses a call that only applies to a backup set
// the configuration no longer names. It is distinct from
// ErrBackupSetNotFound on purpose: this id names a live, configured set,
// and telling a caller "no such backup set" about one sitting on their own
// dashboard is a worse answer than no answer.
var ErrBackupSetConfigured = errors.New("service: backup set is still configured")

// UnconfiguredBackupSet is one backup set the journal holds history for
// and the running configuration does not name, with what it still holds.
type UnconfiguredBackupSet struct {
	// BackupSetID is "source/set", the same id GetBackupSet takes and
	// answers ErrBackupSetNotFound for.
	BackupSetID string
	SourceName  string
	SetName     string

	// Artifacts is every journal row on record for it.
	Artifacts int

	// Retained is how many of those are a finished backup holding a
	// durable local copy: the ones the removal confirmation promised
	// would stay on storage.
	Retained int

	// Stranded is how many were caught mid-acquisition and will never
	// move again, because the processing cycle walks configured sets.
	// ClearStrandedArtifacts is what ends them.
	Stranded int

	// Quarantined is how many are held for a human and cannot be acted on
	// while the set is unconfigured: revalidate, retry and reinstate all
	// refuse a row whose backup set the configuration does not name.
	Quarantined int

	// Bytes is what these occupy on local storage, as recorded.
	Bytes int64

	// RetentionPolicy is what governs them, and it is a field rather than
	// an omission because that is issue #418's third acceptance
	// criterion: the surface that lists these backups has to SAY which
	// policy applies, including none. A caller that has to infer it from
	// a missing field will render a blank where the important word goes.
	//
	// It is always "none" today. It is a string rather than a bool so
	// that a future answer ("the policy this set last ran under") is a
	// value change rather than a schema change.
	RetentionPolicy string

	FirstDiscovered time.Time
	LastActivity    time.Time
}

// StrandedArtifact is one acquisition-state journal row of an
// unconfigured backup set: work a cycle started and no cycle will finish.
type StrandedArtifact struct {
	// ID is "source/set/name".
	ID string
	// State is the FR-10 lifecycle state it is stuck in.
	State string
	// ResiduePath is the .partial file it points at, empty when it never
	// got as far as writing one.
	ResiduePath string
	// ResidueBytes is what that file measures right now.
	ResidueBytes int64
	// Cleared reports that this call removed the residue and ended the
	// row. Always false from PreviewStrandedArtifacts.
	Cleared bool
	// Refusal is why this row was left alone, empty when it was not.
	Refusal string
}

// UnconfiguredBackupSets is every backup set the journal remembers and
// this configuration does not name, in id order.
func (b *BackupService) UnconfiguredBackupSets(ctx context.Context) ([]UnconfiguredBackupSet, error) {
	sets, err := b.state.Load().inner.UnconfiguredSets(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: listing unconfigured backup sets: %w", err)
	}
	out := make([]UnconfiguredBackupSet, 0, len(sets))
	for _, u := range sets {
		out = append(out, UnconfiguredBackupSet{
			BackupSetID:     u.Set.String(),
			SourceName:      u.Set.Source,
			SetName:         u.Set.Set,
			Artifacts:       u.Artifacts,
			Retained:        u.Retained,
			Stranded:        u.Stranded,
			Quarantined:     u.Quarantined,
			Bytes:           u.Bytes,
			RetentionPolicy: "none",
			FirstDiscovered: u.FirstDiscovered,
			LastActivity:    u.LastActivity,
		})
	}
	return out, nil
}

// PreviewStrandedArtifacts reports what ClearStrandedArtifacts would do to
// one unconfigured backup set, and writes nothing.
func (b *BackupService) PreviewStrandedArtifacts(ctx context.Context, id string) ([]StrandedArtifact, error) {
	set, err := b.unconfiguredSetID(id)
	if err != nil {
		return nil, err
	}
	found, err := b.state.Load().inner.StrandedArtifacts(ctx, set)
	return toServiceStranded(found), translateStrandedErr(id, err)
}

// ClearStrandedArtifacts removes the .partial residue of one unconfigured
// backup set's stranded rows and ends them.
//
// It never touches a retained backup and has no way to: the app layer
// acts only on acquisition-state rows, refuses any row carrying a durable
// placement, and refuses any path that is not one of FR-12's .partial
// names. See internal/app/unconfigured.go's clearOne.
func (b *BackupService) ClearStrandedArtifacts(ctx context.Context, id string) ([]StrandedArtifact, error) {
	set, err := b.unconfiguredSetID(id)
	if err != nil {
		return nil, err
	}
	found, err := b.state.Load().inner.ClearStranded(ctx, set)
	return toServiceStranded(found), translateStrandedErr(id, err)
}

// unconfiguredSetID parses a "source/set" id for the two calls above. A
// syntactically impossible id cannot name anything, and is refused with
// the same sentinel a well-formed unknown one gets, so a caller never has
// to tell the two apart.
func (b *BackupService) unconfiguredSetID(id string) (model.BackupSetID, error) {
	source, set, ok := splitBackupSetID(id)
	if !ok {
		return model.BackupSetID{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}
	parsed, err := model.NewBackupSetID(source, set)
	if err != nil {
		return model.BackupSetID{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}
	return parsed, nil
}

// translateStrandedErr maps the app layer's two refusals onto this
// package's own sentinels, which is the whole of §7.2's boundary rule
// here: a caller outside core/ cannot name *app.NotFoundError, and it
// must still be able to tell "no such backup set" from "that backup set
// is configured", because those call for opposite responses.
func translateStrandedErr(id string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, app.ErrBackupSetConfigured) {
		return fmt.Errorf("%w: %s", ErrBackupSetConfigured, id)
	}
	var notFound *app.NotFoundError
	if errors.As(err, &notFound) {
		return fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}
	return fmt.Errorf("service: stranded artifacts of %s: %w", id, err)
}

func toServiceStranded(found []app.StrandedArtifact) []StrandedArtifact {
	if len(found) == 0 {
		return nil
	}
	out := make([]StrandedArtifact, 0, len(found))
	for _, s := range found {
		row := StrandedArtifact{
			ID:           s.Artifact.String(),
			State:        string(s.State),
			ResiduePath:  s.PartialPath,
			ResidueBytes: s.PartialBytes,
			Cleared:      s.Cleared,
		}
		if s.Err != nil {
			row.Refusal = s.Err.Error()
		}
		out = append(out, row)
	}
	return out
}
