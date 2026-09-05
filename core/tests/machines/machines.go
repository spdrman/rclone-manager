// Package machines is the one way a test in this repository gets a
// machine (issue #447): a source machine playing the VPS being backed up,
// optionally a storage medium, on a dedicated network created for the
// test and removed after it.
//
// It is the Go half of what scripts/e2e/two-machine-backup.sh does in
// shell: two machines on a temporary network, one of them the thing under
// test. The shell script's manager is docker-in-docker running the real
// installer; here the manager is the test process, and the address it
// reaches the source by is the one thing that differs between the two
// placements this package supports (see Placement below).
//
// # One call, one tier
//
//	m := machines.Start(t)
//	src := m.Source                 // a real sshd, chrooted, key-only
//	src.LimitConnections(t, 2)      // the production rule from #264
//	medium := m.Medium(t)           // a real S3 API, on the same network
//
// A file that calls Start is in the machine tier, and core/internal/testtier
// holds it to that: it has to live under core/tests, it runs under the
// gate's gotestwatch step, and it may not exec docker itself. Everything a
// fixture had to learn (bounded docker calls, the mid-test watchdog from
// #161, image presence before pull from #243, the labelled sweep from
// #150) comes with the call.
//
// # What this package is, right now
//
// A composition over core/tests/sftpfixture and core/tests/miniofixture,
// which gained network attachment for it, plus the network itself, the
// source image with iptables in it, and the failure shapes. That is
// deliberate: those two packages carry 1300 lines of behaviour that was
// paid for in incidents, and this was written on a machine whose Docker
// daemon was down, so nothing here could be run. Composing what exists is
// the change that can be reviewed statically; folding the fixtures into
// this package is #450, and it happens with a daemon.
//
// # Placement
//
// On Docker Desktop for macOS a host process cannot sit on a bridge
// network, so by default the test process reaches the source through a
// port published on 127.0.0.1, exactly as the fixtures always have. The
// dedicated network still exists and the source is on it, which is what
// lets the connection-cap probe run from a throwaway container on that
// network and what lets a medium reach the source by name.
//
// When NetworkEnv is set, the test process is itself a container on that
// network (#451's driver does this), nothing publishes a port, and the
// source is reached by its alias. Source.Addr answers correctly in both
// placements, so a test never has to know which one it is in.
//
// # What it exposes and what it never will
//
// Machines and failure shapes. Never assertions: a test composes what it
// needs from Source, Medium, LimitConnections and Kill, and the moment a
// method here starts asserting on behalf of a test this package has
// become the god-object the design was warned about.
package machines

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
	"github.com/spdrman/rclone-manager/core/tests/miniofixture"
	"github.com/spdrman/rclone-manager/core/tests/sftpfixture"
)

// NetworkEnv, when set, names the network this test process is already a
// container on. Start then attaches the machines to that network and
// publishes nothing. Only a driver that put the process there should set
// it.
const NetworkEnv = "RCLONE_MANAGER_MACHINES_NETWORK"

// sourceDockerfile is the source machine: the same image the SFTP fixture
// has always used, plus iptables so LimitConnections can impose the
// production rule without the container being privileged. It is the
// Dockerfile scripts/e2e/two-machine-backup.sh builds for its own source
// machine, restated here until #451 gives the two one home.
const sourceDockerfile = "FROM atmoz/sftp:alpine\nRUN apk add --no-cache iptables\n"

const (
	dockerInfoTimeout    = 60 * time.Second
	dockerBuildTimeout   = 5 * time.Minute
	dockerNetworkTimeout = 30 * time.Second
	dockerExecTimeout    = 30 * time.Second
	probeTimeout         = 60 * time.Second
)

var errDockerTimedOut = errors.New("docker did not answer in time")

// Machines is what Start hands back: the network and the source, and a
// medium on request.
type Machines struct {
	// Network is the docker network the machines share. Created by Start
	// unless NetworkEnv named one.
	Network string
	// Source is the VPS being backed up.
	Source *Source

	inNetwork bool
	mu        sync.Mutex
	medium    *Medium
}

// Source is the source machine. It embeds the SFTP fixture, so everything
// a test used to read off sftpfixture.Fixture (Host, Port, User, KeyFile,
// KnownHostsFile, BadKnownHostsFile, UploadDir, Source, Deny, Context,
// ContainerID, ExpectContainerDeath) is still there, unchanged.
type Source struct {
	*sftpfixture.Fixture
	// Alias is the name other containers on the network reach this
	// machine by.
	Alias string

	network   string
	inNetwork bool
	image     string
	capped    int
}

