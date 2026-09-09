package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/cliname"
)

// The command an operator types is `rbm`, and `backup-manager` goes on
// working. That promise is kept by a symlink beside the binary in the
// image (container/Dockerfile), not by anything in Go, and it holds only
// for as long as this binary behaves identically however it was reached.
//
// Nothing here reads argv[0] today. main() passes os.Args[1:] to run() and
// dispatches on the first word of that, so the filename the operator
// happened to type never enters a decision. That is not a property anybody
// wrote down, though, and it is exactly the kind of property somebody
// undoes for a good local reason: a usage line that names "the command you
// actually typed", a diagnostic prefix taken from the executable, a mode
// selected by invocation name the way busybox does it. Any of those would
// make the alias behave differently from the real name in a way no test in
// this tree would notice, and the operator who finds out is one whose
// script broke after an upgrade.
//
// So this is the check that would have to be deleted first.

// coreRoot is this package's directory two levels up, which is the root of
// the core module. Relative, because a test binary's working directory is
// its own package directory and that is a promise the toolchain makes.
const coreRoot = "../.."

// TestNothingDispatchesOnArgv0 reads every non-test source file under
// core/ and requires os.Args to be used in exactly one shape, os.Args[1:],
// and os.Executable not to be used at all.
//
// The rule is about the SHAPE rather than about an index, because
// `os.Args[0]` is only the obvious spelling. Binding the whole slice first
// (`args := os.Args`) and reading args[0] three functions later is the
// same read with nothing to grep for, and a check that looked for the
// literal index would pass it. os.Args[1:] is the one use this binary has
// and the one use that cannot see the invocation name at all, so anything
// else is either argv[0] or one step from it, and is worth the sentence it
// costs to say why.
//
// It walks core/ rather than this package alone. cliecho composes the
// commands the Web UI prints, internal/obs writes the startup line, and
// either could reach for the executable's own name with the same good
// intentions; the packaging binaries under apps/ are outside this module
// and outside this test, which is said here rather than left as a
// discovery.
//
// Vendored code and testdata are skipped: neither ships in this binary,
// and a fixture that happens to contain the string is not a dispatch.
func TestNothingDispatchesOnArgv0(t *testing.T) {
	files := 0
	err := filepath.WalkDir(coreRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "vendor", ".git", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		checkArgv0(t, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", coreRoot, err)
	}

	// A walk that read nothing would report no violations and pass, which
	// is the failure this whole file exists to make impossible elsewhere.
	if files < 100 {
		t.Fatalf("this walk read only %d source files under %s, which is far fewer than core/ has. It is looking at the wrong tree, and a check that reads nothing agrees with everything.", files, coreRoot)
	}
}

// checkArgv0 is the rule above applied to one file.
func checkArgv0(t *testing.T, path string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	// The one legitimate shape, collected first so the walk below can tell
	// it apart from every other read of the same identifier.
	dispatchSlice := map[ast.Node]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		slice, ok := n.(*ast.SliceExpr)
		if !ok || !isOSArgs(slice.X) || slice.High != nil || slice.Max != nil {
			return true
		}
		if lit, ok := slice.Low.(*ast.BasicLit); ok && lit.Value == "1" {
			dispatchSlice[slice.X] = true
		}
		return true
	})

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return true
		}
		switch sel.Sel.Name {
		case "Args":
			if dispatchSlice[ast.Node(sel)] {
				return true
			}
			t.Errorf("%s:%d reads os.Args in a shape other than os.Args[1:].\n\nThe old command name is kept alive by a symlink beside this binary, so `backup-manager status` and `rbm status` are the same binary reached under two filenames and have to behave identically. Anything that can see argv[0] can make them differ, and the operator who finds out is one whose script broke after an upgrade. Take the argument from the parsed command line instead; if a build genuinely needs to know its own filename, that is a decision to argue in the commit rather than a line to slip past this test.",
				fset.Position(sel.Pos()).Filename, fset.Position(sel.Pos()).Line)
		case "Executable":
			t.Errorf("%s:%d calls os.Executable, which answers the same question os.Args[0] does and has the same problem: see the message above and this file's doc comment.",
				fset.Position(sel.Pos()).Filename, fset.Position(sel.Pos()).Line)
		}
		return true
	})
}

