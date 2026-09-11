// Package mediumcheck_test carries issue #665's two pins against this
// package: core/internal/backend may not import mediumcheck (mediumcheck
// imports archive, placement and transport; backend must stay importable
// from all of those without closing a cycle - see backend/doc.go), so
// backend.ProbeStepNames is its own copy of mediumcheck.Steps as strings,
// and this is the external test package that may import both sides to
// hold the copy to its original.
package mediumcheck_test

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/spdrman/backupd/core/internal/backend"
	"github.com/spdrman/backupd/core/internal/mediumcheck"
)

// TestTheProbeStepVocabularyMatchesMediumcheckSteps is issue #665's test
// 26: backend.ProbeStepNames must equal mediumcheck.Steps, as strings,
// IN ORDER, both directions. A manifest's Probe declares one entry per
// name in this list in this exact order (backend.validateManifestProbe),
// so a drift here either lets a manifest omit a real step silently or
// makes it refuse a step mediumcheck no longer runs.
func TestTheProbeStepVocabularyMatchesMediumcheckSteps(t *testing.T) {
	if len(mediumcheck.Steps) == 0 {
		t.Fatal("mediumcheck.Steps is empty, so this test checks nothing")
	}
	if len(backend.ProbeStepNames) != len(mediumcheck.Steps) {
		t.Fatalf("backend.ProbeStepNames has %d entries, mediumcheck.Steps has %d",
			len(backend.ProbeStepNames), len(mediumcheck.Steps))
	}
	for i, step := range mediumcheck.Steps {
		if backend.ProbeStepNames[i] != string(step) {
			t.Errorf("position %d: backend.ProbeStepNames has %q, mediumcheck.Steps has %q",
				i, backend.ProbeStepNames[i], step)
		}
	}
}

// TestTheLocalManifestSkipsExactlyWhatTheLocalCheckSkips is issue #665's
// test 27, and it is what makes "the existing local behaviour is
// expressible in the manifest without a special case" mechanical rather
// than asserted: run RunLocal against a real, reachable temp directory,
// and the set of Steps its Report marks Skipped - restricted to the
// steps mediumcheck.Steps (the probe vocabulary) actually declares,
// since LocalSteps also carries "space", which is local-only and never
// part of a manifest's Probe - must equal the run:false steps
// bundled/local_volume.json declares, WITH THE SAME REASON STRINGS: this
// is what stops the two from drifting the moment somebody reworks
// localcheck.go's prose without touching the manifest, or the other way
// round.
func TestTheLocalManifestSkipsExactlyWhatTheLocalCheckSkips(t *testing.T) {
	report, err := mediumcheck.RunLocal(context.Background(), nil, mediumcheck.LocalTarget{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("RunLocal: %v", err)
	}

	probeVocabulary := map[string]bool{}
	for _, name := range backend.ProbeStepNames {
		probeVocabulary[name] = true
	}

	gotSkipped := map[string]string{} // step -> detail
	for _, c := range report.Checks {
		if !probeVocabulary[string(c.Step)] {
			continue // "space": local-only, never part of a manifest's probe
		}
		if c.Outcome == mediumcheck.Skipped {
			gotSkipped[string(c.Step)] = c.Detail
		}
	}

	reg, err := backend.Bundled()
	if err != nil {
		t.Fatalf("backend.Bundled(): %v", err)
	}
	localVolume, err := reg.Backend("local_volume")
	if err != nil {
		t.Fatalf("backend.Backend(\"local_volume\"): %v", err)
	}

	wantSkipped := map[string]string{}
	for _, step := range localVolume.Probe.Steps {
		if !step.Run {
			wantSkipped[step.Step] = step.Reason
		}
	}

	if len(wantSkipped) == 0 {
		t.Fatal("bundled/local_volume.json declares no skipped step, so this test checks nothing")
	}

	gotSteps := stepNames(gotSkipped)
	wantSteps := stepNames(wantSkipped)
	if !reflect.DeepEqual(gotSteps, wantSteps) {
		t.Fatalf("skipped-step sets disagree:\n  RunLocal:            %v\n  local_volume.json:   %v", gotSteps, wantSteps)
	}
	for step, wantReason := range wantSkipped {
		if got := gotSkipped[step]; got != wantReason {
			t.Errorf("step %q's skip reason disagrees:\n  RunLocal:            %q\n  local_volume.json:   %q", step, got, wantReason)
		}
	}
}

func stepNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
