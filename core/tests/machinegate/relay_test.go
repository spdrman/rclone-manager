package machinegate_test

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// startRelay puts a plain TCP relay in front of a machine and returns the
// address the relay listens on.
//
// It is the shape any test that wants to interfere with the link needs: add
// latency, drop a connection mid-transfer, throttle. The interference goes
// in the copy loop; this one does nothing but forward, because what it is
// here to establish is the part that is easy to get silently wrong.
func startRelay(t *testing.T, upstream string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			down, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			wg.Add(1)
			go func(down net.Conn) {
				defer wg.Done()
				defer func() { _ = down.Close() }()
				up, dialErr := net.Dial("tcp", upstream)
				if dialErr != nil {
					return
				}
				defer func() { _ = up.Close() }()
				var inner sync.WaitGroup
				inner.Add(2)
				go func() { defer inner.Done(); _, _ = io.Copy(up, down); _ = up.Close() }()
				go func() { defer inner.Done(); _, _ = io.Copy(down, up); _ = down.Close() }()
				inner.Wait()
			}(down)
		}
	}()
	return ln.Addr().String()
}

// TestHostKeyVerificationStaysRealThroughARelay is the proof for
// machines.Source.KnownHostsFor, and it is here rather than in the harness
// package because the thing that has to stay true is FR-6 as the ADAPTER
// enforces it, not as a test helper reimplements it.
//
// A test that puts something in front of a machine has to re-record the
// machine's host keys against the relay's address, because known_hosts
// matches on address and the adapter would otherwise refuse a server whose
// key it holds. The way that goes wrong is not by failing: it is by the
// test weakening the check to get past the refusal, or by pinning an
// address that is never consulted, at which point the suite still passes
// and verifies nothing.
//
// So both directions are asserted. Through the relay with the re-addressed
// file the adapter connects; through the relay with the machine's OWN file
// it is refused, because that file records a different address. The second
// half is what makes the first half mean something: without it, "it
// connected" is equally consistent with verification being off.
func TestHostKeyVerificationStaysRealThroughARelay(t *testing.T) {
	src := machines.Start(t).Source(t)
	writeUploadFile(t, src, "through-the-relay.txt", []byte("relayed"))

	relayAddr := startRelay(t, src.Addr())
	relayHost, relayPortText, err := net.SplitHostPort(relayAddr)
	if err != nil {
		t.Fatalf("splitting the relay address %q: %v", relayAddr, err)
	}
	relayPort, err := strconv.Atoi(relayPortText)
	if err != nil {
		t.Fatalf("reading the relay port out of %q: %v", relayAddr, err)
	}

	adapter := rclone.New()
	ctx := context.Background()

	through := src.TransportSource("relayed", "")
	through.Host = relayHost
	through.Port = relayPort

	t.Run("with the machine's keys re-recorded against the relay, it connects", func(t *testing.T) {
		s := through
		s.KnownHosts = src.KnownHostsFor(t, relayHost, relayPort)
		entries, err := adapter.List(ctx, s)
		if err != nil {
			t.Fatalf("List through the relay with the machine's own host keys pinned at the relay's address: %v\nThe relay forwards to this exact machine, so the keys are this machine's keys and only the address changed. A refusal here is the harness re-addressing them wrongly, and the tempting fix (turn the check off) is the one that must not be taken.", err)
		}
		if len(entries) == 0 {
			t.Fatal("List through the relay returned nothing, so the relay is not actually forwarding to the machine this test seeded")
		}
	})

	t.Run("with the machine's own file, the relay's address is refused", func(t *testing.T) {
		s := through
		s.KnownHosts = src.KnownHostsFile // records the machine's OWN address, not the relay's
		_, err := adapter.List(ctx, s)
		if err == nil {
			t.Fatal("List through the relay succeeded against a known_hosts that has no entry for the relay's address at all. Host-key verification is not being enforced on this path, which means the subtest above proved nothing: it would pass with any key, or none.")
		}
		if !strings.Contains(err.Error(), "knownhosts") {
			t.Fatalf("the refusal is not a host-key refusal, so it says nothing about verification being enforced here: %v", err)
		}
	})
}
