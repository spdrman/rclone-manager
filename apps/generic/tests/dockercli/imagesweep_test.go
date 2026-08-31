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
// image, for one reason, and that reason is not an architectural
// boundary: no second image-building suite exists yet, so there is
// nothing to share it with.
//
// An earlier version of this comment said §7.1 put dockerlease out of
// reach. Checked against docs/EPIC-B-multi-nas.md:1201, it does not.
// §7.1 requires that core/ imports no code from apps/, that core/ and
// ui/shared/ import no provider SDK, and that each provider app can be
// removed without breaking core tests. An image sweeper sitting in
// core/tests/dockerlease violates none of those, and this file already
// imports dockerlease for imageStaleAfter, which is the same direction
// of travel that rule permits. The stale second reason has gone too:
// #161 is closed and #186 is merged, so dockerlease is no longer under
// active change.
//
// If a second image-building suite ever appears, lifting these functions
// into dockerlease is the obvious move. It is not a rename away, though:
// dockerlease's Sweep() takes no arguments and has no label selector, so
// the two vocabularies have to be reconciled first.
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
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
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
	// removed a moment later, so fifteen minutes clears it comfortably.
	//
	// An image is stamped when its build STARTS, not when it finishes:
	// buildImage evaluates the bornLabelKey argument while it is building
	// the exec.Command, before cmd.Run. So the usable margin is thirty
	// minutes minus the build, and a cold two-stage build of
	// container/Dockerfile (two Go binaries, an npm ci and a Vite build)
	// under this machine's load is not a small subtraction. Do not read
	// the thirty minutes as thirty minutes of test time.
	//
	// What actually bounds a live run's image age is not this derivation
	// at all. It is `go test`'s package timeout: ten minutes by default,
	// with no -timeout override in scripts/ci-local.sh, in any workflow,
	// or in concurrent-runs-check.sh. A run cannot keep an image alive
	// past its own test binary, so ten minutes against thirty is the real
	// safety factor. That is worth writing down because it lives outside
	// this file and would disappear silently the day someone raised
	// -timeout, and TestTheGoTestTimeoutKeepsALiveRunsImageInsideTheMargin
	// is what turns "silently" into a failing test.
	//
	// Reaping an image out from under a slow but healthy run would turn
	// this cleanup into a new source of exactly the cross-worktree
	// interference #185 is about, and the only cost of the wider margin
	// is that a killed run's image lingers a little longer.
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
	if !builder.built {
		return
	}
	_, _ = dockerRun("rmi", "-f", imageReference())
}

// sweepImagesOnce is a pointer so that a test can put a fresh one in
// front of the real sweep and drive it, without either consuming the
// production one (which would silently disable the sweep for the rest of
// the run) or copying a sync.Once, which go vet rightly refuses.
var sweepImagesOnce = new(sync.Once)

// sweepImages reclaims images this suite built more than imageStaleAfter
// ago, at most once per test binary however many times it is called.
// Call it before building.
func sweepImages() {
	sweepImagesOnce.Do(sweepStaleImages)
}

// sweepFn is what sweepImages calls, indirected through a variable
// because the two arguments the real sweep computes are the part no test
// could otherwise observe. The sweeper's own self-test has to substitute
// both of them for its fixtures to mean anything, and substituting them
// is exactly what leaves the production selector and the production
// cutoff sign unchecked. A test captures the call instead and asserts on
// what was asked for, deleting nothing.
var sweepFn = sweepAllRunsOlderThan

// sweepStaleImages is what the real sweep asks for: every run's images,
// and a cutoff imageStaleAfter in the PAST. Both halves are load-bearing
// and neither is obvious from the call site, which is why they are
// asserted in TestTheRealSweepAsksForEveryRunAndACutoffInThePast.
func sweepStaleImages() {
	sweepFn(time.Now().Add(-imageStaleAfter))
}

// anyLabelValue asks listLabelledImages for every image carrying
// imageLabelKey whatever its value, which is what a real sweep wants.
const anyLabelValue = ""

