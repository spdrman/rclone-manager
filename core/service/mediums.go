package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// Declaring a storage destination, and proving one before it is declared
// (G2.2, issue #594).
//
// # The gap this closes
//
// UpdateSettingsRequest carries Retention and Capacity and nothing else,
// and the only storage-medium route this API had was a preflight of
// something already saved. So a destination was read-only on every
// surface this product has, and the one thing EPIC G is about, setting up
// without already knowing YAML, was impossible for the destination half.
// This file is the write side, at deployment scope, through the identical
// re-read / fold / encode / validate / write / hot-reload sequence
// settings.go documents and for the identical reasons.
//
// # Verify before save, and how the old objection is answered
//
// core/internal/app/mediumpreflight.go used to argue that a preflight
// could only ever take an id, because "the only fields that would make a
// candidate meaningful are the three credential references, and putting
// those on a request body turns a path on this host, or the name of an
// environment variable, into something an API caller sends".
//
// That argument is answered on its own terms rather than overruled, and
// the answer is a FOURTH spelling of a reference:
// StorageMediumCredentials.ID, minted by ImportStorageCredentials, opaque,
// generated on this host, resolving server-side to a 0600 file this
// manager wrote beside config.yaml. A candidate carrying one is
// meaningful without a host path or a variable name ever appearing on a
// request body, which is exactly the thing the old paragraph said could
// not exist. It is the same shape POST /ssh-keys plus
// CreateBackupSetRequest.SSHKeyID has had since #146.
//
// The other three spellings are still accepted here, and that is a
// deliberate, narrower decision than it looks. They now reach this
// deployment through CreateStorageMedium anyway, because a create route
// is this issue's whole point and because the wizard's step 2 offers all
// four (docs/design/s3-destination-wizard.html). Refusing them on the
// PROBE alone would not keep a path off a request body; it would only
// guarantee that the sole way to check a file-backed medium is to write
// it into the operator's configuration first, which is the ordering FR-30
// exists to prevent. What a probe discloses about a path it was handed is
// bounded by mediumcheck's FR-33 rule: an outcome and a category, never
// the underlying error's text, which is exactly as much as the same
// caller learns by creating the medium and preflighting the saved id.
//
// # What never crosses this boundary
//
// Credential MATERIAL, in either direction, on any of these calls. It
// arrives once, on ImportStorageCredentials (mediumcredentials.go), and
// what is stored, echoed, listed or written to config.yaml afterwards is
// a reference. StorageMediumSummary has no field for a secret and
// StorageMediumSpec has none either: the absence is structural, the same
// way config.MediumCredentials' is.

// StorageMediumCredentials names where one medium's credentials come
// from, in the four spellings this boundary accepts. Exactly one must be
// set.
//
// ID is this package's own; the other three are
// config.MediumCredentials' three, field for field. They are separate
// fields rather than a kind-plus-value pair because a kind-plus-value
// pair is a shape in which "file" plus a variable name is representable,
// and the whole point of the closed set is that it is not.
type StorageMediumCredentials struct {
	// ID is a MediumCredentialRef.ID an earlier ImportStorageCredentials
	// call returned. It resolves, server-side, to the 0600 file that
	// import wrote, and what lands in config.yaml is that file's path
	// under `credentials.file`.
	ID string

	// File, Env and Command are config.MediumCredentials' three, for an
	// operator who already has a credential on this host or in a secrets
	// manager. `file` stays the preferred one, here as there: rclone
	// opens it itself, so the secret never enters this process at all.
	File    string
	Env     string
	Command []string
}

// namesNothing reports a credential reference that names no source at
// all. It is a method rather than four comparisons at the call site for
// the reason RetentionUpdate.namesNothing is one: the check decides
// whether a write is partial, and one spelling of it is one chance to
// spell it wrong.
func (c StorageMediumCredentials) namesNothing() bool {
	return c.ID == "" && c.File == "" && c.Env == "" && len(c.Command) == 0
}

