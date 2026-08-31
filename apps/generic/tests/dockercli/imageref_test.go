// This file is issue #185.
//
// `dockercli` used to build, retag and test one image reference that is
// the same string in every checkout on the machine:
// `backup-manager:dockercli-test`. An image tag belongs to the Docker
// daemon, not to the worktree that created it, and this machine carries
// around forty worktrees of this repository sharing one daemon. Two
// `dockercli` runs therefore raced over one name: whichever built last
// owned it, and the other run then tested an image built from a
// different commit while believing it was testing its own.
//
// That cost real diagnosis time twice (see the issue for both), and it
// is the same shape as the stale shared dev server on #172. The fix is
// not a lock around the suite, which would only hide the sharing and
// slow the gate down for everyone. It is to stop sharing the name: every
// run builds its own reference, carrying its own identity, and no run
// can name another run's image even by accident.
package dockercli_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// composeImageRepo is the repository half of the reference
	// container/compose.yaml hard-codes for both of its services
	// (`image: backup-manager:${VERSION:-dev}`). The uniqueness this file
	// introduces therefore has to live entirely in the tag half: compose
	// resolves the repository from the file itself and only the tag from
	// the environment, so a per-run repository would need compose.yaml
	// edited per run, while a per-run tag needs one VERSION value.
	// TestComposeImageResolvesToTheReferenceThisRunBuilt keeps that claim
	// honest against the real file.
	composeImageRepo = "backup-manager"

	// sharedTagBeforeThisFix is the exact reference every run used to
	// build and retag, kept here as a value the current one must never
	// equal again rather than as a sentence in a comment.
	sharedTagBeforeThisFix = composeImageRepo + ":dockercli-test"

	// imageLabelKey marks an image as this suite's to remove, and is the
	// whole safety boundary for sweepImages: an image without it is never
	// touched, exactly as dockerlease.LabelKey works for containers.
	imageLabelKey = "rclone-manager-test-image"
	// imageLabelValue is fixed for images built for real. The sweeper
	// takes the value as an argument so its own test can put its fixtures
	// in a private namespace that no other run's sweep can see, which is
	// the only way that test can assert "this exact image survived"
	// without another agent's concurrent run answering the question.
	imageLabelValue = "1"

	// runLabelKey carries the id of the run that built the image. This is
	// what makes "am I testing my own build?" a question the suite can
	// actually ask the daemon, rather than something it assumes because
	// it typed a tag a moment ago.
	runLabelKey = "rclone-manager-test-run"

	// bornLabelKey carries the build's own wall-clock stamp, in Unix
	// nanoseconds, for sweepImages to age images by.
	//
	// Not `docker image inspect`'s own `.Created`: an image with no
	// layers at all (`FROM scratch` plus nothing but labels, which is
	// what the sweeper's test fixtures are, because they cost nothing to
	// build) has no `Created` field in the inspect output whatsoever, and
	// a template reading it fails outright. A label this suite writes
	// itself is defined for every image it builds, needs no fallback, and
	// can be backdated in a test, which `.Created` cannot be.
	bornLabelKey = "rclone-manager-test-born"
)

// runID identifies this test process, and through it every image the
// process builds. It is computed once at package initialisation so that
// every helper in the package agrees on it.
var runID = newRunID()

// newRunID returns an identifier no other run will produce, in the
// character set a Docker tag allows.
//
// Three parts, each covering what the others cannot:
//   - the process id separates concurrent runs on one host, and is the
//     part a human reading `docker images` can trace back to a process;
//   - the start time, base 36, separates a later run that recycled an
//     earlier run's pid;
//   - eight random bytes separate two runs on different hosts that share
//     one daemon and happened to match on both of the above.
//
// crypto/rand.Read is documented to always fill its argument completely
// and never to return an error, so there is no failure path to handle
// here; the error is read anyway so that a future change to that
// contract cannot pass silently.
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand.Read: " + err.Error())
	}
	return strconv.Itoa(os.Getpid()) + "-" +
		strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
		hex.EncodeToString(b[:])
}

