package mediumcheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// H2.2 (#622): the local hard drive's test connection.
//
// The three failures this file is really about are the three #622 names,
// and the point of every case below is that they are told APART. A check
// that failed for all three the same way would be a check an operator
// cannot act on: a missing path, a directory the service cannot write to
// and a filesystem with no room have three different fixes, and only two
// of them are on this machine at all.
//
// They are distinguished by the STEP, which is the field a surface
// branches on, and by the category where a transport-shaped one exists.
// Every case asserts both, plus the passing control, because a check that
// always fails is indistinguishable from a check that correctly fails.

// localReport runs the check against root with no thresholds and returns
// the report, which every case here starts from.
func localReport(t *testing.T, target LocalTarget) Report {
	t.Helper()
	report, err := RunLocal(context.Background(), nil, target)
	if err != nil {
		t.Fatalf("RunLocal: %v", err)
	}
	if len(report.Checks) != len(LocalSteps) {
		t.Fatalf("the report carries %d checks and LocalSteps has %d; a step that recorded nothing is a hole rather than silence",
			len(report.Checks), len(LocalSteps))
	}
	return report
}

// TestRunLocal_AWritableBackupRootIsReady is the control every other case
// here depends on. Without it, a RunLocal that failed on everything would
// satisfy all three failure cases below.
func TestRunLocal_AWritableBackupRootIsReady(t *testing.T) {
	root := t.TempDir()
	report := localReport(t, LocalTarget{Root: root})

	if !report.OK {
		t.Fatalf("a writable directory did not pass: %+v", report.Checks)
	}
	for _, step := range []Step{StepReach, StepDeliverable, StepSpace, StepWrite, StepReadBack, StepVerification, StepDelete} {
		if got := checkFor(t, report, step).Outcome; got != Passed {
			t.Errorf("%s = %s, want passed", step, got)
		}
	}
	// The two a directory cannot answer are SKIPPED and not quietly
	// passed, which is the rule Skipped exists as a first-class outcome
	// for: a surface rendering either as a pass would say something was
	// established that nobody looked at.
	for _, step := range []Step{StepCredentials, StepStorageClass} {
		if got := checkFor(t, report, step).Outcome; got != Skipped {
			t.Errorf("%s = %s, want skipped: a local directory has no answer for it", step, got)
		}
	}

	// No probe left behind. A destination this manager can write to and
	// not clean up is one no retention pass could ever tidy, and it would
	// leave a probe in somebody's backup directory.
	//
	// The reserved DIRECTORY does stay, which is deliberate and is the
	// only thing in the backup root afterwards. See localProbeDir: two
	// checks at once both create and both remove it, and removing it is
	// what let one refuse the other. What has to go is the file, and this
	// asserts both halves rather than counting entries and calling it
	// clean.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != localProbeDir {
		t.Errorf("the backup root holds %+v, want only the reserved probe directory", entries)
	}
	probes, err := os.ReadDir(filepath.Join(root, localProbeDir))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(probes) != 0 {
		t.Errorf("the check left %d probe(s) behind: %+v", len(probes), probes)
	}
}

// TestRunLocal_AMissingPathFailsReachAsNotFound is the first of the three,
// and on a NAS it is the common one: a volume that did not mount looks
// exactly like a working deployment until something writes.
func TestRunLocal_AMissingPathFailsReachAsNotFound(t *testing.T) {
	root := filepath.Join(t.TempDir(), "never-mounted")
	report := localReport(t, LocalTarget{Root: root})

	if report.OK {
		t.Fatal("the check passed against a directory that is not there")
	}
	reach := checkFor(t, report, StepReach)
	if reach.Outcome != Failed || reach.Category != "not_found" {
		t.Errorf("reach = %s(%s), want failed(not_found)", reach.Outcome, reach.Category)
	}
	// Everything past it was never tried, and says so.
	for _, step := range []Step{StepDeliverable, StepSpace, StepWrite, StepReadBack, StepDelete} {
		if got := checkFor(t, report, step).Outcome; got != Skipped {
			t.Errorf("%s = %s, want skipped after reach failed", step, got)
		}
	}
	// FR-33: the path is a fact about this machine and never reaches the
	// report. It goes to the log through Observe instead.
	for _, c := range report.Checks {
		if c.Detail == "" {
			t.Errorf("%s carries no sentence at all", c.Step)
		}
		if strings.Contains(c.Detail, root) {
			t.Errorf("%s puts a host path in the report: %q", c.Step, c.Detail)
		}
	}
}

