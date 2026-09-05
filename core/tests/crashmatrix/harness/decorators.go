// This file holds every seam the crash-matrix harness uses to turn a real
// process into a real, precisely-timed crash: a journal decorator that
// self-kills the instant a target lifecycle state is durably written, and a
// transport decorator that can race a calibrated timer against a real
// network/disk call, or self-kill immediately after one succeeds. Nothing
// here touches internal/lifecycle, internal/state or internal/transport/rclone;
// it only composes their already-exported interfaces (lifecycle.Journal,
// transport.Transport) the way any external caller could.
//
// See main.go's package doc for why a real SIGKILL, not a simulated one, is
// used throughout, and what that does and does not prove.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// selfDestruct terminates this process immediately and unrecoverably via
// SIGKILL, exactly as an OOM killer or a power loss would: no deferred
// function, no finally-clause, no buffered-writer flush, nothing this
// process's own Go code could do to soften the landing gets a chance to
// run. The trailing infinite loop is a belt-and-braces guard against the
// (should not happen, but is not architecturally impossible) case where
// signal delivery to a multi-threaded Go process is not instantaneous: it
// guarantees this goroutine makes no further forward progress, in
// particular no further journal or file writes, while the kernel finishes
// tearing the process down.
func selfDestruct(reason string) {
	fmt.Fprintf(os.Stderr, "CRASHMATRIX_SELF_KILL: %s\n", reason)
	os.Stderr.Sync()
	if suppressSelfKill {
		// The one path on which selfDestruct returns. See
		// -mutation-suppress-self-kill in main.go: this exists so the
		// test suite can prove, in CI, that its own
		// "the harness must have been killed" guard still fails when the
		// kill genuinely does not happen. Every caller below is written
		// so that a suppressed run behaves like a run that was never
		// asked to die at all, rather than like a run that died and
		// carried on regardless.
		fmt.Fprintf(os.Stderr, "CRASHMATRIX_SELF_KILL_SUPPRESSED: -mutation-suppress-self-kill is set, so this process keeps running\n")
		os.Stderr.Sync()
		return
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	for {
	}
}

// suppressSelfKill turns every selfDestruct call into a no-op. It is set
// only by -mutation-suppress-self-kill and only ever by this package's own
// mutation tests; see main.go's flag doc.
var suppressSelfKill bool

// --- journal decorator: kill the instant a target state is durably written ---

// killAfterStateJournal wraps a real *state.Journal and self-destructs
// immediately after the first RecordTransition call that durably writes
// target as the new state. "Durably" is doing real work here: the
// underlying call has already returned, which for *state.Journal means the
// SQLite transaction already committed (state.Journal's own doc: the whole
// operation happens inside one transaction, so there is no half-written
// state to worry about), so by the time this decorator's kill fires, the
// write this crash point is named after has genuinely, durably happened.
//
// out.Applied guards against firing again on an idempotent replay: a
// resumed run that reaches the same key again would see Applied=false, and
// must not re-trigger the kill (there is nothing left to interrupt; the
// harness is done).
type killAfterStateJournal struct {
	real   lifecycleJournal
	target string // exact state.Transition.To value to kill after; "" disables

	// midVerify, when non-nil, arms the deterministic mid-verify crash:
	// see verifyReadHandoff. It is consulted on Get, not on
	// RecordTransition, because Get is the call lifecycle.Verify makes
	// immediately before it starts reading the local file.
	midVerify *verifyReadHandoff

	mu      sync.Mutex
	already bool
}

// lifecycleJournal is the exact surface both internal/lifecycle.Journal and
// internal/discovery.Deps.Journal need, plus ListByBackupSet for
// internal/reconcile.Journal. Depending on the narrower, local interface
// here (rather than importing internal/lifecycle.Journal) keeps this file
// from caring which package's interface literal it is structurally
// satisfying.
type lifecycleJournal interface {
	Get(ctx context.Context, id model.ArtifactID) (state.Record, error)
	RecordTransition(ctx context.Context, t state.Transition) (state.Outcome, error)
	ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]state.Record, error)
	LastEnteredAt(ctx context.Context, id model.ArtifactID, st string) (time.Time, bool, error)
	LastTransition(ctx context.Context, id model.ArtifactID, from, to string) (time.Time, bool, error)
}

func newKillAfterStateJournal(real *state.Journal, target string) *killAfterStateJournal {
	return &killAfterStateJournal{real: real, target: target}
}

