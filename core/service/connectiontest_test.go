// What `Test Connection` actually did (EPIC G, issue #596).
//
// Every case here runs against a REAL SSH server in this process, on a
// real loopback port, with a real key file and a real known_hosts file on
// disk. That is deliberate and it is the difference between this file and
// a file that would have passed against the bug: the six steps are
// decided at six different layers, and a fake far side answers all six
// from one place.
package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/sourcecheck"
)

// sshFixture is one real SSH server plus the client key it accepts and
// the known_hosts line that trusts it.
type sshFixture struct {
	host            string
	port            int
	keyFile         string
	knownHostsFile  string
	hostFingerprint string
	// passphrase is what keyFile is encrypted with, empty when it is a
	// plain PEM. A fixture that can only produce unencrypted keys cannot
	// exercise the passphrase half of the credentials step at all, and
	// that half is a real deployment shape (#269).
	passphrase string
}

// startSSHFixture runs a real SSH server on loopback that accepts exactly
// one generated key, writes that key to a file the way an operator's
// installation holds one, and writes a known_hosts file trusting the
// server's own host key.
//
// trustWrongKey writes a known_hosts line for a DIFFERENT host key, which
// is what a re-keyed or rebuilt machine looks like from here without
// anything outside this process being touched.
func startSSHFixture(t *testing.T, acceptTheKey, trustWrongKey bool, passphrase ...string) sshFixture {
	t.Helper()
	dir := t.TempDir()
	keyPassphrase := ""
	if len(passphrase) > 0 {
		keyPassphrase = passphrase[0]
	}

	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating the client key: %v", err)
	}
	var block *pem.Block
	if keyPassphrase == "" {
		der, err := x509.MarshalPKCS8PrivateKey(clientPriv)
		if err != nil {
			t.Fatalf("marshalling the client key: %v", err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(clientPriv, "connection-test fixture", []byte(keyPassphrase))
		if err != nil {
			t.Fatalf("encrypting the client key: %v", err)
		}
	}
	keyFile := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing the client key: %v", err)
	}
	clientSSHPub, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating the host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("ssh.NewSignerFromKey: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, offered ssh.PublicKey) (*ssh.Permissions, error) {
			if acceptTheKey && bytes.Equal(offered.Marshal(), clientSSHPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("public key rejected by the in-process test server")
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
				return
			}
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

	addr := ln.Addr().(*net.TCPAddr)
	trusted := hostSigner.PublicKey()
	if trustWrongKey {
		_, other, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generating the stale trusted key: %v", err)
		}
		otherSigner, err := ssh.NewSignerFromKey(other)
		if err != nil {
			t.Fatalf("ssh.NewSignerFromKey (stale): %v", err)
		}
		trusted = otherSigner.PublicKey()
	}
	knownHostsFile := filepath.Join(dir, "known_hosts")
	line := knownhosts.Line([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))}, trusted)
	if err := os.WriteFile(knownHostsFile, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("writing known_hosts: %v", err)
	}

	return sshFixture{
		host:            "127.0.0.1",
		port:            addr.Port,
		keyFile:         keyFile,
		knownHostsFile:  knownHostsFile,
		hostFingerprint: ssh.FingerprintSHA256(hostSigner.PublicKey()),
		passphrase:      keyPassphrase,
	}
}