// TestRunLocal_TheUnderlyingCauseGoesToTheLogAndNotTheReport is the other
// half of the line above: the diagnostic is not lost, it is routed. An
// operator's log is where a path belongs and a browser is not.
func TestRunLocal_TheUnderlyingCauseGoesToTheLogAndNotTheReport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "never-mounted")
	var observed []error
	if _, err := RunLocal(context.Background(), func(_ Step, err error) {
		observed = append(observed, err)
	}, LocalTarget{Root: root}); err != nil {
		t.Fatalf("RunLocal: %v", err)
	}

	if len(observed) == 0 {
		t.Fatal("nothing was observed for a failing check, so the diagnostic is dropped rather than routed")
	}
	found := false
	for _, err := range observed {
		if strings.Contains(err.Error(), root) {
			found = true
		}
	}
	if !found {
		t.Errorf("the observed cause does not carry the path, which is the one thing it exists to carry: %v", observed)
	}
}

// TestRunLocal_ADirectoryTheServiceCannotWriteToFailsWriteAsPermission is
// the second of the three. In a container it is the common one: the
// bind-mount belongs to a user on the host and this service is a
// different one.
//
// It is a REAL write rather than a comparison of file modes, which is the
// whole argument in localcheck.go: ACLs, group membership, read-only
// mounts and user namespaces all make a mode comparison a guess, and this
// process runs as the service uid, so the probe is asked with exactly the
// credentials a transfer would be.
func TestRunLocal_ADirectoryTheServiceCannotWriteToFailsWriteAsPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		// Root ignores the mode, so this case would assert that a write
		// which succeeded failed. Skipping is honest; asserting anything
		// here would be asserting about a different machine.
		t.Skip("running as root, which is not refused by a directory mode, so there is nothing here to observe")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	report := localReport(t, LocalTarget{Root: root})
	if report.OK {
		t.Fatal("the check passed against a directory it cannot write to")
	}
	// Reach passes: the directory is there and readable. That separation
	// is the point of the case, because "cannot see it" and "cannot write
	// to it" are different problems with different fixes.
	if got := checkFor(t, report, StepReach).Outcome; got != Passed {
		t.Errorf("reach = %s, want passed: the directory is there and is a directory", got)
	}
	write := checkFor(t, report, StepWrite)
	if write.Outcome != Failed || write.Category != "permission_denied" {
		t.Errorf("write = %s(%s), want failed(permission_denied)", write.Outcome, write.Category)
	}
}

// TestRunLocal_NoRoomFailsItsOwnStep is the third, and the one with no
// counterpart in the S3 check.
//
// The margin is what is driven rather than the disk, because filling a
// filesystem in a test is neither possible nor kind, and because the
// margin is the number that actually decides: a transfer refuses to start
// into less than it, so a destination below it is not ready however
// cheerfully a hundred-byte probe writes. Setting it above whatever this
// machine has free reaches the identical branch a full disk reaches.
func TestRunLocal_NoRoomFailsItsOwnStep(t *testing.T) {
	root := t.TempDir()
	report := localReport(t, LocalTarget{Root: root, SafetyMarginBytes: 1 << 62})

	if report.OK {
		t.Fatal("the check passed with less room than the deployment's own safety margin")
	}
	space := checkFor(t, report, StepSpace)
	if space.Outcome != Failed {
		t.Fatalf("space = %s, want failed", space.Outcome)
	}
	// No category, deliberately: no transport produced this, it is a
	// statfs reading weighed against the operator's own numbers, and
	// inventing a transport.Category for it would mean adding a member to
	// a vocabulary lifecycle branches on. The STEP is what tells this
	// apart from the other two, and the step is what a surface reads.
	if space.Category != "" {
		t.Errorf("space carries category %q; a failure no transport produced has none", space.Category)
	}
	// The three are distinct, which is the whole acceptance criterion:
	// reach and write are fine here, so nothing about this report could
	// be mistaken for a missing path or a permission problem.
	if got := checkFor(t, report, StepReach).Outcome; got != Passed {
		t.Errorf("reach = %s, want passed", got)
	}
	if got := checkFor(t, report, StepWrite).Outcome; got != Passed {
		t.Errorf("write = %s, want passed: a probe of a hundred bytes fits where a backup does not, which is exactly why the space step exists", got)
	}
}

// TestRunLocal_ADeploymentWithNoBackupRootSaysSo is the state a fresh
// install is in, and it is not an error: there is no backup set to derive
// a root from yet. Saying that beats checking the process's working
// directory, which is what a check with no root to fall back on would end
// up doing.
func TestRunLocal_ADeploymentWithNoBackupRootSaysSo(t *testing.T) {
	report := localReport(t, LocalTarget{})

	if report.OK {
		t.Fatal("the check passed with no backup root at all")
	}
	reach := checkFor(t, report, StepReach)
	if reach.Outcome != Failed {
		t.Errorf("reach = %s, want failed", reach.Outcome)
	}
	if reach.Category != "" {
		t.Errorf("reach carries category %q; nothing was asked of a filesystem, so nothing classified it", reach.Category)
	}
}

