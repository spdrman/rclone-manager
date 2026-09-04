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

// These two guards are this lane's answer to the two integration
// questions #241 owns, written as tests rather than as prose in a pull
// request, because prose in a pull request does not fail.
//
// Both are about code that is not in this tree yet. #238's move engine and
// #240's placement surfaces are in flight in other lanes, and each of them
// lands in exactly the place one of these guards watches. So each guard
// runs a detector over the whole of core/internal, and each one proves its
// detector works against a synthetic file that breaks the rule: without
// that positive control a guard whose subject has not landed yet is
// indistinguishable from a guard that matches nothing.

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
// documented survivor.
//
// On the base this branch actually sits on there is only one copy:
// internal/placement/access.go has not landed on main, so there was
// nothing to collapse when this was written. That is precisely why this
// exists as a test. The second copy arrives with somebody else's rebase,
// and a rule that only lives in a merged pull request description is a
// rule the next lane never reads.
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
// bytes that are hours away from anybody. #238's own guard asks the
// journal-shaped question, and it cannot infer the archive one, because
// nothing rewrites verification_class when a lifecycle rule transitions an
// object or a restore window expires. The composition can only go one way,
// too: archive has no journal read of its own, so it cannot call #238's
// guard, while #238's can call this one with the copies it has already
// loaded.
//
// So the rule is that #238's guard calls archive.CheckSourceDelete, and
// this is the guard that makes a mover which forgets fail the build rather
// than pass review. It is written here because the caller does not exist
// yet and this lane must not edit another lane's files.
func TestNothingDeletesACopyWithoutAskingWhetherAnotherOneIsReadable(t *testing.T) {
	offenders := deletingWithoutArchive(t, internalPackages(t))
	if len(offenders) > 0 {
		t.Errorf("%v remove an object from a storage medium without their package consulting internal/archive; a delete decided from the journal alone deletes the last copy anybody can read and leaves one that is provably intact and hours out of reach", offenders)
	}

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
	if got := deletingWithoutArchive(t, []string{planted}); len(got) == 0 {
		t.Error("the detector did not notice a mover that deletes an object without consulting internal/archive, so the assertion above proves nothing")
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

// deletingWithoutArchive reports every file that calls DeleteObject
// without its own package importing internal/archive.
//
// The unit is the PACKAGE rather than the file, because the guard call and
// the delete call belong in one package but not necessarily in one file,
// and a rule that demanded both in one file would be a rule about layout.
func deletingWithoutArchive(t *testing.T, dirs []string) []string {
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
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err == nil && strings.HasSuffix(p, "/internal/archive") {
				consults[dir] = true
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "DeleteObject" {
				return true
			}
			deletes[dir] = append(deletes[dir], path)
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
