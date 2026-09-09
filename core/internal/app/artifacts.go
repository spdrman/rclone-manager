package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The two read-only artifact queries, and the one place a name that does not
// exist gets refused.
//
// ListArtifacts and GetArtifactDetail are the reads underneath `backup-manager
// artifacts` and the backups screen. Neither writes, neither reaches a
// remote, and both are worth reading for what they refuse rather than for
// what they return.
//
// A filter naming a source or a backup set the configuration does not have
// is a *NotFoundError, never an empty list, because an empty list has to keep
// meaning one thing: this set exists and has no rows yet. Collapse the two
// and a renamed set reads to an operator as "your backups are gone" instead
// of "you asked about something that does not exist", and those call for
// opposite responses.
//
// The other refusal is subtler and runs the other way. Since a backup set's
// configuration can be removed while its journal rows stay, the unfiltered
// list has to widen to sets the configuration no longer names, or the backups
// a removal explicitly promised to keep vanish from the screen that promised
// it. That widening is opt-in per caller, because the same read also feeds
// the quarantine screen, whose three actions all need a configured set to act
// under. ListArtifacts's own doc has the full argument, including why health,
// capacity and retention walk the configuration alone and always will.

// ArtifactFilter narrows ListArtifacts to a subset of configured backup
// sets. An empty Source matches every source; an empty Set matches every
// backup set within whatever sources match Source.
//
// Set is spelled either way round (issue #569). "api-server/var-backups"
// is the id every surface that PRINTS a backup set prints: `sources`,
// `status`, and the heading `retention` puts over each set, which is also
// the operand it takes. It is NOT the id in this listing's own first
// column, which is a whole artifact id and one field longer.
// "var-backups" is the older spelling and still works, but only
// while exactly one source configures that name; FR-7 makes identity
// source-plus-set, so a name two sources share names neither of them and
// resolve refuses it rather than picking one or answering with both.
// That collision is not exotic: one naming convention applied across a
// fleet of hosts produces it on the first host that joins.
//
// A non-empty Source or Set that names nothing in the loaded config is a
// mistake rather than a filter, and resolve refuses it. See ListArtifacts.
type ArtifactFilter struct {
	Source string
	Set    string

	// IncludeUnconfigured widens an UNFILTERED list to the artifacts of
	// backup sets the journal knows and the configuration no longer does
	// (issue #391). It is opt-in, and it is honoured only when Source and
	// Set are both empty; a filter that names something has been through
	// resolve, which refuses an id the configuration does not have, and
	// that refusal stays. See ListArtifacts for which caller asks for
	// this and which deliberately does not.
	IncludeUnconfigured bool
}

// resolve turns this filter into the one whose Source and Set are the
// configured names it selects, and returns the refusal describing what it
// does not select otherwise.
//
// Returning a filter rather than answering yes or no is what lets Set be
// spelled either way round: matches below compares plain names, so the
// source half of a composite id has to come off somewhere, and doing it
// once here keeps every caller of matches reading the same rule. The
// resolved filter is also where the single backup set a filter names can
// be read back (ResolvedSetID), so the CLI and this listing cannot drift
// into two different answers about which set was asked for.
//
// It answers with an explicit lookup rather than by counting how many
// backup sets matched, because those two questions have different
// answers: a source configured with no backup sets of its own also
// matches nothing, and calling that "no configured source" would name the
// wrong thing.
func (f ArtifactFilter) resolve(sources []config.Source) (ArtifactFilter, error) {
	source, set := f.Source, f.Set

	// Issue #569. A Set carrying the separator is the composite id, and
	// it is parsed by the same function that reads every other rendering
	// of one back, so "a/b/c" and "var-backups/" are refused here rather
	// than silently becoming a filter over something else.
	if strings.Contains(set, "/") {
		id, err := model.ParseBackupSetID(set)
		if err != nil {
			return f, fmt.Errorf("app: backup set %q: %w", set, err)
		}
		source, set = id.Source, id.Set
		// Two names for the source, and they disagree. The CLI refuses
		// this on the command line before anything is opened, since it
		// is wrong on every deployment rather than on this one; this is
		// the answer for a caller that built the pair by hand.
		//
		// Deliberately not a *NotFoundError. Both halves of this pair
		// can name real, configured things, and reporting it as one of
		// them missing would print "no configured backup set named
		// cicd-pipeline/var-backups" about a set that is configured,
		// which is the exact untruth #569 was reported for.
		if f.Source != "" && f.Source != source {
			return f, fmt.Errorf("app: backup set %s names source %s, and this filter also names source %s", f.Set, source, f.Source)
		}
	}

	if set == "" {
		if source == "" {
			return f, nil
		}
		for _, src := range sources {
			if src.Name == source {
				return f, nil
			}
		}
		return f, &NotFoundError{Kind: "source", Name: source}
	}

	sourceFound := false
	var found []ArtifactFilter
	for _, src := range sources {
		if source != "" && source != src.Name {
			continue
		}
		sourceFound = true
		for _, bs := range src.BackupSets {
			if bs.Name == set {
				found = append(found, ArtifactFilter{Source: src.Name, Set: bs.Name, IncludeUnconfigured: f.IncludeUnconfigured})
			}
		}
	}

	switch {
	case len(found) == 1:
		return found[0], nil

	// More than one only happens for a bare name with no source to narrow
	// it, since a source may not configure the same set name twice. The
	// candidates go back with the refusal because retyping one of them is
	// the whole remedy, and an operator cannot retype what they were not
	// shown.
	case len(found) > 1:
		candidates := make([]string, 0, len(found))
		for _, c := range found {
			candidates = append(candidates, c.Source+"/"+c.Set)
		}
		return f, &AmbiguousSetError{Name: set, Candidates: candidates}

	// Only a Source that was actually given can be the thing that is
	// missing. With no Source, sourceFound is false only for a config
	// carrying no sources at all, which config.Validate already refuses
	// (FR-5), and reporting an unnamed source for it would name nothing.
	case !sourceFound && source != "":
		return f, &NotFoundError{Kind: "source", Name: source}
	}

	// The set is what is missing. Name it the way FR-7 spells identity,
	// source-plus-set, whenever a source was given to spell it with: the
	// same string fetch reports for the same mistake.
	name := set
	if source != "" {
		name = source + "/" + set
	}
	return f, &NotFoundError{Kind: "backup set", Name: name}
}

