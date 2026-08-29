package main

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

import (
	"context"
	"fmt"
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
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	for {
	}
}

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
}

func newKillAfterStateJournal(real *state.Journal, target string) *killAfterStateJournal {
	return &killAfterStateJournal{real: real, target: target}
}

func (k *killAfterStateJournal) Get(ctx context.Context, id model.ArtifactID) (state.Record, error) {
	return k.real.Get(ctx, id)
}

func (k *killAfterStateJournal) ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]state.Record, error) {
	return k.real.ListByBackupSet(ctx, set)
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
	return raceKill(t.mid, t.timedOut, func() (transport.TransferResult, error) {
		return t.real.CopyToLocal(ctx, source, remotePath, localPartialPath)
	})
}

func (t *timedKillTransport) RemoteHash(ctx context.Context, source transport.Source, remotePath string, algorithm transport.HashAlgorithm) (string, error) {
	return t.real.RemoteHash(ctx, source, remotePath, algorithm)
}

func (t *timedKillTransport) DeleteRemote(ctx context.Context, source transport.Source, remotePath string) error {
	switch t.plan {
	case killPlanMidDelete:
		_, err := raceKill(t.mid, t.timedOut, func() (struct{}, error) {
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
		return nil // unreachable
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
func raceKill[T any](delay time.Duration, timedOut *bool, fn func() (T, error)) (T, error) {
	type result struct {
		val T
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		v, err := fn()
		resultCh <- result{v, err}
	}()

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case r := <-resultCh:
		return r.val, r.err
	case <-timer.C:
		if timedOut != nil {
			*timedOut = true
		}
		selfDestruct(fmt.Sprintf("calibrated timer (%s) fired while the real operation was still in flight", delay))
		var zero T
		return zero, nil // unreachable
	}
}
