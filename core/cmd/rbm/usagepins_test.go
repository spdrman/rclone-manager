package main

import (
	"flag"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/tests/compat"
)

// Where the pinned copy of the usage block lives, and how a line of it is
// spelled once core/tests/compat has captured it.
const (
	// usageCorpusPath is core/tests/compat's checked-in baseline, reached
	// from this package's own directory.
	//
	// A test in package main reading another package's testdata is unusual
	// and deliberate. The set of registered commands is the map in main.go,
	// which only package main can read, and the pinned text lives over
	// there, so one of the two has to travel. Reading a checked-in JSON
	// file is the cheaper and less breakable direction than parsing this
	// package's source from the other side.
	usageCorpusPath = "../../tests/compat/testdata/medium-free-surfaces.json"

	// usageCorpusCell is the cell captureCLI writes the usage block into.
	usageCorpusCell = "06b-cli-usage-block"

	// usageCapturePrefix is how core/tests/compat renders one line of a
	// captured stderr stream. Every line of the usage block arrives in the
	// corpus with it in front, and nothing normalizes anything inside the
	// block: the two substitutions that package makes are the throwaway
	// root directory and runtime.Version(), and neither appears here.
	//
	// This is the one thing about the capture still held in two places.
	// The corpus FORMAT is not: loadPinnedUsageLines decodes through
	// compat.LoadCorpus below. The prefix is a literal in
	// capture_cli.go's own body rather than a constant it exports, so
	// copying it is the only way to read it from here, and the fatal in
	// loadPinnedUsageLines is what makes the copy going stale loud
	// instead of silent.
	usageCapturePrefix = "  err| "

	// usageRecaptureHint is the one command that fixes a failure below, and
	// it names the cell rather than sweeping the whole corpus. A sweep is
	// what launders unrelated drift into a commit that claims to be about
	// something else, which is the trap #549 was filed about; a check that
	// forces people into it would be worse than no check.
	usageRecaptureHint = "COMPAT_UPDATE=" + usageCorpusCell + " go test ./tests/compat/"
)

