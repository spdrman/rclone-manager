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

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/model"
)

// incrementalSet is a backup set resolved the way config.Validate resolves
// one that names the incremental engine, and a transport double that fails
// this test the instant the set's remote is touched from either side: the
// delete (issue #282's discipline, tr.poison) and the listing
// (failForSourceID, which matches on the set id).
//
// Both directions matter and neither is redundant. The delete is the
// outcome EPIC K forbids; the listing is how a source TREE gets walked as
// though its files were finished artifacts, which is what produces the
// journal rows a later cycle would then delete from.
func incrementalSet(t *testing.T) (config.BackupSet, *fakeTransport) {
	t.Helper()

	bs := testBackupSet(t, t.TempDir())
	bs.Engine = model.EngineKopia
	bs.Repository = model.RepositoryRef{Domain: "production", Set: bs.ID}
	bs.SourceIdentity = "1e3f0c2b4a5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"

	tr := newFakeTransport()
	tr.put("backup.dump", "a file inside the source tree", epoch.Unix())
	tr.poison = t
	tr.failForSourceID = bs.ID.String()
	tr.failErr = errors.New("the incremental set's remote was read, which this build must never do")

	return bs, tr
}

// TestRunCycle_AnIncrementalSetNeverReachesTheArtifactPipeline is the
// regression test EPIC K asks for by name, at the cycle entry point: the
// one `run` performs and `daemon` repeats.
func TestRunCycle_AnIncrementalSetNeverReachesTheArtifactPipeline(t *testing.T) {
	bs, tr := incrementalSet(t)

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

// TestFetch_AnIncrementalSetIsRefusedBeforeAnythingReadsTheSource is the
// second entry point, and the reason the guard cannot live in the cycle's
// loop alone.
//
// Fetch is not a shortcut into RunCycle: it calls reconcileOne, discoverOne
// and processArtifacts itself. So `backupd fetch`, the fetch action on the
// API and the button in the web UI were, until this guard, one operator
// click away from walking an incremental set's source tree and offering
// its files for deletion.
//
// --dry-run is covered in the same test because it is the same question
// asked without a write: it lists the remote and reports every object as a
// candidate, which for a source tree is an answer nobody can act on and a
// walk nobody asked for.
func TestFetch_AnIncrementalSetIsRefusedBeforeAnythingReadsTheSource(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		name := "fetch"
		if dryRun {
			name = "fetch --dry-run"
		}

		t.Run(name, func(t *testing.T) {
			bs, tr := incrementalSet(t)

			journal := openJournal(t)
			svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
			svc.Now = fixedNow(epoch)

			result, err := svc.Fetch(context.Background(), "production", bs.Name, dryRun)
			if !errors.Is(err, ErrEngineNotImplemented) {
				t.Fatalf("Fetch error = %v, want errors.Is(_, ErrEngineNotImplemented); anything else means the set ran", err)
			}
			if result.Set != bs.ID {
				t.Errorf("the refused result names set %q, want %q: a caller has to be able to say which set it asked about", result.Set, bs.ID)
			}
			if len(result.Preview) != 0 {
				t.Errorf("a refused fetch returned %d preview entr(ies); the source tree's files are not artifacts waiting to be fetched", len(result.Preview))
			}
			if len(result.Discovery.Discovered) != 0 {
				t.Errorf("a refused fetch discovered %d artifact(s)", len(result.Discovery.Discovered))
			}
			if got := tr.copyToLocalCalls(); got != 0 {
				t.Errorf("CopyToLocal was called %d time(s)", got)
			}
			if got := tr.deleteCallCount(); got != 0 {
				t.Errorf("DeleteRemote was called %d time(s), want 0", got)
			}
			if _, stillThere := tr.objects["backup.dump"]; !stillThere {
				t.Error("a file in the incremental set's source tree was deleted by a fetch")
			}

			records, listErr := journal.ListByBackupSet(context.Background(), bs.ID)
			if listErr != nil {
				t.Fatalf("ListByBackupSet: %v", listErr)
			}
			if len(records) != 0 {
				t.Errorf("a refused fetch left %d journal row(s) for an incremental set; those rows are what a later cycle resumes into a delete", len(records))
			}
		})
	}
}

// TestFetch_AnArtifactSetStillFetches is the control for both arms above.
func TestFetch_AnArtifactSetStillFetches(t *testing.T) {
	localDir := t.TempDir()

	bs := testBackupSet(t, localDir)
	bs.Engine = model.EngineArtifact
	bs.RemotePath = "" // fakeTransport ignores Source.Root.

	tr := newFakeTransport()
	tr.put("backup.dump", "fetch payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	// The dry run comes FIRST, and that ordering is the product's
	// behaviour rather than a test-writing preference: a live fetch on an
	// artifact set commits the artifact and then deletes the remote's own
	// copy (FR-15), so a preview taken afterwards correctly reports an
	// empty remote and would fail this control for the one reason that is
	// not a bug.
	preview, err := svc.Fetch(context.Background(), "production", bs.Name, true)
	if err != nil {
		t.Fatalf("Fetch --dry-run on an artifact set: %v", err)
	}
	if len(preview.Preview) == 0 {
		t.Error("a dry-run fetch on an artifact set previewed nothing")
	}

	result, err := svc.Fetch(context.Background(), "production", bs.Name, false)
	if err != nil {
		t.Fatalf("Fetch on an artifact set: %v", err)
	}
	if len(result.Discovery.Discovered) != 1 {
		t.Fatalf("Discovery.Discovered = %+v, want exactly one artifact", result.Discovery.Discovered)
	}
}

// TestReconcileAll_ReportsAnIncrementalSetWithoutReadingItsRemote covers
// the third entry point. Reconciliation cannot delete, and it does read
// every configured set's remote to settle what the journal believes about
// that set's artifacts -- which for an incremental set is a walk of a
// source tree that can only ever confirm the journal knows nothing.
//
// The refusal is per set, in that set's own row, because FR-1's rule is
// that one set's problem does not stop the others: the artifact set beside
// it in this fixture still reconciles, and that is what the second half
// asserts.
func TestReconcileAll_ReportsAnIncrementalSetWithoutReadingItsRemote(t *testing.T) {
	incremental, tr := incrementalSet(t)

	artifact := testBackupSet(t, t.TempDir())
	artifact.Name = "postgres-secondary"
	artifact.ID = mustSetID(t, "production", "postgres-secondary")
	artifact.RemotePath = ""

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", incremental, artifact)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	reports := svc.ReconcileAll(context.Background())
	if len(reports) != 2 {
		t.Fatalf("len(reports) = %d, want 2: a refused set is still a set this pass visited", len(reports))
	}

	if !errors.Is(reports[0].Err, ErrEngineNotImplemented) {
		t.Errorf("reports[0].Err = %v, want errors.Is(_, ErrEngineNotImplemented)", reports[0].Err)
	}
	if reports[0].Set != incremental.ID {
		t.Errorf("reports[0].Set = %q, want %q", reports[0].Set, incremental.ID)
	}

	// The control, and FR-1's isolation rule: the artifact set beside it
	// reconciled normally. Its own remote is the same fake transport, which
	// only fails for the incremental set's source id, so a guard that
	// refused both would show up here.
	if reports[1].Err != nil {
		t.Errorf("reports[1].Err = %v, want nil: one set's refusal must not stop the next set from reconciling", reports[1].Err)
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