// sweepAllRunsOlderThan is the real sweep: every value of imageLabelKey,
// not just imageLabelValue. Anything this suite builds carries the key,
// including the fixtures its own tests build under a per-run value, and
// all of them are stamped with a real build time, so one rule ages them
// all correctly and nothing this suite creates is exempt from being
// reclaimed after a kill.
//
// The daemon-wide selector is a function of its own rather than a value
// sweepImagesOlderThan accepts, because as a parameter it was the zero
// value of its own type. A caller who passed an empty label by accident
// got the widest possible sweep instead of the narrowest, which inverts
// how every other guard in this file is built: an image without
// imageLabelKey is never touched, an image that cannot be dated is left
// alone, an unparseable stamp is skipped. The safe answer is the default
// everywhere else, and now here too. There is no longer any way to reach
// the daemon-wide sweep without typing "AllRuns".
//
// A cutoff that is not in the past is refused outright. The daemon-wide
// selector paired with a future cutoff force-removes every image this
// suite has on the machine, including the images of runs currently using
// them, and that pairing compiles and reads perfectly naturally. It is
// #161's exact shape and it cost this campaign real time. A refusal
// rather than a panic because this is cleanup and cleanup must be able
// to decline, but it is a loud refusal, because a silent one is
// indistinguishable from a sweeper that does not work.
func sweepAllRunsOlderThan(cutoff time.Time) {
	if !cutoff.Before(time.Now()) {
		warnSweep("refusing to sweep every run's images with a cutoff of %s, which is not in the past: that removes the images of runs still using them", cutoff.Format(time.RFC3339Nano))
		return
	}
	sweepLabelledImagesOlderThan(anyLabelValue, cutoff)
}

// sweepImagesOlderThan sweeps one value of imageLabelKey, and refuses
// anyLabelValue.
//
// The cutoff is a parameter for the reason dockerlease's is: a test has
// to be able to put an image on either side of it without waiting out
// imageStaleAfter. The label value is a parameter for a reason specific
// to this machine: several worktrees run this suite at once, so a test
// that swept every value would be sweeping other agents' images, and "the
// image I expected to survive is gone" would stop meaning anything. Its
// own test therefore sweeps a value only that run can write, while the
// real sweep goes through sweepAllRunsOlderThan.
func sweepImagesOlderThan(label string, cutoff time.Time) {
	if label == anyLabelValue {
		warnSweep("refusing a sweep of %s with an empty value, which would reach every worktree's images; call sweepAllRunsOlderThan if a daemon-wide sweep is really what was meant", imageLabelKey)
		return
	}
	sweepLabelledImagesOlderThan(label, cutoff)
}

