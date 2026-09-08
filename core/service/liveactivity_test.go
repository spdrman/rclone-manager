// This file covers the live activity feed: what a backup set is DOING,
// as opposed to what state it is in (issue #573).
//
// The first case is the one that matters and it is deliberately built on
// the real thing. It opens a real service against a real config file,
// submits a real run cycle that really copies a file, and then asks the
// feed what happened. Nothing here publishes a scripted event, because a
// feed tested against scripted events proves only that the script was
// written down twice: the whole claim is that the steps a cycle actually
// goes through arrive, attributed to the set they happened in, in the
// order they happened.
//
// The rest of the cases are about the two ways a feed like this misleads
// somebody. A set that has said nothing must still appear, or its silence
// becomes ambiguous between "nothing is running" and "nothing is
// reporting". And the buffer is bounded, so it has to say when it dropped
// something rather than quietly presenting a gap as continuity.
package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// eventNames is every event in one set's feed, in the order the feed
// reports them.
func eventNames(set LiveActivitySet) []string {
	out := make([]string, 0, len(set.Events))
	for _, e := range set.Events {
		out = append(out, e.Event)
	}
	return out
}

// followsInOrder reports whether want appears inside got as a
// subsequence, and names the first step that did not.
//
// A subsequence rather than an exact list, deliberately. The claim is
// about ORDER: a cycle transfers before it commits and commits before it
// ends. Pinning the exact event list instead would make this test fail
// every time the pipeline gains a perfectly good new log line, which is
// how a test stops being read and starts being regenerated.
func followsInOrder(want, got []string) (string, bool) {
	i := 0
	for _, g := range got {
		if i < len(want) && want[i] == g {
			i++
		}
	}
	if i == len(want) {
		return "", true
	}
	return want[i], false
}

func setNamed(t *testing.T, live LiveActivity, id string) LiveActivitySet {
	t.Helper()
	for _, s := range live.Sets {
		if s.BackupSetID == id {
			return s
		}
	}
	var ids []string
	for _, s := range live.Sets {
		ids = append(ids, s.BackupSetID)
	}
	t.Fatalf("the feed has no entry for backup set %q; it reported %v", id, ids)
	return LiveActivitySet{}
}

