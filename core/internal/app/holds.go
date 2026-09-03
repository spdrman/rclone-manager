package app

import (
	"context"
	"errors"
	"sync/atomic"
)

// BackupSetHolds answers, for a cycle in flight, which backup sets must
// not be processed right now (issue #350).
//
// # What a hold is for
//
// A backup set being edited while a cycle is running against it is two
// writers on one definition: the cycle is mid-transfer against the old
// remote path while an operator is changing it, and whichever finishes
// last wins silently. So entering edit mode takes a hold, and a hold does
// two things to a cycle rather than one. RunCycle will not START a pass
// over a held set, which is what keeps a scheduled tick from beginning a
// run against a half-edited set moments after the previous one stopped,
// and a hold that lands while a pass is ALREADY inside that set cancels
// that set's own context, which stops the pass where it stands.
//
// Cancelling rather than waiting is the point. Whatever a cancelled pass
// was in the middle of is safe to leave: processArtifact re-checks its
// context immediately before every step (see its own doc), so an
// interrupted artifact stays at whatever pre-durable state it had reached
// and is picked up by a later cycle, never presented as a committed
// artifact that was never fully written.
//
// # Why an interface on the context, not a field on Service
//
// Exactly the reason WithProgressObserver gives for the observer beside
// it: a Service is shared by every caller and rebuilt on every
// configuration hot-reload, while the registry that owns holds outlives
// both. Attaching it to the call's own context scopes it to the cycle
// that asked for it, and means core/service can own the registry (with
// the lease and expiry policy that belongs at that layer) without this
// package importing it, which the dependency direction forbids anyway.
//
// A cycle whose context carries no registry behaves exactly as it did
// before this existed: nothing is held, nothing is watched, and no
// goroutine is started.
type BackupSetHolds interface {
	// Held reports whether setID (a model.BackupSetID.String(), so
	// "source/name") is currently held. It is called from the cycle's own
	// goroutine before each set, and from the watcher goroutine below, so
	// an implementation must be safe for concurrent use.
	Held(setID string) bool

	// Changed returns a channel that is closed the next time ANY hold is
	// placed. A caller re-reads it after each close, which is the
	// ordinary broadcast idiom; one channel for all sets rather than one
	// per set keeps an implementation from having to track which sets a
	// cycle happens to be interested in.
	//
	// It deliberately promises nothing about releases. Nothing needs to
	// wake up when a hold is lifted: RunCycle re-reads Held at the top of
	// every set on every pass, so a released set is picked up by the next
	// tick on its own.
	Changed() <-chan struct{}
}

// ErrBackupSetHeldForEditing is BackupSetCycleResult.Err for a pass this
// cycle stopped because a hold landed on that set, and it exists so a
// stopped pass is not spelled the same way a failed one is.
//
// Before it, the cancellation surfaced as a bare context.Canceled, which
// every consumer of a cycle report reads as a failure: the operation an
// operator submitted was marked failed with "context canceled" as its
// reason, `backup-manager run` exited 1, and the activity feed said a
// backup had gone wrong. Nothing had. See
// BackupSetCycleResult.SystemicFailure, which is what callers should ask
// rather than comparing against this directly.
var ErrBackupSetHeldForEditing = errors.New("app: this backup set's pass was stopped because it is held for editing")

type backupSetHoldsKey struct{}

// WithBackupSetHolds returns a context that asks RunCycle to consult h
// before, and while, processing each backup set. A nil h is the same as
// no registry.
func WithBackupSetHolds(ctx context.Context, h BackupSetHolds) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, backupSetHoldsKey{}, h)
}

// BackupSetHoldsFrom returns the registry WithBackupSetHolds put on ctx,
// or nil when there is none. Exported for the same reason
// ProgressObserverFrom is: the package that installs one has to be able
// to prove it actually installed it, rather than only assert that it
// called the setter.
func BackupSetHoldsFrom(ctx context.Context) BackupSetHolds {
	h, _ := ctx.Value(backupSetHoldsKey{}).(BackupSetHolds)
	return h
}

// withHoldCancellation returns a context that is cancelled as soon as a
// hold lands on setID, plus the cancel func the caller must defer and a
// predicate reporting whether it was THIS watcher that cancelled. When
// ctx carries no holds registry it returns ctx, a no-op and a predicate
// that is always false, so a cycle nobody is holding starts no goroutine
// at all.
//
// The predicate is what lets the caller tell a pass an operator stopped
// apart from a pass the parent context ended (a shutdown, a deadline).
// Both leave the same context.Canceled behind, and they mean opposite
// things to anyone reading the cycle report afterwards.
func withHoldCancellation(ctx context.Context, setID string) (context.Context, context.CancelFunc, func() bool) {
	holds := BackupSetHoldsFrom(ctx)
	if holds == nil {
		return ctx, func() {}, func() bool { return false }
	}
	setCtx, cancel := context.WithCancel(ctx)
	var fired atomic.Bool
	go watchForHold(setCtx, holds, setID, &fired, cancel)
	return setCtx, cancel, fired.Load
}

// watchForHold cancels setCtx the moment setID becomes held, and returns
// when setCtx is done otherwise, so it cannot outlive the set's own pass.
//
// Changed() is read BEFORE Held() on every iteration, and that ordering
// is the whole correctness argument: read the other way round, a hold
// placed between the check and the wait would close a channel this
// goroutine is not yet listening on, and the cancellation would be lost
// until some unrelated later hold happened to wake it.
//
// fired is set BEFORE cancel, never after, so any goroutine that observes
// the cancellation also observes the reason for it. The other order would
// leave a window in which the pass has stopped and the report cannot yet
// say why, which is exactly the window a cycle unwinding through its own
// ctx.Err() checks runs in.
func watchForHold(setCtx context.Context, holds BackupSetHolds, setID string, fired *atomic.Bool, cancel context.CancelFunc) {
	for {
		changed := holds.Changed()
		if holds.Held(setID) {
			fired.Store(true)
			cancel()
			return
		}
		select {
		case <-setCtx.Done():
			return
		case <-changed:
		}
	}
}