// TestUsage_EveryRegisteredCommandIsPinned asks whether every command this
// binary dispatches has its usage text pinned by core/tests/compat.
//
// FR-35 clause 4 says nothing may reword a line an operator already reads,
// and core/tests/compat/testdata/medium-free-surfaces.json is what enforces
// that, by holding those lines byte for byte. The enforcement only ever
// reaches a line the corpus already holds. Cell 06b-cli-usage-block is
// compared additive-only on purpose (capture_cli.go argues why: a new
// subcommand adds lines and changes nothing an operator was already
// reading), and the price of that leniency is that a command whose lines
// were never captured is not protected at all. Nothing forced a command
// into the corpus in the first place, so `unconfigured` and `medium
// preflight` both shipped as registered commands with zero pinned lines,
// their operator-visible text free for anyone to reword, and no test
// anywhere could tell (#549).
//
// This is the thing that tells. It reads three real lists rather than a
// copy of any of them: the dispatch map itself, the usage block this build
// prints, and the corpus checked in beside it. Every pair of the three has
// already gone wrong here. `backup-set remove` shipped dispatchable and
// unlisted (#391), and `unconfigured` and `medium preflight` shipped listed
// and unpinned (#549).
//
// What it does NOT check, said out loud rather than quietly missing,
// because a surface nobody mentions cannot be told apart from one nobody
// thought of:
//
//   - the continuation lines under each entry, and the trailing paragraphs
//     about --config, the three write modes and the route's environment
//     variables. Those are pinned as of whenever they were last captured,
//     and additive-only then refuses to let them be reworded or removed,
//     which is the protection FR-35 asks for. Requiring a fresh pin for
//     every added line of prose would undo the reason 06b is additive-only
//     at all, and would make an honest one-line clarification cost a
//     re-capture.
//   - the exit status each command actually returns. That is
//     06-cli-surfaces' job, for the commands its argv table drives.
//   - a SUBCOMMAND of a command whose dispatch is a string literal rather
//     than a table. This test reads main.go's map, which is keyed by the
//     first word, so `medium` being in it says nothing about `medium
//     preflight`. Two commands close that themselves, one level down:
//     backupSetVerbNames feeds TestUsage_NamesEveryBackupSetVerb and
//     mediumVerbNames feeds TestUsage_NamesEveryMediumVerb, and once a
//     verb is listed in usage() the loop below pins its entry line like
//     any other. `catalog` and `quarantine` still compare against
//     literals in their own files, so a second catalog verb or a fourth
//     quarantine one could ship listed nowhere and nothing here would
//     say so. Giving them tables is the same small change medium just
//     took, and this list is where the gap lives until somebody does.
//
// So the promise is narrower than "the usage block is pinned", and it is
// exactly the one that was missing: every verb an operator can run is a
// verb whose entry line somebody captured, so FR-35 has something to hold
// the next reword against.
//
// It is also narrower than "this repository's operator-facing text is
// pinned", because it reads one binary's dispatch map by name. There are
// six package main binaries in this tree and the Synology packaging one
// (spkctl) is
// the other one an operator types, with a command table of its own that
// nothing in here can see: this test lives in package main precisely
// because that is the only place the map is readable, so a second binary
// wanting the same protection needs its own copy of this file beside its
// own map, and a corpus cell capturing its usage block for that copy to
// compare against. Neither exists today. That is a real gap and a cheap
// one to close when a binary earns it, and it is written down here so
// the next person meets it as a decision rather than as a discovery.
//
// scripts/compat/selftest.sh mutation-tests all three rules below against
// the real tree, because a guard nobody has watched fail is not a guard.
func TestUsage_EveryRegisteredCommandIsPinned(t *testing.T) {
	if len(commands) == 0 {
		t.Fatal("the dispatch map is empty, so this test would compare three empty lists and pass")
	}

	entries := usageEntryLines(captureStderr(t, usage))
	if len(entries) == 0 {
		t.Fatalf("no line of the usage block starts at the command column (exactly two spaces, then a non-space), so this test read no commands out of it at all and would report every one of them as missing. Either usage() was reflowed, in which case teach usageEntryLines the new shape, or it has stopped listing commands.")
	}

	pinned := loadPinnedUsageLines(t)

	// The verb an entry line introduces is its first word, which is true of
	// every shape in that block: `version`, `medium preflight <medium-id>`,
	// `artifacts [--source S]` and `backup-set create <source/backup-set>
	// --host H` all begin with the name the dispatch map is keyed by.
	listed := map[string][]string{}
	for _, line := range entries {
		verb := strings.Fields(line)[0]
		listed[verb] = append(listed[verb], line)
	}

	for _, name := range sortedNames(commands) {
		lines := listed[name]
		if len(lines) == 0 {
			t.Errorf("%q is registered in the commands map and the usage block does not list it. An operator cannot discover it, the black-box verb guard in the tests repository cannot see it, and nothing pins a word of what it prints. Give it an entry in usage(), then %s.",
				name, usageRecaptureHint)
			continue
		}
		for _, line := range lines {
			if pinned[line] {
				continue
			}
			t.Errorf("%q is a registered command whose usage entry nothing pins, so FR-35 clause 4 is not protecting the text an operator reads for it and anyone may reword it silently. The line is:\n    %s\nCapture it into cell %q of %s with:\n    %s",
				name, line, usageCorpusCell, usageCorpusPath, usageRecaptureHint)
		}
	}

	for _, verb := range sortedNames(listed) {
		if _, ok := commands[verb]; !ok {
			t.Errorf("the usage block has an entry for %q and nothing dispatches it, so an operator who reads the only reference they have gets \"unknown command\". Either register it or take the entry out.", verb)
		}
	}
}

// TestUsage_NamesEveryMediumVerb closes the level the test above cannot
// reach, for `medium`.
//
// The map in main.go has one entry for `medium`, so everything above is
// satisfied the moment `medium preflight <medium-id>` is listed and
// pinned, and stays satisfied forever after. A `medium compact` added
// tomorrow would be dispatchable, absent from usage(), invisible to the
// black-box verb guard in the tests repository, and pinned by nothing,
// which is the whole shape of failure #549 is about. It is also the
// example the doc above reaches for, which made it the sharper of the two
// gaps a reviewer found in this file: the guard cited a failure it could
// not itself see.
//
// This is TestUsage_NamesEveryBackupSetVerb's argument applied to the
// other command that has verbs, and it works the same way. The verbs come
// off mediumVerbs, which is cmdMedium's own dispatch rather than a list
// typed here, so adding one over there is checked here without anybody
// remembering this test exists. Listing it in usage() then hands it to
// TestUsage_EveryRegisteredCommandIsPinned, which requires its entry line
// to be captured, so the two together take a new subcommand from
// dispatchable to discoverable to pinned.
func TestUsage_NamesEveryMediumVerb(t *testing.T) {
	verbs := mediumVerbNames()
	if len(verbs) == 0 {
		t.Fatal("mediumVerbNames() is empty, so this test would check nothing and pass. Either mediumVerbs lost its entries, in which case `medium` dispatches nothing at all, or the table moved and this test is now reading the wrong one.")
	}

	out := captureStderr(t, usage)
	for _, verb := range verbs {
		if !strings.Contains(out, "medium "+verb+" ") {
			t.Errorf("usage() does not list \"medium %s\"; an operator cannot discover it, the black-box verb guard cannot see it, and TestUsage_EveryRegisteredCommandIsPinned cannot ask for its entry line to be pinned because there is no entry line. Give it one in usage(), then %s.",
				verb, usageRecaptureHint)
		}
	}
}

