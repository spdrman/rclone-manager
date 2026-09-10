package service

// Configuring a declared destination in its backend's own vocabulary
// (I2.2, issue #669, EPIC I #664).
//
// # Why this is not UpdateStorageMedium
//
// StorageMediumSpec is config.StorageMedium's fields, which is to say
// S3's: Bucket is required and there is no Path. That was right when S3
// was the only destination anybody could declare, and #667 is deleting
// the assumption from the engine, but the SHAPE a caller describes a
// destination with is still a hand-transcribed copy of one backend's
// field list. A local volume cannot be described in it at all, and a
// local volume is the only destination a fresh install has (#670 seeds
// `local` and nothing else).
//
// So these three operations speak in manifest field ids. What a caller
// sends is the vocabulary the registry declares for THAT backend, and
// this file is the one place it is folded onto the named struct fields
// config keeps for on-disk compatibility. They are additive: nothing
// here replaces the spec path, which #594's wizard and `medium add` are
// still on, so retiring that is a later, separate decision.
//
// # Everything that makes a write safe is reused, not re-implemented
//
// ConfigureStorageMedium ends in writeStorageMedium, which is the same
// re-read / fold / encode / validate / write / hot-reload sequence a
// create and an edit go through, and therefore also #636's check in
// front of the write and #636's mark when it is skipped. A second write
// path with its own idea of any of that is how a destination becomes
// creatable in a shape it cannot be edited into, which is the argument
// writeStorageMedium's own doc makes for being one function.
//
// # A field with no home refuses, loudly, naming itself
//
// storageMediumFromFields refuses a declared field it cannot place
// rather than dropping it. Dropping would produce the defect
// StorageMediumSpec's own docblock says that struct exists to prevent: a
// destination that verifies green and then saves as something slightly
// different. The refusal is what catches the next field somebody adds to
// a manifest and forgets to plumb, and it costs nothing on every request
// that has nothing wrong with it.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/backend"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// StorageMediumConfiguration is one declared instance's manifest field
// values, plus where its credential comes from.
//
// Fields is keyed by manifest field id and carries what the engine
// validates: a KindBool field is "true" or "false", and an unset
// optional field is ABSENT rather than present as an empty string or as
// its UnsetMeans value. That last rule is #294's - a default resolved by
// an accessor at read time must not travel as though somebody chose it,
// because the next save freezes it into the operator's file - and it is
// why this struct has no defaulting anywhere.
//
// Credentials is separate, and the separation is load-bearing rather
// than tidy: a credential is not a value. It is checked by a different
// rule, it is resolved by resolveSpecCredentials, and a value map that
// could hold one is a value map something eventually puts material
// into.
type StorageMediumConfiguration struct {
	Fields      map[string]string
	Credentials StorageMediumCredentials
}

// StorageMediumConfigurationState is what one instance has configured
// right now, in its backend's vocabulary.
//
// A form has to have this before it can offer an edit, because these
// operations replace the WHOLE declared field set: a form that started
// empty and saved would unset every field the operator did not retype.
//
// CredentialConfigured is a boolean and never the reference. Whether a
// credential exists is what a form needs - it decides whether the pair
// may be left empty - and it is not material, not a path and not a
// variable name, which is the most that may be said either way (FR-33,
// and #665's C3).
type StorageMediumConfigurationState struct {
	Fields               map[string]string
	CredentialConfigured bool
}

// StorageMediumConfigurationOf reports one declared instance's
// configuration in its backend's vocabulary.
func (b *BackupService) StorageMediumConfigurationOf(
	_ context.Context,
	id string,
) (StorageMediumConfigurationState, error) {
	medium, manifest, err := b.declaredInstance(id)
	if err != nil {
		return StorageMediumConfigurationState{}, err
	}

	fields := map[string]string{}
	for _, field := range manifest.Fields {
		if field.Kind == backend.KindCredential {
			continue
		}
		// Absent, not empty. An unset optional field is what UnsetMeans
		// resolves at read time, and reporting "" for it would put a
		// value where nobody said one.
		if value := mediumFieldValue(medium, field.ID); value != "" {
			fields[field.ID] = value
		}
	}

	return StorageMediumConfigurationState{
		Fields:               fields,
		CredentialConfigured: credentialsConfigured(medium.Credentials),
	}, nil
}

// PreflightStorageMediumConfiguration proves a configuration that has
// not been written, and writes nothing whatever the report says.
//
// It differs from PreflightStorageMediumCandidate in the one way that
// matters here: a candidate can inherit nothing, because it is not
// declared, whereas this instance IS declared and may already have a
// credential the caller cannot read back. So a configuration naming no
// credential is checked with the one already stored, which is what makes
// "change the prefix on a destination whose access key you do not have"
// a thing an operator can do and prove.
//
// It resolves rather than fails when the destination does not work: a
// bucket that is not there is what an operator configured, not a request
// that broke.
func (b *BackupService) PreflightStorageMediumConfiguration(
	ctx context.Context,
	id string,
	cfg StorageMediumConfiguration,
) (MediumPreflight, error) {
	spec, inherit, err := b.specFromConfiguration(id, cfg)
	if err != nil {
		return MediumPreflight{}, err
	}
	candidate, err := b.mediumFromSpec(spec, inherit)
	if err != nil {
		return MediumPreflight{}, err
	}
	report, err := b.state.Load().inner.PreflightMediumCandidate(ctx, candidate)
	if err != nil {
		return MediumPreflight{}, fmt.Errorf("service: preflighting configuration of storage medium %s: %w", id, err)
	}
	return toMediumPreflight(report), nil
}

