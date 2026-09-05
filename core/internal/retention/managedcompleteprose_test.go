package retention

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
)

// This file is issue #505's answer to "why did this reach eleven places".
//
// gfsManagedCompleteStates gained REMOTE_RETAINED with issue #282 and every
// sentence that told a person which states a decision accepts stayed at
// three, because nothing tied the prose to the map. The maps were right the
// whole time, so no test could go red: what was wrong was only what the
// product said about itself, and the operator it said it to was the one
// running a read-only backup set, where REMOTE_RETAINED is the only state
// anything ever reaches. They were told by name that their backups' state
// is not permitted, and had a fault to go looking for that did not exist.
//
// The messages this package owns now read their list off the map
// (gfsManagedCompleteNames), so those cannot drift again by construction.
// This is the half that covers everywhere else: the doc comments, the
// runbook, the Prometheus HELP line, any sentence in any tracked file that
// sets out to list the set.

// managedCompleteEnumerationFloor is how many of the managed-complete
// states a run of state names has to mention before this file treats it as
// an attempt to enumerate the set rather than a sentence that happens to
// name a couple of them.
//
// Three, while the set holds four, and the number is a judgement rather
// than a derivation. Two is where the false positives live: "COMMITTED or
// REMOTE_DELETE_PENDING" is how half this codebase describes the FR-15
// delete window, which is a genuinely different set and always will be.
// Three is high enough that a run has to be reaching for the whole set to
// qualify, and low enough that dropping one name from it is caught, which
// is exactly the mistake #505 is about.
//
// The cost is stated plainly: a new sentence that names only two of the
// four goes unnoticed here. That is the deliberate floor of this check,
// not an oversight in it.
const managedCompleteEnumerationFloor = 3

// notTheManagedCompleteSet is every run of three or more state names in the
// tree that is not this set and must not be corrected into it.
//
// An entry is a claim about one specific piece of text, checked both ways:
// the run must still be in that file (a stale entry fails, the way
// scripts/selftest/check-anchors.sh treats a mutation anchor that no longer
// matches), and no run may be silently forgiven without one.
var notTheManagedCompleteSet = []proseException{
	{
		path: "README.md",
		run:  "COMMITTED, REMOTE_DELETE_PENDING, REMOTE_RETAINED",
		why: "the lifecycle summary's list of what QUARANTINED is reachable from. COMPLETE " +
			"is absent because a COMPLETE artifact found bad goes to QUARANTINED_LOST " +
			"instead: the remote copy is already gone, so there is nothing left to re-fetch.",
	},
	{
		path: "core/internal/app/validate.go",
		run:  "COMMITTED/REMOTE_DELETE_PENDING/REMOTE_RETAINED",
		why: "the quarantine routing, not the eligible set: these three go to QUARANTINED " +
			"and COMPLETE goes to QUARANTINED_LOST, which is the whole point of the sentence.",
	},
	{
		path: "core/internal/lifecycle/retainremote.go",
		run:  "COMMITTED, REMOTE_DELETE_PENDING or REMOTE_RETAINED",
		why: "RetainRemote's own legal from-states. COMPLETE is excluded on purpose: the " +
			"remote copy is already gone by then, so there is nothing left to retain.",
	},
	{
		path: "core/internal/state/placements_test.go",
		run:  "COMMITTED, REMOTE_DELETE_PENDING and REMOTE_RETAINED",
		why: "the lineages QUARANTINED is reachable from with the final artifact on disk. " +
			"COMPLETE is not one of them; it routes to QUARANTINED_LOST instead.",
	},
	{
		path: "core/migrations/0001_init.sql",
		run:  "COMMITTED, REMOTE_DELETE_PENDING, COMPLETE",
		why: "a landed migration. It is the state CHECK constraint as it was before " +
			"REMOTE_RETAINED existed, and internal/state's migration anchor holds the " +
			"published bytes of this file: editing it to read better here would break " +
			"every deployment's upgrade path.",
	},
	{
		path: "core/migrations/0002_quarantined_lost.sql",
		run:  "COMMITTED, REMOTE_DELETE_PENDING, COMPLETE",
		why:  "a landed migration, for the same reason as 0001_init.sql.",
	},
	{
		path: "core/migrations/0007_placements.sql",
		run:  "COMMITTED, REMOTE_DELETE_PENDING, REMOTE_RETAINED",
		why: "the quarantine lineages that keep an ACTIVE placement. COMPLETE is absent " +
			"for the same reason it is absent from placements_test.go's copy above.",
	},
	{
		path: "core/tests/crashmatrix/crash_matrix_test.go",
		run:  "COMMITTED, REMOTE_DELETE_PENDING and COMPLETE",
		why: "the FR-15 crash points this matrix drives. A read-only set is never offered " +
			"the delete step at all, so REMOTE_RETAINED has no crash point to reach here.",
	},
	{
		path: "core/tests/crashmatrix/harness/main.go",
		run:  "COMMITTED/REMOTE_DELETE_PENDING/COMPLETE",
		why:  "the same crash points, named by the harness that reaches them.",
	},
}

