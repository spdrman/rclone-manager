package archive_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// These two guards are #241's answer to the two integration questions it
// owns, written as tests rather than as prose in a pull request, because
// prose in a pull request does not fail.
//
// They were written before their subjects existed. #238's move engine and
// #240's placement surfaces were in flight in other lanes, and each landed
// in exactly the place one of these guards watches; both guards went red
// at the composition, and both were right. What resolved them is recorded
// on #241: the package edge now runs placement -> archive (archive's
// verification gate moved to placement, where its answers are used), the
// second copy of the vocabulary is gone, and the engine's own guard ends
// by calling archive.CheckSourceDelete. Each guard still runs a detector
// over the whole of core/internal, and each still proves its detector
// against synthetic files that break the rule, because a detector nobody
// has watched fail is indistinguishable from one that matches nothing.

// accessVocabulary is FR-34's closed set, spelled as archive.State spells
// it. It is written out here rather than read from archive.States on
// purpose: this test is about the STRINGS appearing a second time in the
// tree, so reading them from the thing under test would make a rename
// silently move the goalposts.
var accessVocabulary = map[string]bool{
	"immediate":        true,
	"requires_restore": true,
	"restoring":        true,
	"unreachable":      true,
}

// TestTheAccessVocabularyIsDefinedInExactlyOnePlace.
//
// #241's review found the vocabulary written down twice, in
// internal/placement and in internal/archive, with each copy documenting
// the duplication in prose and neither collapsing it. archive is the
// survivor, because deriving the state needs the class table, whether a
// restore is running and when a finished one expires, and only archive
// holds those. internal/placement/access.go was deleted at the
// composition and the service read surface derives from archive.
//
// It stays as a test because the second copy arrived once already, with
// somebody else's rebase, and a rule that only lives in a merged pull
// request description is a rule the next lane never reads.
func TestTheAccessVocabularyIsDefinedInExactlyOnePlace(t *testing.T) {
	offenders := declaringAccessStates(t, internalPackages(t))
	if len(offenders) > 0 {
		t.Errorf("the access vocabulary is declared outside internal/archive, in %v; FR-34's four words mean one thing, and two tables that agree today are two tables that disagree after one edit", offenders)
	}

	// The positive control. Without it, a rename of the four words, or a
	// detector that walked no files at all, would leave this green.
	planted := t.TempDir()
	write(t, filepath.Join(planted, "access.go"), `package placement

type State string

const RequiresRestore State = "requires_restore"
`)
	if got := declaringAccessStates(t, []string{planted}); len(got) == 0 {
		t.Error("the detector did not notice a second copy of the vocabulary that was put in front of it, so the assertion above proves nothing")
	}
}

