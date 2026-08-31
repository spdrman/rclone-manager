package packaging

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
//   - The browser client's request paths, which used to be listed here
//     as "not decidable by reading TypeScript string concatenation".
//     They are decidable, and they are now decided:
//     scripts/api/check-client-paths.sh reduces every path client.ts
//     builds and requires each to be an operation api/v1/openapi.json
//     declares, with ten mutation controls in scripts/api/selftest.sh
//     proving it can fail (#211). That gate belongs there rather than
//     here, because it is about the API contract rather than about this
//     README. The one claim about them that IS this file's business,
//     getVersion's route, is checked below.
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

	// The suppression has to expire on its own. missingBacktickedPaths
	// returns before its os.Stat for anything in this map, so once a
	// declared-absent path lands the README keeps describing it as
	// missing and nothing fires, and the entry is precisely what stops
	// anyone noticing. Two of the three name paths that are actively
	// expected to appear, and #122 is open against the UPK proof right
	// now. This cannot produce a false positive: the map is this file's
	// own declaration, so a red here means the declaration is stale.
	for tok, reason := range declaredAbsentPaths {
		if _, err := os.Stat(Path(strings.TrimSuffix(tok, "/"))); err == nil {
			t.Errorf("`%s` now exists, but readme_claims_test.go still declares it absent: %q. The README's claim of absence needs re-deriving against what actually landed, and this entry needs removing, or the path stays exempt from the check for as long as the entry survives.", tok, reason)
		}
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
	clientVersionCall     = `getVersion: () => request("/version")`
	clientVersionRepaired = `getVersion: () => request<WireVersionResponse>("/system/version")`
	clientVersionAnchor   = "getVersion:"
	serverVersionRoute    = `r.Get("/system/version", h.systemVersion)`
	readmeVersionClaim    = "`/api/v1/version`"
)

// versionClaimComplaints decides, from the three documents themselves,
// whether README.md still describes the /api/v1/version mismatch the way
// it actually is, and returns one complaint per problem.
//
// It takes file contents rather than reading them so the positive
// control below can drive this function, this switch and these exact
// needles over planted text. The control it replaces re-implemented the
// decision inline and compared it against a hand-written truth table, so
// it passed unchanged when either arm of the switch was deleted and
// could not notice an extractor that had gone blind.
//
// The first two complaints are that liveness oracle. An exact-string
// needle that stops matching is indistinguishable from a repair unless
// something else says the code is still there: prettier reformatting
// client.ts, or a refactor of router.go's route registration, pins the
// mismatch at false, which reads as "the drift is gone", and the natural
// repair is to delete the README sentence, after which the check sits at
// false/false and can never fire again.
func versionClaimComplaints(clientSrc, routerSrc, readme string) []string {
	var out []string
	callsBareVersion := strings.Contains(clientSrc, clientVersionCall)
	callsSystemVersion := strings.Contains(clientSrc, clientVersionRepaired)
	servesSystemVersion := strings.Contains(routerSrc, serverVersionRoute)
	claimed := strings.Contains(readme, readmeVersionClaim)

	if strings.Contains(clientSrc, clientVersionAnchor) && !callsBareVersion && !callsSystemVersion {
		out = append(out, fmt.Sprintf("the browser client still declares %s, but in neither form this check knows (%s or %s), so the needle has stopped matching and every answer below it is guesswork rather than a repair", clientVersionAnchor, clientVersionCall, clientVersionRepaired))
	}
	if !servesSystemVersion {
		out = append(out, fmt.Sprintf("the router no longer registers %s, so this check can no longer tell a repaired client from an unrecognised one; re-derive the needle before trusting anything it says", serverVersionRoute))
	}

	mismatch := callsBareVersion && servesSystemVersion
	switch {
	case mismatch && !claimed:
		out = append(out, fmt.Sprintf("the browser client still calls %s while the router only serves %s, and README.md no longer says so", clientVersionCall, serverVersionRoute))
	case !mismatch && claimed:
		out = append(out, fmt.Sprintf("README.md still describes the /api/v1/version mismatch, but the client (calls the bare path: %v) or the router (serves the system path: %v) has moved; re-derive the claim rather than leaving it", callsBareVersion, servesSystemVersion))
	}
	return out
}

