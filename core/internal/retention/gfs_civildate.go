package retention

import "time"

// gfsCivilDate is a calendar date (year, month, day) with no time-of-day
// or time zone attached.
//
// GFSDecide touches the real, configured timezone exactly once per record:
// converting its DiscoveredAt instant into the calendar date it fell on
// there (gfsCivilDateIn). Every calculation after that point, comparing
// dates, stepping by a day, stepping by a whole month, finding a week's
// start, is pure calendar arithmetic over (year, month, day) tuples,
// resolved through Go's proleptic Gregorian calendar (time.Date's own
// normalization) rather than through time.Duration.
//
// That split is deliberate, not cosmetic. Duration arithmetic ("subtract
// 24 hours", "subtract N*24 hours") is only correct if every day is
// exactly 24 hours long, which is false on a DST transition day (23 or 25
// hours in whatever zone is configured): a day-count window computed that
// way can silently gain or lose an hour right at its boundary, on exactly
// the kind of day this package is required to be tested against. Calendar
// arithmetic performed a day, or a whole calendar month, at a time has
// neither problem, because it is expressed in the same units the retention
// policy itself is: DST can only ever affect which single calendar date an
// instant falls on (gfsCivilDateIn's job), never how two dates compare, or
// how a date is stepped or bucketed afterward (every method below).
type gfsCivilDate struct {
	Year  int
	Month time.Month
	Day   int
}

// gfsCivilDateIn returns the calendar date t falls on in loc.
func gfsCivilDateIn(t time.Time, loc *time.Location) gfsCivilDate {
	y, m, d := t.In(loc).Date()
	return gfsCivilDate{Year: y, Month: m, Day: d}
}

// utc renders c as midnight UTC. UTC, rather than the record's real
// timezone, is deliberate here: c is already a bare calendar date by the
// time any method below runs, and grounding pure date arithmetic in a
// fixed, DST-free zone keeps a transition in the *configured* timezone
// from ever influencing arithmetic that has nothing to do with it.
func (c gfsCivilDate) utc() time.Time {
	return time.Date(c.Year, c.Month, c.Day, 0, 0, 0, 0, time.UTC)
}

func gfsCivilDateFromUTC(t time.Time) gfsCivilDate {
	y, m, d := t.Date()
	return gfsCivilDate{Year: y, Month: m, Day: d}
}

// addDays returns the date n calendar days after c (n may be negative).
// Crossing a month or year boundary, including Feb 29 in a leap year, is
// handled by time.Date's own normalization.
func (c gfsCivilDate) addDays(n int) gfsCivilDate {
	return gfsCivilDateFromUTC(c.utc().AddDate(0, 0, n))
}

// addMonths returns the date n calendar months after c (n may be
// negative). Callers that want a whole-month offset should call this on a
// date already normalized to the 1st via firstOfMonth: adding months to
// day 1 can never overflow into the next month, since every month has a
// 1st. That is what makes firstOfMonth().addMonths(...) immune to the
// classic day-of-month overflow (e.g. "May 31 minus one month": naively
// applied to the day itself, Go's calendar would resolve "April 31" to
// May 1, silently narrowing a retention window by a full month right when
// it matters most). See TestGFSWeeklyWindowSurvivesMonthEndOverflow.
func (c gfsCivilDate) addMonths(n int) gfsCivilDate {
	return gfsCivilDateFromUTC(c.utc().AddDate(0, n, 0))
}

// firstOfMonth returns the 1st of c's month.
func (c gfsCivilDate) firstOfMonth() gfsCivilDate {
	return gfsCivilDate{Year: c.Year, Month: c.Month, Day: 1}
}

// weekStart returns the date of the most recent occurrence of start on or
// before c (c itself, if c already falls on start).
func (c gfsCivilDate) weekStart(start time.Weekday) gfsCivilDate {
	weekday := c.utc().Weekday()
	offset := (int(weekday) - int(start) + 7) % 7
	return c.addDays(-offset)
}

