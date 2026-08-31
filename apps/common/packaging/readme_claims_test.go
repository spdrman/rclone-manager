package packaging

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file holds issue #112's half of the README audit: the claims the
// top-level README.md makes that are decidable from this repository
// alone, checked on every run instead of re-read by hand once a phase.
//
// The README has drifted twice already, and both times in the same
// shape: a sentence that was true when it was written, about code that
// then moved. "core/cmd/backup-manager/main.go is 25 lines and
// understands exactly one subcommand" survived eleven more subcommands
// landing, and the layout tree survived six new packages. Neither would
// have survived a check, so the checks live here.
//
// Every check below asserts something does NOT happen: the README does
// not name a path that is gone, does not list a command the binary does
// not register, does not omit one it does. A negative assertion proves
// nothing on its own, because a check that can never fire satisfies
// every one of them. So each test carries its own positive control: the
// same extractor and the same comparison, run against input that SHOULD
// trip it, failing the test if it does not. That convention comes from
// matrix_guards_test.go in this package, and it is the reason these
// checks are worth more than the prose they replace.
//
// What is deliberately NOT checked here, and why:
//
//   - The browser client's fourteen unserved /api/v1 paths. The README
//     states the count and the list. Pinning it would mean re-deriving
//     the client's own path set from TypeScript string concatenation,
//     which is not decidable by reading, and #166 is landing a real
//     contract that makes the question answerable properly. The one
//     mismatch that is decidable by exact-string comparison, getVersion,
//     is checked below.
//   - Anything requiring the platform hardware. The acceptance
//     procedures are prose until somebody runs them, and no check here
//     can change that. What IS checked is that the README keeps saying
//     so for exactly as long as the generated matrix does.
//   - The measured binary size. It moves with the toolchain and with
//     rclone, so the README names the command that produces it instead
//     of asserting a number this suite would have to rebuild a
//     cross-compiled binary to confirm.

// readmePath is the one document this file is about.
func readmePath() string { return Path("README.md") }

func readREADME(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(readmePath())
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	return string(data)
}

// ---------------------------------------------------------------------
// Extractors, shared by the real checks and by their positive controls
// ---------------------------------------------------------------------

var (
	markdownLink   = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)
	headingLine    = regexp.MustCompile(`(?m)^#{1,6}\s+(.*?)\s*$`)
	backtickSpan   = regexp.MustCompile("`([^`\n]+)`")
	anchorStripper = regexp.MustCompile(`[^a-z0-9 -]`)
	tableCellCode  = regexp.MustCompile("^\\|\\s*`([^`]+)`")
)

// slugify reproduces the anchor GitHub derives from a heading, which is
// what an in-document link like (#the-lifecycle) actually has to match.
func slugify(heading string) string {
	s := strings.ToLower(strings.TrimSpace(heading))
	s = strings.NewReplacer("`", "", "*", "", "_", "", "[", "", "]", "", "(", "", ")", "").Replace(s)
	s = anchorStripper.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, " ", "-")
}

// headingAnchors is every anchor the document defines.
func headingAnchors(doc string) map[string]bool {
	out := map[string]bool{}
	for _, m := range headingLine.FindAllStringSubmatch(doc, -1) {
		out[slugify(m[1])] = true
	}
	return out
}

// brokenLinks returns one complaint per markdown link in doc that does
// not resolve: a relative path with no file behind it, or a same
// document anchor with no heading behind it. External links are not
// followed; this repository is private, so a reachability check here
// would test the reviewer's credentials rather than the document.
func brokenLinks(doc, root string) []string {
	anchors := headingAnchors(doc)
	var out []string
	for _, m := range markdownLink.FindAllStringSubmatch(doc, -1) {
		target := m[1]
		switch {
		case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"), strings.HasPrefix(target, "mailto:"):
			continue
		case strings.HasPrefix(target, "#"):
			if !anchors[strings.TrimPrefix(target, "#")] {
				out = append(out, fmt.Sprintf("link to %q matches no heading in this document", target))
			}
		default:
			path := target
			if i := strings.IndexByte(path, '#'); i >= 0 {
				path = path[:i]
			}
			if _, err := os.Stat(filepath.Join(root, path)); err != nil {
				out = append(out, fmt.Sprintf("link to %q does not resolve to a file in this repository", target))
			}
		}
	}
	return out
}

