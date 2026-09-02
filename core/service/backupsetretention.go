// This file is issue #333's per-set retention surface: reading which
// policy one backup set is actually retained under, giving that set a
// policy of its own, and taking it back off again so the set inherits the
// deployment's policy once more.
//
// # The config layer already settled the rules, and this adds none
//
// #362 gave config.BackupSet a RetentionConfig *Retention where nil means
// inherit, and put the whole rule in one place: an override names the
// WHOLE chain, half a chain is refused, and everything that is not the
// chain (timezone, week start, FR-19's protection) is inherited from the
// deployment's resolved policy rather than defaulted. See
// config.resolveBackupSetRetention's own doc for why, and for what a
// partial override silently cost before it was refused.
//
// Nothing in this file restates any of that. The two write methods fold
// the submitted policy onto a freshly re-read config and run the identical
// config.Validate a hand-edited config.yaml goes through at boot, so a
// policy refused here is refused for exactly the reason, and with exactly
// the words, the same policy in the file would be. That is what makes the
// CLI verb, the HTTP routes and the Web UI one capability rather than
// three: they all arrive here, and here has no rules of its own.
//
// # Why "clear" is its own method rather than a field
//
// "Give this set no policy of its own" cannot be spelled as a value on a
// sparse update request, where a nil field already means "leave this
// alone". Those are opposite requests, and a surface where the operator
// expresses one by omitting the other is exactly the confusion
// RetentionConfig's pointer exists to prevent, moved one layer up. So
// inheriting again is its own named operation on every surface: DELETE on
// the HTTP sub-resource, --inherit on the CLI verb, and a control that
// says "return this set to the deployment's policy" in the UI.
package service

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// RetentionOverride is one backup set's own retention policy, in the
// provider-agnostic shape a caller outside core/ can name.
//
// It mirrors config.Retention field for field, both spellings of the
// chain included, and that is deliberate rather than lazy. The three
// scalars are FR-18's original spelling and the tiers list is its general
// one; a boundary that carried only one of them would have to translate
// the other, and a translation is a second place where "what does half a
// chain mean" could be answered differently from
// config.resolveBackupSetRetention. Carrying both means every surface
// submits what the operator actually wrote and the config layer is the
// only thing that ever decides whether it is a whole policy.
//
// So `{DailyDays: 120}` reaches config.Validate and is REFUSED there,
// naming the two scalars it is missing, rather than being quietly
// completed from the product defaults (which is what one level up used to
// do, and what #362 was written to stop). Refusing a half policy is the
// contract; being unable to express one is not, because the operator who
// types two of three flags has to be told which third they owe.
type RetentionOverride struct {
	// Timezone and WeekStartsOn are empty to inherit the deployment's.
	// See config.resolveBackupSetRetention: an omitted calendar field
	// inherits rather than defaulting to UTC/monday, because the calendar
	// decides how ANY chain is reckoned rather than what the chain says.
	Timezone     string
	WeekStartsOn string

	// DailyDays, WeeklyMonths and MonthlyMonths are FR-18's original
	// three-scalar chain. All three or none: two of the three is half a
	// policy and config.Validate refuses it.
	DailyDays     int
	WeeklyMonths  int
	MonthlyMonths int

	// Tiers is FR-18's general chain, and is mutually exclusive with the
	// three scalars above (config.Retention.Tiers' own doc: an operator
	// who wrote both is asking two different questions).
	Tiers []RetentionTier

	// ProtectLastKnownGood is nil to inherit the deployment's FR-19
	// posture, which is what an omitted key means one level down too.
	ProtectLastKnownGood *bool
}

// namesNothing reports an override that carries no field at all.
//
// config.Validate refuses this too (an empty `retention: {}` block names
// no chain), so this is a second line rather than the only one. It exists
// to answer with the request-shaped message a caller who sent an empty
// object needs, instead of a config-file-shaped one about a backup set
// path they never wrote, and to make an HTTP PUT with `{}` fail for the
// same stated reason UpdateSettings already fails an empty section for.
//
// An explicitly EMPTY tiers list is not "nothing named", exactly as
// RetentionUpdate.namesNothing already decides for the deployment's own
// chain: it is a request with a meaning, and one with a refusal of its
// own that has to say what emptying a chain would actually do. `nil` and
// `[]RetentionTier{}` therefore mean different things here, and they
// survive the wire as different things too, since JSON decodes an absent
// key to nil and `[]` to a non-nil empty slice.
func (o RetentionOverride) namesNothing() bool {
	return o.Timezone == "" &&
		o.WeekStartsOn == "" &&
		o.DailyDays == 0 &&
		o.WeeklyMonths == 0 &&
		o.MonthlyMonths == 0 &&
		o.Tiers == nil &&
		o.ProtectLastKnownGood == nil
}

