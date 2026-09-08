package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// What a backup set is DOING, as opposed to what state it is in (issue
// #573).
//
// Everything a dashboard could already ask this package answers the second
// question. A set is HEALTHY or FAILING, it holds so many backups, its
// newest known-good is so old. Between two of those readings sits a cycle
// that discovers, transfers, verifies and commits, and none of it reached
// a screen: a set mid-transfer looked exactly like a set sitting idle, so
// the only way to find out whether anything was happening was to read the
// container's log.
//
// # Why this is a read of the event stream rather than a second one
//
// internal/obs has emitted every one of those moments since FR-23, with
// stable names and structured fields. What was missing was not events, it
// was a reader: a log line is gone the instant it is written unless
// something in this process is holding on to it. So this file is a
// bounded, in-memory tail of the SAME events, taken through obs.Sink, and
// not a parallel vocabulary that could drift from the one an operator sees
// in the log.
//
// That is also why nothing here interprets an event into a sentence. The
// feed carries the event's own name, its own level and its own fields, and
// whichever client is presenting decides what to call it, exactly as
// activity.go's durable feed already argues for the transition log. A
// severity baked into the wire would freeze one screen's display decision
// for every other client, and a message composed here would be a second
// place the wording lives.
//
// # Why in memory, and why bounded
//
// For the same reason progress.go gives: this is a volatile fact. It
// changes several times a second, it stops meaning anything the moment the
// process producing it dies, and the last value it held after a restart is
// not stale data but a lie about a cycle that no longer exists. Writing it
// to the journal would put a tick-rate write path on the one table whose
// durability guarantees exist to avoid exactly that.
//
// Bounded, because an unbounded tail of a daemon that runs for months is a
// memory leak with a nice name. The durable record of what happened is the
// journal, read through ListActivity; this is the last few hundred lines,
// and it says where it begins so a client that has fallen behind can tell
// it missed something instead of reading a gap as continuity.
//
// # Why it is not a stream
//
// This is a snapshot a caller reads, and the read carries a cursor and a
// suggested interval. It is not server-sent events and not a long poll,
// and the deployment is the reason. This product runs on a NAS, behind
// whatever reverse proxy the operator already had, and behind serve/ui.go's
// own proxy in the two-container topology. A held-open response is at the
// mercy of every buffering and idle-timeout default between here and the
// browser, and a design that only works when nothing in the path buffers
// is not a design. A plain GET works through all of them, needs no
// reconnection logic, and is exactly as readable from a terminal as from a
// browser, which is what "both surfaces read the same thing" has to mean.
// The cursor is what keeps that cheap: a client sends the last sequence it
// saw and gets only what is newer.

// liveActivityBufferSize is how many events one bucket keeps.
//
// It is a display buffer, not an archive. Two hundred lines is more than
// the strip can show and more than an operator scrolls back through
// looking for the line before an error, which is what this exists for; the
// full history is the journal, one click away. The number is per bucket
// (each backup set, plus the deployment-wide one), so a deployment with
// twenty sets holds at most twenty-one of these.
const liveActivityBufferSize = 200

// The two cadences the feed asks to be polled at.
//
// The server decides rather than the client, because the server is the one
// that knows whether anything is moving. A dashboard that picked its own
// interval would either hammer a NAS holding a dozen idle sets or watch a
// transfer at a cadence that makes the bar jump.
const (
	liveActivityBusyPoll = 1 * time.Second
	liveActivityIdlePoll = 10 * time.Second
)

// liveActivityDefaultLimit and liveActivityMaxLimit bound how many events
// one read returns per set. The limit is advisory in both directions, the
// same way ListActivity's already is: an absent or nonsensical value gets
// the default and an oversized one is clamped, because a client asking for
// a feed should get a feed rather than a refusal over a number.
const (
	liveActivityDefaultLimit = 50
	liveActivityMaxLimit     = liveActivityBufferSize
)

