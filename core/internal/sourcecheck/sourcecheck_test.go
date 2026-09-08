package sourcecheck

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The tests below drive Run against a REAL SSH server running in this
// process, not against a stub of one. That is the whole point: the four
// failures this check exists to tell apart (a key this host cannot open,
// a host that does not resolve, a host key that is not the trusted one,
// a server that refuses the key) are decided at four different layers,
// and a fake that answered them by returning canned outcomes would prove
// the renderer rather than the check.

// inProcessSSH starts a real SSH server on a random loopback port that
// accepts exactly `authorized` and refuses every other key, and returns
// its host, its port and its host key's SHA256 fingerprint.
//
// Pass a nil authorized key for a server that refuses everyone, which is
// how the authentication failure is provoked without touching anything
// outside this process.
func inProcessSSH(t *testing.T, authorized ssh.PublicKey) (host string, port int, hostFingerprint, hostAlgorithm string) {
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
				return // the listener was closed by cleanup
			}
			// No t.* calls in here: this goroutine outlives the test
			// body, and a failure reported after the test has finished
			// panics the run instead of failing the test.
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
	return "127.0.0.1", addr.Port, ssh.FingerprintSHA256(hostSigner.PublicKey()), hostSigner.PublicKey().Type()
}

// clientKey mints a throwaway ed25519 client key and returns a signer for
// it plus its public half.
func clientKey(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a client key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("ssh.NewSignerFromKey: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	return signer, sshPub
}

// realProbe is the same job rclone.ProbeHostKey does, done here so this
// package's tests do not depend on that package's import graph. It is
// wired as Deps.ProbeHostKey exactly as production wires the real one.
func realProbe(ctx context.Context, host string, port int) (string, string, error) {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = conn.Close() }()

	captured := make(chan ssh.PublicKey, 1)
	stop := errors.New("captured")
	_, _, _, err = ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User: "probe",
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured <- key
			return stop
		},
	})
	select {
	case key := <-captured:
		return key.Type(), ssh.FingerprintSHA256(key), nil
	default:
		if err == nil {
			err = errors.New("no host key offered")
		}
		return "", "", err
	}
}

// depsFor wires every dependency to something real, so a test only has to
// state the one thing it is changing.
func depsFor(signer ssh.Signer, trusted []TrustedKey, entries int) Deps {
	return Deps{
		Credentials: func(context.Context) (ssh.Signer, string, error) {
			return signer, ssh.FingerprintSHA256(signer.PublicKey()), nil
		},
		Trusted:      func(context.Context) ([]TrustedKey, error) { return trusted, nil },
		ProbeHostKey: realProbe,
		List:         func(context.Context) (int, error) { return entries, nil },
	}
}

func outcomes(r Report) map[Step]Outcome {
	out := map[Step]Outcome{}
	for _, c := range r.Checks {
		out[c.Step] = c.Outcome
	}
	return out
}

func checkFor(t *testing.T, r Report, step Step) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Step == step {
			return c
		}
	}
	t.Fatalf("the report carries no check for %q; Steps is closed and Run must answer all of it", step)
	return Check{}
}