// TestNothingDeletesACopyWithoutAskingWhetherAnotherOneIsReadable.
//
// This is the composition answer for the two source-delete guards, and
// the direction is not a preference.
//
// archive.CheckSourceDelete asks whether any SURVIVING copy can actually
// be read right now, which is a fact about the present that no journal row
// carries: a placement can say ACTIVE and content-verified and describe
// bytes that are hours away from anybody. The move engine's own guard asks
// the journal-shaped question, and it cannot infer the archive one,
// because nothing rewrites verification_class when a lifecycle rule
// transitions an object or a restore window expires. The composition can
// only go one way, too: archive has no journal read of its own, so it
// cannot call the engine's guard, while the engine's can call this one
// with the copies it has already loaded.
//
// So the rule is that the engine's guard calls archive.CheckSourceDelete
// (placement.Engine.guardSourceDelete, its eighth clause), and this is the
// guard that makes a mover which forgets fail the build rather than pass
// review.
//
// # It requires the call, not the import
//
// The first version of this detector asked whether the deleting package
// imported internal/archive, which was the strongest thing it could ask
// about a caller that did not exist yet. It is not strong enough now.
// placement imports archive for its verification gate as well, so an
// import is satisfied by code that never goes near a delete, and deleting
// the CheckSourceDelete call would have left an import-based guard green.
// The second planted control below is exactly that mover, and it is the
// one the old detector waved through. placement's own destructive_test
// pins where in the package the call sits.
//
// # It requires THIS deleter to ask, not somebody in its package
//
// The second version asked whether the deleting PACKAGE calls
// CheckSourceDelete, and that is a hole a review found by walking through
// it: drop a brand new, wholly unguarded DeleteObject caller into
// internal/placement and this test stays green, because guardSourceDelete
// is in the same package and makes the call on its own account. The guard
// proved "somebody here asks", which is not a fact about the delete that
// is actually being reviewed.
//
// A per-FILE rule is the easy tightening and it is still the wrong one. It
// buys nothing on the case that matters (the rogue mover goes in the file
// that already has the call, or the compliant caller gets split out, and
// either way a rule about which file a function sits in is a rule about
// layout), and it starts failing honest code the moment somebody moves a
// helper.
//
// The unit is the FUNCTION, closed over the package's own call graph,
// because the honest question is whether a consult DOMINATES this delete:
//
//   - a function that calls DeleteObject may make the consult itself,
//     before the delete, or
//   - every function in the package that calls it must make the consult
//     before that call.
//
// That is the real shape in internal/placement and it is why one level of
// caller is enough. Engine.remove holds the raw DeleteObject and asks
// nothing; its only caller, Engine.deleteSource, calls
// Engine.guardSourceDelete (which consults) first and Engine.remove
// second. A rogue mover has neither: it does not consult, and nothing in
// the package calls it in front of a consult, so it has no dominator at
// all and this test names it.
//
// # The two deletes that are deliberately outside the rule
//
// Two functions destroy an object on purpose without asking whether
// another copy is readable, and both are right to.
// Engine.discardDestination removes the object THIS move created at a key
// THIS move computed, so there is no other copy in the question at all;
// Reclaimer.DeleteFromMedium is FR-20's prune, whose whole job is to
// remove the last copy of an artifact no tier wants any more, and where
// CheckSourceDelete could only ever refuse.
//
// They are written down here, one line each with the reason, rather than
// left to a package-wide hole that silently covers them and everything
// else. exemptionsAreStillReal below fails if one of them stops existing
// or stops deleting, so the list cannot rot into a permanent pass, and
// adding a third means editing this file, which is exactly the review the
// package-granular version never forced.
func TestNothingDeletesACopyWithoutAskingWhetherAnotherOneIsReadable(t *testing.T) {
	offenders := deletingWithoutAsking(t, internalPackages(t), deleteExemptions)
	if len(offenders) > 0 {
		t.Errorf("%v remove an object from a storage medium with no call to archive.CheckSourceDelete dominating the delete; a delete decided from the journal alone deletes the last copy anybody can read and leaves one that is provably intact and hours out of reach", offenders)
	}
	exemptionsAreStillReal(t, internalPackages(t), deleteExemptions)

	// The positive control: a mover that never heard of archive.
	planted := t.TempDir()
	write(t, filepath.Join(planted, "mover.go"), `package placement

import "context"

type store interface {
	DeleteObject(ctx context.Context, medium, key string) error
}

func deleteSource(ctx context.Context, s store, medium, key string) error {
	return s.DeleteObject(ctx, medium, key)
}
`)
	if got := deletingWithoutAsking(t, []string{planted}, nil); len(got) == 0 {
		t.Error("the detector did not notice a mover that deletes an object without asking internal/archive, so the assertion above proves nothing")
	}

	// The second positive control: a mover that imports archive, uses it
	// for something else, and deletes without asking. An import-based
	// detector passes this one.
	importing := t.TempDir()
	write(t, filepath.Join(importing, "mover.go"), `package placement

import (
	"context"

	"github.com/spdrman/rclone-manager/core/internal/archive"
)

type store interface {
	DeleteObject(ctx context.Context, medium, key string) error
}

var ceiling = archive.Immediate

func deleteSource(ctx context.Context, s store, medium, key string) error {
	return s.DeleteObject(ctx, medium, key)
}
`)
	if got := deletingWithoutAsking(t, []string{importing}, nil); len(got) == 0 {
		t.Error("the detector waved through a mover that imports internal/archive for something else and never calls CheckSourceDelete; an import is satisfied by any use, so it proves nothing about the delete")
	}

	// The third positive control, and the one this rewrite exists for: a
	// package that contains BOTH the real compliant chain and one rogue
	// mover. Every package-granular detector passes this, because
	// guardSourceDelete is right there making the call. Only the rogue may
	// be named, and it must be named.
	mixed := t.TempDir()
	write(t, filepath.Join(mixed, "engine.go"), compliantChain)
	write(t, filepath.Join(mixed, "rogue.go"), `package placement

import "context"

func (e *Engine) reclaimSourceEarly(ctx context.Context, medium, key string) error {
	return e.Store.DeleteObject(ctx, medium, key)
}
`)
	got := deletingWithoutAsking(t, []string{mixed}, nil)
	if len(got) != 1 || !strings.HasSuffix(got[0], "Engine.reclaimSourceEarly") {
		t.Errorf("a rogue mover dropped into a package that also holds the compliant chain came back as %v; it has to be named, and it has to be the only one named, or this guard is still proving that SOMEBODY in the package asks", got)
	}

	// The fourth positive control: the consult is there, in the caller,
	// and it runs AFTER the delete. A detector that only asked whether the
	// two calls coexist passes this, and an answer computed after the
	// bytes are gone protected nothing.
	late := t.TempDir()
	write(t, filepath.Join(late, "engine.go"), `package placement

import (
	"context"

	"github.com/spdrman/rclone-manager/core/internal/archive"
)

type Engine struct{ Store interface {
	DeleteObject(ctx context.Context, medium, key string) error
} }

func (e *Engine) remove(ctx context.Context, medium, key string) error {
	return e.Store.DeleteObject(ctx, medium, key)
}

func (e *Engine) deleteSource(ctx context.Context, src archive.Copy, all []archive.Copy, medium, key string) error {
	if err := e.remove(ctx, medium, key); err != nil {
		return err
	}
	return archive.CheckSourceDelete(src, all)
}
`)
	if got := deletingWithoutAsking(t, []string{late}, nil); len(got) == 0 {
		t.Error("the detector accepted a caller that consults archive AFTER it has already deleted; the consult has to dominate the delete, not merely appear beside it")
	}

	// The negative control: the shape that satisfies the rule, so a
	// detector that flagged every mover could not pass as a strict one.
	asking := t.TempDir()
	write(t, filepath.Join(asking, "mover.go"), `package placement

import (
	"context"

	arc "github.com/spdrman/rclone-manager/core/internal/archive"
)

type store interface {
	DeleteObject(ctx context.Context, medium, key string) error
}

func deleteSource(ctx context.Context, s store, src arc.Copy, all []arc.Copy, medium, key string) error {
	if err := arc.CheckSourceDelete(src, all); err != nil {
		return err
	}
	return s.DeleteObject(ctx, medium, key)
}
`)
	if got := deletingWithoutAsking(t, []string{asking}, nil); len(got) != 0 {
		t.Errorf("the detector flagged a mover that does ask (%v), so a green run above could be a detector that flags nothing in particular", got)
	}

	// The second negative control: the real shape, where the delete and
	// the consult are in different functions and the caller orders them.
	// A per-function rule with no call graph fails this one, which is why
	// there is a call graph.
	chained := t.TempDir()
	write(t, filepath.Join(chained, "engine.go"), compliantChain)
	if got := deletingWithoutAsking(t, []string{chained}, nil); len(got) != 0 {
		t.Errorf("the detector flagged the compliant chain (%v), where the deleting function is called only from one that consults first; that is internal/placement's own shape and a rule that refuses it is a rule nobody can satisfy", got)
	}
}

