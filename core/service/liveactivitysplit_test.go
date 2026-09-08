// The split between a set's own feed and the deployment's (issue #593).
//
// Every case here needs TWO backup sets plus a deployment-wide event,
// and that is the whole reason the bug shipped green. With one set
// configured, merging the deployment ring into that set's strip looks
// exactly like a correct feed: every line in the reading really did
// happen, really is in order, and really is about the only set there is.
// It is the second set that makes the merge visible, because it is the
// second set that starts showing the first set's context.
package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// twoSetService is the fixture every case in this file needs: two sets in
// one source, so a line landing on the wrong strip has somewhere wrong to
// land.
func twoSetService(t *testing.T) *BackupService {
	t.Helper()
	svc := newTestService(t, config.Source{
		Name: "alpha",
		BackupSets: []config.BackupSet{
			{Name: "nightly", ID: mustBackupSetID(t, "alpha", "nightly")},
			{Name: "weekly", ID: mustBackupSetID(t, "alpha", "weekly")},
		},
	})
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func recordSetEvent(rec *liveActivity, setID, event string) {
	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelInfo, Event: event, Message: event,
		Fields: []obs.Field{{Key: "backup_set", Value: setID}},
	})
}

func recordDeploymentEvent(rec *liveActivity, event string) {
	rec.RecordEvent(obs.Record{
		At: time.Now(), Level: obs.LevelInfo, Event: event, Message: event,
		Fields: []obs.Field{{Key: "cycle_id", Value: "c1"}},
	})
}

// TestLiveActivity_ASetsFeedCarriesOnlyThatSetsOwnLines is the regression
// test for #593.
//
// On a real deployment every set's strip showed the same lines, because
// snapshotLocked merged the one shared deployment ring into every set's
// tail and, with a bounded limit, the shared lines crowded out the
// specific ones. The claim here is deliberately negative and it is
// checked in both directions: nightly's feed carries nothing of weekly's
// and nothing of the deployment's, and weekly's carries nothing of
// nightly's.
func TestLiveActivity_ASetsFeedCarriesOnlyThatSetsOwnLines(t *testing.T) {
	rec := newLiveActivity()

	recordDeploymentEvent(rec, obs.EventCycleStart)
	recordSetEvent(rec, "alpha/nightly", obs.EventDiscovery)
	recordDeploymentEvent(rec, obs.EventDiskPressure)
	recordSetEvent(rec, "alpha/weekly", obs.EventCommit)
	recordDeploymentEvent(rec, obs.EventCycleEnd)

	nightly := rec.snapshot("alpha/nightly", 0, 100)
	weekly := rec.snapshot("alpha/weekly", 0, 100)

	for _, tc := range []struct {
		id   string
		set  LiveActivitySet
		want string
	}{
		{"alpha/nightly", nightly, obs.EventDiscovery},
		{"alpha/weekly", weekly, obs.EventCommit},
	} {
		if len(tc.set.Events) != 1 {
			t.Fatalf("%s reports %v; a set's strip answers \"what is THIS set doing\", so its own %s is the only line it has",
				tc.id, eventNames(tc.set), tc.want)
		}
		if tc.set.Events[0].Event != tc.want {
			t.Errorf("%s reports %q as its one line, want %q", tc.id, tc.set.Events[0].Event, tc.want)
		}
		if tc.set.Events[0].Scope != LiveActivityScopeSet {
			t.Errorf("%s's own line is scoped %q", tc.id, tc.set.Events[0].Scope)
		}
	}

	// The sharpest form of the same claim: the two strips are DIFFERENT.
	// That is the assertion nobody had, and it is the one a single-set
	// fixture cannot make.
	if len(nightly.Events) == len(weekly.Events) && nightly.Events[0].Sequence == weekly.Events[0].Sequence {
		t.Errorf("both sets report the same line at sequence %d; the strips have converged on the deployment's log again",
			nightly.Events[0].Sequence)
	}
}

