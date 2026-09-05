package machinegate_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// Issue #264's first acceptance criterion: the SFTP source path is proven
// against the local fixture, with no production host.
//
// The criterion is easy to read as already met, because every SFTP test in
// this package runs against a real sshd. What none of them SAY is the part
// this issue is actually about: that sshd is on a published port that is
// not 22, so the whole path has been running on a non-default port all
// along, and nothing anywhere records that as the thing being proven. A
// claim nothing asserts is a claim that stops being true quietly.
//
// So this file asserts it, and then asserts the two things that only
// matter on a non-default port:
//
//   - leave the port out and the source is not found by accident, which is
//     what "never defaults or infers one" means at the transport layer;
//   - the pin is keyed [host]:port, and a pin written without the port
//     fails, which is the field failure that made this issue's own
//     debugging expensive.
//
// Nothing here reaches a production host, needs a credential, or knows an
// address. It is a container on this machine.

// requireANonDefaultPort returns the source's port, having established
// that it is not 22.
//
// Under the in-network placement (scripts/e2e/run-machine-tier.sh, #451)
// nothing is published and the server is reached by its alias on 22, so
// there is no non-default port in play and this proof cannot be made. That
// is a skip rather than a pass, and it says what is not being proven,
// because a green tick for a run that never exercised the shape is worse
// than no test.
func requireANonDefaultPort(t *testing.T, src *machines.Source) int {
	t.Helper()
	if src.Port == 22 {
		t.Skipf("the source is reached on port 22 in this placement (%s is set, so the test process is a container on the network and nothing is published). Issue #264's proof is about a source on a NON-default port, and there is none here to run it against.", machines.NetworkEnv)
	}
	return src.Port
}

// TestTheSFTPSourcePathRunsOnANonDefaultPortAgainstTheFixture is the
// criterion itself: real bytes off a real sshd on a port that is not 22,
// with the port supplied as an input.
func TestTheSFTPSourcePathRunsOnANonDefaultPortAgainstTheFixture(t *testing.T) {
	src := machines.Start(t).Source(t)
	port := requireANonDefaultPort(t, src)

	payload := []byte("issue #264: a real artifact, pulled over a non-default port")
	writeUploadFile(t, src, "production.dump", payload)

	adapter := rclone.New()
	ctx := context.Background()
	source := src.TransportSource("sftp-nondefault-port", "")
	if source.Port != port {
		t.Fatalf("the transport source was built with port %d and the machine is on %d; this test would be proving nothing about the port", source.Port, port)
	}

	found, err := adapter.List(ctx, source)
	if err != nil {
		t.Fatalf("List against a source on a non-default port: %v\n%s", err, src.ConnectionTable(t))
	}
	if !listed(found, "production.dump") {
		t.Fatalf("List did not report the seeded artifact, got %v", found)
	}

	local := filepath.Join(t.TempDir(), "production.dump.partial")
	if _, err := adapter.CopyToLocal(ctx, source, "production.dump", local); err != nil {
		t.Fatalf("CopyToLocal over a non-default port: %v", err)
	}

	// The bytes, not the absence of an error. A copy that reported success
	// and wrote nothing is exactly the shape a transport test fails in.
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("reading what landed: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("what landed is not what was seeded: %d bytes vs %d", len(got), len(payload))
	}
	want := sha256.Sum256(payload)
	if got := sha256.Sum256(got); hex.EncodeToString(got[:]) != hex.EncodeToString(want[:]) {
		t.Fatal("the digest of what landed does not match the digest of what was seeded")
	}

	// One more operation over the same port, because List and a copy both
	// go through the same walk and a Stat does not. No RemoteHash here:
	// this fixture is the atmoz/sftp shape, forced internal-sftp with no
	// shell, so `sha256sum` has nothing to run in - which is issue #281's
	// finding and not this test's business.
	stat, err := adapter.Stat(ctx, source, "production.dump")
	if err != nil {
		t.Fatalf("Stat over a non-default port: %v", err)
	}
	if stat.Size != int64(len(payload)) {
		t.Fatalf("Stat reports %d bytes, the artifact is %d", stat.Size, len(payload))
	}
}