// sweepLabelledImagesOlderThan is the body both entry points share, and
// the only one of the three that will remove anything.
func sweepLabelledImagesOlderThan(label string, cutoff time.Time) {
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

// sweepDiagnostics is where this file's refusals and failures are
// written. A variable so a test can read them back: a warning nothing
// can observe is the same as no warning.
var sweepDiagnostics io.Writer = os.Stderr

// warnSweep prints one line about something the sweep declined or could
// not do. Never fatal: a housekeeping step that can redden a suite is
// worse than the leak it exists to fix.
func warnSweep(format string, args ...any) {
	// The write itself is unchecked on purpose: the sink is os.Stderr in
	// production and a buffer in tests, and a sweeper that failed because
	// it could not print a warning would be worse than the warning going
	// missing.
	_, _ = fmt.Fprintf(sweepDiagnostics, "dockercli image sweep: "+format+"\n", args...)
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
		// "Could not list" is not "there is nothing to sweep". Any
		// failure here, dockerTimeout expiring under daemon load
		// included, used to be returned as an empty result, and the
		// sweep then finished having reclaimed nothing and said
		// nothing. That is precisely what this file's own header calls
		// worse than no sweeper at all, and the lesson was already
		// applied one function down in bornOf. It stays non-fatal, but
		// it no longer passes unremarked.
		warnSweep("could not list images with --filter %s: %v; nothing was reclaimed this run", filter, err)
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

// dockerRun is a variable so a test can point this file at a stub. Two
// of the paths that matter most cannot be produced on demand against a
// real daemon: a listing that fails, and a refusal that has to be shown
// to happen BEFORE any destructive command is issued.
var dockerRun = runDocker

func runDocker(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	return string(out), err
}

// timeoutFitsSweepMargin reports whether a `go test` package timeout
// keeps a live run's image comfortably younger than the age at which
// another worktree's sweep will force-remove it. See imageStaleAfter:
// this is the fact the margin actually rests on, so it is checked rather
// than written down and trusted.
func timeoutFitsSweepMargin(timeout, stale time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("the go test package timeout is disabled (%s), so this run can keep its image alive indefinitely while every other worktree treats an image older than %s as abandoned and force-removes it", timeout, stale)
	}
	if timeout >= stale {
		return fmt.Errorf("the go test package timeout is %s, which is not shorter than the %s sweep cutoff: a slow but healthy run can now outlive its own image and have it removed out from under it by another worktree, which is the cross-worktree interference #185 exists to remove", timeout, stale)
	}
	return nil
}

// stubDocker points dockerRun at fn for the duration of the calling
// test.
func stubDocker(t *testing.T, fn func(args ...string) (string, error)) {
	t.Helper()
	prev := dockerRun
	dockerRun = fn
	t.Cleanup(func() { dockerRun = prev })
}

// captureSweepDiagnostics collects everything warnSweep writes during
// the calling test.
func captureSweepDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := sweepDiagnostics
	sweepDiagnostics = &buf
	t.Cleanup(func() { sweepDiagnostics = prev })
	return &buf
}

