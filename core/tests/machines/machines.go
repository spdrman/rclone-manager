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
//	src := m.Source(t)              // a real sshd, chrooted, key-only
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
// # Nothing starts that a test did not ask for
//
// Start creates the network and nothing else. Source and Medium each stand
// their machine up the first time they are called and hand back the same
// one after that, so a test that only needs an S3 API never pays for an
// sshd and a test that only needs an sshd never pays for MinIO. That is not
// a micro-optimisation: the MinIO suite runs eight tests, and an eagerly
// started source would have added an ssh-keygen rsa 2048 and a container
// start to every one of them.
//
// # What this package is
//
// The whole of it, since #450. core/tests/sftpfixture and
// core/tests/miniofixture used to hold the source and the medium and were
// composed from here; their bodies are now source.go and medium.go, and the
// three copies of the docker plumbing, the INFRA: verdict and the #456
// refusal tests collapsed into one on the way in.
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
// network (scripts/e2e/run-machine-tier.sh does this, #451), nothing
// publishes a port, and the source is reached by its alias. Source.Addr
// answers correctly in both placements, so a test never has to know which
// one it is in.
//
// # What it exposes and what it never will
//
// Machines and failure shapes. Never assertions: a test composes what it
// needs from Source, Medium, LimitConnections and Kill, and the moment a
// method here starts asserting on behalf of a test this package has
// become the god-object the design was warned about.
package machines

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
)

// NetworkEnv, when set, names the network this test process is already a
// container on. Start then attaches the machines to that network and
// publishes nothing. Only a driver that put the process there should set
// it.
const NetworkEnv = "RCLONE_MANAGER_MACHINES_NETWORK"

// SourceDockerfile is the source machine: the same image the SFTP fixture
// has always used, plus iptables so LimitConnections can impose the
// production rule without the container being privileged, and netcat so the
// cap can be probed from a second machine on the network.
//
// It is exported because scripts/e2e/two-machine-backup.sh builds the same
// machine and #451 asked for one definition of "the simulated VPS" rather
// than two that drift. scripts/e2e/source-machine.Dockerfile is that one
// definition on disk; sourceDockerfileMustMatchTheScript in machines_test.go
// is what stops this constant and that file parting company.
const SourceDockerfile = "FROM atmoz/sftp:alpine\nRUN apk add --no-cache iptables netcat-openbsd\n"

const (
	dockerBuildTimeout   = 5 * time.Minute
	dockerNetworkTimeout = 30 * time.Second
	dockerExecTimeout    = 30 * time.Second
)

// Machines is what Start hands back: the network, and the machines on it
// once something asks for them.
type Machines struct {
	// Network is the docker network the machines share. Created by Start
	// unless NetworkEnv named one.
	Network string

	inNetwork bool
	mu        sync.Mutex
	source    *Source
	medium    *Medium
}

// infraMarker is the fixed, greppable string every infrastructure refusal
// in this package carries. It is the same literal in tests/dockerlease and
// in distribution/tests/adapterstacks on purpose: a gate log sorts into
// "the machine broke" and "the product broke" with one grep, and a marker
// that varied by package would not.
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
// Until #450 this was four copies, one per fixture, kept word for word the
// same so a reader comparing them found no differences to explain. It is
// one copy now, because the packages that held the other three are gone.
func dockerUnavailable(t *testing.T, reason string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(reason, args...)
	if gateRequiresDocker() {
		t.Fatalf("%s machines: %s\nDocker is a declared prerequisite of this gate (CI_LOCAL=1), so this is an INFRASTRUCTURE failure and not a product one: the machine could not offer a docker daemon. Skipping here would take the whole machine tier out of the run while the gate still printed ok, which is #456.", infraMarker, detail)
	}
	t.Skipf("machines: SKIPPING (missing capability: %s)", detail)
}

// capabilityUnavailable is dockerUnavailable for a capability that is not
// docker itself, and it makes the same decision for the same reason.
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

// Start probes the daemon, reclaims what a killed run left behind and
// creates the dedicated network. It starts no machine: Source and Medium do
// that, on demand, so a test pays only for what it asks for.
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

	// ssh-keygen and ssh-keyscan are as much a prerequisite as docker: the
	// source machine's key material is generated with them, and a machine
	// missing them cannot stand one up. Asking here rather than inside
	// Source keeps the verdict in one place.
	for _, tool := range []string{"docker", "ssh-keygen", "ssh-keyscan"} {
		if _, err := exec.LookPath(tool); err != nil {
			dockerUnavailable(t, "%q not found on PATH: %v", tool, err)
		}
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

	return &Machines{Network: network, inNetwork: inNetwork}
}

// Source starts the machine being backed up the first time it is called and
// returns the same one after that.
func (m *Machines) Source(t *testing.T) *Source {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.source != nil {
		return m.source
	}
	src := m.addSource(t)
	m.source = src
	return src
}

// AnotherSource starts an additional, independent source machine on the
// same network, with its own host key and its own client key.
//
// It is what the host-key cases need and the reason they no longer build an
// image per test function: "this server's key is not the one known_hosts
// pinned" is a statement about two machines, and the honest way to make it
// true is to have two.
func (m *Machines) AnotherSource(t *testing.T) *Source {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addSource(t)
}

// addSource stands up one source machine. The caller holds m.mu.
func (m *Machines) addSource(t *testing.T) *Source {
	t.Helper()
	alias := "source-" + shortID(t)
	return startSource(t, sourceOptions{
		Network:   m.Network,
		Alias:     alias,
		InNetwork: m.inNetwork,
		// NET_ADMIN is what an iptables rule inside the container needs,
		// and it is a capability rather than --privileged, which is the
		// same choice the shell script made for its source machine.
		RunArgs: []string{"--cap-add", "NET_ADMIN"},
	})
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
	med := startMedium(t, mediumOptions{
		Network:   m.Network,
		Alias:     alias,
		InNetwork: m.inNetwork,
	})
	med.Alias = alias
	m.medium = med
	return med
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

func shortID(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("machines: generating an id: %v", err)
	}
	return hex.EncodeToString(b[:])
}
