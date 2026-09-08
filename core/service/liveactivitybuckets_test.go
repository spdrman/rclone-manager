// What the feed will open a bucket for (EPIC G review).
//
// The live feed keeps one 200-slot bucket per backup set and mints them
// lazily, on first sight of an id. Nothing checked the id against the
// configuration, and the ids arriving were not the engine's: since the
// API action log (issue #599) every non-GET request records a line, and
// the id on that line is built straight out of the chi route parameters.
// So `PATCH /api/v1/backup-sets/ghost-src/ghost-set` minted a bucket,
// whatever it answered. 404, 403 with no CSRF token at all, it did not
// matter, because the middleware records what happened AFTER the handler
// and a refusal is exactly the kind of line this feature exists to show.
//
// Nothing removed them either, and LiveActivity builds its id list from
// the configuration, so not one of those buckets was ever readable. It
// was retained garbage at request rate: 50,000 such requests left 50,000
// buckets and 22.2 MB.
//
// Two independent guards, because either one alone leaves a caller that
// can reopen the hole. RecordAPIAction will only put a backup_set on a
// line when the running configuration names that set, and setLocked will
// not mint a bucket for an id the configuration does not name, so a
// future recorder wired in beside this one inherits the second.
package service

import (
	"context"
	"strconv"
	"testing"

	"github.com/spdrman/rclone-manager/core/cliecho"
	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// oneSetService is the fixture these cases need: exactly one configured
// set, so anything else arriving has nowhere legitimate to land.
func oneSetService(t *testing.T) *BackupService {
	t.Helper()
	return newRecordingService(t, config.Source{
		Name:       "alpha",
		BackupSets: []config.BackupSet{{Name: "nightly", ID: mustBackupSetID(t, "alpha", "nightly")}},
	})
}

// bucketIDs is every id the feed currently holds a bucket for.
func bucketIDs(l *liveActivity) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.sets))
	for id := range l.sets {
		out = append(out, id)
	}
	return out
}

// TestRecordAPIAction_AnActionOnASetThisDeploymentDoesNotHaveIsADeploymentLine
// is the first guard.
//
// The route said backup-sets/{source}/{set} and the request named a set
// that does not exist, so the action is real and worth recording and it
// is not about any set. It goes to the deployment's own feed, where an
// operator watching somebody probe this API can actually see it, and it
// opens no bucket.
func TestRecordAPIAction_AnActionOnASetThisDeploymentDoesNotHaveIsADeploymentLine(t *testing.T) {
	svc := oneSetService(t)

	svc.RecordAPIAction(context.Background(), cliecho.APIAction{
		Actor: "alice", Method: "PATCH", Route: "/backup-sets/{source}/{set}",
		Status: 404, ErrorCode: "BACKUP_SET_NOT_FOUND",
		BackupSetID: "ghost-src/ghost-set",
		Command:     "backup-manager backup-set patch ghost-src/ghost-set --stale-after 48h",
	})

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if live.Deployment == nil || len(live.Deployment.Events) != 1 {
		t.Fatalf("the refusal reached %v; an action on a set this deployment does not have belongs to the deployment, and dropping it would hide somebody probing this API", live.Deployment)
	}
	for _, f := range live.Deployment.Events[0].Fields {
		if f.Key == "backup_set" {
			t.Errorf("the line carries backup_set=%q for a set the configuration does not name, which is what routes it into a bucket nothing can ever read", f.Value)
		}
	}
	if got := bucketIDs(svc.activity); len(got) != 0 {
		t.Errorf("the feed opened buckets %v for an action on a set that does not exist", got)
	}
}

// An action on a set that DOES exist still lands on that set: the guard
// is a check, not a blanket.
func TestRecordAPIAction_StillLandsOnAConfiguredSet(t *testing.T) {
	svc := oneSetService(t)

	svc.RecordAPIAction(context.Background(), cliecho.APIAction{
		Actor: "alice", Method: "PATCH", Route: "/backup-sets/{source}/{set}",
		Status: 200, BackupSetID: "alpha/nightly",
		Command: "backup-manager backup-set patch alpha/nightly --stale-after 48h",
	})

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if got := setNamed(t, live, "alpha/nightly"); len(got.Events) != 1 {
		t.Fatalf("alpha/nightly's feed holds %v", eventNamesOf(got.Events))
	}
	if live.Deployment != nil && len(live.Deployment.Events) != 0 {
		t.Errorf("the deployment's feed holds the action too, so it would be printed twice")
	}
}

