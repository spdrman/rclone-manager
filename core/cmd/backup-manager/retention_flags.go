package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// The retention override flags, from the flag.FlagSet variables that back
// them to the resolved policy they fold onto a loaded config.
//
// Three steps rather than one, and the middle one is why. Declaring, then
// resolving into an explicit "what the operator actually said" value, then
// applying, is what keeps an unset flag distinguishable from a flag set to
// its zero value: the config layer reads several of these zeros as "the
// operator did not say", so collapsing the two would silently turn
// --protect-last-known-good=false into no opinion at all.
//
// Nothing here validates. Every value is handed to the config layer
// unparsed beyond the split, so a mistake typed at a flag is refused for the
// identical reason, in the identical words, as the same mistake written into
// the YAML file. An operator who fixed one by reading its message should not
// meet a different message from the other.

// retentionFlags holds the flag.FlagSet variables backing the FR-18/FR-19
// retention override flags `backup-manager retention` accepts (issue #111,
// B3.6, extended by #156, B3.8). Each one is optional: an operator who
// passes none of them gets exactly today's behavior, the loaded config
// file's own resolved retention policy, untouched.
type retentionFlags struct {
	fs *flag.FlagSet

	timezone      *string
	weekStartsOn  *string
	dailyDays     *int
	weeklyMonths  *int
	monthlyMonths *int
	tiers         *retentionTierFlag
	tierMediums   *retentionTierMediumFlag
	protect       *bool
}

// retentionTierFlag collects a repeatable -tier flag into an ordered chain.
//
// The spec syntax is name:granularity:keep, with two optional suffixes:
// a window unit (name:granularity:keep:window_unit) and, for the custom
// granularity, its length written into the granularity itself as
// "days=14". Every value is handed to config.ValidateRetention unparsed
// beyond the split, so a mistake in a flag is refused for the identical
// reason and with the identical text the same mistake in the YAML file
// would be.
type retentionTierFlag struct {
	tiers []config.RetentionTier
}

func (f *retentionTierFlag) String() string {
	names := make([]string, len(f.tiers))
	for i, t := range f.tiers {
		names[i] = t.Name
	}
	return strings.Join(names, ",")
}

func (f *retentionTierFlag) Set(spec string) error {
	parts := strings.Split(spec, ":")
	if len(parts) < 3 || len(parts) > 4 {
		return fmt.Errorf("tier %q must be written name:granularity:keep, optionally name:granularity:keep:window_unit", spec)
	}
	t := config.RetentionTier{Name: parts[0], Granularity: parts[1]}
	if len(parts) == 4 {
		t.WindowUnit = parts[3]
	}

	// "days=14" carries the custom period's length. Splitting it out here
	// rather than adding a fifth colon-separated position keeps the common
	// three-field form readable and keeps period_days attached to the one
	// granularity it belongs to.
	if g, days, ok := strings.Cut(t.Granularity, "="); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return fmt.Errorf("tier %q: period length %q is not a number", spec, days)
		}
		t.Granularity, t.PeriodDays = g, n
	}

	keep, err := strconv.Atoi(parts[2])
	if err != nil {
		return fmt.Errorf("tier %q: keep %q is not a number", spec, parts[2])
	}
	t.Keep = keep

	f.tiers = append(f.tiers, t)
	return nil
}

// tierMediumOverride is one --tier-medium NAME=MEDIUM_ID pairing: the
// name of a tier this command line supplied, and the storage medium its
// artifacts live on.
//
// The medium is carried as the operator typed it and is never inspected
// here. Whether it is spelled legally, whether it is the reserved local
// id, and whether any storage_mediums entry declares it are all decided
// by the config layer, in the identical words the same mistake written
// into config.yaml is refused with. A copy of any of those rules here
// would be a second rule free to disagree with the first.
type tierMediumOverride struct {
	tier   string
	medium string
}

// retentionTierMediumFlag collects the repeatable -tier-medium flag.
//
// A repeatable flag rather than a fifth colon-separated position on
// -tier, for the reason retentionTierFlag.Set already gives about
// "days=14": the three-field form stays readable, and the value stays
// attached to the thing it belongs to.
//
// Set decides only what is decidable from the one string it is handed:
// the shape, and that the same tier is not given two destinations. It
// cannot decide whether a -tier of that name was given, because flags
// are parsed in the order they were typed and "-tier-medium daily=x
// -tier daily:day:7" is a legal command line. That check is
// applyRetentionOverrides', which sees the whole resolved set at once.
type retentionTierMediumFlag struct {
	pairs []tierMediumOverride
}