// TestRun_EveryStepPasses is the control on every refusal below. Without
// it, an implementation that failed everything would pass all four
// failure tests and nothing would notice.
func TestRun_EveryStepPasses(t *testing.T) {
	signer, pub := clientKey(t)
	host, port, fingerprint, algorithm := inProcessSSH(t, pub)

	report := Run(context.Background(), Target{
		BackupSetID:          "api-server/var-backups",
		Host:                 host,
		Port:                 port,
		User:                 "backups",
		RemotePath:           "/var/backups",
		KeyReference:         "ssh_key_2",
		PassphraseConfigured: true,
	}, depsFor(signer, []TrustedKey{{Algorithm: algorithm, Fingerprint: fingerprint, Line: 1}}, 41))

	if !report.OK {
		t.Fatalf("a server that answers, offers the trusted key and accepts the credential reported not OK: %+v", report.Checks)
	}
	if len(report.Checks) != len(Steps) {
		t.Fatalf("got %d checks, want one per step (%d): %+v", len(report.Checks), len(Steps), report.Checks)
	}
	for i, want := range Steps {
		if report.Checks[i].Step != want {
			t.Errorf("check %d is %q, want %q: the order of Steps is the order a surface renders", i, report.Checks[i].Step, want)
		}
		if report.Checks[i].Outcome != Passed {
			t.Errorf("%s: outcome %q, want %q (%s)", want, report.Checks[i].Outcome, Passed, report.Checks[i].Detail)
		}
	}

	// The details have to carry the facts an operator acts on, not just
	// the word "passed". These are the four the issue names.
	if d := checkFor(t, report, StepCredentials).Detail; !strings.Contains(d, "ssh_key_2") || !strings.Contains(d, "passphrase") {
		t.Errorf("credentials detail does not name the key or say the passphrase opened it: %q", d)
	}
	if d := checkFor(t, report, StepConnect).Detail; !strings.Contains(d, "ms") && !strings.Contains(d, "s") {
		t.Errorf("connect detail does not say how fast the connect was: %q", d)
	}
	if d := checkFor(t, report, StepHostKey).Detail; !strings.Contains(d, fingerprint) {
		t.Errorf("host_key detail does not print the fingerprint that matched: %q", d)
	}
	if d := checkFor(t, report, StepList).Detail; !strings.Contains(d, "/var/backups") || !strings.Contains(d, "41") {
		t.Errorf("list detail does not name the path listed and how many entries came back: %q", d)
	}
}

// TestRun_HostKeyMismatchSkipsAuthenticateAndList is the acceptance
// criterion, and the sharpest one: nothing may be offered to a server
// whose identity did not check out, and the two steps that never ran must
// say they never ran rather than being quietly absent or quietly passed.
func TestRun_HostKeyMismatchSkipsAuthenticateAndList(t *testing.T) {
	signer, pub := clientKey(t)
	host, port, offeredFingerprint, algorithm := inProcessSSH(t, pub)

	// A different key entirely, which is exactly what a rebuilt or
	// re-keyed machine offers.
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating the stale trusted key: %v", err)
	}
	otherSigner, err := ssh.NewSignerFromKey(other)
	if err != nil {
		t.Fatalf("ssh.NewSignerFromKey: %v", err)
	}
	staleFingerprint := ssh.FingerprintSHA256(otherSigner.PublicKey())

	listed := false
	deps := depsFor(signer, []TrustedKey{{Algorithm: algorithm, Fingerprint: staleFingerprint, Line: 3}}, 0)
	deps.List = func(context.Context) (int, error) {
		listed = true
		return 0, nil
	}

	report := Run(context.Background(), Target{
		BackupSetID: "cicd-pipeline/var-backups", Host: host, Port: port,
		User: "deploy", RemotePath: "/var/backups", KeyReference: "ssh_key_4",
	}, deps)

	got := outcomes(report)
	for _, step := range []Step{StepCredentials, StepResolve, StepConnect} {
		if got[step] != Passed {
			t.Errorf("%s: outcome %q, want %q: the steps before the mismatch really did succeed", step, got[step], Passed)
		}
	}
	if got[StepHostKey] != Failed {
		t.Fatalf("host_key: outcome %q, want %q against a server offering an untrusted key", got[StepHostKey], Failed)
	}
	if c := checkFor(t, report, StepHostKey); c.Category != CategoryHostKey {
		t.Errorf("host_key category %q, want %q: the category is the half a surface branches on", c.Category, CategoryHostKey)
	}
	// Both fingerprints, in ssh-keygen -lf form. An operator settles a
	// mismatch by comparing them by eye, and a report carrying only one
	// of them cannot be compared against anything.
	detail := checkFor(t, report, StepHostKey).Detail
	if !strings.Contains(detail, offeredFingerprint) {
		t.Errorf("host_key detail does not print the OFFERED fingerprint %q: %q", offeredFingerprint, detail)
	}
	if !strings.Contains(detail, staleFingerprint) {
		t.Errorf("host_key detail does not print the TRUSTED fingerprint %q: %q", staleFingerprint, detail)
	}

	for _, step := range []Step{StepAuthenticate, StepList} {
		if got[step] != Skipped {
			t.Errorf("%s: outcome %q, want %q. A surface that draws an unattempted authentication as anything but \"this was never tried\" has told an operator their credentials are fine on the strength of a step that never ran", step, got[step], Skipped)
		}
		if checkFor(t, report, step).Detail == "" {
			t.Errorf("%s was skipped with no reason, so a reader cannot tell why it never ran", step)
		}
	}
	if listed {
		t.Error("the remote path was listed after the host key did not match; the connection test authenticated against a server whose identity did not check out")
	}
	if report.OK {
		t.Error("the report is OK with a failed host_key step")
	}
	if stopped, ok := report.StoppedAt(); !ok || stopped.Step != StepHostKey {
		t.Errorf("StoppedAt() = %+v, %v; want the host_key check, so a surface can name the step rather than the whole button", stopped, ok)
	}
}