// TestLiveActivity_ReportsTheStepsARealCycleGoesThroughInOrder is the
// claim, against a real cycle: an operator watching this feed sees the
// set discover, transfer, verify and commit, in that order, with every
// one of those lines attributed to the set it happened in, and sees the
// cycle itself start and end on the deployment's own feed.
//
// Those are two readings since issue #593, and that is the point of it.
// A set's strip carries the set's own work; the cycle brackets belong to
// no single set and are served in their own right rather than copied onto
// every strip.
func TestLiveActivity_ReportsTheStepsARealCycleGoesThroughInOrder(t *testing.T) {
	configPath := writeTestConfigFile(t)

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "live-activity-1",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	if done := waitForTerminalStatus(t, svc, op.ID); done.Status != "completed" {
		t.Fatalf("the cycle ended %q (%s); this test needs a cycle that actually ran", done.Status, done.Error)
	}

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}

	set := setNamed(t, live, "production/postgres-primary")
	names := eventNames(set)
	if len(names) == 0 {
		t.Fatal("a cycle that copied a file left the feed with nothing in it, so nothing on a screen could tell an operator it had happened")
	}

	want := []string{
		obs.EventDiscovery,
		obs.EventLifecycleTransition,
		obs.EventTransferStats,
		obs.EventCommit,
	}
	if missing, ok := followsInOrder(want, names); !ok {
		t.Errorf("the feed never reached %q in order. It reported:\n  %s\nand the steps a cycle goes through are:\n  %s",
			missing, strings.Join(names, "\n  "), strings.Join(want, "\n  "))
	}

	// Sequence numbers order the feed and let a client ask for only what
	// it has not seen. They have to be strictly increasing, or a client
	// that resumes from one either repeats a line or skips one.
	for i := 1; i < len(set.Events); i++ {
		if set.Events[i].Sequence <= set.Events[i-1].Sequence {
			t.Fatalf("event %d has sequence %d and the one before it has %d; a feed a client cannot resume from is a feed a client has to re-read whole",
				i, set.Events[i].Sequence, set.Events[i-1].Sequence)
		}
	}
	if set.LatestSequence != set.Events[len(set.Events)-1].Sequence {
		t.Errorf("the set reports latest sequence %d and its last event is %d", set.LatestSequence, set.Events[len(set.Events)-1].Sequence)
	}

	// Every line on this strip is about this set, carries a clock and
	// carries something to print.
	for _, e := range set.Events {
		if e.Scope != LiveActivityScopeSet {
			t.Errorf("event %q on a set's own strip has scope %q; since #593 a strip carries that set's ring and nothing else", e.Event, e.Scope)
		}
		if e.At.IsZero() {
			t.Errorf("event %q carries no timestamp, so nothing could put a clock beside it", e.Event)
		}
		if e.Message == "" {
			t.Errorf("event %q carries no message, so a terminal has nothing to print", e.Event)
		}
	}
	if scope := findEvent(set, obs.EventDiscovery).Scope; scope != LiveActivityScopeSet {
		t.Errorf("discovery is scoped %q, and it names the set it discovered", scope)
	}

	// The cycle's own brackets, on the deployment's feed. Nothing is lost
	// by taking them off the strip: they are still one poll away, still
	// carrying the same sequence numbers, so a client holding both can
	// interleave them exactly where they happened.
	if live.Deployment == nil {
		t.Fatalf("a reading of every set carries no deployment bucket, so a cycle starting and ending is on no screen at all")
	}
	deploymentNames := eventNamesOf(live.Deployment.Events)
	if missing, ok := followsInOrder([]string{obs.EventCycleStart, obs.EventCycleEnd}, deploymentNames); !ok {
		t.Errorf("the deployment's feed never reached %q in order. It reported:\n  %s",
			missing, strings.Join(deploymentNames, "\n  "))
	}
	for _, e := range live.Deployment.Events {
		if e.Scope != LiveActivityScopeDeployment {
			t.Errorf("event %q in the deployment bucket is scoped %q", e.Event, e.Scope)
		}
	}

	// The discovery line carries its own numbers, so a client renders
	// "41 artifacts, 26 pending" from the event rather than from a
	// sentence this package wrote for one screen.
	if v, ok := eventField(findEvent(set, obs.EventDiscovery), "discovered"); !ok || v == "" {
		t.Errorf("the discovery event carries discovered=%q (present=%v); without its fields a client can only reprint the message", v, ok)
	}

	// A finished cycle is not a running one. The moving part of a
	// progress bar has to stop when the work does.
	if set.Active {
		t.Error("the set still reports itself as active after its cycle reached a terminal status")
	}
	if set.FinishedAt == nil {
		t.Error("a set whose cycle has ended reports no finish time, so an idle strip cannot say when the last cycle was")
	}
}

// TestLiveActivity_ListsEverySetIncludingOnesWithNothingToSay is why the
// strip can be pinned. A panel that appears only once something has
// happened teaches an operator to hunt for it, and its absence is then
// ambiguous between "nothing is running" and "nothing is reporting".
func TestLiveActivity_ListsEverySetIncludingOnesWithNothingToSay(t *testing.T) {
	svc := newTestService(t, config.Source{
		Name: "alpha",
		BackupSets: []config.BackupSet{
			{Name: "nightly", ID: mustBackupSetID(t, "alpha", "nightly")},
			{Name: "weekly", ID: mustBackupSetID(t, "alpha", "weekly")},
		},
	})
	t.Cleanup(func() { _ = svc.Close() })

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if len(live.Sets) != 2 {
		t.Fatalf("the feed reported %d sets and the configuration declares 2", len(live.Sets))
	}
	for _, s := range live.Sets {
		if s.Active {
			t.Errorf("%s reports itself active with no cycle ever having run", s.BackupSetID)
		}
		if len(s.Events) != 0 {
			t.Errorf("%s reports %d events with no cycle ever having run", s.BackupSetID, len(s.Events))
		}
		if s.ProgressBasis != LiveActivityBasisUnknown {
			t.Errorf("%s reports progress basis %q before anything has been discovered; a basis of anything else puts a number behind a bar that has no denominator",
				s.BackupSetID, s.ProgressBasis)
		}
		if s.ArtifactsTotal != nil {
			t.Errorf("%s reports a total of %d artifacts before anything has been discovered", s.BackupSetID, *s.ArtifactsTotal)
		}
	}
}