// imageReference is the image this run builds and tests. Every caller
// goes through this function rather than through a constant, so there is
// no longer a fixed string in the package for a second run to collide
// with.
func imageReference() string {
	return composeImageRepo + ":dockercli-" + runID
}

// composeVersion returns the VERSION value that makes
// container/compose.yaml's `image: backup-manager:${VERSION:-dev}`
// resolve to exactly ref, and refuses anything it cannot resolve.
//
// This is the coupling `startComposeStack` used to assert with
// `strings.HasSuffix(image, ":dockercli-test")`. That assertion could
// only ever check for one hard-coded tag, so it had to be removed along
// with the hard-coded tag; the coupling itself is real and stays
// checked, just against compose.yaml's actual repository name instead of
// against a tag literal.
func composeVersion(ref string) (string, error) {
	repo, tag, ok := strings.Cut(ref, ":")
	if !ok || tag == "" {
		return "", fmt.Errorf("image reference %q has no tag, so compose's %s:${VERSION:-dev} cannot be made to resolve to it", ref, composeImageRepo)
	}
	if repo != composeImageRepo {
		return "", fmt.Errorf("image reference %q is in repository %q, but container/compose.yaml resolves both of its services to %q; compose reads the repository from the file and only the tag from VERSION, so a reference outside that repository can never be reached with --no-build", ref, repo, composeImageRepo)
	}
	return tag, nil
}

// dockerTagPattern is Docker's own grammar for the tag half of a
// reference: an alphanumeric or underscore, then up to 127 more of
// alphanumeric, dot, underscore or dash.
var dockerTagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

// TestImageReferenceIsUniquePerRun is acceptance criterion 1, checked
// without needing Docker at all: the reference is derived per run, is
// never the shared tag again, and is a reference Docker will actually
// accept.
func TestImageReferenceIsUniquePerRun(t *testing.T) {
	ref := imageReference()

	if ref == sharedTagBeforeThisFix {
		t.Fatalf("imageReference() = %q, the globally shared tag every worktree on this machine used to build; two concurrent runs would own it in turn and each would test the other's build", ref)
	}

	repo, tag, ok := strings.Cut(ref, ":")
	if !ok {
		t.Fatalf("imageReference() = %q, which has no tag at all", ref)
	}
	if repo != composeImageRepo {
		t.Errorf("imageReference() repository = %q, want %q (container/compose.yaml hard-codes it)", repo, composeImageRepo)
	}
	if !strings.Contains(tag, runID) {
		t.Errorf("imageReference() tag = %q, want it to carry this run's id %q; without the run id in the reference itself, two runs can still land on one name", tag, runID)
	}
	if !dockerTagPattern.MatchString(tag) {
		t.Errorf("imageReference() tag = %q, which is not a tag Docker accepts (%s)", tag, dockerTagPattern)
	}

	// Uniqueness is the whole point, so check the generator rather than
	// trusting that one sample looks unusual. A generator seeded from
	// something constant (a fixed rand source, a truncated timestamp)
	// would repeat here immediately, and this is the assertion that
	// would catch it.
	const draws = 10000
	seen := make(map[string]int, draws)
	for i := range draws {
		id := newRunID()
		if first, dup := seen[id]; dup {
			t.Fatalf("newRunID() returned %q on both draw %d and draw %d; a generator that repeats within one process repeats across processes too, which is the collision this whole change removes", id, first, i)
		}
		seen[id] = i
	}
}

