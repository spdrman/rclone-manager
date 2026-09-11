package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/spdrman/backupd/core/internal/config"
)

// The local hard drive as a first-class storage destination, and the
// destination a newly created retention tier starts on (H2.2, issue
// #622).
//
// # The one decision this whole issue turns on
//
// Before this file, "where does a tier's backups live" had two spellings.
// In the configuration file it is absence: `medium:` omitted means the
// backup set's own local_path, and `medium: local` is REFUSED rather than
// accepted as a synonym, which is what keeps a settings save from
// injecting a key into a file that never configured a destination
// (config.RetentionTier.Medium's own doc, and FR-35's round-trip rule).
// Everywhere else, `config.MediumLocal` is the reserved id a placement
// record already carries for exactly the same fact.
//
// Two spellings of one fact is how a read and a write come to disagree,
// and on this particular fact a disagreement is not a cosmetic bug: a
// settings save REPLACES the operator's whole retention chain, so a read
// that reports one spelling and a write that accepts the other would
// either be refused by config.Validate or would move somebody's backups
// by the act of editing something else. That is the failure
// RetentionTier.Medium's doc already names, one layer down.
//
// So the vocabulary is settled HERE, at the one boundary between the
// configuration file and everything above it, in both directions:
//
//	reading   a tier that names nothing reports StorageMediumLocalID
//	writing   a tier that names StorageMediumLocalID writes nothing
//
// Above core/service every tier names a destination, always, and
// StorageMediumLocalID is the id of the local one. Below core/service
// local is spelled by absence and only by absence, so the configuration
// file, config.Validate and every older binary see exactly what they saw
// before. The round trip is exact, which is the property that matters:
// read the chain, change one number, send the whole chain back, and the
// file that comes out is the file that went in.
//
// It is settled in two functions rather than at each call site.
// mediumForSurface and mediumForConfig below are the only two places in
// this repository that know absence and "local" are the same thing, and
// the four projections that cross this boundary (toRetentionTiers on the
// way out, applyRetentionUpdate and toConfigRetention on the way in, and
// the destinations list here) all go through them.
//
// # The one thing that still renders it, and why that is not a third spelling
//
// `backupd settings` and `backup-set retention` print a
// `medium=...` column only for a tier that is NOT on the local hard drive.
// That reads like a call site with an opinion and is not one: it is a
// RENDERING rule ("a destination column is worth a reader's attention when
// it is not the one every deployment already has"), it is the same rule
// those two printers already had, and it is what keeps FR-35's
// byte-for-byte CLI promise for a medium-free deployment. What those
// commands would otherwise print is `medium=local` on every tier of every
// configuration written before EPIC E, which is a reworded line an
// operator already reads.
//
// # What "default" governs, said once
//
// The destination a NEWLY CREATED tier starts on. Not where anything
// already is: moving the default moves no backup and rewrites no tier,
// and SetDefaultStorageMedium's own test asserts that the chain is
// untouched across the move. A default that relocated backups would be a
// one-click migration hiding behind a settings word, which is the
// opposite of what FR-27's consent gate exists for.

// StorageMediumLocalID is the id of the local hard drive: the destination
// every deployment already has, which is each backup set's own
// local_path.
//
// It is config.MediumLocal re-exported rather than a second constant, the
// way GranularityDay and TierLastKnownGoodName are re-exported in
// settings.go: aliasing rather than re-declaring is what makes drift
// impossible, and this is the one string in this product that MUST mean
// the same thing in a placement record, in a retention tier, in an API
// response and in a picker.
const StorageMediumLocalID = config.MediumLocal

// ErrStorageMediumIsDefault is the removal refusal #622 adds beside
// ErrStorageMediumInUse: the destination a new tier starts on cannot be
// un-declared.
//
// Its own sentinel rather than a second use of ErrStorageMediumInUse,
// because the two refusals are about different things and call for
// different next steps. In-use says "backups are there and removing the
// declaration would leave them unreachable"; this one says "nothing is
// necessarily there, but this is where the next tier would start, so
// choose another one first". A caller that could not tell them apart
// would show an operator a list of affected backup sets for a
// destination that holds none.
var ErrStorageMediumIsDefault = errors.New("service: storage medium is this deployment's default destination")

// AsStorageMediumIsDefault reports whether err is, or wraps,
// ErrStorageMediumIsDefault, so the layer above turns it into a named
// refusal rather than a 500. Beside the sentinel rather than at each call
// site, for the reason every other As* in this codebase is.
func AsStorageMediumIsDefault(err error) bool { return errors.Is(err, ErrStorageMediumIsDefault) }