// filterArgOf returns the value of the first --filter in a docker
// argument list.
func filterArgOf(args []string) string {
	for i, arg := range args {
		if arg == "--filter" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestTheRealSweepAsksForEveryRunAndACutoffInThePast covers the two
// arguments production computes and every other test in this file
// substitutes away.
//
// TestSweepImagesReclaimsStaleImagesAndNothingElse has to pass its own
// label value and its own cutoff, or its fixtures would be answering
// about forty other worktrees' images. The consequence is that the real
// selector and the real cutoff go unchecked: if the sign were inverted
// the sweeper would reap live runs and that test would still pass, and
// if the selector were wrong the sweeper would reclaim nothing forever
// and the whole suite would stay green while disk grew without bound.
// Capturing the call answers both without deleting anything.
func TestTheRealSweepAsksForEveryRunAndACutoffInThePast(t *testing.T) {
	// The production wiring itself: sweepImages must reach the
	// daemon-wide sweep, not the single-value one, or a killed run's
	// image is never anyone else's to reclaim.
	if got, want := reflect.ValueOf(sweepFn).Pointer(), reflect.ValueOf(sweepAllRunsOlderThan).Pointer(); got != want {
		t.Fatalf("sweepFn is not sweepAllRunsOlderThan; the real sweep only reclaims other runs' leftovers while it goes through the daemon-wide entry point")
	}

	var cutoffs []time.Time
	prevFn := sweepFn
	sweepFn = func(cutoff time.Time) { cutoffs = append(cutoffs, cutoff) }
	t.Cleanup(func() { sweepFn = prevFn })

	// A fresh Once in front of the real one, so this drives the whole
	// chain (sweepImages -> Once -> sweepStaleImages -> sweepFn) without
	// consuming the production Once and leaving the rest of this run
	// with no sweep at all.
	prevOnce := sweepImagesOnce
	sweepImagesOnce = new(sync.Once)
	t.Cleanup(func() { sweepImagesOnce = prevOnce })

	before := time.Now()
	sweepImages()
	sweepImages()
	after := time.Now()

	if len(cutoffs) != 1 {
		t.Fatalf("two calls to sweepImages produced %d sweeps, want exactly 1; the sweep runs on the way in to every build and must not be paid for more than once per test binary", len(cutoffs))
	}
	cutoff := cutoffs[0]

	// The sign. `time.Now().Add(imageStaleAfter)` is one character away
	// and would make every live run's image eligible immediately.
	if !cutoff.Before(before) {
		t.Fatalf("the real sweep asked for a cutoff of %s, which is not in the past (the call was made at %s): a cutoff in the future makes every image this suite has on the machine stale, including the ones runs are using right now", cutoff.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano))
	}

	// And the distance: exactly imageStaleAfter back from the moment of
	// the call, not some other duration that happens to be negative.
	if cutoff.Before(before.Add(-imageStaleAfter)) || cutoff.After(after.Add(-imageStaleAfter)) {
		t.Errorf("the real sweep asked for a cutoff of %s, want imageStaleAfter (%s) before the moment of the call, which was between %s and %s", cutoff.Format(time.RFC3339Nano), imageStaleAfter, before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}
}

// TestTheDaemonWideSweepRefusesTheArgumentsThatWouldReapLiveRuns covers
// the two refusals added for the empty-label hazard, and it covers them
// against a stub rather than the daemon on purpose: if a refusal were
// broken, running this test against the real daemon would force-remove
// every test image on the machine, which is the outcome the refusals
// exist to prevent. Asserting that no docker command was issued at all
// is a stronger statement than "the fixture survived" anyway.
//
// The same stub gives the production filter string for free, which is
// the anyLabelValue branch of listLabelledImages: the one branch the real
// sweep always takes and no other test ever reaches.
func TestTheDaemonWideSweepRefusesTheArgumentsThatWouldReapLiveRuns(t *testing.T) {
	log := captureSweepDiagnostics(t)

	var calls [][]string
	stubDocker(t, func(args ...string) (string, error) {
		calls = append(calls, args)
		return "", nil
	})

	sweepAllRunsOlderThan(time.Now().Add(time.Hour))
	if len(calls) != 0 {
		t.Errorf("sweepAllRunsOlderThan with a cutoff an hour in the future reached docker (%v); every image this suite has built on this machine is younger than that cutoff, so this call would have removed the images of runs still using them", calls)
	}
	if log.Len() == 0 {
		t.Errorf("sweepAllRunsOlderThan refused a future cutoff silently; a silent refusal is indistinguishable from a sweeper that does not work, which is the failure mode this file's header is written against")
	}

	log.Reset()
	calls = nil
	sweepImagesOlderThan(anyLabelValue, time.Now().Add(time.Hour))
	if len(calls) != 0 {
		t.Errorf("sweepImagesOlderThan(anyLabelValue, ...) reached docker (%v); an empty label is the zero value of that parameter and must select nothing, not everything", calls)
	}
	if log.Len() == 0 {
		t.Errorf("sweepImagesOlderThan refused an empty label value silently")
	}

	// The positive control. Without it, both assertions above would pass
	// against a sweeper that never issues a docker command at all, which
	// is a broken sweeper rather than a safe one.
	calls = nil
	sweepAllRunsOlderThan(time.Now().Add(-time.Hour))
	if len(calls) == 0 {
		t.Fatalf("sweepAllRunsOlderThan with a cutoff an hour in the past issued no docker command at all; the two refusals above prove nothing if this entry point never sweeps")
	}
	if got, want := filterArgOf(calls[0]), "label="+imageLabelKey; got != want {
		t.Errorf("the real sweep listed with --filter %q, want %q; with an =value suffix it only ever sees its own run's images and a killed run's leftovers are reclaimed by nobody", got, want)
	}
}

// TestListLabelledImagesSaysSoWhenItCannotList is the third of this
// file's fail-open shapes: `docker images` failing used to return an
// empty slice, which the sweep reads as "nothing to reclaim" and reports
// as success. On this machine a 30 second docker call timing out under
// load is not hypothetical.
func TestListLabelledImagesSaysSoWhenItCannotList(t *testing.T) {
	log := captureSweepDiagnostics(t)

	// The positive control first, so the diagnostic below is a failure
	// being reported rather than a line this function always prints.
	stubDocker(t, func(args ...string) (string, error) {
		return "sha256:aaa\nsha256:bbb\nsha256:aaa\n", nil
	})
	if got := listLabelledImages(imageLabelValue); len(got) != 2 {
		t.Fatalf("listLabelledImages of a successful listing = %v, want the two distinct ids", got)
	}
	if log.Len() != 0 {
		t.Fatalf("a successful listing wrote %q to the diagnostics; then the assertion below cannot tell a failure from ordinary noise", log.String())
	}

	stubDocker(t, func(args ...string) (string, error) {
		return "", errors.New("Cannot connect to the Docker daemon")
	})
	if got := listLabelledImages(imageLabelValue); got != nil {
		t.Errorf("listLabelledImages returned %v from a failed listing, want nil", got)
	}
	if !strings.Contains(log.String(), "Cannot connect to the Docker daemon") {
		t.Errorf("a failed listing produced no diagnostic (%q); the sweep then reclaims nothing and reports success, which this file's own header calls worse than no sweeper at all", log.String())
	}
}

// TestTheGoTestTimeoutKeepsALiveRunsImageInsideTheMargin pins the fact
// imageStaleAfter's safety actually rests on, which lives outside this
// file and outside this repository's own configuration: `go test`'s
// default ten minute package timeout. Raise it past imageStaleAfter and
// a healthy run can outlive its own image.
func TestTheGoTestTimeoutKeepsALiveRunsImageInsideTheMargin(t *testing.T) {
	f := flag.Lookup("test.timeout")
	if f == nil {
		t.Skip("no -test.timeout flag registered, so this binary is not being run by `go test`")
	}
	getter, ok := f.Value.(flag.Getter)
	if !ok {
		t.Skip("-test.timeout does not report its value, so the margin cannot be checked here")
	}
	timeout, ok := getter.Get().(time.Duration)
	if !ok {
		t.Skipf("-test.timeout reported %T, not a duration", getter.Get())
	}
	if err := timeoutFitsSweepMargin(timeout, imageStaleAfter); err != nil {
		t.Errorf("%v", err)
	}
}

// TestTimeoutFitsSweepMarginRejectsATimeoutThatOutlivesTheCutoff is the
// control for the test above: it fires on the values that would make the
// margin untrue, so a green run there means the timeout is short rather
// than the check being inert.
func TestTimeoutFitsSweepMarginRejectsATimeoutThatOutlivesTheCutoff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		wantErr bool
	}{
		{"go test's default", 10 * time.Minute, false},
		{"just inside the cutoff", imageStaleAfter - time.Second, false},
		{"exactly the cutoff", imageStaleAfter, true},
		{"past the cutoff", imageStaleAfter + time.Minute, true},
		{"disabled with -timeout 0", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := timeoutFitsSweepMargin(tc.timeout, imageStaleAfter)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("timeoutFitsSweepMargin(%s, %s) error = %v, want error: %v", tc.timeout, imageStaleAfter, err, tc.wantErr)
			}
		})
	}
}

