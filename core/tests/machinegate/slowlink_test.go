// A relay that hands the client bytes at a fixed rate, so a transfer lasts
// long enough to be interrupted, and the check that slowing the link did not
// weaken anything else.
//
// slowLink's own comment carries the long argument for why this is a relay
// and not rclone's --bwlimit, and it is worth reading before reaching for the
// simpler option: the bandwidth limiter parks in a wait that ignores the
// caller's context, so throttling with it makes part of the transfer
// uninterruptible and turns any timing assertion into a statement about the
// token bucket instead of about the code under test. A slow wire changes only
// the speed of the wire.
//
// The cell in this file is the one that keeps the detour honest. Everything
// reached through the relay is reached at the relay's address, so host-key
// verification has to be re-pinned there, and a re-pin done wrongly leaves
// the check passing everything. The decoy key is what makes that visible.
package machinegate_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// slowLink is a TCP relay that sits in front of the SFTP fixture and hands
// the client bytes at a fixed rate.
//
// # Why this exists rather than --bwlimit
//
// Because a test that needs a transfer to last long enough to interrupt has
// two ways to get one, and only one of them is about the transfer.
//
// rclone's own --bwlimit is the other one, and it was what the
// mid-transfer cancellation row used until issue #414. It throttles
// INSIDE rclone, in two places: fs/accounting's Account.accountRead pays
// the token bucket after every chunk it hands the copy loop, and
// fs/fshttp's dialer pays it again on every socket read. Both wait with
// tokenBucket.LimitBandwidth, which calls WaitN(context.Background(), n).
// Parked in that wait, nothing is watching the caller's context, by
// construction. So the one lever the test reaches for to make the transfer
// long is also a lever that makes part of it uninterruptible, and an
// assertion about how quickly a cancelled copy returns is then really an
// assertion about the bucket.
//
// A relay is slow the way a real link is slow. rclone reads whatever
// arrives, whenever it arrives, and every ctx check it makes between chunks
// still runs. Nothing about the code path under test changes; only the
// speed of the wire does.
//
// # What it does not do
//
// It does not shape the client-to-server direction, which carries SFTP read
// requests and is a few bytes per 32KiB of payload, and it does not model
// latency, loss or reordering. It makes one thing true (the server's bytes
// arrive at a known rate) and leaves the rest of the connection alone.
type slowLink struct {
	fixture *machines.Source
	// knownHosts is the fixture's own host keys, re-pinned to this relay's
	// port so host-key verification stays real rather than being turned
	// off for the sake of the detour.
	knownHosts string

	ln    net.Listener
	rate  int // bytes per second, server to client
	chunk int

	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

// slowLinkChunk is how much the relay moves between sleeps. It is well
// under the 32KiB sftpConfig pins as the sftp chunk size, so the pacing is
// finer-grained than the protocol's own unit and no single sleep can hide a
// whole request.
const slowLinkChunk = 8 * 1024

// startSlowLink puts a relay in front of f, delivering the server's bytes
// at bytesPerSecond, and registers its cleanup.
func startSlowLink(t *testing.T, f *machines.Source, bytesPerSecond int) *slowLink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("slowLink: listen: %v", err)
	}
	l := &slowLink{fixture: f, ln: ln, rate: bytesPerSecond, chunk: slowLinkChunk}
	// The relay listens on 127.0.0.1 in this process, in both placements,
	// so that is the address the machine's real keys are re-pinned to. The
	// machine's own address is whatever Addr() says and is not used here.
	l.knownHosts = f.KnownHostsFor(t, "127.0.0.1", l.Port())

	// serve() is counted in the same WaitGroup as the connections it
	// spawns, so the counter is never zero while it is still accepting.
	// Without that, an Accept that returns just as the Cleanup runs would
	// call wg.Add on a WaitGroup whose Wait had already been entered at
	// zero, which is a documented misuse and panics rather than flaking.
	l.wg.Add(1)
	go l.serve()
	t.Cleanup(func() {
		l.mu.Lock()
		l.closed = true
		conns := l.conns
		l.conns = nil
		l.mu.Unlock()
		_ = ln.Close()
		// Closing the sockets rather than only the listener, because a
		// relay goroutine parked in a read on a connection rclone has not
		// finished with would otherwise hold this Cleanup, and a fixture
		// that can hang the suite is the failure #161 is about.
		for _, c := range conns {
			_ = c.Close()
		}
		l.wg.Wait()
	})
	t.Logf("slow link: 127.0.0.1:%d -> %s at %d B/s", l.Port(), f.Addr(), bytesPerSecond)
	return l
}

// Port is the relay's own port, which is what a Source has to dial.
func (l *slowLink) Port() int { return l.ln.Addr().(*net.TCPAddr).Port }