// mediumForSurface is the READ half of the normalisation: what a tier's
// destination is called above this boundary.
//
// Absence becomes the reserved local id, so no caller anywhere has to
// know that a tier used to be able to name nothing. Anything else is
// handed on unchanged, including a name this configuration does not
// declare: whether a name resolves is config.Validate's question, asked
// over the whole configuration, and a projection that quietly rewrote an
// unresolvable name would be a second, softer answer to it.
func mediumForSurface(configured string) string {
	if configured == "" {
		return StorageMediumLocalID
	}
	return configured
}

// mediumForConfig is the WRITE half: what a tier's destination is called
// in the configuration file.
//
// The reserved local id becomes absence, which is the only spelling of
// local the schema accepts (config.Validate refuses `medium: local` in so
// many words). An already-absent value stays absent, so a caller that
// never learned the new vocabulary writes exactly what it wrote before.
func mediumForConfig(surface string) string {
	if surface == StorageMediumLocalID {
		return ""
	}
	return surface
}

// One consequence of the pair above is worth writing down rather than
// leaving somebody to find: `--policy-file` and `backup-set retention
// --policy-file` accept `medium: local` in the YAML they are handed,
// where the SAME text in config.yaml is refused.
//
// That is not an accident of the plumbing (those files are parsed into
// config.Retention and then projected out through toRetentionTiers, so
// they meet mediumForSurface like any other read), and it is not a hole.
// It lands on the right meaning: `local` is the name the picker shows,
// the name `medium default` takes, and the name `--tier-medium` takes, so
// an operator who writes it in a policy file has written what every other
// surface taught them, and the file that gets persisted still spells local
// by absence. Refusing it here would mean refusing the product's own word
// on one surface out of five, which is the shape of problem #622 exists to
// remove rather than an instance of the rule it enforces.
//
// config.yaml itself stays strict, deliberately. That is the file an older
// binary loads, and its round-trip rule is what FR-35 pins.

// localStorageMediumSummary is the local hard drive as the destinations
// list reports it, for a configuration that never declared it.
//
// Since #670 a fresh install declares local for real
// (seedLocalStorageMedium), and toStorageMediumSummaries uses that row
// instead of calling this function. This one stays as the fallback for
// every configuration written before #670: it is SYNTHESISED on every
// read rather than materialised, the same decision
// EffectiveDefaultStorageMedium and RetentionTier.EffectiveMedium both
// make, so an old config.yaml keeps loading and resolving exactly as it
// always did, with no migration and no rewrite the operator never asked
// for.
//
// Path is the drive it writes to, resolved through
// config.EffectiveBackupRoot exactly as the capacity section resolves it,
// so the two cannot report different mounts for one deployment. It is
// empty when this configuration cannot say (no backup sets yet, or sets on
// genuinely different volumes), which a surface renders as "not known
// yet" rather than as a blank path, and which the local test connection
// reports as its own first failure.
//
// The rest of the fields are deliberately the ones that mean something
// for a directory and no more. There is no bucket, no region and no
// endpoint, because there is no provider; StorageClass says what a local
// filesystem gives you, which is a copy that can be read back the instant
// it lands; UploadVerification is `readback`, which is not a claim about
// configuration but a statement of what proving a local copy actually
// takes, and it is the class internal/placement already resolves a local
// placement under.
func localStorageMediumSummary(cfg *config.Config) StorageMediumSummary {
	root := ""
	if cfg != nil {
		root = cfg.EffectiveBackupRoot()
	}
	return StorageMediumSummary{
		ID:   StorageMediumLocalID,
		Type: storageMediumTypeLocal,
		Path: root,
		// Empty rather than a made-up word. A surface that saw a storage
		// class here would render a class beside a directory, and the
		// only honest thing to put in it is the absence of one.
		StorageClass:        "",
		UploadVerification:  "readback",
		ReadsRequireRestore: false,
		IsDefault:           cfg.EffectiveDefaultStorageMedium() == StorageMediumLocalID,
		IsLocal:             true,
	}
}

// storageMediumTypeLocal is the `type` the local entry reports.
//
// It is NOT a type config.StorageMedium may be declared with: that set is
// closed to s3 and grows only by an FR-28 architecture decision, and
// config.Validate refuses anything else. This word exists on the READ
// surface only, so a picker can tell a directory from a bucket without
// pattern-matching an id, and it is the same word transport already uses
// for the store that resolves a local placement.
const storageMediumTypeLocal = "local"