// The scopes an event in this feed can have.
//
// Most events name the set they happened in, or an artifact that does.
// A few genuinely do not: a cycle starting covers every set, and a
// capacity check is about a filesystem.
//
// Those used to be reported to EVERY set's strip, marked as
// deployment-wide, because the alternatives on offer were dropping them
// (which hides them) and pinning them to one set (which puts them on the
// wrong screen), and both of those are worse. Issue #593 is what that
// cost: with a bounded tail, the one shared ring crowded out each set's
// own lines until every strip on a real deployment showed the same log,
// and a strip that answers "what is the whole system doing" is not the
// question the strip was put there to answer.
//
// The third option did not exist yet. It does now: issue #599's global
// terminal is docked to the bottom of every page and carries the
// deployment's own log, so a deployment-wide line has a screen it belongs
// on. So the split is strict. A set's feed is that set's ring and nothing
// else, and the deployment's ring is served in its own right (see
// LiveActivity.Deployment). There is deliberately no allowlist of
// deployment events that "still belong" on a strip: the permissive
// version is exactly what made the strips useless, and a line an operator
// needs beside a set is a line that should name that set when it is
// emitted rather than be copied onto every strip by this package.
const (
	LiveActivityScopeSet        = "set"
	LiveActivityScopeDeployment = "deployment"
)

// What a set's progress fraction counts.
//
// Artifacts, and the feed says so rather than leaving an operator to infer
// it from a bar. Artifacts done over artifacts this pass will walk is a
// fraction of two numbers this process actually holds; it jumps unevenly
// when a set mixes a 2GB dump with a 4KB text file, and that is honest,
// because the alternative is a byte total that does not exist until
// discovery has finished and that no remote reports reliably even then.
//
// Unknown is the window before a set's pass has counted its rows. It is a
// separate value rather than a zero denominator because zero of zero draws
// as a finished cycle, which is the one reading an operator would act on
// wrongly.
const (
	LiveActivityBasisArtifacts = "artifacts"
	LiveActivityBasisUnknown   = "unknown"
)

// How a set's pass ENDED, which is a different question from how many of
// its artifacts failed.
//
// The two came apart the moment a pass could stop before it reached an
// artifact. A set whose reconcile or discovery failed never gets as far as
// the walk, so it leaves the failure count at zero and the denominator
// absent, and a strip reading only those two draws it as a set with
// nothing to do. Cancelled is its own answer for internal/app's own
// reason (see BackupSetCycleResult.SystemicFailure): a pass an operator
// stopped by taking an edit hold is something this manager was asked to
// do, and spelling it the way a source that has gone unreachable is
// spelled is a false alarm in a product whose job is to be believed about
// backups.
//
// They are internal/app's own constants rather than a second spelling of
// them, the way OperationStages (progress.go) re-exports app.Stages: this
// package serves the verdict the cycle reached, and a copy of a closed
// vocabulary is a copy that drifts.
const (
	LiveActivityOutcomeOK      = app.SetOutcomeOK
	LiveActivityOutcomeFailed  = app.SetOutcomeFailed
	LiveActivityOutcomeStopped = app.SetOutcomeStopped
)

// LiveActivityOutcomes lists every outcome this feed can report, in the
// order api/v1/openapi.json declares them. It exists so the contract's
// enum and this package's vocabulary are compared rather than both being
// written down and trusted; see the test in apps/common/webhost that holds
// the two together.
var LiveActivityOutcomes = append([]string(nil), app.SetOutcomes...)

// LiveActivityField is one field of an event, already rendered and already
// redacted (obs.Field). It is a pair rather than a map because the order
// the event logged them in is the order that reads best.
type LiveActivityField struct {
	Key   string
	Value string
}

// LiveActivityEvent is one line of the feed.
//
// Event is internal/obs's own event name and Level is internal/obs's own
// severity. Neither is a display decision this package made: the emitter
// chose the level when it decided the line was a warning rather than a
// note, and a client colouring by it is reading what the engine said, not
// a verdict invented on the way to a screen.
type LiveActivityEvent struct {
	Sequence int64
	At       time.Time
	Level    string
	Event    string
	Scope    string
	Message  string
	Fields   []LiveActivityField
}

