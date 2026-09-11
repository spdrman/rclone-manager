package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/backupdproject/backupd/core/service"
)

// Issue #571's second half: the guarantee that a `backup-set create` typed
// on a host finds the engine serving it rests entirely on the CLI and the
// web host naming the SAME journal, and until this check that was two
// constants in two modules with nothing comparing them. It had already
// drifted further than that, because only one of the two copies read
// $STATE_DATABASE, so an operator who moved the journal for the engine
// moved the engine out of the CLI's sight and nothing anywhere said so.
//
// This lives here rather than beside service.StateDatabaseDefault, which
// is where I wrote it first, and the move is the point. The check has to
// read a file in apps/ and a file in core/, and core/ may not depend on
// apps/ in any direction: scripts/architecture/check-core-dependency-rule.sh
// proves that by deleting apps/ outright and running core's tests, which
// turned a missing file into a hard failure of the whole gate. apps/generic
// already imports core, so reading downward from here is the direction the
// layering allows, the same way apps/common/webhost/contract_test.go reads
// the contract it holds its handlers to.
var commandsThatNameTheJournal = []string{
	filepath.Join("main.go"),
	filepath.Join("..", "..", "..", "..", "core", "cmd", "backupd", "backupset.go"),
}

// TestBothCommandsTakeTheJournalFromThisOneDefinition goes red if the two
// surfaces part again.
//
// It reads the flag declaration itself rather than comparing two constants,
// because the drift that actually happened was not two different literals:
// it was one literal each, identical, with only one of them wrapped in an
// environment lookup. A test that compared the constants would have been
// green throughout.
func TestBothCommandsTakeTheJournalFromThisOneDefinition(t *testing.T) {
	for _, path := range commandsThatNameTheJournal {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v\nThis test names the two files that declare --state-database; if one of them moved, point it at the new path rather than deleting the check.", path, err)
		}

		declarations := 0
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 || !isStringLiteral(call.Args[0], "state-database") {
				return true
			}
			declarations++
			if !isCallTo(call.Args[1], "service", "StateDatabaseDefault") {
				t.Errorf("%s declares --state-database with a default of its own (%s) rather than service.StateDatabaseDefault(); the CLI and the web host then name different journals, and a `backup-set create` stops finding the engine it is standing next to (#571)",
					path, exprText(fset, call.Args[1]))
			}
			return true
		})
		if declarations != 1 {
			t.Errorf("%s declares --state-database %d times, want exactly 1: a second declaration is a second answer to which deployment this process is about", path, declarations)
		}

		// And neither file may keep a journal path of its own beside the
		// flag. Two constants that happen to match today is exactly the
		// state this check exists to end.
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for _, value := range spec.Values {
				if isStringLiteral(value, service.DefaultStateDatabase) {
					t.Errorf("%s declares its own copy of the packaged journal path %q; there is one definition of it (service.DefaultStateDatabase) and this is how the two came to disagree about $%s",
						path, service.DefaultStateDatabase, service.StateDatabaseEnv)
				}
			}
			return true
		})
	}
}

func isStringLiteral(e ast.Expr, want string) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	got, err := strconv.Unquote(lit.Value)
	return err == nil && got == want
}

func isCallTo(e ast.Expr, pkg, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// exprText renders an expression the way it was written, so a failure names
// what the file actually says rather than an AST node type.
func exprText(fset *token.FileSet, e ast.Expr) string {
	start := fset.Position(e.Pos())
	end := fset.Position(e.End())
	return start.Filename + ":" + strconv.Itoa(start.Line) + " columns " +
		strconv.Itoa(start.Column) + "-" + strconv.Itoa(end.Column)
}