// compliantChain is internal/placement's real shape, reduced: a raw
// deleter that asks nothing, reached from exactly one function that
// consults archive through its own guard before it calls the deleter.
const compliantChain = `package placement

import (
	"context"

	"github.com/spdrman/rclone-manager/core/internal/archive"
)

type Engine struct{ Store interface {
	DeleteObject(ctx context.Context, medium, key string) error
} }

func (e *Engine) remove(ctx context.Context, medium, key string) error {
	return e.Store.DeleteObject(ctx, medium, key)
}

func (e *Engine) guardSourceDelete(src archive.Copy, all []archive.Copy) error {
	return archive.CheckSourceDelete(src, all)
}

func (e *Engine) deleteSource(ctx context.Context, src archive.Copy, all []archive.Copy, medium, key string) error {
	if err := e.guardSourceDelete(src, all); err != nil {
		return err
	}
	return e.remove(ctx, medium, key)
}
`

// deleteExemption is one function that removes an object from a medium and
// is deliberately not required to ask archive first.
//
// Pkg is a path suffix so the entry says which package it is about, and
// Func is the detector's own key: "Type.Method" for a method, the bare
// name for a package-level function.
type deleteExemption struct {
	Pkg    string
	Func   string
	Reason string
}

var deleteExemptions = []deleteExemption{
	{
		Pkg:  "internal/placement",
		Func: "Engine.discardDestination",
		Reason: "it removes the object THIS move created, at a key THIS move computed, on the abandon and recopy paths. " +
			"There is no other copy in the question: the artifact's surviving copy is the source this path has just put back to ACTIVE. " +
			"placement's TestDiscardDestinationNeverTouchesTheSource is what holds it to addressing the destination only.",
	},
	{
		Pkg:  "internal/placement",
		Func: "Reclaimer.DeleteFromMedium",
		Reason: "it is FR-20's prune with the copy on a medium, and its whole job is to remove the LAST copy of an artifact no retention tier " +
			"selects any more. CheckSourceDelete could only ever refuse it. What stands in for the readable-survivor question here is FR-16's " +
			"identity re-check against the medium, which reclaim.go runs immediately before the delete.",
	},
	{
		Pkg:  "internal/mediumcheck",
		Func: "run.deleted",
		Reason: "the medium preflight (#443), and it is outside the rule because it is outside the SUBJECT of the rule: there is no artifact " +
			"anywhere in this delete. The only key it can pass to DeleteObject is one it generated itself from crypto/rand, under a reserved " +
			".rclone-manager-preflight/ segment that transport.MediumKey cannot spell for any configured artifact, and the object at it is a " +
			"fixed 120-byte probe this same call wrote seconds earlier. CheckSourceDelete asks whether another copy of the BACKUP is readable, " +
			"and there is no backup: refusing to delete the probe would leave litter in an operator's bucket that nothing in this product ever " +
			"cleans up. mediumcheck's TestProbeKey_LivesUnderASegmentNoArtifactCanReach pins the containment, and its happy path asserts exactly " +
			"one upload and exactly one delete.",
	},
}

