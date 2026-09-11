// How an operation WENT, carried on the feed as a stated fact, and the
// start that never got a completion (issue #625).
//
// The feed has always carried the engine's own level, which is the half
// of this that was already accepted: the emitter chose it, and a client
// colouring by it is reading what the engine said rather than a verdict
// invented on the way to a screen. What it did not carry was success.
// There is no such severity, so every client that wanted to draw a
// completion as good news had to re-derive one from event names and
// lifecycle state values, and anything the derivation did not recognise
// stayed neutral.
//
// These cases are about the wire, not about the renderer: the outcome
// arrives, the pairing arrives, and an action whose start is still held
// with no completion behind it is reported as unfinished rather than
// being indistinguishable from one that never happened.
package service

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/spdrman/backupd/core/internal/obs"
)

func TestLiveActivity_CarriesTheResultTheEngineStated(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))

	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelInfo, Event: obs.EventCommit, Message: "durable commit complete",
		Result: obs.ResultSuccess,
		Fields: []obs.Field{{Key: "artifact", Value: "alpha/nightly/one.dump"}},
	})

	got := rec.snapshot("alpha/nightly", 0, 100)
	if len(got.Events) != 1 {
		t.Fatalf("the feed holds %d events and one was recorded", len(got.Events))
	}
	if got.Events[0].Result != string(obs.ResultSuccess) {
		t.Errorf("the feed reports result %q for a commit the engine stated as %q; a client that cannot read it is left re-deriving one",
			got.Events[0].Result, obs.ResultSuccess)
	}
}

func TestLiveActivity_CarriesThePairingThatMakesAStartAndAnEndOneAction(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))

	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelInfo, Event: obs.EventCycleStart, Message: "cycle starting",
		Action: obs.ActionCycle, ActionID: "cycle-42",
	})

	deployment := deploymentBucket(t, rec)
	if len(deployment.Events) != 1 {
		t.Fatalf("the deployment bucket holds %d events and one was recorded", len(deployment.Events))
	}
	e := deployment.Events[0]
	if e.Action != obs.ActionCycle || e.ActionID != "cycle-42" {
		t.Errorf("the feed reports the start as action %q id %q, want %q / %q", e.Action, e.ActionID, obs.ActionCycle, "cycle-42")
	}
}

// TestLiveActivity_ReportsAnActionThatStartedAndNeverFinished is the
// state an operator most needs named. A start is held with no completion
// behind it, so the reading says so rather than leaving the absence to be
// noticed by somebody reading every line.
func TestLiveActivity_ReportsAnActionThatStartedAndNeverFinished(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))
	started := time.Now()

	rec.RecordEvent(obs.Record{
		At: started, Level: obs.LevelInfo, Event: obs.EventCycleStart, Message: "cycle starting",
		Action: obs.ActionCycle, ActionID: "cycle-42",
	})

	deployment := deploymentBucket(t, rec)
	if len(deployment.Unfinished) != 1 {
		t.Fatalf("the deployment bucket reports %d unfinished actions and a cycle started and never ended", len(deployment.Unfinished))
	}
	open := deployment.Unfinished[0]
	if open.Action != obs.ActionCycle || open.ActionID != "cycle-42" {
		t.Errorf("the unfinished action is %q / %q, want %q / %q", open.Action, open.ActionID, obs.ActionCycle, "cycle-42")
	}
	if !open.StartedAt.Equal(started) {
		t.Errorf("the unfinished action started at %v and the start line was recorded at %v; without the moment nothing can say how long it has been quiet", open.StartedAt, started)
	}

	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelInfo, Event: obs.EventCycleEnd, Message: "cycle finished",
		Action: obs.ActionCycle, ActionID: "cycle-42", Result: obs.ResultSuccess,
	})

	deployment = deploymentBucket(t, rec)
	if len(deployment.Unfinished) != 0 {
		t.Errorf("the deployment bucket still reports %d unfinished actions after the cycle reported its outcome", len(deployment.Unfinished))
	}
}

// TestLiveActivity_ReportsAnUnfinishedActionOnTheSetItBelongsTo keeps the
// strict split issue #593 drew. An action inside one backup set is that
// set's, and reporting it on every strip is what made the strips useless.
func TestLiveActivity_ReportsAnUnfinishedActionOnTheSetItBelongsTo(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly", "alpha/weekly"))

	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelInfo, Event: "connection_test", Message: "connection test starting",
		Action: "connection_test", ActionID: "ct-1",
		Fields: []obs.Field{{Key: "backup_set", Value: "alpha/nightly"}},
	})

	if open := rec.snapshot("alpha/nightly", 0, 100).Unfinished; len(open) != 1 {
		t.Errorf("the set the action names reports %d unfinished actions, want 1", len(open))
	}
	if open := rec.snapshot("alpha/weekly", 0, 100).Unfinished; len(open) != 0 {
		t.Errorf("a set the action does not name reports %d unfinished actions", len(open))
	}
}

