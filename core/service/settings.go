// This file is issue #140 (B3.7)'s settings surface: reading the
// server-side configuration a shared Web UI is allowed to administer, and
// writing a change to it, through the same persist-then-hot-reload
// sequence backupsets.go established for the add-backup-set wizard.
//
// # "Generic" means enumerated, not open
//
// The HTTP layer on top of this (apps/common/webhost's GET/PATCH
// /api/v1/settings) is deliberately one route for every administrable
// setting rather than one route per setting, so adding the next one is a
// field on UpdateSettingsRequest and nothing else. That generality stops
// firmly short of "accept a config and write it": UpdateSettingsRequest
// enumerates, field by field, exactly what may be changed, there is no
// passthrough of arbitrary YAML keys anywhere in this file, and every
// write goes through the identical config.Validate a hand-edited file
// goes through at boot. A caller cannot reach state.database, a backup
// set's remote, or a validator command through this path, because there
// is no field here that names one.
//
// # Refuse, never partially apply
//
// UpdateSettings folds the request onto a COPY of the freshly re-read
// config, validates the whole thing, and only then writes. A refusal
// therefore leaves both the file and this process's running policy
// exactly as they were: there is no ordering in this file where a
// half-applied policy can reach either. settings_test.go asserts the file
// is byte-for-byte unchanged after every refusal it drives.
package service

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// Retention granularity names, re-exported from internal/config so a
// caller outside core/ (apps/common/webhost, and through it the settings
// form) can name one without importing a package Go's own "internal"
// rule puts out of its reach. These are the same constants
// config.Validate checks against, not a second list: aliasing rather than
// re-declaring is what makes drift impossible.
const (
	GranularityDay      = config.GranularityDay
	GranularityWeek     = config.GranularityWeek
	GranularityMonth    = config.GranularityMonth
	GranularityQuarter  = config.GranularityQuarter
	GranularityHalfYear = config.GranularityHalfYear
	GranularityYear     = config.GranularityYear
	GranularityDays     = config.GranularityDays
)

// TierLastKnownGoodName is the tier name FR-19's protected term already
// occupies, which a configured tier may therefore not claim. See
// config.TierLastKnownGoodName.
const TierLastKnownGoodName = config.TierLastKnownGoodName

// RetentionTierKeepMax and RetentionTierPeriodDaysMax bound a tier's two
// numbers from above. See config's own doc for why an unbounded value is
// refused rather than left to overflow into a silently empty selection.
const (
	RetentionTierKeepMax       = config.RetentionTierKeepMax
	RetentionTierPeriodDaysMax = config.RetentionTierPeriodDaysMax
)

// RetentionTier is one link in FR-18's retention chain, in the plain,
// provider-agnostic shape a caller outside core/ sees (config.RetentionTier
// is not nameable from there). Field for field the same; see
// config.RetentionTier's own doc for what each one means and why the
// shape is flat and enumerated rather than free-form.
type RetentionTier struct {
	Name        string
	Granularity string
	// PeriodDays is required when Granularity is GranularityDays, and
	// must be zero for every other granularity.
	PeriodDays int
	Keep       int
	// WindowUnit measures the look-back in a unit other than Granularity;
	// empty means "the same as Granularity", which is the ordinary case.
	WindowUnit string
	// Medium names the storage medium this tier's artifacts live on
	// (EPIC E, FR-27); empty means the local backup root, which is what
	// every tier of every configuration written before EPIC E means.
	//
	// It is carried here for a reason stronger than symmetry with the
	// config schema. RetentionUpdate.Tiers REPLACES the whole chain, so
	// a field this type cannot hold is a field a settings save deletes
	// from the operator's file: editing daily's keep would have quietly
	// moved monthly's artifacts back onto local disk. A lossy boundary
	// between the file and the form is a configuration change nobody
	// asked for, made by the act of changing something else.
	Medium string
}

// RetentionSettings is the FR-18/FR-19 policy as it is actually
// deciding.
//
// Tiers is always the RESOLVED chain (config.Retention.EffectiveTiers), so
// a config file written with the legacy daily_days/weekly_months/
// monthly_months sugar reports the three-tier chain those keys stand for
// rather than the sugar itself. A form that rendered the sugar could not
// show the policy in effect for a tiers-based file at all, and would need
// two layouts for one policy.
//
// ProtectLastKnownGood is a plain bool, not the *bool the config schema
// uses: the pointer exists there only to tell "absent" from "explicitly
// false" while defaulting, and by the time a policy has been validated
// that question is settled. RetentionUpdate below keeps the pointer,
// because on the WRITE side "not named" is a real, distinct input again.
type RetentionSettings struct {
	Timezone             string
	WeekStartsOn         string
	Tiers                []RetentionTier
	ProtectLastKnownGood bool
}

