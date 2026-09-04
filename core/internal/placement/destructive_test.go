package placement

import (
	"fmt"
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
	return packageFilesIn(t, ".")
}

// packageFilesIn parses one directory's non-test Go files. It is what lets
// the structural guards below be proved against planted source rather than
// only asserted about this package.
func packageFilesIn(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
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
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
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

// rootIdent returns the identifier an expression is rooted at, so rec,
// rec.Placements, rec.Placements[0], rec.Placements[1:], (&rec) and *p all
// come back as the identifier whose memory they reach.
//
// The slice, paren and address-of cases are the ones a review added, and
// they are the whole of finding G4 in one function: rec.Placements[0] and
// ps, where ps := rec.Placements, are the same backing array, and a walk
// that only followed selectors and indexes could see the first and not the
// second.
func rootIdent(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.UnaryExpr:
			if x.Op != token.AND {
				return ""
			}
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
// identifier one e.Journal.Get produced, and nothing on the way there
// reaches its memory.
//
// # Reassignment was never the only way in
//
// The first version of this test asked only about assignments whose target
// was rooted at that identifier, and a review walked straight past it. A
// slice header is not the array behind it, so
//
//	ps := rec.Placements
//	ps[0].Status = state.PlacementActive
//
// edits exactly the row the guard is about to read, while every assignment
// in sight is rooted at ps. Nothing was reassigned, so nothing was seen.
// The same hole takes a struct copy (fresh := rec shares the same backing
// array), a pointer (p := &rec), the builtin copy(), and handing the slice
// to a function that writes through it, which is not an assignment in this
// function at all.
//
// So the rule is about REACH rather than about assignment. Everything that
// can reach the record's memory is tainted, transitively: the identifier
// itself, and anything taking an alias of it. A write through any of them
// before the guard is a violation, and so is passing any of them to a
// function that cannot be shown not to write through it. A call to
// something outside this package counts as writing, because nothing here
// can show otherwise, and on this path the safe answer to "I cannot tell"
// is no.
//
// Proved against planted source below rather than only asserted about this
// package, because a detector nobody has watched fail is exactly the thing
// these four findings were about.
func TestTheGuardReadsTheRecordDeleteSourceFetchedAndNothingElse(t *testing.T) {
	if got := recordProvenanceViolations(t, packageFiles(t)); len(got) > 0 {
		for _, v := range got {
			t.Error(v)
		}
	}

	for _, c := range provenanceControls {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "engine.go"), []byte(c.source), 0o600); err != nil {
				t.Fatalf("writing the planted source: %v", err)
			}
			got := recordProvenanceViolations(t, packageFilesIn(t, dir))
			if c.wantViolation && len(got) == 0 {
				t.Errorf("the detector accepted %s, so a green run against this package proves nothing about that shape", c.name)
			}
			if !c.wantViolation && len(got) > 0 {
				t.Errorf("the detector flagged the compliant shape: %v", got)
			}
		})
	}
}

// provenanceControl is one planted deleteSource the detector is run over.
type provenanceControl struct {
	name          string
	source        string
	wantViolation bool
}

// provenanceHeader is everything a planted control needs around its own
// deleteSource: a journal read, a guard call in the right place, and a
// package-level reader the record is legitimately handed to.
const provenanceHeader = `package placement

import "context"

type Engine struct{ Journal interface {
	Get(ctx context.Context, a string) (Record, error)
} }

type Record struct{ Placements []Placement }
type Placement struct {
	Medium            string
	Status            string
	VerificationClass string
	Hash              string
}

func placementOn(rec Record, medium string) (Placement, bool) {
	for _, p := range rec.Placements {
		if p.Medium == medium {
			return p, true
		}
	}
	return Placement{}, false
}

func (e *Engine) guardSourceDelete(ctx context.Context, mv string, rec Record, want string) (string, error) {
	return "", nil
}

func (e *Engine) fresh(mv string) []Placement { return nil }

func refreshDestination(ps []Placement, class string) {
	for i := range ps {
		ps[i].VerificationClass = class
	}
}
`

