package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/cliecho"
)

// What this binary calls itself, checked in the one place it can be checked.
//
// The CLI's rename (0.3.3, core/cliecho/cliname.go) moved `backupd`
// to `backupd` and moved the web host to `backupd-web` everywhere AROUND this
// binary: the image ships /backupd-web with the old name symlinked beside it,
// container/compose.yaml and all eleven adapter compose files run /backupd-web,
// distribution/packaging's canonical manifest names it, and the CLI's own
// published exit-code table sends an operator to `backupd-web serve` for the
// same status. What none of that reached was the source of the binary
// itself, which went on printing `backupd-web:` in front of every
// diagnostic and `usage: backupd-web` at the top of its help. So a
// fresh `docker compose logs` printed a name no compose file, no document
// and no other binary in the image used any more, and the operator who
// followed the CLI's own sentence to `backupd-web` met a program that
// introduced itself as something else.
//
// That gap existed because nothing compared the two. The name was a string
// literal here and a constant over there, and a literal cannot go red.
//
// # Why a source walk and not the output
//
// Because "everywhere it names itself" is a claim about every branch, and
// most of these lines are refusals: an unreadable auth store, a --upstream
// that is not absolute, a profile with no gateway to trust. Driving all of
// them through a process would be a fixture per refusal and would still
// only cover the ones somebody remembered. The literals are all right here,
// and reading them is the only check that is exhaustive by construction.
//
// # Why the rule is about paths and not about the word
//
// Three shapes in this tree spell backupd and are NOT a program
// naming itself (cliname.go argues each one at length): the filesystem
// paths packaging mounts and an operator already has on disk
// (/etc/backupd/config), the project, image and compose service
// (ghcr.io/spdrman/backupd), and the User-Agent core sends. A check
// that banned the word outright would have to be suppressed at
// defaultConfigPath on its first run, and a check somebody has to suppress
// is a check somebody deletes.
//
// A path segment is what separates them, and it separates them for a
// reason rather than by luck: a path is preceded by a separator because it
// is a component of something, and a program naming itself never is. So a
// literal may contain backupd immediately after a `/`, and may not
// contain it anywhere else.
//
// # Why the walk crosses into apps/common
//
// Because this binary prints lines composed there, verbatim, into the same
// container log: apps/common/webhost/serve prefixes its shutdown warning,
// and apps/common/auth/local prefixes the first-run enrollment notice that
// is the very first line a new deployment shows anybody. An operator
// reading `docker compose logs` cannot tell which module a line came from
// and should not have to, so "what this binary calls itself" is not a
// property this directory can hold on its own.
//
// Reading a file in a sibling module from here is the same move
// statedatabase_contract_test.go already makes for the journal default,
// and for the same layering reason: apps/generic already depends on
// apps/common and on core, so reading in that direction is the direction
// the dependency rule allows.
//
// apps/synology is deliberately NOT walked. Its literals are payload
// FILENAMES (spk/layout.go's CoreBinaries), and those keep the old
// spelling on purpose, because they name the files the image actually
// contains, symlink included.
var selfNameRoots = []string{
	".",
	filepath.Join("..", "..", "..", "common"),
}

// legacyName is what this binary must no longer call itself, and it is
// spelled out rather than derived from cliecho.Binary on purpose. The day
// that constant moves again, THIS is still the string that must not come
// back, and a check that followed the constant would quietly start
// asserting something else.
const legacyName = "backupd"

// TestNothingPrintsANameThisBinaryDoesNotHave walks the source this binary
// is built from and requires every string literal in it to spell
// backupd only as a path segment.
func TestNothingPrintsANameThisBinaryDoesNotHave(t *testing.T) {
	visited := map[string]bool{}

	for _, root := range selfNameRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", "testdata", "dist", ".git":
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			visited[filepath.ToSlash(path)] = true

			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, unquoteErr := strconv.Unquote(lit.Value)
				if unquoteErr != nil {
					return true
				}
				if namesAnotherProgram(value) {
					t.Errorf("%s:%d spells %q outside a path segment:\n\n%s\n\nThis binary is %s (core/cliecho.WebBinary) and the CLI beside it is %s (cliecho.Binary). Read the name from the constant rather than writing it out, so the two cannot part again; a filesystem path such as /etc/%s/config is exempt and stays a literal, which is the whole distinction cliname.go draws.",
						path, fset.Position(lit.Pos()).Line, legacyName, excerpt(value, legacyName),
						cliecho.WebBinary, cliecho.Binary, legacyName)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	// A walk that reaches nothing passes, and a walk whose roots have
	// moved reaches nothing. These three files are the ones this check was
	// written for, so naming them turns "found no problems" into a claim
	// that something was actually read. Two of them are in the other
	// module, which is also the half of the walk most likely to be the one
	// that quietly stops happening.
	for _, want := range []string{
		"main.go",
		"../../../common/webhost/serve/run.go",
		"../../../common/auth/local/service.go",
	} {
		if !visited[want] {
			t.Errorf("the walk never parsed %s, so a green result here says nothing about it; the roots in selfNameRoots have moved", want)
		}
	}
	if len(visited) < 50 {
		t.Errorf("the walk parsed %d files, which is far fewer than the two modules hold; something is skipping directories it should not", len(visited))
	}
}

// TestNamesAnotherProgramTellsThePathsApart is the positive control for the
// rule above. The walk's whole value is that it can tell a printed name
// from a mounted directory, and a walk that found nothing proves neither
// half of that on its own.
func TestNamesAnotherProgramTellsThePathsApart(t *testing.T) {
	mustFlag := []string{
		legacyName + ": unknown command %q",
		"usage: " + legacyName + "-web <command> [flags]",
		`echo -n "$PASS" | ` + legacyName + "-web auth create-admin",
		"it has no state database to run " + legacyName + " status",
	}
	for _, s := range mustFlag {
		if !namesAnotherProgram(s) {
			t.Errorf("%q was not flagged, so the rule would have let the 0.3.3 gap through", s)
		}
	}

	mustPass := []string{
		"/etc/" + legacyName + "/config/config.yaml",
		"/var/lib/" + legacyName,
		"/" + legacyName + "-web",
		cliecho.WebBinary + ": shutdown complete",
		"/data/state/local-auth.json",
	}
	for _, s := range mustPass {
		if namesAnotherProgram(s) {
			t.Errorf("%q was flagged; the rule has to leave the paths and the image's own filenames alone or somebody will suppress it", s)
		}
	}
}

// namesAnotherProgram reports whether s spells the pre-0.3.3 name anywhere
// other than immediately after a path separator.
func namesAnotherProgram(s string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], legacyName)
		if j < 0 {
			return false
		}
		at := i + j
		if at == 0 || s[at-1] != '/' {
			return true
		}
		i = at + len(legacyName)
	}
}

