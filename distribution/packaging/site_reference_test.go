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
	src, err := os.ReadFile(Path("core/cmd/backup-manager/main.go"))
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