// Medium is a storage medium on the same network. It embeds the MinIO
// fixture, so Medium(), MediumForBucket and NewBucket are all still there.
type Medium struct {
	*miniofixture.Fixture
	Alias string
}

// infraMarker is the fixed, greppable string every infrastructure refusal
// in this package carries. It is the same literal in sftpfixture,
// miniofixture and tests/dockerlease on purpose: a gate log sorts into "the
// machine broke" and "the product broke" with one grep, and a marker that
// varied by package would not.
const infraMarker = "INFRA:"

// dockerUnavailable ends the calling test for a docker that is not there to
// be used, and decides whether that is a skip or a failure.
//
// On a machine that simply has no docker, skipping is honest: the machine
// tier is evidence for the gate, not a requirement on every developer's
// laptop. Inside the gate it is the opposite. Docker is a declared
// prerequisite there, so the same condition means the gate's own machine is
// broken, and a skip quietly deletes the machine tier from the run while
// the run goes on printing ok.
//
// That is not hypothetical (#456). Start used to fail a WEDGED daemon,
// three lines above a skip for an UNREACHABLE one, and a Docker VM that
// dies mid-run is unreachable rather than wedged. In one stored gate log it
// did exactly that: 13 of the 14 conformance mutation cells printed
// `ok ... 0.08s` against a dead daemon, and one cell happened to refuse,
// which is the only reason anybody noticed.
//
// So under the gate this is a failure carrying infraMarker. The one way
// past it is CI_LOCAL_SKIP_DOCKER=1, the gate's own documented opt-out for
// a run with the daemon down, which already ledgers that run as INCOMPLETE.
//
// This is deliberately a fourth copy rather than something exported from
// one of the three packages underneath. Making it shared would put a test
// harness's private verdict on another harness's public surface, and #450
// is folding all four into this one anyway, at which point three of the
// copies go away with the packages that hold them.
func dockerUnavailable(t *testing.T, reason string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(reason, args...)
	if gateRequiresDocker() {
		t.Fatalf("%s machines: %s\nDocker is a declared prerequisite of this gate (CI_LOCAL=1), so this is an INFRASTRUCTURE failure and not a product one: the machine could not offer a docker daemon. Skipping here would take the whole machine tier out of the run while the gate still printed ok, which is #456.", infraMarker, detail)
	}
	t.Skipf("machines: SKIPPING (missing capability: %s)", detail)
}

// capabilityUnavailable is dockerUnavailable for a capability that is not
// docker itself, and it makes the same decision for the same reason. It is
// a sibling rather than a rewrite of dockerUnavailable because that one is
// word for word the same in all five packages that have it, and a reader
// comparing them should find no differences to explain.
//
// It shares gateRequiresDocker, which is right rather than convenient:
// everything reached through this package needs a live daemon first, so a
// run that opted out of docker never gets here to be asked.
func capabilityUnavailable(t *testing.T, reason string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(reason, args...)
	if gateRequiresDocker() {
		t.Fatalf("%s machines: %s\nThe gate's own machine is expected to be able to do this (CI_LOCAL=1), so this is an INFRASTRUCTURE failure and not a product one. Skipping here would take the proof out of the run while the gate still printed ok, which is #456.", infraMarker, detail)
	}
	t.Skipf("machines: SKIPPING (missing capability: %s)", detail)
}

// gateRequiresDocker reports whether this process is inside the local gate,
// which declares docker a prerequisite. scripts/ci-local.sh exports
// CI_LOCAL=1. CI_LOCAL_SKIP_DOCKER=1 is that same gate's documented opt-out
// for a run with the daemon down, and it already ends the run INCOMPLETE,
// so it is honoured here rather than overruled: a fixture that refused
// anyway would make that flag a lie.
func gateRequiresDocker() bool {
	return os.Getenv("CI_LOCAL") == "1" && os.Getenv("CI_LOCAL_SKIP_DOCKER") != "1"
}