// proseException is one run of state names that is deliberately not this
// set. why is not decoration: an exception a reader cannot judge is an
// exception nobody will ever remove.
type proseException struct {
	path string
	run  string
	why  string
}

// TestEveryEnumerationOfTheManagedCompleteSetNamesAllOfIt is the guard.
//
// Add a state to gfsManagedCompleteStates and every sentence in the tree
// that lists the set goes red until it names the new one too, which is the
// property #505 needed and did not have.
func TestEveryEnumerationOfTheManagedCompleteSetNamesAllOfIt(t *testing.T) {
	runs := scanStateRuns(t)

	want := managedCompleteNameSet()
	exempt := map[string]bool{}
	for _, e := range notTheManagedCompleteSet {
		exempt[e.path+"\x00"+e.run] = true
	}

	for _, r := range runs {
		if len(r.named) < managedCompleteEnumerationFloor || equalNameSets(r.named, want) {
			continue
		}
		if exempt[r.path+"\x00"+r.run] {
			continue
		}
		t.Errorf("%s:%d lists %d of the %d states gfsIsManagedComplete accepts and stops there:\n"+
			"    %s\n"+
			"  missing: %s\n\n"+
			"  gfsManagedCompleteStates accepts %s. A sentence that names a strict subset of\n"+
			"  that tells whoever reads it that the states it left out are refused, and for\n"+
			"  REMOTE_RETAINED that reader is running a read-only backup set where it is the\n"+
			"  only state anything reaches. Either name all of them, or add an entry to\n"+
			"  notTheManagedCompleteSet in %s saying which set this actually is.",
			r.path, r.line, len(r.named), len(want), r.run,
			strings.Join(missingFrom(r.named, want), ", "),
			lifecycle.NameSet(gfsManagedCompleteStates), thisFileRepoPath())
	}
}

// TestEveryManagedCompleteExceptionStillMatchesTheTree is the other half.
//
// An exception whose text has moved on forgives nothing and hides nothing;
// it just sits there reading like a decision somebody made. Same failure
// mode as a mutation anchor that no longer plants anything, and the same
// treatment: name it and go red.
func TestEveryManagedCompleteExceptionStillMatchesTheTree(t *testing.T) {
	runs := scanStateRuns(t)

	seen := map[string]bool{}
	for _, r := range runs {
		seen[r.path+"\x00"+r.run] = true
	}

	for _, e := range notTheManagedCompleteSet {
		if seen[e.path+"\x00"+e.run] {
			continue
		}
		t.Errorf("the exception for %s no longer matches anything in the tree:\n"+
			"    %s\n"+
			"  recorded because: %s\n\n"+
			"  Either that text moved and the entry should follow it, or it is gone and the\n"+
			"  entry should be deleted. Leaving it forgives nothing and reads like a live\n"+
			"  decision.", e.path, e.run, e.why)
	}
}