// ConfigureStorageMedium writes one declared instance's configuration.
//
// It ends in writeStorageMedium, so it inherits every property an edit
// has, including the one that matters most: the engine runs the same
// check in front of the write and refuses with
// ErrStorageMediumNotProven when it fails (#636). A caller's own check
// passing is not what makes this safe - a bucket policy can change
// between the two - and this being the same write path is what makes the
// two agree.
func (b *BackupService) ConfigureStorageMedium(
	ctx context.Context,
	id string,
	cfg StorageMediumConfiguration,
) (StorageMediumSummary, error) {
	spec, _, err := b.specFromConfiguration(id, cfg)
	if err != nil {
		return StorageMediumSummary{}, err
	}
	// mustExist, and the credential inheritance writeStorageMedium
	// derives for itself from a spec naming nothing: the same rule an
	// edit already has, rather than a second statement of it here.
	return b.writeStorageMedium(ctx, spec, true)
}

// specFromConfiguration turns a manifest-shaped configuration into the
// spec the existing write path takes, validating it against the
// manifest first. The second return is whether the credential is
// inherited, which the probe needs and the write derives itself.
func (b *BackupService) specFromConfiguration(
	id string,
	cfg StorageMediumConfiguration,
) (StorageMediumSpec, bool, error) {
	medium, manifest, err := b.declaredInstance(id)
	if err != nil {
		return StorageMediumSpec{}, false, err
	}

	// Only the minted-id spelling, or none. The other three
	// (config.MediumCredentials' file, env and command) stay available
	// through UpdateStorageMedium and `medium edit`, and they are
	// refused here rather than quietly accepted because a manifest's
	// KindCredential field is a reference this deployment minted - see
	// backend.validateFieldValue's credential case, which refuses a
	// value carrying a path separator. Accepting a host path on this
	// route would mean the manifest's own rule for the field it declares
	// does not apply on the route named after it.
	if cfg.Credentials.File != "" || cfg.Credentials.Env != "" || len(cfg.Credentials.Command) > 0 {
		return StorageMediumSpec{}, false, fmt.Errorf(
			"%w: this route takes a credential this manager minted, or none at all to keep the one already configured; a credential held in a file, an environment variable or a command is configured through the storage-medium edit route",
			ErrInvalidRequest)
	}
	inherit := cfg.Credentials.namesNothing()
	if inherit && !credentialsConfigured(medium.Credentials) {
		if field, ok := manifest.CredentialField(); ok && field.Required {
			return StorageMediumSpec{}, false, fmt.Errorf(
				"%w: the %s backend requires %s and this destination has none configured",
				ErrInvalidRequest, manifest.ID, field.ID)
		}
	}

	if problems := b.validateFields(manifest, id, cfg, inherit); len(problems) > 0 {
		return StorageMediumSpec{}, false, fmt.Errorf("%w: %s", ErrInvalidRequest, strings.Join(problems, "; "))
	}

	spec, err := storageMediumFromFields(medium, manifest, cfg)
	if err != nil {
		return StorageMediumSpec{}, false, err
	}
	return spec, inherit, nil
}

// validateFields is the registry's own instance validation, run before
// anything is folded onto a struct, and it returns every problem rather
// than the first: a form showing one error at a time makes an operator
// submit four times to learn four things.
//
// The credential field is handed a SHAPE token rather than the stored
// reference, and that is deliberate. backend.ValidateInstance's rule for
// a KindCredential field is that the value is a reference with no path
// separator; the reference actually written to config.yaml is a path
// (the 0600 file ImportStorageCredentials wrote), so passing it would
// fail a rule it was never about. What this boundary knows at this
// moment is that a reference exists and that the caller did not supply a
// path, and the token says exactly that much.
func (b *BackupService) validateFields(
	manifest backend.Manifest,
	id string,
	cfg StorageMediumConfiguration,
	inherit bool,
) []string {
	values := make(map[string]string, len(cfg.Fields)+1)
	for field, value := range cfg.Fields {
		values[field] = value
	}
	if field, ok := manifest.CredentialField(); ok {
		switch {
		case cfg.Credentials.ID != "":
			values[field.ID] = cfg.Credentials.ID
		case inherit:
			values[field.ID] = "configured"
		}
	}

	registry, err := backend.Bundled()
	if err != nil {
		return []string{err.Error()}
	}
	errs := registry.ValidateInstance(manifest.ID, "storage_mediums["+id+"]", values)
	problems := make([]string, 0, len(errs))
	for _, err := range errs {
		problems = append(problems, err.Error())
	}
	sort.Strings(problems)
	return problems
}

