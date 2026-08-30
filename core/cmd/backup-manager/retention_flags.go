package main

import (
	"flag"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// retentionFlags holds the flag.FlagSet variables backing the six
// FR-18/FR-19 retention override flags `backup-manager retention` accepts
// (issue #111, B3.6). Each one is optional: an operator who passes none of
// them gets exactly today's behavior, the loaded config file's own
// resolved retention policy, untouched.
type retentionFlags struct {
	fs *flag.FlagSet

	timezone      *string
	weekStartsOn  *string
	dailyDays     *int
	weeklyMonths  *int
	monthlyMonths *int
	protect       *bool
}

// registerRetentionFlags adds the six retention override flags to fs. It
// does not parse anything; call fs.Parse and then resolveRetentionFlags
// once parsing has happened, so protectLastKnownGood's "was --
// protect-last-known-good actually passed" question can be answered from
// fs.Visit rather than guessed at from the flag's own zero value.
func registerRetentionFlags(fs *flag.FlagSet) *retentionFlags {
	return &retentionFlags{
		fs:            fs,
		timezone:      fs.String("timezone", "", "override retention.timezone (an IANA name; unset leaves the loaded config's value)"),
		weekStartsOn:  fs.String("week-starts-on", "", "override retention.week_starts_on (a weekday name; unset leaves the loaded config's value)"),
		dailyDays:     fs.Int("daily-days", 0, "override retention.daily_days (unset, or 0, leaves the loaded config's value)"),
		weeklyMonths:  fs.Int("weekly-months", 0, "override retention.weekly_months (unset, or 0, leaves the loaded config's value)"),
		monthlyMonths: fs.Int("monthly-months", 0, "override retention.monthly_months (unset, or 0, leaves the loaded config's value)"),
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
// A zero-valued field in o (empty string, 0, nil pointer) is read as
// "this flag was not passed" and leaves r's already-resolved value alone,
// per registerRetentionFlags' own flag defaults: r is expected to be a
// backup-set's already-loaded, already-validated retention policy (e.g.
// config.LoadAndValidate's own output), so what is "left alone" is never
// an unresolved zero value, it is whatever the file (or that file's own
// documented defaults) already resolved to. This is what keeps "an
// operator who touches no new surface gets exactly today's behavior"
// true: applyRetentionOverrides with a zero-valued o is a no-op on an
// already-resolved r, by construction, not by a special case here.
func applyRetentionOverrides(r *config.Retention, o retentionOverrides) error {
	if o.timezone != "" {
		r.Timezone = o.timezone
	}
	if o.weekStartsOn != "" {
		r.WeekStartsOn = o.weekStartsOn
	}
	if o.dailyDays != 0 {
		r.DailyDays = o.dailyDays
	}
	if o.weeklyMonths != 0 {
		r.WeeklyMonths = o.weeklyMonths
	}
	if o.monthlyMonths != 0 {
		r.MonthlyMonths = o.monthlyMonths
	}
	if o.protectLastKnownGood != nil {
		r.ProtectLastKnownGood = o.protectLastKnownGood
	}
	return config.ValidateRetention(r)
}
