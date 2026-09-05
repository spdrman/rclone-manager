package rclone

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/tests/sftpfixture"
)

// slowLink is a TCP relay that sits in front of the SFTP fixture and hands
// the client bytes at a fixed rate.
//
// # Why this exists rather than --bwlimit
//
// Because a test that needs a transfer to last long enough to interrupt has
// two ways to get one, and only one of them is about the transfer.
//
// rclone's own --bwlimit is the other one, and it was what
// gate_test.go's MidTransferCancellation used until issue #414. It throttles
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
	fixture *sftpfixture.Fixture
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
func startSlowLink(t *testing.T, f *sftpfixture.Fixture, bytesPerSecond int) *slowLink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("slowLink: listen: %v", err)
	}
	l := &slowLink{fixture: f, ln: ln, rate: bytesPerSecond, chunk: slowLinkChunk}
	l.knownHosts = repinKnownHosts(t, f.KnownHostsFile, l.Port())

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
	t.Logf("slow link: 127.0.0.1:%d -> %s:%d at %d B/s", l.Port(), f.Host, f.Port, bytesPerSecond)
	return l
}

// Port is the relay's own port, which is what a Source has to dial.
func (l *slowLink) Port() int { return l.ln.Addr().(*net.TCPAddr).Port }

// Source mirrors sftpfixture.Fixture.Source, pointing at the relay instead
// of at the container, with the host keys re-pinned to match. Everything
// else, including real host-key verification, is exactly what the fixture
// would have handed out.
func (l *slowLink) Source(id, root string) transport.Source {
	return transport.Source{
		ID:         id,
		Type:       "sftp",
		Host:       "127.0.0.1",
		Port:       l.Port(),
		User:       l.fixture.User,
		KeyFile:    l.fixture.KeyFile,
		KnownHosts: l.knownHosts,
		Root:       path.Join("upload", root),
	}
}

func (l *slowLink) serve() {
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
	upstream, err := net.Dial("tcp", net.JoinHostPort(l.fixture.Host, strconv.Itoa(l.fixture.Port)))
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

// repinKnownHosts rewrites the fixture's known_hosts so its real host keys
// are pinned to the relay's port instead of the container's.
//
// The alternative was to point the Source at the relay with host-key
// verification relaxed, and that would have quietly removed FR-6's
// verification from a test in the gate that exists to prove FR-6-shaped
// things. The keys here are the fixture's own, byte for byte; only the
// address they are pinned to changes, which is the one thing the detour
// really did change.
func repinKnownHosts(t *testing.T, knownHostsFile string, port int) string {
	t.Helper()
	fh, err := os.Open(knownHostsFile)
	if err != nil {
		t.Fatalf("slowLink: reading the fixture's known_hosts: %v", err)
	}
	defer fh.Close()

	var out strings.Builder
	entries := 0
	scanner := bufio.NewScanner(fh)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// host, keytype, key. ssh-keyscan writes exactly that.
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		fmt.Fprintf(&out, "[127.0.0.1]:%d %s %s\n", port, fields[1], fields[2])
		entries++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("slowLink: scanning the fixture's known_hosts: %v", err)
	}
	// Zero entries would authenticate nothing and fail closed later with a
	// host-key error that looks like a real refusal. Say it here instead.
	if entries == 0 {
		t.Fatalf("slowLink: the fixture's known_hosts at %s carried no usable entries, so nothing could be re-pinned", knownHostsFile)
	}

	repinned := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(repinned, []byte(out.String()), 0o600); err != nil {
		t.Fatalf("slowLink: writing the re-pinned known_hosts: %v", err)
	}
	return repinned
}
