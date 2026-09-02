package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// decodeNDJSONLines parses buf as newline-delimited JSON, one object per
// line, mirroring internal/obs's own test helper (events_test.go's
// decodeLines) since that one is unexported and this package cannot reach
// it.
func decodeNDJSONLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decoding log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TestProcessArtifact_VerifyFailureLogsTheJournalsOwnDetail is issue
// #284's second gap: the emitted lifecycle_transition event must carry
// the same reason the journal recorded, at the moment the transition
// happens, so an operator watching structured logs (FR-23) sees it
// without needing the CLI's own new detail view at all.
//
// The failure driven here is the issue's own reproduction: a backup set
// that requires sha256 verification against a backend that cannot supply
// a comparable remote hash (a hardened SFTP account is the real-world
// case; this fake transport's remoteHashErr stands in for it). The copy
// itself succeeds, so this is a real FR-13 layer-2 capability-absence
// FAILED, driven through the actual internal/lifecycle.Verify code path,
// not a hand-built log assertion.
func TestProcessArtifact_VerifyFailureLogsTheJournalsOwnDetail(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.Validation = config.Validation{Hash: "sha256"}
	source := transport.Source{ID: "verify-fail-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())
	tr.remoteHashErr = transport.NewError(transport.UnsupportedCapability, "remote_hash",
		errors.New(`backend "fake" cannot compute sha256`))

	journal := openJournal(t)
	rec := discoverOneRecord(t, context.Background(), journal, tr, source, bs)

	var buf bytes.Buffer
	logger := obs.New(&buf, obs.LevelInfo)
	svc := New(&config.Config{}, journal, tr, logger)

	svc.processArtifact(context.Background(), source, bs, rec)

	final, err := journal.Get(context.Background(), rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != "FAILED" {
		t.Fatalf("journal state = %q, want FAILED (test setup did not reach the state under test)", final.State)
	}

	var transitionLine map[string]any
	for _, line := range decodeNDJSONLines(t, &buf) {
		if line["event"] == obs.EventLifecycleTransition && line["to"] == "FAILED" {
			transitionLine = line
			break
		}
	}
	if transitionLine == nil {
		t.Fatalf("no lifecycle_transition event with to=FAILED was logged; buf=%s", buf.String())
	}

	detail, _ := transitionLine["detail"].(string)
	if detail == "" {
		t.Fatalf("lifecycle_transition event's detail field is empty; the journal's own diagnostic sentence was dropped before the log line was written")
	}
	const wantSubstring = "hash verification required (sha256) but the backend could not supply a comparable remote hash"
	if !strings.Contains(detail, wantSubstring) {
		t.Errorf("detail = %q, want it to contain %q (the journal's own transition detail, not a generic message)", detail, wantSubstring)
	}

	// The journal's own state_transitions.detail (read back through
	// GetArtifactDetail, issue #284's other half) must say the identical
	// thing: the log line and the CLI's new detail view are two windows
	// onto the one recorded sentence, not two independently-worded texts
	// that could drift apart.
	fromJournal, err := svc.GetArtifactDetail(context.Background(), rec.Artifact)
	if err != nil {
		t.Fatalf("GetArtifactDetail: %v", err)
	}
	if fromJournal.FailureReason != detail {
		t.Errorf("journal's FailureReason = %q, log line's detail = %q, want them identical", fromJournal.FailureReason, detail)
	}
}