// TestLiveActivity_TheDeploymentsLinesAreServedInTheirOwnRight is the
// other half of #593, and the half that has to land with it.
//
// Nothing is dropped by the split: a cycle starting, a capacity check and
// a cycle ending still reach a screen, they just reach the one screen
// they belong on. Without this bucket the fix would delete those lines
// from the UI rather than move them.
func TestLiveActivity_TheDeploymentsLinesAreServedInTheirOwnRight(t *testing.T) {
	svc := twoSetService(t)

	recordDeploymentEvent(svc.activity, obs.EventCycleStart)
	recordSetEvent(svc.activity, "alpha/nightly", obs.EventDiscovery)
	recordDeploymentEvent(svc.activity, obs.EventCycleEnd)

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if live.Deployment == nil {
		t.Fatalf("the reading carries no deployment bucket, so the two deployment-wide lines the split took off the strips are now on no screen at all")
	}
	got := eventNamesOf(live.Deployment.Events)
	if len(got) != 2 || got[0] != obs.EventCycleStart || got[1] != obs.EventCycleEnd {
		t.Fatalf("the deployment bucket reports %v, want the cycle start and the cycle end", got)
	}
	for _, e := range live.Deployment.Events {
		if e.Scope != LiveActivityScopeDeployment {
			t.Errorf("a line in the deployment bucket is scoped %q", e.Scope)
		}
	}
	if live.Deployment.LatestSequence != 3 {
		t.Errorf("the deployment bucket reports its newest sequence as %d, and the cycle end was the third event this process emitted",
			live.Deployment.LatestSequence)
	}
}

// TestLiveActivity_AnswersForADeploymentWithNoConfiguredSets is the case
// a fresh install lands in, and the one the feed could not answer at all.
//
// LiveActivity built its reading by walking Config.Sources[].BackupSets[],
// so an instance with nothing configured answered "sets": [] and the
// deployment bucket behind it was unreachable. That is exactly the moment
// a new operator is clicking through a wizard with nothing configured
// yet, which is when a terminal is worth the most.
func TestLiveActivity_AnswersForADeploymentWithNoConfiguredSets(t *testing.T) {
	svc := newTestService(t)
	t.Cleanup(func() { _ = svc.Close() })

	recordDeploymentEvent(svc.activity, obs.EventStartup)

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if len(live.Sets) != 0 {
		t.Fatalf("a deployment with no configured sets reports %d of them", len(live.Sets))
	}
	if live.Deployment == nil || len(live.Deployment.Events) != 1 {
		t.Fatalf("a deployment with no configured sets reports %v for its own bucket; the startup line is the only thing it has to show and it is unreachable",
			live.Deployment)
	}
	if live.Deployment.Events[0].Event != obs.EventStartup {
		t.Errorf("the deployment bucket reports %q, want %q", live.Deployment.Events[0].Event, obs.EventStartup)
	}
}

// TestLiveActivity_ASetScopedReadIsAboutThatSetAlone pins the request
// side of the same distinction, which is what a CLI following one set
// needs: asking about a set answers about the set, and asking about the
// deployment answers about the deployment.
func TestLiveActivity_ASetScopedReadIsAboutThatSetAlone(t *testing.T) {
	svc := twoSetService(t)

	recordDeploymentEvent(svc.activity, obs.EventCycleStart)
	recordSetEvent(svc.activity, "alpha/nightly", obs.EventDiscovery)

	set, err := svc.LiveActivity(context.Background(), LiveActivityRequest{BackupSetID: "alpha/nightly"})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if len(set.Sets) != 1 || set.Sets[0].BackupSetID != "alpha/nightly" {
		t.Fatalf("a read narrowed to alpha/nightly reported %d sets", len(set.Sets))
	}
	if set.Deployment != nil {
		t.Errorf("a read narrowed to one set also carried the deployment bucket; a caller that asked about one set is not asking for the deployment's log as well")
	}

	deployment, err := svc.LiveActivity(context.Background(), LiveActivityRequest{DeploymentOnly: true})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if len(deployment.Sets) != 0 {
		t.Errorf("a deployment-scoped read reported %d sets", len(deployment.Sets))
	}
	if deployment.Deployment == nil || len(deployment.Deployment.Events) != 1 {
		t.Fatalf("a deployment-scoped read reported %v", deployment.Deployment)
	}
}