func (f *retentionTierMediumFlag) String() string {
	specs := make([]string, len(f.pairs))
	for i, p := range f.pairs {
		specs[i] = p.tier + "=" + p.medium
	}
	return strings.Join(specs, ",")
}

func (f *retentionTierMediumFlag) Set(spec string) error {
	name, medium, ok := strings.Cut(spec, "=")
	if !ok {
		return fmt.Errorf("tier medium %q must be written NAME=MEDIUM_ID, naming a tier this command line's -tier flags gave and the storage medium its artifacts live on", spec)
	}
	if name == "" {
		return fmt.Errorf("tier medium %q names no tier before the =", spec)
	}
	if medium == "" {
		// Not read as "put this tier on local". Local is spelled by
		// leaving the medium out entirely, exactly as it is in the config
		// file (config.RetentionTier.Medium's own doc), so an empty value
		// here is a half-typed flag rather than a second spelling of the
		// default.
		return fmt.Errorf("tier medium %q names no medium after the =; a tier lives on the local backup root by not naming a medium at all, so leave the whole flag out rather than passing it empty", spec)
	}
	for _, p := range f.pairs {
		if p.tier == name {
			return fmt.Errorf("tier medium %q gives tier %q a second destination; it already has %q, and a tier's artifacts live in one place", spec, name, p.medium)
		}
	}
	f.pairs = append(f.pairs, tierMediumOverride{tier: name, medium: medium})
	return nil
}

// registerRetentionFlags adds the retention override flags to fs. It
// does not parse anything; call fs.Parse and then resolveRetentionFlags
// once parsing has happened, so protectLastKnownGood's "was --
// protect-last-known-good actually passed" question can be answered from
// fs.Visit rather than guessed at from the flag's own zero value.
func registerRetentionFlags(fs *flag.FlagSet) *retentionFlags {
	tiers := &retentionTierFlag{}
	fs.Var(tiers, "tier", "append one tier to an FR-18 retention chain, written name:granularity:keep[:window_unit] "+
		"(granularity: day, week, month, quarter, half_year, year, or days=N for a custom period). Repeatable, and it "+
		"replaces the whole chain: -tier cannot be combined with -daily-days, -weekly-months or -monthly-months, which "+
		"are sugar for the default chain. Unset leaves the loaded config's own policy")
	tierMediums := &retentionTierMediumFlag{}
	fs.Var(tierMediums, "tier-medium", "name the storage medium one -tier lives on, written NAME=MEDIUM_ID (EPIC E, FR-27). "+
		"Repeatable, at most once per tier, and refused when no -tier of that name was given. A tier with no -tier-medium "+
		"lives on the backup set's own local path, which is how local is spelled in the config file too. The id is handed "+
		"to the config layer unchecked, so one no storage_mediums entry declares is refused in the same words config.yaml "+
		"would refuse it in")
	return &retentionFlags{
		fs:            fs,
		timezone:      fs.String("timezone", "", "override retention.timezone (an IANA name; unset leaves the loaded config's value)"),
		weekStartsOn:  fs.String("week-starts-on", "", "override retention.week_starts_on (a weekday name; unset leaves the loaded config's value)"),
		dailyDays:     fs.Int("daily-days", 0, "override retention.daily_days (unset, or 0, leaves the loaded config's value)"),
		weeklyMonths:  fs.Int("weekly-months", 0, "override retention.weekly_months (unset, or 0, leaves the loaded config's value)"),
		monthlyMonths: fs.Int("monthly-months", 0, "override retention.monthly_months (unset, or 0, leaves the loaded config's value)"),
		tiers:         tiers,
		tierMediums:   tierMediums,
		// Default true is irrelevant unless the flag is actually passed:
		// resolveRetentionFlags below only ever reads this pointer's value
		// when fs.Visit confirms the flag was set on the command line, so
		// an operator who never mentions this flag gets no override
		// either way, exactly like the other five.
		protect: fs.Bool("protect-last-known-good", true, "override retention.protect_last_known_good; pass =false to explicitly disable FR-19 protection, which LastKnownGoodDecide treats as a materially more dangerous configuration (unset leaves the loaded config's value)"),
	}
}