// TestLiveActivity_MintsNoBucketForASetTheConfigurationDoesNotName is the
// second guard, taken at the buffer itself rather than at the recorder,
// so a caller wired in beside RecordAPIAction later cannot reopen this.
func TestLiveActivity_MintsNoBucketForASetTheConfigurationDoesNotName(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))

	recordSetEvent(rec, "ghost-src/ghost-set", obs.EventLifecycleTransition)

	if got := bucketIDs(rec); len(got) != 0 {
		t.Fatalf("the feed opened buckets %v for an event naming a set the configuration does not have", got)
	}
	// Not dropped: it goes where a line that belongs to no set goes, so
	// the global terminal still shows it.
	if got := rec.deployment.len(); got != 1 {
		t.Errorf("the deployment's bucket holds %d events; a line about a set that does not exist is a deployment-scoped line, not a line nobody sees", got)
	}
	if got := rec.deployment.at(0).Scope; got != LiveActivityScopeDeployment {
		t.Errorf("the line went into the deployment's bucket carrying scope %q", got)
	}
}

// The engine's own numbers get the same treatment: a progress reading or
// an outcome for a set the configuration does not name opens nothing.
func TestLiveActivity_ProgressAndOutcomeMintNoBucketEither(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))

	rec.ObserveProgress(app.Progress{Stage: app.StageTransferring, BackupSetID: "ghost-src/ghost-set"})
	rec.ObserveSetOutcome("ghost-src/ghost-set", LiveActivityOutcomeFailed)

	if got := bucketIDs(rec); len(got) != 0 {
		t.Fatalf("the feed opened buckets %v from readings about a set the configuration does not have", got)
	}
}

// TestRecordAPIAction_ProbingThisAPIGrowsNothing is the measured shape,
// smaller. Every request names a set that has never existed, the way a
// loop hitting one route with a fresh id does, and the feed holds exactly
// the buckets the configuration names before and after.
func TestRecordAPIAction_ProbingThisAPIGrowsNothing(t *testing.T) {
	svc := oneSetService(t)

	for i := 0; i < 5000; i++ {
		id := "ghost-" + strconv.Itoa(i) + "/set-" + strconv.Itoa(i)
		svc.RecordAPIAction(context.Background(), cliecho.APIAction{
			Actor: "alice", Method: "PATCH", Route: "/backup-sets/{source}/{set}",
			Status: 404, ErrorCode: "BACKUP_SET_NOT_FOUND", BackupSetID: id,
			Command: "backup-manager backup-set patch " + id + " --stale-after 48h",
		})
	}

	if got := bucketIDs(svc.activity); len(got) != 0 {
		t.Fatalf("5000 requests naming sets that do not exist left %d buckets, none of which LiveActivity can ever read, and nothing removes them", len(got))
	}
}

// A set that appears in the configuration AFTER the process started is
// still served: the check reads the running configuration through the
// same atomic snapshot every other reader does, so a hot reload that adds
// a set makes its bucket mintable without a restart.
func TestLiveActivity_ASetAddedByAReloadGetsItsBucket(t *testing.T) {
	svc := oneSetService(t)

	recordSetEvent(svc.activity, "alpha/nightly", obs.EventDiscovery)
	if got := bucketIDs(svc.activity); len(got) != 1 {
		t.Fatalf("the configured set holds buckets %v", got)
	}

	// The same swap a hot reload makes.
	svc.state.Store(&configState{
		inner: app.New(testConfig(config.Source{
			Name: "alpha",
			BackupSets: []config.BackupSet{
				{Name: "nightly", ID: mustBackupSetID(t, "alpha", "nightly")},
				{Name: "weekly", ID: mustBackupSetID(t, "alpha", "weekly")},
			},
		}), nil, nil, nil),
		revision: "r2",
	})

	recordSetEvent(svc.activity, "alpha/weekly", obs.EventDiscovery)

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if got := setNamed(t, live, "alpha/weekly"); len(got.Events) != 1 {
		t.Errorf("a set added by a reload holds %v, so its strip would stay blank until a restart", eventNamesOf(got.Events))
	}
}

// configuredSets is the predicate a bare feed is built with in these
// tests, standing in for the running configuration a real service hands
// it. It is spelled out rather than defaulted to "yes" because a feed
// that says yes to everything is the bug this file is about.
func configuredSets(ids ...string) func(string) bool {
	return func(id string) bool {
		for _, want := range ids {
			if want == id {
				return true
			}
		}
		return false
	}
}
