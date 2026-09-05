package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// TestExecuteRunCycle_SummaryTellsABarrenCycleFromAGoodOne is issue #361
// at this boundary. An operation completes when the cycle ran, which is a
// deliberate decision this package already made (an artifact's own
// quarantine is a business outcome, not an operation failure) and not
// one this issue overturns. What it means, though, is that a cycle which
// backed nothing up finished here looking exactly like one that backed
// everything up: same status, same backup_sets_processed, same duration.
// That is the same lie `run` was telling its cron job, told to whoever is
// reading the Web UI instead.
func TestExecuteRunCycle_SummaryTellsABarrenCycleFromAGoodOne(t *testing.T) {
	cases := []struct {
		name        string
		report      app.CycleReport
		wantWalked  int
		wantThrough int
	}{
		{
			name: "a cycle that got nothing through",
			report: app.CycleReport{Sets: []app.BackupSetCycleResult{
				{Progress: app.CycleProgress{Walked: 3}},
			}},
			wantWalked:  3,
			wantThrough: 0,
		},
		{
			name: "a cycle that got everything through",
			report: app.CycleReport{Sets: []app.BackupSetCycleResult{
				{Progress: app.CycleProgress{Walked: 3, Durable: 3}},
			}},
			wantWalked:  3,
			wantThrough: 3,
		},
		{
			name: "two backup sets, added up",
			report: app.CycleReport{Sets: []app.BackupSetCycleResult{
				{Progress: app.CycleProgress{Walked: 2, Durable: 2}},
				{Progress: app.CycleProgress{Walked: 4, Durable: 1}},
			}},
			wantWalked:  6,
			wantThrough: 3,
		},
		{
			name:        "a quiet cycle",
			report:      app.CycleReport{Sets: []app.BackupSetCycleResult{{}}},
			wantWalked:  0,
			wantThrough: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			report := tc.report
			withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
				return report
			})

			op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
				IdempotencyKey: "idem-" + tc.name,
				Actor:          "alice",
				ConfigRevision: svc.ConfigRevision(),
			})
			if err != nil {
				t.Fatalf("SubmitRunCycle: %v", err)
			}
			done := waitForTerminalStatus(t, svc, op.ID)
			if done.Status != "completed" {
				t.Fatalf("Status = %q, want %q (Error = %q)", done.Status, "completed", done.Error)
			}

			var summary struct {
				ArtifactsWalked  int `json:"artifacts_walked"`
				ArtifactsThrough int `json:"artifacts_through"`
			}
			if err := json.Unmarshal([]byte(done.Result), &summary); err != nil {
				t.Fatalf("the stored summary is not JSON (%q): %v", done.Result, err)
			}
			if summary.ArtifactsWalked != tc.wantWalked || summary.ArtifactsThrough != tc.wantThrough {
				t.Errorf("summary = walked %d, through %d; want walked %d, through %d. Without these two numbers a cycle that backed nothing up is indistinguishable here from one that backed everything up.\nresult: %s",
					summary.ArtifactsWalked, summary.ArtifactsThrough, tc.wantWalked, tc.wantThrough, done.Result)
			}
		})
	}
}
