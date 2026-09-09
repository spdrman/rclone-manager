package cliecho

import (
	"strings"
	"testing"
	"time"
)

// seconds renders the value of a duration flag, and the only thing that
// makes such a value worth printing is that the binary takes it back.
//
// Both flags it feeds (--stable-for and --stale-after) are declared with
// flag.Duration, so "takes it back" is exactly time.ParseDuration, and
// this asserts the round trip rather than the spelling: parse what was
// printed and require the same number of seconds to come out.
//
// It is a sweep rather than a table because the failure this test was
// written for lived in half the input space and not in the other half. A
// hand-picked table is chosen by whoever wrote the renderer, out of the
// values they were already thinking about, which is how the two values
// this package's own examples used were the two that worked.
func TestSecondsAlwaysParsesBackToTheSameDuration(t *testing.T) {
	// The named cases, so a regression says which shape broke rather than
	// only naming the first integer that failed.
	for _, tc := range []struct {
		in   int
		want string
	}{
		{0, "0s"},
		{1, "1s"},
		{10, "10s"},     // a bare seconds value ending in a zero
		{30, "30s"},     // the one a stale_after patch actually carries
		{90, "1m30s"},   // minutes and seconds
		{300, "5m"},     // a whole number of minutes
		{600, "10m"},    // minutes, ending in a zero
		{3600, "1h"},    // a whole hour
		{3610, "1h10s"}, // an hour with an EMPTY minutes component
		{3660, "1h1m"},  // an hour and a minute, no seconds
		{86400, "24h"},  // a day
		{172800, "48h"}, // the value the examples have always used
		{93784, "26h3m4s"},
	} {
		got := seconds(tc.in)
		if got != tc.want {
			t.Errorf("seconds(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// The property, over every value up to a bit more than a day, plus a
	// scatter of larger ones. A rendered duration that time.ParseDuration
	// refuses is a command an operator pastes and gets exit 2 from, and
	// one it accepts as a DIFFERENT duration is worse: it is a command
	// that runs and does something else.
	check := func(v int) {
		t.Helper()
		got := seconds(v)
		d, err := time.ParseDuration(got)
		if err != nil {
			t.Fatalf("seconds(%d) = %q, which time.ParseDuration refuses (%v). Both flags this feeds are flag.Duration, so a value it refuses is a command line the binary refuses.", v, got, err)
		}
		if want := time.Duration(v) * time.Second; d != want {
			t.Fatalf("seconds(%d) = %q, which parses back as %s rather than %s. A command that runs and means something else is worse than one that will not run.", v, got, d, want)
		}
		if strings.HasSuffix(got, "0m") && got != "0m" || strings.HasSuffix(got, "0s") && got != "0s" {
			// Not a correctness rule, a legibility one: 48h0m0s is what
			// this renderer exists not to print.
			if strings.HasSuffix(got, "m0s") || strings.HasSuffix(got, "h0m") {
				t.Fatalf("seconds(%d) = %q, which spells a zero unit an operator would not type", v, got)
			}
		}
	}
	for v := 0; v <= 90000; v++ {
		check(v)
	}
	for _, v := range []int{100000, 604800, 2592000, 31536000, 315360000} {
		check(v)
	}
}

// The parse test in core/cmd/rbm only ever drives the bodies
// this package's own examples carry, so a duration shape no example uses
// is a shape nothing checks end to end. This is that corpus rule, held
// here where the examples are.
func TestTheExamplesDriveMoreThanOneShapeOfDuration(t *testing.T) {
	shapes := map[string]bool{}
	for _, ex := range Examples() {
		for _, arg := range Echo(ex).Command {
			d, err := time.ParseDuration(arg)
			if err != nil || d <= 0 {
				continue
			}
			switch {
			case !strings.Contains(arg, "h") && !strings.Contains(arg, "m"):
				shapes["bare seconds"] = true
			case strings.HasSuffix(arg, "s") && strings.Contains(arg, "h") && !strings.Contains(arg, "m"):
				shapes["an hour with no minutes"] = true
			}
			if strings.HasSuffix(strings.TrimSuffix(arg, "s"), "0") || strings.HasSuffix(strings.TrimSuffix(arg, "m"), "0") {
				shapes["a trailing zero digit"] = true
			}
		}
	}
	for _, want := range []string{"bare seconds", "an hour with no minutes", "a trailing zero digit"} {
		if !shapes[want] {
			t.Errorf("no example drives a duration with %s, so TestEveryEchoedCommandParses never sees one. The renderer that ate a digit was green for exactly this reason: the corpus visited only the two values that worked.", want)
		}
	}
}