// TestLiveActivity_KeepsABoundedTailAndSaysWhereItStarts is the honest
// half of "a fixed number of lines in the strip with the full history a
// click away". The buffer is bounded, so it reports where it begins:
// a client that has fallen behind can tell it missed something instead
// of reading a gap as continuity.
func TestLiveActivity_KeepsABoundedTailAndSaysWhereItStarts(t *testing.T) {
	rec := newLiveActivity()
	total := liveActivityBufferSize + 50
	for i := 0; i < total; i++ {
		rec.RecordEvent(obs.Record{
			At:      time.Now(),
			Level:   obs.LevelInfo,
			Event:   obs.EventLifecycleTransition,
			Message: "lifecycle transition",
			Fields:  []obs.Field{{Key: "artifact", Value: "alpha/nightly/one.dump"}},
		})
	}

	got := rec.snapshot("alpha/nightly", 0, liveActivityBufferSize)
	if len(got.Events) > liveActivityBufferSize {
		t.Fatalf("the buffer holds %d events and its bound is %d", len(got.Events), liveActivityBufferSize)
	}
	if got.LatestSequence != int64(total) {
		t.Errorf("latest sequence is %d after %d events", got.LatestSequence, total)
	}
	if got.OldestSequence <= 1 {
		t.Errorf("oldest sequence is %d after %d events overflowed a %d-event buffer; a client cannot tell it missed anything unless the buffer says where it starts",
			got.OldestSequence, total, liveActivityBufferSize)
	}
	if got.Events[0].Sequence != got.OldestSequence {
		t.Errorf("the first event reported has sequence %d and the set says its oldest is %d", got.Events[0].Sequence, got.OldestSequence)
	}
}

// TestLiveActivity_SinceReturnsOnlyWhatIsNewer is what makes this
// pollable without re-sending the whole tail on every tick, and it is
// also the shape a CLI following the feed would use.
func TestLiveActivity_SinceReturnsOnlyWhatIsNewer(t *testing.T) {
	rec := newLiveActivity()
	for i := 0; i < 5; i++ {
		rec.RecordEvent(obs.Record{
			At:      time.Now(),
			Level:   obs.LevelInfo,
			Event:   obs.EventCommit,
			Message: "durable commit complete",
			Fields:  []obs.Field{{Key: "artifact", Value: "alpha/nightly/one.dump"}},
		})
	}

	all := rec.snapshot("alpha/nightly", 0, 100)
	if len(all.Events) != 5 {
		t.Fatalf("the feed holds %d events and five were recorded", len(all.Events))
	}
	rest := rec.snapshot("alpha/nightly", all.Events[2].Sequence, 100)
	if len(rest.Events) != 2 {
		t.Fatalf("asking for everything after sequence %d returned %d events, and two were recorded after it",
			all.Events[2].Sequence, len(rest.Events))
	}
	for _, e := range rest.Events {
		if e.Sequence <= all.Events[2].Sequence {
			t.Errorf("event %d came back for a cursor at %d", e.Sequence, all.Events[2].Sequence)
		}
	}
	// The set's own bounds are about the buffer, not about the slice a
	// cursor happened to ask for; a client needs them to notice a gap.
	if rest.LatestSequence != all.LatestSequence || rest.OldestSequence != all.OldestSequence {
		t.Errorf("a cursored read reports bounds %d..%d and the buffer's are %d..%d",
			rest.OldestSequence, rest.LatestSequence, all.OldestSequence, all.LatestSequence)
	}
}

