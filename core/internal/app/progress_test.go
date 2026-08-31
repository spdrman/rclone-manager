package app

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// recordingObserver collects every reading a cycle publishes.
type recordingObserver struct {
	mu       sync.Mutex
	readings []Progress
}

func (r *recordingObserver) ObserveProgress(p Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readings = append(r.readings, p)
}

func (r *recordingObserver) all() []Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Progress, len(r.readings))
	copy(out, r.readings)
	return out
}

// reportingTransport is fakeTransport with the one thing the real rclone
// adapter does that the fake does not: it reports byte progress to
// whatever transport.ProgressReporter the caller put on the context.
//
// It reports in three steps rather than one so a test can tell a feed that
// only ever says "nothing" and "everything" apart from one that actually
// measures a transfer in flight.
type reportingTransport struct {
	*fakeTransport
}

func (r reportingTransport) CopyToLocal(ctx context.Context, source transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	obj, ok := r.objects[remotePath]
	if ok {
		if reporter := transport.ProgressReporterFrom(ctx); reporter != nil {
			total := int64(len(obj.data))
			for _, done := range []int64{0, total / 2, total} {
				reporter.CopyProgress(transport.ByteProgress{
					BytesTransferred: done,
					BytesTotal:       total,
					BytesPerSecond:   1024,
				})
			}
		}
	}
	return r.fakeTransport.CopyToLocal(ctx, source, remotePath, localPartialPath)
}

// TestRunCycle_PublishesLiveProgress is issue #221's core claim at the
// layer that owns the cycle: a run cycle reports where it is, which
// artifact it is on, and how far through that artifact's copy it has got,
// to an observer the caller supplied, and it does so from inside the real
// pipeline rather than from a summary written afterwards.
func TestRunCycle_PublishesLiveProgress(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := reportingTransport{fakeTransport: newFakeTransport()}
	tr.put("backup.dump", "cycle payload for progress", epoch.Unix())

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	obs := &recordingObserver{}
	report := svc.RunCycle(WithProgressObserver(context.Background(), obs))

	if len(report.Sets) != 1 || report.Sets[0].Err != nil {
		t.Fatalf("cycle did not run cleanly: %+v", report.Sets)
	}

	readings := obs.all()
	if len(readings) == 0 {
		t.Fatal("the cycle published no progress at all")
	}

	// Every stage the pipeline actually performs is reported. A feed that
	// only ever says "transferring" would leave the checklist frozen on
	// the first step for the rest of the run.
	seen := map[string]bool{}
	for _, r := range readings {
		seen[r.Stage] = true
	}
	for _, stage := range Stages {
		if !seen[stage] {
			t.Errorf("no reading reported stage %q; stages seen were %v", stage, seen)
		}
	}

	// The set count is known up front and is reported as such: one
	// configured, enabled set here.
	for i, r := range readings {
		if r.BackupSetsTotal != 1 {
			t.Fatalf("reading %d reports BackupSetsTotal = %d, want 1", i, r.BackupSetsTotal)
		}
	}
	if last := readings[len(readings)-1]; last.BackupSetsDone != 1 {
		t.Errorf("the final reading reports BackupSetsDone = %d, want 1: a finished set that still counts as unfinished leaves the count stuck", last.BackupSetsDone)
	}

	// The artifact is named while it is being worked on, not only
	// afterwards.
	named := false
	for _, r := range readings {
		if r.Stage == StageTransferring && r.Artifact == "backup.dump" {
			named = true
		}
	}
	if !named {
		t.Errorf("no transferring reading named the artifact being copied; readings: %+v", readings)
	}

	// The intermediate byte reading: the whole reason this is not just the
	// operations table with extra steps.
	intermediate := 0
	for _, r := range readings {
		if r.BytesTransferred == nil || r.BytesTotal == nil {
			continue
		}
		if *r.BytesTransferred > 0 && *r.BytesTransferred < *r.BytesTotal {
			intermediate++
		}
	}
	if intermediate == 0 {
		t.Errorf("no reading reported a byte count strictly between zero and the artifact's size; a feed that only ever reads 0 and 100%% is what issue #221 reports")
	}

	// Byte counters belong to a copy in flight. A verifying or committing
	// stage still carrying the previous transfer's numbers would report a
	// finished copy as though it were still running.
	for i, r := range readings {
		if r.Stage == StageTransferring {
			continue
		}
		if r.BytesTransferred != nil || r.BytesTotal != nil || r.BytesPerSecond != nil {
			t.Errorf("reading %d is at stage %q but still carries byte counters (%s of %s at %s): those describe a copy in flight and there is none",
				i, r.Stage, shown(r.BytesTransferred), shown(r.BytesTotal), shown(r.BytesPerSecond))
		}
	}

	// The artifact this cycle finished is counted.
	if last := readings[len(readings)-1]; last.ArtifactsDone != 1 {
		t.Errorf("the final reading reports ArtifactsDone = %d, want 1", last.ArtifactsDone)
	}
}

