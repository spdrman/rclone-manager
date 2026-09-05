package sftpintegration_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/asyncreader"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/sftpfixture"
)

// The numbers TestSFTPAdapterCancelsAMidFlightTransfer is built from. They
// are together, and documented, because the way a cancellation test stops
// working is not a broken assertion, it is a constant quietly moved past
// the point where the assertion can discriminate.
// TestMidTransferCancellationCanStillFail is what says so out loud.
const (
	// cancelLinkRate is what the slow link in front of the fixture is worth.
	// Fast enough that the row costs the gate seconds rather than minutes,
	// slow enough that the payload below spans many progress samples at
	// whatever interval the adapter samples at. That interval is the
	// adapter's own unexported default, so this row does not name it: it
	// counts the samples it actually saw instead, which is the property the
	// number was standing in for anyway.
	cancelLinkRate = 1024 * 1024

	// cancelPayload has to be comfortably larger than everything that can
	// still move after a cancel lands, or "it stopped short" stops being
	// distinguishable from "it very nearly finished". Sixteen MiB against a
	// ceiling of about four is the margin, and the guard test checks it
	// rather than trusting this sentence.
	cancelPayload = 16 * 1024 * 1024

	// cancelTrigger is how much of the payload has to have provably moved
	// before the cancel fires. It is what makes this MID-transfer: the
	// cancel is fired by the progress reporter, so the transfer being under
	// way is a precondition of the cancellation happening at all rather
	// than something a timer hoped for.
	cancelTrigger = cancelPayload / 8

	// minControlSamples is how many progress readings the uncancelled
	// control has to produce. It is the runtime replacement for an
	// arithmetic claim about sampling windows: the thing that actually has
	// to be true is that the transfer is observable often enough for the
	// trigger to fire near the byte count that fired it, and counting real
	// samples says that about the run in front of you rather than about a
	// constant somebody wrote down.
	minControlSamples = 20

	// postCancelAllowance is how many bytes may still be accounted after the
	// cancel. rclone checks the context once per chunk it hands the copy
	// loop and a chunk is at most asyncreader.BufferSize, so one chunk can
	// be in flight when the cancel lands and at most one more can be handed
	// over between the sample that fired it and it taking effect. Measured
	// on this fixture the overshoot was zero.
	postCancelAllowance = 2 * asyncreader.BufferSize
)

// cancellationIgnored is the sentence every failure in that row ends with,
// because they all mean the same thing and it is worth saying in the words
// an operator would use.
const cancellationIgnored = "a cancelled backup that keeps reading is FR-22's cancellation not propagating through the transport"

// TestMidTransferCancellationCanStillFail guards the constants above rather
// than any behaviour, the same way transport/rclone's
// TestKeyCommandTimeoutBudget_CanStillFail guards keysource_test.go's. A
// bound that has been widened past the defect it is about reports on it by
// sitting green through it.
func TestMidTransferCancellationCanStillFail(t *testing.T) {
	// An adapter that ignored the cancellation entirely would move the
	// whole payload. The worst case the row tolerates is the trigger
	// overshooting by a chunk and then the allowance on top, so that total
	// has to stay well clear of the payload.
	worstTolerated := int64(cancelTrigger) + int64(asyncreader.BufferSize) + int64(postCancelAllowance)
	if worstTolerated >= cancelPayload/2 {
		t.Errorf("the row tolerates a byte count as high as %d, against a %d payload. At this margin a copy that "+
			"barely noticed the cancellation passes, and one that ignored it needs only to be a little slow to pass too",
			worstTolerated, int64(cancelPayload))
	}

	// And the trigger has to be reachable, or the row fails every time for
	// a reason that has nothing to do with cancellation.
	if cancelTrigger >= cancelPayload {
		t.Errorf("the cancel fires at %d bytes, which a %d payload never reaches", int64(cancelTrigger), int64(cancelPayload))
	}

	// The trigger also has to leave room for readings BEFORE it, or "the
	// cancel fired while bytes were moving" would be true of the very first
	// sample and would stop meaning anything.
	if cancelTrigger > cancelPayload/4 {
		t.Errorf("the cancel fires at %d of a %d payload, so late that most of the transfer is already done when it lands",
			int64(cancelTrigger), int64(cancelPayload))
	}
}