// TestLiveActivity_AttributesAnEventByWhateverNamesItsSet pins the three
// ways a record is sorted: an event that names its own set, an event that
// names an artifact inside one, and an event that names neither and
// therefore belongs to the deployment.
//
// Attribution was never the bug in #593 and this test is why: it was
// already sorting events into the right buckets. What changed under it is
// where the third bucket is READ, which is the half the assertions below
// had to follow.
func TestLiveActivity_AttributesAnEventByWhateverNamesItsSet(t *testing.T) {
	rec := newLiveActivity()

	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelInfo, Event: obs.EventDiscovery, Message: "discovery pass complete",
		Fields: []obs.Field{{Key: "backup_set", Value: "alpha/nightly"}},
	})
	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelInfo, Event: obs.EventCommit, Message: "durable commit complete",
		Fields: []obs.Field{{Key: "artifact", Value: "alpha/nightly/one.dump"}},
	})
	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelError, Event: obs.EventError, Message: "error",
		Fields: []obs.Field{{Key: "op", Value: "discover"}, {Key: "error", Value: "the source refused the connection"}},
	})

	set := rec.snapshot("alpha/nightly", 0, 100)
	if len(set.Events) != 2 {
		t.Fatalf("the set's feed holds %d events; the discovery names it and the commit's artifact belongs to it, and nothing else does", len(set.Events))
	}
	for _, e := range set.Events {
		switch e.Event {
		case obs.EventDiscovery, obs.EventCommit:
			if e.Scope != LiveActivityScopeSet {
				t.Errorf("event %q is scoped %q and it names this set", e.Event, e.Scope)
			}
		default:
			t.Errorf("event %q reached alpha/nightly's strip and names neither the set nor an artifact in it", e.Event)
		}
	}

	// The set that saw nothing of its own says so, with an empty feed.
	// Before #593 it showed the deployment's error instead, which is how
	// two sets doing entirely different things drew identical strips.
	other := rec.snapshot("alpha/weekly", 0, 100)
	if len(other.Events) != 0 {
		t.Fatalf("a set that saw nothing of its own reports %v; a strip answers what THIS set is doing", eventNamesOf(other.Events))
	}

	// The unattributed error is not dropped: it is on the deployment's
	// own feed, marked as what it is and still carrying its level, so the
	// global terminal can colour it.
	_, deployment := rec.read(nil, true, 0, 100)
	if deployment == nil || len(deployment.Events) != 1 {
		t.Fatalf("the deployment bucket holds %v; the unattributed error belongs to it", deployment)
	}
	if deployment.Events[0].Scope != LiveActivityScopeDeployment {
		t.Errorf("the unattributed error is scoped %q", deployment.Events[0].Scope)
	}
	if deployment.Events[0].Level != "error" {
		t.Errorf("the error arrived at level %q; a terminal that cannot colour an error is one an operator has to read line by line", deployment.Events[0].Level)
	}
}

