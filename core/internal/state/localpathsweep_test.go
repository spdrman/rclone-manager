package state_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sweptFiles are the production files FR-29 names as asking "can I read
// this artifact locally": lifecycle's pre-delete revalidation, periodic
// revalidation, the application-level validate pass, and reconciliation.
//
// Every one of them used to read state.Record.LocalPath and os.Stat it.
// They now go through Record.ReadableLocalPath, and this test is what
// keeps them there: the point of routing four packages through one
// accessor is lost the moment a fifth change quietly reads the field
// again, and nothing else in the build would notice.
var sweptFiles = []string{
	filepath.Join("internal", "lifecycle", "remotedelete.go"),
	filepath.Join("internal", "revalidate", "checks.go"),
	filepath.Join("internal", "app", "validate.go"),
	filepath.Join("internal", "reconcile", "localcheck.go"),
}

// TestTheLocalPathSweepHolds refuses a read of the LocalPath field in any
// of the swept files.
//
// It matches on the field access rather than on the string "LocalPath",
// because these files legitimately mention it in prose: each one explains
// why it now asks the placement instead, and a check that banned the word
// would force the explanation out of the file that needs it most.
func TestTheLocalPathSweepHolds(t *testing.T) {
	root := coreModuleRootFrom(t)

	for _, rel := range sweptFiles {
		t.Run(rel, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatalf("reading %s: %v; if this file moved, move it here too rather than dropping it from the sweep", rel, err)
			}
			for i, line := range strings.Split(string(content), "\n") {
				code := line
				if idx := strings.Index(code, "//"); idx >= 0 {
					code = code[:idx]
				}
				if strings.Contains(code, "rec.LocalPath") || strings.Contains(code, "Record.LocalPath") {
					t.Errorf("%s:%d reads the LocalPath field directly:\n\t%s\nAsk Record.ReadableLocalPath instead: it is the one place the answer changes when an artifact's only copy is on a storage medium (#239).", rel, i+1, strings.TrimSpace(line))
				}
			}
		})
	}
}

// TestTheSweepCheckCanActuallyFail is the positive control. The check
// above is an absence assertion over a hand-maintained file list, which is
// two ways to pass for the wrong reason at once: a file that moved, and a
// pattern that never matches anything.
func TestTheSweepCheckCanActuallyFail(t *testing.T) {
	root := coreModuleRootFrom(t)

	// The pattern finds a real read where one genuinely still exists.
	// lifecycle/verify.go reads LocalPath on purpose and is deliberately
	// NOT in the sweep: at VERIFYING that field names the .partial being
	// hashed, which is an in-flight file rather than a durable copy, so a
	// placement is exactly the wrong thing to ask.
	content, err := os.ReadFile(filepath.Join(root, "internal", "lifecycle", "verify.go"))
	if err != nil {
		t.Fatalf("reading verify.go: %v", err)
	}
	if !strings.Contains(string(content), "rec.LocalPath") {
		t.Fatal("the pattern this sweep searches for matches nothing even in a file that definitely still reads the field, so the sweep check above proves nothing")
	}
}

func coreModuleRootFrom(t *testing.T) string {
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
