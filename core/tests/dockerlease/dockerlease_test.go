package dockerlease

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The label is the entire safety boundary for Sweep, so these tests drive a
// real docker rather than a fake: the thing worth proving is that a `docker
// rm -f` built from a `--filter label=` never reaches a container somebody
// else owns.

func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("docker unavailable")
	}
}

// create makes a container without starting it. Created is all Sweep reads,
// and not starting one keeps the test cheap and side-effect free.
func create(t *testing.T, labelled bool) string {
	t.Helper()
	args := []string{"create"}
	if labelled {
		args = append(args, LabelFlag, LabelSpec)
	}
	args = append(args, "alpine:latest", "true")
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker create: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	return id
}

func exists(t *testing.T, id string) bool {
	t.Helper()
	return exec.Command("docker", "inspect", id).Run() == nil
}

// Every test here sweeps its OWN ids and nothing else. sweepOlderThan with
// a cutoff in the future is not an option: it lists every labelled container
// on the daemon, and under `go test ./...` those include live SFTP fixtures
// held by tests/sftpintegration, tests/crashmatrix, core/service and
// core/internal/transport/rclone, running in parallel with this package, plus
// every other worktree sharing this machine's daemon. See
// TestNoSweepTestReapsAContainerItDoesNotOwn.

func TestSweepLeavesAContainerYoungerThanTheCutoff(t *testing.T) {
	requireDocker(t)
	id := create(t, true)

	sweepIDs([]string{id}, time.Now().Add(-time.Hour))

	if !exists(t, id) {
		t.Fatal("swept a labelled container created after the cutoff; a threshold that " +
			"reaches live containers would delete one out from under a running test")
	}
}

func TestSweepRemovesALabelledContainerOlderThanTheCutoff(t *testing.T) {
	requireDocker(t)
	id := create(t, true)

	// Cutoff in the future, so a container created a moment ago is stale.
	sweepIDs([]string{id}, time.Now().Add(time.Hour))

	if exists(t, id) {
		t.Fatal("left a labelled container older than the cutoff; this is the leak in #150")
	}
}

// TestSweepNeverTouchesAnUnlabelledContainer asks the question through
// listLabelled rather than by sweeping, because the label filter IS
// listLabelled: sweepIDs removes whatever it is handed. Asserting on the
// candidate list proves the same boundary and, unlike a sweep with a future
// cutoff, cannot take a live container in another package down with it.
func TestSweepNeverTouchesAnUnlabelledContainer(t *testing.T) {
	requireDocker(t)
	mine := create(t, false)
	labelled := create(t, true)

	candidates := listLabelled()

	if !containsID(candidates, labelled) {
		t.Fatal("listLabelled did not offer up a labelled container at all, so the absence of the unlabelled one below would prove nothing")
	}
	if containsID(candidates, mine) {
		t.Fatal("listLabelled offered up a container this repo does not own; the label filter is the only " +
			"thing standing between Sweep and somebody else's work")
	}
}

// containsID compares by prefix because `docker ps -q` answers in short ids
// while `docker create` returns the full one.
func containsID(ids []string, id string) bool {
	for _, got := range ids {
		if got != "" && (strings.HasPrefix(id, got) || strings.HasPrefix(got, id)) {
			return true
		}
	}
	return false
}

// TestSweepStillReapsWhenAListedContainerVanishedFromUnderIt is issue #161's
// sweeper finding. StaleAfter is fifteen minutes, and yet #161 found
// rclone-manager-gate-sftp-* containers still running after 4 and 11 hours.
// Part of that is simply that they predate the label (it landed the same
// morning, in #151), but there is a live defect underneath it: the batch
// `docker inspect` used to date the candidates exits non-zero if ANY of its
// arguments is missing, and that exit status was being read as "nothing can
// be dated". One container removed by another worktree between the listing
// and the inspect therefore turned the entire sweep into a silent no-op.
func TestSweepStillReapsWhenAListedContainerVanishedFromUnderIt(t *testing.T) {
	requireDocker(t)

	// Positive control: the same call, with a batch containing nothing
	// stale-but-missing, does reap. Without it "the live one is gone"
	// below could just as well mean sweepIDs never works at all.
	control := create(t, true)
	sweepIDs([]string{control}, time.Now().Add(time.Hour))
	if exists(t, control) {
		t.Fatal("sweepIDs left a labelled container older than the cutoff even with a clean batch, so it does not reap at all and the assertion below would prove nothing")
	}

	live := create(t, true)
	const vanished = "0000000000000000000000000000000000000000000000000000000000000000"
	sweepIDs([]string{live, vanished}, time.Now().Add(time.Hour))
	if exists(t, live) {
		t.Fatal("one already-vanished id in the batch made the whole sweep a no-op; on a machine where several worktrees share one docker daemon that race is routine, and a sweeper that silently sweeps nothing is exactly the class of bug #161 is about")
	}
}

// --- this package must not reap other packages' live containers -----------

// runSelf re-runs some of this package's own tests in a child process, so
// the parent can watch what they did to a container they never created.
func runSelf(t *testing.T, pattern string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run="+pattern, "-test.v=true", "-test.timeout=2m")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s in a child process failed:\n%s", pattern, out)
	}
	return string(out)
}

// TestNoSweepTestReapsAContainerItDoesNotOwn is the #161 root cause, and it
// is this package's own tests doing it.
//
// `go test ./...` runs packages in parallel. While tests/dockerlease is
// running, tests/sftpintegration, tests/crashmatrix, core/service and
// core/internal/transport/rclone are each holding a LIVE, labelled SFTP
// fixture container, and so is every other worktree on this machine running
// its own gate against the same docker daemon. A sweep here with a cutoff in
// the future matches every labelled container in existence, not just this
// test's own, so it kills healthy fixtures belonging to tests that have
// nothing to do with it.
//
// That is exactly the signature in the issue: the container vanishes
// mid-suite, everything after it gets connection refused, and the victim is
// a different test on every run, because the victim is simply whichever
// fixture happened to be alive when this package ran. Captured from the
// daemon's own event stream during a run of this branch: `kill signal=9`,
// `die exitCode=137` and `destroy` on a two-second-old SFTP fixture, in the
// same instant as this package's own throwaway containers were being created
// and destroyed around it.
func TestNoSweepTestReapsAContainerItDoesNotOwn(t *testing.T) {
	requireDocker(t)

	// A labelled container this package does not own, standing in for the
	// live fixture another package is holding at the same moment.
	bystander := create(t, true)
	if !exists(t, bystander) {
		t.Fatal("the bystander is not there before the sweep tests run, so the assertion after them would be about nothing")
	}
	if exists(t, "0000000000000000000000000000000000000000000000000000000000000000") {
		t.Fatal("exists() says yes to an id that cannot be there, so it can never report a reaping")
	}

	runSelf(t, "^TestSweep")

	if !exists(t, bystander) {
		t.Fatalf("this package's own sweep tests destroyed a labelled container (%s) they never created. Under `go test ./...` that container is somebody else's live SFTP fixture, which is the container death in #161", bystander)
	}
}
