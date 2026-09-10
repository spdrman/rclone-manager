package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/mediumcheck"
)

// Proving a storage medium works before a real backup finds out for an
// operator (issue #443).
//
// internal/mediumcheck owns what a working medium means and what each step
// proves. This file is the two decisions about who may ask and when, and both
// are narrower than the equivalent for a backup set's source.
//
// There are two entry points, and until G2.2 (#594) there was one. The
// paragraph that used to stand here said a medium is declared in the
// configuration file and nowhere else, so this could only ever take an
// id: the only fields that would make a candidate meaningful are the
// three credential references, and putting those on a request body turns
// a path on this host, or the name of an environment variable, into
// something an API caller sends.
//
// That premise is what changed, and it changed on its own terms rather
// than by being overruled. core/service now mints an opaque credential id
// (ImportMediumCredentials), which is a FOURTH spelling of a reference:
// it is generated here, it names nothing about this host, and it resolves
// server-side to a 0600 file this manager wrote beside config.yaml. A
// candidate carrying one is meaningful without a path or a variable name
// ever reaching a request body, which is exactly what the old paragraph
// said could not exist. So PreflightMediumCandidate below takes a
// candidate, and the wizard can check a destination BEFORE it is written
// down, which is the ordering FR-30 wants: a destination that fails
// verification is one no artifact was ever pointed at.
//
// This package still takes no position on where the reference came from.
// config.StorageMedium is the same struct either way, and the boundary
// that accepts a candidate from outside (core/service) is where the rule
// about which spellings an API caller may send is written and enforced.
//
// And nothing calls it on a schedule. It writes a probe object into somebody's
// bucket and deletes it, which is entirely reasonable when a person asked and
// entirely unreasonable every poll interval forever.
//
// The two refusals in front of it are separate on purpose. A medium the
// configuration does not declare is a typo an operator can fix. A declared
// medium this build cannot resolve is a different problem, and reporting it
// as "not declared" sends somebody looking for a typo that is not there.

// PreflightMedium proves one declared storage medium actually works,
// before a cycle carrying a real backup finds out for an operator (issue
// #443). See internal/mediumcheck for what it proves and why each step is
// there.
//
// # It takes an id, never a medium
//
// A backup set's test-connection takes a CANDIDATE: host, user, key
// reference, all of it unsaved, because the wizard's whole job is to prove
// a source before anything is written down. This one deliberately does not
// work that way, and the difference is FR-33.
//
// A medium is declared in the configuration file and nowhere else: there
// is no API that creates one, by design, because the only fields that
// would make a candidate medium meaningful are the three credential
// references, and putting those on a request body makes a path on the host
// or the name of a variable into something an API caller sends. Every
// medium this can be asked about is one an administrator already wrote
// down, and this reads the reference out of the configuration rather than
// being handed one.
//
// That is also all the settings form needs. A form pointing a retention
// tier at a medium is choosing among the mediums the configuration
// already declares, so running this against the id it is about to name
// answers the question it actually has, before the save.
//
// # It is never part of a cycle
//
// Nothing calls this on a schedule and nothing calls it from RunCycle. It
// writes a probe object to somebody's bucket and deletes it, which is a
// perfectly reasonable thing to do when a person asked and an unreasonable
// thing to do every poll interval forever.
func (s *Service) PreflightMedium(ctx context.Context, id string) (mediumcheck.Report, error) {
	if s.Config == nil {
		return mediumcheck.Report{}, fmt.Errorf("app: preflight: this instance has no configuration to read a storage medium out of")
	}
	if s.MediumStore == nil {
		return mediumcheck.Report{}, fmt.Errorf("app: preflight: this instance has no way to reach a storage medium")
	}

	// Declared first, resolvable second, and they are asked separately
	// because they are different answers. A medium id the configuration
	// does not declare is a named refusal, the same shape every other
	// surface in this package gives a name it cannot resolve: the operator
	// typed something and what they need back is "that is not one of the
	// mediums you declared". A DECLARED medium this build cannot resolve
	// is a different thing entirely, and reporting it as "not declared"
	// would send somebody looking for a typo that is not there.
	if !s.declaresMedium(id) {
		return mediumcheck.Report{}, fmt.Errorf("app: preflight: %w: %q", ErrMediumNotDeclared, id)
	}
	medium, class, err := MediumResolver(s.Config.StorageMediums).Resolve(id)
	if err != nil {
		return mediumcheck.Report{}, fmt.Errorf("app: preflight: %w", err)
	}

	deps := mediumcheck.Deps{
		Store: s.MediumStore,
		// The one place the classified cause is allowed to go. See
		// internal/mediumcheck's package doc on FR-33: the Report carries
		// this package's own sentences, and what actually came back names
		// a path or a variable on this host, so it goes to the operator's
		// log where their diagnostics already are.
		Observe: func(step mediumcheck.Step, err error) {
			s.logger().Error(ctx, "medium-preflight", fmt.Errorf("storage medium %q, %s check: %w", id, step, err))
		},
	}
	return mediumcheck.Run(ctx, deps, medium, class)
}