// excerpt shows the offending lines of a literal rather than the whole of
// it. One of these literals is the entire usage block, and a failure
// message that prints a hundred and fifty lines of help text to say that
// one word in it is wrong is a message nobody reads to the end.
//
// Every matching line rather than the first, because that block spelled
// the name three times in three different senses and a reader who fixes
// the one they were shown would come straight back.
func excerpt(s, needle string) string {
	var found []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			found = append(found, "\t"+strings.TrimSpace(line))
		}
	}
	if len(found) == 0 {
		return "\t" + s
	}
	return strings.Join(found, "\n")
}

// TestUsageIntroducesThisBinaryByName is the half the walk above cannot
// see: that the first line an operator reads names this binary, spelled
// the way the compose file that runs it spells it.
//
// It reads the printed text rather than the source, because the usage block
// is assembled at run time now (one Fprintf over one raw literal, for the
// reason usage() itself gives) and "the constant appears somewhere in the
// function" is not the same claim as "the first line says backupd-web".
func TestUsageIntroducesThisBinaryByName(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	stderr := os.Stderr
	os.Stderr = w
	usage()
	os.Stderr = stderr
	w.Close()

	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	text := string(buf[:n])

	wantFirst := "usage: " + cliecho.WebBinary + " <command> [flags]"
	if first, _, _ := strings.Cut(text, "\n"); first != wantFirst {
		t.Errorf("usage begins %q, want %q; this is the line an operator reads after following the CLI's exit-code table to `%s serve`, and it is the one line that has to agree with the compose file that starts it", first, wantFirst, cliecho.WebBinary)
	}

	// The block also tells an operator to run the CLI (`healthcheck` says
	// why it exists rather than a state database question) and to run this
	// binary again (`auth create-admin`). Both are commands somebody types,
	// so both follow the constants.
	for _, want := range []string{
		cliecho.Binary + " status",
		cliecho.WebBinary + " auth",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("usage never names %q; a help text that tells an operator to run a command spelled differently from the one the image ships is the gap this file exists to close", want)
		}
	}
}

// TestTheWebHostNamesItselfFromTheConstant is the third angle, and the
// only one that observes the shipped program rather than the source it
// was built from or a function called inside the test process.
//
// The walk above proves no literal spells the old name and the usage test
// proves the help block names the constant, and both of those are claims
// about this package. What neither can see is a binary that builds, links
// and then introduces itself differently anyway, which is precisely what
// an operator meets in `docker compose logs`. So this one builds it, runs
// it with a command it does not have, and reads the two lines that come
// back.
//
// It replaced TestNoSourceFileSpellsTheWebHostsNameAsALiteral, which read
// the same directory line by line for two spellings of the old name. The
// AST walk above reads the same files for every literal, and apps/common
// as well, so that check is a strict subset of this file's first test and
// keeping it would have been the same assertion written twice.
func TestTheWebHostNamesItselfFromTheConstant(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary; -short is for the runs that cannot afford a compile")
	}

	// Split, so this file does not contain the exact literal the walk
	// above exists to find. It skips _test.go, so this would not fail the
	// check, but a grep by a human looking for a stray old name would
	// stop here for nothing.
	oldName := legacyName + "-web"

	bin := filepath.Join(t.TempDir(), cliecho.WebBinary)
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	run := exec.Command(bin, "no-such-command")
	out, _ := run.CombinedOutput()
	got := string(out)

	if !strings.Contains(got, cliecho.WebBinary+": unknown command") {
		t.Errorf("the diagnostic does not name cliecho.WebBinary (%q):\n%s", cliecho.WebBinary, got)
	}
	if !strings.Contains(got, "usage: "+cliecho.WebBinary+" <command>") {
		t.Errorf("the usage line does not name cliecho.WebBinary (%q):\n%s", cliecho.WebBinary, got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, oldName+":") || strings.HasPrefix(line, "usage: "+oldName) {
			t.Errorf("the binary still spells the old name in an operator-facing line: %q", line)
		}
	}
}