// repoTopLevels are the directory names a backticked token has to start
// with before this file treats it as a claim about a path in this tree
// rather than as a command, a state name or a config key. Being
// conservative here is deliberate: a false positive turns the check into
// something people disable.
var repoTopLevels = []string{
	"core/", "apps/", "ui/", "docs/", "scripts/", "container/", "tools/", ".github/", ".husky/",
}

var pathShaped = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// goSelector matches a Go qualified identifier whose last segment is
// package.Exported, e.g. `core/service.Open`. Those read like paths and
// are not, so they are skipped rather than reported as missing files.
// The uppercase letter after the dot is what separates them from a real
// filename: no file in this tree is named `something.Open`.
var goSelector = regexp.MustCompile(`\.[A-Z][A-Za-z0-9]*$`)

// missingBacktickedPaths returns one complaint per backticked token in
// doc that looks like a path into this repository and is not one.
// declaredAbsent is the set of paths the document names precisely
// BECAUSE they do not exist; a check that forbade those would force the
// README to stop admitting what is missing, which is the opposite of
// what it is for.
func missingBacktickedPaths(doc, root string, declaredAbsent map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range backtickSpan.FindAllStringSubmatch(doc, -1) {
		tok := strings.TrimSpace(m[1])
		if seen[tok] {
			continue
		}
		seen[tok] = true
		if !pathShaped.MatchString(tok) {
			continue
		}
		hasPrefix := false
		for _, p := range repoTopLevels {
			if strings.HasPrefix(tok, p) {
				hasPrefix = true
				break
			}
		}
		if !hasPrefix || goSelector.MatchString(tok) {
			continue
		}
		if _, ok := declaredAbsent[tok]; ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, strings.TrimSuffix(tok, "/"))); err != nil {
			out = append(out, fmt.Sprintf("`%s` does not exist in this repository", tok))
		}
	}
	sort.Strings(out)
	return out
}

// region returns the lines between <!-- BEGIN name --> and
// <!-- END name -->. The README carries these markers around the two
// blocks this file compares against code, the same way
// docs/conformance/phase-4-matrix.md marks its generated region: a
// marked region is what lets a check be exact instead of guessing which
// table it is looking at.
func region(t *testing.T, doc, name string) string {
	t.Helper()
	begin := "<!-- BEGIN " + name + " -->"
	end := "<!-- END " + name + " -->"
	i := strings.Index(doc, begin)
	j := strings.Index(doc, end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("README.md has no %s ... %s region; the checks in readme_claims_test.go read that region, so removing it removes the check", begin, end)
	}
	return doc[i+len(begin) : j]
}

// commandsFromTable reads the first backticked cell of every table row
// in a region, which is how the README lists a command.
func commandsFromTable(regionText string) []string {
	var out []string
	for _, line := range strings.Split(regionText, "\n") {
		line = strings.TrimSpace(line)
		if m := tableCellCode.FindStringSubmatch(line); m != nil {
			out = append(out, strings.Fields(m[1])[0])
		}
	}
	sort.Strings(out)
	return out
}

var (
	commandsMapBlock = regexp.MustCompile(`(?s)var commands = map\[string\]func\(\[\]string\) int\{(.*?)\n\}`)
	commandsMapKey   = regexp.MustCompile(`(?m)^\s*"([a-z-]+)":`)
	usageBlock       = regexp.MustCompile("(?s)func usage\\(\\) \\{.*?`(.*?)`")
	usageCommand     = regexp.MustCompile(`(?m)^  ([a-z][a-z-]*)(?:\s|$)`)
)

