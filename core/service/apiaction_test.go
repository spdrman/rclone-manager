package service

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/cliecho"
	"github.com/spdrman/backupd/core/internal/config"
	"github.com/spdrman/backupd/core/internal/obs"
)

// newRecordingService is newTestService with a real logger behind it.
//
// The live feed is this logger's obs.Sink (service.New), so a service
// built with a nil logger has no tap at all and records nothing anywhere.
// That is fine for the tests that publish into the ring directly and
// wrong for these, which are about the path a real action takes: through
// obs, through the redaction, and out into both the log and the feed.
func newRecordingService(t *testing.T, sources ...config.Source) *BackupService {
	t.Helper()
	svc := New(testConfig(sources...), openTestJournal(t), nil, obs.New(io.Discard, obs.LevelInfo))
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// What an action taken in the Web UI leaves behind (issue #599).
//
// Before this, nothing: `grep RecordEvent apps/common/webhost` found
// nothing at all, so a connection test, a config write and a settings
// patch left no trace for any terminal to show. The claim here is that
// one now reaches the same live feed the cycle's own events do, carrying
// the actor, the outcome and the command that would have done the same
// thing, and that it lands on the right feed.

func TestRecordAPIAction_ReachesTheDeploymentsFeedWithItsCommand(t *testing.T) {
	svc := newRecordingService(t)

	svc.RecordAPIAction(context.Background(), cliecho.APIAction{
		Actor:   "alice",
		Method:  "PATCH",
		Route:   "/settings",
		Status:  200,
		Command: "backupd settings patch --timezone Europe/Berlin",
	})

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if live.Deployment == nil || len(live.Deployment.Events) != 1 {
		t.Fatalf("the action reached %v; an action that names no backup set belongs to the deployment", live.Deployment)
	}
	e := live.Deployment.Events[0]
	if e.Event != "api_action" {
		t.Errorf("the action was recorded as event %q", e.Event)
	}
	fields := map[string]string{}
	for _, f := range e.Fields {
		fields[f.Key] = f.Value
	}
	if fields["actor"] != "alice" {
		t.Errorf("the line names actor %q; two operators administering one deployment have to be able to tell each other apart", fields["actor"])
	}
	if fields["route"] != "PATCH /api/v1/settings" {
		t.Errorf("the line names route %q", fields["route"])
	}
	if !strings.Contains(fields["command"], "backupd settings patch") {
		t.Errorf("the line carries command %q", fields["command"])
	}
	if _, present := fields["command_gap"]; present {
		t.Errorf("a line with a command also carries a gap: %v", fields)
	}
}

// An action about one backup set lands on THAT set's feed, which is what
// makes the strips useful now that they are about one set each.
func TestRecordAPIAction_LandsOnTheSetItWasAbout(t *testing.T) {
	svc := newRecordingService(t, config.Source{
		Name: "alpha",
		BackupSets: []config.BackupSet{
			{Name: "nightly", ID: mustBackupSetID(t, "alpha", "nightly")},
			{Name: "weekly", ID: mustBackupSetID(t, "alpha", "weekly")},
		},
	})

	svc.RecordAPIAction(context.Background(), cliecho.APIAction{
		Actor: "alice", Method: "PATCH", Route: "/backup-sets/{source}/{set}",
		Status: 200, BackupSetID: "alpha/nightly",
		Command: "backupd backup-set patch alpha/nightly --stale-after 48h",
	})

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if got := setNamed(t, live, "alpha/nightly"); len(got.Events) != 1 {
		t.Fatalf("alpha/nightly's feed holds %v", eventNamesOf(got.Events))
	}
	if got := setNamed(t, live, "alpha/weekly"); len(got.Events) != 0 {
		t.Errorf("alpha/weekly's feed holds %v; the action was about the other set", eventNamesOf(got.Events))
	}
	if live.Deployment == nil || len(live.Deployment.Events) != 0 {
		t.Errorf("the deployment's feed holds the action too, so it would be printed twice")
	}
}

// A refusal is the case an operator has nothing else to go on, so it is
// recorded at a level a terminal can colour, carrying the code and the
// reason rather than only a number.
func TestRecordAPIAction_ARefusalCarriesItsReason(t *testing.T) {
	svc := newRecordingService(t)

	svc.RecordAPIAction(context.Background(), cliecho.APIAction{
		Actor: "alice", Method: "POST", Route: "/operations", Status: 403,
		ErrorCode: "DESTRUCTIVE_OPERATIONS_DISABLED",
		Message:   "destructive operations are disabled until the gate has been verified",
		Gap:       "no backupd equivalent yet",
		GapDetail: "`backupd run` starts a cycle in your own shell, not in this engine",
	})

	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if live.Deployment == nil || len(live.Deployment.Events) != 1 {
		t.Fatalf("the refusal reached %v", live.Deployment)
	}
	e := live.Deployment.Events[0]
	if e.Level != "warn" {
		t.Errorf("a refusal was recorded at level %q; a terminal that cannot colour it is one an operator has to read line by line", e.Level)
	}
	if !strings.Contains(e.Message, "refused") || !strings.Contains(e.Message, "DESTRUCTIVE_OPERATIONS_DISABLED") {
		t.Errorf("the refusal says %q", e.Message)
	}
	fields := map[string]string{}
	for _, f := range e.Fields {
		fields[f.Key] = f.Value
	}
	if fields["detail"] == "" {
		t.Error("the refusal carries no detail, so the terminal has a code and no words")
	}
	if fields["command_gap"] == "" || !strings.Contains(fields["command_gap_detail"], "not in this engine") {
		t.Errorf("the refusal names no gap: %v", fields)
	}
	if _, present := fields["command"]; present {
		t.Errorf("run_cycle echoed a command: %v", fields)
	}
}

// A 5xx is this process's own failure rather than the caller's, and it is
// recorded as one.
func TestRecordAPIAction_AFailureIsAnErrorAndARefusalIsAWarning(t *testing.T) {
	svc := newRecordingService(t)

	svc.RecordAPIAction(context.Background(), cliecho.APIAction{
		Method: "POST", Route: "/catalog/rebuild", Status: 500, ErrorCode: "INTERNAL",
		Command: "backupd catalog rebuild",
	})
	live, err := svc.LiveActivity(context.Background(), LiveActivityRequest{})
	if err != nil {
		t.Fatalf("LiveActivity: %v", err)
	}
	if live.Deployment == nil || len(live.Deployment.Events) != 1 {
		t.Fatalf("the failure reached %v", live.Deployment)
	}
	if got := live.Deployment.Events[0].Level; got != "error" {
		t.Errorf("a 500 was recorded at level %q, want error", got)
	}
}

// A nil service is a half-built one, and recording must never be the
// thing that panics a request.
func TestRecordAPIAction_IsSafeOnANilService(t *testing.T) {
	var svc *BackupService
	svc.RecordAPIAction(context.Background(), cliecho.APIAction{Method: "POST", Route: "/settings"})
}
