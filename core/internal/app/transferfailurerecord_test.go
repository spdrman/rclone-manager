package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/internal/config"
	"github.com/spdrman/backupd/core/internal/lifecycle"
	"github.com/spdrman/backupd/core/internal/model"
	"github.com/spdrman/backupd/core/internal/obs"
	"github.com/spdrman/backupd/core/internal/state"
	"github.com/spdrman/backupd/core/internal/transport"
)

// Issue #570: a failed transfer has to be able to record its own failure,
// and whatever the engine then tells an operator has to match what the
// journal holds.
//
// The field report is two attempts running against one artifact at the same
// time, which nothing in this product stops: `runOnce` (core/service) is an
// in-process lock, `backupd fetch` and `run` open the journal in a
// second process behind a SHARED lock, and attemptKey carries only the
// artifact and its retry count, so two live attempts derive the same
// idempotency keys. The loser's copy failed, and by the time it went to
// write FAILED the winner had already carried the artifact to
// REMOTE_RETAINED. failCopy asked for TRANSFERRING -> FAILED, the journal
// refused it (correctly), and the engine was left reporting a verdict it had
// not recorded while `artifacts` showed a perfectly durable artifact.
//
// That is the disagreement this file pins: not "does the transfer fail" and
// not "does the journal refuse an illegal move", both of which already
// worked, but whether the engine's account of an artifact and the journal's
// can be read side by side without contradicting each other. Both cases
// below go through the same assertion for exactly that reason. Two tests
// each holding their own fixture would prove each surface self-consistent
// and nothing at all about the pair.

// otherAttemptRetains stands in for the second attempt that won the race:
// it walks the artifact the rest of the way from TRANSFERRING to
// REMOTE_RETAINED and leaves a real durable local file behind, exactly as a
// second process's own pipeline would have.
//
// It goes through lifecycle.Advance rather than writing rows directly, so
// the simulated winner is held to the same transition table the real one is
// and this fixture cannot walk a path production could not.
func otherAttemptRetains(t *testing.T, ctx context.Context, journal Journal, artifact model.ArtifactID, finalPath string) {
	t.Helper()

	mustWriteFile(t, finalPath, "payload bytes")

	deps := lifecycle.Deps{Journal: journal, Now: fixedNow(epoch)}
	steps := []struct {
		from, to lifecycle.State
	}{
		{lifecycle.Transferring, lifecycle.Transferred},
		{lifecycle.Transferred, lifecycle.Verifying},
		{lifecycle.Verifying, lifecycle.Verified},
		{lifecycle.Verified, lifecycle.Committing},
		{lifecycle.Committing, lifecycle.Committed},
		{lifecycle.Committed, lifecycle.RemoteRetained},
	}
	for i, step := range steps {
		tr := state.Transition{
			Artifact: artifact,
			Key:      "other-attempt:" + artifact.String() + ":" + string(step.to),
			From:     string(step.from),
			To:       string(step.to),
		}
		if step.to == lifecycle.Committed {
			tr.LocalPath = &finalPath
		}
		if _, err := lifecycle.Advance(ctx, deps, tr); err != nil {
			t.Fatalf("simulating the winning attempt, step %d (%s -> %s): %v", i, step.from, step.to, err)
		}
	}
}

// transferErrorLine returns the FR-23 error event processArtifact emitted
// for the transfer step, which is the only account of a failed transfer an
// operator ever sees.
func transferErrorLine(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	for _, line := range decodeNDJSONLines(t, buf) {
		if line["event"] == obs.EventError && line["op"] == "transfer" {
			text, _ := line["error"].(string)
			return text
		}
	}
	return ""
}

// assertTransferAccountIsCoherent is issue #570's property, in one place.
//
// It asks the three questions an operator asks after a transfer fails, of
// the three surfaces that answer them, and requires the answers to compose:
// what does `artifacts` say this artifact is, does `status` count it as a
// failure, and what did the engine say happened. A verdict the engine
// reported but could not write down fails here, and so does a failure count
// that does not match the states the rows are actually in.
func assertTransferAccountIsCoherent(t *testing.T, ctx context.Context, svc *Service, artifact model.ArtifactID, logged string) {
	t.Helper()

	records, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	var row state.Record
	found := false
	for _, r := range records {
		if r.Artifact == artifact {
			row, found = r, true
			break
		}
	}
	if !found {
		t.Fatalf("artifacts has no row for %s at all", artifact)
	}

	report, err := svc.BuildHealthReport(ctx, VersionInfo{BinaryVersion: "test"})
	if err != nil {
		t.Fatalf("BuildHealthReport: %v", err)
	}
	if len(report.BackupSets) != 1 {
		t.Fatalf("health report covers %d backup sets, want 1", len(report.BackupSets))
	}
	failures := report.BackupSets[0].Failures

	failedState := row.State == string(lifecycle.Failed)
	if failedState != (failures > 0) {
		t.Errorf("status and artifacts disagree about %s: artifacts says %q, status reports failures=%d",
			artifact, row.State, failures)
	}

	// The engine must never report a verdict the journal refused. This is
	// the sentence #570 was filed on, and its shape is the giveaway: an
	// ErrStateMismatch reaching an operator means the engine observed
	// something about this artifact and could not write it down, so
	// whatever it just said is not backed by any row.
	if strings.Contains(logged, state.ErrStateMismatch.Error()) {
		t.Errorf("the engine reported a verdict it could not record for %s: %s", artifact, logged)
	}

	// And an artifact that did NOT end in a failure state has to be named
	// as what it actually is. A bare "copy failed" against a row reading
	// REMOTE_RETAINED is the two surfaces telling an operator different
	// things, which is the complaint #570 makes even once the engine has
	// stopped claiming a verdict it never wrote.
	if !failedState && !strings.Contains(logged, row.State) {
		t.Errorf("the transfer failed and the engine never says %s is %q, so the log and `artifacts` describe two different artifacts: %s", artifact, row.State, logged)
	}
}

