package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/cliecho"
)

// A gap line is a promise, made to an operator reading a terminal, that
// this binary has no verb for what they just did. Nothing checked it, and
// five of them were wrong at once (issue #599's review):
//
//   - GET /activity said "there is no `activity` verb yet". main.go has
//     dispatched `activity` since #598.
//   - GET /activity/live said "there is no `activity --follow` yet". It
//     ships in the same file.
//   - POST .../retention/apply said "`retention` on a terminal previews
//     and deletes nothing", quoting a usage() line #602 had replaced with
//     `retention apply <source/backup-set> --acknowledge`.
//   - PATCH /settings refused a tier-chain replacement and quoted usage()
//     as its authority, and usage() said the opposite, because #595 had
//     put --policy-file on `settings patch`.
//   - The two edit-hold reads said no verb read or released a hold, and
//     #600 built `backup-set edit-hold [--release]`.
//
// Every one of those is a sentence in core/cliecho and a map entry here,
// and no package could see both: cliecho may not import a main package,
// and the routes table is in cliecho. This test is the join. It is the
// only place in the tree that can read a gap's prose and the verb tables
// the binary actually dispatches on, which is why it lives here and not
// beside the builders.
//
// What it can see is a verb NAMED in a gap sentence. Prose that describes
// a verb without naming one ("there is no verb that reads a backup set's
// edit hold", which was two of the five) is beyond any textual rule, and
// TestTheGapVerbRuleCatchesTheSentencesThatShipped says so out loud rather
// than leaving the coverage to be assumed.
func TestNoGapClaimsAVerbThisBinaryShips(t *testing.T) {
	gaps := cliecho.Gaps()
	if len(gaps) == 0 {
		t.Fatal("core/cliecho reports no gap sentences at all, so this test checked nothing")
	}

	// A route can print more than one sentence (a run_cycle and a per-set
	// run refuse differently on the same route) and the exemption is
	// declared once for the entry, so the "was it spent" half is asked of
	// the route and the "does it name a shipped verb" half of each
	// sentence.
	namedByRoute := map[string]map[string]bool{}
	declaredByRoute := map[string][]string{}

	checked := 0
	for _, gap := range gaps {
		checked++
		allowed := map[string]bool{}
		for _, verb := range gap.NamesShippedVerbs {
			allowed[verb] = true
		}
		declaredByRoute[gap.Route] = gap.NamesShippedVerbs
		if namedByRoute[gap.Route] == nil {
			namedByRoute[gap.Route] = map[string]bool{}
		}
		for _, verb := range shippedVerbsNamedIn(gap.Why) {
			namedByRoute[gap.Route][verb] = true
			if allowed[verb] {
				continue
			}
			t.Errorf("the gap on %s names `%s`, which this binary dispatches:\n  %s\nA gap is a promise that there is no verb, so a sentence naming one that ships is either out of date (build the command instead) or is naming it as a counterexample, in which case say so with namesShippedVerbs: []string{%q}.",
				gap.Route, verb, gap.Why, verb)
		}
	}
	if checked == 0 {
		t.Fatal("no gap sentence was examined")
	}

	// The exemption has to be spent. A verb listed and named by none of
	// that route's sentences is a standing permission for the next person
	// to write a stale sentence under.
	for route, declared := range declaredByRoute {
		for _, verb := range declared {
			if !namedByRoute[route][verb] {
				t.Errorf("%s declares namesShippedVerbs %q and none of its sentences names that verb. Delete the entry rather than leaving the check switched off for a verb nothing mentions.",
					route, verb)
			}
		}
	}
}

// backtickedPhrase finds the phrases a gap sentence quotes. Only quoted
// text is read, because that is where this package's prose puts a command
// and because an unquoted "run" or "check" is an ordinary English word: a
// rule that fired on those would be turned off within a week, which is the
// way a check dies.
var backtickedPhrase = regexp.MustCompile("`([^`]+)`")