// retentionOverrides is what registerRetentionFlags' six flags resolve to
// once fs has been parsed: exactly the fields an operator actually named
// on the command line. protectLastKnownGood is a *bool, mirroring
// config.Retention.ProtectLastKnownGood's own reason for being a pointer
// (see config.go's doc): "the flag was never passed" and "the flag was
// passed as =false" have to stay distinguishable inputs, the same
// distinction the YAML file's absent-key-vs-explicit-false already
// preserves, or this surface would flatten exactly the nuance issue #111
// calls out as the one a naive "empty = default" reading loses.
type retentionOverrides struct {
	timezone             string
	weekStartsOn         string
	dailyDays            int
	weeklyMonths         int
	monthlyMonths        int
	tiers                []config.RetentionTier
	tierMediums          []tierMediumOverride
	protectLastKnownGood *bool
}

// resolveRetentionFlags reads rf's parsed flag values into a
// retentionOverrides, using fs.Visit to tell "--protect-last-known-good
// was never passed" apart from "--protect-last-known-good=false was
// passed," which a bare *bool's zero value cannot do on its own (see the
// retentionOverrides doc). Call this only after fs.Parse has already run.
func resolveRetentionFlags(rf *retentionFlags) retentionOverrides {
	o := retentionOverrides{
		timezone:      *rf.timezone,
		weekStartsOn:  *rf.weekStartsOn,
		dailyDays:     *rf.dailyDays,
		weeklyMonths:  *rf.weeklyMonths,
		monthlyMonths: *rf.monthlyMonths,
		tiers:         rf.tiers.tiers,
		tierMediums:   rf.tierMediums.pairs,
	}
	rf.fs.Visit(func(f *flag.Flag) {
		if f.Name == "protect-last-known-good" {
			v := *rf.protect
			o.protectLastKnownGood = &v
		}
	})
	return o
}

// applyRetentionOverrides folds o onto r in place and then validates the
// result through config.ValidateRetention, the exact function
// config.Validate itself uses for the YAML file's own retention block, so
// a CLI-provided value is accepted or refused for the identical reason
// the same value would be in the config file.
//
// A zero-valued field in o (empty string, 0, nil pointer, empty slice) is read as
// "this flag was not passed" and leaves r's already-resolved value alone,
// per registerRetentionFlags' own flag defaults: r is expected to be a
// backup-set's already-loaded, already-validated retention policy (e.g.
// config.LoadAndValidate's own output), so what is "left alone" is never
// an unresolved zero value, it is whatever the file (or that file's own
// documented defaults) already resolved to. This is what keeps "an
// operator who touches no new surface gets exactly today's behavior"
// true: applyRetentionOverrides with a zero-valued o is a no-op on an
// already-resolved r, by construction, not by a special case here.
//
// Folding r is only half the step for a caller holding a whole
// *config.Config. Since issue #333 every retention decision reads a backup
// set's own resolved config.BackupSet.Retention, which config.Validate
// computed from the global policy as it stood at load, so a caller that
// folds onto cfg.Retention and stops has changed nothing any decision
// reads. cmdRetention re-runs cfg.Validate after this returns; see its own
// comment for why that is the whole re-resolution step and what it means
// for a set that declares its own policy.
//
// Folding is all-or-nothing: a refused override leaves *r exactly as the
// caller passed it. That matters most for the two mutual-exclusion
// refusals below, which used to return with the scalars already written,
// leaving the caller holding a policy that carried both spellings at once
// (a tiers list plus a scalar), which is the very state being refused and
// which config.EffectiveTiers resolves silently rather than refusing.
// Every caller aborts on an error today, so nothing observes it; this
// policy decides what gets deleted, and the next caller should not have
// to know that.
func applyRetentionOverrides(r *config.Retention, o retentionOverrides) error {
	// Both checks are decided against the caller's inputs alone, before
	// anything is written, so neither depends on a value the folding
	// below produces.
	scalars := o.dailyDays != 0 || o.weeklyMonths != 0 || o.monthlyMonths != 0
	switch {
	case len(o.tiers) > 0 && scalars:
		return fmt.Errorf("-tier cannot be combined with -daily-days, -weekly-months or -monthly-months: those three are sugar for the default chain, so pass one spelling or the other")
	case len(o.tiers) == 0 && len(r.Tiers) > 0 && scalars:
		return fmt.Errorf("-daily-days, -weekly-months and -monthly-months are sugar for the default chain and cannot override a config file that already defines retention.tiers; pass -tier to replace the chain instead")
	}
	if err := tierMediumsNameAGivenTier(o); err != nil {
		return err
	}

	// Folded onto a copy, so a refusal from config.ValidateRetention
	// below leaves the caller's policy untouched too. The copy is shallow
	// and shares the Tiers backing array, which is safe because nothing
	// past here writes through it: a -tier override allocates its own
	// slice, and ValidateRetention only reads the tiers it checks.
	next := *r
	if o.timezone != "" {
		next.Timezone = o.timezone
	}
	if o.weekStartsOn != "" {
		next.WeekStartsOn = o.weekStartsOn
	}
	if o.dailyDays != 0 {
		next.DailyDays = o.dailyDays
	}
	if o.weeklyMonths != 0 {
		next.WeeklyMonths = o.weeklyMonths
	}
	if o.monthlyMonths != 0 {
		next.MonthlyMonths = o.monthlyMonths
	}
	// A -tier chain replaces the policy's chain outright rather than
	// merging into it, and clears the three scalars it supersedes. Merging
	// would need a rule for what a partially-overridden chain means, and
	// there is no honest one: an operator naming two tiers on the command
	// line is describing the whole policy for this run, not editing three
	// of the file's five links. Clearing the scalars is what keeps that
	// well-formed, since config.ValidateRetention (called below, the same
	// function the YAML file goes through) refuses a Retention that
	// carries both spellings at once.
	if len(o.tiers) > 0 {
		next.Tiers = append([]config.RetentionTier(nil), o.tiers...)
		next.DailyDays, next.WeeklyMonths, next.MonthlyMonths = 0, 0, 0
		// -tier-medium writes onto the chain -tier just supplied, never
		// onto the file's own, which is why it lives inside this branch:
		// with no -tier there is no chain of this command line's to put a
		// destination on, and the refusal above has already said so.
		//
		// The medium goes in exactly as typed. config.ValidateRetention,
		// three lines down, is what refuses the reserved local id and a
		// name that is not lower_snake_case, and cmdRetention's own
		// cfg.Validate is what refuses one no storage_mediums entry
		// declares. Every one of those messages is the config file's own.
		for _, tm := range o.tierMediums {
			for i := range next.Tiers {
				if next.Tiers[i].Name == tm.tier {
					next.Tiers[i].Medium = tm.medium
				}
			}
		}
	}
	if o.protectLastKnownGood != nil {
		next.ProtectLastKnownGood = o.protectLastKnownGood
	}
	if err := config.ValidateRetention(&next); err != nil {
		return err
	}
	*r = next
	return nil
}

