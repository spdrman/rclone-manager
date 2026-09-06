// This file is issue #146 (B2.7)'s host-key-probe API surface: fetching a
// remote SSH server's host key BEFORE it is trusted, so the add-backup-set
// wizard's "Verify server" step can show an operator a real fingerprint
// (docs/ssh-setup.md step 4's `ssh-keyscan`) instead of the hardcoded
// placeholder BackupSetWizardPage.tsx shipped with in #98.
//
// It lives in this package, next to ssh.go and keysource.go, on purpose:
// ssh.go's own package comment says the SSH posture "needs one owner and
// one test file", and a probe is part of that same posture even though it
// runs before trust is established rather than after. It is NOT a second,
// competing way to verify a host key at connection time — sftpConfig
// (ssh.go) still refuses to build a connection unless known_hosts pins a
// real file, and this probe never writes to one. It only ever answers "what
// key does this host offer right now", the exact question `ssh-keyscan`
// answers, so a human (or, here, the wizard) can compare it against an
// out-of-band source before anything gets written to known_hosts at all
// (see docs/ssh-setup.md step 4's "Compare the two fingerprints by eye").
package rclone

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// probeTimeout bounds how long ProbeHostKey waits for a TCP connection and
// the SSH key-exchange that reveals the host key. A probe is meant to be a
// single round trip against a server the operator just typed a hostname
// for; an unreachable or hanging host should fail fast rather than hang
// the wizard's "Verify server" step.
var probeTimeout = 5 * time.Second

// errHostKeyCaptured is returned by the HostKeyCallback below the instant
// it has captured the offered key, to abort the handshake before it gets
// anywhere near user authentication. ProbeHostKey never has, and never
// needs, credentials for the host it is probing: capturing the key during
// key exchange is enough, and key exchange happens before authentication
// in the SSH protocol regardless of what HostKeyCallback returns.
var errHostKeyCaptured = errors.New("rclone: host key captured")

// HostKeyProbeResult is what ProbeHostKey reports: the host key currently
// offered by a server, before any trust decision has been made about it.
type HostKeyProbeResult struct {
	// Algorithm is the SSH public key algorithm name (e.g. "ssh-ed25519"),
	// exactly as ssh.PublicKey.Type() reports it.
	Algorithm string

	// Fingerprint is the SHA256 fingerprint in the same "SHA256:base64…"
	// form `ssh-keygen -lf` prints (ssh.FingerprintSHA256).
	Fingerprint string

	// KnownHostsLine is this key rendered in the exact known_hosts file
	// format sshd/rclone read (golang.org/x/crypto/ssh/knownhosts.Line),
	// addressed to host:port. A caller that goes on to trust this key
	// writes exactly this line to the known_hosts file a backup set's
	// Remote.KnownHosts will point at; ProbeHostKey itself never writes
	// anything.
	KnownHostsLine string
}

// ProbeHostKey dials host:port and captures the SSH host key it offers,
// without authenticating and without trusting or persisting anything.
// This is the automated, remotely-driven equivalent of docs/ssh-setup.md
// step 4's `ssh-keyscan`; the trust decision itself (verifying the
// fingerprint out-of-band, then writing it to a known_hosts file) stays
// exactly as manual, or as caller-driven, as that doc already describes —
// this function answers "what key is being offered right now", nothing
// more.
func ProbeHostKey(ctx context.Context, host string, port int) (HostKeyProbeResult, error) {
	if host == "" {
		return HostKeyProbeResult{}, errors.New("host is required")
	}
	if port <= 0 || port > 65535 {
		return HostKeyProbeResult{}, fmt.Errorf("port %d is out of range", port)
	}

	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return HostKeyProbeResult{}, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	var captured ssh.PublicKey
	clientConfig := &ssh.ClientConfig{
		// No credentials: a probe only needs the key exchange, which the
		// SSH protocol performs before authentication is even attempted,
		// so there is nothing here for a real account or password to do.
		User: "probe",
		Auth: nil,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured = key
			return errHostKeyCaptured
		},
		Timeout: probeTimeout,
	}

	_, _, _, err = ssh.NewClientConn(conn, addr, clientConfig)
	if captured == nil {
		// The handshake failed before key exchange ever reached our
		// callback (a non-SSH service on that port, a protocol version
		// mismatch, ...): this is a genuine probe failure, not the
		// expected errHostKeyCaptured abort.
		if err == nil {
			err = errors.New("server did not offer a host key")
		}
		return HostKeyProbeResult{}, fmt.Errorf("probing host key at %s: %w", addr, err)
	}

	return HostKeyProbeResult{
		Algorithm:      captured.Type(),
		Fingerprint:    ssh.FingerprintSHA256(captured),
		KnownHostsLine: knownhosts.Line([]string{addr}, captured),
	}, nil
}