// TestTheWideSelectorSeesEveryValueOfTheSweepLabel answers the same
// question as the stub above, but against the real daemon, and read-only.
// A superset assertion over images this test built itself touches no
// other worktree's images and needs no delete: if the daemon-wide filter
// matched nothing, the real sweep would reclaim nothing forever and
// nothing in this suite would notice.
func TestTheWideSelectorSeesEveryValueOfTheSweepLabel(t *testing.T) {
	requireDocker(t)

	mine := "selftest-wide-" + runID
	theirs := "selftest-wide-other-" + runID
	a := buildMarkerImage(t, "wide-a", mine, time.Now())
	b := buildMarkerImage(t, "wide-b", theirs, time.Now())

	wide := indexIDs(listLabelledImages(anyLabelValue))
	if !wide[a] {
		t.Errorf("a listing for every value of %s did not include %s, which carries %s=%s", imageLabelKey, a, imageLabelKey, mine)
	}
	if !wide[b] {
		t.Errorf("a listing for every value of %s did not include %s, which carries %s=%s; the real sweep uses exactly this listing, so a value it cannot see is a leak nothing reclaims", imageLabelKey, b, imageLabelKey, theirs)
	}

	// The control: the same call restricted to one value must not return
	// the other one. Without it, "the wide listing contains both" would
	// pass just as happily against a filter that ignores its argument.
	narrow := indexIDs(listLabelledImages(mine))
	if !narrow[a] {
		t.Errorf("a listing for %s=%s did not include %s, which carries exactly that label", imageLabelKey, mine, a)
	}
	if narrow[b] {
		t.Errorf("a listing for %s=%s included %s, which carries %s=%s; if the value half of the filter does nothing then the sweeper's own self-test has been sweeping every worktree's images all along", imageLabelKey, mine, b, imageLabelKey, theirs)
	}
}

