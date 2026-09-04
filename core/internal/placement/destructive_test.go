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
		want   []string
	}{
		{"remove", []string{"deleteSource"}},
		{"guardSourceDelete", []string{"deleteSource"}},
		{"proveLocalSourceSafe", []string{"guardSourceDelete"}},
		{"proveMediumSourceSafe", []string{"guardSourceDelete"}},
		{"copiesOf", []string{"guardSourceDelete"}},

		// observe is the one entry here that is not part of the delete
		// ordering, and it now has two callers rather than one.
		//
		// Everything above it destroys something or authorises destroying
		// something, and for those "exactly one caller" IS the safety
		// property: a second caller is a second ordering decided somewhere
		// else. observe destroys nothing. It asks a medium whether a
		// restore of one object is in effect, and it is listed here so
		// that the question cannot be asked in a place that then acts on
		// the answer without the guard.
		//
		// verifyCopy is the second caller and it is the archive gate's, in
		// front of Verify: an archived copy cannot earn a class that needs
		// reading it, and refusing before the read is what stops the
		// engine spending a GET and then treating InvalidObjectState as a
		// failed verification worth retrying. It reads and refuses; it
		// deletes nothing. The list is spelled out rather than relaxed to
		// "one or more" so that a third caller still has to be argued for
		// here.
		{"observe", []string{"copiesOf", "verifyCopy"}},
	} {
		got := callers[tc.method]
		want := map[string]bool{}
		for _, w := range tc.want {
			want[w] = true
		}
		ok := len(got) == len(tc.want)
		for _, g := range got {
			matched := false
			for w := range want {
				if strings.HasSuffix(g, ":"+w) {
					matched = true
					delete(want, w)
					break
				}
			}
			if !matched {
				ok = false
			}
		}
		if !ok || len(want) != 0 {
			t.Errorf("e.%s is called from %v; it must be called from exactly %v", tc.method, got, tc.want)
		}
	}
}

// TestTheGuardAsksArchiveExactlyOnceAndOnlyWhereItDecides pins where the
// one call to archive.CheckSourceDelete in this package sits.
//
// internal/archive's own composition guard proves the package MAKES the
// call; this proves it makes it in guardSourceDelete, which is the only
// function that hands deleteSource a target, and nowhere else. A second
// call somewhere convenient would be a second place the answer could be
// computed from different copies, and a call that moved out of the guard
// would be a call the guard's own refusal path no longer covers.
func TestTheGuardAsksArchiveExactlyOnceAndOnlyWhereItDecides(t *testing.T) {
	var callers []string
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
				if !ok || sel.Sel.Name != "CheckSourceDelete" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "archive" {
					callers = append(callers, name+":"+fn.Name.Name)
				}
				return true
			})
		}
	}
	if len(callers) != 1 || !strings.HasSuffix(callers[0], ":guardSourceDelete") {
		t.Fatalf("archive.CheckSourceDelete is called from %v; it must be called from guardSourceDelete and nowhere else, because that is the only function that hands the source delete a target", callers)
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

// engineMethod finds a method on *Engine by name, across the package's
// non-test files.
func engineMethod(files map[string]*ast.File, name string) *ast.FuncDecl {
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != name || fn.Body == nil {
				continue
			}
			return fn
		}
	}
	return nil
}