func TestREADMEVersionRouteMismatchIsStillReal(t *testing.T) {
	client, err := os.ReadFile(Path(filepath.Join("ui", "shared", "src", "api", "client.ts")))
	if err != nil {
		t.Fatalf("read client.ts: %v", err)
	}
	router, err := os.ReadFile(Path(filepath.Join("apps", "common", "webhost", "router.go")))
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}

	if complaints := versionClaimComplaints(string(client), string(router), readREADME(t)); len(complaints) > 0 {
		t.Errorf("README.md's /api/v1/version claim:\n  %s", strings.Join(complaints, "\n  "))
	}

	// Positive control: the same function, over planted client, router
	// and README text. Every row drives the real needles and the real
	// switch, so deleting either arm above, or breaking either
	// extractor, reds a row here.
	const (
		bare     = "export const api = {\n  " + clientVersionCall + ",\n};\n"
		repaired = "export const api = {\n  " + clientVersionRepaired + ",\n};\n"
		renamed  = "export const api = {\n  getVersion: () => request(\"/v2/version\"),\n};\n"
		serves   = "func routes(r chi.Router) {\n\t" + serverVersionRoute + "\n}\n"
		moved    = "func routes(r chi.Router) {\n\tr.Get(systemVersionPath, h.systemVersion)\n}\n"
		says     = "The browser client asks for " + readmeVersionClaim + " and nothing serves it.\n"
		silent   = "The browser client and the web host agree on every route.\n"
	)
	for _, tc := range []struct {
		name                   string
		client, router, readme string
		want                   []string
	}{
		{"drift hidden", bare, serves, silent, []string{"no longer says so"}},
		{"claim outlived the drift", repaired, serves, says, []string{"has moved"}},
		{"honest, drift present", bare, serves, says, nil},
		{"honest, drift gone", repaired, serves, silent, nil},
		{"the client needle went blind", renamed, serves, says, []string{"stopped matching", "has moved"}},
		{"the router needle went blind", bare, moved, says, []string{"no longer registers", "has moved"}},
	} {
		got := versionClaimComplaints(tc.client, tc.router, tc.readme)
		if len(got) != len(tc.want) {
			t.Errorf("positive control %q: got %d complaint(s), want %d: %v", tc.name, len(got), len(tc.want), got)
			continue
		}
		for i, want := range tc.want {
			if !strings.Contains(got[i], want) {
				t.Errorf("positive control %q: complaint %d reads %q, want it to say %q", tc.name, i, got[i], want)
			}
		}
	}
}

// ---------------------------------------------------------------------
// 5. The README keeps saying "uncertified" for exactly as long as the
//    generated matrix still has an unexecuted operator cell in it
// ---------------------------------------------------------------------

const uncertifiedPhrase = "build-supported and uncertified"

// pendingOperatorTotal matches the PENDING_OPERATOR row of the matrix's
// Totals table and captures its count.
//
// The count is the whole point. matrix.go renders a Totals row for every
// outcome unconditionally, zero counts included, so a fully certified
// matrix still renders `| PENDING_OPERATOR | 0 |`, and a check that
// looked for the label alone answered "pending" no matter what the
// matrix said. That made the claim one-sided: the branch for a repaired
// matrix could never execute, and the README could have kept describing
// an uncertified build long after certification. The body table
// abbreviates the cell as OPERATOR, so this row is the only place in the
// document that can decide the question.
var pendingOperatorTotal = regexp.MustCompile(`\|\s*PENDING_OPERATOR\s*\|\s*(\d+)\s*\|`)

// certificationComplaints decides, from the matrix and the README
// themselves, whether the README's certification claim still tracks the
// generated matrix. A missing Totals row is its own failure with its own
// message: it means this check has stopped seeing the table, which is
// not the same as a count of zero and must not be read as one.
func certificationComplaints(matrix, readme string) []string {
	m := pendingOperatorTotal.FindStringSubmatch(matrix)
	if m == nil {
		return []string{"the conformance matrix has no `| PENDING_OPERATOR | <n> |` row in its Totals table; matrix.go renders that row for every outcome unconditionally, so its absence means this check has stopped seeing the table rather than that nothing is pending"}
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return []string{fmt.Sprintf("the matrix's PENDING_OPERATOR total reads %q, which is not a count this check can compare: %v", m[1], err)}
	}
	pending := n > 0
	stated := strings.Contains(readme, uncertifiedPhrase)
	switch {
	case pending && !stated:
		return []string{fmt.Sprintf("the conformance matrix still reports %d PENDING_OPERATOR cell(s), so no platform here has been certified on hardware, but README.md no longer says %q", n, uncertifiedPhrase)}
	case !pending && stated:
		return []string{fmt.Sprintf("the conformance matrix reports no PENDING_OPERATOR cell any more, so README.md's %q claim needs re-deriving from whatever the acceptance runs actually found", uncertifiedPhrase)}
	}
	return nil
}

