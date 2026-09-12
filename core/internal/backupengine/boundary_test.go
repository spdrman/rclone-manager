package backupengine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The vendor name is assembled rather than written so these tests can search
// for it without containing a match of their own, the same trick
// internal/placement's guard needs for the word it keeps out of production
// code.
var vendorName = "ko" + "pia"

// adapterDir is the one directory allowed to name the vendor: the adapter
// subpackage, relative to this file.
const adapterDir = "kopia"

// TestEngineFileNamesNoVendor is the enforcement half of the package doc's
// claim that no embedded-engine type appears in any signature in engine.go.
//
// A type cannot appear in a signature without its package qualifier or its
// import path appearing in the file, and both contain the vendor's name. So
// "engine.go does not contain that string" is a complete, cheap check of a
// claim that would otherwise be maintained by hope: the failure mode this
// catches is somebody adding a convenient parameter of an upstream type, in
// a hurry, because it was right there.
func TestEngineFileNamesNoVendor(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("reading engine.go: %v", err)
	}

	text := strings.ToLower(string(src))

	if strings.Contains(text, vendorName) {
		t.Errorf("engine.go mentions %q; the embedded engine must stay inside the adapter subpackage", vendorName)
	}

	// The boundary is only meaningful if the file it protects is the one
	// carrying the interface, so fail loudly if engine.go ever stops being
	// that file rather than passing vacuously.
	for _, want := range []string{"type Engine interface", "type Repository interface"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("engine.go no longer declares %q; this guard is checking the wrong file", want)
		}
	}
}

// TestNoVendorImportOutsideTheAdapter widens the same check to the whole
// module, because "engine.go is clean" stops being the interesting property
// the moment a second file in this package exists -- and folding the
// streaming spike in is exactly what created them.
//
// The quarantine claim is about every package, not about one file: a helper
// in internal/lifecycle that imported the vendor directly would leave
// engine.go spotless and the boundary gone. Test files are scanned too, with
// one exception for the adapter's own directory, so the obvious cheat -- a
// test helper that reaches around the adapter -- is not available either.
func TestNoVendorImportOutsideTheAdapter(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	needle := "github.com/" + vendorName + "/" + vendorName
	adapter := filepath.Join(root, "internal", "backupengine", adapterDir)

	var checked int

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if path == adapter || d.Name() == "testdata" {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		checked++

		if strings.Contains(string(src), needle) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s imports %s; every import of the embedded engine belongs in internal/backupengine/%s",
				rel, needle, adapterDir)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if checked == 0 {
		t.Fatal("scanned no Go files; this guard is looking in the wrong place")
	}

	// The scan is only a proof of quarantine if the quarantined package is
	// actually where the vendor lives, so check that it does.
	adapterFile := filepath.Join(adapter, "adapter.go")

	src, err := os.ReadFile(adapterFile)
	if err != nil {
		t.Fatalf("reading %s: %v", adapterFile, err)
	}

	if !strings.Contains(string(src), needle) {
		t.Errorf("%s does not import %s; this guard is passing vacuously", adapterFile, needle)
	}

	t.Logf("scanned %d Go files outside internal/backupengine/%s", checked, adapterDir)
}

// TestOneEngineAndOneRepositoryDeclaration is the other half of the
// consolidation, and the reason it is a test rather than a review note.
//
// Two packages each declaring an Engine is not a merge conflict, it is two
// boundaries: callers pick one, the second accumulates its own semantics, and
// the quarantine stops meaning anything because there are two doors. The
// streaming spike arrived as exactly that -- a second, vendor-importing
// Engine in this package -- and this test is what makes its return a failing
// build instead of an argument.
func TestOneEngineAndOneRepositoryDeclaration(t *testing.T) {
	t.Parallel()

	declaration := regexp.MustCompile(`(?m)^type (Engine|Repository) (interface|struct)\b`)

	found := map[string][]string{}

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, m := range declaration.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = append(found[m[1]], path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the backupengine tree: %v", err)
	}

	for _, name := range []string{"Engine", "Repository"} {
		switch where := found[name]; len(where) {
		case 1:
			if filepath.Base(where[0]) != "engine.go" {
				t.Errorf("%s is declared in %s; the boundary's types belong in engine.go", name, where[0])
			}
		case 0:
			t.Errorf("no declaration of %s found anywhere under internal/backupengine", name)
		default:
			t.Errorf("%s is declared %d times, in %v; one boundary means one declaration", name, len(where), where)
		}
	}
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod, so the scan covers the module rather than one package.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test's working directory")
		}

		dir = parent
	}
}