// CapacitySettings is FR-21's configuration as it is actually deciding
// (issue #286): the operator's storage cap, the two levels a reading is
// weighed against, the safety margin held back before every transfer, and
// the filesystem all of that is measured on.
//
// Every number is bytes. The MB/GB picker beside the field in the UI is
// display only and converts at the edge; nothing on this boundary or in
// the config file carries a unit, because a number whose meaning depends
// on a second field is a two-order-of-magnitude mistake waiting to be
// made.
type CapacitySettings struct {
	// CapBytes is the ceiling, and zero means no cap. See
	// config.Capacity.CapBytes: it is a sentinel, and neither this
	// boundary nor anything under it may resolve it to a number.
	CapBytes int64

	// WarningFreeBytes, CriticalFreeBytes and SafetyMarginBytes are the
	// rest of FR-21's numbers. Zero means "no line here" for the first
	// two, which is the default.
	WarningFreeBytes  int64
	CriticalFreeBytes int64
	SafetyMarginBytes int64

	// BackupRoot is the directory whose filesystem a manager-wide storage
	// reading is taken from, ALREADY RESOLVED: the configured value when
	// there is one, and otherwise the directory every backup set's
	// destination has in common. Empty means this configuration cannot say
	// (no backup sets, or sets on genuinely different volumes), which a
	// form has to render as "not known yet" rather than as a blank path.
	//
	// It is reported so an operator can see which mount the cap is
	// measured against before trusting a number drawn from it. The engine
	// runs in a container, and a reading taken from the wrong mount is a
	// confident wrong number nobody would notice.
	BackupRoot string

	// BackupRootConfigured separates a root an operator chose from one
	// this product derived. A form that showed the derived value in an
	// input box would turn the next save into an explicit choice, pinning
	// today's derivation into the file forever.
	BackupRootConfigured bool
}

// Settings is every server-side setting this API can report. One field
// per section, so a future section is an added field rather than a new
// method or a new route.
type Settings struct {
	Retention RetentionSettings
	Capacity  CapacitySettings
}

// CapacityUpdate names the capacity fields a settings write should change.
// Every field is a pointer, and nil means "leave this alone".
//
// The pointers are load-bearing rather than stylistic, and more so here
// than anywhere else in this file: zero is a MEANING in three of these
// four fields ("no cap", "no warning line", "no critical line"), so a
// plain int64 could not tell "remove my cap" from "I did not mention the
// cap". Those are opposite requests.
type CapacityUpdate struct {
	CapBytes          *int64
	WarningFreeBytes  *int64
	CriticalFreeBytes *int64
	SafetyMarginBytes *int64
}

// namesNothing reports a capacity section that carries no field at all,
// which UpdateSettings refuses exactly as it refuses an absent one. See
// RetentionUpdate.namesNothing for why the check is structural.
func (u CapacityUpdate) namesNothing() bool {
	return u.CapBytes == nil &&
		u.WarningFreeBytes == nil &&
		u.CriticalFreeBytes == nil &&
		u.SafetyMarginBytes == nil
}