func indexIDs(ids []string) map[string]bool {
	index := make(map[string]bool, len(ids))
	for _, id := range ids {
		index[id] = true
	}
	return index
}

// TestBornOfDatesTheImagesItFoundEvenWhenOneHasGone is #161's finding as
// a test rather than as a claim in a comment. Another worktree removing
// one image between the listing and the inspect is routine on this
// machine, and reading the batch inspect's exit status as "nothing can be
// dated" silences the entire sweep when it happens.
func TestBornOfDatesTheImagesItFoundEvenWhenOneHasGone(t *testing.T) {
	requireDocker(t)

	real := buildMarkerImage(t, "still-here", "selftest-gone-"+runID, time.Now())
	// Well-formed and certain not to exist: the shape of an id another
	// worktree removed a moment ago.
	gone := "sha256:" + strings.Repeat("0", 64)
	if imageExists(t, gone) {
		t.Fatalf("%s exists on this daemon, so it is not standing in for a removed image and this test proves nothing", gone)
	}

	born := bornOf([]string{real, gone})
	if _, ok := born[real]; !ok {
		t.Errorf("bornOf could not date %s when asked about it alongside one image that has gone; that turns the whole sweep into a silent no-op the moment another worktree removes anything, which is exactly the #161 bug this function is written against", real)
	}
	if _, ok := born[gone]; ok {
		t.Errorf("bornOf returned a birth time for %s, which does not exist", gone)
	}
}

// TestAnImageWithAnUnusableBornStampIsNeverSwept is bornOf's other
// safety refusal: no usable stamp is not a licence to delete. Dropping
// the ParseInt guard makes an image nobody can date deletable, and
// nothing in this suite noticed because every other fixture carries a
// valid numeric stamp.
func TestAnImageWithAnUnusableBornStampIsNeverSwept(t *testing.T) {
	requireDocker(t)

	mine := "selftest-undatable-" + runID
	undatable := buildLabelledImage(t,
		imageLabelKey, mine,
		runLabelKey, "undatable-run",
		bornLabelKey, "not-a-number",
	)
	datable := buildMarkerImage(t, "datable-run", mine, time.Now())

	if _, ok := bornOf([]string{undatable})[undatable]; ok {
		t.Errorf("bornOf dated %s, whose %s label is %q; a stamp that does not parse must leave the image undated rather than produce a time", undatable, bornLabelKey, "not-a-number")
	}

	// One sweep, one cutoff, two images in one label value: the datable
	// one must go and the undatable one must stay. The removal is the
	// positive control, without which "it survived" would pass against a
	// sweep that did nothing.
	sweepImagesOlderThan(mine, time.Now().Add(time.Hour))

	if imageExists(t, datable) {
		t.Fatalf("a sweep of %s=%s with a cutoff an hour in the future left %s in place, so it reclaimed nothing and the survival below proves nothing", imageLabelKey, mine, datable)
	}
	if !imageExists(t, undatable) {
		t.Errorf("the same sweep removed %s, whose %s label cannot be parsed; an image this suite cannot date might belong to a run that is still using it, so it must be left alone", undatable, bornLabelKey)
	}
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
