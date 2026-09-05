package rclone

import (
	"context"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"

	"github.com/spdrman/rclone-manager/core/tests/sftpfixture"
)

// handshakeSamples is how many real connects the measurement below makes.
// One sample is a number the scheduler picked as much as the network did;
// ten of them, worst taken, is a statement about the host.
const handshakeSamples = 6

// handshakeHeadroom is how many times the slowest connect measured on this
// host the ConnectTimeout ceiling has to be.
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
// ConnectTimeout's derivation (issue #415).
//
// The other half, the ceiling, is arithmetic: six attempts have to fit
// inside the budget app.DefaultRetryPolicy's doc claims, and
// app/retrybudget_test.go pins it. Arithmetic alone can only ever push a
// timeout down, though, and a connect timeout pushed far enough down stops
// being a bound on a failure and becomes a cause of one. This is the floor
// under it, and it is measured rather than asserted: the numbers below come
// from this host, dialling a real sshd through the same fsFor every
// operation in this adapter uses, on the run you are reading.
func TestConnectTimeoutLeavesARealHandshakeRoom(t *testing.T) {
	f := sftpfixture.Start(t)
	a := New()
	src := f.Source("connect-timeout-headroom", "")

	var worst, total time.Duration
	for i := 1; i <= handshakeSamples; i++ {
		// oneConnectionAtATime first, so what is measured is the connect
		// a production operation performs and not a differently
		// configured one. fsFor is where the TCP dial, the SSH key
		// exchange and the authentication all happen; ConnectTimeout is
		// the deadline over that whole step.
		ctx := oneConnectionAtATime(context.Background())
		start := time.Now()
		fsys, err := a.fsFor(ctx, src)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("connect %d of %d against the fixture failed, so there is nothing to measure: %v", i, handshakeSamples, err)
		}
		shutdownFs(ctx, fsys)

		total += elapsed
		if elapsed > worst {
			worst = elapsed
		}
	}

	mean := total / handshakeSamples
	t.Logf("%d real connects through fsFor against the fixture: worst %s, mean %s; the %s ceiling is %.0fx the worst",
		handshakeSamples, worst.Round(time.Millisecond), mean.Round(time.Millisecond),
		ConnectTimeout, float64(ConnectTimeout)/float64(worst))

	// A measurement of zero is not a fast host, it is a broken clock or a
	// connect that never happened, and either one would make the ratio
	// below infinite and this test vacuous.
	if worst <= 0 {
		t.Fatalf("the slowest of %d connects measured %s, which is not a measurement; this row cannot say anything about headroom", handshakeSamples, worst)
	}

	if need := time.Duration(handshakeHeadroom) * worst; ConnectTimeout < need {
		t.Fatalf("ConnectTimeout is %s, under the %s that is %dx the slowest real connect this host performed (%s).\n"+
			"Two things look like this, and they want opposite fixes.\n"+
			"  If ConnectTimeout was shortened: that ceiling can be lowered to keep app.DefaultRetryPolicy's budget "+
			"honest (#415) right up to the point where it starts refusing connects that would have succeeded, and "+
			"this is that point. If the budget genuinely has to shrink further, take it out of MaxAttempts instead: "+
			"fewer attempts costs a retry, a shorter connect timeout costs the source.\n"+
			"  If ConnectTimeout is still %s: a loopback SSH handshake to a container took %s, and nothing about a "+
			"healthy machine explains that. Re-run this on a host that is not also running another gate.",
			ConnectTimeout, need, handshakeHeadroom, worst.Round(time.Millisecond), ConnectTimeout, worst.Round(time.Millisecond))
	}
}

// TestAStalledSourceIsBoundedByADifferentNumber is the honesty guard on
// app.DefaultRetryPolicy's budget (issue #415).
//
// That budget is "six attempts, at most ConnectTimeout each", and it is
// true of a source that never answers, which is what an operator means when
// they say a NAS is off. It is NOT true of every failure. ConnectTimeout
// bounds a dial; a source that answers, accepts the session and then goes
// quiet partway through a read is bounded by rclone's --timeout, an idle
// timeout on the transfer, which this adapter deliberately does not touch.
//
// So there are two numbers, they are far apart, and the way #415 comes back
// is somebody reading "six times ConnectTimeout" as the whole story. This
// row fails if they ever become one number, in either direction: by the
// idle timeout being pulled down to the connect timeout (which would start
// failing slow-but-live links), or by this adapter starting to override it
// at all (which would move a bound app's doc describes without app's doc
// knowing).
func TestAStalledSourceIsBoundedByADifferentNumber(t *testing.T) {
	rcloneDefault := fs.GetConfig(context.Background())
	ours := fs.GetConfig(oneConnectionAtATime(context.Background()))

	idle := time.Duration(ours.Timeout)
	connect := time.Duration(ours.ConnectTimeout)
	t.Logf("a dial is bounded by ConnectTimeout (%s, ours); a stalled transfer by --timeout (%s, rclone's own default, untouched)",
		connect, idle)

	// This adapter bounds one of them and not the other, on purpose.
	if idle != time.Duration(rcloneDefault.Timeout) {
		t.Errorf("this adapter now sets rclone's --timeout to %s, against the %s default it used to leave alone. "+
			"app.DefaultRetryPolicy's doc describes the stalled-source worst case in terms of that default and would "+
			"now be wrong; and --timeout is a bound on a transfer making no progress, so shortening it fails "+
			"slow-but-live links rather than unreachable ones",
			idle, time.Duration(rcloneDefault.Timeout))
	}

	// And they have to stay recognisably different, or "six attempts at
	// ConnectTimeout each" would quietly start reading as the whole budget.
	if idle <= connect {
		t.Errorf("--timeout is %s, at or under the %s connect timeout, so the two bounds have collapsed into one. "+
			"app.DefaultRetryPolicy's doc has a whole section explaining that they are different failures with "+
			"different costs; if that is genuinely no longer true, that section is what has to change",
			idle, connect)
	}
}