// RetentionUpdate names the retention fields a settings write should
// change. Every field is optional and a zero value means "leave this
// alone", which is what lets one generic write endpoint carry a change to
// a single toggle without the caller having to send back (and therefore
// re-assert) a whole policy it never looked at.
//
// The pointer/nil-slice spelling is load-bearing rather than stylistic:
// "the operator did not touch the chain" and "the operator emptied the
// chain" are genuinely different requests, and only one of them is legal
// (see Tiers' own doc below).
type RetentionUpdate struct {
	// Timezone and WeekStartsOn are nil to leave the current value alone.
	Timezone     *string
	WeekStartsOn *string

	// Tiers replaces the whole chain. nil leaves it alone, which also
	// leaves a legacy daily_days/weekly_months/monthly_months file in its
	// own spelling; a non-nil chain clears those three scalars, because
	// config.Validate refuses a config carrying both spellings at once
	// (Retention.Tiers' own doc: an operator who wrote both is asking two
	// different questions). This is exactly the rule the CLI's own -tier
	// override already applies in applyRetentionOverrides
	// (core/cmd/backup-manager/retention_flags.go), so the two write
	// paths cannot resolve the same submission differently.
	//
	// An explicitly EMPTY chain is refused rather than applied. In the
	// config file an empty tiers list is indistinguishable from an absent
	// key and reinstates FR-18's default 7/3/12 policy, which is the
	// fail-safe reading for a file but the opposite of what "I removed
	// every tier" means coming from a form. There is no "keep nothing"
	// spelling in this schema at all (retention is turned off by not
	// running a retention pass), so the honest answer to that submission
	// is a refusal that says so, not a silent widening of the policy.
	Tiers []RetentionTier

	// ProtectLastKnownGood turns FR-19's protection on or off. nil leaves
	// it alone; an explicit false is written through as an explicit
	// false, never as an omitted key, since an omitted key reads back as
	// true. See config.Retention.ProtectLastKnownGood's own doc.
	//
	// internal/retention calls an explicit false "a materially more
	// dangerous configuration" (LastKnownGoodDecide), and issue #140
	// requires the operator be shown that before the change takes effect.
	// That confirmation belongs at the UI, in front of the human: this
	// method has no way to tell an operator who confirmed from one who
	// was never asked, so it honours the value exactly as written rather
	// than pretending to be a second gate.
	ProtectLastKnownGood *bool
}

// namesNothing reports a retention section that carries no field at all,
// which UpdateSettings refuses exactly as it refuses an absent section.
// An explicitly empty Tiers is NOT "nothing named": it is a request with a
// meaning, and a refusal of its own that says what emptying the chain
// would actually do.
func (u RetentionUpdate) namesNothing() bool {
	return u.Timezone == nil &&
		u.WeekStartsOn == nil &&
		u.Tiers == nil &&
		u.ProtectLastKnownGood == nil
}

// UpdateSettingsRequest is one settings write. One optional pointer per
// section: a nil section is untouched, and a request that names no
// setting at all is refused rather than treated as a no-op write, so a
// caller never gets a 200 for a request that did nothing. "Names no
// setting" is structural, not per-section: a section that is present but
// entirely empty asks for nothing just as surely as an absent one, and is
// refused the same way (see RetentionUpdate.namesNothing).
type UpdateSettingsRequest struct {
	Retention *RetentionUpdate
	Capacity  *CapacityUpdate
}

// namesNothing reports a request that asks for no change at all: no
// section present, or every section present carrying no field. It is
// structural rather than per-section for the reason UpdateSettings' own
// doc gives: a present-but-empty section asks for nothing just as surely
// as an absent one, and honouring it would rewrite the operator's file,
// move ConfigRevision and answer 200 for a request with no content.
func (r UpdateSettingsRequest) namesNothing() bool {
	named := false
	if r.Retention != nil {
		if !r.Retention.namesNothing() {
			named = true
		}
	}
	if r.Capacity != nil {
		if !r.Capacity.namesNothing() {
			named = true
		}
	}
	return !named
}

// anySectionIsEmpty reports a section that was sent but carries no field.
// It is refused separately from namesNothing so that a request naming a
// real retention change AND an empty capacity object is still refused: the
// empty object is a caller mistake, and silently ignoring half a request
// is how a settings page reports success for a change that never
// happened.
func (r UpdateSettingsRequest) anySectionIsEmpty() bool {
	return (r.Retention != nil && r.Retention.namesNothing()) ||
		(r.Capacity != nil && r.Capacity.namesNothing())
}

// RetentionSchemaInfo is the closed value sets and bounds
// config.Validate enforces on a retention chain, reported so a client can
// build its pickers from, and validate against, the same rules rather
// than a hand-copied list. See config.RetentionGranularities' own doc.
type RetentionSchemaInfo struct {
	Granularities []string
	WindowUnits   []string
	// TierNamePattern is an anchored regular expression in a syntax a
	// browser also accepts (config.RetentionTierNamePattern).
	TierNamePattern string
	// ReservedTierName is the one name a configured tier may not use.
	ReservedTierName string
	KeepMax          int
	PeriodDaysMax    int
	// DefaultTiers is the chain a config that configures NEITHER
	// spelling resolves to (config.DefaultRetentionTiers), served so a
	// form's "restore the default chain" affordance fills itself from the
	// product's actual default instead of a copy of those numbers
	// transcribed into a client. A stale copy there does not merely
	// display the wrong thing: saving it writes an explicit tiers list,
	// which permanently migrates a legacy config off the default it would
	// otherwise have tracked, possibly onto a NARROWER policy. Every other
	// closed value set in this struct is served for the same reason.
	DefaultTiers []RetentionTier
}