// TestProcessArtifact_TransferFailure_EngineAndSurfacesAgree drives both
// shapes of failed transfer through the real pipeline and holds them to the
// same property.
//
// The superseded case is issue #570's own reproduction: the copy fails
// while another attempt carries the same artifact to a durable, retained
// state, which is the situation the field report caught twice in one cycle.
// The plain case is its control, and it is what stops "report nothing" from
// passing for a fix: a transfer that fails with nothing else touching the
// artifact still has to land at FAILED, be counted by `status`, and leave a
// reason an operator can read.
func TestProcessArtifact_TransferFailure_EngineAndSurfacesAgree(t *testing.T) {
	copyFailed := transport.NewError(transport.NotFound, "copy_to_local",
		errors.New("rename /data/backups/set/backup.dump.partial.ac832174.partial /data/backups/set/backup.dump.partial: no such file or directory"))

	cases := []struct {
		name string

		// superseded makes another attempt finish the artifact while this
		// one's copy is in flight.
		superseded bool

		wantState    string
		wantFailures int
	}{
		{
			name:         "nothing else touches the artifact",
			superseded:   false,
			wantState:    string(lifecycle.Failed),
			wantFailures: 1,
		},
		{
			name:         "another attempt retains the artifact while the copy runs",
			superseded:   true,
			wantState:    string(lifecycle.RemoteRetained),
			wantFailures: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			localDir := t.TempDir()
			bs := testBackupSet(t, localDir)
			bs.ReadOnly = true
			source := transport.Source{ID: "issue-570"}

			tr := newFakeTransport()
			tr.put("backup.dump", "payload bytes", epoch.Unix())
			tr.copyToLocalErr = copyFailed

			journal := openJournal(t)
			rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

			if tc.superseded {
				// Inside the copy, which is the only window the field
				// report leaves: the collision guard has already run and
				// found nothing at the final name, TRANSFERRING is
				// already recorded, and the copy has not yet returned its
				// error.
				tr.beforeCopy = func() {
					otherAttemptRetains(t, ctx, journal, rec.Artifact, filepath.Join(localDir, "backup.dump"))
				}
			}

			var buf bytes.Buffer
			svc := New(testConfig(t, config.Source{
				Name:       source.ID,
				BackupSets: []config.BackupSet{bs},
			}), journal, tr, obs.New(&buf, obs.LevelInfo))

			svc.processArtifact(ctx, source, bs, rec)

			final, err := journal.Get(ctx, rec.Artifact)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if final.State != tc.wantState {
				t.Fatalf("journal state = %q, want %q (this test's setup did not reach the state under test)", final.State, tc.wantState)
			}
			if tc.superseded {
				if _, statErr := os.Stat(filepath.Join(localDir, "backup.dump")); statErr != nil {
					t.Fatalf("the winning attempt's durable local copy is missing: %v", statErr)
				}
			}

			logged := transferErrorLine(t, &buf)
			if logged == "" {
				t.Fatalf("the transfer failed and nothing was reported to an operator at all; log=%s", buf.String())
			}

			assertTransferAccountIsCoherent(t, ctx, svc, rec.Artifact, logged)

			report, err := svc.BuildHealthReport(ctx, VersionInfo{BinaryVersion: "test"})
			if err != nil {
				t.Fatalf("BuildHealthReport: %v", err)
			}
			if got := report.BackupSets[0].Failures; got != tc.wantFailures {
				t.Errorf("status reports failures=%d, want %d", got, tc.wantFailures)
			}

			if tc.wantState == string(lifecycle.Failed) {
				detail, err := svc.GetArtifactDetail(ctx, rec.Artifact)
				if err != nil {
					t.Fatalf("GetArtifactDetail: %v", err)
				}
				if detail.FailureReason == "" {
					t.Error("a FAILED artifact carries no recorded reason, so an operator has nothing to act on")
				}
			}
		})
	}
}