// TestRun_UnreadableCredentialSkipsEverythingElse is the other half of
// the skip discipline, from the other end of the list: a key this host
// cannot open is not a network problem and must not be reported as one.
func TestRun_UnreadableCredentialSkipsEverythingElse(t *testing.T) {
	signer, pub := clientKey(t)
	host, port, fingerprint, algorithm := inProcessSSH(t, pub)

	probed := false
	deps := depsFor(signer, []TrustedKey{{Algorithm: algorithm, Fingerprint: fingerprint, Line: 1}}, 4)
	deps.Credentials = func(context.Context) (ssh.Signer, string, error) {
		return nil, "", errors.New("open /var/lib/backup-manager/keys/ssh_key_4: permission denied")
	}
	deps.ProbeHostKey = func(ctx context.Context, h string, p int) (string, string, error) {
		probed = true
		return realProbe(ctx, h, p)
	}

	var observed []Step
	deps.Observe = func(step Step, _ error) { observed = append(observed, step) }

	report := Run(context.Background(), Target{
		BackupSetID: "api-server/var-backups", Host: host, Port: port,
		User: "backups", RemotePath: "/var/backups", KeyReference: "ssh_key_4",
	}, deps)

	got := outcomes(report)
	if got[StepCredentials] != Failed {
		t.Fatalf("credentials: outcome %q, want %q", got[StepCredentials], Failed)
	}
	if c := checkFor(t, report, StepCredentials); c.Category != CategoryCredentials {
		t.Errorf("credentials category %q, want %q", c.Category, CategoryCredentials)
	}
	for _, step := range []Step{StepResolve, StepConnect, StepHostKey, StepAuthenticate, StepList} {
		if got[step] != Skipped {
			t.Errorf("%s: outcome %q, want %q after the credential could not be obtained", step, got[step], Skipped)
		}
	}
	if probed {
		t.Error("the host key was probed after the credential failed; nothing should touch the far side once the key cannot be opened on this host")
	}
	if len(observed) != 1 || observed[0] != StepCredentials {
		t.Errorf("Observe was called for %v, want exactly [credentials]: the classified cause goes to the log, once, for the step that failed", observed)
	}
}

// TestRun_ServerRefusesTheKeyFailsAtAuthenticate proves the layer below
// the host key is reported on its own. Before this package, a server that
// answered, offered the trusted key and then refused the credential was
// the same sentence as a typo'd hostname.
func TestRun_ServerRefusesTheKeyFailsAtAuthenticate(t *testing.T) {
	signer, _ := clientKey(t)
	// nil authorized: the server offers a real host key and refuses every
	// credential, which is exactly an un-authorised key on a real host.
	host, port, fingerprint, algorithm := inProcessSSH(t, nil)

	listed := false
	deps := depsFor(signer, []TrustedKey{{Algorithm: algorithm, Fingerprint: fingerprint, Line: 1}}, 0)
	deps.List = func(context.Context) (int, error) {
		listed = true
		return 0, nil
	}

	report := Run(context.Background(), Target{
		BackupSetID: "api-server/var-backups", Host: host, Port: port,
		User: "backups", RemotePath: "/var/backups", KeyReference: "ssh_key_2",
	}, deps)

	got := outcomes(report)
	if got[StepHostKey] != Passed {
		t.Fatalf("host_key: outcome %q, want %q: this server DOES offer the trusted key", got[StepHostKey], Passed)
	}
	if got[StepAuthenticate] != Failed {
		t.Fatalf("authenticate: outcome %q, want %q against a server that refuses every key", got[StepAuthenticate], Failed)
	}
	if c := checkFor(t, report, StepAuthenticate); c.Category != CategoryAuthentication {
		t.Errorf("authenticate category %q, want %q", c.Category, CategoryAuthentication)
	}
	if got[StepList] != Skipped {
		t.Errorf("list: outcome %q, want %q after authentication failed", got[StepList], Skipped)
	}
	if listed {
		t.Error("the remote path was listed after authentication failed")
	}
}