// TestLiveActivity_CountsArtifactsAndSaysSo is the answer to "what does
// the percentage count". It counts artifacts, the feed says so in a
// field, and a client renders the fraction from two numbers rather than
// being handed one it cannot check.
func TestLiveActivity_CountsArtifactsAndSaysSo(t *testing.T) {
	rec := newLiveActivity()
	planned := 41
	rec.ObserveProgress(app.Progress{
		Stage:                 app.StageTransferring,
		BackupSetID:           "alpha/nightly",
		Artifact:              "dpkg.status.2.gz",
		SetArtifactsPlanned:   &planned,
		SetArtifactsCompleted: 26,
		BytesTransferred:      int64p(1024),
		BytesTotal:            int64p(4096),
		BytesPerSecond:        int64p(512),
	})

	set := rec.snapshot("alpha/nightly", 0, 100)
	if set.ProgressBasis != LiveActivityBasisArtifacts {
		t.Errorf("the feed reports basis %q; a bar whose reader has to infer what it counts is a bar that gets misread", set.ProgressBasis)
	}
	if set.ArtifactsTotal == nil || *set.ArtifactsTotal != 41 {
		t.Errorf("artifacts total = %v, want 41", set.ArtifactsTotal)
	}
	if set.ArtifactsCompleted != 26 {
		t.Errorf("artifacts completed = %d, want 26", set.ArtifactsCompleted)
	}
	if set.Stage != app.StageTransferring || set.Artifact != "dpkg.status.2.gz" {
		t.Errorf("stage/artifact = %q/%q, want transferring and dpkg.status.2.gz", set.Stage, set.Artifact)
	}
	if set.BytesPerSecond == nil || *set.BytesPerSecond != 512 {
		t.Errorf("bytes per second = %v, want 512: without a rate nothing can say how long is left, and this package will not invent one", set.BytesPerSecond)
	}

	// A reading with no denominator yet says so rather than reporting
	// zero of zero, which reads as a finished cycle.
	rec.ObserveProgress(app.Progress{Stage: app.StageDiscovering, BackupSetID: "alpha/weekly"})
	weekly := rec.snapshot("alpha/weekly", 0, 100)
	if weekly.ProgressBasis != LiveActivityBasisUnknown || weekly.ArtifactsTotal != nil {
		t.Errorf("a set still discovering reports basis %q and total %v", weekly.ProgressBasis, weekly.ArtifactsTotal)
	}
}

// TestLiveActivity_CountsThisPassesFailuresPerSet is the failing panel's
// headline. It is per set and per pass: last week's failure is not this
// pass's, and another set's is not this one's.
func TestLiveActivity_CountsThisPassesFailuresPerSet(t *testing.T) {
	rec := newLiveActivity()
	fail := func(setID, artifact, to string) {
		rec.RecordEvent(obs.Record{
			At: time.Now(), Level: obs.LevelInfo, Event: obs.EventLifecycleTransition, Message: "lifecycle transition",
			Fields: []obs.Field{
				{Key: "artifact", Value: setID + "/" + artifact},
				{Key: "from", Value: "TRANSFERRING"},
				{Key: "to", Value: to},
			},
		})
	}
	fail("alpha/nightly", "one.dump", "FAILED")
	fail("alpha/nightly", "two.dump", "QUARANTINED")
	fail("alpha/nightly", "three.dump", "COMMITTED")
	fail("alpha/weekly", "four.dump", "FAILED")

	if got := rec.snapshot("alpha/nightly", 0, 100).Failures; got != 2 {
		t.Errorf("alpha/nightly reports %d failures this pass, and two of its artifacts ended badly", got)
	}
	if got := rec.snapshot("alpha/weekly", 0, 100).Failures; got != 1 {
		t.Errorf("alpha/weekly reports %d failures this pass", got)
	}

	// A new pass over the set starts the count again.
	rec.ObserveProgress(app.Progress{Stage: app.StageDiscovering, BackupSetID: "alpha/nightly"})
	if got := rec.snapshot("alpha/nightly", 0, 100).Failures; got != 0 {
		t.Errorf("a fresh pass over alpha/nightly still reports %d failures from the pass before it", got)
	}
}

// TestLiveActivity_AsksToBePolledSoonerWhileWorkIsMoving keeps the poll
// cadence a decision the server makes. A NAS with a dozen idle sets must
// not be asked every second, and a transfer in flight must not be watched
// every ten.
func TestLiveActivity_AsksToBePolledSoonerWhileWorkIsMoving(t *testing.T) {
	svc := newTestService(t, config.Source{
		Name:       "alpha",
		BackupSets: []config.BackupSet{{Name: "nightly", ID: mustBackupSetID(t, "alpha", "nightly")}},
	})
	t.Cleanup(func() { _ = svc.Close() })

	idle, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if idle.PollAfter <= 0 {
		t.Fatalf("the feed suggests polling after %s, which tells a client nothing", idle.PollAfter)
	}

	svc.activity.beginCycle()
	svc.activity.ObserveProgress(app.Progress{Stage: app.StageTransferring, BackupSetID: "alpha/nightly"})
	busy, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if busy.PollAfter >= idle.PollAfter {
		t.Errorf("the feed asks to be polled after %s while a transfer is running and after %s while nothing is; watching a transfer at an idle cadence is a bar that jumps",
			busy.PollAfter, idle.PollAfter)
	}
}