// StorageMediumSpec is one storage medium as a caller describes it,
// whether to create it, to edit it, or to have it checked before any of
// that.
//
// It is config.StorageMedium's fields with Credentials widened by the id
// spelling, and it is deliberately the SAME struct for all three verbs.
// A create that took one shape and a candidate-preflight that took
// another would be two descriptions of one destination, and the interesting
// bug in this feature is a medium that verifies green and then saves as
// something slightly different.
type StorageMediumSpec struct {
	// ID is the medium id a retention tier names: lower_snake_case, and
	// not "local", which is reserved. config.Validate is what enforces
	// that, over the whole configuration, so nothing here second-guesses
	// it.
	ID string

	// Type names the backend. "s3" is the only value; the set is closed
	// in config and this passes it through so the refusal for anything
	// else is config's own.
	Type string

	// Region, Endpoint, Bucket, Prefix, StorageClass and
	// UploadVerification are config.StorageMedium's, unchanged and
	// unexamined here. In particular the region and the endpoint are
	// handed on as typed: StorageMedium.Region's own doc argues that a
	// list of legal regions in this product would be a second, staler
	// copy that refuses a region that works, and a wizard's provider
	// preset only fills the endpoint field in.
	Region             string
	Endpoint           string
	Bucket             string
	Prefix             string
	StorageClass       string
	UploadVerification string

	// Credentials is where this medium's credentials come from.
	Credentials StorageMediumCredentials
}

// ErrStorageMediumInUse is the removal refusal FR-30 requires: a
// destination artifacts already reference cannot be un-declared from a
// settings page.
//
// Its own sentinel, like ErrMediumDisclosureRequired, because it is not a
// malformed request. The caller asked something this deployment
// understood perfectly and will not do, and the surface above needs to
// tell those two apart to choose between "fix your request" and "here is
// what is on it".
var ErrStorageMediumInUse = errors.New("service: storage medium still holds copies")

// ErrStorageMediumExists is a create over an id the configuration already
// declares. config.Validate would refuse the duplicate too, but it would
// refuse it as a validation error about a list, and an operator typing a
// name into a form needs "that name is taken".
var ErrStorageMediumExists = errors.New("service: storage medium already declared")

// StorageMediumUsage is what one medium currently holds, as this boundary
// reports it: the FR-30 answer a surface renders beside a failed
// re-verification and the reason a removal is refused.
//
// See core/internal/app.MediumUsage for what is counted and why the
// journal is the only thing consulted.
type StorageMediumUsage struct {
	// Medium is the medium id this is about.
	Medium string

	// Placements is how many copies name it and are not GONE.
	Placements int

	// BackupSets is one entry per backup set with copies here, sorted by
	// id. It is a LIST rather than only a count because "148 copies
	// affected" with nothing to act on is not a report, it is a number.
	BackupSets []StorageMediumUsageBySet
}

// StorageMediumUsageBySet is one backup set's share of a medium.
type StorageMediumUsageBySet struct {
	Set        string
	Placements int

	// OnlyCopyHere is how many of those copies are the artifact's only
	// one anywhere. It is the population FR-30's invariant is about, and
	// it is reported separately because it calls for a different
	// sentence.
	OnlyCopyHere int
}

// ListStorageMediums reports every declared destination, in declaration
// order, exactly as Settings does.
//
// It exists beside Settings.Mediums rather than instead of it because the
// two are asked by different screens for different reasons: a settings
// page reads the whole policy at once, and a destinations page (and
// `backup-manager medium list`) asks only this. Both project through
// toStorageMediumSummaries, so they cannot drift.
func (b *BackupService) ListStorageMediums(_ context.Context) ([]StorageMediumSummary, error) {
	return toStorageMediumSummaries(b.state.Load().inner.Config), nil
}

// GetStorageMedium reports one declared destination.
func (b *BackupService) GetStorageMedium(_ context.Context, id string) (StorageMediumSummary, error) {
	for _, m := range toStorageMediumSummaries(b.state.Load().inner.Config) {
		if m.ID == id {
			return m, nil
		}
	}
	return StorageMediumSummary{}, fmt.Errorf("%w: %s", ErrMediumNotFound, id)
}