// exemptionsAreStillReal fails if an exemption names something that has
// stopped existing or stopped deleting.
//
// Without it the list is a permanent pass waiting to happen: rename the
// function, or take its delete away and give it to a new one, and the
// entry silently starts covering nothing while the new deleter walks in
// under a rule nobody re-read.
func exemptionsAreStillReal(t *testing.T, dirs []string, exemptions []deleteExemption) {
	t.Helper()
	pkgs := parsePackages(t, dirs)
	for _, ex := range exemptions {
		var found bool
		for dir, fns := range pkgs {
			if !strings.HasSuffix(filepath.ToSlash(dir), ex.Pkg) {
				continue
			}
			if fn, ok := fns[ex.Func]; ok && len(fn.deletes) > 0 {
				found = true
			}
		}
		if !found {
			t.Errorf("the exemption for %s.%s names a function that either does not exist any more or no longer removes an object from a medium; an exemption that covers nothing is a hole waiting for the next deleter, so it has to go rather than sit here",
				ex.Pkg, ex.Func)
		}
	}
}

// pkgFunc is one function or method, with everything the rule asks about
// it.
type pkgFunc struct {
	key      string
	file     string
	decls    int
	deletes  []token.Pos
	consults []token.Pos
	calls    []pkgCall

	// consultingDecls is how many of this key's declarations call
	// archive.CheckSourceDelete. It only ever differs from decls where
	// build tags put two bodies behind one name.
	consultingDecls int
}

// pkgCall is one call this function makes to another function in the same
// package, and where it makes it.
type pkgCall struct {
	callee string
	pos    token.Pos
}