// BackupSetRetention is the whole answer to "which policy is this backup
// set retained under, and where did it come from".
//
// All three parts are reported together on purpose. "Which policy is in
// force" and "is that this set's own or the deployment's" are one
// question for an operator staring at a preview that is about to delete
// something, and answering it in two calls is how a surface ends up
// showing a chain beside the wrong attribution.
type BackupSetRetention struct {
	// BackupSetID is "source/name", the set this answer is about.
	BackupSetID string

	// IsOverride is true when this set declares its own policy
	// (config.BackupSet.RetentionIsOverride). It reads the raw override
	// rather than comparing Effective against Deployment, because a set
	// that deliberately pinned a chain identical to the deployment's is
	// NOT inheriting: the whole point of pinning it is that a later edit
	// to the deployment's policy must not move it.
	IsOverride bool

	// Effective is the policy actually deciding for this set, resolved:
	// its own when IsOverride, the deployment's otherwise, with the
	// chain expanded to tiers either way (RetentionSettings' own doc).
	Effective RetentionSettings

	// Deployment is the top-level policy, resolved, whether or not this
	// set is currently inheriting it.
	//
	// It is served beside Effective for two reasons a client cannot meet
	// on its own. It is what "this is what you would go back to" means
	// for a set that is overriding, which is the thing an operator needs
	// before clearing an override that might be wider than the
	// deployment's. And it is the honest starting point for a form that
	// is about to CREATE an override: pre-filling from a whole resolved
	// chain is what makes every submission from that form a whole policy
	// rather than a half one.
	Deployment RetentionSettings

	// Override is the raw policy this set declared, exactly as it sits in
	// config.yaml before inheritance filled anything in, or nil when the
	// set inherits. Effective is what decides; this is what an operator
	// would find if they went and edited the file, which is a different
	// and equally necessary answer for an edit form.
	Override *RetentionOverride
}

