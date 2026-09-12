// EPIC K's single non-negotiable rule, held where it can actually be
// broken: a backup set running the incremental engine must never have its
// source deleted because a run finished.
//
// The hazard is not hypothetical and it is not in the future. #780 taught
// config.Validate to ACCEPT `engine: kopia`, because the schema seam has to
// exist before the repository adapter (#781) and the snapshot lifecycle
// (#783) can be built against it. This package's cycle, meanwhile, skips
// exactly two kinds of set -- disabled, and held for editing -- and runs
// everything else through the artifact pipeline: reconcile, discover the
// tree, copy every matching file, commit, and then offer the source's copy
// for deletion unless the set is read-only.
//
// So for as long as the incremental pipeline does not exist, an operator
// who configured the engine this build says it accepts would get their
// source TREE walked as if every file in it were a finished artifact, and
// then deleted. That is the one outcome EPIC K names as unacceptable, and
// it would arrive as a successful-looking cycle.
//
// The test below is written the way issue #282's read-only proof is: the
// transport double fails the test the instant a delete is invoked, rather
// than the assertion checking a refusal after the fact. It also proves the
// set never reached DISCOVERY at all, which is the stronger claim and the
// one that matters for a source tree: an incremental set's remote must not
// be walked, copied or deleted by this build.

package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/model"
)

// TestRunCycle_AnIncrementalSetNeverReachesTheArtifactPipeline is the
// regression test EPIC K asks for by name.
func TestRunCycle_AnIncrementalSetNeverReachesTheArtifactPipeline(t *testing.T) {
	localDir := t.TempDir()

	bs := testBackupSet(t, localDir)
	bs.Engine = model.EngineKopia
	bs.Repository = model.RepositoryRef{Domain: "production", Set: bs.ID}
	bs.SourceIdentity = "1e3f0c2b4a5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"

	tr := newFakeTransport()
	tr.put("backup.dump", "a file inside the source tree", epoch.Unix())
	// Issue #282's discipline: the delete fails this test the moment it is
	// called, so nothing here rests on a post-hoc count.
	tr.poison = t
	// And the same for the read half, from the other direction: if
	// discovery runs at all, the set's error becomes this one instead of
	// the refusal, so the assertion below cannot pass by accident.
	tr.failForSourceID = bs.ID.String()
	tr.failErr = errors.New("the incremental set's remote was listed, which this build must never do")

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(context.Background())

	if len(report.Sets) != 1 {
		t.Fatalf("len(report.Sets) = %d, want 1; a refused set is still a set this cycle visited, and dropping it from the report hides the refusal from every surface that reads it", len(report.Sets))
	}

	set := report.Sets[0]
	if !errors.Is(set.Err, ErrEngineNotImplemented) {
		t.Fatalf("BackupSetCycleResult.Err = %v, want errors.Is(_, ErrEngineNotImplemented); anything else means the set ran", set.Err)
	}

	// The refusal is what an operator reads, and it has to distinguish a
	// build limitation from a configuration mistake: the config is
	// correct, this build simply cannot run it yet.
	for _, want := range []string{string(model.EngineKopia), "#783"} {
		if !strings.Contains(set.Err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q, so it does not say which engine was refused or that the limitation is the build's", set.Err, want)
		}
	}

	// Nothing was discovered, which is the claim that matters for a source
	// tree: no walk, no copy, no journal row that a later cycle would
	// resume into a delete.
	if len(set.Discovery.Discovered) != 0 {
		t.Errorf("discovery recorded %d artifact(s) for an incremental set", len(set.Discovery.Discovered))
	}
	if got := tr.copyToLocalCalls(); got != 0 {
		t.Errorf("CopyToLocal was called %d time(s); an incremental set's source must not be copied file by file", got)
	}
	if got := tr.deleteCallCount(); got != 0 {
		t.Errorf("DeleteRemote was called %d time(s), want 0", got)
	}
	if _, stillThere := tr.objects["backup.dump"]; !stillThere {
		t.Error("a file in the incremental set's source tree was deleted; a completed run must never delete anything on the source")
	}
}

// TestRunCycle_AnArtifactSetStillRuns is the control the refusal above
// needs. Without it a guard that refused every set would pass that test
// for entirely the wrong reason, and this package's whole purpose would be
// broken with one test still green.
//
// It covers both spellings of an artifact set: the resolved value
// config.Validate fills in, and the zero value a hand-built config carries
// (which is every fixture in this package, and is why the guard treats ""
// as runnable).
func TestRunCycle_AnArtifactSetStillRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		engine model.BackupEngine
	}{
		{"resolved by Validate", model.EngineArtifact},
		{"a config that skipped Validate", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localDir := t.TempDir()

			bs := testBackupSet(t, localDir)
			bs.Engine = tc.engine
			bs.RemotePath = "" // fakeTransport ignores Source.Root.

			tr := newFakeTransport()
			tr.put("backup.dump", "cycle payload", epoch.Unix())

			journal := openJournal(t)
			svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
			svc.Now = fixedNow(epoch)

			report := svc.RunCycle(context.Background())

			if len(report.Sets) != 1 {
				t.Fatalf("len(report.Sets) = %d, want 1", len(report.Sets))
			}
			if set := report.Sets[0]; set.Err != nil {
				t.Fatalf("BackupSetCycleResult.Err = %v, want nil: an artifact set must run exactly as it always has", set.Err)
			}
			if len(report.Sets[0].Discovery.Discovered) != 1 {
				t.Fatalf("Discovery.Discovered = %+v, want exactly one artifact", report.Sets[0].Discovery.Discovered)
			}
		})
	}
}

// TestUnrunnableEngine_RefusesOnlyWhatThisBuildCannotRun pins the predicate
// itself, including the engine nobody has taught it about: an engine this
// build does not recognise is refused rather than run, because running it
// means running the artifact pipeline over whatever it actually is.
func TestUnrunnableEngine_RefusesOnlyWhatThisBuildCannotRun(t *testing.T) {
	for _, runnable := range []model.BackupEngine{model.EngineArtifact, ""} {
		if err := unrunnableEngine(runnable); err != nil {
			t.Errorf("unrunnableEngine(%q) = %v, want nil", runnable, err)
		}
	}

	for _, refused := range []model.BackupEngine{model.EngineKopia, model.BackupEngine("something-else")} {
		err := unrunnableEngine(refused)
		if err == nil {
			t.Errorf("unrunnableEngine(%q) = nil; an engine this build has no pipeline for must not be run through the artifact pipeline", refused)

			continue
		}
		if !errors.Is(err, ErrEngineNotImplemented) {
			t.Errorf("unrunnableEngine(%q) = %v, want errors.Is(_, ErrEngineNotImplemented)", refused, err)
		}
	}
}