func TestREADMECertificationClaimTracksTheMatrix(t *testing.T) {
	matrix, err := os.ReadFile(Path(filepath.Join("docs", "conformance", "phase-4-matrix.md")))
	if err != nil {
		t.Fatalf("read phase-4-matrix.md: %v", err)
	}
	if complaints := certificationComplaints(string(matrix), readREADME(t)); len(complaints) > 0 {
		t.Errorf("README.md's certification claim:\n  %s", strings.Join(complaints, "\n  "))
	}

	// Positive control: the same function over planted Totals tables,
	// including the zero-count table the old substring test read as
	// pending. Both arms of the biconditional fire, and a table this
	// check can no longer read says so in its own words.
	const (
		twenty = "### Totals\n\n| Outcome | Cells |\n|---|---|\n| PASS | 73 |\n| PENDING_OPERATOR | 20 |\n| FAIL | 0 |\n"
		zero   = "### Totals\n\n| Outcome | Cells |\n|---|---|\n| PASS | 93 |\n| PENDING_OPERATOR | 0 |\n| FAIL | 0 |\n"
		gone   = "### Totals\n\n| Outcome | Cells |\n|---|---|\n| PASS | 93 |\n| FAIL | 0 |\n"
		says   = "Every provider here is " + uncertifiedPhrase + " until an operator runs the procedure.\n"
		quiet  = "Every provider here has been certified on real hardware.\n"
	)
	for _, tc := range []struct {
		name            string
		matrix, readme  string
		wantOneMentions string
		wantComplaint   bool
	}{
		{"pending, and the README stopped saying so", twenty, quiet, "no longer says", true},
		{"certified, and the README still says uncertified", zero, says, "needs re-deriving", true},
		{"pending and stated", twenty, says, "", false},
		{"certified and silent", zero, quiet, "", false},
		{"the Totals row went missing", gone, says, "stopped seeing the table", true},
	} {
		got := certificationComplaints(tc.matrix, tc.readme)
		if tc.wantComplaint != (len(got) > 0) {
			t.Errorf("positive control %q: complaints %v, want a complaint: %v", tc.name, got, tc.wantComplaint)
			continue
		}
		if tc.wantComplaint && !strings.Contains(got[0], tc.wantOneMentions) {
			t.Errorf("positive control %q: complaint reads %q, want it to say %q", tc.name, got[0], tc.wantOneMentions)
		}
	}
}

// ---------------------------------------------------------------------
// 6. The README's per-platform support tiers come from the files that
//    declare them, in both directions
// ---------------------------------------------------------------------

var tierRow = regexp.MustCompile(`(?m)^\|\s*([A-Za-z][A-Za-z ]*[A-Za-z])\s*\|\s*Tier ([ABC])\s*\|`)

func tiersFromTable(regionText string) map[string]string {
	out := map[string]string{}
	for _, m := range tierRow.FindAllStringSubmatch(regionText, -1) {
		out[strings.TrimSpace(m[1])] = m[2]
	}
	return out
}

// readmeTierRowName is the name the README's support table gives a
// declared platform where that differs from the display name the JSON
// carries. The README writes the vendor name out in the two rows the
// JSON abbreviates, so matching on displayName alone would report those
// two as undocumented and the README's own rows as invented.
var readmeTierRowName = map[string]string{
	"generic": "Generic Docker and Linux",
	"ugos":    "UGREEN UGOS Pro",
}

func tierRowName(id, displayName string) string {
	if n, ok := readmeTierRowName[id]; ok {
		return n
	}
	return displayName
}

