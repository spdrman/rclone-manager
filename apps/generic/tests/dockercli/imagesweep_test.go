// Image cleanup for issue #185's per-run image reference.
//
// One shared tag had exactly one virtue: it could not grow. A reference
// per run trades a collision for unbounded disk unless every run's image
// goes away again, and on a machine that already had 18.5 GB of images
// and 15.9 GB of build cache when #161 looked (see that issue), "the
// suite leaks an image per run" is not a theoretical cost.
//
// So there are two mechanisms, for two different exits:
//
//   - TestMain removes this run's image after m.Run returns. That covers
//     a pass, a failure, a t.Fatalf and a skip, which is every exit the
//     test binary itself controls.
//   - sweepImages, called on the way IN to a build, reclaims images left
//     by runs that never got to run their own cleanup: a `go test`
//     timeout, a Ctrl-C, an agent cancelling the command. t.Cleanup and
//     TestMain both run in-process, so a SIGKILL takes them with it, and
//     no amount of trying harder on the way out can fix that. A run
//     started afterwards cleans up what the killed one left.
//
// That second half is deliberately the same shape as core/tests/
// dockerlease, and the issue asks whether that sweeper already covers
// this. It does not, and cannot as written: it is containers only, end to
// end. It lists with `docker ps -aq --filter label=...`, dates with
// `docker inspect`, and removes with `docker rm -f`. Images live in a
// different namespace with different verbs (`docker images`, `docker
// image inspect`, `docker rmi`), so covering them is new code wherever it
// lives, not a new call to Sweep. It also matters that images are not
// dated the same way: see bornLabelKey.
//
// It lives here, next to the only suite in the repository that builds an
// image, rather than in dockerlease, for two reasons. dockerlease is
// core/'s, and core/ must build and test with apps/ deleted entirely
// (§7.1), so a second suite wanting this would be under apps/ too. And
// dockerlease is under active change on #161 right now. If a second
// image-building suite ever appears, lifting these three functions into
// dockerlease is the obvious move, and the label constants are already
// shaped for it.
//
// The one finding from #161 that does carry straight over is its sweeper
// bug, and bornOf below is written against it: a batch `docker inspect`
// exits non-zero if any one of its arguments has gone, while still
// printing a good line for every id it did find. Reading that status as
// "nothing can be dated" turned the whole sweep into a silent no-op the
// moment another worktree removed something between the listing and the
// inspect, which on this machine is routine. This code parses what comes
// back and ignores the status, for the same reason.
package dockercli_test

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
)

const (
	// imageStaleAfter is how old an image built by this suite must be
	// before a later run will reclaim it.
	//
	// Twice dockerlease.StaleAfter, derived from it rather than picked,
	// because an image and a container are exposed for different lengths
	// of time. A fixture container is created at the point it is used and
	// removed a moment later, so fifteen minutes clears it comfortably. An
	// image is stamped when the build finishes and then has to survive
	// every remaining test in the run, including a compose stack with
	// health-gated startup. Reaping one out from under a slow but healthy
	// run would turn this cleanup into a new source of exactly the
	// cross-worktree interference #185 is about, and the only cost of the
	// wider margin is that a killed run's image lingers a little longer.
	imageStaleAfter = 2 * dockerlease.StaleAfter

	// dockerTimeout bounds every docker call this file makes. Cleanup
	// must never be the reason a suite hangs.
	dockerTimeout = 30 * time.Second
)

// TestMain removes the image this run built once every test has
// finished with it.
//
// os.Exit skips deferred functions, so the removal is written out
// straight-line before it rather than deferred, and the code from m.Run
// is carried across it: cleanup must not be able to turn a failing run
// green or a green run red.
func TestMain(m *testing.M) {
	code := m.Run()
	removeBuiltImage()
	os.Exit(code)
}