// isOSArgs reports whether e is the selector os.Args.
func isOSArgs(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Args" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "os"
}

// legacyName is the command this binary answered to before 0.3.3, and goes
// on answering to. It is spelled out here rather than derived from
// cliname.Binary because it is a promise about a name that no longer moves:
// the day the constant changes again, `backup-manager` is still the thing
// an operator's four-year-old cron line says, and a test that followed the
// constant would quietly stop making the promise.
const legacyName = "backup-manager"

// TestTheOldNameReachesTheSameBinary builds this command, symlinks it under
// the old name, and requires the two to answer identically.
//
// The rename ships with a symlink in the image and a claim attached to it:
// nothing an operator already automated has to change. The symlink itself
// belongs to container/Dockerfile and cannot be tested from here, but the
// half that can go wrong in Go can be, and it is the half that would fail
// silently. A binary that looked at its own argv[0] would still start,
// still run the right subcommand, and print a different sentence, and the
// only place that shows up is on somebody's terminal after an upgrade.
//
// A symlink rather than a copy, because the copy would prove something
// weaker. Exec through a symlink hands the child the symlink's own path as
// argv[0], which is exactly the arrangement the image makes; a second copy
// of the file would test that two identical binaries behave identically,
// which nobody doubted.
//
// The three argv it drives are chosen for the three ways the name reaches
// an operator: `version` prints it on stdout, no arguments at all prints
// the usage block on stderr, and an unknown command prints the diagnostic
// prefix. Between them every shape in this package is covered, and all
// three exit through a different path.
func TestTheOldNameReachesTheSameBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary; -short is for the runs that cannot afford a compile")
	}

	dir := t.TempDir()
	canonical := filepath.Join(dir, cliname.Binary)
	build := exec.Command("go", "build", "-o", canonical, "./cmd/backup-manager")
	build.Dir = coreRoot
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", cliname.Binary, err, out)
	}

	alias := filepath.Join(dir, legacyName)
	if alias == canonical {
		t.Fatalf("%s and %s are the same path, so this test would compare a binary with itself and pass however it behaved. cliname.Binary is %q; if the product really has gone back to the old name, this test has stopped meaning anything and should say so out loud rather than keep passing.",
			cliname.Binary, legacyName, cliname.Binary)
	}
	if err := os.Symlink(canonical, alias); err != nil {
		t.Fatalf("symlinking %s to %s: %v", alias, canonical, err)
	}

	for _, argv := range [][]string{
		{"version"},
		{},
		{"definitely-not-a-command"},
	} {
		label := strings.Join(argv, " ")
		if label == "" {
			label = "(no arguments)"
		}
		wantCode, wantOut, wantErr := runUnderName(t, canonical, argv)
		gotCode, gotOut, gotErr := runUnderName(t, alias, argv)

		if gotCode != wantCode {
			t.Errorf("%s %s exited %d and %s %s exited %d. The old name is a symlink to the same file, so a script that branches on the exit status has to get the same answer from either.",
				legacyName, label, gotCode, cliname.Binary, label, wantCode)
		}
		if gotOut != wantOut {
			t.Errorf("%s %s and %s %s printed different things on stdout.\nunder %s:\n%s\nunder %s:\n%s",
				legacyName, label, cliname.Binary, label, cliname.Binary, wantOut, legacyName, gotOut)
		}
		if gotErr != wantErr {
			t.Errorf("%s %s and %s %s printed different things on stderr.\nunder %s:\n%s\nunder %s:\n%s",
				legacyName, label, cliname.Binary, label, cliname.Binary, wantErr, legacyName, gotErr)
		}
	}
}

// runUnderName executes path with argv and returns its exit status and both
// streams.
//
// The environment is built rather than inherited, for the reason
// core/tests/compat's runCLI gives at greater length: a developer with
// $BACKUP_MANAGER_API_URL exported would otherwise have both halves of the
// comparison aimed at their own engine.
func runUnderName(t *testing.T, path string, argv []string) (int, string, string) {
	t.Helper()

	cmd := exec.Command(path, argv...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TZ=UTC"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %s %v: %v", path, argv, err)
		}
		code = exitErr.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}