// parsePackages reads every directory into a map of function key to
// pkgFunc.
//
// Method calls are resolved only through the receiver's own identifier
// (e.foo() inside a method whose receiver is named e), and plain calls
// through the package's own function names. Anything else is somebody
// else's function and is left unresolved on purpose: guessing at the type
// of an arbitrary expression without a type checker is how a detector
// starts quietly matching the wrong thing.
func parsePackages(t *testing.T, dirs []string) map[string]map[string]*pkgFunc {
	t.Helper()
	out := map[string]map[string]*pkgFunc{}
	forEachFile(t, dirs, func(path string, file *ast.File) {
		dir := filepath.Dir(path)
		slashed := filepath.ToSlash(path)
		// The adapter is what DeleteObject is implemented BY, and the
		// boundary is what declares it. Neither is a decision about
		// whether a particular copy may go, which is what this rule is
		// about.
		if strings.Contains(slashed, "/internal/transport/") {
			return
		}
		if out[dir] == nil {
			out[dir] = map[string]*pkgFunc{}
		}
		// Every declared name first, so a call can be resolved against a
		// function declared in another file of the same package.
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := funcKey(fn)
			if existing, clash := out[dir][key]; clash {
				// Two declarations of one name in one directory means
				// build tags (internal/capacity's StatPath is the real
				// one). The detector cannot tell which variant a call
				// reaches, so it takes the union of what they delete and
				// call, and counts a consult only when EVERY variant makes
				// one. Both directions of that are the conservative one.
				existing.decls++
				continue
			}
			out[dir][key] = &pkgFunc{key: key, file: path, decls: 1}
		}
	})
	// Second pass, now that every name in every directory is known.
	forEachFile(t, dirs, func(path string, file *ast.File) {
		dir := filepath.Dir(path)
		if strings.Contains(filepath.ToSlash(path), "/internal/transport/") {
			return
		}
		archiveName := ""
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasSuffix(p, "/internal/archive") {
				continue
			}
			archiveName = "archive"
			if imp.Name != nil {
				archiveName = imp.Name.Name
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			info := out[dir][funcKey(fn)]
			recvIdent, recvType := receiverOf(fn)
			consultsHere := len(info.consults)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					switch fun.Sel.Name {
					case "DeleteObject":
						info.deletes = append(info.deletes, call.Pos())
						return true
					case "CheckSourceDelete":
						if pkg, ok := fun.X.(*ast.Ident); ok && archiveName != "" && pkg.Name == archiveName {
							info.consults = append(info.consults, call.Pos())
							return true
						}
					}
					// e.foo(...) inside a method whose receiver is e.
					if id, ok := fun.X.(*ast.Ident); ok && recvIdent != "" && id.Name == recvIdent {
						info.calls = append(info.calls, pkgCall{callee: recvType + "." + fun.Sel.Name, pos: call.Pos()})
					}
				case *ast.Ident:
					if _, ok := out[dir][fun.Name]; ok {
						info.calls = append(info.calls, pkgCall{callee: fun.Name, pos: call.Pos()})
					}
				}
				return true
			})
			if len(info.consults) > consultsHere {
				info.consultingDecls++
			}
		}
	})
	// A name behind build tags only counts as consulting when every one of
	// its bodies does.
	for _, fns := range out {
		for _, fn := range fns {
			if fn.decls > 1 && fn.consultingDecls < fn.decls {
				fn.consults = nil
			}
		}
	}
	return out
}

func funcKey(fn *ast.FuncDecl) string {
	if _, recvType := receiverOf(fn); recvType != "" {
		return recvType + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// receiverOf returns a method's receiver identifier and type name, both
// empty for a plain function.
func receiverOf(fn *ast.FuncDecl) (string, string) {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "", ""
	}
	f := fn.Recv.List[0]
	name := ""
	if len(f.Names) > 0 {
		name = f.Names[0].Name
	}
	typ := f.Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return name, id.Name
	}
	if idx, ok := typ.(*ast.IndexExpr); ok { // a generic receiver
		if id, ok := idx.X.(*ast.Ident); ok {
			return name, id.Name
		}
	}
	return name, ""
}