// TestRun_ResolveFailureIsNotANetworkFailure keeps DNS its own answer.
// "this name does not exist" and "nothing answered on that port" send an
// operator to two different places.
func TestRun_ResolveFailureIsNotANetworkFailure(t *testing.T) {
	signer, _ := clientKey(t)
	deps := depsFor(signer, nil, 0)

	report := Run(context.Background(), Target{
		BackupSetID: "api-server/var-backups",
		// .invalid is reserved by RFC 2606 and must never resolve, so
		// this test does not depend on the machine running it having no
		// wildcard resolver for some other made-up name.
		Host: "this-host-does-not-exist.invalid", Port: 22,
		User: "backups", RemotePath: "/var/backups", KeyReference: "ssh_key_2",
	}, deps)

	got := outcomes(report)
	if got[StepCredentials] != Passed {
		t.Errorf("credentials: outcome %q, want %q: the key was fine", got[StepCredentials], Passed)
	}
	if got[StepResolve] != Failed {
		t.Fatalf("resolve: outcome %q, want %q for a name that cannot resolve", got[StepResolve], Failed)
	}
	if c := checkFor(t, report, StepResolve); c.Category != CategoryDNS {
		t.Errorf("resolve category %q, want %q: DNS and network send an operator to different places", c.Category, CategoryDNS)
	}
	for _, step := range []Step{StepConnect, StepHostKey, StepAuthenticate, StepList} {
		if got[step] != Skipped {
			t.Errorf("%s: outcome %q, want %q after the name did not resolve", step, got[step], Skipped)
		}
	}
}

// TestRun_NoDetailCarriesTransportErrorText is FR-33's discipline held
// mechanically rather than by care at each call site, the same way
// mediumcheck holds it. Every dependency below fails with a sentence full
// of things that must never reach a rendered surface, and no fragment of
// any of them may appear in the report.
func TestRun_NoDetailCarriesTransportErrorText(t *testing.T) {
	poison := []string{
		"/var/lib/backup-manager/keys/ssh_key_4",
		"ssh: handshake failed: knownhosts.HostKeyCallback",
		"dial tcp 203.0.113.24:1209: connect: connection refused",
		"sftp: \"Permission denied\" (SSH_FX_PERMISSION_DENIED)",
	}
	poisoned := errors.New(strings.Join(poison, " | "))

	signer, pub := clientKey(t)
	host, port, fingerprint, algorithm := inProcessSSH(t, pub)

	for _, tc := range []struct {
		name string
		with func(*Deps)
	}{
		{"credentials", func(d *Deps) {
			d.Credentials = func(context.Context) (ssh.Signer, string, error) { return nil, "", poisoned }
		}},
		{"host key probe", func(d *Deps) {
			d.ProbeHostKey = func(context.Context, string, int) (string, string, error) { return "", "", poisoned }
		}},
		{"known hosts", func(d *Deps) {
			d.Trusted = func(context.Context) ([]TrustedKey, error) { return nil, poisoned }
		}},
		{"list", func(d *Deps) {
			d.List = func(context.Context) (int, error) { return 0, poisoned }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := depsFor(signer, []TrustedKey{{Algorithm: algorithm, Fingerprint: fingerprint, Line: 1}}, 3)
			tc.with(&deps)
			report := Run(context.Background(), Target{
				BackupSetID: "api-server/var-backups", Host: host, Port: port,
				User: "backups", RemotePath: "/var/backups", KeyReference: "ssh_key_2",
			}, deps)
			if report.OK {
				t.Fatalf("the report is OK with %s failing, so this test would prove nothing", tc.name)
			}
			for _, c := range report.Checks {
				for _, fragment := range poison {
					if strings.Contains(c.Detail, fragment) {
						t.Errorf("%s's detail carries transport error text %q, which a caller must not have to treat as safe to render:\n  %s", c.Step, fragment, c.Detail)
					}
				}
			}
		})
	}
}