// TestLiveActivity_TheDeploymentBucketIsReadAtTheSameMomentAsTheSets is
// the torn-read claim, restated for the shape the split leaves behind.
//
// A client holds ONE cursor and it is the highest sequence anywhere in
// the reading, so a reading assembled bucket by bucket with the lock
// released in between hands back buckets sampled at different moments:
// the cursor lands on the latest of them and whatever arrived for an
// earlier bucket meanwhile is filtered out of the next poll and never
// returned to anybody.
//
// Strict alternation is what makes that detectable. The writer emits one
// deployment-wide event, then one of alpha/nightly's, forever, so at any
// single instant the two buckets' newest sequences differ by exactly one.
// A read that sampled them at two different moments can show any gap at
// all.
func TestLiveActivity_TheDeploymentBucketIsReadAtTheSameMomentAsTheSets(t *testing.T) {
	svc := twoSetService(t)

	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				recordDeploymentEvent(svc.activity, obs.EventCycleStart)
				recordSetEvent(svc.activity, "alpha/nightly", obs.EventDiscovery)
			}
		}
	}()
	defer func() { close(stop); writers.Wait() }()

	for i := 0; i < 400; i++ {
		live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
		if err != nil {
			t.Fatalf("LiveActivity: %v", err)
		}
		if live.Deployment == nil {
			t.Fatalf("the reading carries no deployment bucket")
		}
		nightly := setNamed(t, live, "alpha/nightly")
		if nightly.LatestSequence == 0 || live.Deployment.LatestSequence == 0 {
			continue // nothing has been written into both buckets yet
		}
		gap := nightly.LatestSequence - live.Deployment.LatestSequence
		if gap < 0 {
			gap = -gap
		}
		if gap > 1 {
			t.Fatalf("one reading reports the deployment at sequence %d and alpha/nightly at %d. The writer alternates strictly between the two, so at any single instant they differ by one; a gap of %d means the reading was assembled from two different moments, and the client's single cursor will land on the later one and skip whatever the earlier bucket gained in between",
				live.Deployment.LatestSequence, nightly.LatestSequence, gap)
		}
	}
}

// eventNamesOf is eventNames for a bare slice, which is what the
// deployment bucket carries.
func eventNamesOf(events []LiveActivityEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Event)
	}
	return out
}

// TestLiveActivity_ASetNamedBesideTheDeploymentScopeAnswersAboutTheSet
// is the combination nothing covered, and the one the request's own doc
// already promised an answer to.
//
// DeploymentOnly's doc says setting both is not an error and the
// narrower answer wins, because a caller that named a set asked about
// that set. The code read the two flags independently instead: naming a
// set left the deployment out, and asking for the deployment scope left
// every set out, so a request carrying both was answered with no sets
// and no deployment bucket. Nothing won.
//
// A reading with nothing in it is the exact failure this whole feed
// exists to prevent. Silence about a set reads as a quiet set, and a
// client that sent both parameters (a terminal that remembered a set
// filter, a hand-typed URL) would watch an empty panel for ever with
// nothing anywhere saying why.
func TestLiveActivity_ASetNamedBesideTheDeploymentScopeAnswersAboutTheSet(t *testing.T) {
	svc := twoSetService(t)

	recordDeploymentEvent(svc.activity, obs.EventCycleStart)
	recordSetEvent(svc.activity, "alpha/nightly", obs.EventDiscovery)
	recordSetEvent(svc.activity, "alpha/weekly", obs.EventCommit)

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{
		BackupSetID:    "alpha/nightly",
		DeploymentOnly: true,
	})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if len(live.Sets) != 1 || live.Sets[0].BackupSetID != "alpha/nightly" {
		t.Fatalf("a read naming alpha/nightly AND the deployment scope reported %d set(s); the narrower of the two is the set, and answering with neither is the empty reading this feed exists to prevent",
			len(live.Sets))
	}
	if got := eventNamesOf(live.Sets[0].Events); len(got) != 1 || got[0] != obs.EventDiscovery {
		t.Errorf("alpha/nightly's strip reports %v, want its own discovery line alone", got)
	}
	if live.Deployment != nil {
		t.Errorf("the reading also carried the deployment bucket; the caller named a set, and naming a set is the narrower question")
	}
}