var provenanceControls = []provenanceControl{
	{
		name: "the compliant shape: one journal read, nothing else reaches the record",
		source: provenanceHeader + `
func (e *Engine) deleteSource(ctx context.Context, mv string) error {
	rec, err := e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	src, ok := placementOn(rec, "local")
	if !ok {
		return err
	}
	_ = src
	_, err = e.guardSourceDelete(ctx, mv, rec, "content")
	return err
}
`,
	},
	{
		name: "the loud mistake: the record is recomposed before the guard",
		source: provenanceHeader + `
func (e *Engine) deleteSource(ctx context.Context, mv string) error {
	rec, err := e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	rec = Record{Placements: e.fresh(mv)}
	_, err = e.guardSourceDelete(ctx, mv, rec, "content")
	return err
}
`,
		wantViolation: true,
	},
	{
		name: "the quiet mistake: one row is edited in place through the record",
		source: provenanceHeader + `
func (e *Engine) deleteSource(ctx context.Context, mv string) error {
	rec, err := e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	rec.Placements[0].Status = "ACTIVE"
	_, err = e.guardSourceDelete(ctx, mv, rec, "content")
	return err
}
`,
		wantViolation: true,
	},
	{
		name: "the mutation this test was reopened for: the slice is aliased, then written through",
		source: provenanceHeader + `
func (e *Engine) deleteSource(ctx context.Context, mv string) error {
	rec, err := e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	ps := rec.Placements
	ps[0].Status = "ACTIVE"
	ps[0].VerificationClass = "content"
	_, err = e.guardSourceDelete(ctx, mv, rec, "content")
	return err
}
`,
		wantViolation: true,
	},
	{
		name: "a struct copy of the record shares the same backing array",
		source: provenanceHeader + `
func (e *Engine) deleteSource(ctx context.Context, mv string) error {
	rec, err := e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	fresh := rec
	fresh.Placements[0].Hash = ""
	_, err = e.guardSourceDelete(ctx, mv, rec, "content")
	return err
}
`,
		wantViolation: true,
	},
	{
		name: "a pointer to the record",
		source: provenanceHeader + `
func (e *Engine) deleteSource(ctx context.Context, mv string) error {
	rec, err := e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	p := &rec
	p.Placements[0].Status = "ACTIVE"
	_, err = e.guardSourceDelete(ctx, mv, rec, "content")
	return err
}
`,
		wantViolation: true,
	},
	{
		name: "copy() into the record's own slice, which is not an assignment at all",
		source: provenanceHeader + `
func (e *Engine) deleteSource(ctx context.Context, mv string) error {
	rec, err := e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	copy(rec.Placements, e.fresh(mv))
	_, err = e.guardSourceDelete(ctx, mv, rec, "content")
	return err
}
`,
		wantViolation: true,
	},
	{
		name: "the slice is handed to a function that writes through it",
		source: provenanceHeader + `
func (e *Engine) deleteSource(ctx context.Context, mv string) error {
	rec, err := e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	refreshDestination(rec.Placements, "content")
	_, err = e.guardSourceDelete(ctx, mv, rec, "content")
	return err
}
`,
		wantViolation: true,
	},
	{
		name: "two journal reads, so the guard does not read the one deleteSource proved things about",
		source: provenanceHeader + `
func (e *Engine) deleteSource(ctx context.Context, mv string) error {
	rec, err := e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	rec, err = e.Journal.Get(ctx, mv)
	if err != nil {
		return err
	}
	_, err = e.guardSourceDelete(ctx, mv, rec, "content")
	return err
}
`,
		wantViolation: true,
	},
}

// recordProvenanceViolations reports every way the record handed to
// guardSourceDelete could have been touched between the journal read and
// the guard.
func recordProvenanceViolations(t *testing.T, files map[string]*ast.File) []string {
	t.Helper()
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

	taint := taintedByRecord(fn, guard.Pos(), recArg.Name)

	var out []string
	var fromJournalGet, touched int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil || n.Pos() >= guard.Pos() {
			return true
		}
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			if isJournalGetInto(stmt, recArg.Name) {
				fromJournalGet++
				touched++
				return true
			}
			// Taking an alias is not a write. It is what put the target in
			// the taint set in the first place, and every write THROUGH
			// that alias is caught on its own line.
			if len(stmt.Lhs) == 1 && len(stmt.Rhs) == 1 {
				if id, ok := stmt.Lhs[0].(*ast.Ident); ok && taint[id.Name] && taint[rootIdent(stmt.Rhs[0])] {
					return true
				}
			}
			for _, lhs := range stmt.Lhs {
				root := rootIdent(lhs)
				if !taint[root] {
					continue
				}
				touched++
				out = append(out, describeWrite(recArg.Name, root, lhs, stmt.Rhs))
			}
		case *ast.IncDecStmt:
			if root := rootIdent(stmt.X); taint[root] {
				touched++
				out = append(out, fmt.Sprintf("%s reaches the record %s and is incremented before e.guardSourceDelete", root, recArg.Name))
			}
		case *ast.CallExpr:
			if v := callViolations(files, stmt, taint, recArg.Name); v != "" {
				touched++
				out = append(out, v)
			}
		}
		return true
	})

	if fromJournalGet != 1 {
		out = append(out, fmt.Sprintf("%s is assigned from e.Journal.Get %d times before the guard, want exactly 1; the guard has to read one record, fetched once, from the journal", recArg.Name, fromJournalGet))
	}
	if touched == 0 {
		out = append(out, "nothing at all was found reaching the guard's record argument, so this test is not measuring anything")
	}
	return out
}