// TestLiveActivity_RefusesABackupSetThisDeploymentDoesNotHave keeps the
// filter honest: an unknown id is a mistake worth reporting, not an empty
// feed that reads like a quiet set.
func TestLiveActivity_RefusesABackupSetThisDeploymentDoesNotHave(t *testing.T) {
	svc := newTestService(t, config.Source{
		Name:       "alpha",
		BackupSets: []config.BackupSet{{Name: "nightly", ID: mustBackupSetID(t, "alpha", "nightly")}},
	})
	t.Cleanup(func() { _ = svc.Close() })

	if _, err := svc.LiveActivity(context.Background(), LiveActivityRequest{BackupSetID: "alpha/does-not-exist"}); err == nil {
		t.Fatal("asking for a backup set this deployment does not have came back with a feed instead of an error")
	}

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{BackupSetID: "alpha/nightly"})
	if err != nil {
		t.Fatalf("LiveActivity for a set that does exist: %v", err)
	}
	if len(live.Sets) != 1 || live.Sets[0].BackupSetID != "alpha/nightly" {
		t.Errorf("filtering to one set returned %d sets", len(live.Sets))
	}
}

func findEvent(set LiveActivitySet, name string) LiveActivityEvent {
	for _, e := range set.Events {
		if e.Event == name {
			return e
		}
	}
	return LiveActivityEvent{}
}

func eventField(e LiveActivityEvent, key string) (string, bool) {
	for _, f := range e.Fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return "", false
}

// TestLiveActivity_ACursoredReadHandsBackTheOldestFirst is the whole
// point of a cursor, and it is the case a limit quietly broke.
//
// A client polls, advances its cursor to the newest sequence it was
// given, and asks again. So a reading that answers a cursor with the
// NEWEST few events tells the client to skip everything under them:
// the middle of any burst larger than the limit is dropped, for good,
// with the response saying nothing about it. Oldest first is what makes
// a cursor mean "carry on from here" rather than "jump to the end".
func TestLiveActivity_ACursoredReadHandsBackTheOldestFirst(t *testing.T) {
	rec := newLiveActivity()
	const burst = 120
	const limit = 10
	for i := 0; i < burst; i++ {
		rec.RecordEvent(obs.Record{
			At: time.Now(), Level: obs.LevelInfo, Event: obs.EventCommit, Message: "durable commit complete",
			Fields: []obs.Field{{Key: "artifact", Value: "alpha/nightly/one.dump"}},
		})
	}

	// Follow the feed the way a browser does: read, take the highest
	// sequence handed back, ask again from there.
	var seen []int64
	cursor := int64(0)
	for polls := 0; polls < burst; polls++ {
		got := rec.snapshot("alpha/nightly", cursor, limit)
		if len(got.Events) == 0 {
			break
		}
		for _, e := range got.Events {
			seen = append(seen, e.Sequence)
		}
		cursor = got.Events[len(got.Events)-1].Sequence
	}

	if len(seen) != burst {
		t.Fatalf("a client polling with a cursor saw %d of the %d events in the burst; a limit that keeps the newest few tells the cursor to skip the middle, and the middle never comes back",
			len(seen), burst)
	}
	for i, seq := range seen {
		if seq != int64(i+1) {
			t.Fatalf("the %dth event a client saw has sequence %d, and the feed emitted them 1..%d in order", i, seq, burst)
		}
	}
}

