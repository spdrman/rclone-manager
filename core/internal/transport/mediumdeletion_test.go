package transport_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNothingInProductionDeletesFromAMedium is EPIC E's Phase 1 exit-gate
// line held as a test rather than as a claim in a PR description:
// "nothing in this phase can delete an artifact copy anywhere".
//
// MediumStore has a DeleteObject, and the rclone adapter implements it,
// because a boundary that cannot delete is not the boundary a move engine
// gets built on and discovering that after the adapter exists is worse than
// deciding it now. artifactstore's Remove landed under exactly this
// argument and with exactly this property: it exists, and nothing in the
// pipeline calls it.
//
// So this walks every non-test Go file under core/ and refuses a call to
// DeleteObject anywhere but the two files that are allowed to name it: the
// interface that declares it and the adapter that implements it. When #238
// wires the move engine up, this test is the thing that has to be edited,
// deliberately, in the change that does it.
func TestNothingInProductionDeletesFromAMedium(t *testing.T) {
	root := coreModuleRoot(t)

	allowed := map[string]bool{
		filepath.Join("internal", "transport", "medium.go"):           true,
		filepath.Join("internal", "transport", "rclone", "medium.go"): true,
		// The contract suite is test support that does not carry a
		// _test.go suffix, because a suite has to be importable by the
		// packages it runs against. It deletes what it uploaded, inside
		// its own fixture, which is the opposite of a production
		// deletion path.
		filepath.Join("internal", "transport", "contract", "medium.go"): true,
	}

	var offenders []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		if strings.Contains(string(content), "DeleteObject(") && !allowed[rel] {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if scanned < 50 {
		t.Fatalf("only %d production Go files were scanned under %s, which is too few for this to be checking anything", scanned, root)
	}
	if len(offenders) > 0 {
		t.Errorf("these production files call DeleteObject: %v.\n"+
			"EPIC E Phase 1 adds no deletion path; a mover that deletes from a medium is #238's, "+
			"and the change that introduces it edits this test on purpose", offenders)
	}
}

// TestTheDeletionScanCanActuallyFail is the positive control. An absence
// assertion that cannot fail is not an assertion, and the scan above is the
// shape this repository has been caught by before: a walk that visits
// nothing, or a match that can never fire, passes silently forever.
func TestTheDeletionScanCanActuallyFail(t *testing.T) {
	root := coreModuleRoot(t)
	found := false
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(content), "DeleteObject(") {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if !found {
		t.Fatal("the scan found no mention of DeleteObject anywhere under core/, not even the adapter's own implementation, so its verdict about production callers means nothing")
	}
}

// coreModuleRoot walks up from this package to the directory holding
// core/'s go.mod, so the scans above cover the whole module rather than
// whatever directory the test happened to run in.
func coreModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("found no go.mod above %s", dir)
		}
		dir = parent
	}
}