// TestComposeImageResolvesToTheReferenceThisRunBuilt is acceptance
// criterion 4. The per-run reference is only usable if `docker compose
// --no-build` can still be pointed at it, and that depends on two things
// this test reads out of the real files rather than restating: what
// container/compose.yaml's `image:` actually says, and what
// composeVersion hands compose as VERSION.
//
// Static on purpose. TestComposeStack_WebUIProxiesToTheEngineEndToEnd
// already brings the stack up for real, but it can only fail after a
// build and a stack start; this fails immediately, and names the file
// and the service that broke the resolution.
func TestComposeImageResolvesToTheReferenceThisRunBuilt(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "container", "compose.yaml"))
	if err != nil {
		t.Fatalf("ReadFile compose.yaml: %v", err)
	}

	var cf composeFile
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("yaml.Unmarshal compose.yaml: %v", err)
	}

	version, err := composeVersion(imageReference())
	if err != nil {
		t.Fatalf("composeVersion(%q): %v", imageReference(), err)
	}

	const template = composeImageRepo + ":${VERSION:-dev}"
	for _, service := range []string{"rclone-manager", "web-ui"} {
		svc, ok := cf.Services[service]
		if !ok {
			t.Fatalf("compose.yaml has no %q service", service)
		}
		if svc.Image != template {
			t.Errorf("services.%s.image = %q, want %q; startComposeStack points compose at this run's own image by setting VERSION alone, which only works while the repository half is exactly %q", service, svc.Image, template, composeImageRepo)
			continue
		}
		if got := strings.ReplaceAll(svc.Image, "${VERSION:-dev}", version); got != imageReference() {
			t.Errorf("services.%s.image resolves to %q with VERSION=%q, want this run's image %q", service, got, version, imageReference())
		}
	}
}

// TestTheImageUnderTestIsTheOneThisRunBuilt is the issue's Given/When/
// Then, asked of the daemon instead of assumed: the image sitting at
// this run's reference must carry this run's id.
//
// Under the shared tag this is precisely the question that had no good
// answer. Two runs both built `backup-manager:dockercli-test`, the
// second build moved the name, and the first run went on inspecting,
// running and compose-ing an image it had not built, with nothing
// anywhere able to notice.
//
// TestARaceOverOneTagIsWhatThePerRunReferenceRemoves is this test's
// positive control: it proves runLabelOf reports whichever image a name
// currently points at, including a foreign one, so the equality below is
// falsifiable rather than tautological.
func TestTheImageUnderTestIsTheOneThisRunBuilt(t *testing.T) {
	ref := buildImage(t)

	if ref != imageReference() {
		t.Fatalf("buildImage returned %q, want this run's own reference %q", ref, imageReference())
	}
	if got := runLabelOf(t, ref); got != runID {
		t.Fatalf("the image at %s was built by run %q, not by this one (%q): this run is testing another worktree's build, which is issue #185 exactly", ref, got, runID)
	}

	// TestMain's removal keys on this flag and nothing else observes it,
	// so if buildImage ever stopped setting it every run on this machine
	// would leak its image and the suite would stay green while doing it.
	if !builder.built {
		t.Errorf("buildImage returned %s without recording that it built anything; removeBuiltImage keys on that flag, so TestMain would leave this run's image on the daemon", ref)
	}

	// Logged, not merely checked, so that concurrent-runs-check.sh can
	// read back which image each run actually tested and show that two
	// runs from two worktrees tested two different ones.
	t.Logf("dockercli image under test: reference=%s id=%s run=%s", ref, imageIDOf(t, ref), runID)
}