// RetentionSchema reports the rules a retention chain is validated
// against. It is a package-level function rather than a method because
// the answer is a property of the schema, identical for every
// BackupService and available before one exists.
func RetentionSchema() RetentionSchemaInfo {
	return RetentionSchemaInfo{
		Granularities:    config.RetentionGranularities(),
		WindowUnits:      config.RetentionWindowUnits(),
		TierNamePattern:  config.RetentionTierNamePattern,
		ReservedTierName: config.TierLastKnownGoodName,
		KeepMax:          config.RetentionTierKeepMax,
		PeriodDaysMax:    config.RetentionTierPeriodDaysMax,
		DefaultTiers:     toRetentionTiers(config.DefaultRetentionTiers()),
	}
}

// Settings reports the settings this BackupService is currently running.
//
// Read from the loaded, validated, in-memory config (b.state), not from
// the file: the question a settings page asks is "what policy is in
// effect", and a config file edited by hand since this process started is
// not in effect until something reloads it. UpdateSettings below does
// re-read the file before writing, so an out-of-band edit is never
// clobbered — the two are answering different questions on purpose.
func (b *BackupService) Settings(_ context.Context) (Settings, error) {
	cfg := b.state.Load().inner.Config
	return Settings{
		Retention: toRetentionSettings(cfg.Retention),
		Capacity:  toCapacitySettings(cfg),
	}, nil
}

// toCapacitySettings projects the capacity block onto the boundary shape,
// resolving BackupRoot on the way out so a caller never has to re-derive
// it and cannot derive it differently.
func toCapacitySettings(cfg *config.Config) CapacitySettings {
	return CapacitySettings{
		CapBytes:             cfg.Capacity.CapBytes,
		WarningFreeBytes:     cfg.Capacity.WarningFreeBytes,
		CriticalFreeBytes:    cfg.Capacity.CriticalFreeBytes,
		SafetyMarginBytes:    cfg.Capacity.SafetyMarginBytes,
		BackupRoot:           cfg.EffectiveBackupRoot(),
		BackupRootConfigured: cfg.Capacity.BackupRoot != "",
	}
}

