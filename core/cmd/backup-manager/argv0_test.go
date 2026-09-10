package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The command an operator types is `rbm`, and this binary must answer the
// same way no matter what path it was reached through: the image installs
// it at /rbm, a developer runs it straight out of go build under whatever
// name they chose, and the e2e harness copies it somewhere else again.
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
			t.Errorf("%s:%d reads os.Args in a shape other than os.Args[1:].\n\nThis binary is reached under more than one path - the image installs it at /rbm, a developer runs it straight out of go build under whatever name they chose, and the e2e harness copies it somewhere else again - and it has to behave identically under all of them. Anything that can see argv[0] can make them differ, and the operator who finds out is one whose script broke after an upgrade. Take the argument from the parsed command line instead; if a build genuinely needs to know its own filename, that is a decision to argue in the commit rather than a line to slip past this test.",
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