// TestLiveActivity_ForgetsAnUnfinishedActionOnceItsStartHasScrolledOut is
// the bound. The unfinished list is derived from the tail this process is
// still holding rather than from a second structure that grows, so a
// start that fell out of the bounded buffer stops being reported along
// with every other line from that far back. The archive is the journal.
func TestLiveActivity_ForgetsAnUnfinishedActionOnceItsStartHasScrolledOut(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))

	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelInfo, Event: obs.EventCycleStart, Message: "cycle starting",
		Action: obs.ActionCycle, ActionID: "cycle-42",
	})
	for i := 0; i < liveActivityBufferSize; i++ {
		rec.RecordEvent(obs.Record{
			At: time.Now(), Level: obs.LevelInfo, Event: obs.EventError, Message: "error",
			Fields: []obs.Field{{Key: "op", Value: "noise"}},
		})
	}

	if open := deploymentBucket(t, rec).Unfinished; len(open) != 0 {
		t.Errorf("the deployment bucket reports %d unfinished actions after the start scrolled out of a %d-line buffer", len(open), liveActivityBufferSize)
	}
}

// TestLiveActivity_ReportsOneRowPerActionEvenIfAnIdIsRepeated is about
// the ids this package does not mint. A cycle's action id is the cycle
// id its caller chose, so a caller that reused one would otherwise put
// two rows on this list claiming to be the same action, and a client
// keying its list by that id has two rows with one key.
func TestLiveActivity_ReportsOneRowPerActionEvenIfAnIdIsRepeated(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))

	for i := 0; i < 2; i++ {
		rec.RecordEvent(obs.Record{
			At: time.Now(), Level: obs.LevelInfo, Event: obs.EventCycleStart, Message: "cycle starting",
			Action: obs.ActionCycle, ActionID: "cycle-42",
		})
	}

	if open := deploymentBucket(t, rec).Unfinished; len(open) != 1 {
		t.Errorf("the deployment bucket reports %d unfinished actions and one action id started twice", len(open))
	}
}

// TestLiveActivity_AnActionEndedThroughItsHandleLeavesNoPhantomRow is the
// bucket-routing half of the pairing, driven through a real obs.Logger
// with this feed as its sink rather than through scripted records.
//
// The scripted version cannot see the defect at all. attributeRecord
// routes a record into a per-set ring or the deployment ring by reading
// backup_set off it, and unfinishedIn runs per bucket, so a start naming
// a set and a completion naming none are a start in one ring and a
// completion in another. The completion clears nothing, and the set's
// strip reports an action that announced itself and went quiet, forever,
// for an action that finished cleanly. Only a test that lets obs build
// both records can catch it.
func TestLiveActivity_AnActionEndedThroughItsHandleLeavesNoPhantomRow(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))
	logger := obs.New(nil, obs.LevelDebug).WithSink(rec)
	ctx := context.Background()

	// Succeeded, with no attributes, which is the shape the API invites
	// and the one that used to break this.
	logger.Begin(ctx, "connection_test", "connection_test", "connection test starting",
		slog.String("backup_set", "alpha/nightly")).
		Succeeded(ctx, "connection test finished")

	set := rec.snapshot("alpha/nightly", 0, 100)
	if len(set.Events) != 2 {
		t.Fatalf("the set's ring holds %d events; a start and a completion that land in two different rings is the defect this is about", len(set.Events))
	}
	if len(set.Unfinished) != 0 {
		t.Errorf("the set reports %d unfinished actions after one that finished cleanly: %+v", len(set.Unfinished), set.Unfinished)
	}
	if open := deploymentBucket(t, rec).Unfinished; len(open) != 0 {
		t.Errorf("the deployment bucket reports %d unfinished actions for an action that named a set and finished: %+v", len(open), open)
	}
}

// deploymentBucket reads the whole feed and returns the bucket that
// belongs to no single backup set.
func deploymentBucket(t *testing.T, rec *liveActivity) *LiveActivityDeployment {
	t.Helper()
	_, deployment := rec.read(nil, true, 0, 100)
	if deployment == nil {
		t.Fatal("the reading carries no deployment bucket")
	}
	return deployment
}
