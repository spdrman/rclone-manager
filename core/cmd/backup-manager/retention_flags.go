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
package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

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
	return &retentionFlags{
		fs:            fs,
		timezone:      fs.String("timezone", "", "override retention.timezone (an IANA name; unset leaves the loaded config's value)"),
		weekStartsOn:  fs.String("week-starts-on", "", "override retention.week_starts_on (a weekday name; unset leaves the loaded config's value)"),
		dailyDays:     fs.Int("daily-days", 0, "override retention.daily_days (unset, or 0, leaves the loaded config's value)"),
		weeklyMonths:  fs.Int("weekly-months", 0, "override retention.weekly_months (unset, or 0, leaves the loaded config's value)"),
		monthlyMonths: fs.Int("monthly-months", 0, "override retention.monthly_months (unset, or 0, leaves the loaded config's value)"),
		tiers:         tiers,
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
