package app

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// ArtifactFilter narrows ListArtifacts to a subset of configured backup
// sets. An empty Source matches every source; an empty Set matches every
// backup set within whatever sources match Source. This mirrors config's
// own source-then-set nesting (FR-7: identity is source-plus-set, never
// set alone), so a Set filter with no Source is deliberately not a
// shortcut across sources: two different sources may each have a set
// with the same name.
//
// A non-empty Source or Set that names nothing in the loaded config is a
// mistake rather than a filter, and resolve refuses it. See ListArtifacts.
type ArtifactFilter struct {
	Source string
	Set    string
}

// resolve reports whether this filter names anything in sources, and
// returns the *NotFoundError describing what it does not name otherwise.
//
// It answers with an explicit lookup rather than by counting how many
// backup sets matched, because those two questions have different
// answers: a source configured with no backup sets of its own also
// matches nothing, and calling that "no configured source" would name the
// wrong thing.
func (f ArtifactFilter) resolve(sources []config.Source) error {
	if f.Source == "" && f.Set == "" {
		return nil
	}

	sourceFound := false
	for _, src := range sources {
		if f.Source != "" && f.Source != src.Name {
			continue
		}
		sourceFound = true
		if f.Set == "" {
			return nil
		}
		for _, bs := range src.BackupSets {
			if bs.Name == f.Set {
				return nil
			}
		}
	}

	// Only a Source that was actually given can be the thing that is
	// missing. With no Source, sourceFound is false only for a config
	// carrying no sources at all, which config.Validate already refuses
	// (FR-5), and reporting an unnamed source for it would name nothing.
	if !sourceFound && f.Source != "" {
		return &NotFoundError{Kind: "source", Name: f.Source}
	}
	// The set is what is missing. Name it the way FR-7 spells identity,
	// source-plus-set, whenever a source was given to spell it with: the
	// same string fetch reports for the same mistake.
	name := f.Set
	if f.Source != "" {
		name = f.Source + "/" + f.Set
	}
	return &NotFoundError{Kind: "backup set", Name: name}
}

func (f ArtifactFilter) matches(sourceName, setName string) bool {
	if f.Source != "" && f.Source != sourceName {
		return false
	}
	if f.Set != "" && f.Set != setName {
		return false
	}
	return true
}

// ListArtifacts is `backup-manager artifacts`' use case: every journal
// record for every backup set filter selects, in config order (source
// order, then backup-set order within each source), which is the same
// deterministic order Sources() renders in.
//
// # An unconfigured filter is refused, not answered with nothing
//
// A filter naming a source or a backup set the loaded config does not
// have gets a *NotFoundError, the same one Fetch already returns for the
// same mistake, rather than an empty list (issue #187). An empty list has
// to keep meaning one thing: this backup set exists and has no journal
// rows yet. If it also meant "there is no such backup set", then a typo
// in a set name, or a rename that reached config.yaml but not the script
// calling this, would read to an operator as "your backups are not
// there" instead of "you asked about something that does not exist", and
// those two call for opposite responses. FR-7 makes a backup set's
// identity source-plus-set, so a name that appears nowhere in config is
// not an identity this can be a filter over.
func (s *Service) ListArtifacts(ctx context.Context, filter ArtifactFilter) ([]state.Record, error) {
	if err := filter.resolve(s.Config.Sources); err != nil {
		return nil, err
	}

	var out []state.Record
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if !filter.matches(src.Name, bs.Name) {
				continue
			}
			records, err := s.Journal.ListByBackupSet(ctx, bs.ID)
			if err != nil {
				return out, fmt.Errorf("app: artifacts: listing %s: %w", bs.ID, err)
			}
			out = append(out, records...)
		}
	}
	return out, nil
}

// ArtifactDetail is one artifact's full journal row plus, when it is
// currently FAILED, QUARANTINED or QUARANTINED_LOST, the literal
// diagnostic sentence internal/lifecycle recorded on the transition that
// put it there (issue #284).
//
// FailureReason is deliberately not the same thing as
// internal/lifecycle.QuarantineReason: that function is a best-effort
// reconstruction from whatever else the record happens to carry, built
// specifically because its author declined to read state_transitions
// directly (see its own doc). FailureReason is the literal text, read
// from the one place it actually lives, and it is populated for FAILED
// the same way it is for the two quarantine states, since FR-10 gives
// FAILED no reason field anywhere else at all.
type ArtifactDetail struct {
	state.Record

	// FailureReason is empty for any state other than FAILED, QUARANTINED
	// or QUARANTINED_LOST. It can still be empty for one of those three:
	// that means the transition that produced the current state was
	// recorded with no Detail, not that this call failed.
	FailureReason string

	// FailureReasonAt is when the transition that produced FailureReason
	// happened. Only meaningful when FailureReason is non-empty.
	FailureReasonAt time.Time
}

// GetArtifactDetail is `backup-manager artifacts <source/backup-set/name>`'s
// use case (issue #284): until this existed, sqlite3 against the state
// database directly was the only way for an operator to learn why one
// specific artifact reached FAILED or QUARANTINED, because
// state_transitions.detail is written there and read back nowhere else in
// this codebase (see internal/state's LastEnteredDetail, which this
// calls, and internal/lifecycle/quarantine.go's QuarantineReason doc for
// why nothing else durably carries that text).
//
// # Redaction
//
// FailureReason is whatever text the lifecycle step that produced the
// transition wrote (internal/lifecycle/verify.go, quarantine.go,
// internal/reconcile, internal/revalidate, or an operator-triggered
// internal/app.ValidateArtifact call). None of those construct that text
// from an obs.Secret's revealed value (key material only ever leaves its
// Secret wrapper in internal/transport/rclone/ssh.go, to build rclone's
// own connection config, never to build an error or a Detail string), so
// this method cannot surface key material or a known_hosts path. It CAN
// surface whatever those callers' own upstream errors already contain,
// which for a source configured with a non-default port is the port
// itself (issue #295, tracked separately: state_transitions.detail
// already carries that today, independent of this method existing to
// read it back). Once #295's redaction lands where those callers build
// their Detail text, this method inherits it automatically, since it only
// reads back what was written; it does not itself decide what belongs in
// the clear.
func (s *Service) GetArtifactDetail(ctx context.Context, id model.ArtifactID) (ArtifactDetail, error) {
	rec, err := s.Journal.Get(ctx, id)
	if err != nil {
		return ArtifactDetail{}, fmt.Errorf("app: artifact detail: %s: %w", id, err)
	}
	out := ArtifactDetail{Record: rec}

	switch lifecycle.State(rec.State) {
	case lifecycle.Failed, lifecycle.Quarantined, lifecycle.QuarantinedLost:
		detail, at, found, err := s.Journal.LastEnteredDetail(ctx, id, rec.State)
		if err != nil {
			return ArtifactDetail{}, fmt.Errorf("app: artifact detail: %s: %w", id, err)
		}
		if found {
			out.FailureReason = detail
			out.FailureReasonAt = at
		}
	}
	return out, nil
}
