// A source-scanning guard rather than a behaviour test: it reads the four
// production files FR-29 named and refuses one that asks whether an
// artifact is readable locally without also asking where else it is.
//
// It is a scan because the failure it prevents cannot be caught by
// exercising this package. Every one of those callers used to read
// Record.LocalPath and os.Stat it, and reading only the new bool is a
// perfectly compiling, perfectly passing way to preserve the old bug: it
// reads "no readable local path" as "no copy at all" and quarantines a
// healthy artifact that has just been moved to a medium. That shipped once.
//
// A test that scans source is only as good as its own falsifiability, which
// is why the second test here plants a violation and insists the scan
// catches it. A sweep whose file list has gone stale, or whose pattern no
// longer matches anything, passes silently and looks exactly like a sweep
// that found nothing wrong.

package state_test

import (
	"fmt"
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

// TestTheLocalPathSweepHolds refuses, in every swept file, a read of the
// LocalPath field, a ReadableLocalPath call with no ActiveMediumPlacements
// call beside it, and a file that stopped asking altogether.
//
// # What this test can and cannot constrain (issue #434)
//
// The first version of this test checked only that the swept files did
// not read the field. It was green while three of the four read the
// accessor's false answer as "no durable copy" and quarantined every
// completed move, because it constrained that the question was asked and
// nothing about what was done with the answer. A structural test cannot
// express "what the caller does with the answer": that lives in each
// package's own behavioural tests against the shape a completed move
// leaves (a GONE local placement beside an ACTIVE medium one), which are
// internal/reconcile/onmedium_test.go, internal/lifecycle/onmedium_test.go,
// internal/app/validate_onmedium_test.go and
// internal/revalidate/medium_test.go.
//
// What a structural test CAN constrain is that the second half of the
// question is asked at all. ReadableLocalPath's false has two meanings,
// and Record.ActiveMediumPlacements is how a caller tells them apart, so
// a swept file that calls the first and never the second has, by
// construction, no way to tell a moved artifact from a lost one. That is
// the shape every one of #434's three defective callers had, and it is
// refused here, with a positive control below proving each refusal can
// fire.
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
			for _, v := range sweepViolations(rel, string(content)) {
				t.Error(v)
			}
		})
	}
}

// sweepViolations is the whole of the sweep's judgement over one file's
// source, as a list of refusals, so the positive control can run it over
// text that is known to violate each clause.
func sweepViolations(rel, content string) []string {
	var out []string
	asksLocal, asksMedium := false, false
	for i, line := range strings.Split(content, "\n") {
		code := line
		if idx := strings.Index(code, "//"); idx >= 0 {
			code = code[:idx]
		}
		if strings.Contains(code, "rec.LocalPath") || strings.Contains(code, "Record.LocalPath") {
			out = append(out, fmt.Sprintf("%s:%d reads the LocalPath field directly:\n\t%s\nAsk Record.ReadableLocalPath instead: it is the one place the answer changes when an artifact's only copy is on a storage medium (#239).", rel, i+1, strings.TrimSpace(line)))
		}
		if strings.Contains(code, ".ReadableLocalPath(") {
			asksLocal = true
		}
		if strings.Contains(code, ".ActiveMediumPlacements(") {
			asksMedium = true
		}
	}
	if !asksLocal {
		out = append(out, fmt.Sprintf("%s no longer calls Record.ReadableLocalPath at all. If it stopped asking whether the artifact is readable locally, take it out of sweptFiles on purpose rather than letting the sweep go quiet on it.", rel))
	}
	if asksLocal && !asksMedium {
		out = append(out, fmt.Sprintf("%s calls Record.ReadableLocalPath and never calls Record.ActiveMediumPlacements. A false answer from the first has two meanings, \"the copy is on a storage medium\" and \"there is no copy anywhere\", and a caller that never asks the second has no way to tell a completed move from a lost artifact. That is how the first moved artifact was marked QUARANTINED_LOST (#434). Ask both, and pin what this file does with the answer in its own package's tests against the shape a completed move leaves.", rel))
	}
	return out
}

// TestTheSweepCheckCanActuallyFail is the positive control. The check
// above is an absence assertion over a hand-maintained file list, which is
// two ways to pass for the wrong reason at once: a file that moved, and a
// pattern that never matches anything. The clauses issue #434 added are
// each run over source that is known to violate exactly one of them, so a
// clause that stopped firing is caught here rather than discovered by the
// next moved artifact.
func TestTheSweepCheckCanActuallyFail(t *testing.T) {
	root := coreModuleRootFrom(t)

	t.Run("the field-read pattern matches a real read", func(t *testing.T) {
		// lifecycle/verify.go reads LocalPath on purpose and is
		// deliberately NOT in the sweep: at VERIFYING that field names the
		// .partial being hashed, which is an in-flight file rather than a
		// durable copy, so a placement is exactly the wrong thing to ask.
		content, err := os.ReadFile(filepath.Join(root, "internal", "lifecycle", "verify.go"))
		if err != nil {
			t.Fatalf("reading verify.go: %v", err)
		}
		if !strings.Contains(string(content), "rec.LocalPath") {
			t.Fatal("the pattern this sweep searches for matches nothing even in a file that definitely still reads the field, so the sweep check above proves nothing")
		}
	})

	const (
		compliant = "localPath, ok := rec.ReadableLocalPath()\nif !ok {\n\tif ms := rec.ActiveMediumPlacements(); len(ms) > 0 {\n\t\treturn elsewhere\n\t}\n}\n"
		fieldRead = compliant + "info, err := os.Stat(rec.LocalPath)\n"
		localOnly = "localPath, ok := rec.ReadableLocalPath()\nif !ok {\n\treturn invalid(\"no local final path is recorded\")\n}\n"
		asksNone  = "// this file once asked rec.ReadableLocalPath() and rec.ActiveMediumPlacements()\nreturn nil\n"
	)

	cases := []struct {
		name    string
		source  string
		wantHit string // a fragment of the one refusal this source must earn
	}{
		{"a compliant caller earns no refusal", compliant, ""},
		{"a direct field read is refused", fieldRead, "reads the LocalPath field directly"},
		{"asking the local question alone is refused", localOnly, "never calls Record.ActiveMediumPlacements"},
		{"a file that stopped asking is refused", asksNone, "no longer calls Record.ReadableLocalPath"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sweepViolations("synthetic.go", tc.source)
			if tc.wantHit == "" {
				if len(got) != 0 {
					t.Fatalf("a compliant caller was refused:\n%s", strings.Join(got, "\n"))
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d refusals, want exactly the one for %q:\n%s", len(got), tc.wantHit, strings.Join(got, "\n"))
			}
			if !strings.Contains(got[0], tc.wantHit) {
				t.Fatalf("refusal = %q, want it to be the one about %q", got[0], tc.wantHit)
			}
		})
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