func (k *killAfterStateJournal) Get(ctx context.Context, id model.ArtifactID) (state.Record, error) {
	rec, err := k.real.Get(ctx, id)
	if err == nil && k.midVerify != nil && rec.State == k.midVerify.armAtState {
		// This is the Get lifecycle.Verify makes on its own way in (see
		// verify.go: Verify reads the record itself, then decide() calls
		// readAndHashLocal(rec.LocalPath) as its very first act). Handing
		// back a record whose LocalPath is the handoff pipe is what puts
		// the real read under this harness's control; see
		// verifyReadHandoff for the whole argument.
		k.midVerify.arm(rec.LocalPath)
		rec.LocalPath = k.midVerify.pipePath
	}
	return rec, err
}

func (k *killAfterStateJournal) ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]state.Record, error) {
	return k.real.ListByBackupSet(ctx, set)
}

func (k *killAfterStateJournal) LastEnteredAt(ctx context.Context, id model.ArtifactID, st string) (time.Time, bool, error) {
	return k.real.LastEnteredAt(ctx, id, st)
}

func (k *killAfterStateJournal) LastTransition(ctx context.Context, id model.ArtifactID, from, to string) (time.Time, bool, error) {
	return k.real.LastTransition(ctx, id, from, to)
}

func (k *killAfterStateJournal) RecordTransition(ctx context.Context, t state.Transition) (state.Outcome, error) {
	out, err := k.real.RecordTransition(ctx, t)
	if err == nil && out.Applied && k.target != "" && t.To == k.target {
		k.mu.Lock()
		fired := k.already
		k.already = true
		k.mu.Unlock()
		if !fired {
			selfDestruct(fmt.Sprintf("journal durably recorded %s -> %s (key %s)", t.From, t.To, t.Key))
		}
	}
	return out, err
}

// --- transport decorator: race a calibrated timer, or kill right after a real call succeeds ---

// killPlan names one of the harness's timing-based or post-call crash
// points. See main.go for how each is chosen and calibrated.
type killPlan int

const (
	killPlanNone killPlan = iota
	killPlanMidTransfer
	killPlanMidDelete
	killPlanAfterRealDelete
)

// timedKillTransport wraps a real transport.Transport. Depending on plan,
// it either races a real call against a calibrated timer (self-destructing
// if the timer wins, meaning the call was genuinely still in flight when
// the kill happened) or lets a real call finish and self-destructs
// immediately afterward, before returning control to the caller.
type timedKillTransport struct {
	real transport.Transport

	plan     killPlan
	mid      time.Duration // armed delay for killPlanMidTransfer / killPlanMidDelete
	timedOut *bool         // set true if the race's timer fired first (mid-flight plans)
}

var _ transport.Transport = (*timedKillTransport)(nil)

func (t *timedKillTransport) List(ctx context.Context, source transport.Source) ([]transport.RemoteArtifact, error) {
	return t.real.List(ctx, source)
}

func (t *timedKillTransport) Stat(ctx context.Context, source transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	return t.real.Stat(ctx, source, remotePath)
}

func (t *timedKillTransport) CopyToLocal(ctx context.Context, source transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	if t.plan != killPlanMidTransfer {
		return t.real.CopyToLocal(ctx, source, remotePath, localPartialPath)
	}
	return raceKill("mid-transfer", t.mid, t.timedOut, func() (transport.TransferResult, error) {
		return t.real.CopyToLocal(ctx, source, remotePath, localPartialPath)
	})
}

func (t *timedKillTransport) RemoteHash(ctx context.Context, source transport.Source, remotePath string, algorithm transport.HashAlgorithm) (string, error) {
	return t.real.RemoteHash(ctx, source, remotePath, algorithm)
}

func (t *timedKillTransport) DeleteRemote(ctx context.Context, source transport.Source, remotePath string) error {
	switch t.plan {
	case killPlanMidDelete:
		_, err := raceKill("mid-delete", t.mid, t.timedOut, func() (struct{}, error) {
			return struct{}{}, t.real.DeleteRemote(ctx, source, remotePath)
		})
		return err
	case killPlanAfterRealDelete:
		if err := t.real.DeleteRemote(ctx, source, remotePath); err != nil {
			return err
		}
		// The real delete has already, genuinely succeeded on the remote by
		// this point. Killing here, before ever returning to
		// lifecycle.DeleteRemote, is exactly crash_safety.go's "the remote
		// delete may have already succeeded ... without the caller having
		// recorded that yet" window: REMOTE_DELETE_PENDING is the only
		// thing durably on record, and it always will be for this run.
		selfDestruct("real remote delete succeeded; killing before the caller could record COMPLETE")
		// Reached only under -mutation-suppress-self-kill, where the
		// honest answer is the one the real delete just gave: it
		// succeeded.
		return nil
	default:
		return t.real.DeleteRemote(ctx, source, remotePath)
	}
}