// TestRunLocal_APathThatIsAFileIsItsOwnRefusal: backups are written as
// files INSIDE the root, so a root that is a file is a typo in a path
// rather than a permission or a mount problem.
func TestRunLocal_APathThatIsAFileIsItsOwnRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	report := localReport(t, LocalTarget{Root: path})
	if report.OK {
		t.Fatal("the check passed against a backup root that is a file")
	}
	reach := checkFor(t, report, StepReach)
	if reach.Outcome != Failed || reach.Category != "configuration" {
		t.Errorf("reach = %s(%s), want failed(configuration)", reach.Outcome, reach.Category)
	}
}

// TestRunLocal_ReportsTheReservedLocalID pins the one string this package
// shares with internal/config without importing it. mediumcheck sits
// under everything and imports config nowhere, exactly as config does not
// import artifactstore to say MediumLocal and KindLocal are one word, so
// the agreement is held by a test rather than by a dependency.
func TestRunLocal_ReportsTheReservedLocalID(t *testing.T) {
	report := localReport(t, LocalTarget{Root: t.TempDir()})
	if report.Medium != "local" {
		t.Errorf("report.Medium = %q, want the reserved local id", report.Medium)
	}
}

// TestRunLocal_ACancelledContextIsAnErrorAndNotAReport keeps Run's own
// rule: a destination that does not work is a successful call with a
// report saying so, and the error return is for something that stopped
// the check running at all.
func TestRunLocal_ACancelledContextIsAnErrorAndNotAReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := RunLocal(ctx, nil, LocalTarget{Root: t.TempDir()}); !errors.Is(err, context.Canceled) {
		t.Errorf("RunLocal on a cancelled context = %v, want context.Canceled", err)
	}
}

// TestRunLocal_TwoChecksAtOnceDoNotFailEachOther is the concurrency case,
// and the failure it pins is worse than a flake.
//
// The check used to remove its probe DIRECTORY on the way out. Two checks
// running at once then raced on it: one removed the directory between the
// other's MkdirAll and its WriteFile, and the loser reported a failed
// write. Under fs.ErrNotExist that is the arm which says "the directory
// this deployment's backups land in went away between being looked at and
// being written to", which is the sentence this product uses for a NAS
// volume that did not mount, produced by somebody double-clicking a
// button.
//
// Two checks at once is not exotic. The destinations card and every tier
// picker offer this on one page, and the local destination is the one
// they all point at by default.
func TestRunLocal_TwoChecksAtOnceDoNotFailEachOther(t *testing.T) {
	root := t.TempDir()

	// Repeated, because the window is narrow: one pass could pass against
	// the bug. Each round runs a pair, which is the smallest arrangement
	// that can race at all.
	for round := 0; round < 40; round++ {
		var wg sync.WaitGroup
		reports := make([]Report, 2)
		errs := make([]error, 2)
		for i := range reports {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				reports[i], errs[i] = RunLocal(context.Background(), nil, LocalTarget{Root: root})
			}(i)
		}
		wg.Wait()

		for i, report := range reports {
			if errs[i] != nil {
				t.Fatalf("round %d, check %d: RunLocal: %v", round, i, errs[i])
			}
			if !report.OK {
				t.Fatalf("round %d, check %d failed against a writable directory, so one check refused because of the other: %+v",
					round, i, report.Failures())
			}
		}
	}
}

// TestRunLocal_LeavesItsOwnProbeDirectoryBehind pins the fix directly,
// because the case above is a race and a race that happens not to fire is
// a case that passes for the wrong reason.
//
// The directory stays. It is empty, it is reserved, it is named for what
// it is, and no artifact can be written under it, so leaving it costs an
// inode and buys the concurrency property above. The probe FILE is still
// removed, which is the thing that actually matters: a destination this
// manager can write to and not delete from is one no retention pass could
// ever clean up, and that claim is what StepDelete makes.
func TestRunLocal_LeavesItsOwnProbeDirectoryBehind(t *testing.T) {
	root := t.TempDir()
	if report := localReport(t, LocalTarget{Root: root}); !report.OK {
		t.Fatalf("precondition failed: %+v", report.Failures())
	}

	dir := filepath.Join(root, localProbeDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the probe directory is gone, so a concurrent check can have it removed underneath it: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the probe directory still holds %d entr(ies); the FILE has to go even though the directory stays: %+v", len(entries), entries)
	}
}