// removeBuiltImage removes this run's own image, if this run got as far
// as building one. Best-effort and silent, like the sweep: a housekeeping
// step that can fail a suite is worse than the leak it fixes.
func removeBuiltImage() {
	if !builtImage {
		return
	}
	_, _ = dockerRun("rmi", "-f", imageReference())
}

var sweepImagesOnce sync.Once

// sweepImages reclaims images this suite built more than imageStaleAfter
// ago, at most once per test binary however many times it is called.
// Call it before building.
func sweepImages() {
	sweepImagesOnce.Do(func() {
		// Every value of the label, not just imageLabelValue. Anything
		// this suite builds carries the key, including the fixtures its
		// own tests build under a per-run value, and all of them are
		// stamped with a real build time, so one rule ages them all
		// correctly and nothing this suite creates is exempt from being
		// reclaimed after a kill.
		sweepImagesOlderThan(anyLabelValue, time.Now().Add(-imageStaleAfter))
	})
}

// anyLabelValue asks listLabelledImages for every image carrying
// imageLabelKey whatever its value, which is what a real sweep wants.
const anyLabelValue = ""

// sweepImagesOlderThan is sweepImages's body with both of its inputs
// lifted out.
//
// The cutoff is a parameter for the reason dockerlease's is: a test has
// to be able to put an image on either side of it without waiting out
// imageStaleAfter. The label value is a parameter for a reason specific
// to this machine: several worktrees run this suite at once, so a test
// that swept every value would be sweeping other agents' images, and "the
// image I expected to survive is gone" would stop meaning anything. Its
// own test therefore sweeps a value only that run can write, while the
// real sweep passes anyLabelValue and covers everything.
func sweepImagesOlderThan(label string, cutoff time.Time) {
	ids := listLabelledImages(label)
	stale := make([]string, 0, len(ids))
	for id, born := range bornOf(ids) {
		if born.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return
	}
	_, _ = dockerRun(append([]string{"rmi", "-f"}, stale...)...)
}

