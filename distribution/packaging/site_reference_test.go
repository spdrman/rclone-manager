package packaging

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docs/site/reference.html sets out to be the whole surface in one place:
// every screen, every command, every flag. That is a promise a page
// cannot keep on its own, and the way it breaks is not a wrong sentence,
// it is a command that lands and never gets a row.
//
// The README's own command table has the same job and is already held to
// main.go's dispatch table by readme_claims_test.go. This does the same
// for the site's, reusing that file's extractors on purpose: the two
// pages then cannot disagree with the binary in different directions,
// because they are both compared against the same one thing.
//
// The reason it is a separate file rather than another case in there is
// only which document it reads. Everything else, including the positive
// controls below, follows the same rule that file states: a check that
// only ever asserts an absence has to prove it can fire.

func referencePath() string { return Path("docs/site/reference.html") }

func readReference(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(referencePath())
	if err != nil {
		t.Fatalf("read docs/site/reference.html: %v", err)
	}
	return string(data)
}

// htmlRegion is region()'s counterpart for an HTML document. The marker
// spelling is deliberately identical, so somebody who has read one of
// these files recognises it in the other.
func htmlRegion(t *testing.T, doc, name string) string {
	t.Helper()
	begin := "<!-- BEGIN " + name + " -->"
	end := "<!-- END " + name + " -->"
	i := strings.Index(doc, begin)
	j := strings.Index(doc, end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("docs/site/reference.html has no %s ... %s region; this test reads that region, so removing it removes the check", begin, end)
	}
	return doc[i+len(begin) : j]
}

// The first cell of a row, when that cell is a single <code> span. The
// group headers in these tables are <td colspan="3"> carrying prose, so
// they match nothing here and are skipped without being listed as
// exceptions, which is what keeps this extractor honest as the tables
// grow section headings.
var htmlRowCode = regexp.MustCompile(`<tr><td><code>([^<]+)</code></td>`)

func commandsFromHTMLTable(regionText string) []string {
	var out []string
	for _, m := range htmlRowCode.FindAllStringSubmatch(regionText, -1) {
		out = append(out, strings.Fields(m[1])[0])
	}
	sort.Strings(out)
	return out
}