// TestLeavingThePortOutDoesNotFindTheSourceAnyway is "never defaults or
// infers one" as a measurement rather than a claim.
//
// transport.Source.Port is an int where zero means "whatever the backend
// does by default", which for sftp is 22. So this takes the source that
// just worked, removes ONLY the port, and requires it to fail. If it
// succeeded, something between this call and the server would be supplying
// a port nobody asked for, and every guarantee in this issue about the port
// being an input would be decoration.
func TestLeavingThePortOutDoesNotFindTheSourceAnyway(t *testing.T) {
	src := machines.Start(t).Source(t)
	requireANonDefaultPort(t, src)

	writeUploadFile(t, src, "production.dump", []byte("nobody should reach this without the port"))

	adapter := rclone.New()
	ctx := context.Background()

	// Positive control first. Without it, the refusal below is equally
	// consistent with the machine being broken, the key being wrong, or
	// the pin being stale, and it would still look like a pass.
	withPort := src.TransportSource("sftp-with-port", "")
	if _, err := adapter.List(ctx, withPort); err != nil {
		t.Fatalf("the control failed, so the refusal below would prove nothing: %v\n%s", err, src.ConnectionTable(t))
	}

	withoutPort := src.TransportSource("sftp-without-port", "")
	withoutPort.Port = 0
	if _, err := adapter.List(ctx, withoutPort); err == nil {
		t.Fatal("List reached the source with no port supplied. Something is filling one in, and the port stops being an input the moment anything can guess it.")
	}
}

// TestThePinIsKeyedToThePortAndAPinWithoutItIsRefused is the field failure
// this issue spent real time on, held as a test.
//
// An operator connects with `ssh`, sees the ed25519 fingerprint, pins the
// line it showed them, and rclone answers `knownhosts: key mismatch`. That
// is the man-in-the-middle alarm, and it is firing here for an entry that
// is merely missing its port. The alarm stops working the day operators
// learn it usually means "add something to the file".
//
// The installer refuses a deployment in that state at preflight now, and
// this is the proof that the state it refuses is really the broken one.
func TestThePinIsKeyedToThePortAndAPinWithoutItIsRefused(t *testing.T) {
	src := machines.Start(t).Source(t)
	port := requireANonDefaultPort(t, src)

	writeUploadFile(t, src, "production.dump", []byte("pinned"))

	recorded, err := os.ReadFile(src.KnownHostsFile)
	if err != nil {
		t.Fatalf("reading the machine's own pinned host keys: %v", err)
	}
	bracketed := "[" + src.Host + "]:" + strconv.Itoa(port)
	if !strings.Contains(string(recorded), bracketed) {
		t.Fatalf("the pin for a source on a non-default port is not keyed [host]:port, so the rest of this test is about a shape that does not exist")
	}

	adapter := rclone.New()
	ctx := context.Background()

	good := src.TransportSource("sftp-pinned-with-port", "")
	if _, err := adapter.List(ctx, good); err != nil {
		t.Fatalf("the control failed, so the refusal below would prove nothing: %v\n%s", err, src.ConnectionTable(t))
	}

	// The same key, the same host, the same everything, with the port
	// taken out of the pattern: exactly what an operator gets by copying
	// what `ssh` printed.
	stripped := strings.ReplaceAll(string(recorded), bracketed, src.Host)
	if stripped == string(recorded) {
		t.Fatal("nothing was stripped, so this is the same file as the control")
	}
	unkeyed := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(unkeyed, []byte(stripped), 0o600); err != nil {
		t.Fatalf("writing the unkeyed pin: %v", err)
	}

	bad := src.TransportSource("sftp-pinned-without-port", "")
	bad.KnownHosts = unkeyed
	if _, err := adapter.List(ctx, bad); err == nil {
		t.Fatal("a pin written without the port was accepted for a source that is not on 22. Either the pin is not being checked, or this test is no longer about the failure it was written for.")
	}
}

func listed(artifacts []transport.RemoteArtifact, name string) bool {
	for _, a := range artifacts {
		if a.Path == name || filepath.Base(a.Path) == name {
			return true
		}
	}
	return false
}