// raceKill runs fn on its own goroutine and races it against a timer of
// delay. If the timer wins, the whole process is self-destructed
// immediately: fn's underlying real syscall (a real copy, a real SFTP
// delete) is still genuinely in flight in the kernel at that instant, so
// this is a real interruption of a real operation, not a simulated one.
// The result is delivered back through resultCh purely so a raceKill call
// whose timer does NOT win still returns fn's real result to its own
// caller; nothing observes it once the timer has already won, because the
// process is gone by then.
//
// timedOut is set to true before the kill fires, purely so a caller that
// somehow observes both branches in a test build (never true in the real
// self-destructing binary, only exercised by this package's own unit test
// for raceKill's timer-vs-completion race itself) can tell which branch
// ran.
func raceKill[T any](label string, delay time.Duration, timedOut *bool, fn func() (T, error)) (T, error) {
	type result struct {
		val T
		err error
	}
	resultCh := make(chan result, 1)
	started := time.Now()
	go func() {
		v, err := fn()
		resultCh <- result{v, err}
	}()

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case r := <-resultCh:
		// The kill missed its window, and this is the only place that can
		// say by how much: the real operation's duration is knowable only
		// on the runs where it finished. Issue #248 asked for exactly
		// this, because "harness was not killed by SIGKILL (err=<nil>)"
		// reads like a signal-delivery problem. Two of the three plans
		// that still race a timer treat both sides as legitimate, so this
		// is a fact rather than a complaint; the test that does require
		// the kill quotes it back in its own failure.
		fmt.Printf("KILL_MISSED plan=%s kill_after=%s real_operation=%s\n", label, delay, time.Since(started))
		return r.val, r.err
	case <-timer.C:
		if timedOut != nil {
			*timedOut = true
		}
		selfDestruct(fmt.Sprintf("calibrated timer (%s) fired while the real operation was still in flight", delay))
		// Only reachable under -mutation-suppress-self-kill, where
		// selfDestruct returns instead of killing. Waiting for fn's real
		// result (rather than returning a zero value that the caller
		// would read as a successful call that transferred nothing) is
		// what makes a suppressed run an honest reproduction of "the
		// kill never happened".
		r := <-resultCh
		return r.val, r.err
	}
}

// --- deterministic mid-verify: hand the read its bytes, then kill it ------