// LiveActivitySet is one backup set's strip.
type LiveActivitySet struct {
	BackupSetID string

	// Active is whether a cycle is inside this set in THIS process right
	// now. It is what a moving indicator is driven by, and it is a
	// separate fact from the fraction: a large artifact can hold the
	// numbers still for a long time while everything is fine, and a
	// percentage on its own cannot tell "slow" from "stuck".
	Active bool

	// Stage is one of internal/app's stage constants, and Artifact is the
	// artifact that stage is about. Both are empty when nothing is in
	// flight.
	Stage    string
	Artifact string

	// ArtifactsCompleted over ArtifactsTotal is the fraction, and
	// ProgressBasis says what it counts. Total is nil until the set's own
	// pass has counted the rows it will walk; see LiveActivityBasisUnknown.
	ArtifactsCompleted int
	ArtifactsTotal     *int
	ProgressBasis      string

	// BytesTransferred, BytesTotal and BytesPerSecond describe the ONE
	// artifact named by Artifact, never the set. They are nil whenever no
	// copy is in flight and reporting, which is a different fact from a
	// measured zero.
	//
	// There is deliberately no time-remaining figure here. The numbers a
	// client needs to work one out for the artifact in flight are all
	// present, and computing it here would put a figure this package
	// cannot check on a wire that other clients then have to serve too.
	BytesTransferred *int64
	BytesTotal       *int64
	BytesPerSecond   *int64

	// Failures is how many of this set's artifacts ended this pass in a
	// terminal failure state. It resets when a new pass over the set
	// begins, because it describes the pass rather than the set's history.
	Failures int

	// Outcome is how the last pass over this set ENDED, one of the
	// LiveActivityOutcome constants, and empty until one has. It is not
	// derivable from Failures: see those constants for the two passes
	// that end badly with nothing to count.
	Outcome string

	// StartedAt and FinishedAt bound the pass this strip describes. Both
	// are nil until a pass has actually started, and FinishedAt is nil
	// while one is running.
	StartedAt  *time.Time
	FinishedAt *time.Time

	// Events is the tail, oldest first, filtered by whatever cursor the
	// caller sent.
	Events []LiveActivityEvent

	// Truncated says Limit cut this reading short and the rest is still
	// held. Events above are the OLDEST held that are newer than the
	// cursor, so a caller that wants the whole tail asks again from the
	// sequence this one ends at and loses nothing on the way.
	Truncated bool

	// Dropped says lines this caller's cursor had not reached yet fell
	// out of the bounded buffer before this read did. They are gone, and
	// the tail above is NOT continuous with whatever the caller already
	// holds.
	Dropped bool

	// OldestSequence and LatestSequence are the bounds of what is still
	// held for this set, NOT of the slice above. A client whose cursor is
	// older than OldestSequence knows it missed lines and can say so
	// instead of presenting a gap as continuity.
	OldestSequence int64
	LatestSequence int64
}

// LiveActivity is one reading of the whole feed.
type LiveActivity struct {
	ObservedAt time.Time

	// Epoch names the process this reading came from, and it changes on
	// every start. The sequence counter a cursor is built from is
	// per-process and starts again at zero with it, so a client that kept
	// its cursor across a restart would ask for everything after a number
	// the new process has not reached and be told, correctly and
	// uselessly, that there is nothing new: it would hold a dead cycle's
	// lines on screen and show none of the live ones. A client compares
	// this with the one its last reading carried and, on a difference,
	// drops the cursor and everything behind it.
	Epoch string

	// PollAfter is how long this process suggests waiting before asking
	// again. See the two constants above for why the server decides.
	PollAfter time.Duration

	Sets []LiveActivitySet

	// Deployment is the log that belongs to no single backup set, served
	// in its own right rather than copied onto every strip (issue #593).
	//
	// Nil means this reading was not asked for it: a caller that narrowed
	// to one backup set asked about that set, not about the deployment.
	// A deployment with no configured sets still gets one, which is the
	// case the old reading could not answer at all: it built its whole
	// answer by walking the configured sets, so a fresh install answered
	// with an empty list and the bucket behind it was unreachable, at
	// exactly the moment a new operator is pressing buttons in a wizard
	// and most needs to see something.
	Deployment *LiveActivityDeployment
}