// UpdateSettings validates req against the config file's current content,
// persists the result atomically, and hot-reloads this BackupService so
// the new policy is immediately in effect, following the same sequence, in
// the same order, and for the same reasons as CreateBackupSet
// (backupsets.go's package doc), validator-catalog resolution included.
//
// It returns the settings that are now IN EFFECT, so a caller renders the
// policy this process is actually deciding with (defaults resolved,
// timezone canonicalised) rather than echoing back its own request. That
// is deliberately not the same thing as "what the file now says": the file
// keeps the operator's own omissions, and a default they never wrote is
// resolved in memory on every load rather than frozen into their YAML (see
// the encode step below).
func (b *BackupService) UpdateSettings(_ context.Context, req UpdateSettingsRequest) (Settings, error) {
	if b.configPath == "" {
		return Settings{}, ErrConfigNotFileBacked
	}
	// A section that is present but carries no field at all is refused
	// exactly like an absent one. The guard is structural (every field of
	// every named section nil) rather than per-section, because
	// "{\"retention\":{}}" satisfies a per-section check while asking for
	// nothing: it would re-marshal and rewrite the operator's config file,
	// move ConfigRevision (invalidating every outstanding retention
	// preview), swap the running app.Service and answer 200, all for a
	// request with no content. UpdateSettingsRequest's own doc promises a
	// caller never gets a 200 for a request that did nothing, and only the
	// structural form of the check keeps that promise.
	if req.namesNothing() {
		return Settings{}, fmt.Errorf("%w: a settings write must name at least one setting to change", ErrInvalidRequest)
	}
	if req.anySectionIsEmpty() {
		return Settings{}, fmt.Errorf("%w: a settings section was sent with no field in it; omit the section instead of sending an empty one", ErrInvalidRequest)
	}
	if req.Retention != nil && req.Retention.Tiers != nil && len(req.Retention.Tiers) == 0 {
		// See RetentionUpdate.Tiers' own doc. The message has to carry
		// what an empty chain would actually mean, or an operator reads
		// the refusal as a form-validation quibble and never learns that
		// emptying the list widens the policy rather than disabling it.
		return Settings{}, fmt.Errorf("%w: retention.tiers must name at least one tier; an empty chain is not \"keep nothing\", it reinstates the default daily/weekly/monthly policy, and retention is turned off by not running a retention pass at all", ErrInvalidRequest)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	// Re-read from disk rather than from b.state, the same "always read
	// fresh" discipline CreateBackupSet documents: the write below is
	// based on the file's actual current content, so a change made by
	// hand (or by a second process) since this service loaded survives a
	// settings write that does not touch it.
	cfg, err := config.Load(b.configPath)
	if err != nil {
		return Settings{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	if req.Retention != nil {
		applyRetentionUpdate(&cfg.Retention, *req.Retention)
	}
	if req.Capacity != nil {
		applyCapacityUpdate(&cfg.Capacity, *req.Capacity)
	}

	// Encode what will be written BEFORE cfg.Validate, and write these
	// bytes rather than re-encoding the validated struct.
	//
	// config.Validate resolves defaults IN PLACE (its own doc), so
	// marshalling afterwards persists every one of them: a file that never
	// mentioned alerts.repeated_failure_threshold or
	// completion.delete_safety_delay gains both, and a file using neither
	// retention spelling gains a literal daily_days/weekly_months/
	// monthly_months 7/3/12. delete_safety_delay is a deletion-safety knob
	// (config.DefaultDeleteSafetyDelay's own doc explains why its zero
	// value is not read literally), so freezing this release's value into
	// the operator's file as a side effect of an unrelated retention edit
	// would silently opt that deployment out of every future change to it.
	// The three legacy retention scalars carry omitempty for exactly this
	// round trip. The running process still uses the validated struct
	// below, so nothing about what this service DOES changes; only what is
	// left in the file does. settings_test.go pins both halves.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return Settings{}, fmt.Errorf("service: encoding configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		// Safe to echo back to an API caller, for exactly the reason
		// CreateBackupSet gives for the identical line: a
		// config.ValidationError's text is built from this package's own
		// field descriptions and the caller's own submitted values, never
		// from a state or rclone error string.
		return Settings{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// Validator resolution, on the same terms and in the same order
	// CreateBackupSet uses it (backupsets.go's own comment carries the
	// full reasoning): every fallible part runs BEFORE the write, and the
	// pure in-memory assignment runs after it.
	//
	// This is not optional bookkeeping. config.Load never resolves a
	// backup set's Validation.ValidatorID into the Validation.Command
	// internal/lifecycle/verify.go runs -- the file only ever carries the
	// id -- so hot-reloading a config that skipped this step swaps in an
	// app.Service where every registered-validator backup set holds an id
	// and a nil command, which config.Validation.ResolvedCommand refuses
	// with ErrValidatorNotResolved and verify.go turns into a failed
	// verification for every artifact. That is fail-closed, but it halts a
	// live deployment's backups because somebody saved a timezone. It also
	// keeps computeConfigRevision below computing over the SAME resolved
	// config Open and CreateBackupSet compute over, so one file cannot
	// yield two different revisions depending on which path last reloaded
	// it.
	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		return Settings{}, err
	}

	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		return Settings{}, fmt.Errorf("service: persisting configuration: %w", err)
	}

	// Now, and only now that the file on disk holds the id, put the
	// resolved commands into this process's in-memory copy. cfg is never
	// written back out from here (the bytes above were captured before
	// this ran), so the resolved path stays out of config.yaml.
	applyValidators()

	// The one atomic swap that makes the new policy take effect, with the
	// same {inner, revision} non-torn guarantee every other write carries;
	// see adoptConfig and BackupService.state's doc.
	b.adoptConfig(cfg)

	return Settings{
		Retention: toRetentionSettings(cfg.Retention),
		Capacity:  toCapacitySettings(cfg),
	}, nil
}

// applyCapacityUpdate folds u onto c in place, and validates nothing: the
// caller runs the WHOLE config through the identical config.Validate a
// hand-edited YAML file goes through at boot, so a cap refused here is
// refused for exactly the reason the same number in the file would be.
//
// An explicit zero is written through as a zero, which is the whole reason
// these are pointers: on this block zero is a request ("no cap", "no
// warning line"), not an omission.
func applyCapacityUpdate(c *config.Capacity, u CapacityUpdate) {
	if u.CapBytes != nil {
		c.CapBytes = *u.CapBytes
	}
	if u.WarningFreeBytes != nil {
		c.WarningFreeBytes = *u.WarningFreeBytes
	}
	if u.CriticalFreeBytes != nil {
		c.CriticalFreeBytes = *u.CriticalFreeBytes
	}
	if u.SafetyMarginBytes != nil {
		c.SafetyMarginBytes = *u.SafetyMarginBytes
	}
}

// applyRetentionUpdate folds u onto r in place. It never validates: the
// caller runs the WHOLE config through config.Validate afterwards, which
// is the same function the YAML file goes through at boot, so a value
// submitted here is accepted or refused for the identical reason the same
// value would be in the file.
//
// Folding onto the freshly re-read config (rather than onto the running
// one) is what makes a partial update partial: every field u leaves
// unnamed keeps whatever the file currently says.
func applyRetentionUpdate(r *config.Retention, u RetentionUpdate) {
	if u.Timezone != nil {
		r.Timezone = *u.Timezone
	}
	if u.WeekStartsOn != nil {
		r.WeekStartsOn = *u.WeekStartsOn
	}
	if len(u.Tiers) > 0 {
		r.Tiers = make([]config.RetentionTier, 0, len(u.Tiers))
		for _, t := range u.Tiers {
			r.Tiers = append(r.Tiers, config.RetentionTier{
				Name:        t.Name,
				Granularity: t.Granularity,
				PeriodDays:  t.PeriodDays,
				Keep:        t.Keep,
				WindowUnit:  t.WindowUnit,
				// Carried through rather than dropped: this assignment
				// replaces the operator's whole chain, so a field left
				// out here is a field the save deletes from their file.
				// Whether the named medium exists is config.Validate's
				// question, asked over the whole config a few lines
				// after this one; nothing here second-guesses it, which
				// is what keeps every medium rule in one package.
				Medium: t.Medium,
			})
		}
		// The three scalars are sugar for the default chain, and
		// config.Validate refuses a config that carries both spellings.
		// Clearing them is therefore part of writing a chain, not an
		// extra opinion: without it, a legacy file plus a submitted chain
		// produces a config the daemon will not start on.
		r.DailyDays, r.WeeklyMonths, r.MonthlyMonths = 0, 0, 0
	}
	if u.ProtectLastKnownGood != nil {
		protect := *u.ProtectLastKnownGood
		r.ProtectLastKnownGood = &protect
	}
}

// toRetentionTiers projects internal/config's tier shape into the
// provider-agnostic one, for the two callers that need it (the policy in
// effect, and the schema's default chain).
func toRetentionTiers(in []config.RetentionTier) []RetentionTier {
	out := make([]RetentionTier, 0, len(in))
	for _, t := range in {
		out = append(out, RetentionTier{
			Name:        t.Name,
			Granularity: t.Granularity,
			PeriodDays:  t.PeriodDays,
			Keep:        t.Keep,
			WindowUnit:  t.WindowUnit,
			Medium:      t.Medium,
		})
	}
	return out
}

// toRetentionSettings projects a validated config.Retention into the
// provider-agnostic shape above, resolving the chain through
// EffectiveTiers so the caller always sees the tiers actually deciding
// rather than whichever of the two spellings the file happens to use.
func toRetentionSettings(r config.Retention) RetentionSettings {
	tiers := toRetentionTiers(r.EffectiveTiers())
	return RetentionSettings{
		Timezone:     r.Timezone,
		WeekStartsOn: r.WeekStartsOn,
		Tiers:        tiers,
		// nil reads as "absent", which config.Validate defaults to true;
		// applying that same reading here keeps an unvalidated Retention
		// (a caller that bypassed Validate) reporting the safe answer
		// rather than an accidental protection-off, exactly as
		// LastKnownGoodDecide does.
		ProtectLastKnownGood: r.ProtectLastKnownGood == nil || *r.ProtectLastKnownGood,
	}
}