// storageMediumFromFields folds a manifest's field values onto the
// existing instance and returns the spec the write path takes.
//
// The switch is on FIELD ID and never on which backend is being
// configured, which is the difference between a translation and a
// special case. config.StorageMedium keeps named typed fields because
// #664's compatibility pin requires the on-disk schema to stay
// byte-for-byte what it was, so this translation has to exist somewhere;
// what matters is that `bucket` and `path` are both just field ids here,
// and that a manifest declaring one and not the other needs no code.
//
// A field with nowhere to go is refused, naming itself. See this file's
// header for why that is worth the ugliness.
func storageMediumFromFields(
	medium config.StorageMedium,
	manifest backend.Manifest,
	cfg StorageMediumConfiguration,
) (StorageMediumSpec, error) {
	// Every declared field is replaced, including the ones the caller
	// left out, because this is a whole-record replace: an absent field
	// means unset, not "leave what is there". A patch would need a
	// second spelling for "clear this", which is the shape
	// UpdateStorageMedium's doc rejects for the same reason.
	spec := StorageMediumSpec{
		ID:          medium.ID,
		Type:        medium.Type,
		Credentials: cfg.Credentials,
	}

	for _, field := range manifest.Fields {
		if field.Kind == backend.KindCredential {
			continue
		}
		value := cfg.Fields[field.ID]
		switch field.ID {
		case "region":
			spec.Region = value
		case "endpoint":
			spec.Endpoint = value
		case "bucket":
			spec.Bucket = value
		case "prefix":
			spec.Prefix = value
		case "storage_class":
			spec.StorageClass = value
		case "upload_verification":
			spec.UploadVerification = value
		default:
			return StorageMediumSpec{}, fmt.Errorf(
				"%w: the %s backend declares %s and this manager cannot yet store it; nothing was written",
				ErrInvalidRequest, manifest.ID, field.ID)
		}
	}
	return spec, nil
}

// declaredInstance reads one declared instance and the manifest for its
// backend.
//
// config.StorageMedium.Type IS the backend id - "s3" resolves straight
// to the registry's "s3" - so there is no mapping here and deliberately
// no wrapper pretending there is one. The lookup is the registry's
// refusal when a configuration names a backend this build does not
// register, which is the FR-4 floor rather than something to re-check.
func (b *BackupService) declaredInstance(id string) (config.StorageMedium, backend.Manifest, error) {
	cfg := b.state.Load().inner.Config
	for _, m := range cfg.StorageMediums {
		if m.ID != id {
			continue
		}
		registry, err := backend.Bundled()
		if err != nil {
			return config.StorageMedium{}, backend.Manifest{}, err
		}
		manifest, err := registry.Backend(m.Type)
		if err != nil {
			return config.StorageMedium{}, backend.Manifest{}, fmt.Errorf("service: storage medium %s: %w", id, err)
		}
		return m, manifest, nil
	}
	return config.StorageMedium{}, backend.Manifest{}, fmt.Errorf("%w: %s", ErrMediumNotFound, id)
}

// mediumFieldValue and the switch in storageMediumFromFields are the two
// directions of ONE table, and they are in one file on purpose.
//
// config.StorageMedium keeps named typed fields because #664's
// compatibility pin requires the on-disk schema to stay byte-for-byte
// what it was, so a translation between a manifest's vocabulary and
// those names has to exist. What must not happen is the two directions
// drifting: a read that reports `prefix` and a write that stores it
// somewhere else would make a form show one destination and save
// another. Keeping them adjacent is the cheapest way to make a missing
// case in one visible against the other.
//
// #667 is landing config.storageMediumFieldValue for the read direction,
// unexported, feeding backend.ValidateInstance. When that lands
// exported, this half collapses onto it and the write half stays here;
// that consolidation is a merge job rather than something either branch
// can do alone.
func mediumFieldValue(medium config.StorageMedium, fieldID string) string {
	switch fieldID {
	case "region":
		return medium.Region
	case "endpoint":
		return medium.Endpoint
	case "bucket":
		return medium.Bucket
	case "prefix":
		return medium.Prefix
	case "storage_class":
		return medium.StorageClass
	case "upload_verification":
		return medium.UploadVerification
	default:
		// A field this manager cannot store yet reads as unset rather
		// than as an error, and only here: a READ that refused would
		// make a destination unreadable because of a field nobody has
		// set, while the WRITE refuses because saving one silently would
		// lose what the operator typed. The asymmetry is deliberate.
		return ""
	}
}

// credentialsConfigured reports whether a medium names a credential
// source at all. It says nothing about which, and nothing about what is
// behind it: that is the whole of what may cross this boundary (FR-33).
func credentialsConfigured(c config.MediumCredentials) bool {
	return c.File != "" || c.Env != "" || len(c.Command) > 0
}