// TestUsage_ListsEveryBackupSetPatchFlag relates a REGISTERED flag to the
// text an operator reads, which nothing in this repository did.
//
// The whole file above is about commands, and it works: a verb that is
// dispatchable has to be listed and a listed line has to be pinned. A flag
// had none of that. TestUsage_EveryRegisteredCommandIsPinned keys on the
// first word of an entry line, so an entry listing zero flags satisfies it
// completely, and the "what this does NOT check" list next door names
// continuation lines and exit statuses and never mentions flags at all.
//
// The cost was not hypothetical. Issue #572 added five flags to
// `backup-set patch` (--ssh-key-file, --ssh-key-id, --known-hosts-line,
// --trust-host-key and --acknowledge-host-key-change), every one of them
// declared, parsed, routed and tested, and not one of them reached the
// usage block. An operator reading the only reference the binary gives
// them could not find out that a key rotation or a host-key re-trust was
// reachable from a terminal at all, which for a feature whose entire
// premise is "you used to have to delete the set and make it again" is
// the feature not shipping. --acknowledge-repoint had been missing from
// the same entry since #350, for the same reason: nothing was looking.
//
// It walks the FlagSet rather than a list typed here, in the shape
// backupSetVerbNames and mediumVerbNames already use one level up: the
// answer comes from the declaration, so a flag added tomorrow is checked
// without anybody remembering this test exists.
//
// What it deliberately does NOT do, in the spirit of the list above:
//
//   - pin the continuation lines it reads. It asks whether a flag is
//     NAMED somewhere in the command's block, never how the block is
//     worded. That exclusion is argued at length above and reversing it
//     would make an honest one-line clarification cost a re-capture.
//
//   - hold `backup-set create` to the same rule. That entry is not
//     complete either (--port and --acknowledge-repoint are both missing
//     from it), and the same VisitAll would say so. It is left for
//     whoever owns that entry rather than swept in here, because #572
//     touched patch and this is the guard for the gap #572 fell into.
//     Written down so the next person meets it as a decision.
func TestUsage_ListsEveryBackupSetPatchFlag(t *testing.T) {
	block := usageCommandBlock(captureStderr(t, usage), "backup-set patch ")
	if block == "" {
		t.Fatal("the usage block has no entry starting \"backup-set patch \", so this test read no text for that command and would report every flag as fine. Either the entry was reworded, in which case teach this test the new opening, or `backup-set patch` has stopped being listed, which TestUsage_EveryRegisteredCommandIsPinned should already have said.")
	}

	// --config is every command's, and usage() says so once in a trailing
	// paragraph rather than on twenty entry lines. The create-only flags
	// are not this verb's to list, and backupSetCreateOnlyFlags is the
	// same list refuseFlagsOfTheOtherVerb refuses them by, so the two
	// cannot drift.
	notPatchs := map[string]bool{"config": true}
	for _, name := range backupSetCreateOnlyFlags {
		notPatchs[name] = true
	}

	checked := 0
	declareBackupSetFlags().fs.VisitAll(func(fl *flag.Flag) {
		if notPatchs[fl.Name] {
			return
		}
		checked++
		// A boundary rather than a substring, so listing --ssh-key-file
		// can never be read as also listing a future --ssh-key.
		named := regexp.MustCompile(`--` + regexp.QuoteMeta(fl.Name) + `([^\w-]|$)`)
		if !named.MatchString(block) {
			t.Errorf("--%s is a registered `backup-set patch` flag and the usage block never names it, so the only reference the binary gives an operator does not say it exists. Add it to the entry in usage(), then %s.\nThe block is:\n%s",
				fl.Name, usageRecaptureHint, block)
		}
	})
	if checked == 0 {
		t.Fatal("no flag survived the exclusions, so this test compared nothing and passed. Either declareBackupSetFlags stopped registering flags, or backupSetCreateOnlyFlags has grown to cover all of them.")
	}
}

