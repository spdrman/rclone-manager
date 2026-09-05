package sftpintegration_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/sftpfixture"
)

// handshakeSamples is how many real connects the measurement below makes.
// One sample is a number the scheduler picked as much as the network did;
// several of them, worst taken, is a statement about the host.
const handshakeSamples = 6

// handshakeHeadroom is how many times the slowest connect measured on this
// host the rclone.ConnectTimeout ceiling has to be.
//
// It is a ratio rather than a duration because the thing it guards against
// is a direction, not a number: #415's ceiling exists to make the retry
// budget honest, and the way that goes wrong next is somebody shortening it
// further to make the budget look better still, until a legitimately slow
// source stops being able to connect at all. A ratio keeps saying that on a
// fast machine and on a loaded one.
//
// Five, and deliberately loose, because the SHARP guard on this number is
// somewhere else: app/retrybudget_test.go pins the budget, so ConnectTimeout
// cannot move by a single second in either direction without going red and
// naming the doc that has to move with it. What is left for this row is the
// question arithmetic cannot answer, "is the number one a real connect fits
// inside", and a tight ratio here would answer it by flaking. This fixture
// is a container on a Docker VM with four CPUs; its connect is already
// mostly overhead a real NAS on a LAN does not pay, and under a parallel
// gate run that overhead is what grows. At five the row only reddens once a
// loopback SSH handshake needs three seconds, which is a statement about the
// machine rather than about the ceiling, and the message below says so.
const handshakeHeadroom = 5

// TestConnectTimeoutLeavesARealHandshakeRoom is the measured half of
// rclone.ConnectTimeout's derivation (issue #415).
//
// The other half, the ceiling, is arithmetic: six attempts have to fit
// inside the budget app.DefaultRetryPolicy's doc claims, and
// app/retrybudget_test.go pins it. Arithmetic alone can only ever push a
// timeout down, though, and a connect timeout pushed far enough down stops
// being a bound on a failure and becomes a cause of one. This is the floor
// under it, and it is measured rather than asserted: the numbers below come
// from this host, dialling a real sshd through the adapter's own public API,
// on the run you are reading.
//
// # What is being timed
//
// One List against an empty directory. That is a connect (TCP dial, SSH key
// exchange, authentication) plus a single LIST round trip plus the pool
// shutdown, because List is the cheapest exported call that performs a full
// connect and this package is deliberately outside transport/rclone and
// cannot reach fsFor directly. Timing slightly MORE than the connect is the
// safe direction for a floor: it can only make this row stricter, never
// laxer, and on loopback the extra round trip is noise beside a handshake.
func TestConnectTimeoutLeavesARealHandshakeRoom(t *testing.T) {
	f := sftpfixture.Start(t)
	a := rclone.New()

	// An empty root, so the LIST that rides along with the connect has
	// nothing to enumerate and the measurement stays as close to the
	// handshake as an exported call can get.
	const root = "connect-timeout-headroom"
	if err := os.MkdirAll(filepath.Join(f.UploadDir, root), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	src := f.Source(root, root)

	var worst, total time.Duration
	for i := 1; i <= handshakeSamples; i++ {
		start := time.Now()
		got, err := a.List(context.Background(), src)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("connect %d of %d against the fixture failed, so there is nothing to measure: %v", i, handshakeSamples, err)
		}
		if len(got) != 0 {
			t.Fatalf("the measurement root %q is not empty (%d entries), so what was timed includes enumerating it", root, len(got))
		}

		total += elapsed
		if elapsed > worst {
			worst = elapsed
		}
	}

	mean := total / handshakeSamples
	t.Logf("%d real connects through Adapter.List against the fixture: worst %s, mean %s; the %s ceiling is %.0fx the worst",
		handshakeSamples, worst.Round(time.Millisecond), mean.Round(time.Millisecond),
		rclone.ConnectTimeout, float64(rclone.ConnectTimeout)/float64(worst))

	// A measurement of zero is not a fast host, it is a broken clock or a
	// connect that never happened, and either one would make the ratio
	// below infinite and this test vacuous.
	if worst <= 0 {
		t.Fatalf("the slowest of %d connects measured %s, which is not a measurement; this row cannot say anything about headroom", handshakeSamples, worst)
	}

	if need := time.Duration(handshakeHeadroom) * worst; rclone.ConnectTimeout < need {
		t.Fatalf("ConnectTimeout is %s, under the %s that is %dx the slowest real connect this host performed (%s).\n"+
			"Two things look like this, and they want opposite fixes.\n"+
			"  If ConnectTimeout was shortened: that ceiling can be lowered to keep app.DefaultRetryPolicy's budget "+
			"honest (#415) right up to the point where it starts refusing connects that would have succeeded, and "+
			"this is that point. If the budget genuinely has to shrink further, take it out of MaxAttempts instead: "+
			"fewer attempts costs a retry, a shorter connect timeout costs the source.\n"+
			"  If ConnectTimeout is still %s: a loopback SSH handshake to a container took %s, and nothing about a "+
			"healthy machine explains that. Re-run this on a host that is not also running another gate.",
			rclone.ConnectTimeout, need, handshakeHeadroom, worst.Round(time.Millisecond), rclone.ConnectTimeout, worst.Round(time.Millisecond))
	}
}