// LiveActivityDeployment is the deployment-wide tail: the events that
// name no single backup set.
//
// It carries the same four honesty flags a set's strip does and for the
// same reasons (see LiveActivitySet), and deliberately none of the
// progress counters: there is no such thing as how far through its own
// pass a deployment is.
type LiveActivityDeployment struct {
	// Events is the tail, oldest first, filtered by whatever cursor the
	// caller sent.
	Events []LiveActivityEvent

	// Truncated says Limit cut this reading short and the rest is still
	// held.
	Truncated bool

	// Dropped says lines this caller's cursor had not reached yet fell
	// out of the bounded buffer before this read did.
	Dropped bool

	// OldestSequence and LatestSequence are the bounds of what is still
	// held, NOT of the slice above.
	OldestSequence int64
	LatestSequence int64
}

// LiveActivityRequest is what a caller asks for.
type LiveActivityRequest struct {
	// BackupSetID narrows the read to one set. Empty means every
	// configured set, which is what a dashboard showing a strip per set
	// wants: one request per tick rather than one per set, which matters
	// because a browser will not open more than a handful of concurrent
	// connections to one origin.
	BackupSetID string

	// Since is the highest sequence the caller has already seen. Zero
	// means "whatever is still held".
	Since int64

	// Limit bounds how many events come back per set. See
	// liveActivityDefaultLimit for why it is advisory.
	Limit int

	// DeploymentOnly narrows the read to the deployment-wide bucket and
	// no set at all.
	//
	// It is the read a terminal following the deployment's own log wants,
	// and the one half of the set/deployment distinction that a
	// BackupSetID cannot express: naming no set already means "every
	// set". Setting both is not an error, and the narrower answer wins:
	// a caller that named a set asked about that set.
	DeploymentOnly bool
}

// LiveActivity reports what every configured backup set is doing.
//
// Every configured set appears, whether or not anything has happened to
// it. That is the point of a pinned strip: a panel that appears only
// during activity teaches an operator to hunt for it, and its absence then
// means either "nothing is running" or "nothing is reporting" with no way
// to tell which.
//
// Read-only in the strictest sense: it takes no lock the cycle needs,
// starts nothing and writes nothing.
func (b *BackupService) LiveActivity(_ context.Context, req LiveActivityRequest) (LiveActivity, error) {
	if req.BackupSetID != "" {
		if err := b.requireBackupSet(req.BackupSetID); err != nil {
			return LiveActivity{}, err
		}
	}

	limit := req.Limit
	if limit <= 0 {
		limit = liveActivityDefaultLimit
	}
	if limit > liveActivityMaxLimit {
		limit = liveActivityMaxLimit
	}

	st := b.state.Load()
	var ids []string
	if !req.DeploymentOnly {
		for _, src := range st.inner.Config.Sources {
			for _, bs := range src.BackupSets {
				id := src.Name + "/" + bs.Name
				if req.BackupSetID != "" && id != req.BackupSetID {
					continue
				}
				ids = append(ids, id)
			}
		}
	}

	// Whether the deployment's own bucket is part of this reading. A
	// caller that named a set asked about that set; everybody else gets
	// it, including a deployment with nothing configured, which is the
	// whole of the case the old reading could not answer.
	wantDeployment := req.BackupSetID == ""

	// Every set, under ONE lock acquisition. The client holds a single
	// cursor and it is the highest sequence anywhere in the reading, so a
	// reading assembled bucket by bucket with the lock released between
	// them would hand back buckets sampled at different moments: the
	// cursor lands on the latest of them and whatever arrived for an
	// earlier bucket meanwhile is filtered out of the next poll and never
	// returned to anybody. One reading is one moment.
	sets, deployment := b.activity.read(ids, wantDeployment, req.Since, limit)
	out := LiveActivity{
		ObservedAt: now(),
		Epoch:      b.activity.epochID(),
		PollAfter:  liveActivityIdlePoll,
		Sets:       sets,
		Deployment: deployment,
	}
	for _, set := range out.Sets {
		// Truncated asks for the busy cadence for the same reason Active
		// does: this process knows there is more to hand over, and
		// waiting out the idle interval to hand it over is the client
		// falling further behind on purpose.
		if set.Active || set.Truncated {
			out.PollAfter = liveActivityBusyPoll
		}
	}
	if out.Deployment != nil && out.Deployment.Truncated {
		out.PollAfter = liveActivityBusyPoll
	}
	return out, nil
}