// usageCommandBlock returns everything usage() says about one command:
// its entry line, plus every continuation line under it, up to the next
// entry line.
//
// It is usageEntryLines' shape with the opposite job. That one wants the
// index and throws the prose away; this one wants one command's whole
// paragraph, because a flag is as legitimately named on a continuation
// line as on the entry itself (`backup-set create` names most of its own
// that way), and a check that read only the entry line would be a check
// about formatting.
func usageCommandBlock(text, prefix string) string {
	var out []string
	inIndex := false
	collecting := false
	for _, line := range strings.Split(text, "\n") {
		if !inIndex {
			inIndex = strings.HasPrefix(line, "commands:")
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Back at column zero: the index is over.
		if !strings.HasPrefix(line, " ") {
			break
		}
		if isEntry := strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "); isEntry {
			collecting = strings.HasPrefix(strings.TrimSpace(line), prefix)
		}
		if collecting {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// usageEntryLines returns the lines of the usage block that introduce a
// command, which is the index an operator scans to find out what they can
// run.
//
// It reads the run of lines under the "commands:" header and stops at the
// first line that starts back at column zero, which is where that index
// ends and the trailing paragraphs begin. Inside the run, an entry line is
// exactly two spaces then a non-space; a continuation line is indented much
// further so a description sits in its own column.
//
// Bounding it to the index is the whole point rather than a tidiness. This
// used to take any two-space-indented line anywhere in the block, and #551
// then added an exit-code table to a trailing paragraph, indented exactly
// that far:
//
//	0   the command did what it was asked
//	1   an ordinary failure: ...
//
// which this read as four commands named 0, 1, 2 and 3 that nothing
// dispatches. Neither change was wrong on its own and neither lane could
// see the other, which is the kind of thing that only shows up once both
// are in one tree. An indentation is a rendering detail and a header is a
// structure, so key off the structure.
func usageEntryLines(text string) []string {
	var out []string
	inIndex := false
	for _, line := range strings.Split(text, "\n") {
		if !inIndex {
			inIndex = strings.HasPrefix(line, "commands:")
			continue
		}
		if trimmed := strings.TrimSpace(line); trimmed == "" {
			continue
		}
		// Back at column zero: the index is over.
		if !strings.HasPrefix(line, " ") {
			break
		}
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// loadPinnedUsageLines reads the usage lines the FR-35 corpus holds.
//
// Through core/tests/compat's own LoadCorpus, rather than a local struct
// that redeclares the file's shape. Both packages are in the core module
// and compat's corpus.go is the pure half of it (it reads no database,
// builds no binary and runs no command), so importing it costs this test
// nothing and takes the format out of two places at once. A renamed JSON
// key now fails to compile here instead of decoding into an empty cell
// and being caught, one step later, by the fatal below.
//
// Every refusal in here is a t.Fatal rather than a skip on purpose. This
// test's whole claim is that a command's text is pinned somewhere else, and
// "I could not find the somewhere else" and "it is pinned" are the two
// answers it exists to keep apart. A corpus that moved, a cell that was
// renamed or a capture format that changed all leave this test comparing
// against nothing, which would report every command as fine.
func loadPinnedUsageLines(t *testing.T) map[string]bool {
	t.Helper()

	corpus, err := compat.LoadCorpus(usageCorpusPath)
	if err != nil {
		t.Fatalf("reading the FR-35 corpus at %s: %v\n\nThis test compares the usage block against the lines that file pins. If the corpus moved, move this path with it rather than leaving the check to pass against a file that is not there.", usageCorpusPath, err)
	}

	cell, ok := corpus.Cells[usageCorpusCell]
	if !ok {
		t.Fatalf("%s has no cell %q, so nothing in this repository pins the usage block and this test has nothing to check against. If the cell was renamed, rename it here too.", usageCorpusPath, usageCorpusCell)
	}
	if len(cell.Lines) == 0 {
		t.Fatalf("cell %q of %s holds no lines, so it passes whatever the usage block says.", usageCorpusCell, usageCorpusPath)
	}

	pinned := map[string]bool{}
	for _, line := range cell.Lines {
		if s, ok := strings.CutPrefix(line, usageCapturePrefix); ok {
			pinned[s] = true
		}
	}
	if len(pinned) == 0 {
		t.Fatalf("no line of cell %q carries the %q prefix core/tests/compat renders a captured stderr line with, so this test would find nothing pinned and report every command as unprotected. The capture format changed; teach usageCapturePrefix the new one.", usageCorpusCell, usageCapturePrefix)
	}
	return pinned
}

// sortedNames keeps the failures above in a stable order, so a run that
// finds four of them reads the same way twice.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