// SetDefaultStorageMedium moves the destination a newly created retention
// tier starts on (issue #622).
//
// It moves that and nothing else. Not one existing tier is rewritten and
// not one backup is relocated, which is the whole reason this is a single
// write with no disclosure in front of it: FR-27's consent gate stands in
// front of the save that actually sends a tier's backups off this machine,
// and putting a second acknowledgment in front of a write with no
// consequence would train an operator to click through the one that
// matters.
//
// The local hard drive is spelled by clearing the key, for the reason
// mediumForConfig gives: local has one spelling in a configuration file
// and it is absence. So a deployment that moves its default out to S3 and
// back again ends up with the file it started with, which is the FR-35
// round trip that lets an older binary go on loading it.
//
// Whether the id names a declared destination is asked HERE as well as by
// config.Validate, and the duplication is deliberate in the direction
// resolveSpecCredentials' is: Validate sees the whole configuration and
// answers about a key, while an operator picking from a list needs "that
// is not one of your destinations" against the id they picked.
func (b *BackupService) SetDefaultStorageMedium(ctx context.Context, id string) (StorageMediumSummary, error) {
	if b.configPath == "" {
		return StorageMediumSummary{}, ErrConfigNotFileBacked
	}
	if id == "" {
		return StorageMediumSummary{}, fmt.Errorf("%w: a storage destination id is required; the local hard drive is %q", ErrInvalidRequest, StorageMediumLocalID)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	// Re-read from disk rather than from b.state, the same "always read
	// fresh" discipline every write in this package documents: a
	// destination added by hand, or by a second process, since this
	// service loaded is a destination this write must be able to name.
	cfg, err := config.Load(b.configPath)
	if err != nil {
		return StorageMediumSummary{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}
	if !declaresStorageDestination(cfg, id) {
		return StorageMediumSummary{}, fmt.Errorf("%w: %s", ErrMediumNotFound, id)
	}

	cfg.DefaultStorageMedium = mediumForConfig(id)
	if err := b.persistConfig(cfg); err != nil {
		return StorageMediumSummary{}, err
	}
	return b.GetStorageMedium(ctx, id)
}

// declaresStorageDestination reports whether id names a destination this
// configuration has: the local hard drive, which every deployment has, or
// one of the declared mediums.
//
// One predicate rather than a comparison written out at each call site,
// because "is this a real destination" is now asked by the default write,
// by the removal refusals and by the list, and three spellings of it is
// three chances to forget that local is one.
func declaresStorageDestination(cfg *config.Config, id string) bool {
	if id == StorageMediumLocalID {
		return true
	}
	if cfg == nil {
		return false
	}
	for _, m := range cfg.StorageMediums {
		if m.ID == id {
			return true
		}
	}
	return false
}

// normalizeDefaultStorageMedium enforces the third invariant #622 states:
// if a removal leaves exactly one destination, that one becomes the
// default.
//
// It is applied on the way out of a removal, unconditionally, rather than
// left to follow from a fact about local specifically. The issue asks for
// the rule to be stated and tested rather than inherited from a side
// effect, and there is a second reason to write it down: it also repairs
// a default this removal orphaned. The default cannot normally be
// removed (RemoveStorageMedium refuses it), but
// a configuration edited by hand between two of this process's writes can
// arrive here naming a destination the removal just took out, and a
// dangling default is a config.Validate failure that would surface as "the
// removal broke my configuration" rather than as anything an operator
// could act on.
//
// It reports whether it changed anything, so a caller writes the file for
// this reason only when there is a reason.
func normalizeDefaultStorageMedium(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	// Zero declared destinations only ever happens on a configuration
	// that never declared local at all (every one written before #670,
	// or one hand-edited since) — a fresh install's seed guarantees at
	// least one row from first boot, and RemoveStorageMedium's own
	// invariant (the default cannot be removed, and the last one
	// remaining is always the default) keeps a legitimate removal from
	// ever reaching zero. Spelled as a count over the whole set rather
	// than a name check, so the rule in the code is the rule in the
	// issue.
	if len(cfg.StorageMediums) == 0 {
		if cfg.DefaultStorageMedium == "" {
			return false
		}
		cfg.DefaultStorageMedium = ""
		return true
	}
	if cfg.DefaultStorageMedium == "" || declaresStorageDestination(cfg, cfg.DefaultStorageMedium) {
		return false
	}
	// The default named something this configuration no longer declares.
	// Falling back to the local hard drive is the safe direction: it is
	// the destination that cannot go away, and a new tier starting on
	// local disk is a tier whose backups stay where every unset tier's
	// backups already are.
	cfg.DefaultStorageMedium = ""
	return true
}

// storageMediumIsDefaultRefusal builds the removal refusal, naming what to
// do about it rather than only saying no.
//
// The sentence carries the fix because the fix is not guessable from the
// refusal: an operator looking at a destination they want gone has no
// reason to know that the destination a new tier starts on is a separate,
// movable setting. It also says what is NOT the problem, because the other
// removal refusal on this call is about backups being there and somebody
// meeting this one will reasonably assume it is that.
func storageMediumIsDefaultRefusal(id string) error {
	return fmt.Errorf("%w: %s is the destination a newly created retention tier starts on, so this deployment would have no default if it went away. "+
		"Nothing is necessarily stored on it: this refusal is about where the NEXT tier would start, not about backups that are already somewhere. "+
		"Make another destination the default first, then remove this one",
		ErrStorageMediumIsDefault, id)
}