// liveActivity is the in-memory feed itself: one bounded buffer per backup
// set, one for the events that belong to no single set, and the live
// counters a strip draws its bar from.
//
// It implements both halves of what a strip needs, which is why it is one
// type rather than two. obs.Sink gives it the event stream, which covers
// every path a cycle can take without any of them being re-instrumented.
// app.ProgressObserver gives it the numbers only live sampling can
// produce: a transfer's rate, and how far through its own rows a set's
// pass has got.
type liveActivity struct {
	// epoch names this process's feed and never changes, so it is read
	// without the lock. See LiveActivity.Epoch for what a client does
	// with it.
	epoch string

	mu  sync.Mutex
	seq int64

	// deployment holds the events that name no set. See the scope
	// constants for why they are kept rather than dropped.
	deployment *liveActivityRing

	sets map[string]*liveActivitySetState

	// current is the set a cycle is inside right now, or "". It is what
	// turns a stream of readings into "this pass started" and "that pass
	// ended" without the cycle having to announce either.
	current string
}

// liveActivitySetState is one set's buffer and counters.
type liveActivitySetState struct {
	ring *liveActivityRing

	active             bool
	stage              string
	artifact           string
	artifactsCompleted int
	artifactsTotal     *int
	bytesTransferred   *int64
	bytesTotal         *int64
	bytesPerSecond     *int64
	failures           int
	outcome            string
	startedAt          *time.Time
	finishedAt         *time.Time
}

func newLiveActivity() *liveActivity {
	return &liveActivity{
		epoch:      newLiveActivityEpoch(),
		deployment: newLiveActivityRing(liveActivityBufferSize),
		sets:       make(map[string]*liveActivitySetState),
	}
}

// newLiveActivityEpoch mints the name one process's feed goes by.
//
// Random rather than a clock, because the only property that matters is
// that two starts never collide, and a clock read twice inside one tick
// collides quietly. It falls back to the clock if the system source
// refuses, which is the same fail-safe-rather-than-fail-loud policy the
// rest of the observability path holds to: a feed that cannot mint a name
// should still serve, and the worst a repeated name costs is the restart
// detection this exists for, which is exactly where the sequence
// comparison beside it still catches the case.
func newLiveActivityEpoch() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 16)
}

// epochID is the feed's epoch, nil-receiver-safe like everything else a
// half-built service can reach.
func (l *liveActivity) epochID() string {
	if l == nil {
		return ""
	}
	return l.epoch
}

var (
	_ obs.Sink               = (*liveActivity)(nil)
	_ app.ProgressObserver   = (*liveActivity)(nil)
	_ app.SetOutcomeObserver = (*liveActivity)(nil)
)

// RecordEvent is obs.Sink. It runs on whichever goroutine reached the
// event, inside a running cycle, so it does exactly one lock, one append
// and a return: following the work must never be a reason the work is
// slower.
func (l *liveActivity) RecordEvent(r obs.Record) {
	if l == nil {
		return
	}
	setID, scope := attributeRecord(r)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	e := LiveActivityEvent{
		Sequence: l.seq,
		At:       r.At,
		Level:    strings.ToLower(r.Level.String()),
		Event:    r.Event,
		Scope:    scope,
		Message:  r.Message,
		Fields:   make([]LiveActivityField, 0, len(r.Fields)),
	}
	for _, f := range r.Fields {
		e.Fields = append(e.Fields, LiveActivityField{Key: f.Key, Value: f.Value})
	}

	if scope == LiveActivityScopeDeployment {
		l.deployment.add(e)
		return
	}
	st := l.setLocked(setID)
	st.ring.add(e)
	if isTerminalFailureTransition(r) {
		st.failures++
	}
}

// ObserveProgress is app.ProgressObserver: the live numbers, which no
// event carries because they are only true for the instant they were
// sampled in.
//
// It is also where a pass's boundaries come from. A reading naming a
// different set than the last one means the previous set's pass has ended
// and this one's has begun, so the counters that describe a pass reset
// here rather than needing the cycle to announce anything.
func (l *liveActivity) ObserveProgress(p app.Progress) {
	if l == nil || p.BackupSetID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.current != p.BackupSetID {
		l.finishCurrentLocked()
		l.current = p.BackupSetID
		st := l.setLocked(p.BackupSetID)
		st.beginPass(now())
	}

	st := l.setLocked(p.BackupSetID)
	st.stage = p.Stage
	st.artifact = p.Artifact
	st.artifactsCompleted = p.SetArtifactsCompleted
	st.artifactsTotal = copyInt(p.SetArtifactsPlanned)
	st.bytesTransferred = copyInt64(p.BytesTransferred)
	st.bytesTotal = copyInt64(p.BytesTotal)
	st.bytesPerSecond = copyInt64(p.BytesPerSecond)
}

