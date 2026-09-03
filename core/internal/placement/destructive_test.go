package placement

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the destructive-safety half of #238, and it makes its
// claims about the SOURCE rather than about a run.
//
// internal/lifecycle/remotedelete.go and internal/retention/prune.go both
// carry suites that prove their one dangerous call refuses in every unsafe
// world. This package has those too (sourcedelete_test.go). What is here
// instead is the claim those suites cannot make by running: that there is
// no OTHER way out. A test that drives the engine can only ever prove the
// paths it thought to drive; parsing the package proves there are no
// others.

func packageFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatal("no source files were parsed, so this test proved nothing")
	}
	return out
}

// TestThisPackageHasNoDeletePathOfItsOwn is the structural half of "there
// is no path where a source is deleted against an unverified destination".
//
// The engine removes bytes through exactly two seams, LocalStore.Remove and
// MediumStore.DeleteObject, and both are behind guards. A direct
// os.Remove, os.RemoveAll or os.Truncate anywhere in this package would be
// a third way out that no guard covers and no test would think to drive.
//
// os.Lstat, os.Open and friends are fine and are used: reading is not the
// hazard. The list below is the mutating half of the os package that could
// destroy an artifact.
func TestThisPackageHasNoDeletePathOfItsOwn(t *testing.T) {
	forbidden := map[string]bool{
		"Remove": true, "RemoveAll": true, "Truncate": true,
		"Rename": true, "Create": true, "WriteFile": true, "OpenFile": true,
	}
	for name, file := range packageFiles(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" || !forbidden[sel.Sel.Name] {
				return true
			}
			t.Errorf("%s calls os.%s directly; every byte this package destroys or overwrites must go through LocalStore or MediumStore, which are the only two seams a guard sits on",
				name, sel.Sel.Name)
			return true
		})
	}
}

// TestTheSourceDeleteHasExactlyOneCaller pins the shape the whole safety
// argument rests on: remove() is reached from deleteSource and from
// nowhere else, and deleteSource is reached only from the phase the
// transition table says authorises it.
//
// A second caller of remove() would be a second ordering, decided
// somewhere else, which is exactly the argument artifactstore's package
// doc makes about why there is no Move primitive.
func TestTheSourceDeleteHasExactlyOneCaller(t *testing.T) {
	callers := map[string][]string{}
	for name, file := range packageFiles(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "e" {
					callers[sel.Sel.Name] = append(callers[sel.Sel.Name], name+":"+fn.Name.Name)
				}
				return true
			})
		}
	}

	for _, tc := range []struct {
		method string
		want   string
	}{
		{"remove", "deleteSource"},
		{"guardSourceDelete", "deleteSource"},
		{"proveLocalSourceSafe", "guardSourceDelete"},
		{"proveMediumSourceSafe", "guardSourceDelete"},
	} {
		got := callers[tc.method]
		if len(got) != 1 || !strings.HasSuffix(got[0], ":"+tc.want) {
			t.Errorf("e.%s is called from %v; it must be called from %s and nothing else", tc.method, got, tc.want)
		}
	}
}

// TestDiscardDestinationNeverTouchesTheSource is the other direction. The
// cleanup path is the one delete in this package that is NOT behind
// guardSourceDelete, on the grounds that it removes an object this move
// itself created at a key this move itself computed. That grounds only
// holds while it addresses the destination, so this pins it: every locator
// discardDestination hands to a store comes from mv.DestinationKey.
func TestDiscardDestinationNeverTouchesTheSource(t *testing.T) {
	var found bool
	for name, file := range packageFiles(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "discardDestination" {
				continue
			}
			found = true
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name != "SourceLocation" && sel.Sel.Name != "SourceMedium" && sel.Sel.Name != "SourcePlacementID" {
					return true
				}
				t.Errorf("%s: discardDestination reads mv.%s; the cleanup path is unguarded precisely because it only ever addresses the destination",
					name, sel.Sel.Name)
				return true
			})
		}
	}
	if !found {
		t.Fatal("discardDestination was not found, so this test proved nothing")
	}
}