// StorageMediumUsage reports what the journal says is on one medium.
//
// It answers for an id the configuration no longer declares too, and that
// is not an oversight: a removed medium is precisely the case where an
// operator needs to know what was left on it, and refusing the question
// because the declaration is gone would answer the wrong one.
func (b *BackupService) StorageMediumUsage(ctx context.Context, id string) (StorageMediumUsage, error) {
	usage, err := b.state.Load().inner.MediumUsageOf(ctx, id)
	if err != nil {
		return StorageMediumUsage{}, err
	}
	out := StorageMediumUsage{
		Medium:     usage.Medium,
		Placements: usage.Placements,
		BackupSets: make([]StorageMediumUsageBySet, 0, len(usage.BackupSets)),
	}
	for _, s := range usage.BackupSets {
		out.BackupSets = append(out.BackupSets, StorageMediumUsageBySet{
			Set: s.Set, Placements: s.Placements, OnlyCopyHere: s.OnlyCopyHere,
		})
	}
	return out, nil
}

// PreflightStorageMediumCandidate proves a destination that has not been
// saved, through the identical mediumcheck every declared one goes
// through, and writes nothing whatever the report says.
//
// This is the wizard's step 3 and `medium preflight --candidate`'s whole
// body. The only thing it resolves is the credential reference, and it
// resolves it exactly as CreateStorageMedium will, so a candidate that
// passes here is a candidate the save cannot turn into something else.
func (b *BackupService) PreflightStorageMediumCandidate(ctx context.Context, spec StorageMediumSpec) (MediumPreflight, error) {
	// Never inherit here: a candidate is by definition not declared, so
	// there is nothing to inherit FROM, and a probe of a medium whose
	// credential came from somewhere the caller did not name would be a
	// probe of something other than what was asked about.
	candidate, err := b.mediumFromSpec(spec, false)
	if err != nil {
		return MediumPreflight{}, err
	}
	report, err := b.state.Load().inner.PreflightMediumCandidate(ctx, candidate)
	if err != nil {
		return MediumPreflight{}, fmt.Errorf("service: preflighting candidate storage medium %s: %w", spec.ID, err)
	}
	return toMediumPreflight(report), nil
}

// CreateStorageMedium declares a new destination in the configuration
// file this BackupService was opened from and hot-reloads this service so
// it is immediately selectable by a retention tier.
//
// Declaring a destination MOVES NOTHING on its own, and that is worth
// saying here as well as on screen: artifacts arrive on a medium only
// once a retention tier names it, which is a separate write with a
// disclosure of its own (ErrMediumDisclosureRequired). So this call needs
// no acknowledgment, and adding one would train an operator to click
// through the acknowledgment that matters.
//
// It does not verify. Verification is PreflightStorageMediumCandidate,
// called before this by the wizard and by `medium add` (which refuses to
// write when it fails), and keeping the two separate is what lets
// `--no-verify` exist for an operator building configuration offline
// against a bucket this host cannot reach. A create that silently ran a
// probe would also write a probe object into somebody's bucket from a
// call whose name says nothing about buckets.
func (b *BackupService) CreateStorageMedium(_ context.Context, spec StorageMediumSpec) (StorageMediumSummary, error) {
	return b.writeStorageMedium(spec, false)
}