// ObserveSetOutcome is app.SetOutcomeObserver: how one set's pass ended,
// which is a fact no reading carries.
//
// The strip's headline used to be drawn from the failure count and the
// fraction alone, and both of those are zero and absent for a pass that
// failed before it reached an artifact, so the earliest failure there is
// drew as a set with nothing to do. This is the verdict itself, recorded
// against the set it belongs to and cleared by the next pass over it (see
// beginPass) for the same reason the failure count is: last night's
// verdict is not tonight's.
func (l *liveActivity) ObserveSetOutcome(backupSetID, outcome string) {
	if l == nil || backupSetID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setLocked(backupSetID).outcome = outcome
}

// beginCycle and endCycle bracket one run of the engine.
//
// They exist because "is this set being worked on" cannot be derived from
// readings alone: readings stop arriving both when a cycle finishes and
// when it stalls, and those are opposite things to show an operator. The
// callers are the same two places that already bracket a cycle for the
// edit-hold watch (operations.go and scheduler.go), so a cycle that ends
// however it ends leaves nothing claiming to be running.
func (l *liveActivity) beginCycle() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishCurrentLocked()
}

func (l *liveActivity) endCycle() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishCurrentLocked()
}

// finishCurrentLocked closes whichever set's pass is open, stamping the
// time it ended. The caller holds l.mu.
func (l *liveActivity) finishCurrentLocked() {
	if l.current == "" {
		return
	}
	if st, ok := l.sets[l.current]; ok {
		st.active = false
		st.stage = ""
		st.artifact = ""
		st.bytesTransferred = nil
		st.bytesTotal = nil
		st.bytesPerSecond = nil
		ended := now()
		st.finishedAt = &ended
	}
	l.current = ""
}

// setLocked returns the state for id, creating it on first sight. The
// caller holds l.mu.
func (l *liveActivity) setLocked(id string) *liveActivitySetState {
	st, ok := l.sets[id]
	if !ok {
		st = &liveActivitySetState{ring: newLiveActivityRing(liveActivityBufferSize)}
		l.sets[id] = st
	}
	return st
}

// beginPass resets everything that describes one pass over a set. The
// failure count goes with it: two failures last night is not two failures
// now, and a strip that carried them forward would keep an operator
// looking at a problem that has already been dealt with.
func (st *liveActivitySetState) beginPass(at time.Time) {
	st.active = true
	st.stage = ""
	st.artifact = ""
	st.artifactsCompleted = 0
	st.artifactsTotal = nil
	st.bytesTransferred = nil
	st.bytesTotal = nil
	st.bytesPerSecond = nil
	st.failures = 0
	st.outcome = ""
	started := at
	st.startedAt = &started
	st.finishedAt = nil
}

// snapshot builds one set's strip: its counters and its own events, and
// nothing that happened in another set or across the deployment.
func (l *liveActivity) snapshot(id string, since int64, limit int) LiveActivitySet {
	sets, _ := l.read([]string{id}, false, since, limit)
	return sets[0]
}

// read builds every named set's strip, and optionally the deployment's
// own bucket, under ONE lock acquisition.
//
// That is not an optimisation, it is the correctness of the cursor. A
// caller holds one cursor across every bucket in a reading, so two
// buckets read at two different moments hand it a cursor that is ahead of
// one of them: see LiveActivity's own comment for what that loses. The
// deployment's bucket is inside the same acquisition for exactly that
// reason, and it is now a bucket a client polls rather than a ring copied
// into every set (issue #593).
func (l *liveActivity) read(ids []string, wantDeployment bool, since int64, limit int) ([]LiveActivitySet, *LiveActivityDeployment) {
	out := make([]LiveActivitySet, 0, len(ids))
	if l == nil {
		for _, id := range ids {
			out = append(out, LiveActivitySet{BackupSetID: id, ProgressBasis: LiveActivityBasisUnknown})
		}
		if wantDeployment {
			return out, &LiveActivityDeployment{Events: []LiveActivityEvent{}}
		}
		return out, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		out = append(out, l.snapshotLocked(id, since, limit))
	}
	if !wantDeployment {
		return out, nil
	}
	deployment := &LiveActivityDeployment{}
	deployment.OldestSequence, deployment.LatestSequence = boundsOf(l.deployment)
	deployment.Events, deployment.Truncated = tailOf(l.deployment, since, limit)
	deployment.Dropped = droppedSince(since, l.deployment)
	return out, deployment
}

