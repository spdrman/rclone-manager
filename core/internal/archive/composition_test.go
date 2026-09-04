package archive_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
func TestNothingDeletesACopyWithoutAskingWhetherAnotherOneIsReadable(t *testing.T) {
	offenders := deletingWithoutAsking(t, internalPackages(t))
	if len(offenders) > 0 {
		t.Errorf("%v remove an object from a storage medium without their package calling archive.CheckSourceDelete; a delete decided from the journal alone deletes the last copy anybody can read and leaves one that is provably intact and hours out of reach", offenders)
	}

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
	if got := deletingWithoutAsking(t, []string{planted}); len(got) == 0 {
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
	if got := deletingWithoutAsking(t, []string{importing}); len(got) == 0 {
		t.Error("the detector waved through a mover that imports internal/archive for something else and never calls CheckSourceDelete; an import is satisfied by any use, so it proves nothing about the delete")
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
	if got := deletingWithoutAsking(t, []string{asking}); len(got) != 0 {
		t.Errorf("the detector flagged a mover that does ask (%v), so a green run above could be a detector that flags nothing in particular", got)
	}
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

// deletingWithoutAsking reports every file that calls DeleteObject without
// its own package calling archive.CheckSourceDelete.
//
// The unit is the PACKAGE rather than the file, because the guard call and
// the delete call belong in one package but not necessarily in one file,
// and a rule that demanded both in one file would be a rule about layout.
//
// The call is recognised through whatever name the file imports
// internal/archive under, so renaming the import cannot hide it, and a
// CheckSourceDelete on anything that is not that import does not count.
func deletingWithoutAsking(t *testing.T, dirs []string) []string {
	t.Helper()
	deletes := map[string][]string{}
	consults := map[string]bool{}

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
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "DeleteObject":
				deletes[dir] = append(deletes[dir], path)
			case "CheckSourceDelete":
				if pkg, ok := sel.X.(*ast.Ident); ok && archiveName != "" && pkg.Name == archiveName {
					consults[dir] = true
				}
			}
			return true
		})
	})

	var out []string
	for dir, files := range deletes {
		if consults[dir] {
			continue
		}
		out = append(out, files...)
	}
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