// TestLiveActivity_ReadsEverySetAsOfOneMoment is the other half of the
// same cursor, and the one that loses events nobody ever saw.
//
// A client holds ONE cursor, and it is the highest sequence across every
// set in the reading. So a reading assembled set by set, with the lock
// taken and released between them, hands back buckets sampled at
// different moments: the cursor lands on the latest of them, and
// whatever arrived for an earlier bucket while the loop was still
// walking is filtered out of the next poll and never returned to
// anybody. One reading has to be one moment.
func TestLiveActivity_ReadsEverySetAsOfOneMoment(t *testing.T) {
	sets := make([]config.BackupSet, 0, 6)
	for _, name := range []string{"one", "two", "three", "four", "five", "six"} {
		sets = append(sets, config.BackupSet{Name: name, ID: mustBackupSetID(t, "alpha", name)})
	}
	svc := newTestService(t, config.Source{Name: "alpha", BackupSets: sets})
	t.Cleanup(func() { _ = svc.Close() })

	// Strict alternation between the FIRST and the LAST set in the
	// reading, which is what makes a torn read detectable now that a set
	// reads its own ring alone (issue #593). At any single instant those
	// two buckets' newest sequences differ by exactly one; a loop that
	// released the lock between them can show any gap at all, and the six
	// buckets between them are the distance it has to tear across.
	first, last := "alpha/one", "alpha/six"
	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		record := func(id string) {
			svc.activity.RecordEvent(obs.Record{
				At: time.Now(), Level: obs.LevelInfo, Event: obs.EventDiscovery, Message: "discovery pass complete",
				Fields: []obs.Field{{Key: "backup_set", Value: id}},
			})
		}
		for {
			select {
			case <-stop:
				return
			default:
				record(first)
				record(last)
			}
		}
	}()
	defer func() { close(stop); writers.Wait() }()

	for i := 0; i < 400; i++ {
		live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
		if err != nil {
			t.Fatalf("LiveActivity: %v", err)
		}
		if len(live.Sets) != len(sets) {
			t.Fatalf("the reading holds %d sets and the configuration declares %d", len(live.Sets), len(sets))
		}
		a, b := setNamed(t, live, first), setNamed(t, live, last)
		if a.LatestSequence == 0 || b.LatestSequence == 0 {
			continue // nothing has been written into both buckets yet
		}
		gap := a.LatestSequence - b.LatestSequence
		if gap < 0 {
			gap = -gap
		}
		if gap > 1 {
			t.Fatalf("one reading reports %s at sequence %d and %s at %d. The writer alternates strictly between the two, so at any single instant they differ by one; a gap of %d means the reading was assembled from two different moments, and the client's single cursor will land on the later one and skip whatever the earlier bucket gained in between",
				first, a.LatestSequence, last, b.LatestSequence, gap)
		}
	}
}

// TestLiveActivity_SaysWhenALimitCutTheReadingShort is the flag that
// makes the page above safe to act on. A caller handed fewer events than
// there are has to be able to tell that from having caught up, or it
// waits out an idle interval while the process is holding lines for it.
func TestLiveActivity_SaysWhenALimitCutTheReadingShort(t *testing.T) {
	rec := newLiveActivity()
	for i := 0; i < 30; i++ {
		rec.RecordEvent(obs.Record{
			At: time.Now(), Level: obs.LevelInfo, Event: obs.EventCommit, Message: "durable commit complete",
			Fields: []obs.Field{{Key: "artifact", Value: "alpha/nightly/one.dump"}},
		})
	}

	cut := rec.snapshot("alpha/nightly", 0, 10)
	if !cut.Truncated {
		t.Errorf("a read of 10 out of 30 held events reports truncated=false, so a client cannot tell a page from the end of the feed")
	}
	if len(cut.Events) != 10 {
		t.Errorf("the read handed back %d events for a limit of 10", len(cut.Events))
	}

	rest := rec.snapshot("alpha/nightly", cut.Events[len(cut.Events)-1].Sequence, 100)
	if rest.Truncated {
		t.Errorf("a read that handed back everything newer than the cursor still reports truncated=true")
	}
	if len(rest.Events) != 20 {
		t.Errorf("asking again from the cursor the first page ended at returned %d events, and 20 were left", len(rest.Events))
	}
}