// before, after and compare order two calendar dates. This is a plain
// tuple comparison, not a conversion to time.Time and an instant
// comparison: (Year, Month, Day) already carries exactly the order
// chronological dates do for every value this package ever constructs
// (time.Date's normalization guarantees each gfsCivilDate names exactly
// one valid Gregorian date), so no time zone needs to be involved to
// compare two of them.
func (c gfsCivilDate) before(o gfsCivilDate) bool { return c.compare(o) < 0 }
func (c gfsCivilDate) after(o gfsCivilDate) bool  { return c.compare(o) > 0 }

func (c gfsCivilDate) compare(o gfsCivilDate) int {
	switch {
	case c.Year != o.Year:
		return c.Year - o.Year
	case c.Month != o.Month:
		return int(c.Month) - int(o.Month)
	default:
		return c.Day - o.Day
	}
}

// firstOfQuarter returns the 1st of January, April, July or October,
// whichever calendar quarter c falls in.
func (c gfsCivilDate) firstOfQuarter() gfsCivilDate {
	m := time.Month(((int(c.Month)-1)/3)*3 + 1)
	return gfsCivilDate{Year: c.Year, Month: m, Day: 1}
}

// firstOfHalfYear returns the 1st of January or the 1st of July,
// whichever calendar half c falls in.
//
// A calendar half, rather than "six months back from wherever we are",
// is what makes a semi-annual tier's buckets stable: two runs a week
// apart put the same backup in the same bucket, and an operator can say
// which backup a semi-annual policy is holding without knowing what day
// the last retention pass ran.
func (c gfsCivilDate) firstOfHalfYear() gfsCivilDate {
	m := time.January
	if c.Month >= time.July {
		m = time.July
	}
	return gfsCivilDate{Year: c.Year, Month: m, Day: 1}
}

// firstOfYear returns the 1st of January of c's year.
func (c gfsCivilDate) firstOfYear() gfsCivilDate {
	return gfsCivilDate{Year: c.Year, Month: time.January, Day: 1}
}

// gfsPeriodEpoch anchors the custom "every N days" granularity.
//
// A custom period has no calendar landmark to hang a bucket on the way a
// month has its 1st, so it needs a fixed reference point, and the one
// thing it must not be is "today": buckets counted back from the current
// date shift by a day every day, which would move a backup from one
// bucket to the next between two runs and make the tier's selection
// depend on when the pass happened to run. That is exactly the
// determinism this package's doc rules out. 1970-01-01 is arbitrary but
// permanent, which is the only property that matters here.
var gfsPeriodEpoch = gfsCivilDate{Year: 1970, Month: time.January, Day: 1}

// daysSinceEpoch returns the number of whole days between gfsPeriodEpoch
// and c (negative for dates before it). This is calendar arithmetic
// grounded in UTC midnights, so it is unaffected by any DST transition in
// the configured retention timezone; see this file's type doc.
func (c gfsCivilDate) daysSinceEpoch() int {
	return int(c.utc().Sub(gfsPeriodEpoch.utc()).Hours() / 24)
}

// periodStart returns the first day of the n-day period c falls in,
// counting periods from gfsPeriodEpoch. n must be positive; callers
// resolve a non-positive period before reaching here (see gfs.go's
// gfsResolveTier).
//
// Euclidean division, not Go's truncating division, is deliberate: a date
// before the epoch has a negative day count, and truncation there would
// round toward zero and hand two adjacent pre-epoch dates the same bucket
// as two post-epoch ones. No production config dates a backup to the
// 1960s, but a bucket function that is wrong on half its domain is not
// something to leave sitting in a code path that deletes files.
func (c gfsCivilDate) periodStart(n int) gfsCivilDate {
	days := c.daysSinceEpoch()
	idx := days / n
	if days%n < 0 {
		idx--
	}
	return gfsPeriodEpoch.addDays(idx * n)
}