// ResolvedSetID is the "source/set" id this filter names, or the empty
// string when it names none or more than one.
//
// It exists for the caller that has to tell a running engine which backup
// set a listing is about (GET /backups filters on one such id and on
// nothing else), and it is a method on the filter rather than a second
// walk over the configuration in that caller specifically because there
// used to be two walks. The one over there predated the composite id and
// silently returned "" for a bare name two sources shared, which is how
// an ambiguous filter came to be compared against the whole journal
// instead of being refused (#569).
//
// A refusal comes back as the empty id rather than as an error, because
// every caller reaches this after ListArtifacts has already refused the
// same filter with the same rule and a message written for the operator.
func (f ArtifactFilter) ResolvedSetID(sources []config.Source) string {
	resolved, err := f.resolve(sources)
	if err != nil || resolved.Set == "" {
		return ""
	}
	return resolved.Source + "/" + resolved.Set
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

// ListArtifacts is `rbm artifacts`' use case: every journal
// record for every backup set filter selects, in config order (source
// order, then backup-set order within each source), which is the same
// deterministic order Sources() renders in.
//
// # Backup sets that are no longer configured
//
// An UNFILTERED list that asks for it (IncludeUnconfigured) also carries
// the artifacts of backup sets the journal knows about and the
// configuration no longer does (issue #391). Before removal existed those
// two sets of ids were always the same, so walking the configuration was
// a complete answer; it is not any more, and the difference is exactly
// what removal is required to preserve. The confirmation an operator
// accepts says the retained backups "stay on NAS storage and remain
// listed under Backups", and the backups list is this call with no
// filter, so a config-only walk would have made those backups vanish
// from the one screen that was promised they would not.
//
// The widening is opt-in because this call feeds two screens, not one.
// The backups list (core/service.ListArtifacts with no filter, the
// `artifacts` command with none) asks for it. The quarantine list is
// the same read with a filter on top, and it deliberately does NOT ask:
// that screen carries three write actions, every one of which needs the
// artifact's backup set to be configured (the checks run under the set's
// validation policy, and a retry hands the row back to a pipeline that
// only walks configured sets), so a quarantined row under a removed set
// would be a row with three buttons none of which can do anything good.
// It stays on the backups list, marked quarantined, and comes back to
// the quarantine screen the moment the set is configured again. Health
// (FR-24), capacity (FR-21) and retention walk the configuration alone
// and always will: a removed set has no freshness to assess, no forecast
// to make and no policy to apply.
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
	// The resolved filter, not the one that came in: matches compares
	// plain source and set names, and the Set an operator typed may be
	// the whole "source/set" id (#569).
	resolved, err := filter.resolve(s.Config.Sources)
	if err != nil {
		return nil, err
	}

	var out []state.Record
	configured := map[string]bool{}
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			configured[bs.ID.String()] = true
			if !resolved.matches(src.Name, bs.Name) {
				continue
			}
			records, err := s.Journal.ListByBackupSet(ctx, bs.ID)
			if err != nil {
				return out, fmt.Errorf("app: artifacts: listing %s: %w", bs.ID, err)
			}
			out = append(out, records...)
		}
	}

	// Then the sets that are gone. See this function's own doc for why
	// they belong here at all; the shape is what needs explaining.
	//
	// Appended after the configured ones rather than merged into them, so
	// the "config order, then backup-set order within each source" this
	// function has always promised still describes the part of the list
	// the configuration can account for, and a deployment that has never
	// removed anything gets a byte-identical answer to the one it got
	// before.
	//
	// Only for a filter that names nothing AND asked for it. A filter
	// naming a specific source or set has already been through resolve
	// above, which refuses an id the configuration does not have (issue
	// #187), and that refusal stays: "no such backup set" and "this
	// backup set has no artifacts" must not collapse into one answer
	// just because removal now exists.
	if filter.Source == "" && filter.Set == "" && filter.IncludeUnconfigured {
		known, err := s.Journal.ListBackupSetIDs(ctx)
		if err != nil {
			return out, fmt.Errorf("app: artifacts: listing the backup sets on record: %w", err)
		}
		for _, id := range known {
			if configured[id.String()] {
				continue
			}
			records, err := s.Journal.ListByBackupSet(ctx, id)
			if err != nil {
				return out, fmt.Errorf("app: artifacts: listing %s: %w", id, err)
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

	// Copies is one entry per durable copy of this artifact, each with
	// the access state FR-34 defines: whether its bytes can be read right
	// now, and what it would take if they cannot. It is nil for a record
	// with no placements, which is what a record built by hand has and
	// what an artifact that has not yet been transferred has.
	Copies []ArtifactCopy
}

// GetArtifactDetail is `rbm artifacts <source/backup-set/name>`'s
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
	out := ArtifactDetail{Record: rec, Copies: s.artifactCopies(rec, s.now())}

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