// UpdateStorageMedium replaces one declared destination's description
// with spec, keyed on spec.ID.
//
// A whole-record replace rather than a field-by-field patch, and the
// reason is the same one RetentionUpdate.Tiers gives for replacing the
// whole chain: a medium's fields are not independent. An endpoint that
// changed without its region, or a credential reference swapped without
// the bucket it authorizes, is a destination that describes nowhere. The
// caller reads the medium, edits it, and submits the whole thing, which
// is what the wizard's edit mode does.
//
// Editing a medium artifacts already live on is ALLOWED, deliberately,
// and that is FR-30's own direction: a destination that stopped answering
// is one an operator needs to be able to fix, and refusing the edit would
// leave rotating an expired credential as a config-file job. It is
// removal that is refused while a copy names it, because removal is the
// one that leaves those copies unreachable.
//
// # The one field a whole-record replace does not replace
//
// A spec naming NO credential source keeps the medium's current one. That
// is the exception to the paragraph above, and it exists because the
// credential is the one part of a medium this API deliberately cannot
// read back: StorageMediumSummary has no field for it, not even for its
// kind, so a form cannot pre-fill it and a caller cannot resubmit what
// they never received. Without this exception, changing a storage class
// would require re-importing an access key, which is both absurd and the
// kind of friction that ends with operators editing YAML again.
//
// A spec that DOES name one replaces it, which is credential rotation and
// is the other thing an operator needs this verb for.
func (b *BackupService) UpdateStorageMedium(_ context.Context, spec StorageMediumSpec) (StorageMediumSummary, error) {
	return b.writeStorageMedium(spec, true)
}

// RemoveStorageMedium un-declares a destination, and refuses while any
// copy names it (FR-30).
//
// The refusal is not a nicety. Today a removed medium leaves
// ErrMediumNotDeclared behind for every surface that touches the copies
// on it and those copies read as unreachable, which is honest and
// survivable, and is not something an operator should be able to do by
// accident from a list. The refusal carries the count and the backup
// sets, so what comes back is something to act on rather than a "no".
//
// A tier still naming the medium is refused too, by config.Validate over
// the whole configuration, which is where that rule already lives.
func (b *BackupService) RemoveStorageMedium(ctx context.Context, id string) error {
	if b.configPath == "" {
		return ErrConfigNotFileBacked
	}
	if id == "" {
		return fmt.Errorf("%w: a storage medium id is required", ErrInvalidRequest)
	}

	// Asked BEFORE the file is re-read, encoded or written, so a refused
	// removal leaves the configuration exactly as it was. This is the
	// same "refuse, never partially apply" ordering settings.go holds.
	usage, err := b.StorageMediumUsage(ctx, id)
	if err != nil {
		return err
	}
	if usage.Placements > 0 {
		return storageMediumInUseRefusal(usage)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return fmt.Errorf("service: re-reading configuration: %w", err)
	}
	kept := make([]config.StorageMedium, 0, len(cfg.StorageMediums))
	found := false
	for _, m := range cfg.StorageMediums {
		if m.ID == id {
			found = true
			continue
		}
		kept = append(kept, m)
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrMediumNotFound, id)
	}
	// nil rather than an empty slice, so a deployment that removes its
	// last medium writes a file with no storage_mediums key at all rather
	// than one with an empty list. config.StorageMediums carries
	// omitempty for exactly this round trip (FR-35), and an empty list
	// left behind would be a key a medium-free deployment never had.
	if len(kept) == 0 {
		kept = nil
	}
	cfg.StorageMediums = kept

	return b.persistConfig(cfg)
}

