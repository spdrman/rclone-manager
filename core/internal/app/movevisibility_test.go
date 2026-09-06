package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// FR-30's move pass had exactly one production consumer before this file:
// a log line that fires when the ENGINE CANNOT BE BUILT. A deployment
// that builds an engine perfectly well and has every single move refused
// reported a clean cycle everywhere, forever.
//
// That is issue #361's defect one layer up, so it gets #361's answer:
// a denominator, a numerator, and the verdict in the FR-23 event stream
// that `daemon` can say even though it has no exit status to say it with.

// refusingMedium is countingMedium that cannot take an upload.
//
// It refuses at the transport boundary with a real transport.Error rather
// than a bare one, because that is what the engine's copy step sees from
// a real endpoint whose credentials are missing or whose bucket is gone,
// and the classification is what decides whether the engine retries.
type refusingMedium struct {
	*countingMedium
	attempts int
}

func newRefusingMedium() *refusingMedium {
	return &refusingMedium{countingMedium: newCountingMedium()}
}

func (m *refusingMedium) UploadFromLocal(context.Context, transport.Medium, string, string, transport.UploadOptions) (transport.UploadResult, error) {
	m.attempts++
	return transport.UploadResult{}, &transport.Error{
		Category: transport.Configuration, Op: "upload",
		Cause: errors.New(`resolving credentials from environment variable "BACKUP_S3_COLD": environment variable "BACKUP_S3_COLD" is not set`),
	}
}

// movingServiceWithStream is movingService with somewhere to write the
// FR-23 event stream, which is the only channel a daemon has, plus the
// one artifact whose chain says it belongs offsite.
//
// It goes through New rather than assigning Logger afterwards, so the
// stream it writes is wrapped in the same obs.Redactor a real deployment
// gets. The move refusals this file is about come straight out of the
// transport, and a test reading an unredacted stream would be reading a
// different stream from the one a daemon ships.
func movingServiceWithStream(t *testing.T, medium transport.MediumStore) (*Service, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	journal := openJournal(t)
	bs := testBackupSet(t, dir)
	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = chainWithOffsiteMonthly()
	cfg.StorageMediums = moveTestMediums()
	resolveTestRetention(cfg)

	stream := &bytes.Buffer{}
	svc := New(cfg, journal, newFakeTransport(), obs.New(stream, obs.LevelInfo))
	svc.MediumStore = medium
	svc.Now = fixedNow(retentionTestNow)

	// 40 days old: past the daily window, inside the monthly one, so the
	// first tier selecting it is monthly and its home is the medium.
	seedMovableArtifact(t, context.Background(), journal, bs, "monthly-only.dump", retentionTestNow.AddDate(0, 0, -40))
	return svc, stream
}

// moveErrors pulls every op="move" error out of an event stream, the way
// cycleErrors does for op="cycle". Reading the stream rather than a
// return value is the point: `daemon` has no exit status, so this is the
// only channel it can report any of this on.
func moveErrors(t *testing.T, stream string) []string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(stream), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("the event stream carries a line that is not JSON: %q", line)
		}
		if event["op"] != "move" {
			continue
		}
		if msg, _ := event["error"].(string); msg != "" {
			found = append(found, msg)
		}
	}
	return found
}