// overridden reports whether any of the six FR-18/FR-19 flags was passed,
// which is what tells a hypothetical preview apart from the deployment's
// own (issue #544).
//
// It reads the same resolved value applyRetentionOverrides folds, rather
// than the flag set, so the two can never disagree about what "an operator
// passed nothing" means: applyRetentionOverrides treats every zero value
// as "not passed", and so does this.
func overridden(o retentionOverrides) bool {
	return o.timezone != "" ||
		o.weekStartsOn != "" ||
		o.dailyDays != 0 ||
		o.weeklyMonths != 0 ||
		o.monthlyMonths != 0 ||
		len(o.tiers) > 0 ||
		len(o.tierMediums) > 0 ||
		o.protectLastKnownGood != nil
}

// tierMediumsNameAGivenTier refuses a -tier-medium that is attached to
// nothing, before anything is folded.
//
// Two shapes, and they are different mistakes. With no -tier at all there
// is no chain of this command line's to put a destination on: the
// deployment's own chain already carries its destinations, and quietly
// writing one onto it would make this preview about a policy nobody
// wrote. With a chain that simply has no tier of that name, the likeliest
// cause is a typo in the name, and a typo silently ignored is exactly the
// failure this whole flag exists to end: the operator would read a
// confident all-local plan for the tier they meant.
//
// Decided against o alone, like the two mutual-exclusion refusals above
// it, so applyRetentionOverrides stays all-or-nothing: a refused override
// leaves the caller's policy exactly as it was passed.
func tierMediumsNameAGivenTier(o retentionOverrides) error {
	if len(o.tierMediums) == 0 {
		return nil
	}
	if len(o.tiers) == 0 {
		return fmt.Errorf("-tier-medium names the destination of a tier this command line supplied, and no -tier was given; pass the whole chain with -tier, or leave both out to preview the deployment's own policy, which already carries its own destinations")
	}
	names := make([]string, 0, len(o.tiers))
	given := make(map[string]bool, len(o.tiers))
	for _, t := range o.tiers {
		if !given[t.Name] {
			names = append(names, t.Name)
		}
		given[t.Name] = true
	}
	for _, tm := range o.tierMediums {
		if !given[tm.tier] {
			return fmt.Errorf("-tier-medium %s=%s names no tier this command line gave; the -tier flags gave %s", tm.tier, tm.medium, strings.Join(names, ", "))
		}
	}
	return nil
}