// TestNoVendorImportAnywhereInTheRepository widens the import sweep past
// this module.
//
// TestNoVendorImportOutsideTheAdapter walks up to the nearest go.mod, which
// is core/: every other Go module in this repository (apps/generic,
// apps/synology, distribution, scripts/...) is outside its reach. That is
// the half of the quarantine claim nobody was checking, and it is not the
// unlikely half: a provider app is exactly the kind of place a "just read
// the snapshot list for the dashboard" import would land, because it is far
// from this package and its author has never read this file.
func TestNoVendorImportAnywhereInTheRepository(t *testing.T) {
	t.Parallel()

	repo := repoRoot(t)
	needle := "github.com/" + vendorName + "/" + vendorName
	adapter := filepath.Join(repo, "core", "internal", "backupengine", adapterDir)

	var checked, modules int

	err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if path == adapter || skippedTree(d.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if d.Name() == "go.mod" {
			modules++

			return nil
		}

		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		checked++

		if strings.Contains(string(src), needle) {
			rel, _ := filepath.Rel(repo, path)
			t.Errorf("%s imports %s; every import of the embedded engine belongs in core/internal/backupengine/%s",
				rel, needle, adapterDir)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", repo, err)
	}

	// Both counts are controls. Without the module count this test could
	// pass having walked one module, which is what the test above already
	// does; the repository has core/ plus the provider apps, the
	// distribution module and the tooling modules.
	if modules < 3 {
		t.Fatalf("found %d go.mod files under %s; this sweep is meant to cover every module in the repository, not one", modules, repo)
	}

	t.Logf("scanned %d Go files across %d modules outside core/internal/backupengine/%s", checked, modules, adapterDir)
}

// domainSurfaces are the surfaces EPIC K names explicitly: the versioned
// API contract, the UI's models, the catalog's persistence, the shared
// domain vocabulary, the configuration schema, the application services and
// the CLI. "No embedded-engine type leaks into api/v1, CLI/UI models or
// catalog persistence" is a claim about these directories, and it is
// checked here by name rather than only as a side effect of the whole-tree
// import sweep, so that deleting or renaming one of them fails loudly
// instead of quietly shrinking what is guaranteed.
//
// Paths are relative to the repository root; goSurface says whether the
// directory is checked as Go source (declarations and imports) or as text
// (the contract's JSON and the UI's TypeScript).
var domainSurfaces = []struct {
	path      string
	goSurface bool
}{
	{"api/v1", false},
	{"ui/shared/src", false},
	{"core/apicontract", true},
	{"core/internal/state", true},
	{"core/internal/model", true},
	{"core/internal/config", true},
	{"core/service", true},
	{"core/cmd", true},
}

// TestNoVendorTypeInTheProductsOwnSurfaces is the leak test, and it is
// deliberately not another string grep.
//
// The distinction it draws is the one the two halves of this boundary
// actually have. An operator writes `engine: kopia`, the API will carry
// that value and the UI will render it: the vendor's name as a VALUE is
// this product's own configuration vocabulary and costs nothing if the
// engine is ever replaced. A DECLARATION named after the vendor is the
// opposite -- a KopiaSnapshotInfo in a catalog row or an API response is
// the vendor's shape in this product's schema, and the migration to get it
// back out is a database and a published contract.
//
// So Go surfaces are parsed, and the names of every type, field, method,
// function and import are what is checked; a constant's value is not.
// Non-Go surfaces are text, where the same distinction is "the bare token,
// quoted" versus "glued to an identifier".
func TestNoVendorTypeInTheProductsOwnSurfaces(t *testing.T) {
	t.Parallel()

	repo := repoRoot(t)

	for _, surface := range domainSurfaces {
		dir := filepath.Join(repo, surface.path)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s: %v; this guard names its surfaces, so a moved or deleted one must fail rather than silently narrow the check", surface.path, err)
		}

		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() {
				if skippedTree(d.Name()) {
					return filepath.SkipDir
				}

				return nil
			}

			rel, _ := filepath.Rel(repo, path)

			if surface.goSurface {
				if !strings.HasSuffix(d.Name(), ".go") {
					return nil
				}

				for _, found := range vendorNamedDeclarations(t, path) {
					t.Errorf("%s declares %s; the embedded engine's shape may not appear in %s", rel, found, surface.path)
				}

				return nil
			}

			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			for _, found := range vendorWordsThatAreNotAValue(string(src)) {
				t.Errorf("%s names the embedded engine as %q; %s may carry the engine's identifier as a value and nothing else",
					rel, found, surface.path)
			}

			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", surface.path, err)
		}
	}

	// Both scanners are self-tested, because a scanner that finds nothing
	// because it looks for nothing is the failure mode of every check of
	// this shape. The surfaces above are clean today, so without these
	// two assertions this test would pass with either matcher broken.
	assertScannersActuallyMatch(t)
}