// snapshotLocked is snapshot's body. The caller holds l.mu.
func (l *liveActivity) snapshotLocked(id string, since int64, limit int) LiveActivitySet {
	out := LiveActivitySet{BackupSetID: id, ProgressBasis: LiveActivityBasisUnknown}
	st := l.sets[id]
	if st != nil {
		out.Active = st.active
		out.Stage = st.stage
		out.Artifact = st.artifact
		out.ArtifactsCompleted = st.artifactsCompleted
		out.ArtifactsTotal = copyInt(st.artifactsTotal)
		out.BytesTransferred = copyInt64(st.bytesTransferred)
		out.BytesTotal = copyInt64(st.bytesTotal)
		out.BytesPerSecond = copyInt64(st.bytesPerSecond)
		out.Failures = st.failures
		out.Outcome = st.outcome
		out.StartedAt = copyTime(st.startedAt)
		out.FinishedAt = copyTime(st.finishedAt)
		if st.artifactsTotal != nil {
			out.ProgressBasis = LiveActivityBasisArtifacts
		}
	}

	var own *liveActivityRing
	if st != nil {
		own = st.ring
	}
	// This set's own ring, and only it. See the scope constants for what
	// merging the deployment's ring in here cost, and for where those
	// events go instead.
	out.OldestSequence, out.LatestSequence = boundsOf(own)
	out.Events, out.Truncated = tailOf(own, since, limit)
	out.Dropped = droppedSince(since, own)
	return out
}

// liveActivityRing is a fixed-capacity, oldest-first buffer. Nothing here
// grows: an entry beyond capacity displaces the oldest, which is what
// "bounded" has to mean for a process that runs for months.
type liveActivityRing struct {
	events []LiveActivityEvent
	cap    int

	// evicted is the sequence of the newest event this buffer has ever
	// discarded, or 0 if it has discarded none. It is what lets a read
	// tell a caller its cursor fell off the back: comparing against the
	// oldest event still held cannot, because a buffer that has never
	// overflowed also starts above 1 whenever the other buffer holds the
	// earlier lines.
	evicted int64
}

func newLiveActivityRing(capacity int) *liveActivityRing {
	return &liveActivityRing{cap: capacity}
}

func (r *liveActivityRing) add(e LiveActivityEvent) {
	r.events = append(r.events, e)
	if len(r.events) > r.cap {
		r.evicted = r.events[len(r.events)-r.cap-1].Sequence
		// Re-slice onto a fresh backing array rather than sliding within
		// the old one: keeping the old array alive would hold on to every
		// string in the dropped events for as long as this buffer lives.
		kept := make([]LiveActivityEvent, r.cap)
		copy(kept, r.events[len(r.events)-r.cap:])
		r.events = kept
	}
}

// boundsOf reports the lowest and highest sequence still held across the
// buffers it is given. Empty buffers contribute nothing, so a bucket that
// has never held an event reports 0..0, which is what "nothing has
// happened here" has to look like now that a set's strip reads its own
// ring alone (issue #593).
func boundsOf(rings ...*liveActivityRing) (oldest, latest int64) {
	for _, r := range rings {
		if r == nil || len(r.events) == 0 {
			continue
		}
		first, last := r.events[0].Sequence, r.events[len(r.events)-1].Sequence
		if oldest == 0 || first < oldest {
			oldest = first
		}
		if last > latest {
			latest = last
		}
	}
	return oldest, latest
}