// sshBackedService is a service whose one backup set points at fx.
func sshBackedService(t *testing.T, fx sshFixture) *BackupService {
	t.Helper()
	// A recording transport rather than nil: the list step goes through
	// the SAME transport.Transport.List a real cycle's discovery calls,
	// so a fixture with no transport would be testing a path no backup
	// takes. connectionceiling_test.go's fake is reused rather than a
	// second one being written.
	svc := New(testConfig(config.Source{
		Name: "api-server",
		BackupSets: []config.BackupSet{{
			Name:       "var-backups",
			ID:         mustBackupSetID(t, "api-server", "var-backups"),
			LocalPath:  t.TempDir(),
			RemotePath: "/var/backups",
			Remote: config.Remote{
				Type:       "sftp",
				Host:       fx.host,
				Port:       fx.port,
				User:       "backups",
				KnownHosts: fx.knownHostsFile,
				Key:        config.Key{File: fx.keyFile},
			},
		}},
	}), openTestJournal(t), &sourceRecordingTransport{}, obs.New(io.Discard, obs.LevelInfo))
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func stepOutcomes(checks []ConnectionCheck) map[string]string {
	out := map[string]string{}
	for _, c := range checks {
		out[c.Step] = c.Outcome
	}
	return out
}

// TestTestBackupSetConnection_ReportsSixNamedSteps is the acceptance
// criterion, and it is written so it cannot pass against 0.3.2's
// behaviour. A verdict is what exists today; a test asserting one would
// have passed against the bug. This asserts the individual step results.
func TestTestBackupSetConnection_ReportsSixNamedSteps(t *testing.T) {
	fx := startSSHFixture(t, true, false)
	svc := sshBackedService(t, fx)

	got, err := svc.TestBackupSetConnection(context.Background(), "api-server/var-backups")
	if err != nil {
		t.Fatalf("TestBackupSetConnection: %v", err)
	}
	if len(got.Checks) != len(sourcecheck.Steps) {
		t.Fatalf("got %d checks, want one per step (%d). A verdict is what shipped in 0.3.2; the steps are the feature: %+v", len(got.Checks), len(sourcecheck.Steps), got.Checks)
	}
	for i, want := range sourcecheck.Steps {
		if got.Checks[i].Step != string(want) {
			t.Errorf("check %d is %q, want %q: the order of Steps is the order a surface renders", i, got.Checks[i].Step, want)
		}
	}

	outcomes := stepOutcomes(got.Checks)
	for _, step := range []sourcecheck.Step{
		sourcecheck.StepCredentials, sourcecheck.StepResolve, sourcecheck.StepConnect,
		sourcecheck.StepHostKey, sourcecheck.StepAuthenticate,
	} {
		if outcomes[string(step)] != string(sourcecheck.Passed) {
			t.Errorf("%s: outcome %q, want %q against a real server that accepts this key", step, outcomes[string(step)], sourcecheck.Passed)
		}
	}

	// host_key prints the fingerprint in ssh-keygen -lf form, which is
	// the acceptance criterion that makes a mismatch settleable.
	for _, c := range got.Checks {
		if c.Step != string(sourcecheck.StepHostKey) {
			continue
		}
		if !strings.Contains(c.Detail, fx.hostFingerprint) {
			t.Errorf("host_key detail does not print the fingerprint the server actually offered (%s): %q", fx.hostFingerprint, c.Detail)
		}
	}
}

// TestTestBackupSetConnection_PutsEveryStepOnThatSetsFeed is the other
// half of the issue: the steps have to reach the TERMINAL, which means
// the live activity ring, attributed to this set and nobody else.
func TestTestBackupSetConnection_PutsEveryStepOnThatSetsFeed(t *testing.T) {
	fx := startSSHFixture(t, true, false)
	svc := sshBackedService(t, fx)

	if _, err := svc.TestBackupSetConnection(context.Background(), "api-server/var-backups"); err != nil {
		t.Fatalf("TestBackupSetConnection: %v", err)
	}

	feed, err := svc.LiveActivity(context.Background(), LiveActivityRequest{BackupSetID: "api-server/var-backups", Limit: 200})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if len(feed.Sets) != 1 {
		t.Fatalf("the reading carries %d sets, want exactly the one asked for", len(feed.Sets))
	}

	seen := map[string]string{}
	for _, e := range feed.Sets[0].Events {
		if e.Event != connectionTestEventName {
			continue
		}
		var step, outcome string
		for _, f := range e.Fields {
			switch f.Key {
			case "step":
				step = f.Value
			case "outcome":
				outcome = f.Value
			}
		}
		if step != "" {
			seen[step] = outcome
		}
	}
	if len(seen) != len(sourcecheck.Steps) {
		t.Fatalf("the set's own feed carries %d connection-test steps, want %d. Pressing the button has to leave the six steps in the terminal, not one verdict: %v", len(seen), len(sourcecheck.Steps), seen)
	}
	for _, step := range sourcecheck.Steps {
		if seen[string(step)] == "" {
			t.Errorf("no %s line reached the feed", step)
		}
	}
}

// TestTestBackupSetConnection_HostKeyMismatchNamesTheStep is the failure
// an operator most needs told apart from the other five, against a real
// server offering a key this set does not trust.
func TestTestBackupSetConnection_HostKeyMismatchNamesTheStep(t *testing.T) {
	fx := startSSHFixture(t, true, true)
	svc := sshBackedService(t, fx)

	got, err := svc.TestBackupSetConnection(context.Background(), "api-server/var-backups")
	if err != nil {
		t.Fatalf("TestBackupSetConnection: %v", err)
	}
	if got.OK {
		t.Fatal("OK = true against a server offering an untrusted host key")
	}
	outcomes := stepOutcomes(got.Checks)
	if outcomes[string(sourcecheck.StepHostKey)] != string(sourcecheck.Failed) {
		t.Fatalf("host_key: outcome %q, want %q", outcomes[string(sourcecheck.StepHostKey)], sourcecheck.Failed)
	}
	for _, step := range []sourcecheck.Step{sourcecheck.StepAuthenticate, sourcecheck.StepList} {
		if outcomes[string(step)] != string(sourcecheck.Skipped) {
			t.Errorf("%s: outcome %q, want %q. Drawing an unattempted authentication as anything but \"this was never tried\" tells an operator their credentials are fine on the strength of a step that never ran", step, outcomes[string(step)], sourcecheck.Skipped)
		}
	}

	// The message stays what it always was for a client reading only
	// `ok` and `message`, so an older one keeps working.
	if got.Message == "" {
		t.Error("Message is empty on a failure, so a client reading only ok/message has nothing to render")
	}
}

// TestTestBackupSetConnection_NoDetailCarriesTransportErrorText is FR-33's
// discipline at the service boundary. The fixture's key file is deleted,
// so the underlying error names a real path on this host, and that path
// must reach the log and not the response.
func TestTestBackupSetConnection_NoDetailCarriesTransportErrorText(t *testing.T) {
	fx := startSSHFixture(t, true, false)
	svc := sshBackedService(t, fx)
	if err := os.Remove(fx.keyFile); err != nil {
		t.Fatalf("removing the key file: %v", err)
	}

	got, err := svc.TestBackupSetConnection(context.Background(), "api-server/var-backups")
	if err != nil {
		t.Fatalf("TestBackupSetConnection: %v", err)
	}
	if got.OK {
		t.Fatal("OK = true with the key file gone")
	}
	if stepOutcomes(got.Checks)[string(sourcecheck.StepCredentials)] != string(sourcecheck.Failed) {
		t.Fatalf("credentials did not fail with the key file removed: %+v", got.Checks)
	}
	for _, c := range got.Checks {
		if strings.Contains(c.Detail, fx.keyFile) {
			t.Errorf("%s's detail carries the resolved key path %q, which is a fact about this machine an API caller has no use for and a reader of an exported response has every use for:\n  %s", c.Step, fx.keyFile, c.Detail)
		}
	}
}