// shippedVerbsNamedIn reports the verbs a gap sentence names that this
// binary actually dispatches.
//
// It reads `backupd <verb> [<sub>]` and the bare `<verb> [<sub>]`
// this file's prose also uses ("`settings` prints the thresholds"), and it
// understands one level of subcommand: a sentence naming
// `backupd backup-set test-connection` is naming a verb this binary
// does NOT have, even though `backup-set` is dispatched, and that is a
// true gap rather than a stale sentence.
func shippedVerbsNamedIn(why string) []string {
	subVerbs := map[string][]string{
		"backup-set": backupSetVerbNames(),
		"retention":  retentionVerbNames(),
		"medium":     mediumVerbNames(),
	}

	seen := map[string]bool{}
	var out []string
	for _, match := range backtickedPhrase.FindAllStringSubmatch(why, -1) {
		words := strings.Fields(match[1])
		if len(words) > 0 && words[0] == cliecho.Binary {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		verb := strings.Trim(words[0], ".,;:!?\"'")
		if _, ok := commands[verb]; !ok {
			continue
		}
		if len(words) > 1 && !strings.HasPrefix(words[1], "-") && !strings.HasPrefix(words[1], "<") {
			sub := strings.Trim(words[1], ".,;:!?\"'")
			if subs, ok := subVerbs[verb]; ok && !contains(subs, sub) {
				// A subcommand this verb does not have, which is exactly
				// what a gap should be naming.
				continue
			}
		}
		if seen[verb] {
			continue
		}
		seen[verb] = true
		out = append(out, verb)
	}
	sort.Strings(out)
	return out
}

// The control, and it is the deliverable as much as the rule is: the five
// sentences that shipped, exactly as they were written, fed through the
// reader above.
//
// Three of them are caught. The other two named no verb at all, and they
// are here so that the limit of this rule is written down where somebody
// adding a sixth gap will read it, rather than being discovered the next
// time a promise goes stale.
func TestTheGapVerbRuleCatchesTheSentencesThatShipped(t *testing.T) {
	for _, tc := range []struct {
		why  string
		want []string
	}{
		{
			why:  "there is no `activity` verb yet, so the durable journal cannot be read from a terminal at all (issue #598)",
			want: []string{"activity"},
		},
		{
			why:  "there is no `activity --follow` yet, so the live feed this terminal shows cannot be watched from a terminal (issues #598 and #599)",
			want: []string{"activity"},
		},
		{
			why:  "deletion runs through the API's preview/apply pair against a reviewed plan_id on purpose; `retention` on a terminal previews and deletes nothing",
			want: []string{"retention"},
		},
		{
			why:  "`settings patch` takes the scalar retention and capacity settings; a full tier-chain replacement is still a config-file edit",
			want: []string{"settings"},
		},
		{
			// A verb that really does not exist stays silent, which is
			// what makes the cases above about the verb table rather than
			// about the presence of backticks.
			why:  "there is no verb that imports a key this machine already holds; `" + cliecho.Binary + " ssh-key import --candidate ID` would be it",
			want: nil,
		},
		{
			// This one shipped as a gap and stopped being one, which
			// makes it the best case in this table: it is the rule doing
			// the job the rule exists for. `backup-set test-connection`
			// was a subcommand that did not exist under a verb that did,
			// so the sentence was a true gap and this case wanted no
			// verbs; issue #624 built the verb, and the same sentence now
			// names one this binary ships. Nobody had to remember to come
			// back and check: the rule went red on the sentence the
			// moment the verb landed, which is exactly the staleness it
			// was written to catch.
			why:  "`" + cliecho.Binary + " backup-set test-connection <source/backup-set>` is the one this route needs",
			want: []string{"backup-set"},
		},
		{
			// And a subcommand that still does not exist under a verb
			// that does, so the arm above no longer covers on its own
			// stays covered. `backup-set` is dispatched and `rekey` is
			// not, which is a true gap rather than a stale sentence.
			why:  "there is no `" + cliecho.Binary + " backup-set rekey <source/backup-set>` that rotates a key on the source host as well as here",
			want: nil,
		},
		{
			// The two that no textual rule can catch: they describe a
			// verb without naming one. Both were wrong, and what caught
			// them was somebody reading them.
			why:  "there is no verb that reads a backup set's edit hold; a terminal edit takes and releases one for the duration of the command",
			want: nil,
		},
		{
			why:  "there is no verb that releases an edit hold, for the same reason there is none that takes one",
			want: nil,
		},
	} {
		got := shippedVerbsNamedIn(tc.why)
		if len(got) != len(tc.want) || (len(got) > 0 && strings.Join(got, ",") != strings.Join(tc.want, ",")) {
			t.Errorf("the sentence\n  %s\nnames shipped verbs %v, want %v", tc.why, got, tc.want)
		}
	}

	// And the rule fires through the exported list too, not only through
	// its own reader: a gap declared without its exemption is a failure.
	gap := cliecho.Gap{Route: "GET /activity", Why: "there is no `activity` verb yet"}
	if len(shippedVerbsNamedIn(gap.Why)) == 0 {
		t.Fatal("a gap claiming a verb this binary dispatches is not seen at all, so TestNoGapClaimsAVerbThisBinaryShips could never fail")
	}
}