// verifyReadHandoff replaces the calibrated stopwatch that used to time the
// -kill-plan=mid-verify crash with a real rendezvous, so that crash point
// stops being a race this machine can lose (issue #248).
//
// # What was wrong with timing it
//
// The old plan measured how long reading and hashing the real .partial
// takes, halved it, and hoped the SIGKILL landed inside the real read. Two
// things make that a losing bet under load. The measurement is taken cold
// and the real read that follows it runs against a warm page cache, so the
// estimate is biased high by a factor this harness cannot know; and the
// only way to lose is to fire LATE, after the read already finished, which
// is exactly the direction that bias pushes. Under gate load the
// calibration measured 114ms, fired at 45.6ms, and the process still
// reached COMPLETE first.
//
// # The rendezvous
//
// lifecycle.Verify reads its own journal record before it does anything
// else, and reads the local file at exactly the path that record carries
// (verify.go: decide() calls readAndHashLocal(rec.LocalPath) first). The
// journal is this harness's own decorator, so the path Verify reads is a
// value this harness returns. Pointing it at a FIFO turns the whole crash
// point deterministic, with no timer anywhere in it:
//
//  1. This harness creates the FIFO and, on the Get that Verify itself
//     makes, starts a writer goroutine and hands Verify the FIFO's path.
//  2. Opening a FIFO for writing blocks until a reader opens it, and
//     opening one for reading blocks until a writer does. So the writer's
//     OpenFile returning is proof that the product's own os.Open inside
//     readAndHashLocal has already happened.
//  3. The writer then streams the artifact's REAL bytes, read from the
//     real .partial file the transfer step actually wrote, into the pipe.
//     A pipe holds only a buffer's worth, so a write of handoffBytes
//     returning is proof the reader has consumed all but at most that
//     buffer: Verify is provably deep inside io.Copy, either blocked in a
//     real read(2) waiting for more or hashing the chunk it just got.
//  4. Only then does this harness SIGKILL itself.
//
// # What this does and does not prove
//
// It proves the same thing the stopwatch version was trying to: the
// process died, uncatchably, while lifecycle.Verify's own mandatory
// read-and-hash was genuinely executing, with VERIFYING the last thing
// durably journaled and nothing downstream of the kill point getting to
// run. The bytes Verify reads are the artifact's real bytes, so what is
// being hashed is real too.
//
// What it does not prove, and what the timed version did not prove either,
// is anything about reading a regular file specifically: the fd Verify
// holds is a pipe rather than the .partial itself. That substitution is
// the price of removing the race, and it is confined to which fd the read
// is against. The real .partial stays on disk, untouched, which is what
// the test then asserts about, and the resumed run after the crash reads
// it normally through the unmodified journal record on disk.
type verifyReadHandoff struct {
	// pipePath is the FIFO handed to lifecycle.Verify in place of the
	// real .partial.
	pipePath string
	// armAtState is the exact state.Record.State value that identifies
	// Verify's own Get (VERIFYING). Passed in from main.go so this file
	// keeps depending on nothing but the narrow lifecycleJournal
	// interface.
	armAtState string
	// handoffBytes is how much of the real file to hand over before
	// killing. It only has to exceed a pipe buffer (64 KiB on Linux, 16
	// KiB on Darwin) by enough that "the reader consumed all but a
	// buffer's worth" is an unambiguous statement about being inside the
	// read, and it must stay below the artifact size so the stream never
	// reaches EOF on its own.
	handoffBytes int64

	// progress reports each rendezvous step to the parent, both as
	// evidence in the test log and as the liveness signal the parent's
	// watchdog measures (see crash_matrix_test.go's progressTracker).
	progress func(event string)

	once sync.Once
}

// arm starts the writer side exactly once, against the real local path the
// journal actually holds. Get calls this before it overwrites LocalPath,
// so realPath here is the genuine .partial, not the pipe.
func (h *verifyReadHandoff) arm(realPath string) {
	h.once.Do(func() { go h.serve(realPath) })
}

// serve is the writer side of the rendezvous. Every failure in here is
// fatal to the harness on purpose: a mid-verify run that silently degrades
// into "no kill happened" is precisely the outcome issue #248 is about, so
// it must be loud and it must be attributed to this mechanism rather than
// surfacing later as an unexplained clean exit.
func (h *verifyReadHandoff) serve(realPath string) {
	src, err := os.Open(realPath)
	if err != nil {
		fatalf("mid-verify handoff: opening the real local file %q to stream it: %v", realPath, err)
	}
	defer src.Close()

	h.progress("verify-handoff-waiting-for-reader")
	// Blocks until lifecycle.Verify's own os.Open of the same FIFO runs.
	// This returning IS the synchronisation point.
	w, err := os.OpenFile(h.pipePath, os.O_WRONLY, 0)
	if err != nil {
		fatalf("mid-verify handoff: opening the write end of %q: %v", h.pipePath, err)
	}
	h.progress("verify-handoff-reader-attached")

	n, err := io.CopyN(w, src, h.handoffBytes)
	if err != nil {
		fatalf("mid-verify handoff: streaming the first %d bytes (got %d): %v", h.handoffBytes, n, err)
	}
	fmt.Printf("HANDOFF verify_read_bytes=%d\n", n)
	h.progress("verify-handoff-complete")

	selfDestruct(fmt.Sprintf("lifecycle.Verify has consumed %d bytes of the real local file through the handoff pipe, so its read is provably still in flight", n))

	// Only reachable under -mutation-suppress-self-kill. Streaming the
	// rest and closing lets Verify's read complete normally and the whole
	// pipeline run on to COMPLETE, which is exactly the shape of the
	// failure this suite has to keep detecting.
	if _, err := io.Copy(w, src); err != nil {
		fatalf("mid-verify handoff (suppressed): streaming the remainder: %v", err)
	}
	_ = w.Close()
}
