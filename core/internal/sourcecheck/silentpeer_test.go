// What a peer that accepts the connection and then says nothing can do to
// this check (PR #628 review).
//
// The banner cases beside this file hang up after their one line, and
// bannerServer's own comment says why: ssh.NewClientConn puts no deadline
// on a handshake it did not dial, so a server that simply went quiet
// would have left every case there waiting forever. That sentence was
// true, and it was describing a bug rather than a fixture constraint.
// The one caller that runs this check with a process-wide lock held
// (core/service.UpdateBackupSet) believed the handshake was bounded, and
// against exactly this peer it was not, so one edit pointed at a silent
// host parked every configuration write in the process behind it until a
// restart. This file is the case that peer gets.
package sourcecheck

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// silentServer is a plain TCP listener that accepts a connection and then
// sends nothing, for as long as the connection is open.
//
// That is not a contrived peer. A load balancer in front of a dead
// backend, an appliance that answers on 22 with something that is not
// sshd, and a firewall that accepts and then drops all look exactly like
// this from the outside: the TCP connect succeeds and no identification
// string ever arrives. It holds every accepted socket open until cleanup
// on purpose; a server that hung up would be bannerServer with an empty
// banner, and the read would return on the close rather than on anything
// this check did.
func silentServer(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return // the listener was closed by cleanup
			}
			// No t.* calls: this goroutine outlives the test body.
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

// TestRun_APeerThatAcceptsAndSaysNothingCannotHoldTheHandshake is the
// regression case for the one way this check could run forever.
//
// ssh.NewClientConn takes an already-open socket and never reads
// ClientConfig.Timeout: in the pinned x/crypto only Dial reads it, to
// bound the TCP connect Dial makes itself, and the field's own doc says
// as much. It takes no context either. So the handshake's only bound was
// the one exchangeHostKey believed it had set, and against a peer that
// accepted the connection and then sent nothing, the key exchange sat in
// a read with nothing to wake it. readIdentification's own two-second
// deadline did not help, because it cleared that deadline on the way out.
//
// Two cases, because there are two bounds and each has to be shown to
// hold without the other. The context's deadline is what every caller in
// core/service passes (its connectionTestTimeout), and handshakeTimeout
// is the floor for a caller that passes none. Each runs the check on a
// goroutine and waits with a select rather than calling it in line,
// because the failure under test is a call that never returns, and a test
// that hangs reports nothing.
func TestRun_APeerThatAcceptsAndSaysNothingCannotHoldTheHandshake(t *testing.T) {
	host, port := silentServer(t)
	target := Target{
		BackupSetID: "api-server/var-backups", Host: host, Port: port,
		User: "backups", RemotePath: "/var/backups", KeyReference: "ssh_key_2",
	}
	deps := Deps{
		Credentials: func(context.Context) (string, error) { return "SHA256:aClientKeyFingerprint", nil },
		Trusted:     func(context.Context) ([]TrustedKey, error) { return nil, nil },
		Verify:      func(string, net.Addr, ssh.PublicKey) error { return nil },
		List:        func(context.Context) (int, error) { return 0, nil },
	}

	runWithin := func(t *testing.T, ctx context.Context, bound time.Duration) (Report, time.Duration) {
		t.Helper()
		done := make(chan Report, 1)
		started := time.Now()
		go func() { done <- Run(ctx, target, deps) }()
		select {
		case r := <-done:
			return r, time.Since(started)
		case <-time.After(bound):
			t.Fatalf("Run has not returned %s after connecting to a peer that accepts and says nothing: the key exchange is blocked in a read nothing bounds", bound)
			return Report{}, 0
		}
	}

	// assertBoundedFailure is what both cases expect of the report: the
	// connect step passed (the socket really was accepted), the host key
	// step failed as a NETWORK fact rather than a host-key one (no key was
	// ever offered to compare), and nothing after it was tried.
	assertBoundedFailure := func(t *testing.T, r Report) {
		t.Helper()
		if r.OK {
			t.Fatal("a peer that never offered a host key passed the check")
		}
		got := outcomes(r)
		if got[StepConnect] != Passed {
			t.Errorf("connect = %s, want %s: the socket was accepted, and that is what the step reports on", got[StepConnect], Passed)
		}
		hostKey := checkFor(t, r, StepHostKey)
		if hostKey.Outcome != Failed || hostKey.Category != CategoryNetwork {
			t.Errorf("host key = %s/%s, want %s/%s: nothing was offered, so there was no key to mismatch", hostKey.Outcome, hostKey.Category, Failed, CategoryNetwork)
		}
		if !strings.Contains(hostKey.Detail, "did not complete an SSH key exchange") {
			t.Errorf("host key detail %q does not say the exchange never completed", hostKey.Detail)
		}
		if got[StepAuthenticate] != Skipped || got[StepList] != Skipped {
			t.Errorf("authenticate = %s and list = %s, want both %s", got[StepAuthenticate], got[StepList], Skipped)
		}
	}

	t.Run("the context's deadline bounds it", func(t *testing.T) {
		const deadline = 3 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()

		report, took := runWithin(t, ctx, 30*time.Second)
		assertBoundedFailure(t, report)
		// Generous over the deadline, because this host runs other suites
		// at the same time; the number that matters is that it is nowhere
		// near handshakeTimeout, which is what a build that ignored the
		// context would take.
		if took > deadline+5*time.Second {
			t.Errorf("Run took %s against a %s context deadline; the handshake is not being cut off by the context", took.Round(time.Millisecond), deadline)
		}
	})

	t.Run("handshakeTimeout bounds it when the context has no deadline", func(t *testing.T) {
		report, took := runWithin(t, context.Background(), 3*handshakeTimeout)
		assertBoundedFailure(t, report)
		// The identification read gives up on its own after two seconds,
		// then the exchange gets handshakeTimeout; anything well past
		// their sum is a bound that is not being applied.
		if took > handshakeTimeout+2*time.Second+5*time.Second {
			t.Errorf("Run took %s with no context deadline; handshakeTimeout (%s) is not bounding the exchange", took.Round(time.Millisecond), handshakeTimeout)
		}
	})
}