// TestLiveActivity_SaysWhenACursorFellOffTheBackOfTheBuffer is the other
// flag, and it is the one the log's own honesty rests on. The buffer is
// bounded, so a client that fell behind far enough has a hole in what it
// holds, and a panel that draws that hole as a continuous log is lying
// about the very thing it exists to show.
func TestLiveActivity_SaysWhenACursorFellOffTheBackOfTheBuffer(t *testing.T) {
	rec := newLiveActivity()
	record := func() {
		rec.RecordEvent(obs.Record{
			At: time.Now(), Level: obs.LevelInfo, Event: obs.EventCommit, Message: "durable commit complete",
			Fields: []obs.Field{{Key: "artifact", Value: "alpha/nightly/one.dump"}},
		})
	}
	for i := 0; i < liveActivityBufferSize; i++ {
		record()
	}

	// Nothing has overflowed yet, so a cursor near the start is still
	// answerable in full.
	if got := rec.snapshot("alpha/nightly", 5, liveActivityMaxLimit); got.Dropped {
		t.Errorf("a full but never-overflowed buffer reports a cursor at 5 as having lost lines")
	}

	for i := 0; i < 60; i++ {
		record()
	}

	behind := rec.snapshot("alpha/nightly", 5, liveActivityMaxLimit)
	if !behind.Dropped {
		t.Errorf("a cursor at 5 after 60 events overflowed a %d-event buffer reports dropped=false; the lines between are gone and the tail is not continuous with what that client holds",
			liveActivityBufferSize)
	}
	caughtUp := rec.snapshot("alpha/nightly", 250, liveActivityMaxLimit)
	if caughtUp.Dropped {
		t.Errorf("a cursor at 250, inside what is still held, reports dropped=true; a marker that fires on a caught-up client is a marker nobody reads")
	}
}

// TestLiveActivity_CarriesHowThePassEndedNotJustItsFailureCount is the
// strip's headline for the pass that goes wrong earliest.
//
// A set whose reconcile or discovery failed never reaches an artifact,
// so it counts no failures and plans no rows, and a panel drawing its
// headline from those two paints it exactly the way it paints a set with
// nothing to do. The pass's own verdict is the fact that separates them.
func TestLiveActivity_CarriesHowThePassEndedNotJustItsFailureCount(t *testing.T) {
	rec := newLiveActivity()
	rec.ObserveProgress(app.Progress{Stage: app.StageDiscovering, BackupSetID: "alpha/nightly"})
	rec.ObserveSetOutcome("alpha/nightly", app.SetOutcomeFailed)

	set := rec.snapshot("alpha/nightly", 0, 100)
	if set.Outcome != LiveActivityOutcomeFailed {
		t.Errorf("the set reports outcome %q after a pass that failed at discovery, want %q", set.Outcome, LiveActivityOutcomeFailed)
	}
	if set.Failures != 0 || set.ArtifactsTotal != nil {
		t.Errorf("this case is about a pass with nothing to count: failures=%d total=%v", set.Failures, set.ArtifactsTotal)
	}

	// A pass an operator stopped is not a pass that broke, and the strip
	// has to be able to say so in its own words.
	rec.ObserveProgress(app.Progress{Stage: app.StageDiscovering, BackupSetID: "alpha/weekly"})
	rec.ObserveSetOutcome("alpha/weekly", app.SetOutcomeStopped)
	if got := rec.snapshot("alpha/weekly", 0, 100).Outcome; got != LiveActivityOutcomeStopped {
		t.Errorf("a pass stopped for an edit hold reports outcome %q, want %q", got, LiveActivityOutcomeStopped)
	}

	// And a new pass over the set starts with no verdict at all, the way
	// the failure count does: last night's outcome is not tonight's.
	rec.ObserveProgress(app.Progress{Stage: app.StageDiscovering, BackupSetID: "alpha/nightly"})
	if got := rec.snapshot("alpha/nightly", 0, 100).Outcome; got != "" {
		t.Errorf("a fresh pass over alpha/nightly still reports outcome %q from the pass before it", got)
	}
}