// registeredCommands reads the dispatch table in main.go: the one place
// that decides what `backup-manager <cmd>` actually accepts.
func registeredCommands(t *testing.T, src string) []string {
	t.Helper()
	block := commandsMapBlock.FindStringSubmatch(src)
	if block == nil {
		t.Fatal("could not find the `var commands = map[string]func([]string) int{...}` dispatch table in main.go; readme_claims_test.go reads it to decide what the CLI accepts")
	}
	var out []string
	for _, m := range commandsMapKey.FindAllStringSubmatch(block[1], -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// usageCommands reads the command names out of the help text main.go
// prints, which is the closest thing this binary has to `--help` output
// and is what an operator actually sees.
func usageCommands(t *testing.T, src string) []string {
	t.Helper()
	block := usageBlock.FindStringSubmatch(src)
	if block == nil {
		t.Fatal("could not find the usage() help text in main.go")
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range usageCommand.FindAllStringSubmatch(block[1], -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func diffSets(want, got []string) (missing, extra []string) {
	w := map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	g := map[string]bool{}
	for _, s := range got {
		g[s] = true
	}
	for _, s := range want {
		if !g[s] {
			missing = append(missing, s)
		}
	}
	for _, s := range got {
		if !w[s] {
			extra = append(extra, s)
		}
	}
	return missing, extra
}

// ---------------------------------------------------------------------
// 1. Every link and every path the README names still resolves
// ---------------------------------------------------------------------

func TestREADMELinksResolve(t *testing.T) {
	doc := readREADME(t)
	if broken := brokenLinks(doc, Path(".")); len(broken) > 0 {
		t.Errorf("README.md:\n  %s", strings.Join(broken, "\n  "))
	}

	// Positive control: the same extractor over a document that carries
	// one dead file link and one dead anchor has to name both.
	control := "# A Heading\n\nSee [gone](docs/no-such-file.md) and [nowhere](#not-a-heading) and [fine](#a-heading).\n"
	got := brokenLinks(control, Path("."))
	if len(got) != 2 {
		t.Errorf("positive control: brokenLinks found %d problems in a document with a dead path and a dead anchor, want 2: %v", len(got), got)
	}
}

// declaredAbsentPaths are the repository paths the README names on
// purpose because they are NOT here. Each one needs a reason, because
// the entry is what stops the check from firing, and an entry without a
// reason is just a silenced finding.
var declaredAbsentPaths = map[string]string{
	"apps/ugos/backend":            "named in the gate section as a component that is absent from this tree, which is why its checks are inapplicable rather than skipped",
	"apps/ugos/frontend/upk-proof": "same: absent, and scripts/ci-local.sh's preflight names it for exactly that reason",
	"tools/backup-manager/":        "the path this project was originally scoped at inside iasbuilt/iac; the README carries the correction and has to be able to name the old location",
}

func TestREADMEBacktickedPathsResolve(t *testing.T) {
	doc := readREADME(t)
	if missing := missingBacktickedPaths(doc, Path("."), declaredAbsentPaths); len(missing) > 0 {
		t.Errorf("README.md names paths that are not in this repository:\n  %s", strings.Join(missing, "\n  "))
	}

	// Positive control, both halves: a path that is gone is reported,
	// and the declared-absent list actually suppresses one.
	control := "Read `core/internal/no-such-package/` and `apps/ugos/backend` and `core/internal/state/` and `core/service.Open`.\n"
	got := missingBacktickedPaths(control, Path("."), declaredAbsentPaths)
	if len(got) != 1 || !strings.Contains(got[0], "no-such-package") {
		t.Errorf("positive control: want exactly the missing path reported, got %v", got)
	}
	if bare := missingBacktickedPaths(control, Path("."), nil); len(bare) != 2 {
		t.Errorf("positive control: with no declared-absent list the same text should report 2 missing paths, got %v", bare)
	}
}

// ---------------------------------------------------------------------
// 2. The README's command table, main.go's dispatch table, and the help
//    text the binary prints all name the same commands
// ---------------------------------------------------------------------

func TestREADMEDocumentsExactlyTheRegisteredCommands(t *testing.T) {
	src, err := os.ReadFile(Path(filepath.Join("core", "cmd", "backup-manager", "main.go")))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	registered := registeredCommands(t, string(src))
	if len(registered) == 0 {
		t.Fatal("read no commands out of main.go's dispatch table")
	}

	// The help text has to match the dispatch table first, or the README
	// agreeing with either one proves nothing.
	if missing, extra := diffSets(registered, usageCommands(t, string(src))); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("main.go's usage() text disagrees with its own dispatch table: missing %v, unregistered %v", missing, extra)
	}

	documented := commandsFromTable(region(t, readREADME(t), "CLI-COMMANDS"))
	missing, extra := diffSets(registered, documented)
	if len(missing) > 0 {
		t.Errorf("README.md's command table omits commands the binary registers: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("README.md's command table names commands the binary does not register: %v", extra)
	}

	// Positive control: a table that drops one command and invents one
	// has to produce exactly one complaint of each kind.
	controlTable := "| `run` | one cycle |\n| `frobnicate` | not a command |\n"
	m, e := diffSets([]string{"run", "version"}, commandsFromTable(controlTable))
	if len(m) != 1 || m[0] != "version" {
		t.Errorf("positive control: want the dropped command reported, got %v", m)
	}
	if len(e) != 1 || e[0] != "frobnicate" {
		t.Errorf("positive control: want the invented command reported, got %v", e)
	}
}

// ---------------------------------------------------------------------
// 3. The README's core/internal inventory matches the packages that are
//    actually there
// ---------------------------------------------------------------------

func coreInternalPackages(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(Path(filepath.Join("core", "internal")))
	if err != nil {
		t.Fatalf("read core/internal: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

var inventoryLine = regexp.MustCompile(`(?m)^\s{2}([a-z][a-z0-9]*)/\s`)

func inventoryNames(regionText string) []string {
	var out []string
	for _, m := range inventoryLine.FindAllStringSubmatch(regionText, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func TestREADMEInventoryMatchesCoreInternal(t *testing.T) {
	onDisk := coreInternalPackages(t)
	documented := inventoryNames(region(t, readREADME(t), "CORE-INTERNAL"))

	missing, extra := diffSets(onDisk, documented)
	if len(missing) > 0 {
		t.Errorf("README.md's core/internal inventory omits packages that exist: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("README.md's core/internal inventory names packages that do not exist: %v", extra)
	}

	// Positive control: the same comparison against a block that has
	// lost a package and gained one it invented.
	control := "  config/       yaml\n  nosuchpkg/    invented\n"
	m, e := diffSets([]string{"config", "state"}, inventoryNames(control))
	if len(m) != 1 || m[0] != "state" {
		t.Errorf("positive control: want the dropped package reported, got %v", m)
	}
	if len(e) != 1 || e[0] != "nosuchpkg" {
		t.Errorf("positive control: want the invented package reported, got %v", e)
	}
}

// ---------------------------------------------------------------------
// 4. The one client/server route mismatch that is decidable by exact
//    string comparison stays described the way it actually is
// ---------------------------------------------------------------------

const (
	clientVersionCall  = `getVersion: () => request("/version")`
	serverVersionRoute = `r.Get("/system/version", h.systemVersion)`
	readmeVersionClaim = "`/api/v1/version`"
)

func TestREADMEVersionRouteMismatchIsStillReal(t *testing.T) {
	client, err := os.ReadFile(Path(filepath.Join("ui", "shared", "src", "api", "client.ts")))
	if err != nil {
		t.Fatalf("read client.ts: %v", err)
	}
	router, err := os.ReadFile(Path(filepath.Join("apps", "common", "webhost", "router.go")))
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}

	callsBareVersion := strings.Contains(string(client), clientVersionCall)
	servesSystemVersion := strings.Contains(string(router), serverVersionRoute)
	mismatch := callsBareVersion && servesSystemVersion
	readmeSaysMismatch := strings.Contains(readREADME(t), readmeVersionClaim)

	switch {
	case mismatch && !readmeSaysMismatch:
		t.Errorf("the browser client still calls %s while the router only serves %s, and README.md no longer says so", clientVersionCall, serverVersionRoute)
	case !mismatch && readmeSaysMismatch:
		t.Errorf("README.md still describes the /api/v1/version mismatch, but the client (%v) or the router (%v) has moved; re-derive the claim rather than leaving it", callsBareVersion, servesSystemVersion)
	}

	// Positive control: both directions of that switch fire on the
	// inputs that should trip them.
	for _, tc := range []struct {
		name              string
		mismatch, claimed bool
		wantComplaint     bool
	}{
		{"drift hidden", true, false, true},
		{"claim outlived the drift", false, true, true},
		{"honest, drift present", true, true, false},
		{"honest, drift gone", false, false, false},
	} {
		got := (tc.mismatch && !tc.claimed) || (!tc.mismatch && tc.claimed)
		if got != tc.wantComplaint {
			t.Errorf("positive control %q: complaint=%v, want %v", tc.name, got, tc.wantComplaint)
		}
	}
}

// ---------------------------------------------------------------------
// 5. The README keeps saying "uncertified" for exactly as long as the
//    generated matrix still has an unexecuted operator cell in it
// ---------------------------------------------------------------------

const uncertifiedPhrase = "build-supported and uncertified"

func TestREADMECertificationClaimTracksTheMatrix(t *testing.T) {
	matrix, err := os.ReadFile(Path(filepath.Join("docs", "conformance", "phase-4-matrix.md")))
	if err != nil {
		t.Fatalf("read phase-4-matrix.md: %v", err)
	}
	pending := strings.Contains(string(matrix), "| PENDING_OPERATOR |")
	stated := strings.Contains(readREADME(t), uncertifiedPhrase)

	if pending && !stated {
		t.Errorf("the conformance matrix still reports PENDING_OPERATOR cells, so no platform here has been certified on hardware, but README.md no longer says %q", uncertifiedPhrase)
	}
	if !pending && stated {
		t.Errorf("the conformance matrix reports no PENDING_OPERATOR cell any more, so README.md's %q claim needs re-deriving from whatever the acceptance runs actually found", uncertifiedPhrase)
	}

	// Positive control: the assertion is a real biconditional, not a
	// one-sided check that a green matrix would quietly satisfy.
	for _, tc := range []struct{ pending, stated, want bool }{
		{true, false, true},
		{false, true, true},
		{true, true, false},
		{false, false, false},
	} {
		if got := (tc.pending && !tc.stated) || (!tc.pending && tc.stated); got != tc.want {
			t.Errorf("positive control (pending=%v stated=%v): complaint=%v, want %v", tc.pending, tc.stated, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------
// 6. The README's per-platform support tiers come from canonical.json
// ---------------------------------------------------------------------

var tierRow = regexp.MustCompile(`(?m)^\|\s*([A-Za-z][A-Za-z ]*[A-Za-z])\s*\|\s*Tier ([ABC])\s*\|`)

func tiersFromTable(regionText string) map[string]string {
	out := map[string]string{}
	for _, m := range tierRow.FindAllStringSubmatch(regionText, -1) {
		out[strings.TrimSpace(m[1])] = m[2]
	}
	return out
}

func TestREADMESupportTiersMatchCanonicalJSON(t *testing.T) {
	documented := tiersFromTable(region(t, readREADME(t), "SUPPORT-MODEL"))
	canonical := MustLoad()

	for id, p := range canonical.Platforms {
		got, ok := documented[p.DisplayName]
		if !ok {
			t.Errorf("README.md's support table has no row for %q (canonical.json platform %q)", p.DisplayName, id)
			continue
		}
		if got != p.Tier {
			t.Errorf("README.md puts %s at Tier %s; canonical.json says Tier %s", p.DisplayName, got, p.Tier)
		}
	}

	// Positive control: a table row carrying the wrong tier is caught,
	// and a missing row is caught.
	control := tiersFromTable("| TrueNAS | Tier C | wrong |\n")
	if control["TrueNAS"] != "C" {
		t.Fatalf("positive control: the extractor did not read the control row: %v", control)
	}
	if control["TrueNAS"] == canonical.Platforms["truenas"].Tier {
		t.Error("positive control: the control row was supposed to disagree with canonical.json and does not, so this comparison proves nothing")
	}
	if _, ok := control["Unraid"]; ok {
		t.Error("positive control: the extractor invented a row that is not in the control text")
	}
}
