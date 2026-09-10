package service

import (
	"fmt"
	"sort"

	"github.com/spdrman/rclone-manager/core/internal/backend"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// This file is EPIC I's (#664) read-only half of the backend registry: the
// catalogue GET /api/v1/backends serves, and the naming rules an instance
// id follows, projected into shapes the API layer is allowed to see.
//
// # Why a projection exists at all
//
// The registry lives in core/internal/backend. apps/common/webhost is a
// different module and may not import core/internal at all — that rule is
// stated in its own package doc ("BackupServiceClient seam (backend.go),
// never core/internal", webhost/doc.go:37) and enforced by the module
// boundary itself. So the handler cannot name backend.Manifest, and the
// choice is not "one copy or two", it is "a projection here or a second
// hand-written manifest format in TypeScript", which is exactly what
// plan-665-registry.md §7.4 rules out ("#668 should add the endpoint, not
// a hand-written TypeScript copy of the field list").
//
// The projection is deliberately field-for-field and total: every field a
// manifest declares appears here, and nothing is summarised away. A
// projection that dropped a field would be a manifest format with two
// spellings, one of which quietly means less.
//
// # Why this is a package-level function and not a BackupServiceClient method
//
// RegisteredValidators (validator.go) is the precedent and the argument is
// its handler's own (handlers_validators.go:52): the catalogue is fixed at
// build time, so there is nothing per-deployment to ask a client for, and
// widening that interface to forward a constant makes the seam wider for
// nothing. backend.Bundled() is memoised for the life of the process and
// is equally build-time-fixed.
//
// # No write counterpart, for Validator's reason one step further on
//
// A route that let a client add a catalogue entry would be an
// arbitrary-backend surface. A manifest is the one document in this
// product that decides what a destination may BE, including which rclone
// backend it dials; accepting one over HTTP would put FR-4's gate on the
// far side of the network from the binary it is supposed to constrain.

// BackendEnumValue is one choice an enum-kind field offers.
type BackendEnumValue struct {
	Value string
	Label string
}

// BackendField is one thing an operator is asked for when they configure
// an instance of a backend.
//
// It carries SHAPE and never a value, which is what makes the whole
// catalogue safe to serve: a credential-kind field says a credential is
// needed here and nothing more, and the material itself has no spelling on
// this boundary at all (see MediumCredentialRef in mediumcredentials.go —
// "there is no read side").
type BackendField struct {
	ID         string
	Label      string
	Help       string
	Kind       string
	Required   bool
	Values     []BackendEnumValue
	Pattern    string
	UnsetMeans string
}

// BackendProbeStep is one step of the verification vocabulary and whether
// this backend runs it, with the sentence explaining a skip.
type BackendProbeStep struct {
	Step   string
	Run    bool
	Reason string
}

// Backend is one registered backend, projected out of its manifest.
type Backend struct {
	ID      string
	Label   string
	Summary string
	Role    string

	// RcloneBackend is the transport the manifest declares, and it stops
	// here: apps/common/webhost does not put it on /api/v1, because #81's
	// standing constraint forbids naming an implementation in the public
	// schema. It stays on this type because the catalogue's Unregistered
	// set is computed as a subtraction against it, and a caller checking
	// that the two sides of that subtraction are disjoint has to be able
	// to read both.
	RcloneBackend string

	Fields []BackendField
	Probe  []BackendProbeStep
}

// BackendCatalog is every backend an instance may be declared on, plus the
// rules an instance id follows.
//
// The rules travel WITH the catalogue rather than in a separate settings
// read, for RetentionSchema's stated reason: the add-a-destination form has
// to refuse exactly what a hand-edited configuration file would be refused
// for, and a client that holds its own copy of the pattern is a client
// whose copy goes stale in one direction only — silently accepting a name
// the engine then rejects.
type BackendCatalog struct {
	Backends []Backend

	// Unregistered are backends this build's transport layer asks for that
	// no manifest declares, so no instance of one can exist.
	//
	// Reported rather than omitted. Hiding them answers an operator worse:
	// somebody who came looking for SFTP learns nothing from a menu that
	// never mentions it and asks again next month, whereas a row saying
	// the shape is understood and is not registered is a real answer.
	// Nothing can be submitted against one — Registry.Backend refuses an
	// id no manifest declares — so this is disclosure with no surface
	// attached.
	Unregistered []string

	// InstanceIDPattern and ReservedInstanceID are the two naming rules,
	// taken from the engine's own constants rather than restated here.
	InstanceIDPattern  string
	ReservedInstanceID string
}

// RegisteredBackends returns the catalogue built from the manifests this
// build ships.
//
// It returns an error for the one reason Bundled() can fail: a manifest
// this build embeds is malformed, which is a build fault rather than
// anything an operator configured. The whole registry is refused in that
// case rather than the well-formed manifests being served with the bad one
// omitted, which is backend.Load's own decision and is preserved here
// rather than softened at the boundary.
func RegisteredBackends() (BackendCatalog, error) {
	reg, err := backend.Bundled()
	if err != nil {
		return BackendCatalog{}, fmt.Errorf("service: backend registry: %w", err)
	}

	ids := reg.IDs()
	out := BackendCatalog{
		Backends:           make([]Backend, 0, len(ids)),
		InstanceIDPattern:  config.StorageMediumIDPattern,
		ReservedInstanceID: backend.ReservedInstanceID,
	}

	// claimed is which rclone backends a manifest speaks for, so the
	// unregistered set below is a subtraction rather than a second list
	// somebody has to remember to update when a manifest is added.
	claimed := make(map[string]bool, len(ids))
	for _, id := range ids {
		m, err := reg.Backend(id)
		if err != nil {
			// Unreachable: id came from the same registry. Reported
			// rather than ignored, because the alternative is a
			// catalogue that is quietly shorter than the registry.
			return BackendCatalog{}, fmt.Errorf("service: backend %q: %w", id, err)
		}
		claimed[m.RcloneBackend] = true
		out.Backends = append(out.Backends, projectManifest(m))
	}

	for _, name := range rclone.RequiredBackends {
		if !claimed[name] {
			out.Unregistered = append(out.Unregistered, name)
		}
	}
	sort.Strings(out.Unregistered)

	return out, nil
}

// projectManifest is the field-for-field copy. It is written out longhand
// rather than by reflection so that a field added to the manifest format
// fails to compile here, which is the only way a projection stays total.
func projectManifest(m backend.Manifest) Backend {
	b := Backend{
		ID:            m.ID,
		Label:         m.Label,
		Summary:       m.Summary,
		Role:          string(m.Role),
		RcloneBackend: m.RcloneBackend,
		Fields:        make([]BackendField, 0, len(m.Fields)),
		Probe:         make([]BackendProbeStep, 0, len(m.Probe.Steps)),
	}
	for _, f := range m.Fields {
		field := BackendField{
			ID:         f.ID,
			Label:      f.Label,
			Help:       f.Help,
			Kind:       string(f.Kind),
			Required:   f.Required,
			Pattern:    f.Pattern,
			UnsetMeans: f.UnsetMeans,
		}
		if len(f.Values) > 0 {
			field.Values = make([]BackendEnumValue, 0, len(f.Values))
			for _, v := range f.Values {
				field.Values = append(field.Values, BackendEnumValue{Value: v.Value, Label: v.Label})
			}
		}
		b.Fields = append(b.Fields, field)
	}
	for _, s := range m.Probe.Steps {
		b.Probe = append(b.Probe, BackendProbeStep{Step: s.Step, Run: s.Run, Reason: s.Reason})
	}
	return b
}
