package transport_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyTheMoveEngineDeletesFromAMedium is the line EPIC E's Phase 1
// exit gate held, moved forward by exactly one package.
//
// Through Phase 1 this test said "nothing in this phase can delete an
// artifact copy anywhere", and it named #238 as the change that would have
// to edit it. This is that change, and the edit is deliberately not an
// exemption: the claim is still an exhaustive one, it has just gone from
// "no production file calls DeleteObject" to "exactly one does, and it is
// the move engine".
//
// That is worth keeping as a whole-module scan even though
// internal/placement carries its own, tighter structural tests
// (TestThisPackageHasNoDeletePathOfItsOwn and
// TestTheSourceDeleteHasExactlyOneCaller). Those two prove the engine has
// one way out; this one proves nothing ELSE grew a second, somewhere the
// engine's own suite would never look. A retention pass, a catalog
// rebuild or an API handler reaching for DeleteObject directly is exactly
// the change that should have to come here and argue for itself.
func TestOnlyTheMoveEngineDeletesFromAMedium(t *testing.T) {
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
		// The move engine (#238, FR-30). It is the one production caller
		// there is meant to be, and it deletes in exactly two places: the
		// source copy, behind guardSourceDelete, and the destination copy
		// it created itself at a key it computed itself. Its own package
		// tests hold that line; this entry only records that the line
		// moved here and nowhere else.
		filepath.Join("internal", "placement", "engine.go"): true,
		// FR-20's prune, once the copy being pruned is an object rather
		// than a file (#239). It is a second deletion, and it is
		// deliberately not a second PACKAGE: the claim this test makes is
		// still exhaustive, and it is still "internal/placement is the
		// only production code that removes a copy of a backup from a
		// medium".
		//
		// It is here rather than folded into engine.go because it answers
		// a different question. The engine deletes a source BECAUSE it
		// just proved another copy exists; this deletes the last copy
		// BECAUSE no tier wants it any more, and its proof is FR-16's
		// identity re-check rather than a verified destination. Two
		// different proofs is exactly why they are two files with two
		// suites, and reclaim.go's own file comment spells out the
		// distinction the next reader will need.
		//
		// internal/retention decides the deletion and cannot reach this:
		// it holds a MediumPruner interface and never a transport type,
		// which is FR-32 held structurally rather than by this list.
		filepath.Join("internal", "placement", "reclaim.go"): true,
		// The medium preflight (#443). It is a third production caller and
		// it is deliberately NOT a third place "the ordering that protects
		// a backup gets decided", because it never touches a backup: the
		// only key it can ever pass to DeleteObject is one it generated
		// itself, from crypto/rand, under a reserved
		// .rclone-manager-preflight/ segment that transport.MediumKey
		// cannot produce for any configured artifact (a source, a backup
		// set and an artifact name each refuse a separator, so none of
		// them can spell that segment).
		//
		// So the invariant this test states is unchanged in substance and
		// wider in words: internal/placement remains the only production
		// code that removes a COPY OF A BACKUP from a medium, and this
		// file removes only the probe object it just wrote. Its own
		// package test pins the containment directly
		// (TestProbeKey_LivesUnderASegmentNoArtifactCanReach), and the
		// happy path asserts exactly one upload and exactly one delete, so
		// a preflight that grew a second deletion is a failure there
		// before it is one here.
		filepath.Join("internal", "mediumcheck", "mediumcheck.go"): true,
		// The move-crash harness is test support that does not carry a
		// _test.go suffix, for the contract suite's reason: it has to be
		// a separate main package so the suite can kill it. It only
		// decorates the adapter's DeleteObject to assert the standing
		// invariant before the call.
		filepath.Join("tests", "movecrash", "harness", "main.go"): true,
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
			"The move engine (internal/placement) is the one production caller EPIC E has; "+
			"a second one is a second place the ordering that protects a backup gets decided, "+
			"so a change that adds one has to come here and say why", offenders)
	}

	// The allow-list has to stay minimal. An entry that no longer deletes
	// is a permission nobody is using, and a permission nobody is using is
	// one the next file to need it inherits by accident.
	for rel := range allowed {
		content, readErr := os.ReadFile(filepath.Join(root, rel))
		if readErr != nil {
			t.Errorf("the allow-list names %s, which cannot be read: %v", rel, readErr)
			continue
		}
		if !strings.Contains(string(content), "DeleteObject(") {
			t.Errorf("the allow-list names %s, which no longer calls DeleteObject; drop it rather than leaving a permission lying around", rel)
		}
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
