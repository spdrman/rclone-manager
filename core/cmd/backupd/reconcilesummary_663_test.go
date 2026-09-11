package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/lifecycle"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// A late addition folded into the same commit as issue #663's Defect A,
// found by FixE2E-2 against a real deployment: once a settled
// self-contradictory row starts printing "journal row needs repair"
// (Defect A above), `backupd reconcile`'s own trailing summary line still
// said "no unresolved findings" unconditionally whenever nothing errored.
// A run that prints a repair instruction and then claims nothing is
// unresolved contradicts itself in two consecutive lines, which is worse
// than either sentence alone.
//
// This file does not pin the new summary's wording -- FixE2E-2's
// machine-tier assertions grep "journal row needs repair" and "disagrees
// with recorded transfer size" directly and check the exit status
// separately, and those two literals stay frozen. What is pinned here is
// the property: the two sentences must not both appear together.

var reconcileSummary663Epoch = time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)

// stageSettledContradiction663 stages one artifact through the real
// journal to COMMITTED with the #662 shape: a good, intact local final
// file whose size agrees with the remote identity recorded at discovery,
// under a row whose OWN transfer record says 0 bytes. checkLocalFinal
// settles this to localValid with a Reason (issue #662), and Defect A
// makes that Reason surface as a NeedsInvestigation finding.
func stageSettledContradiction663(t *testing.T, configPath string) model.ArtifactID {
	t.Helper()
	dir := filepath.Dir(configPath)
	dbPath := filepath.Join(dir, "state.db")
	localDir := filepath.Join(dir, "contradiction-local")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	ctx := context.Background()
	j, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	name := "dpkg.diversions.5.gz"
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%s): %v", name, err)
	}

	payload := []byte("an intact, 41-byte durable local final copy")
	local := filepath.Join(localDir, name)
	if err := os.WriteFile(local, payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	size := int64(len(payload))

	if _, err := j.Discover(ctx, artifact, name+"-discover", "backups/"+name, state.RemoteIdentity{Size: &size}, reconcileSummary663Epoch); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	steps := []struct {
		from, to  lifecycle.State
		localPath *string
		transfer  *state.TransferResult
	}{
		{from: lifecycle.Discovered, to: lifecycle.Transferring},
		// The #662 shape itself: BytesTransferred 0, disagreeing with the
		// remote identity's recorded size above.
		{from: lifecycle.Transferring, to: lifecycle.Transferred, transfer: &state.TransferResult{BytesTransferred: 0}},
		{from: lifecycle.Transferred, to: lifecycle.Verifying},
		{from: lifecycle.Verifying, to: lifecycle.Verified},
		{from: lifecycle.Verified, to: lifecycle.Committing},
		{from: lifecycle.Committing, to: lifecycle.Committed, localPath: &local},
	}
	for i, s := range steps {
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact:   artifact,
			Key:        fmt.Sprintf("%s-%d-%s", name, i, s.to),
			From:       string(s.from),
			To:         string(s.to),
			LocalPath:  s.localPath,
			Transfer:   s.transfer,
			Detail:     "test fixture",
			OccurredAt: reconcileSummary663Epoch.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("%s: %s -> %s: %v", name, s.from, s.to, err)
		}
	}

	return artifact
}

func writeReconcileSummary663Config(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "state.db")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + filepath.Join(dir, "contradiction-local") + "\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// TestReconcile663_SummaryDoesNotContradictARecordFaultFinding is
// FixE2E-2's finding, folded in: `backupd reconcile` must never print
// "journal row needs repair" and "no unresolved findings" in the same
// run. Exit status is asserted separately and must stay 0: a settled
// record fault is something an operator should look at, not an error
// (issue #663, Defect A's own contract -- exitCode moves only on r.Err
// and Report.Errors).
func TestReconcile663_SummaryDoesNotContradictARecordFaultFinding(t *testing.T) {
	configPath := writeReconcileSummary663Config(t)
	stageSettledContradiction663(t, configPath)

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"reconcile", "--config", configPath})
	})

	if got != 0 {
		t.Fatalf("reconcile exit code = %d, want 0: a settled record fault is reported, not an error", got)
	}
	if !strings.Contains(out, "journal row needs repair") {
		t.Fatalf("reconcile did not report the settled record fault at all; stdout:\n%s", out)
	}
	if strings.Contains(out, "no unresolved findings") {
		t.Errorf(
			"#663: the summary line contradicts the finding printed directly above it.\nstdout:\n%s\n"+
				"A run that says a journal row needs repair and then says there are no unresolved findings "+
				"is worse than either sentence alone.", out)
	}
}

// TestReconcile663_SummaryStillReportsNoFindingsOnAHealthyRow is the
// fence: a row with nothing wrong must still get the plain summary, so a
// fix for the case above cannot satisfy itself by always naming a count.
func TestReconcile663_SummaryStillReportsNoFindingsOnAHealthyRow(t *testing.T) {
	configPath := writeTestConfig(t)

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"reconcile", "--config", configPath})
	})

	if got != 0 {
		t.Fatalf("reconcile exit code = %d, want 0", got)
	}
	if !strings.Contains(out, "no unresolved findings") {
		t.Errorf("a healthy deployment's reconcile summary should say so; stdout:\n%s", out)
	}
}