// TestTheProseScanFindsSomethingToCheck is the positive control.
//
// Everything above is a search that reports what it did not find, which is
// the shape of check this repository keeps catching in the act of passing
// while observing nothing: break the flattening, or the regexp, or the file
// walk, and every assertion above goes quiet and green. So the scan's own
// yield is asserted, including that it still sees runs naming the whole set
// rather than only the exceptions.
func TestTheProseScanFindsSomethingToCheck(t *testing.T) {
	runs := scanStateRuns(t)

	if len(runs) < 20 {
		t.Errorf("the scan found %d runs of state names in the whole tree, which is too few to "+
			"have worked: the flattening or the pattern has stopped matching.", len(runs))
	}

	want := managedCompleteNameSet()
	full := 0
	for _, r := range runs {
		if equalNameSets(r.named, want) {
			full++
		}
	}
	if full < 4 {
		t.Errorf("the scan found %d runs naming the whole managed-complete set. Before #505 there "+
			"were several, and the point of that issue was to add more, so finding almost none "+
			"means the scan is not reading what it thinks it is.", full)
	}
}

// TestManagedCompleteNamesRendersEveryStateInTheMap keeps the renderer the
// refusals now use honest.
//
// gfsManagedCompleteNames is what makes prune's and last-known-good's
// messages incapable of disagreeing with the map. That guarantee is worth
// exactly as much as the rendering is complete, so a state in the map and
// not in the sentence is a failure here rather than a surprise in a support
// thread.
func TestManagedCompleteNamesRendersEveryStateInTheMap(t *testing.T) {
	got := gfsManagedCompleteNames()

	for s, in := range gfsManagedCompleteStates {
		if !in {
			continue
		}
		if !strings.Contains(got, string(s)) {
			t.Errorf("gfsManagedCompleteNames() = %q, which does not name %s even though "+
				"gfsIsManagedComplete accepts it", got, s)
		}
	}
	for _, s := range lifecycle.AllStates {
		if gfsManagedCompleteStates[s] {
			continue
		}
		if strings.Contains(got, string(s)) {
			t.Errorf("gfsManagedCompleteNames() = %q, which names %s even though "+
				"gfsIsManagedComplete refuses it", got, s)
		}
	}
	if want := len(gfsManagedCompleteStates); strings.Count(got, ",")+strings.Count(got, " or ") != want-1 {
		t.Errorf("gfsManagedCompleteNames() = %q, which does not read as a list of %d states", got, want)
	}
}

// stateRun is one run of state names found in one file.
type stateRun struct {
	path  string
	line  int
	run   string
	named []string
}

// scanStateRuns reads every tracked text file and returns every run of two
// or more managed-complete state names in it.
//
// It looks only for the uppercase contract spelling, the one the journal
// stores and an operator reads in a log line, and never for the lowercase
// prose form. "complete" and "committed" are ordinary English words that
// appear hundreds of times in this tree with nothing to do with a state,
// and a check that cries wolf that often gets switched off. The one message
// that did spell the list in lowercase now renders it from the map instead,
// which is the better fix anyway.
func scanStateRuns(t *testing.T) []stateRun {
	t.Helper()

	root := repositoryRoot(t)
	self := thisFileRepoPath()

	names := managedCompleteNameSet()
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	token := `\b(?:` + strings.Join(names, "|") + `)\b`
	sep := `(?:\s*[,/;|-]\s*|\s+)(?:(?:or|and)\s+)?`
	pattern := regexp.MustCompile(token + `(?:` + sep + token + `)+`)

	var out []stateRun
	for _, rel := range trackedTextFiles(t, root) {
		if rel == self {
			// The file listing the exceptions cannot be its own
			// exception, and every entry in it quotes the run it
			// forgives verbatim.
			continue
		}
		blob, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		flat, lines := flattenForProse(string(blob))
		for _, loc := range pattern.FindAllStringIndex(flat, -1) {
			run := flat[loc[0]:loc[1]]
			out = append(out, stateRun{
				path:  rel,
				line:  lines[loc[0]],
				run:   run,
				named: namesIn(run, names),
			})
		}
	}
	return out
}