// Source mirrors machines.Source.TransportSource, pointing at the relay instead
// of at the container, with the host keys re-pinned to match. Everything
// else, including real host-key verification, is exactly what the fixture
// would have handed out.
func (l *slowLink) Source(id, root string) transport.Source {
	// Derived from the machine's own TransportSource rather than rebuilt
	// field by field, so the only things that differ are the two that have
	// to: the address, which is the relay's, and the known_hosts, which
	// records the machine's real keys at that address. A hand-built copy
	// would silently stop matching the moment TransportSource gained a
	// field.
	src := l.fixture.TransportSource(id, root)
	src.Host = "127.0.0.1"
	src.Port = l.Port()
	src.KnownHosts = l.knownHosts
	return src
}

func (l *slowLink) serve() {
	defer l.wg.Done()
	for {
		client, err := l.ln.Accept()
		if err != nil {
			return
		}
		l.track(client)
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handle(client)
		}()
	}
}

func (l *slowLink) track(c net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		_ = c.Close()
		return
	}
	l.conns = append(l.conns, c)
}

func (l *slowLink) handle(client net.Conn) {
	defer func() { _ = client.Close() }()
	// Addr() is the machine as this process reaches it: a published
	// loopback port on the host, its network alias inside a manager
	// container. The relay does not need to know which.
	upstream, err := net.Dial("tcp", l.fixture.Addr())
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	l.track(upstream)

	var both sync.WaitGroup
	both.Add(2)
	// Client to server unshaped: these are SFTP requests, a few bytes each,
	// and slowing them would slow the round trips rather than the payload.
	go func() {
		defer both.Done()
		_, _ = io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	go func() {
		defer both.Done()
		l.pace(client, upstream)
		closeWrite(client)
	}()
	both.Wait()
}

// pace copies src to dst at l.rate, sleeping for exactly the time the bytes
// it just moved are worth.
func (l *slowLink) pace(dst io.Writer, src io.Reader) {
	buf := make([]byte, l.chunk)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			time.Sleep(time.Duration(float64(n) / float64(l.rate) * float64(time.Second)))
		}
		if err != nil {
			return
		}
	}
}

func closeWrite(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}

// repinKnownHosts used to live here, rewriting the machine's known_hosts so
// its real keys were pinned to the relay's port. It is
// machines.Source.KnownHostsFor now, with DecoyKnownHostsFor as its
// sibling, for two reasons.
//
// It has a test on it, driving the real adapter through a real relay in
// both directions (core/tests/machinegate/relay_test.go), which a helper
// in a test file did not.
//
// And the address is no longer always 127.0.0.1. The relay's own address
// is, because the relay runs in this process; the MACHINE's is not, and
// once the tier runs inside a manager container (#451) it is an alias on a
// bridge network. Keeping the two apart in a helper that only ever saw one
// of them is how this would have gone quietly wrong.

// SourceWithKnownHosts is Source with a different known_hosts file, which
// exists only so the negative control below can point a relay Source at the
// wrong key.
func (l *slowLink) SourceWithKnownHosts(id, root, knownHosts string) transport.Source {
	src := l.Source(id, root)
	src.KnownHosts = knownHosts
	return src
}

// BadKnownHosts is the machine's decoy host key, re-pinned to this relay's
// port the same way the real one is.
func (l *slowLink) BadKnownHosts(t *testing.T) string {
	t.Helper()
	return l.fixture.DecoyKnownHostsFor(t, "127.0.0.1", l.Port())
}

// TestSlowLinkStillVerifiesHostKeys is the negative control on the detour
// itself, and it is here because of how quietly this one fails.
//
// Putting a relay in front of the fixture means the Source no longer points
// at the address the fixture's known_hosts pins, so the obvious way to make
// the connection work again is to stop verifying the host key. That would
// leave every test using this relay passing while silently no longer
// exercising FR-6, which is the shape of defect this repository keeps
// finding rather than a hypothetical.
//
// So the relay re-pins the fixture's REAL keys to its own port, and this row
// proves the re-pinning is load-bearing: the same connection, through the
// same relay, with the fixture's decoy key pinned instead, has to be
// refused. If host-key verification had been quietly relaxed for the sake of
// the detour, this row would pass the wrong way round and say so.
func TestSlowLinkStillVerifiesHostKeys(t *testing.T) {
	f := machines.Start(t).Source(t)
	a := rclone.New()
	// Rate is irrelevant here: nothing gets far enough to transfer.
	link := startSlowLink(t, f, cancelLinkRate)

	// Positive control first. Without it, "the bad key was refused" has a
	// second explanation, which is that nothing can connect through the
	// relay at all and the refusal has nothing to do with the key.
	if _, err := a.List(context.Background(), link.Source("slowlink-hostkey-good", "")); err != nil {
		t.Fatalf("the relay could not connect with the fixture's own host keys re-pinned to its port, so this row "+
			"cannot say anything about which key was refused: %v", err)
	}

	bad := link.SourceWithKnownHosts("slowlink-hostkey-bad", "", link.BadKnownHosts(t))
	_, err := a.List(context.Background(), bad)
	if err == nil {
		t.Fatal("a connection through the relay succeeded against the fixture's DECOY host key. " +
			"Host-key verification is not happening on this path, so every test that reaches the fixture through " +
			"the relay has been proving less than it says (FR-6).")
	}
	t.Logf("the relay refused the decoy host key, so verification is real on this path: %v", err)
}