// TestRunCycle_CountsEveryEnabledBackupSetAndSkipsDisabledOnes pins the one
// denominator this feed does report. It is knowable at the start of the
// cycle (it is a count of the configuration snapshot the cycle began
// with), and it must count what the cycle will actually visit: a disabled
// set is never processed, so counting it would leave the cycle reporting
// "set 1 of 2" and then stopping.
func TestRunCycle_CountsEveryEnabledBackupSetAndSkipsDisabledOnes(t *testing.T) {
	enabled := testBackupSet(t, t.TempDir())
	enabled.RemotePath = ""

	disabled := testBackupSet(t, t.TempDir())
	disabled.Name = "disabled-set"
	disabled.ID = mustSetID(t, "production", "disabled-set")
	disabled.RemotePath = ""
	disabled.Disabled = true

	second := testBackupSet(t, t.TempDir())
	second.Name = "second-set"
	second.ID = mustSetID(t, "production", "second-set")
	second.RemotePath = ""

	tr := reportingTransport{fakeTransport: newFakeTransport()}
	tr.put("backup.dump", "payload", epoch.Unix())

	svc := New(testConfig(t, testSource("production", enabled, disabled, second)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	obs := &recordingObserver{}
	svc.RunCycle(WithProgressObserver(context.Background(), obs))

	readings := obs.all()
	if len(readings) == 0 {
		t.Fatal("the cycle published no progress at all")
	}
	for i, r := range readings {
		if r.BackupSetsTotal != 2 {
			t.Fatalf("reading %d reports BackupSetsTotal = %d, want 2 (three configured, one disabled)", i, r.BackupSetsTotal)
		}
	}
	if last := readings[len(readings)-1]; last.BackupSetsDone != 2 {
		t.Errorf("the final reading reports BackupSetsDone = %d, want 2", last.BackupSetsDone)
	}
}

// TestRunCycle_WithoutAnObserverIsUnchanged is the negative control. Every
// caller in this repository before issue #221, and the CLI still, runs a
// cycle with no observer at all; that path must not depend on progress
// existing.
func TestRunCycle_WithoutAnObserverIsUnchanged(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := reportingTransport{fakeTransport: newFakeTransport()}
	tr.put("backup.dump", "payload", epoch.Unix())

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(context.Background())
	if len(report.Sets) != 1 || report.Sets[0].Err != nil {
		t.Fatalf("cycle did not run cleanly without an observer: %+v", report.Sets)
	}
	if entries, err := os.ReadDir(localDir); err != nil || len(entries) == 0 {
		t.Fatalf("the cycle transferred nothing without an observer (ReadDir(%q) = %v, %v)", localDir, entries, err)
	}
}

// shown renders a nullable counter for a failure message. A raw *int64
// prints as an address, which tells a reader nothing about the reading
// that was wrong.
func shown(v *int64) string {
	if v == nil {
		return "unmeasured"
	}
	return strconv.FormatInt(*v, 10)
}