// flattenForProse turns a file into one line of scannable text, and returns
// the source line each byte of it came from.
//
// A sentence that wraps is still one sentence, and #505's own examples
// wrap: config.go's list had COMMITTED at the end of one comment line and
// the rest on the next. A line-at-a-time scan would have read that as two
// short runs and found nothing wrong with either. Comment lead-ins and
// quoting go too, so a Go doc comment, a markdown bullet list, a SQL IN
// clause and a Go string literal all reduce to the same shape.
func flattenForProse(blob string) (string, []int) {
	var b strings.Builder
	var lines []int

	for i, raw := range strings.Split(blob, "\n") {
		text := strings.TrimSpace(raw)
		for _, lead := range []string{"//", "*", "#"} {
			if strings.HasPrefix(text, lead) {
				text = strings.TrimSpace(strings.TrimPrefix(text, lead))
				break
			}
		}
		text = proseQuoting.Replace(text)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
			lines = append(lines, i+1)
		}
		b.WriteString(text)
		for range []byte(text) {
			lines = append(lines, i+1)
		}
	}
	return b.String(), lines
}

// proseQuoting removes the characters that only ever wrap a state name:
// markdown backticks, and the quotes a SQL IN clause or a Go string literal
// puts around one. Dropping them means one pattern reads all of them.
var proseQuoting = strings.NewReplacer("`", "", `"`, "", "'", "")

// trackedTextFiles lists the repository's tracked files that could hold a
// sentence about states. git ls-files rather than a walk, so a build
// directory, a vendored copy or another agent's worktree cannot add to the
// corpus this check reads.
func trackedTextFiles(t *testing.T, root string) []string {
	t.Helper()

	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", root, err)
	}

	scannable := map[string]bool{
		".go": true, ".md": true, ".sql": true, ".json": true, ".yaml": true,
		".yml": true, ".sh": true, ".ts": true, ".tsx": true, ".js": true,
		".mjs": true, ".py": true, ".html": true, ".css": true, ".txt": true,
	}

	var files []string
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" || !scannable[strings.ToLower(filepath.Ext(rel))] {
			continue
		}
		files = append(files, rel)
	}
	if len(files) == 0 {
		t.Fatalf("git ls-files in %s returned no scannable file, so this check would inspect nothing", root)
	}
	return files
}

// repositoryRoot resolves the checkout this package ships in, and refuses
// anything that does not hold this file.
func repositoryRoot(t *testing.T) string {
	t.Helper()

	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	root := strings.TrimSpace(string(out))
	if _, err := os.Stat(filepath.Join(root, thisFileRepoPath())); err != nil {
		t.Fatalf("%s does not hold %s, so it is not the repository this package ships in: %v",
			root, thisFileRepoPath(), err)
	}
	return root
}

// thisFileRepoPath is this file's own repository-relative path, taken from
// the compiler rather than written down, so it cannot name a file that has
// been renamed out from under it.
func thisFileRepoPath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "core/internal/retention/managedcompleteprose_test.go"
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return file
	}
	// .../core/internal/retention/<name>
	return filepath.ToSlash(filepath.Join("core", "internal", "retention", filepath.Base(abs)))
}

// managedCompleteNameSet is gfsManagedCompleteStates as plain strings.
func managedCompleteNameSet() []string {
	var out []string
	for s, in := range gfsManagedCompleteStates {
		if in {
			out = append(out, string(s))
		}
	}
	sort.Strings(out)
	return out
}

// namesIn returns which of names the run mentions, without duplicates.
func namesIn(run string, names []string) []string {
	var out []string
	for _, n := range names {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(n) + `\b`).MatchString(run) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func equalNameSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func missingFrom(named, want []string) []string {
	have := map[string]bool{}
	for _, n := range named {
		have[n] = true
	}
	var out []string
	for _, w := range want {
		if !have[w] {
			out = append(out, w)
		}
	}
	return out
}