// vendorNamedDeclarations returns the declarations in one Go file whose
// NAME carries the vendor's, plus any import of it. Values are deliberately
// not examined: `EngineKopia BackupEngine = "kopia"` is this product's own
// enum, and refusing it would mean the configuration vocabulary EPIC K
// specifies could not be written down.
func vendorNamedDeclarations(t *testing.T, path string) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	named := func(name string) bool {
		return name != "" && strings.Contains(strings.ToLower(name), vendorName)
	}

	var found []string

	ast.Inspect(file, func(n ast.Node) bool {
		switch decl := n.(type) {
		case *ast.ImportSpec:
			if strings.Contains(strings.ToLower(decl.Path.Value), vendorName) {
				found = append(found, "an import of "+decl.Path.Value)
			}
		case *ast.TypeSpec:
			if named(decl.Name.Name) {
				found = append(found, "type "+decl.Name.Name)
			}
		case *ast.FuncDecl:
			if named(decl.Name.Name) {
				found = append(found, "func "+decl.Name.Name)
			}
		case *ast.Field:
			for _, name := range decl.Names {
				if named(name.Name) {
					found = append(found, "field or method "+name.Name)
				}
			}
		}

		return true
	})

	return found
}

// vendorWordsThatAreNotAValue returns every mention of the vendor in a
// non-Go surface that is part of a NAME rather than a bare token.
//
// The vendor's name quoted on its own -- in an enum, a discriminator, a
// test fixture -- is the engine's identifier travelling as data, which is
// what an operator types and what the API will carry. The same word glued
// to an identifier, or followed by a dot, is a name, and a name is a
// shape: either the vendor's type in this product's contract or a column
// that has to be migrated when the engine changes.
//
// The two neighbouring characters are all this needs to tell them apart,
// which is why this is a regexp and not a parser for two languages.
func vendorWordsThatAreNotAValue(src string) []string {
	mention := regexp.MustCompile(`(?i)(.?)` + vendorName + `(.?)`)

	var found []string

	for _, m := range mention.FindAllStringSubmatch(src, -1) {
		if identifierish(m[1]) || identifierish(m[2]) {
			found = append(found, m[0])
		}
	}

	return found
}

// identifierish reports whether a character neighbouring a vendor mention
// makes it part of a name. Punctuation that ENDS a token (a quote, a comma,
// a colon, a bracket, whitespace, a newline) does not; letters, digits,
// underscores, hyphens and dots do.
func identifierish(neighbour string) bool {
	for _, r := range neighbour {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return true
		case r == '_', r == '-', r == '.':
			return true
		}
	}

	return false
}

// assertScannersActuallyMatch drives both matchers over sources that must
// fail and sources that must pass, so the clean result above is evidence
// about the surfaces rather than about a broken scanner.
func assertScannersActuallyMatch(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	leaky := filepath.Join(dir, "leak.go")
	if err := os.WriteFile(leaky, []byte(
		"package catalog\n\n"+
			"import repo \"github.com/"+vendorName+"/"+vendorName+"/repo\"\n\n"+
			"type "+strings.ToUpper(vendorName[:1])+vendorName[1:]+"Snapshot struct {\n"+
			"\t"+strings.ToUpper(vendorName[:1])+vendorName[1:]+"ID string\n"+
			"}\n\n"+
			"func open() (*repo.Repository, error) { return nil, nil }\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if found := vendorNamedDeclarations(t, leaky); len(found) < 3 {
		t.Errorf("the Go scanner found %v in a file that leaks an import, a type and a field; it is not looking at what this test claims", found)
	}

	clean := filepath.Join(dir, "clean.go")
	if err := os.WriteFile(clean, []byte(
		"package model\n\n"+
			"type BackupEngine string\n\n"+
			"const EngineTwo BackupEngine = \""+vendorName+"\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if found := vendorNamedDeclarations(t, clean); len(found) != 0 {
		t.Errorf("the Go scanner rejected %v in a file whose only mention is an enum VALUE; the configuration vocabulary EPIC K specifies could not be written down", found)
	}

	for _, leak := range []string{
		`{"type": "` + vendorName + `Snapshot"}`,
		`interface ` + strings.ToUpper(vendorName[:1]) + vendorName[1:] + `Snapshot {}`,
		`const id = ` + vendorName + `.SnapshotID`,
		`{"column": "` + vendorName + `_snapshot_id"}`,
	} {
		if found := vendorWordsThatAreNotAValue(leak); len(found) == 0 {
			t.Errorf("the text scanner passed %q, which names the engine rather than carrying its identifier", leak)
		}
	}

	for _, ok := range []string{
		`{"engine": {"enum": ["artifact", "` + vendorName + `"]}}`,
		`engine === "` + vendorName + `" ? "Incremental" : "Artifact"`,
		`engine: ` + vendorName + `,`,
	} {
		if found := vendorWordsThatAreNotAValue(ok); len(found) != 0 {
			t.Errorf("the text scanner rejected %q (%v), which carries the engine's identifier as a value", ok, found)
		}
	}
}

// skippedTree names directories no boundary check learns anything from:
// dependency trees, build output, version control and fixtures.
func skippedTree(name string) bool {
	switch name {
	case "node_modules", ".git", "dist", "build", "coverage", ".next", "vendor", "testdata", ".venv":
		return true
	}

	return false
}

// repoRoot walks up from the module root to the directory holding the
// repository's own marker, so a sweep can cover every module rather than
// the one the test happens to live in.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir := moduleRoot(t)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.work found above the module root; this sweep cannot tell where the repository begins")
		}

		dir = parent
	}
}