// Start creates the network, builds the source image if this daemon does
// not have it, starts the source machine on the network and returns once
// it accepts a real SSH session. Everything is removed when the test ends,
// including on a panic on another goroutine, because the fixture underneath
// already handles that.
//
// On a developer machine it SKIPS when docker is genuinely absent, since
// the machine tier is evidence for the gate rather than a requirement on
// every laptop. Inside the gate, where docker is a declared prerequisite,
// the same condition FAILS and says INFRA:, because a skip there deletes
// the machine tier from a run that goes on reporting ok (#160, #456). A
// wedged daemon fails either way. dockerUnavailable is where that verdict
// is made.
func Start(t *testing.T) *Machines {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		dockerUnavailable(t, "%q not found on PATH: %v", "docker", err)
	}
	if _, errOut, err := dockerRun(dockerInfoTimeout, "info"); err != nil {
		if errors.Is(err, errDockerTimedOut) {
			t.Fatalf("%s machines: `docker info` did not answer within %s. The daemon is there but wedged, and skipping would silently remove the machine tier from the gate, so this is a failure: %v\n%s", infraMarker, dockerInfoTimeout, err, errOut)
		}
		dockerUnavailable(t, "docker daemon not reachable: %v\n%s", err, errOut)
	}

	// Reclaim what a KILLED run left behind before adding to it: the
	// containers, and now the networks too.
	dockerlease.Sweep()
	dockerlease.SweepNetworks()

	network := os.Getenv(NetworkEnv)
	inNetwork := network != ""
	if !inNetwork {
		network = createNetwork(t)
	}

	image := ensureSourceImage(t)
	alias := "source-" + shortID(t)

	fx := sftpfixture.StartWith(t, sftpfixture.Options{
		Network:   network,
		Alias:     alias,
		InNetwork: inNetwork,
		Image:     image,
		// NET_ADMIN is what an iptables rule inside the container needs,
		// and it is a capability rather than --privileged, which is the
		// same choice the shell script made for its source machine.
		RunArgs: []string{"--cap-add", "NET_ADMIN"},
	})

	return &Machines{
		Network: network,
		Source: &Source{
			Fixture:   fx,
			Alias:     alias,
			network:   network,
			inNetwork: inNetwork,
			image:     image,
		},
		inNetwork: inNetwork,
	}
}

// Medium starts a storage medium on the network the first time it is
// called and returns the same one after that. A test that never calls it
// never pays for MinIO.
func (m *Machines) Medium(t *testing.T) *Medium {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.medium != nil {
		return m.medium
	}
	alias := "medium-" + shortID(t)
	fx := miniofixture.StartWith(t, miniofixture.Options{
		Network:   m.Network,
		Alias:     alias,
		InNetwork: m.inNetwork,
	})
	m.medium = &Medium{Fixture: fx, Alias: alias}
	return m.medium
}