// declaresMedium reports whether the running configuration names id at
// all. config.MediumLocal is deliberately not a medium here: it is the
// reserved id for a backup set's own local_path, reached through the local
// store and never through this boundary, and MediumResolver refuses it in
// so many words.
func (s *Service) declaresMedium(id string) bool {
	if id == "" || id == config.MediumLocal {
		return false
	}
	for _, m := range s.Config.StorageMediums {
		if m.ID == id {
			return true
		}
	}
	return false
}

// AsMediumNotDeclared reports whether err is, or wraps,
// ErrMediumNotDeclared, so the layers above turn it into a named 404
// rather than a 500. It exists beside the sentinel rather than at each
// call site for the reason every other As* in this codebase does: an
// errors.Is spelled out three times is three chances to spell it
// differently.
func AsMediumNotDeclared(err error) bool { return errors.Is(err, ErrMediumNotDeclared) }

// MediumCandidate is a storage medium that is not (yet) declared in the
// running configuration: a destination somebody has just described and
// wants proven before it is written down.
//
// It is config.StorageMedium rather than a struct of its own, so a
// candidate and a declared medium cannot be described differently and
// then behave differently. The id is still required, because the report
// names it and because a probe key is derived from the prefix, but it is
// deliberately not checked for uniqueness here: whether the id is free is
// a question about a configuration this function does not write.
type MediumCandidate = config.StorageMedium

// PreflightMediumCandidate proves a storage medium that IS NOT declared,
// through the identical mediumcheck.Run every declared medium goes
// through (G2.2, issue #594).
//
// It writes nothing, whatever the report says. That is the whole contract
// and it is worth stating separately from "it does not persist the
// candidate": it also does not adopt it, cache it, or leave it anywhere a
// later call could find it. The only side effect this has is the one
// PreflightMedium has, which is a probe object written into somebody's
// bucket and deleted again.
//
// The credential reference inside candidate has already been resolved by
// the caller into something this deployment is allowed to open. This
// function does not, and must not, re-derive it: core/service is where an
// opaque credentials id becomes a server-side path, and a second
// resolution here would be a second answer to "which file", which is the
// one question a credential reference cannot have two answers to.
func (s *Service) PreflightMediumCandidate(ctx context.Context, candidate MediumCandidate) (mediumcheck.Report, error) {
	if s.MediumStore == nil {
		return mediumcheck.Report{}, fmt.Errorf("app: preflight: this instance has no way to reach a storage medium")
	}
	if candidate.ID == "" {
		return mediumcheck.Report{}, fmt.Errorf("app: preflight: a candidate storage medium needs an id")
	}
	if candidate.ID == config.MediumLocal {
		return mediumcheck.Report{}, fmt.Errorf(
			"app: preflight: %q is the implicit local medium (a backup set's own local_path), not a destination that can be declared",
			config.MediumLocal)
	}

	// MediumResolver over a one-element list, rather than a second
	// translation from config.StorageMedium to transport.Medium written
	// here. The mapping from a declared medium to a reachable one, and
	// from upload_verification to a placement.Class, is FR-31's and it
	// has exactly one home (mediums.go). A candidate resolved by a
	// different route is a candidate that could be proven under rules the
	// saved medium will not run under.
	medium, class, err := MediumResolver([]config.StorageMedium{candidate}).Resolve(candidate.ID)
	if err != nil {
		return mediumcheck.Report{}, fmt.Errorf("app: preflight: %w", err)
	}

	deps := mediumcheck.Deps{
		Store: s.MediumStore,
		Observe: func(step mediumcheck.Step, err error) {
			s.logger().Error(ctx, "medium-preflight", fmt.Errorf("candidate storage medium %q, %s check: %w", candidate.ID, step, err))
		},
	}
	return mediumcheck.Run(ctx, deps, medium, class)
}