// taintedByRecord is every identifier that can reach the record's memory:
// the record itself, and anything that takes an alias of it, transitively.
//
// A call's RESULT is deliberately not an alias. placementOn(rec, medium)
// returns a copy of a row and nothing that writes to it reaches rec, and
// treating every return value as tainted would make this rule about
// nothing in particular. What a call CAN do is write through the argument
// it was handed, and that is asked separately, of the callee's own body.
func taintedByRecord(fn *ast.FuncDecl, before token.Pos, record string) map[string]bool {
	taint := map[string]bool{record: true}
	for changed := true; changed; {
		changed = false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil || n.Pos() >= before {
				return true
			}
			var lhs []ast.Expr
			var rhs []ast.Expr
			switch stmt := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = stmt.Lhs, stmt.Rhs
			case *ast.ValueSpec:
				for _, name := range stmt.Names {
					lhs = append(lhs, name)
				}
				rhs = stmt.Values
			default:
				return true
			}
			if len(lhs) != len(rhs) {
				return true
			}
			for i := range rhs {
				if !taint[rootIdent(rhs[i])] {
					continue
				}
				if root := rootIdent(lhs[i]); root != "" && !taint[root] {
					taint[root] = true
					changed = true
				}
			}
			return true
		})
	}
	return taint
}

func isJournalGetInto(stmt *ast.AssignStmt, record string) bool {
	if len(stmt.Rhs) != 1 || len(stmt.Lhs) == 0 {
		return false
	}
	if rootIdent(stmt.Lhs[0]) != record {
		return false
	}
	call, ok := stmt.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Get" {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != "Journal" {
		return false
	}
	id, ok := inner.X.(*ast.Ident)
	return ok && id.Name == "e"
}

func describeWrite(record, root string, lhs ast.Expr, rhs []ast.Expr) string {
	how := "is reassigned"
	if _, bare := lhs.(*ast.Ident); !bare {
		how = "is written through"
	}
	from := "something that is not a journal read"
	if len(rhs) == 1 {
		if call, ok := rhs[0].(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				from = "e." + sel.Sel.Name
			}
		}
	}
	via := ""
	if root != record {
		via = fmt.Sprintf(" (%s aliases %s, so this reaches the same rows)", root, record)
	}
	return fmt.Sprintf("%s %s from %s%s before it reaches e.guardSourceDelete; the guard's whole job is to require what the journal DURABLY recorded, and a record edited from the check deleteSource just ran satisfies every destination clause by construction",
		root, how, from, via)
}

// callViolations reports a call that is handed something reaching the
// record and cannot be shown not to write through it.
func callViolations(files map[string]*ast.File, call *ast.CallExpr, taint map[string]bool, record string) string {
	for i, arg := range call.Args {
		root := rootIdent(arg)
		if !taint[root] {
			continue
		}
		name := calleeName(call)
		if name == "" {
			return fmt.Sprintf("something reaching %s is passed to a call this detector cannot name, so nothing here can show it is not written through before e.guardSourceDelete", record)
		}
		if writesThroughParam(files, name, i, map[string]bool{}) {
			return fmt.Sprintf("%s is passed to %s, which writes through that parameter, before e.guardSourceDelete; a record edited by a callee is edited just as thoroughly as one edited here", root, name)
		}
	}
	return ""
}

func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// writesThroughParam reports whether every declaration of name in this
// package leaves its argIndex-th parameter alone.
//
// A name this package does not declare answers true. The builtin copy is
// the plainest case: nothing here can read its body, and it exists to
// overwrite its first argument.
func writesThroughParam(files map[string]*ast.File, name string, argIndex int, visited map[string]bool) bool {
	if visited[name] {
		return false
	}
	visited[name] = true

	var found bool
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != name || fn.Body == nil {
				continue
			}
			found = true
			param := paramName(fn, argIndex)
			if param == "" || param == "_" {
				continue
			}
			inner := taintedByParam(fn, param)
			writes := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch stmt := n.(type) {
				case *ast.AssignStmt:
					if len(stmt.Lhs) == 1 && len(stmt.Rhs) == 1 {
						if id, ok := stmt.Lhs[0].(*ast.Ident); ok && inner[id.Name] && inner[rootIdent(stmt.Rhs[0])] {
							return true
						}
					}
					for _, lhs := range stmt.Lhs {
						if inner[rootIdent(lhs)] {
							writes = true
						}
					}
				case *ast.IncDecStmt:
					if inner[rootIdent(stmt.X)] {
						writes = true
					}
				case *ast.CallExpr:
					for i, arg := range stmt.Args {
						if !inner[rootIdent(arg)] {
							continue
						}
						next := calleeName(stmt)
						if next == "" || writesThroughParam(files, next, i, visited) {
							writes = true
						}
					}
				}
				return true
			})
			if writes {
				return true
			}
		}
	}
	return !found
}

func paramName(fn *ast.FuncDecl, argIndex int) string {
	i := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			if i == argIndex {
				return ""
			}
			i++
			continue
		}
		for _, n := range field.Names {
			if i == argIndex {
				return n.Name
			}
			i++
		}
	}
	return ""
}

// taintedByParam is taintedByRecord for a callee's own parameter, over its
// whole body.
func taintedByParam(fn *ast.FuncDecl, param string) map[string]bool {
	taint := map[string]bool{param: true}
	for changed := true; changed; {
		changed = false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != len(assign.Rhs) {
				return true
			}
			for i := range assign.Rhs {
				if !taint[rootIdent(assign.Rhs[i])] {
					continue
				}
				if root := rootIdent(assign.Lhs[i]); root != "" && !taint[root] {
					taint[root] = true
					changed = true
				}
			}
			return true
		})
	}
	return taint
}
