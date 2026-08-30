package retention

import (
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// This file turns FR-18's configured retention chain into the
// {tier, inSpan, bucket} triples GFSDecide loops over.
//
// The loop itself has not changed and neither has the formula: every tier
// still selects the newest valid artifact in each of its own buckets
// inside its own look-back window, KEEP is still the union of those
// selections (plus FR-19's protected term, composed later in
// lastknowngood.go), and DELETE is still everything the union did not
// claim. What this file adds is that the number of tiers, and the
// granularity each one buckets at, now come from configuration instead of
// from a three-element literal.
//
// Two things follow from that and are worth stating where the code lives,
// because both bear directly on what gets deleted:
//
//   - A chain need not be contiguous. daily plus annual with nothing in
//     between is a legal policy, and every artifact in the gap is a delete
//     candidate. Nothing here tries to be helpful and widen a window to
//     close a hole an operator left; that would be this code overruling
//     the policy it was handed.
//   - A tier that cannot be evaluated is an error, never an empty
//     selection. See gfsResolveTier.

// gfsBoundTier is one link of the chain, resolved against a fixed "today"
// and a fixed week-start weekday, so neither has to be threaded through
// the two closures on every record.
type gfsBoundTier struct {
	tier   GFSTier
	inSpan func(gfsCivilDate) bool
	bucket func(gfsCivilDate) gfsCivilDate
}

// gfsResolveChain binds every tier in chain against today.
func gfsResolveChain(chain []config.RetentionTier, today gfsCivilDate, weekStartDay time.Weekday) ([]gfsBoundTier, error) {
	out := make([]gfsBoundTier, 0, len(chain))
	for i, t := range chain {
		bound, err := gfsResolveTier(t, today, weekStartDay)
		if err != nil {
			return nil, fmt.Errorf("retention: tiers[%d]: %w", i, err)
		}
		out = append(out, bound)
	}
	return out, nil
}

// gfsResolveTier turns one configured tier into its bucket and window
// functions.
//
// A non-positive Keep resolves to a tier that selects nothing, matching
// what a zero or negative daily_days has always meant (see GFSDecide's
// doc): it is the only way a caller who bypassed config.Validate can spell
// "this tier is off", and reading it as an error would break every such
// caller. Anything else this function cannot make sense of is an error,
// because there is no reading of "granularity: fortnight" under which a
// tier legitimately keeps nothing, and treating it as one would hand FR-20
// a plan to delete backups the operator believes are covered.
func gfsResolveTier(t config.RetentionTier, today gfsCivilDate, weekStartDay time.Weekday) (gfsBoundTier, error) {
	if t.Name == "" {
		return gfsBoundTier{}, fmt.Errorf("a tier must have a name; an unnamed tier cannot be reported against a KEEP verdict")
	}

	bucket, err := gfsBucketFunc(t, weekStartDay)
	if err != nil {
		return gfsBoundTier{}, err
	}

	// The look-back is counted in WindowUnit when one is set, and in the
	// tier's own granularity otherwise. The two differ for exactly one
	// tier in FR-18's default chain, weekly, which buckets by week but
	// looks back over calendar months; without that split the legacy
	// weekly_months key could not be expressed here at all.
	unit := t.WindowUnit
	if unit == "" {
		unit = t.Granularity
	}
	if unit == config.GranularityDays && t.Granularity != config.GranularityDays {
		return gfsBoundTier{}, fmt.Errorf("window_unit %q needs a period length, which only granularity %q carries", unit, config.GranularityDays)
	}
	anchor, err := gfsBucketFunc(config.RetentionTier{Granularity: unit, PeriodDays: t.PeriodDays}, weekStartDay)
	if err != nil {
		return gfsBoundTier{}, fmt.Errorf("window_unit: %w", err)
	}
	step, err := gfsStepFunc(unit, t.PeriodDays)
	if err != nil {
		return gfsBoundTier{}, fmt.Errorf("window_unit: %w", err)
	}

	// Anchor first, then step whole units. Doing it in that order is what
	// makes the arithmetic immune to day-of-month overflow: stepping from
	// the 1st of a month can never land on a day that month does not have,
	// whereas stepping from "the 31st" can and silently narrows a window
	// by a whole period. gfs_civildate.go's addMonths doc has the long
	// version, and TestGFSDecideLongPeriodCalendarCases holds the
	// half-year case to it.
	keep := t.Keep
	return gfsBoundTier{
		tier:   gfsTierName(t.Name),
		bucket: bucket,
		inSpan: func(d gfsCivilDate) bool {
			if keep <= 0 {
				return false
			}
			start := step(anchor(today), -(keep - 1))
			return !d.before(start) && !d.after(today)
		},
	}, nil
}

// gfsBucketFunc maps a calendar date onto the first day of the bucket it
// falls in, for one granularity. Two dates share a bucket exactly when
// this returns the same date for both, which is what makes each bucket
// contribute at most one artifact to KEEP.
func gfsBucketFunc(t config.RetentionTier, weekStartDay time.Weekday) (func(gfsCivilDate) gfsCivilDate, error) {
	switch t.Granularity {
	case config.GranularityDay:
		return func(d gfsCivilDate) gfsCivilDate { return d }, nil
	case config.GranularityWeek:
		return func(d gfsCivilDate) gfsCivilDate { return d.weekStart(weekStartDay) }, nil
	case config.GranularityMonth:
		return func(d gfsCivilDate) gfsCivilDate { return d.firstOfMonth() }, nil
	case config.GranularityQuarter:
		return func(d gfsCivilDate) gfsCivilDate { return d.firstOfQuarter() }, nil
	case config.GranularityHalfYear:
		return func(d gfsCivilDate) gfsCivilDate { return d.firstOfHalfYear() }, nil
	case config.GranularityYear:
		return func(d gfsCivilDate) gfsCivilDate { return d.firstOfYear() }, nil
	case config.GranularityDays:
		n := t.PeriodDays
		if n <= 0 {
			return nil, fmt.Errorf("granularity %q needs a positive period_days (got %d)", config.GranularityDays, n)
		}
		return func(d gfsCivilDate) gfsCivilDate { return d.periodStart(n) }, nil
	case "":
		return nil, fmt.Errorf("granularity must be set")
	default:
		return nil, fmt.Errorf("unknown granularity %q", t.Granularity)
	}
}

// gfsStepFunc returns "move n whole units from this bucket start", for one
// granularity. n is negative when walking a window backward.
//
// Every step is calendar arithmetic (whole days or whole months), never a
// duration: a year is not 365 days, a half-year is not 182, and a day is
// not always 24 hours in the configured timezone. See gfs_civildate.go's
// type doc for why that distinction is the whole reason this package
// carries its own date type.
func gfsStepFunc(granularity string, periodDays int) (func(gfsCivilDate, int) gfsCivilDate, error) {
	switch granularity {
	case config.GranularityDay:
		return func(c gfsCivilDate, n int) gfsCivilDate { return c.addDays(n) }, nil
	case config.GranularityWeek:
		return func(c gfsCivilDate, n int) gfsCivilDate { return c.addDays(7 * n) }, nil
	case config.GranularityMonth:
		return func(c gfsCivilDate, n int) gfsCivilDate { return c.addMonths(n) }, nil
	case config.GranularityQuarter:
		return func(c gfsCivilDate, n int) gfsCivilDate { return c.addMonths(3 * n) }, nil
	case config.GranularityHalfYear:
		return func(c gfsCivilDate, n int) gfsCivilDate { return c.addMonths(6 * n) }, nil
	case config.GranularityYear:
		return func(c gfsCivilDate, n int) gfsCivilDate { return c.addMonths(12 * n) }, nil
	case config.GranularityDays:
		if periodDays <= 0 {
			return nil, fmt.Errorf("granularity %q needs a positive period_days (got %d)", config.GranularityDays, periodDays)
		}
		return func(c gfsCivilDate, n int) gfsCivilDate { return c.addDays(periodDays * n) }, nil
	case "":
		return nil, fmt.Errorf("granularity must be set")
	default:
		return nil, fmt.Errorf("unknown granularity %q", granularity)
	}
}