// TestRunCycle_SaysSoWhenEveryMoveFailed is the finding itself.
//
// One artifact is due to move to the medium its monthly tier names, the
// medium cannot be written to, and the cycle ends. Every backup set's own
// work succeeded, so nothing anywhere else in this cycle is unhappy, and
// that is exactly the case: the cycle is clean and the artifact is not
// where the operator said it belongs.
func TestRunCycle_SaysSoWhenEveryMoveFailed(t *testing.T) {
	svc, stream := movingServiceWithStream(t, newRefusingMedium())

	report := svc.RunCycle(context.Background())

	p := report.MoveProgress()
	if p.Attempted == 0 {
		t.Fatalf("precondition: this cycle attempted no moves at all (%+v), so it is not the case this test is about", report.Moves)
	}
	if p.Landed != 0 {
		t.Fatalf("precondition: %d move(s) landed against a medium that refuses every upload: %+v", p.Landed, report.Moves)
	}
	if report.MovesErr != nil {
		t.Fatalf("precondition: the move pass failed to run at all (%v); this test is about a pass that ran and got nothing through", report.MovesErr)
	}
	// The whole cycle is otherwise clean, which is what makes the silence
	// dangerous rather than merely incomplete.
	for _, set := range report.Sets {
		if set.Verdict().NothingGotThrough() || set.SystemicFailure() {
			t.Fatalf("precondition: %s is unhappy for an ingestion reason (%+v), so a complaint in the stream could be about that instead", set.Set, set.Verdict())
		}
	}

	msgs := moveErrors(t, stream.String())
	if len(msgs) != 1 {
		t.Fatalf("op=move errors = %v, want exactly one saying this cycle moved nothing.\nstream:\n%s", msgs, stream.String())
	}
	if !strings.Contains(msgs[0], "moved nothing") {
		t.Errorf("message = %q, want it to say this cycle moved nothing", msgs[0])
	}
	if !strings.Contains(msgs[0], "BACKUP_S3_COLD") {
		t.Errorf("message = %q; the engine's own refusal says what to fix and this drops it, sending an operator to a log to find what was already in hand", msgs[0])
	}
}

// TestRunCycle_SaysNothingAboutMovesWhenOneLanded is the control, and
// without it every assertion above would pass against a build that
// complained after every cycle.
func TestRunCycle_SaysNothingAboutMovesWhenOneLanded(t *testing.T) {
	svc, stream := movingServiceWithStream(t, newCountingMedium())

	report := svc.RunCycle(context.Background())
	if report.Moves.Completed != 1 {
		t.Fatalf("precondition: Moves = %+v, want one completed move", report.Moves)
	}
	if msgs := moveErrors(t, stream.String()); len(msgs) != 0 {
		t.Errorf("a cycle whose move completed complained anyway: %v", msgs)
	}
}

// TestRunCycle_SaysNothingAboutMovesInADeploymentWithNoMedium is FR-35's
// compatibility promise, and it holds by arithmetic rather than by a
// second "are mediums configured" test: a deployment with nowhere to move
// anything attempts nothing, so the denominator is zero and the verdict
// is never true.
//
// It matters beyond tidiness. This binary's stream is consumed by log
// shipping in deployments written before EPIC E, and a new error line per
// poll interval in every one of them would be a real regression.
func TestRunCycle_SaysNothingAboutMovesInADeploymentWithNoMedium(t *testing.T) {
	svc, stream := movingServiceWithStream(t, newRefusingMedium())
	svc.Config.StorageMediums = nil
	svc.Config.Retention = testRetention()
	resolveTestRetention(svc.Config)

	report := svc.RunCycle(context.Background())
	if report.MoveProgress().Attempted != 0 {
		t.Fatalf("precondition: a deployment with no medium attempted %d move(s)", report.MoveProgress().Attempted)
	}
	if msgs := moveErrors(t, stream.String()); len(msgs) != 0 {
		t.Errorf("a deployment that declares no storage medium got a move complaint: %v", msgs)
	}
}

// TestMoveProgress_CountsOneAttemptPerArtifact pins the denominator
// against the engine's own report, because the three counters it carries
// do not add up to it.
//
// Planned + Resumed + Refused double-counts a planned move that then
// stopped short with a reason, which is the commonest failing shape there
// is, and it leaves out a move that stopped short with none. One outcome
// per artifact is the count that is actually true.
func TestMoveProgress_CountsOneAttemptPerArtifact(t *testing.T) {
	svc, _ := movingServiceWithStream(t, newRefusingMedium())

	report := svc.RunCycle(context.Background())
	p := report.MoveProgress()

	if p.Attempted != 1 {
		t.Fatalf("Attempted = %d for one artifact, want 1: %+v", p.Attempted, report.Moves)
	}
	if sum := report.Moves.Planned + report.Moves.Resumed + report.Moves.Refused; sum == p.Attempted {
		t.Skip("this fixture does not produce the double count, so it proves nothing about it")
	} else if sum <= p.Attempted {
		t.Errorf("Planned+Resumed+Refused = %d and one artifact was taken up; this test is meant to be looking at the shape where those disagree", sum)
	}
}
