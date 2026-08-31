package dockerlease

import (
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

func TestSweepLeavesAContainerYoungerThanTheCutoff(t *testing.T) {
	requireDocker(t)
	id := create(t, true)

	sweepOlderThan(time.Now().Add(-time.Hour))

	if !exists(t, id) {
		t.Fatal("swept a labelled container created after the cutoff; a threshold that " +
			"reaches live containers would delete one out from under a running test")
	}
}

func TestSweepRemovesALabelledContainerOlderThanTheCutoff(t *testing.T) {
	requireDocker(t)
	id := create(t, true)

	// Cutoff in the future, so a container created a moment ago is stale.
	sweepOlderThan(time.Now().Add(time.Hour))

	if exists(t, id) {
		t.Fatal("left a labelled container older than the cutoff; this is the leak in #150")
	}
}

func TestSweepNeverTouchesAnUnlabelledContainer(t *testing.T) {
	requireDocker(t)
	mine := create(t, false)
	labelled := create(t, true)

	// Same future cutoff: both are old enough, only one is ours.
	sweepOlderThan(time.Now().Add(time.Hour))

	if !exists(t, mine) {
		t.Fatal("removed a container this repo does not own; the label filter is the only " +
			"thing standing between Sweep and somebody else's work")
	}
	if exists(t, labelled) {
		t.Fatal("did not remove the labelled container, so the previous assertion proves nothing")
	}
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