// journalWritingMethods returns every method on *Engine that can reach a
// write on the move journal, directly or through another method.
//
// PlanMove and AdvanceMove are the whole write surface of MoveJournal;
// Get and ListMoves are reads. The closure is a fixpoint rather than a
// hand-written list so a new helper that writes is classified by what it
// does rather than by whether somebody remembered to add it here.
func journalWritingMethods(files map[string]*ast.File) map[string]bool {
	writes := map[string]bool{"PlanMove": true, "AdvanceMove": true}
	direct := map[string]bool{}
	calls := map[string][]string{}

	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// e.Journal.AdvanceMove(...)
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && writes[sel.Sel.Name] {
					if id, ok := inner.X.(*ast.Ident); ok && id.Name == "e" && inner.Sel.Name == "Journal" {
						direct[fn.Name.Name] = true
					}
					return true
				}
				// e.someHelper(...)
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "e" {
					calls[fn.Name.Name] = append(calls[fn.Name.Name], sel.Sel.Name)
				}
				return true
			})
		}
	}

	for changed := true; changed; {
		changed = false
		for caller, callees := range calls {
			if direct[caller] {
				continue
			}
			for _, callee := range callees {
				if direct[callee] {
					direct[caller] = true
					changed = true
					break
				}
			}
		}
	}
	return direct
}

// TestDeleteSourceWritesNothingToTheJournalBeforeTheGuard pins the one
// argument in this package that was made only in a comment.
//
// deleteSource re-verifies the destination from scratch and then does NOT
// write that fresh result back over the destination's placement, on
// purpose: guardSourceDelete's job is to require what the journal DURABLY
// recorded when it authorised this delete, and the fresh check is the
// separate, independent question of what is true right now. Refreshing the
// row from the check that just ran collapses those two questions into one.
// Every destination clause in the guard, the placement's existence, its
// ACTIVE status, its key, its verification class, its verified-at, its
// hash, would then be satisfied by construction, and not one of them could
// ever fire again.
//
// That is a comment arguing for the ABSENCE of a line, and nothing in the
// suite noticed the line being added: adding it makes the guard agree with
// itself, so every behavioural test stays green. This is the check that
// goes red instead.
//
// The claim is lexical, which is only the same as "before it runs" while
// deleteSource has no function literals, so that is asserted rather than
// assumed. A writer reached through a return is fine and there are several:
// the recopy-or-abandon paths leave the function, so they never reach the
// guard at all.
func TestDeleteSourceWritesNothingToTheJournalBeforeTheGuard(t *testing.T) {
	files := packageFiles(t)
	fn := engineMethod(files, "deleteSource")
	if fn == nil {
		t.Fatal("no (*Engine).deleteSource was found, so this test proved nothing")
	}
	writers := journalWritingMethods(files)
	if !writers["intendSourceDelete"] || !writers["recopyOrAbandon"] {
		t.Fatalf("the journal-write closure came back as %v, which does not even contain the methods that obviously write; the detector is broken, not the code", writers)
	}

	var guardPos token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "guardSourceDelete" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "e" && !guardPos.IsValid() {
				guardPos = sel.Pos()
			}
		}
		if lit, ok := n.(*ast.FuncLit); ok {
			t.Errorf("deleteSource contains a function literal at offset %d; this test reads the body in source order, which only equals execution order while there are none", lit.Pos())
		}
		return true
	})
	if !guardPos.IsValid() {
		t.Fatal("deleteSource does not call e.guardSourceDelete at all, which is a much worse bug than the one this test is looking for")
	}

	// Walk with an ancestor stack so a write can be recognised as leaving
	// the function rather than falling through to the guard.
	var stack []ast.Node
	var seen int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		defer func() { stack = append(stack, n) }()

		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		var what string
		switch x := sel.X.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "e" && x.Sel.Name == "Journal" &&
				(sel.Sel.Name == "AdvanceMove" || sel.Sel.Name == "PlanMove") {
				what = "e.Journal." + sel.Sel.Name
			}
		case *ast.Ident:
			if x.Name == "e" && writers[sel.Sel.Name] && sel.Sel.Name != "deleteSource" {
				what = "e." + sel.Sel.Name
			}
		}
		if what == "" {
			return true
		}
		seen++
		if sel.Pos() > guardPos {
			return true
		}
		for _, anc := range stack {
			if _, ok := anc.(*ast.ReturnStmt); ok {
				return true
			}
		}
		t.Errorf("deleteSource calls %s before e.guardSourceDelete, and not as a return; the guard reads the placement the VERIFIED write recorded, and a write on the way to it satisfies every one of the guard's destination clauses by construction so none of them can fire",
			what)
		return true
	})
	if seen == 0 {
		t.Fatal("no journal write was found anywhere in deleteSource, so this test is not measuring anything")
	}
}

