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
// It takes an id and never a candidate, which is the opposite of a source
// test-connection. A source is proven before it is written down, because the
// wizard's whole job is to check a host somebody just typed. A medium cannot
// work that way: the only fields that would make a candidate meaningful are
// the three credential references, and putting those on a request body turns
// a path on this host, or the name of an environment variable, into something
// an API caller sends. FR-33 settles it the other way, so a medium is declared
// in the configuration file and nowhere else, and this reads the reference
// back out of it.
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