// BackupSetRetention reports which policy backup set id is retained under.
//
// Read from the loaded, validated, in-memory config, not from the file,
// for the reason Settings gives for the same choice: the question is
// "what is deciding", and a file edited by hand since this process
// started is not deciding anything until something reloads it.
func (b *BackupService) BackupSetRetention(_ context.Context, id string) (BackupSetRetention, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return BackupSetRetention{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	cfg := b.state.Load().inner.Config
	bs, found := lookupBackupSet(cfg, sourceName, setName)
	if !found {
		return BackupSetRetention{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}
	return toBackupSetRetention(sourceName, cfg.Retention, bs), nil
}

// SetBackupSetRetention gives one already-persisted backup set a
// retention policy of its own, persists it to the configuration file this
// BackupService was opened from, and hot-reloads so the new policy is
// immediately deciding.
//
// # It replaces, it never merges
//
// The submitted policy becomes this set's whole override. There is no
// field-by-field merge with what the set previously declared and none
// with the deployment's chain, because merging two chains produces a
// policy nobody wrote and nobody can predict from reading either half:
// config.BackupSet.RetentionConfig's own doc, and the reason #362's first
// draft was rejected. A caller that wants to change one number in an
// existing override reads the current one (BackupSetRetention above) and
// submits the whole thing back, which is the same round trip a form makes
// anyway.
//
// That is also why this is not a field on UpdateBackupSet's sparse patch.
// A sparse patch's contract is "nil leaves this alone", and a whole-policy
// value inside a field-by-field request is exactly the shape that invites
// somebody to send half of one.
func (b *BackupService) SetBackupSetRetention(_ context.Context, id string, o RetentionOverride) (BackupSetRetention, error) {
	if o.Tiers != nil && len(o.Tiers) == 0 {
		// The same refusal, and for the same reason, UpdateSettings gives
		// an explicitly emptied global chain: in the config file an empty
		// tiers list is indistinguishable from an absent key and reads as
		// the default policy, which is the fail-safe reading for a file
		// and the opposite of what "I removed every tier" means coming
		// from a form.
		return BackupSetRetention{}, fmt.Errorf("%w: retention.tiers must name at least one tier; an empty chain is not \"keep nothing\", "+
			"it reinstates the default daily/weekly/monthly policy, and retention is turned off by not running a retention pass at all", ErrInvalidRequest)
	}
	if o.namesNothing() {
		return BackupSetRetention{}, fmt.Errorf("%w: a backup set's own retention policy has to name a whole chain: "+
			"either a tiers list, or all three of daily_days, weekly_months and monthly_months. "+
			"To return this set to the deployment's policy, clear its override instead of sending an empty one", ErrInvalidRequest)
	}

	return b.writeBackupSetRetention(id, func(bs *config.BackupSet) {
		override := toConfigRetention(o)
		bs.RetentionConfig = &override
	})
}

// ClearBackupSetRetention removes one backup set's own retention policy,
// so it is retained under the deployment's policy again, and leaves no
// residue of the chain it used to declare (this issue's own third
// Given/When/Then).
//
// Clearing is not always the safe direction, and nothing here pretends it
// is: a set whose own chain was WIDER than the deployment's retains less
// after this call, so a later retention apply may delete restore points
// that were being kept. That is a policy change like any other, and it is
// treated the way this codebase already treats turning FR-19's protection
// off through PATCH /settings: the change itself is authenticated and
// CSRF-protected, the deletion it enables stays behind the destructive
// gate at apply time, and the surface in front of the human is what shows
// the two chains before the change is made (see the router's own comment
// on the settings route, and BackupSetRetention.Deployment above, which
// exists so a client can show exactly that).
func (b *BackupService) ClearBackupSetRetention(_ context.Context, id string) (BackupSetRetention, error) {
	return b.writeBackupSetRetention(id, func(bs *config.BackupSet) {
		bs.RetentionConfig = nil
	})
}

// writeBackupSetRetention is the persist-then-hot-reload sequence both
// write methods above share: re-read the file fresh rather than trusting
// the running in-memory copy, apply the change, encode the bytes BEFORE
// config.Validate resolves defaults in place, resolve the validator
// catalog before the write so the only step after it cannot fail, then
// one atomic state.Store so no concurrent reader ever sees a torn
// {inner, revision} pair.
//
// Every step and every reason is CreateBackupSet's, documented in full in
// backupsets.go's package doc and repeated in SetBackupSetEnabled and
// UpdateSettings. This is a fourth copy of that sequence and it should
// not stay one: SetBackupSetEnabled, SetBackupSetReadOnly and
// UpdateSettings can all be expressed as a mutate function handed to this
// helper. Doing that conversion here would have rewritten three functions
// that two other in-flight branches are editing, so the consolidation is
// left as its own change with nothing else in it; this helper is already
// shaped to receive them.
func (b *BackupService) writeBackupSetRetention(id string, mutate func(*config.BackupSet)) (BackupSetRetention, error) {
	if b.configPath == "" {
		return BackupSetRetention{}, ErrConfigNotFileBacked
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return BackupSetRetention{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return BackupSetRetention{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	found := false
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name != setName {
				continue
			}
			mutate(&cfg.Sources[i].BackupSets[j])
			found = true
		}
	}
	if !found {
		return BackupSetRetention{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	// Encoded before Validate, which resolves defaults in place; see
	// UpdateSettings' own comment for the full reasoning and for what an
	// unrelated edit would otherwise silently freeze into the file.
	//
	// It matters more than usual here. config.Validate resolves every
	// set's Retention from its override, and a marshal afterwards would
	// write the RESOLVED chain back into the operator's own override
	// block: an override that named three scalars would come back as a
	// tiers list, and one that omitted the timezone would gain this
	// deployment's timezone as an explicit key, which stops it tracking a
	// later change to the deployment's calendar. Retention is resolved
	// into BackupSet.Retention (yaml:"-"), never into RetentionConfig, so
	// the file keeps exactly what was submitted.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return BackupSetRetention{}, fmt.Errorf("service: encoding configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		// Safe to echo back to an API caller for exactly the reason
		// CreateBackupSet gives for the identical line: a
		// config.ValidationError's text is built from this package's own
		// field descriptions and the caller's own submitted values, never
		// from a state or rclone error string. This is the whole refusal
		// surface for a bad per-set policy, and it is the config layer's
		// own words rather than a second wording of them.
		return BackupSetRetention{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		return BackupSetRetention{}, err
	}

	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		return BackupSetRetention{}, fmt.Errorf("service: persisting configuration: %w", err)
	}

	applyValidators()

	prevInner := b.state.Load().inner
	newInner := app.New(cfg, b.journal, prevInner.Transport, b.logger)
	if !newInner.AdoptAlerts(prevInner.Alerts) && b.alertSink != nil {
		newInner.EnableAlerts(sinkAdapter{sink: b.alertSink})
	}
	b.state.Store(&configState{inner: newInner, revision: computeConfigRevision(cfg)})

	bs, _ := lookupBackupSet(cfg, sourceName, setName)
	return toBackupSetRetention(sourceName, cfg.Retention, bs), nil
}

// lookupBackupSet is findBackupSet with the "was it there at all"
// question answered rather than folded into a zero value. Retention reads
// need the difference: a zero config.BackupSet has a nil RetentionConfig,
// which is indistinguishable from a real set that inherits, so a caller
// that treated "not found" as a zero set would report a confident
// "retained under the deployment's policy" for a backup set that does not
// exist.
func lookupBackupSet(cfg *config.Config, sourceName, name string) (config.BackupSet, bool) {
	for _, src := range cfg.Sources {
		if src.Name != sourceName {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == name {
				return bs, true
			}
		}
	}
	return config.BackupSet{}, false
}

// toBackupSetRetention projects one validated backup set's retention
// answer onto the boundary shape.
//
// bs.Retention (resolved) rather than bs.RetentionConfig is what
// Effective reports, on the same before/after-Validate discipline every
// other consumer of that field follows, so a caller never has to ask
// whether inheritance has happened yet.
func toBackupSetRetention(sourceName string, deployment config.Retention, bs config.BackupSet) BackupSetRetention {
	out := BackupSetRetention{
		BackupSetID: sourceName + "/" + bs.Name,
		IsOverride:  bs.RetentionIsOverride(),
		Effective:   toRetentionSettings(bs.Retention),
		Deployment:  toRetentionSettings(deployment),
	}
	if bs.RetentionConfig != nil {
		override := toRetentionOverride(*bs.RetentionConfig)
		out.Override = &override
	}
	return out
}

// toRetentionOverride projects a raw config.Retention override onto the
// boundary shape WITHOUT resolving anything: the three scalars stay
// scalars, an omitted timezone stays empty, and a nil protection stays
// nil. That is the point of this projection as opposed to
// toRetentionSettings beside it, which resolves everything: this one
// answers "what does the file say", and an edit form that resolved it
// would turn every inherited field into an explicit one the moment
// somebody saved.
func toRetentionOverride(r config.Retention) RetentionOverride {
	o := RetentionOverride{
		Timezone:      r.Timezone,
		WeekStartsOn:  r.WeekStartsOn,
		DailyDays:     r.DailyDays,
		WeeklyMonths:  r.WeeklyMonths,
		MonthlyMonths: r.MonthlyMonths,
	}
	if len(r.Tiers) > 0 {
		o.Tiers = toRetentionTiers(r.Tiers)
	}
	if r.ProtectLastKnownGood != nil {
		protect := *r.ProtectLastKnownGood
		o.ProtectLastKnownGood = &protect
	}
	return o
}

// toConfigRetention is toRetentionOverride's inverse: the submitted
// policy, unresolved, exactly as it will be written into config.yaml.
//
// It fills in no default and completes no half chain. Whether what comes
// out of here is a whole policy is config.Validate's question, asked over
// the whole config a few lines after this runs, and answering any part of
// it here would be the second validation path this issue exists to avoid.
func toConfigRetention(o RetentionOverride) config.Retention {
	r := config.Retention{
		Timezone:      o.Timezone,
		WeekStartsOn:  o.WeekStartsOn,
		DailyDays:     o.DailyDays,
		WeeklyMonths:  o.WeeklyMonths,
		MonthlyMonths: o.MonthlyMonths,
	}
	for _, t := range o.Tiers {
		r.Tiers = append(r.Tiers, config.RetentionTier{
			Name:        t.Name,
			Granularity: t.Granularity,
			PeriodDays:  t.PeriodDays,
			Keep:        t.Keep,
			WindowUnit:  t.WindowUnit,
			// Carried through for the reason RetentionTier.Medium's own
			// doc gives on the settings path: this assignment replaces a
			// whole chain, so a field this projection drops is a field
			// the save deletes from the operator's file.
			Medium: t.Medium,
		})
	}
	if o.ProtectLastKnownGood != nil {
		protect := *o.ProtectLastKnownGood
		r.ProtectLastKnownGood = &protect
	}
	return r
}