// listLabelledImages returns the full ids of every image carrying
// imageLabelKey, restricted to one value of it unless given
// anyLabelValue. That key is the whole safety boundary: an image without
// it is never a candidate, whatever its age.
//
// `-a` matters. An image whose tag has already been taken over or removed
// is untagged, and `docker images` hides untagged images unless asked for
// them, so leaving it off would silently exempt exactly the leftovers
// most worth reclaiming.
func listLabelledImages(label string) []string {
	filter := "label=" + imageLabelKey
	if label != anyLabelValue {
		filter += "=" + label
	}
	out, err := dockerRun("images", "-a", "-q", "--no-trunc", "--filter", filter)
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	ids := make([]string, 0, 8)
	for _, id := range strings.Fields(out) {
		// One image with several tags is listed once per tag, and
		// `docker rmi` on a repeated id fails on the second attempt.
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// bornOf maps image id to the moment this suite built it, read from
// bornLabelKey.
//
// The exit status is deliberately ignored, which is #161's sweeper
// finding applied here before it can bite: a batch inspect exits non-zero
// if any one argument has gone while still printing a good line for every
// id it did find, so reading the status as "nothing can be dated" makes
// one image removed by another worktree between the listing and this call
// silence the entire sweep. A sweeper that silently sweeps nothing is
// worse than no sweeper, because it looks like the leak is handled.
func bornOf(ids []string) map[string]time.Time {
	if len(ids) == 0 {
		return nil
	}
	out, _ := dockerRun(append([]string{
		"image", "inspect", "--format",
		"{{.Id}} {{index .Config.Labels \"" + bornLabelKey + "\"}}",
	}, ids...)...)

	born := make(map[string]time.Time, len(ids))
	for _, line := range strings.Split(out, "\n") {
		id, stamp, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		nanos, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil {
			// No usable stamp is not a licence to delete. An image that
			// cannot be dated is left alone.
			continue
		}
		born[id] = time.Unix(0, nanos)
	}
	return born
}

func dockerRun(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	return string(out), err
}

// TestSweepImagesReclaimsStaleImagesAndNothingElse is acceptance
// criterion 3's other half: TestMain covers the exits the test binary
// controls, and this covers the one it does not.
//
// Every assertion names an exact image this test built. Nothing here
// counts images, and nothing here asks the daemon a question about the
// machine as a whole, because several worktrees run this suite at once
// and a global count would be answering about all of them.
//
// The cutoff is the variable and the fixtures are all born now, rather
// than the other way round. A backdated fixture would be stale by the
// real rule too, so another worktree's sweep could remove it while this
// test was still running and "it is gone" would pass for a reason that
// has nothing to do with this code. The cutoff is a parameter nothing
// outside this test can move, and sweeping the same four images twice
// with two cutoffs is a cleaner statement of what the cutoff does than
// one sweep of images placed either side of it.
func TestSweepImagesReclaimsStaleImagesAndNothingElse(t *testing.T) {
	requireDocker(t)

	mine := "selftest-" + runID
	someoneElses := "selftest-other-" + runID

	first := buildMarkerImage(t, "first-run", mine, time.Now())
	second := buildMarkerImage(t, "second-run", mine, time.Now())
	otherValue := buildMarkerImage(t, "other-value-run", someoneElses, time.Now())
	// Carries no sweep label at all, and so must survive every sweep.
	unlabelled := buildLabelledImage(t,
		runLabelKey, "unlabelled-run",
		bornLabelKey, strconv.FormatInt(time.Now().Add(-time.Hour).UnixNano(), 10),
	)

	// Nothing here is older than the cutoff, so nothing should go. The
	// outcome is only recorded, not asserted: the assertion that gives it
	// meaning is the reap below, so that is the one that has to be read
	// first when this test fails.
	sweepImagesOlderThan(mine, time.Now().Add(-imageStaleAfter))
	firstSurvived, secondSurvived := imageExists(t, first), imageExists(t, second)

	// Everything in this run's own value is now older than the cutoff.
	sweepImagesOlderThan(mine, time.Now().Add(time.Hour))

	// The positive control, asserted first: without it, every "still
	// there" below would pass just as happily against a sweep that does
	// nothing at all, which is the failure mode #161 found in the
	// container sweeper.
	if imageExists(t, first) || imageExists(t, second) {
		t.Fatalf("sweepImagesOlderThan left %s and/or %s, both labelled %s=%s and both built before a cutoff an hour in the future, so it reclaims nothing and none of the assertions below prove anything", first, second, imageLabelKey, mine)
	}

	// Which makes this meaningful: the same two images, the same call,
	// an earlier cutoff, and they stayed.
	if !firstSurvived {
		t.Errorf("a sweep with a cutoff earlier than %s was born removed it anyway; a sweeper that ignores the cutoff deletes images out from under runs still using them", first)
	}
	if !secondSurvived {
		t.Errorf("a sweep with a cutoff earlier than %s was born removed it anyway; a sweeper that ignores the cutoff deletes images out from under runs still using them", second)
	}

	if !imageExists(t, otherValue) {
		t.Errorf("sweepImagesOlderThan removed %s, which is labelled %s=%s and not %s=%s; sweeping outside the value it was given is how one worktree's cleanup destroys another worktree's run", otherValue, imageLabelKey, someoneElses, imageLabelKey, mine)
	}
	if !imageExists(t, unlabelled) {
		t.Errorf("sweepImagesOlderThan removed %s, which carries no %s label at all and is an hour old; that key is the entire safety boundary between this suite's images and everything else on the daemon", unlabelled, imageLabelKey)
	}
}

// imageExists reports whether an image id is still present, by id, never
// by scanning a list.
func imageExists(t *testing.T, id string) bool {
	t.Helper()
	return exec.Command("docker", "image", "inspect", id).Run() == nil
}
