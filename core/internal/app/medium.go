package app

import (
	"errors"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Naming a configured medium for a caller that only wants to reach it.
//
// This package has two functions that turn a config.StorageMedium into a
// transport.Medium, and the split is deliberate rather than a duplication
// somebody missed. MediumResolver.Resolve (mediums.go) is the move engine's
// seam: it also answers what a copy must ACHIEVE there before a source may
// be deleted, it refuses config.MediumLocal by name, and it refuses a
// declared type this build has no backend for, because everything asking it
// is about to do something irreversible on the strength of the answer.
//
// MediumFor is for the callers that only need somewhere to address: a
// restore request, and the conformance suite's own reader. They pass the
// value straight to the store, so the store's own refusal is the check, and
// adding a class they would ignore would mean deciding FR-31's mapping in a
// second place. The cost of the split is that MediumFor is the more
// permissive of the two, and a caller that grows a destructive step wants
// Resolve instead.
//
// What both share is the property transport.Medium's own tests pin: the
// value carries a reference to a credential and never a credential, so it is
// safe to log and safe to put in an error. That is what makes
// ErrMediumNotDeclared able to name the medium it refused.

// ErrMediumNotDeclared is what MediumFor returns for a medium id this
// configuration does not declare.
//
// It is its own sentinel because it is a state an operator reaches
// normally rather than a bug: a placement row outlives the configuration
// that created it, so an artifact can name a medium that was removed from
// config.yaml last week. Every surface that acts on a copy has to be able
// to say "that medium is not configured any more" rather than "internal
// error", and the copy itself may well still be there.
var ErrMediumNotDeclared = errors.New("app: this configuration does not declare that storage medium")

// MediumFor builds the transport-boundary descriptor for one configured
// storage medium.
//
// This is the one direction config.StorageMedium is translated in, and it
// lives here for the reason transport.Medium's own doc gives: internal/app
// builds one from a config.StorageMedium, and nothing below the transport
// boundary reads config. Two translations of this struct would be two
// answers to "which bucket does this medium write to", and the wrong one
// would be found by an operator, not by a reviewer.
//
// It carries no credential, only the reference to where one comes from,
// which is the property transport.Medium's own tests pin. So the value it
// returns is safe to log, and safe to put in an error.
func MediumFor(cfg *config.Config, id string) (transport.Medium, string, error) {
	if cfg == nil {
		return transport.Medium{}, "", fmt.Errorf("%w: %q", ErrMediumNotDeclared, id)
	}
	for _, m := range cfg.StorageMediums {
		if m.ID != id {
			continue
		}
		return transport.Medium{
			ID:           m.ID,
			Type:         transport.MediumType(m.Type),
			Region:       m.Region,
			Endpoint:     m.Endpoint,
			Bucket:       m.Bucket,
			Prefix:       m.Prefix,
			StorageClass: m.EffectiveStorageClass(),
			Credentials: transport.MediumCredentials{
				File:    m.Credentials.File,
				Env:     m.Credentials.Env,
				Command: append([]string(nil), m.Credentials.Command...),
			},
		}, m.EffectiveStorageClass(), nil
	}
	return transport.Medium{}, "", fmt.Errorf("%w: %q", ErrMediumNotDeclared, id)
}