func TestTheSiteReferenceDocumentsExactlyTheRegisteredCommands(t *testing.T) {
	src, err := os.ReadFile(Path("core/cmd/backupd/main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	registered := registeredCommands(t, string(src))
	if len(registered) == 0 {
		t.Fatal("read no commands out of main.go's dispatch table")
	}

	documented := commandsFromHTMLTable(htmlRegion(t, readReference(t), "CLI-COMMANDS"))
	missing, extra := diffSets(registered, documented)
	if len(missing) > 0 {
		t.Errorf("docs/site/reference.html's command table omits commands the binary registers: %v.\n"+
			"That page says it is every command, so a command with no row there is the page lying rather than the page being short.", missing)
	}
	if len(extra) > 0 {
		t.Errorf("docs/site/reference.html's command table names commands the binary does not register: %v", extra)
	}
}

func TestTheSiteReferenceExtractorCanActuallyFail(t *testing.T) {
	// The positive control, and it has to cover both halves: a table that
	// drops one command and invents one produces exactly one complaint of
	// each kind. Without this, the test above also passes against an
	// extractor that reads zero rows out of every table it is given.
	control := `
      <table>
        <tbody>
          <tr><td colspan="3"><strong>a group heading, not a command</strong></td></tr>
          <tr><td><code>run</code></td><td>one cycle</td></tr>
          <tr><td><code>frobnicate</code></td><td>not a command</td></tr>
        </tbody>
      </table>`
	got := commandsFromHTMLTable(control)
	if len(got) != 2 || got[0] != "frobnicate" || got[1] != "run" {
		t.Fatalf("the extractor read %v; it should read exactly the two <code> rows and skip the colspan heading", got)
	}
	missing, extra := diffSets([]string{"run", "version"}, got)
	if len(missing) != 1 || missing[0] != "version" {
		t.Errorf("want exactly the dropped command reported, got %v", missing)
	}
	if len(extra) != 1 || extra[0] != "frobnicate" {
		t.Errorf("want exactly the invented command reported, got %v", extra)
	}
}

func TestTheSiteReferenceRegionMarkersAreThere(t *testing.T) {
	// Both of them, and the installer one too even though this package
	// does not read it: its own check lives in the installer's Python
	// suite, and a marker deleted here would take that check with it
	// silently, on a change nobody would think to run that suite for.
	doc := readReference(t)
	for _, name := range []string{"CLI-COMMANDS", "INSTALLER-FLAGS"} {
		if !strings.Contains(doc, "<!-- BEGIN "+name+" -->") || !strings.Contains(doc, "<!-- END "+name+" -->") {
			t.Errorf("docs/site/reference.html has lost its %s region markers; the checks that hold that page to the code read them", name)
		}
	}
}

// ---------------------------------------------------------------------
// 4. Every route App.tsx actually mounts has a documented screen
// ---------------------------------------------------------------------
//
// The web interface cannot be generated the way the command and flag
// tables above are: there is no dispatch table for a screen, only a
// component tree. The realistic floor is narrower and it is this: every
// route App.tsx mounts is named in a map below, and the section or
// anchor that name points at actually exists in the file it names. A
// route added to App.tsx and left out of the map fails as a missing
// route; a route removed from App.tsx and left in the map fails too,
// the same way declaredAbsentPaths in readme_claims_test.go is made to
// expire on its own rather than silently cover for a page that moved on.

var routePath = regexp.MustCompile(`path="([^"]+)"`)

// routesInAppTSX is every distinct route path ui/shared/src/App.tsx
// mounts, authenticated or not. Two literal Routes, in two separate
// <Routes> blocks, both spell their path "*" — the unauthenticated
// catch-all that renders LoginPage, and the authenticated catch-all that
// redirects home — and a regex over the whole file cannot tell them
// apart. Both are covered by the single "*" exemption below, for the
// two different, stated reasons.
func routesInAppTSX(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(Path("ui/shared/src/App.tsx"))
	if err != nil {
		t.Fatalf("read ui/shared/src/App.tsx: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range routePath.FindAllStringSubmatch(string(src), -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

type documentedRoute struct {
	file, anchor string
}

// routeSections is where every route App.tsx mounts is documented. Not
// every one gets its own reference.html section: "/sets/new" opens the
// six-step wizard first-run.html already walks screen by screen, and
// pointing at its first step is truer than a second, shorter retelling
// on this page.
var routeSections = map[string]documentedRoute{
	"/":                           {"reference.html", "web-dashboard"},
	"/sets":                       {"reference.html", "web-sets"},
	"/sets/new":                   {"first-run.html", "step1"},
	"/sets/:source/:set":          {"reference.html", "web-set-detail"},
	"/backups":                    {"reference.html", "web-backups"},
	"/backups/:source/:set/:name": {"reference.html", "web-backup-detail"},
	"/activity":                   {"reference.html", "web-activity"},
	"/quarantine":                 {"reference.html", "web-quarantine"},
	"/settings":                   {"reference.html", "web-settings"},
	"/catalog-recovery":           {"reference.html", "web-catalog"},
	"/enroll":                     {"first-run.html", "enrol"},
}

// routeExemptions are routes deliberately left out of routeSections,
// each with the reason it is not a screen a reader looks something up
// by. "*" is the only one: a redirect home for an authenticated request
// that matched nothing (not a screen at all) and, in the other <Routes>
// block, the unauthenticated catch-all that renders LoginPage, which
// first-run.html#again already pictures and describes.
var routeExemptions = map[string]string{
	"*": "two different catch-alls share this literal path in App.tsx: an authenticated redirect home, which is not a screen, and the unauthenticated fallback that renders LoginPage, pictured at first-run.html#again",
}

func hasHTMLAnchor(doc, id string) bool {
	return strings.Contains(doc, `id="`+id+`"`)
}

func TestTheSiteReferenceDocumentsExactlyTheAppsRoutes(t *testing.T) {
	routes := routesInAppTSX(t)
	if len(routes) == 0 {
		t.Fatal("read no <Route path=\"...\"> out of ui/shared/src/App.tsx")
	}

	documented := make([]string, 0, len(routeSections))
	for r := range routeSections {
		documented = append(documented, r)
	}
	for r := range routeExemptions {
		documented = append(documented, r)
	}
	sort.Strings(documented)

	missing, extra := diffSets(routes, documented)
	if len(missing) > 0 {
		t.Errorf("App.tsx mounts routes that routeSections/routeExemptions in site_reference_test.go does not account for: %v. "+
			"A route with nobody documenting it is exactly the gap this test exists to close.", missing)
	}
	if len(extra) > 0 {
		t.Errorf("routeSections/routeExemptions in site_reference_test.go names routes App.tsx no longer mounts: %v. "+
			"That entry is now covering nothing; remove it the way readme_claims_test.go's declaredAbsentPaths asks its own stale entries to be removed.", extra)
	}

	// Every exemption still has to name a route App.tsx actually mounts,
	// for the same reason readme_claims_test.go re-checks
	// declaredAbsentPaths: an exemption nothing matches any more is
	// silently covering for whatever now collides with its name.
	routeSet := map[string]bool{}
	for _, r := range routes {
		routeSet[r] = true
	}
	for r := range routeExemptions {
		if !routeSet[r] {
			t.Errorf("routeExemptions names %q, which App.tsx no longer mounts; the exemption is stale", r)
		}
	}
}

func TestTheSiteReferenceRouteAnchorsExist(t *testing.T) {
	docs := map[string]string{}
	for _, section := range routeSections {
		if _, ok := docs[section.file]; ok {
			continue
		}
		data, err := os.ReadFile(Path("docs/site/" + section.file))
		if err != nil {
			t.Fatalf("read docs/site/%s: %v", section.file, err)
		}
		docs[section.file] = string(data)
	}
	for route, section := range routeSections {
		if !hasHTMLAnchor(docs[section.file], section.anchor) {
			t.Errorf("route %q is mapped to %s#%s, which does not exist; the section it once named moved or was renamed", route, section.file, section.anchor)
		}
	}
}

func TestTheSiteReferenceRouteExtractorsCanActuallyFail(t *testing.T) {
	// routesInAppTSX's regex, over a control document naming three
	// routes, has to read exactly three, in the two-Routes-blocks shape
	// the real file uses.
	control := `
      <Routes>
        <Route path="/enroll" element={<A />} />
        <Route path="*" element={<B />} />
      </Routes>
      <Routes>
        <Route
          path="/widgets"
        />
      </Routes>`
	seen := map[string]bool{}
	var got []string
	for _, m := range routePath.FindAllStringSubmatch(control, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		got = append(got, m[1])
	}
	sort.Strings(got)
	want := []string{"*", "/enroll", "/widgets"}
	if len(got) != len(want) {
		t.Fatalf("route extractor read %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("route extractor read %v, want %v", got, want)
		}
	}

	// hasHTMLAnchor: present and absent both have to come back right, or
	// TestTheSiteReferenceRouteAnchorsExist could pass by always saying yes.
	if !hasHTMLAnchor(`<h3 id="web-dashboard">Dashboard</h3>`, "web-dashboard") {
		t.Fatal("hasHTMLAnchor missed an id that is there")
	}
	if hasHTMLAnchor(`<h3 id="web-dashboard">Dashboard</h3>`, "web-nowhere") {
		t.Fatal("hasHTMLAnchor found an id that is not there")
	}
}