// declaredTiers is every platform this repository declares a §4A tier
// for, keyed by the name the README's table uses, plus any disagreement
// between the two files that declare them.
//
// It reads both files because canonical.json declares four platforms and
// the README's table carries seven rows: checking canonical.json alone
// left Generic Docker and Linux, Synology DSM and UGREEN UGOS Pro
// covered by nothing at all. conformance.json declares all seven with
// their tiers, and the README's own prose says the tiers come from more
// than one place, so the check reads both and reports it when they
// disagree rather than silently preferring one.
func declaredTiers(canonical Canonical, conf Conformance) (map[string]string, []string) {
	out := map[string]string{}
	for id, p := range conf.Providers {
		out[tierRowName(id, p.DisplayName)] = p.Tier
	}
	var conflicts []string
	for id, p := range canonical.Platforms {
		name := tierRowName(id, p.DisplayName)
		if got, ok := out[name]; ok && got != p.Tier {
			conflicts = append(conflicts, fmt.Sprintf("canonical.json puts %s at Tier %s and conformance.json puts it at Tier %s; the README cannot agree with both, so fix the declaration before reading the table", name, p.Tier, got))
			continue
		}
		out[name] = p.Tier
	}
	sort.Strings(conflicts)
	return out, conflicts
}

// tierComplaints compares the README's table against the declared tiers
// in BOTH directions: a declared platform the table omits or misstates,
// and a row the table carries that nothing in this repository declares.
// The reverse direction is the half that was missing, and it is the half
// three of the seven rows needed.
func tierComplaints(documented, declared map[string]string) []string {
	var out []string
	for name, tier := range declared {
		got, ok := documented[name]
		if !ok {
			out = append(out, fmt.Sprintf("README.md's support table has no row for %q, which this repository declares at Tier %s", name, tier))
			continue
		}
		if got != tier {
			out = append(out, fmt.Sprintf("README.md puts %s at Tier %s; this repository declares Tier %s", name, got, tier))
		}
	}
	for name := range documented {
		if _, ok := declared[name]; !ok {
			out = append(out, fmt.Sprintf("README.md's support table has a row for %q, which neither canonical.json nor conformance.json declares; a support tier with nothing behind it is a claim this repository cannot keep", name))
		}
	}
	sort.Strings(out)
	return out
}

func TestREADMESupportTiersMatchTheDeclaredPlatforms(t *testing.T) {
	declared, conflicts := declaredTiers(MustLoad(), MustLoadConformance())
	if len(conflicts) > 0 {
		t.Errorf("the two files that declare support tiers disagree:\n  %s", strings.Join(conflicts, "\n  "))
	}
	documented := tiersFromTable(region(t, readREADME(t), "SUPPORT-MODEL"))
	if len(documented) == 0 {
		t.Fatal("read no rows out of README.md's SUPPORT-MODEL region; the comparison below would pass vacuously")
	}
	if complaints := tierComplaints(documented, declared); len(complaints) > 0 {
		t.Errorf("README.md's support table:\n  %s", strings.Join(complaints, "\n  "))
	}

	// Positive control: the real extractor and the real comparison, over
	// a table that carries one wrong tier and one platform nothing
	// declares, and over one that drops a declared platform entirely.
	// The old control read a row and stopped, so it exercised neither
	// direction of the comparison and could not have noticed that one of
	// them was missing.
	declaredControl := map[string]string{"TrueNAS": "B", "Unraid": "B"}
	for _, tc := range []struct {
		name  string
		table string
		want  []string
	}{
		{
			"one wrong tier and one invented row",
			"| TrueNAS | Tier C | wrong |\n| Unraid | Tier B | right |\n| Nutanix AHV | Tier A | nothing declares this |\n",
			[]string{"puts TrueNAS at Tier C", "has a row for \"Nutanix AHV\""},
		},
		{
			"a declared platform the table dropped",
			"| TrueNAS | Tier B | right |\n",
			[]string{"has no row for \"Unraid\""},
		},
		{
			"a table that agrees",
			"| TrueNAS | Tier B | right |\n| Unraid | Tier B | right |\n",
			nil,
		},
	} {
		got := tierComplaints(tiersFromTable(tc.table), declaredControl)
		if len(got) != len(tc.want) {
			t.Errorf("positive control %q: got %d complaint(s), want %d: %v", tc.name, len(got), len(tc.want), got)
			continue
		}
		for i, want := range tc.want {
			if !strings.Contains(got[i], want) {
				t.Errorf("positive control %q: complaint %d reads %q, want it to say %q", tc.name, i, got[i], want)
			}
		}
	}
}