// PreflightLocalMedium proves the LOCAL hard drive works: the directory
// this deployment's backups land in, checked the same way and reported in
// the same shape as a storage medium (H2.2, issue #622).
//
// It is a separate entry point rather than a branch inside PreflightMedium
// above, and the reason is the one declaresMedium already states: the
// reserved local id is not a medium this boundary resolves. MediumResolver
// refuses it in so many words, there is no config.StorageMedium behind it
// and no transport.MediumStore that could reach it, so a branch inside
// that function would be a function whose two halves share nothing but a
// name. What they DO share is the Report, which is the part a surface
// cares about.
//
// The thresholds come from this Service's own resolved Capacity rather
// than from the raw config, so the free-space step weighs the same numbers
// admitCapacity weighs before a real transfer. A check that used different
// numbers from the guard would be a check that passes for a destination
// the next transfer refuses.
func (s *Service) PreflightLocalMedium(ctx context.Context) (mediumcheck.Report, error) {
	if s.Config == nil {
		return mediumcheck.Report{}, fmt.Errorf("app: preflight: this instance has no configuration to read a backup root out of")
	}
	return mediumcheck.RunLocal(ctx, func(step mediumcheck.Step, err error) {
		// The one place the underlying cause is allowed to go, exactly as
		// PreflightMedium's Observe is. An os error names a path on this
		// machine, and the report is rendered in a browser and exported
		// from a terminal; the operator's log is where their diagnostics
		// already live.
		s.logger().Error(ctx, "medium-preflight", fmt.Errorf("local storage destination, %s check: %w", step, err))
	}, mediumcheck.LocalTarget{
		Root:              s.Config.EffectiveBackupRoot(),
		ID:                config.MediumLocal,
		OtherRoots:        s.otherLocalDestinations(config.MediumLocal),
		SafetyMarginBytes: s.Capacity.SafetyMarginBytes,
		CriticalFreeBytes: s.Capacity.CriticalFreeBytes,
	})
}

// PreflightLocalVolumeMedium proves one declared local_volume destination
// works, through the identical local check the implicit backup root uses
// (issue #666): a directory fails in exactly the ways RunLocal already
// answers - gone, unwritable, full, or secretly the SAME disk as another
// configured local destination reached by a different path. It is
// dispatched by ROLE (core/service's PreflightStorageMedium chooses this
// over the generic PreflightMedium/mediumcheck.Run for any medium whose
// Type is StorageMediumTypeLocalVolume), not by id, because "local",
// "second_disk" and "archive_disk" are all local_volume instances and all
// prove the same four things the same way.
func (s *Service) PreflightLocalVolumeMedium(ctx context.Context, m config.StorageMedium) (mediumcheck.Report, error) {
	if s.Config == nil {
		return mediumcheck.Report{}, fmt.Errorf("app: preflight: this instance has no configuration to read local destinations out of")
	}
	return mediumcheck.RunLocal(ctx, func(step mediumcheck.Step, err error) {
		s.logger().Error(ctx, "medium-preflight", fmt.Errorf("local volume %q, %s check: %w", m.ID, step, err))
	}, mediumcheck.LocalTarget{
		Root:              m.Path,
		ID:                m.ID,
		OtherRoots:        s.otherLocalDestinations(m.ID),
		SafetyMarginBytes: s.Capacity.SafetyMarginBytes,
		CriticalFreeBytes: s.Capacity.CriticalFreeBytes,
	})
}

// otherLocalDestinations is every local destination this deployment
// already writes backups into, other than excludeID: the implicit backup
// root under the reserved local id, and every other declared
// local_volume instance under its own. It is what mediumcheck's
// StepDistinctVolume weighs a candidate directory against, so a second
// "destination" that turns out to be the same disk under a different
// path is told apart from a genuinely separate one (#666).
func (s *Service) otherLocalDestinations(excludeID string) map[string]string {
	others := map[string]string{}
	if root := s.Config.EffectiveBackupRoot(); root != "" && config.MediumLocal != excludeID {
		others[config.MediumLocal] = root
	}
	for _, m := range s.Config.StorageMediums {
		if m.Type != config.StorageMediumTypeLocalVolume || m.ID == excludeID {
			continue
		}
		others[m.ID] = m.Path
	}
	return others
}
