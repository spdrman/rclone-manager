package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// FR-30's move pass reaches the activity feed here or nowhere. Moves are
// written to placement_moves and never to state_transitions, so there is
// no per-artifact feed entry for one, and summarizeCycle counted
// ingestion only. A run cycle in which every move was refused completed
// with a summary identical to one in which every move landed.
//
// This is issue #361's shape one layer up, so it gets #361's answer in
// #368's place: two numbers on the recorded summary, read back through
// the same optional-pointer discipline, so a summary written by a build
// that did not record them says "I do not know" rather than "nothing
// moved".

// movesReport builds a CycleReport carrying the engine outcomes a real
// move pass would have produced for n artifacts, of which landed reached
// DONE.
func movesReport(attempted, landed int) app.CycleReport {
	var r placement.CycleReport
	for i := 0; i < attempted; i++ {
		o := placement.Outcome{Phase: placement.Abandoned, Refused: "the destination could not be verified"}
		if i < landed {
			o = placement.Outcome{Phase: placement.Done}
		}
		r.Outcomes = append(r.Outcomes, o)
		if o.Phase == placement.Done {
			r.Completed++
		} else {
			r.Abandoned++
		}
	}
	return app.CycleReport{Sets: []app.BackupSetCycleResult{{}}, Moves: r}
}

// TestExecuteRunCycle_SummaryTellsACycleThatMovedNothingFromOneThatMoved
// is the finding at this boundary.
func TestExecuteRunCycle_SummaryTellsACycleThatMovedNothingFromOneThatMoved(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		report                  app.CycleReport
		wantAttempted, wantLand int
	}{
		{"every move refused", movesReport(3, 0), 3, 0},
		{"every move landed", movesReport(3, 3), 3, 3},
		{"some of each", movesReport(4, 1), 4, 1},
		{"a deployment with nothing to move", movesReport(0, 0), 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			report := tc.report
			withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
				return report
			})

			op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
				IdempotencyKey: "moves-" + tc.name,
				Actor:          "alice",
				ConfigRevision: svc.ConfigRevision(),
			})
			if err != nil {
				t.Fatalf("SubmitRunCycle: %v", err)
			}
			done := waitForTerminalStatus(t, svc, op.ID)
			if done.Status != "completed" {
				t.Fatalf("Status = %q, want completed (Error = %q)", done.Status, done.Error)
			}

			var summary struct {
				MovesAttempted *int `json:"moves_attempted"`
				MovesLanded    *int `json:"moves_landed"`
			}
			if err := json.Unmarshal([]byte(done.Result), &summary); err != nil {
				t.Fatalf("the stored summary is not JSON (%q): %v", done.Result, err)
			}
			if summary.MovesAttempted == nil || summary.MovesLanded == nil {
				t.Fatalf("the summary records nothing about moves (%s); a cycle whose every move was refused completes here identically to one whose every move landed", done.Result)
			}
			if *summary.MovesAttempted != tc.wantAttempted || *summary.MovesLanded != tc.wantLand {
				t.Errorf("summary = attempted %d, landed %d; want attempted %d, landed %d\nresult: %s",
					*summary.MovesAttempted, *summary.MovesLanded, tc.wantAttempted, tc.wantLand, done.Result)
			}

			if done.Cycle == nil || done.Cycle.Moves == nil {
				t.Fatalf("the read side reports no move outcome at all: %+v", done.Cycle)
			}
			if done.Cycle.Moves.Attempted != tc.wantAttempted || done.Cycle.Moves.Landed != tc.wantLand {
				t.Errorf("Cycle.Moves = %+v, want attempted %d, landed %d", *done.Cycle.Moves, tc.wantAttempted, tc.wantLand)
			}
		})
	}
}

// TestParseCycleSummary_ASummaryWithoutMoveCountsReportsNoMoveOutcome is
// the compatibility rule #368 already established for the ingestion
// counts, applied to these two.
//
// A run cycle recorded by an earlier build has no move counts in its
// summary at all. Parsing that as zero and zero would put "0 of 0 moved"
// in front of an operator about a cycle that may well have moved plenty,
// and there is no way for them to tell it apart from a real barren pass.
// So the pair is optional on the way back, and its absence is reported as
// absence.
func TestParseCycleSummary_ASummaryWithoutMoveCountsReportsNoMoveOutcome(t *testing.T) {
	rec := state.Operation{
		Action: ActionRunCycle,
		Status: state.OperationCompleted,
		Result: `{"backup_sets_processed":2,"artifacts_walked":5,"artifacts_through":5,"duration_ms":12}`,
	}
	out := parseCycleSummary(rec)
	if out == nil {
		t.Fatal("a summary carrying the two ingestion counts parsed as no outcome at all; those two are still readable")
	}
	if out.ArtifactsWalked != 5 || out.ArtifactsThrough != 5 {
		t.Errorf("the ingestion counts came back as %+v", out)
	}
	if out.Moves != nil {
		t.Errorf("Moves = %+v for a summary that records none; an older build's cycle is not a cycle that moved nothing", *out.Moves)
	}
}

// TestSummarizeCycle_CarriesNoRefusalText is FR-33 at the one place this
// change could have broken it.
//
// MoveProgress carries the engine's own refusal sentence, which is
// exactly what an operator needs on a terminal and exactly what must not
// go onto the wire: it is whatever the transport handed back, about an
// endpoint, a bucket and a credential reference, assembled by code that
// was never written to a redaction contract. The counts are facts this
// product computed. Only the counts cross.
func TestSummarizeCycle_CarriesNoRefusalText(t *testing.T) {
	report := movesReport(2, 0)
	report.Moves.Outcomes[0].Refused = "resolving credentials from environment variable \"BACKUP_S3_SECRET\""

	summary := summarizeCycle(report)
	for _, unwanted := range []string{"BACKUP_S3_SECRET", "credentials", "resolving"} {
		if strings.Contains(summary, unwanted) {
			t.Errorf("the recorded summary carries %q, which came out of a transport error: %s", unwanted, summary)
		}
	}
	// The control: the counts really are in there, so this is not passing
	// against a summary that carries nothing at all.
	if !strings.Contains(summary, "moves_attempted") {
		t.Errorf("the summary records no move counts either: %s", summary)
	}
}
