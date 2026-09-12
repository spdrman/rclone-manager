package backupengine_test

import (
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