// rootIdent returns the identifier an assignment target is rooted at, so
// rec, rec.Placements and rec.Placements[0] all come back as "rec".
func rootIdent(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		default:
			return ""
		}
	}
}

// TestTheGuardReadsTheRecordDeleteSourceFetchedAndNothingElse is the other
// half of the same argument, and it exists because the first half is not
// enough on its own.
//
// TestDeleteSourceWritesNothingToTheJournalBeforeTheGuard was written
// against the mutation the comment describes, a write-back through
// AdvanceMove, and that mutation turns out to be noisy: AdvanceMove always
// records a phase transition, so the nominal-move test sees the extra hop
// and goes red too. The quiet version of the same mistake never touches
// the journal at all. It edits the record in memory:
//
//	rec = withDestinationPlacement(rec, e.destinationPlacement(mv, src, result))
//
// That has exactly the effect the comment warns about, every destination
// clause satisfied by construction, and it is invisible to a test that
// looks for writes, invisible to the phase walk, and invisible to every
// behavioural test in the suite, because the guard then agrees with the
// check that just ran in every world including the bad ones.
//
// So this pins the provenance instead: the record the guard reads is the
// identifier one e.Journal.Get produced, and nothing reassigns any part of
// it on the way there.
func TestTheGuardReadsTheRecordDeleteSourceFetchedAndNothingElse(t *testing.T) {
	files := packageFiles(t)
	fn := engineMethod(files, "deleteSource")
	if fn == nil {
		t.Fatal("no (*Engine).deleteSource was found, so this test proved nothing")
	}

	var guard *ast.CallExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || guard != nil {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "guardSourceDelete" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "e" {
				guard = call
			}
		}
		return true
	})
	if guard == nil {
		t.Fatal("deleteSource does not call e.guardSourceDelete at all")
	}
	if len(guard.Args) != 4 {
		t.Fatalf("e.guardSourceDelete takes %d arguments here; this test reads the third one as the journal record and has to be updated with the signature", len(guard.Args))
	}
	recArg, ok := guard.Args[2].(*ast.Ident)
	if !ok {
		t.Fatalf("the record handed to e.guardSourceDelete is %T rather than a plain identifier; the guard must be given the record the journal returned, not one composed at the call", guard.Args[2])
	}

	// Every assignment rooted at that identifier, before the guard.
	var fromJournalGet, other int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Pos() > guard.Pos() {
			return true
		}
		var touches bool
		for _, lhs := range assign.Lhs {
			if rootIdent(lhs) == recArg.Name {
				touches = true
			}
		}
		if !touches {
			return true
		}
		src := "something that is not a journal read"
		if len(assign.Rhs) == 1 {
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if inner, ok := sel.X.(*ast.SelectorExpr); ok {
						if id, ok := inner.X.(*ast.Ident); ok && id.Name == "e" && inner.Sel.Name == "Journal" && sel.Sel.Name == "Get" {
							fromJournalGet++
							return true
						}
					}
					src = "e." + sel.Sel.Name
				}
			}
		}
		other++
		t.Errorf("%s is reassigned from %s before it reaches e.guardSourceDelete; the guard's whole job is to require what the journal DURABLY recorded, and a record edited from the check deleteSource just ran satisfies every destination clause by construction",
			recArg.Name, src)
		return true
	})

	if fromJournalGet != 1 {
		t.Errorf("%s is assigned from e.Journal.Get %d times before the guard, want exactly 1; the guard has to read one record, fetched once, from the journal", recArg.Name, fromJournalGet)
	}
	if other == 0 && fromJournalGet == 0 {
		t.Error("no assignment to the guard's record argument was found at all, so this test is not measuring anything")
	}
}
