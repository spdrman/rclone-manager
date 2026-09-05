package machines

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The readiness probe, against servers whose answer is decided in advance.
//
// These three came from core/internal/transport/rclone/ssh_test.go with
// #448. They were always pure (a real SSH server in this process, no
// container at all), and they were always about a harness probe rather than
// about the rclone adapter, so this is where they belong. What they point
// at is waitForSSHAuth, which is the probe every machine this package
// starts has to satisfy before Start returns.

// throwawayClientKey generates an ed25519 client identity for a test. It
// lives only under the test's temp directory and authenticates only against
// a server this file started, so there is no real secret here.
func throwawayClientKey(t *testing.T) (privateKeyPath string, authorizedKeyLine string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a client key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshalling the client key: %v", err)
	}
	privateKeyPath = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(privateKeyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing the client key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("converting the public key: %v", err)
	}
	return privateKeyPath, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

// startInProcessSSHServer runs a real SSH server in this process, on a
// random loopback port, that authenticates exactly one public key and
// refuses every other. It exists so the probe can be pointed at a server
// whose answer is decided in advance: a container's is not.
//
// Pass a nil authorized key for a server that refuses everyone.
func startInProcessSSHServer(t *testing.T, authorized ssh.PublicKey) string {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("ssh.NewSignerFromKey: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, offered ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized != nil && bytes.Equal(offered.Marshal(), authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("public key rejected by the in-process test server")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			raw, acceptErr := ln.Accept()
			if acceptErr != nil {
				return // the listener was closed by cleanup
			}
			// No t.* calls in here: this outlives the test body, and a
			// failure reported after the test has finished panics the run
			// instead of failing the test.
			go func(c net.Conn) {
				sc, chans, reqs, hsErr := ssh.NewServerConn(c, cfg)
				if hsErr != nil {
					_ = c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "this fixture server offers no channels")
				}
				_ = sc.Close()
			}(raw)
		}
	}()

	return ln.Addr().String()
}

// clientPublicKey re-reads the authorized_keys line generateClientSSHKeyPair
// produced, as a parsed key the in-process server can compare against.
func clientPublicKey(t *testing.T, authorizedKeyLine string) ssh.PublicKey {
	t.Helper()
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKeyLine))
	if err != nil {
		t.Fatalf("parsing the generated authorized_keys line: %v", err)
	}
	return key
}

// TestReadinessProbeAcceptsAServerThatAuthenticates is the control on
// the two refusals below. Without it, a probe that answered "not ready" to
// everything, including a healthy fixture, would pass them both, and the
// only thing that would notice is the Docker suite timing out.
func TestReadinessProbeAcceptsAServerThatAuthenticates(t *testing.T) {
	clientKeyPath, authorizedKeyLine := throwawayClientKey(t)
	addr := startInProcessSSHServer(t, clientPublicKey(t, authorizedKeyLine))

	start := time.Now()
	if err := waitForSSHAuth(addr, clientConfigFor(t, clientKeyPath, User), sshReadyWindow); err != nil {
		t.Fatalf("the probe refused a server that authenticates this exact key, so every refusal it reports elsewhere is worthless: %v", err)
	}
	t.Logf("accepted in %s", time.Since(start))
}

// TestReadinessProbeRefusesAServerThatRejectsTheKey is the half of
// #250 that a handshake alone would miss. "Ready" has to mean the server
// will authenticate the caller's key, not merely that it speaks SSH, so
// this points the probe at a server that completes the transport handshake
// happily and then refuses the key.
func TestReadinessProbeRefusesAServerThatRejectsTheKey(t *testing.T) {
	clientKeyPath, _ := throwawayClientKey(t)
	addr := startInProcessSSHServer(t, nil) // authorizes nobody

	const window = 500 * time.Millisecond
	start := time.Now()
	err := waitForSSHAuth(addr, clientConfigFor(t, clientKeyPath, User), window)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the probe called a server ready that refuses this key outright")
	}
	// The error text is the positive control on the mechanism here. A probe
	// that never reached user authentication would still return SOME error
	// against this server if it were broken in an unrelated way, and would
	// satisfy "returned an error" while proving nothing about auth. Only a
	// probe that got through the key exchange and was then turned away at
	// authentication produces this.
	if !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("the probe failed, but not at authentication, so it says nothing about whether the probe checks auth at all: %v", err)
	}
	t.Logf("refused in %s: %v", elapsed, err)
}

// TestReadinessProbeRefusesASilentPeer used to be the third case here.
// It is source_test.go's TestSSHHandshakeIsBoundedAgainstASilentPeer,
// which is the same peer (a listener that completes the TCP handshake and
// then never sends a byte), the same elapsed-time control, and one level
// closer to the thing being bounded: it points at trySSHHandshake, which
// is where the deadline that fixes it actually lives.
