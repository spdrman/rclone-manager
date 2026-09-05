// This file is the volatile half of the operation model, and everything
// about it follows from that word.
//
// operations.go keeps the durable half: an operation was submitted,
// started, finished or failed, and each of those has to survive the
// process dying. Live progress is the opposite kind of fact. It changes
// several times a second, it stops meaning anything the instant the cycle
// producing it stops, and the last value it held is not merely stale
// after a restart, it actively describes a transfer that no longer exists
// as though it were still moving. So nothing in this file is written
// anywhere, and an operation left running by a process that died reports
// no progress at all rather than the last number it managed to take.
//
// That is also why the readings are separated from the operation record
// by a registry rather than kept on it. An entry exists for exactly as
// long as a goroutine in THIS process is executing that operation, which
// makes "is this reading live" a structural question instead of a
// judgement about timestamps.
//
// The other constraint is that the write side runs on the transport's own
// sampling path, alongside the copy it is measuring. Every operation here
// is a lock, a handful of field assignments and a return: observing a
// transfer must never be a reason the transfer is slower.
package service

import (
	"sync"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// OperationProgress is this package's plain, provider-agnostic reading of
// one RUNNING operation's live progress (docs/EPIC-B-multi-nas.md §52).
//
// # Why this is not on the operation row
//
// An operation record is durable and crash-safe: it says a run cycle was
// submitted, started and finished, and every one of those is a fact that
// must survive the process dying. Progress is the opposite kind of thing.
// It changes several times a second, it is meaningless once the cycle it
// describes has stopped, and after a restart the last value it held is not
// stale data, it is a lie: it would describe a transfer that no longer
// exists as though it were still moving.
//
// Writing it to the operations table would therefore buy nothing and cost
// two things that matter. It would put a tick-rate write path on the one
// table whose durability guarantees exist precisely so that the few writes
// it does take are trustworthy, and it would leave the last reading behind
// on disk for the next process to serve as live.
//
// So it lives in memory, in liveProgress below, keyed by operation id, for
// exactly as long as the goroutine executing that operation is running in
// THIS process, and it is served only alongside an operation whose durable
// status is "running". Everything else, including an operation that was
// running when the process died, reports no progress at all. See
// Operation.Progress for what a client does with that.
type OperationProgress struct {
	// ObservedAt is when this reading was taken, so a client can tell a
	// transfer that is not moving from a service that has stopped
	// sampling.
	ObservedAt time.Time

	// Sequence increments on every reading for this operation, starting
	// at 1. §52's "last update sequence"; it is what lets a client see
	// that the service is still producing readings even when the numbers
	// in them have not changed.
	Sequence int64

	// Stage is one of internal/app's stage constants.
	Stage string

	BackupSetID     string
	BackupSetsDone  int
	BackupSetsTotal int

	Artifact      string
	ArtifactsDone int

	// BytesTransferred, BytesTotal and BytesPerSecond describe the single
	// artifact named by Artifact, never the whole cycle, and are nil
	// whenever no copy is in flight and reporting. Nil is "not being
	// measured"; a non-nil zero is "measured, and nothing yet".
	BytesTransferred *int64
	BytesTotal       *int64
	BytesPerSecond   *int64
}

// OperationStages lists, in order, every stage a running operation can
// report through OperationProgress.Stage.
//
// It is internal/app's own list, re-exported here because apps/ cannot
// import a core/internal package and still has to be able to check what it
// serves against api/v1/openapi.json's OperationProgress.stage enum. The
// copy is deliberate: a caller must not be able to edit the cycle's own
// vocabulary by writing through a shared backing array.
var OperationStages = append([]string(nil), app.Stages...)

// liveProgress is the in-memory registry of every operation currently
// executing in this process.
//
// Keyed by operation id rather than held as a single slot, even though
// SubmitRunCycle's single-flight lock means at most one run cycle executes
// at a time today: the read paths (GetOperation, ListOperations) are
// id-addressed, single-flight is a policy in operations.go rather than a
// structural property of this type, and a registry that silently answered
// for the wrong operation the day a second concurrent action lands is a
// worse failure than a map lookup.
//
// Nothing prunes it, and nothing needs to: an entry is created when
// execution starts and removed by the deferred end call when execution
// stops, however it stops, so it holds exactly as many entries as there
// are operations executing right now.
type liveProgress struct {
	mu      sync.RWMutex
	running map[string]*operationProgress
}

func newLiveProgress() *liveProgress {
	return &liveProgress{running: make(map[string]*operationProgress)}
}

// begin registers operationID as executing and returns the observer the
// cycle reports into. Calling it twice for the same id replaces the
// previous entry, which is the right answer for the only way that can
// happen: a re-execution of the same id would make the earlier entry
// stale by definition.
func (l *liveProgress) begin(operationID string) *operationProgress {
	p := &operationProgress{}
	l.mu.Lock()
	l.running[operationID] = p
	l.mu.Unlock()
	return p
}

// end removes operationID's entry. It is called from a defer in
// executeRunCycle, so it runs whether the cycle completed, failed or
// panicked: an operation that is no longer executing must not keep
// answering with the last reading it managed to take.
func (l *liveProgress) end(operationID string) {
	l.mu.Lock()
	delete(l.running, operationID)
	l.mu.Unlock()
}

// snapshot returns the latest reading for operationID. ok is false when no
// operation with that id is executing in this process, which is the
// ordinary case for every finished operation and for every operation left
// behind by a previous process.
func (l *liveProgress) snapshot(operationID string) (OperationProgress, bool) {
	l.mu.RLock()
	p := l.running[operationID]
	l.mu.RUnlock()
	if p == nil {
		return OperationProgress{}, false
	}
	return p.read()
}

// operationProgress is one executing operation's latest reading. It
// implements app.ProgressObserver, which is how internal/app hands
// readings to this package without knowing anything about operations.
type operationProgress struct {
	mu   sync.Mutex
	seq  int64
	cur  OperationProgress
	seen bool
}

var _ app.ProgressObserver = (*operationProgress)(nil)

// ObserveProgress records one reading. It is called from whichever
// goroutine reached the change, including the transport's sampler, so it
// takes the lock and returns immediately: it must never be the reason a
// transfer slows down.
func (p *operationProgress) ObserveProgress(in app.Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	p.seen = true
	p.cur = OperationProgress{
		ObservedAt:       now(),
		Sequence:         p.seq,
		Stage:            in.Stage,
		BackupSetID:      in.BackupSetID,
		BackupSetsDone:   in.BackupSetsDone,
		BackupSetsTotal:  in.BackupSetsTotal,
		Artifact:         in.Artifact,
		ArtifactsDone:    in.ArtifactsDone,
		BytesTransferred: copyInt64(in.BytesTransferred),
		BytesTotal:       copyInt64(in.BytesTotal),
		BytesPerSecond:   copyInt64(in.BytesPerSecond),
	}
}

// read returns the latest reading, or ok == false when this operation has
// been registered but has not reported anything yet. That window is real
// (executeRunCycle registers the entry before it calls RunCycle) and it is
// reported as "no progress", not as a reading full of zeroes: a cycle that
// has not said anything yet has genuinely not been measured.
func (p *operationProgress) read() (OperationProgress, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seen {
		return OperationProgress{}, false
	}
	return p.cur, true
}

// copyInt64 copies a pointed-to value so the reading handed to a caller
// shares no memory with the cycle still producing readings.
func copyInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}