// tailOf returns the OLDEST limit events in r that are newer than since,
// oldest first, and whether limit cut it short.
//
// One ring, since issue #593. It used to merge a set's ring with the
// deployment's, on the argument that the two interleave in time and the
// line before an error is often the other buffer's. That argument is
// still true and it is why the deployment's bucket carries the same
// sequence numbers: a client holding both can interleave them itself,
// which is what the global terminal does. What it may not do is put the
// whole deployment log on every set's strip, which is what merging here
// meant.
//
// Oldest is what makes the limit safe. A client advances its cursor to the
// newest sequence it was handed, so a reading that answers a cursor with
// the newest few events is telling the client to skip everything under
// them: any burst larger than the limit loses its middle, permanently,
// with nothing in the response saying so. Handing back the oldest instead
// turns the limit into a page rather than a gap, and the flag says there
// is another page to ask for.
func tailOf(r *liveActivityRing, since int64, limit int) ([]LiveActivityEvent, bool) {
	held := ringEvents(r)
	kept := make([]LiveActivityEvent, 0, len(held))
	for _, e := range held {
		if e.Sequence > since {
			kept = append(kept, e)
		}
	}
	truncated := false
	if limit > 0 && len(kept) > limit {
		kept = kept[:limit]
		truncated = true
	}
	// A fresh slice rather than a window onto the ring's own backing
	// array, because a caller must never hold one onto anything this
	// package will write again.
	out := make([]LiveActivityEvent, len(kept))
	copy(out, kept)
	return out, truncated
}

// droppedSince reports whether anything newer than since has already been
// thrown away by one of the buffers a strip reads from.
//
// It is the honest half of a bounded tail. A client that polls with a
// cursor and gets a slice back has no way to tell "nothing else happened"
// from "the rest is gone", and presenting the second as the first is a log
// that looks continuous and is not.
func droppedSince(since int64, rings ...*liveActivityRing) bool {
	for _, r := range rings {
		if r != nil && r.evicted > since {
			return true
		}
	}
	return false
}

func ringEvents(r *liveActivityRing) []LiveActivityEvent {
	if r == nil {
		return nil
	}
	return r.events
}

// copyInt and copyTime hand back a value that shares no memory with the
// state still being written to, for the same reason copyInt64 (progress.go)
// exists.
func copyInt(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

func copyTime(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	t := *v
	return &t
}

// attributeRecord decides which strip an event belongs on.
//
// Three rules, in order, and the order is what makes the answer stable.
// An event that names its own backup set is taken at its word. An event
// that names an artifact names a set inside it, because an artifact id is
// exactly source/set/name. Anything else genuinely belongs to no single
// set and is reported as deployment-wide.
func attributeRecord(r obs.Record) (string, string) {
	var artifact string
	for _, f := range r.Fields {
		switch f.Key {
		case "backup_set":
			if f.Value != "" {
				return f.Value, LiveActivityScopeSet
			}
		case "artifact":
			artifact = f.Value
		}
	}
	if id, ok := backupSetOfArtifact(artifact); ok {
		return id, LiveActivityScopeSet
	}
	return "", LiveActivityScopeDeployment
}

// backupSetOfArtifact splits an artifact id back into the backup set it
// belongs to. An artifact id is exactly source/set/name and a name may not
// contain a slash (model.NewArtifactID refuses one), so the first two
// segments are the set and anything with fewer is not an artifact id at
// all.
func backupSetOfArtifact(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	first := strings.Index(id, "/")
	if first < 0 {
		return "", false
	}
	second := strings.Index(id[first+1:], "/")
	if second < 0 {
		return "", false
	}
	return id[:first+1+second], true
}

// isTerminalFailureTransition reports whether a record is an artifact
// arriving in a state it does not come back from on its own.
//
// It reads the transition rather than the level, because a transition to
// FAILED is logged at info: the engine recording a failure correctly is
// not itself an error. The three states are internal/lifecycle's terminal
// failures, spelled here rather than imported because apps/ and this
// package both serve these strings and neither may reach into
// core/internal from the wire.
func isTerminalFailureTransition(r obs.Record) bool {
	if r.Event != obs.EventLifecycleTransition {
		return false
	}
	for _, f := range r.Fields {
		if f.Key != "to" {
			continue
		}
		switch f.Value {
		case "FAILED", "QUARANTINED", "QUARANTINED_LOST":
			return true
		}
	}
	return false
}