// deletingWithoutAsking reports every function that removes an object from
// a medium with no archive.CheckSourceDelete dominating the delete.
//
// A function passes when it consults before its own delete, or when every
// function in its package that calls it consults, itself or through
// something it calls, before that call. A function nothing in the package
// calls has no dominator at all and is reported: it is either an exported
// entry point, in which case the guard belongs in it, or it is dead.
func deletingWithoutAsking(t *testing.T, dirs []string, exemptions []deleteExemption) []string {
	t.Helper()
	pkgs := parsePackages(t, dirs)

	var out []string
	for dir, fns := range pkgs {
		exempt := map[string]bool{}
		for _, ex := range exemptions {
			if strings.HasSuffix(filepath.ToSlash(dir), ex.Pkg) {
				exempt[ex.Func] = true
			}
		}
		consulting := reachesConsult(fns)
		callers := map[string][]*pkgFunc{}
		for _, fn := range fns {
			for _, c := range fn.calls {
				callers[c.callee] = append(callers[c.callee], fn)
			}
		}

		for key, fn := range fns {
			if len(fn.deletes) == 0 || exempt[key] {
				continue
			}
			if consultsBefore(fn.consults, fn.deletes[0]) {
				continue
			}
			if len(callers[key]) == 0 {
				out = append(out, dir+":"+key)
				continue
			}
			for _, caller := range callers[key] {
				at := firstCallTo(caller, key)
				if !callerConsultsBefore(caller, consulting, at) {
					out = append(out, dir+":"+key+" (called from "+caller.key+", which does not ask first)")
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// reachesConsult is the set of functions that call archive.CheckSourceDelete,
// directly or through another function in the same package.
func reachesConsult(fns map[string]*pkgFunc) map[string]bool {
	does := map[string]bool{}
	for key, fn := range fns {
		if len(fn.consults) > 0 {
			does[key] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for key, fn := range fns {
			if does[key] {
				continue
			}
			for _, c := range fn.calls {
				if does[c.callee] {
					does[key] = true
					changed = true
					break
				}
			}
		}
	}
	return does
}

func consultsBefore(consults []token.Pos, delete token.Pos) bool {
	for _, c := range consults {
		if c < delete {
			return true
		}
	}
	return false
}

func firstCallTo(caller *pkgFunc, callee string) token.Pos {
	var at token.Pos
	for _, c := range caller.calls {
		if c.callee == callee && (!at.IsValid() || c.pos < at) {
			at = c.pos
		}
	}
	return at
}

// callerConsultsBefore reports whether caller asks archive, itself or
// through something it calls, before position at.
func callerConsultsBefore(caller *pkgFunc, consulting map[string]bool, at token.Pos) bool {
	if consultsBefore(caller.consults, at) {
		return true
	}
	for _, c := range caller.calls {
		if consulting[c.callee] && c.pos < at {
			return true
		}
	}
	return false
}

// declaringAccessStates reports every file outside internal/archive that
// declares a constant or variable whose value is one of FR-34's four
// words.
//
// It matches on the VALUE rather than on an identifier, because the second
// copy of a vocabulary is rarely spelled with the same identifiers. It is
// what the string says that has to be unique.
func declaringAccessStates(t *testing.T, dirs []string) []string {
	t.Helper()
	var out []string
	forEachFile(t, dirs, func(path string, file *ast.File) {
		if strings.Contains(filepath.ToSlash(path), "/internal/archive/") {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for _, value := range spec.Values {
				lit, ok := value.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil || !accessVocabulary[unquoted] {
					continue
				}
				out = append(out, path+" declares "+strconv.Quote(unquoted))
			}
			return true
		})
	})
	return out
}

// internalPackages is every directory under core/internal, which is the
// whole of the tree these two rules govern.
func internalPackages(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving core/: %v", err)
	}
	internal := filepath.Join(root, "internal")
	var dirs []string
	if err := filepath.WalkDir(internal, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walking %s: %v", internal, err)
	}
	if len(dirs) < 5 {
		t.Fatalf("walking core/internal found %d directories, so these guards would run over nothing", len(dirs))
	}
	return dirs
}

// forEachFile parses every non-test Go file directly in each directory.
//
// Test files are skipped on purpose: a double in a test may name anything
// it likes, and a rule that reached into them would be a rule about how
// tests are written rather than about what the product does.
func forEachFile(t *testing.T, dirs []string, fn func(path string, file *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			full, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			full.Imports = parsed.Imports
			fn(path, full)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