// writeStorageMedium is create and edit's shared body: the one place a
// medium is folded onto the freshly re-read configuration.
//
// mustExist is the only difference between the two verbs, and it is a
// bool rather than two near-identical functions because everything else
// (resolution, the fold, the encode-before-validate, the write, the
// reload) has to be identical or a medium could be creatable in a shape
// it could not be edited into.
func (b *BackupService) writeStorageMedium(spec StorageMediumSpec, mustExist bool) (StorageMediumSummary, error) {
	if b.configPath == "" {
		return StorageMediumSummary{}, ErrConfigNotFileBacked
	}
	// An edit may name no credential source, and then it keeps the one
	// the medium already has (UpdateStorageMedium's own doc). A CREATE
	// may not: there is nothing to keep, and a medium with no credential
	// reference names no way to reach anything.
	inherit := mustExist && spec.Credentials.namesNothing()
	medium, err := b.mediumFromSpec(spec, inherit)
	if err != nil {
		return StorageMediumSummary{}, err
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	// Re-read from disk rather than from b.state, the same "always read
	// fresh" discipline CreateBackupSet and UpdateSettings both document:
	// the write below is based on the file's actual current content, so a
	// change made by hand, or by a second process, since this service
	// loaded survives a medium write that does not touch it.
	cfg, err := config.Load(b.configPath)
	if err != nil {
		return StorageMediumSummary{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	at := -1
	for i := range cfg.StorageMediums {
		if cfg.StorageMediums[i].ID == medium.ID {
			at = i
			break
		}
	}
	switch {
	case mustExist && at < 0:
		return StorageMediumSummary{}, fmt.Errorf("%w: %s", ErrMediumNotFound, medium.ID)
	case !mustExist && at >= 0:
		return StorageMediumSummary{}, fmt.Errorf("%w: %s", ErrStorageMediumExists, medium.ID)
	case at >= 0:
		if inherit {
			// The credential is carried forward from the file that was
			// just re-read, not from this process's loaded copy, so an
			// edit that names no credential preserves a reference an
			// administrator changed by hand since this service started.
			medium.Credentials = cfg.StorageMediums[at].Credentials
		}
		// In place, so an edit does not reorder the operator's file. The
		// list is served in declaration order on every surface, and a
		// save that moved the edited medium to the end would look, to
		// somebody watching the settings page, like a change they did not
		// make.
		cfg.StorageMediums[at] = medium
	default:
		cfg.StorageMediums = append(cfg.StorageMediums, medium)
	}

	if err := b.persistConfig(cfg); err != nil {
		return StorageMediumSummary{}, err
	}
	return b.GetStorageMedium(context.Background(), medium.ID)
}

// persistConfig is the encode / validate / write / hot-reload tail every
// write in this file shares, and it is deliberately settings.go's tail
// verbatim rather than a variation on it.
//
// The encode happens BEFORE cfg.Validate for the reason UpdateSettings
// states at length: Validate resolves defaults IN PLACE, so marshalling
// afterwards would freeze this release's alerts thresholds, delete-safety
// delay and default retention chain into the operator's file as a side
// effect of adding a storage destination. The running process still uses
// the validated struct; only the bytes on disk differ from it.
//
// The caller holds configMu.
func (b *BackupService) persistConfig(cfg *config.Config) error {
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("service: encoding configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		// Safe to echo back, for the reason CreateBackupSet and
		// UpdateSettings both give for the identical line: a
		// config.ValidationError's text is built from that package's own
		// field descriptions and the caller's own submitted values, never
		// from a state or rclone error string. A credential reference is
		// a submitted value; a credential is not, and there is no field
		// here one could have arrived in.
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		return err
	}
	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		return fmt.Errorf("service: persisting configuration: %w", err)
	}
	applyValidators()
	b.adoptConfig(cfg)
	return nil
}

// mediumFromSpec turns a caller's description into the config record, and
// is the ONE place a credentials id becomes a path.
//
// It is shared by the candidate preflight and by both writes on purpose:
// what is proven and what is saved must be the same destination, and a
// second resolution written for the probe is a second answer to "which
// file".
func (b *BackupService) mediumFromSpec(spec StorageMediumSpec, inheritCredentials bool) (config.StorageMedium, error) {
	if strings.TrimSpace(spec.ID) == "" {
		return config.StorageMedium{}, fmt.Errorf("%w: a storage medium needs an id", ErrInvalidRequest)
	}
	var creds config.MediumCredentials
	if !inheritCredentials {
		var err error
		creds, err = b.resolveSpecCredentials(spec.Credentials)
		if err != nil {
			return config.StorageMedium{}, err
		}
	}
	return config.StorageMedium{
		ID:                 spec.ID,
		Type:               spec.Type,
		Region:             spec.Region,
		Endpoint:           spec.Endpoint,
		Bucket:             spec.Bucket,
		Prefix:             spec.Prefix,
		StorageClass:       spec.StorageClass,
		UploadVerification: spec.UploadVerification,
		Credentials:        creds,
	}, nil
}

// resolveSpecCredentials turns the four accepted spellings into the three
// the schema holds, refusing anything that names none or more than one.
//
// The "exactly one" rule is asked here as well as in config.Validate, and
// the duplication is deliberate. config.Validate sees the resolved record
// and can only say "exactly one of file, env or command", which is a
// sentence about a schema this caller never wrote: somebody who sent an
// id AND an env var needs to be told about the id and the env var they
// actually sent.
func (b *BackupService) resolveSpecCredentials(in StorageMediumCredentials) (config.MediumCredentials, error) {
	named := make([]string, 0, 4)
	if in.ID != "" {
		named = append(named, "credentials_id")
	}
	if in.File != "" {
		named = append(named, "credentials.file")
	}
	if in.Env != "" {
		named = append(named, "credentials.env")
	}
	if len(in.Command) > 0 {
		named = append(named, "credentials.command")
	}
	switch len(named) {
	case 0:
		return config.MediumCredentials{}, fmt.Errorf(
			"%w: a storage medium needs a credential reference: one of credentials_id (from an earlier credentials import), "+
				"credentials.file, credentials.env or credentials.command", ErrInvalidRequest)
	case 1:
	default:
		sort.Strings(named)
		return config.MediumCredentials{}, fmt.Errorf(
			"%w: a storage medium names exactly one credential source, and this names %s", ErrInvalidRequest, strings.Join(named, " and "))
	}

	switch {
	case in.ID != "":
		path, err := resolveMediumCredentialsFileIn(b.configPath, in.ID)
		if err != nil {
			return config.MediumCredentials{}, err
		}
		// The id becomes a `file`, which is the source this schema
		// prefers hardest: rclone opens the file itself, so the secret
		// never enters this process at all. That is the whole reason the
		// import writes a shared-credentials file rather than, say,
		// keeping the material somewhere this process would have to read.
		return config.MediumCredentials{File: path}, nil
	case in.File != "":
		return config.MediumCredentials{File: in.File}, nil
	case in.Env != "":
		return config.MediumCredentials{Env: in.Env}, nil
	default:
		return config.MediumCredentials{Command: append([]string(nil), in.Command...)}, nil
	}
}

// storageMediumInUseRefusal builds the FR-30 removal refusal, carrying
// the count and the sets rather than only the word "no".
//
// The sentence names what the copies ARE and what they are not, because
// the distinction is the whole of FR-30: an unreachable copy is one this
// deployment cannot ask about, and it is emphatically not a copy that is
// gone.
func storageMediumInUseRefusal(usage StorageMediumUsage) error {
	sets := make([]string, 0, len(usage.BackupSets))
	onlyCopy := 0
	for _, s := range usage.BackupSets {
		sets = append(sets, fmt.Sprintf("%s (%d)", s.Set, s.Placements))
		onlyCopy += s.OnlyCopyHere
	}
	msg := fmt.Sprintf(
		"%d cop%s on storage medium %q, across %s. Removing the declaration would not delete them, it would leave this "+
			"deployment with no bucket, no endpoint and no credential to reach them with, so they would read as unreachable "+
			"and no prune could ever run against them",
		usage.Placements, plural(usage.Placements, "y", "ies"), usage.Medium, strings.Join(sets, ", "))
	if onlyCopy > 0 {
		msg += fmt.Sprintf("; %d of them %s the only confirmed copy of their artifact anywhere", onlyCopy, plural(onlyCopy, "is", "are"))
	}
	return fmt.Errorf("%w: %s", ErrStorageMediumInUse, msg)
}

// plural picks between two spellings for n, so the refusal above reads as
// a sentence for one copy as well as for a hundred and forty-eight.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// AsStorageMediumInUse reports whether err is, or wraps,
// ErrStorageMediumInUse, so the layer above turns it into a named refusal
// rather than a 500. Beside the sentinel rather than at each call site,
// for the reason every other As* in this codebase is: an errors.Is
// spelled out three times is three chances to spell it differently.
func AsStorageMediumInUse(err error) bool { return errors.Is(err, ErrStorageMediumInUse) }