// imageIDOf returns the full content id a reference currently resolves
// to. Two references can share one id (that is what a retag is), so this
// is what distinguishes "two names" from "two builds".
func imageIDOf(t *testing.T, ref string) string {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", ref).Output()
	if err != nil {
		t.Fatalf("docker image inspect %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

// TestARaceOverOneTagIsWhatThePerRunReferenceRemoves demonstrates the
// mechanism at the daemon level, and is the positive control for the
// test above.
//
// The stand-in for the old shared tag is itself scoped to this run. That
// is not timidity, it is the point: a control that reaches for a name
// other runs can also write is not a control, because another agent's
// concurrent run would be answering the question. Two images this test
// built itself, and one name only this run can write, reproduce the race
// completely and in isolation.
func TestARaceOverOneTagIsWhatThePerRunReferenceRemoves(t *testing.T) {
	requireDocker(t)

	alpha := buildMarkerImage(t, "worktree-alpha", imageLabelValue, time.Now())
	beta := buildMarkerImage(t, "worktree-beta", imageLabelValue, time.Now())

	// One name, standing in for `backup-manager:dockercli-test`.
	shared := "rclone-manager-dockercli-race-" + runID + ":shared"
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", shared).Run() })

	retag(t, alpha, shared)
	if got := runLabelOf(t, shared); got != "worktree-alpha" {
		t.Fatalf("after tagging alpha's image as %s, the name resolves to run %q, want %q; runLabelOf does not read the name's current target and nothing below proves anything", shared, got, "worktree-alpha")
	}

	// The second worktree builds. Nothing warned the first one.
	retag(t, beta, shared)
	if got := runLabelOf(t, shared); got != "worktree-beta" {
		t.Fatalf("after a second run tagged its own image as %s, the name still resolves to run %q; if a shared tag did not move like this there would be no issue #185 to fix", shared, got)
	}

	// The same two images under their own per-run references are exactly
	// what the fix relies on: the second build cannot reach the first
	// one's name, so it cannot move it.
	if got := runLabelOf(t, alpha); got != "worktree-alpha" {
		t.Errorf("alpha's own reference %s resolves to run %q, want %q", alpha, got, "worktree-alpha")
	}
	if got := runLabelOf(t, beta); got != "worktree-beta" {
		t.Errorf("beta's own reference %s resolves to run %q, want %q", beta, got, "worktree-beta")
	}
}

// runLabelOf reports which run built the image a reference currently
// resolves to, by asking the daemon rather than by parsing the reference.
func runLabelOf(t *testing.T, ref string) string {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", "--format",
		"{{index .Config.Labels \""+runLabelKey+"\"}}", ref).Output()
	if err != nil {
		var exit *exec.ExitError
		stderr := ""
		if errors.As(err, &exit) {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("docker image inspect %s: %v\n%s", ref, err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// retag points name at the image ref currently resolves to, the way the
// suite used to point `backup-manager:dockercli-test` at its own build.
func retag(t *testing.T, ref, name string) {
	t.Helper()
	if out, err := exec.Command("docker", "tag", ref, name).CombinedOutput(); err != nil {
		t.Fatalf("docker tag %s %s: %v\n%s", ref, name, err, out)
	}
}

// buildMarkerImage builds a layerless image standing in for some other
// worktree's build: it carries the run, sweep-namespace and birth labels
// a test wants to see and nothing else, and registers its own removal. It
// costs no network and essentially no time or disk, which is what makes
// it usable in tests that must not pay for a second real image.
//
// born is written as a label rather than left to the daemon because a
// layerless image has no creation timestamp at all in `docker image
// inspect` output, and because a sweeper test needs to place an image on
// both sides of a cutoff, which no Docker flag allows.
func buildMarkerImage(t *testing.T, run, label string, born time.Time) string {
	t.Helper()
	return buildLabelledImage(t,
		imageLabelKey, label,
		runLabelKey, run,
		bornLabelKey, strconv.FormatInt(born.UnixNano(), 10),
	)
}

// buildLabelledImage builds a layerless image carrying exactly the
// key/value label pairs given, and returns its full id. Taking the pairs
// rather than a fixed set is what lets a test build the one case that
// matters most to sweepImages: an image carrying no sweep label at all,
// which must survive every sweep.
func buildLabelledImage(t *testing.T, labels ...string) string {
	t.Helper()
	if len(labels)%2 != 0 {
		t.Fatalf("buildLabelledImage got %d label arguments, want key/value pairs", len(labels))
	}

	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Dockerfile: %v", err)
	}

	args := []string{"build", "-q", "-f", dockerfile}
	for i := 0; i < len(labels); i += 2 {
		args = append(args, "--label", labels[i]+"="+labels[i+1])
	}
	args = append(args, dir)

	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		var exit *exec.ExitError
		stderr := ""
		if errors.As(err, &exit) {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("docker build of a marker image labelled %v: %v\n%s", labels, err, stderr)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		t.Fatalf("docker build -q produced no image id for a marker image labelled %v", labels)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", id).Run() })
	return id
}