// TestSFTPAdapterCancelsAMidFlightTransfer is issue #414.
//
// What it proves, and what it deliberately does not, both need saying,
// because the version before it claimed the first while only ever
// demonstrating the second.
//
// # What it proves
//
// That a context cancelled while a real SFTP transfer is genuinely in
// flight stops that transfer. The evidence is the bytes: the copy's own
// progress is watched while it runs, the cancel is fired BY that watcher
// the moment it has seen a meaningful amount of the payload actually move,
// and the count then stops within one buffered chunk and never reaches the
// total. The uncancelled control immediately above it does the same copy
// over the same throttled link and moves every byte, so "it stopped short"
// is a statement about the cancellation and not about the fixture.
//
// # Why the observable is bytes and not elapsed time
//
// Because on sftp the call does not return when the transfer stops. The
// adapter's sftpConfig pins concurrency at 64 and chunk_size at 32Ki, so
// about 2MiB of read requests are outstanding at any moment, and the server
// answers all of them whatever the client has decided. The copy loop stops
// at the cancel; the socket then drains that window at whatever the link is
// worth. Measured here: the byte count froze at the cancel, and the call
// returned a couple of seconds later. A test that timed the call would be
// timing the drain, so the elapsed times below are logged and never
// asserted.
//
// That is also why this uses a slow LINK rather than rclone's --bwlimit.
// The old version of this row set --bwlimit to fmt.Sprintf("%d", 64*1024),
// and rclone reads a bare 65536 as 64Mi rather than 64Ki, so the throttle
// never did anything and what made the assertion pass was Docker loopback
// being slower than a 150ms timer. Correcting the unit did not fix it, it
// exposed the real problem: at a real 64KiB/s the copy took 16.1s of the
// 16s an uncancelled one needed, because --bwlimit throttles inside rclone
// with WaitN(context.Background(), n) and the 1MiB payload was one drain.
// See slowLink's own doc.
//
// # What it does not prove
//
// That a cancelled sftp copy RETURNS promptly. It does not, on a slow link,
// and the number is bounded by the in-flight window over the link speed
// rather than by anything this repository controls.
func TestSFTPAdapterCancelsAMidFlightTransfer(t *testing.T) {
	f := sftpfixture.Start(t)
	a := rclone.New()

	link := startSlowLink(t, f, cancelLinkRate)
	linkSrc := link.Source("adapter-cancel-slow", "")

	// Random rather than zeroes, so nothing between here and the relay can
	// make the payload cheaper than its size and leave the link delivering
	// fewer bytes than the rate implies.
	content := make([]byte, cancelPayload)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("generate content: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.UploadDir, "cancel-source.bin"), content, 0o644); err != nil {
		t.Fatalf("seed cancel-source.bin: %v", err)
	}

	// The control, first: this copy, this link, no cancellation. Without it
	// "the count stopped short of the payload" has a second explanation (the
	// link never delivers the payload at all) that is indistinguishable from
	// the one being claimed.
	//
	// It is also where the sampling rate is measured. The adapter's own
	// sample interval is unexported and this package cannot set it, so
	// instead of asserting arithmetic about a number it cannot see, the
	// control counts the readings it really got.
	var controlSamples int
	controlReport := transport.ProgressReporterFunc(func(transport.ByteProgress) { controlSamples++ })
	controlStart := time.Now()
	control, err := a.CopyToLocal(
		transport.WithProgressReporter(context.Background(), controlReport),
		linkSrc, "cancel-source.bin", filepath.Join(t.TempDir(), "control.bin"))
	uncancelled := time.Since(controlStart)
	if err != nil {
		t.Fatalf("the uncancelled control copy over the slow link failed, so nothing below can be attributed to a cancellation: %v", err)
	}
	if control.BytesTransferred != cancelPayload {
		t.Fatalf("the uncancelled control moved %d of %d bytes; the link does not deliver the whole payload, so a short count later proves nothing",
			control.BytesTransferred, int64(cancelPayload))
	}
	t.Logf("control: the same copy over the same link, uncancelled, moved all %d bytes in %v across %d progress samples",
		int64(cancelPayload), uncancelled.Round(time.Millisecond), controlSamples)

	if controlSamples < minControlSamples {
		t.Fatalf("the control copy produced only %d progress samples over %v, under the %d this row needs. "+
			"The cancel below fires from a sample, so at this resolution it would land wherever the first reading past "+
			"%d bytes happened to be rather than near it. Either the link (%d B/s) is too fast for the payload (%d bytes), "+
			"or the adapter's progress sampling interval has grown",
			controlSamples, uncancelled.Round(time.Millisecond), minControlSamples,
			int64(cancelTrigger), int64(cancelLinkRate), int64(cancelPayload))
	}

	// Now the same copy, cancelled by its own progress.
	//
	// Firing on an observed byte count rather than on a timer is the whole
	// difference between this row and the one it replaces. A timer cancels
	// at a moment and hopes the transfer had started; measured on this
	// fixture, an SFTP connect plus rclone's own pre-transfer work is a
	// couple of seconds before the first byte is ever accounted, so a timer
	// short enough to be "mid" a transfer usually is not. This one cannot
	// fire before the transfer is under way, because what fires it IS the
	// transfer.
	var (
		mu           sync.Mutex
		peak         int64
		atCancel     int64
		samples      int
		cancelledAt  time.Duration
		firstBytesAt time.Duration
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	report := transport.ProgressReporterFunc(func(p transport.ByteProgress) {
		mu.Lock()
		samples++
		if p.BytesTransferred > peak {
			peak = p.BytesTransferred
		}
		if p.BytesTransferred > 0 && firstBytesAt == 0 {
			firstBytesAt = time.Since(start)
		}
		fire := p.BytesTransferred >= cancelTrigger && atCancel == 0
		if fire {
			atCancel = p.BytesTransferred
			cancelledAt = time.Since(start)
		}
		mu.Unlock()
		if fire {
			cancel()
		}
	})

	localPath := filepath.Join(t.TempDir(), "cancel-source.bin.partial")
	_, err = a.CopyToLocal(transport.WithProgressReporter(ctx, report), linkSrc, "cancel-source.bin", localPath)
	elapsed := time.Since(start)

	mu.Lock()
	defer mu.Unlock()

	t.Logf("cancelled copy: %d progress samples, first bytes at %v, cancelled at %v with %d of %d bytes moved, peak %d, call returned after %v",
		samples, firstBytesAt.Round(time.Millisecond), cancelledAt.Round(time.Millisecond),
		atCancel, int64(cancelPayload), peak, elapsed.Round(time.Millisecond))

	// The cancel has to have fired at all, and it has to have fired while
	// bytes were moving. This is the "mid-transfer" in the name, and it is
	// an observation rather than an assumption.
	if atCancel == 0 {
		t.Fatalf("the transfer never reached %d accounted bytes, so nothing here cancelled a transfer in flight "+
			"(peak %d of %d over %v, %d samples); the copy either failed early or never started",
			int64(cancelTrigger), peak, int64(cancelPayload), elapsed, samples)
	}

	if err == nil {
		t.Fatalf("the copy completed successfully after being cancelled with %d of %d bytes moved; "+
			cancellationIgnored, atCancel, int64(cancelPayload))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the copy failed with %v (%T), which does not wrap context.Canceled; "+
			"it stopped for some reason other than the cancellation, so this row says nothing about cancellation", err, err)
	}

	// And it has to have STOPPED, which is the other half and the one a
	// returned error alone does not give: an adapter that ran the copy to
	// completion and only then noticed the context would return exactly
	// this error.
	//
	// The allowance is derived from what rclone can have in hand when the
	// cancel lands. Account checks the context once per chunk it hands the
	// copy loop, and a chunk is at most asyncreader.BufferSize; one such
	// chunk can be mid-flight when the cancel arrives, and at most one more
	// can be handed over between the sample that fired the cancel and the
	// cancel taking effect. Measured on this fixture the overshoot was zero
	// bytes; two chunks is the ceiling, not the expectation.
	if limit := atCancel + postCancelAllowance; peak > limit {
		t.Fatalf("the byte count went on to %d after a cancel fired at %d, past the %d two of rclone's own %d-byte "+
			"buffered chunks allow. The copy kept reading after the cancellation: %s",
			peak, atCancel, limit, int64(asyncreader.BufferSize), cancellationIgnored)
	}
	if peak >= cancelPayload {
		t.Fatalf("the byte count reached %d of %d, so the whole payload moved despite the cancellation: %s",
			peak, int64(cancelPayload), cancellationIgnored)
	}

	// Logged, never asserted: see this row's doc. The gap between the
	// transfer stopping and the call returning is the in-flight SFTP window
	// draining off a deliberately slow link, and it is a property of the
	// protocol's own read-ahead rather than of anything cancellation could
	// reach. The window is stated as what the drain actually cost at the
	// link's known rate, which is a measurement of this run, rather than as
	// a restatement of the adapter's configuration.
	drain := elapsed - cancelledAt
	t.Logf("the transfer stopped at %v and CopyToLocal returned at %v: %v of that is about %d bytes of outstanding "+
		"SFTP reads (concurrency x chunk_size) draining off a %d B/s link. An uncancelled copy needed %v.",
		cancelledAt.Round(time.Millisecond), elapsed.Round(time.Millisecond), drain.Round(time.Millisecond),
		int64(drain.Seconds()*float64(cancelLinkRate)), int64(cancelLinkRate), uncancelled.Round(time.Millisecond))

	if info, statErr := os.Stat(localPath); statErr == nil {
		t.Logf("partial local file left behind: %d of %d bytes", info.Size(), int64(cancelPayload))
		if info.Size() >= cancelPayload {
			t.Fatalf("local file is fully sized (%d bytes) after a cancelled transfer; not actually interrupted", info.Size())
		}
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected error stat-ing local partial file: %v", statErr)
	} else {
		t.Log("local backend removed the partially-written file on cancellation (backend/local's own cleanup-on-error path)")
	}
}
