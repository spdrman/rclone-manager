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

	"github.com/spdrman/rclone-manager/core/internal/app"
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

// Settings is every server-side setting this API can report. One field
// per section, so a future section is an added field rather than a new
// method or a new route.
type Settings struct {
	Retention RetentionSettings
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

// UpdateSettingsRequest is one settings write. One optional pointer per
// section: a nil section is untouched, and a request that names no
// section at all is refused rather than treated as a no-op write, so a
// caller never gets a 200 for a request that did nothing.
type UpdateSettingsRequest struct {
	Retention *RetentionUpdate
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
	return Settings{Retention: toRetentionSettings(b.state.Load().inner.Config.Retention)}, nil
}

// UpdateSettings validates req against the config file's current content,
// persists the result atomically, and hot-reloads this BackupService so
// the new policy is immediately in effect — the same sequence, and the
// same reasoning, as CreateBackupSet (backupsets.go's package doc).
//
// It returns the settings that are now running, so a caller renders what
// was actually persisted (defaults resolved, timezone canonicalised)
// rather than echoing back its own request.
func (b *BackupService) UpdateSettings(_ context.Context, req UpdateSettingsRequest) (Settings, error) {
	if b.configPath == "" {
		return Settings{}, ErrConfigNotFileBacked
	}
	if req.Retention == nil {
		return Settings{}, fmt.Errorf("%w: a settings write must name at least one setting to change", ErrInvalidRequest)
	}
	if req.Retention.Tiers != nil && len(req.Retention.Tiers) == 0 {
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

	applyRetentionUpdate(&cfg.Retention, *req.Retention)

	if err := cfg.Validate(); err != nil {
		// Safe to echo back to an API caller, for exactly the reason
		// CreateBackupSet gives for the identical line: a
		// config.ValidationError's text is built from this package's own
		// field descriptions and the caller's own submitted values, never
		// from a state or rclone error string.
		return Settings{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	if err := writeConfigAtomically(b.configPath, cfg); err != nil {
		return Settings{}, fmt.Errorf("service: persisting configuration: %w", err)
	}

	// The one atomic swap that makes the new policy take effect, with the
	// same {inner, revision} non-torn guarantee CreateBackupSet's own
	// Store() carries; see BackupService.state's doc. prevInner is read
	// once, before the swap, purely to carry the already-wired Transport
	// and alert state forward.
	prevInner := b.state.Load().inner
	newInner := app.New(cfg, b.journal, prevInner.Transport, b.logger)
	if !newInner.AdoptAlerts(prevInner.Alerts) && b.alertSink != nil {
		newInner.EnableAlerts(sinkAdapter{sink: b.alertSink})
	}
	b.state.Store(&configState{inner: newInner, revision: computeConfigRevision(cfg)})

	return Settings{Retention: toRetentionSettings(cfg.Retention)}, nil
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

// toRetentionSettings projects a validated config.Retention into the
// provider-agnostic shape above, resolving the chain through
// EffectiveTiers so the caller always sees the tiers actually deciding
// rather than whichever of the two spellings the file happens to use.
func toRetentionSettings(r config.Retention) RetentionSettings {
	effective := r.EffectiveTiers()
	tiers := make([]RetentionTier, 0, len(effective))
	for _, t := range effective {
		tiers = append(tiers, RetentionTier{
			Name:        t.Name,
			Granularity: t.Granularity,
			PeriodDays:  t.PeriodDays,
			Keep:        t.Keep,
			WindowUnit:  t.WindowUnit,
		})
	}
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