// Addr is host:port as the manager (this test process) reaches the source.
// Published loopback port on the host, alias and 22 inside the network.
func (s *Source) Addr() string {
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// LimitConnections installs the production rule behind #264 on the source
// machine: a REJECT with a TCP reset for any SYN to port 22 that would take
// one address above n simultaneous established connections. Then it proves
// the rule bites, from a throwaway container on the same network, before
// letting the test trust it: a rule that installs and does not bite would
// turn the test into a copy of the uncapped case, green and proving
// nothing.
//
// The test process's own connections are subject to the same cap. On the
// host placement they arrive through Docker Desktop's port proxy, which
// presents one source address for all of them, so the cap counts them
// together exactly as a real source counts one manager.
//
// A kernel that will not install a connlimit rule inside a container is a
// capability the machine running this does not have. On a laptop that is a
// skip that names itself, the same verdict the shell script gives with
// CANNOT RUN. Inside the gate it is a refusal, for #456's reason: the gate
// machine is expected to have it, and skipping there would take #264's
// connection-cap proof out of a run that went on printing ok. Measured on
// the Docker Desktop VM this gate runs against, the rule installs and it
// bites, so the capability is there to be required.
func (s *Source) LimitConnections(t *testing.T, n int) {
	t.Helper()
	if n < 1 {
		t.Fatalf("machines: LimitConnections(%d): a cap below one is a block, not a cap", n)
	}
	_, errOut, err := dockerRun(dockerExecTimeout, "exec", s.ContainerID(),
		"iptables", "-A", "INPUT", "!", "-i", "lo", "-p", "tcp", "--syn", "--dport", "22",
		"-m", "connlimit", "--connlimit-above", strconv.Itoa(n), "--connlimit-mask", "32",
		"-j", "REJECT", "--reject-with", "tcp-reset")
	if err != nil {
		capabilityUnavailable(t, "this kernel will not install an iptables connlimit rule inside the source container: %v\n%s\nWithout the rule the connection-cap shape (#264) is untested here, and running the test anyway would make it a copy of the uncapped case.", err, errOut)
	}
	s.capped = n

	// Proven, not assumed. Hold n connections open and check the next one
	// is refused, from inside the network so no proxy is in the way. The
	// held connections are the positive control: if they were refused
	// too, the rule would be a block and this would say so.
	script := fmt.Sprintf(`
set -e
for i in $(seq 1 %d); do (sleep 20 | nc %s 22 >/dev/null 2>&1) & done
sleep 3
if nc -w 3 %s 22 </dev/null 2>/dev/null | grep -q '^SSH-'; then
  echo "connection %d was accepted"; exit 1
fi
exit 0
`, n, s.Alias, s.Alias, n+1)
	out, errOut, err := dockerRun(probeTimeout, "run", "--rm", "--network", s.network,
		dockerlease.LabelFlag, dockerlease.LabelSpec, s.image, "sh", "-c", script)
	if err != nil {
		t.Fatalf("machines: the connection cap did not bite: a connection past %d to %s was accepted, or the probe could not run: %v\n%s%s\nThis test is only worth running if the cap is real.", n, s.Alias, err, out, errOut)
	}
}

// Kill removes the source machine out from under the test, on purpose,
// and tells the fixture's watchdog so the death is evidence rather than a
// failure. Context() is cancelled with a ContainerDiedError, which is what
// a test proving the fail-fast path (#161) waits for.
func (s *Source) Kill(t *testing.T) {
	t.Helper()
	s.ExpectContainerDeath()
	if _, errOut, err := dockerRun(dockerExecTimeout, "rm", "-f", s.ContainerID()); err != nil {
		t.Fatalf("machines: could not remove the source container %s: %v\n%s", s.ContainerID(), err, errOut)
	}
}

// --- the network ----------------------------------------------------------

func createNetwork(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("rclone-manager-machines-%d-%d", os.Getpid(), time.Now().UnixNano())
	// Registered before the network exists and before any machine joins
	// it, so t.Cleanup's LIFO order removes the machines first. A network
	// with an endpoint on it cannot be removed, and "network is in use" at
	// teardown is how a network survives a run.
	t.Cleanup(func() {
		_, _, _ = dockerRun(dockerNetworkTimeout, "network", "rm", name)
	})
	if _, errOut, err := dockerRun(dockerNetworkTimeout, "network", "create",
		dockerlease.LabelFlag, dockerlease.LabelSpec, name); err != nil {
		t.Fatalf("machines: could not create the network %s: %v\n%s", name, err, errOut)
	}
	return name
}

// --- the source image -----------------------------------------------------

var (
	imageOnce sync.Once
	imageRef  string
	imageErr  error
)

// ensureSourceImage builds the source image once per daemon. The tag
// carries a digest of the Dockerfile, so a daemon that already has this
// exact image builds nothing, and a changed Dockerfile is a new tag rather
// than a stale one: the same discipline as sftpfixture.ensureImage (#243),
// and, like it, a build that cannot happen is a failure and not a skip.
func ensureSourceImage(t *testing.T) string {
	t.Helper()
	imageOnce.Do(func() {
		sum := sha256.Sum256([]byte(sourceDockerfile))
		tag := "rclone-manager-machines-source:" + hex.EncodeToString(sum[:6])
		if _, _, err := dockerRun(dockerExecTimeout, "image", "inspect", tag); err == nil {
			imageRef = tag
			return
		}
		_, errOut, err := dockerRunStdin(dockerBuildTimeout, sourceDockerfile, "build", "-q", "-t", tag, "-")
		if err != nil {
			imageErr = fmt.Errorf("building the source machine image %s: %w\n%s", tag, err, errOut)
			return
		}
		imageRef = tag
	})
	if imageErr != nil {
		t.Fatalf("machines: %v\nThat is a FAILURE and deliberately not a skip: skipping would take the whole machine tier out of the gate while the gate went on reporting ok (#160).", imageErr)
	}
	return imageRef
}

// --- docker ---------------------------------------------------------------

// dockerRun runs one docker command under a hard timeout. A test helper
// must never be the reason a suite hangs (#161).
func dockerRun(timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	return dockerRunStdin(timeout, "", args...)
}

func dockerRunStdin(timeout time.Duration, stdin string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = strings.TrimSpace(outBuf.String())
	stderr = strings.TrimSpace(errBuf.String())
	switch {
	case ctx.Err() != nil:
		return stdout, stderr, fmt.Errorf("%w: `docker %s` was still running after %s", errDockerTimedOut, args[0], timeout)
	case runErr != nil:
		return stdout, stderr, fmt.Errorf("%w: %s", runErr, stderr)
	}
	return stdout, stderr, nil
}

func shortID(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("machines: generating an id: %v", err)
	}
	return hex.EncodeToString(b[:])
}
