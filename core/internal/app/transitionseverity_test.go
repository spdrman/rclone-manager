package app

// What the event stream says when an artifact ends an attempt in one of
// the states this machine calls exceptional (issue #625).
//
// This is driven through processArtifact against a real journal rather
// than asserted on internal/obs alone, because the defect was never in
// obs: the level and the outcome are only right if the CALL SITE tells
// obs which kind of transition this is, and a call site that always
// passed "no" would satisfy every test in that package and leave the
// stream exactly as wrong as it was.
//
// The scenario is issue #419's, reused because it is the honest way to
// reach a quarantine through the pipeline: the source answers the copy
// and then goes quiet for the hash call, the verification stalls, and
// after a bounded number of attempts the artifact is handed to an
// operator.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/internal/config"
	"github.com/spdrman/backupd/core/internal/lifecycle"
	"github.com/spdrman/backupd/core/internal/obs"
	"github.com/spdrman/backupd/core/internal/transport"
	"github.com/spdrman/backupd/core/internal/transport/retry"
	"time"
)

func TestAnArtifactReachingAnExceptionalStateIsLoggedWhereSeverityFindsIt(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.Validation = config.Validation{Hash: "sha256"}
	source := transport.Source{ID: "production"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())

	journal := openJournal(t)
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	var stream bytes.Buffer
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, obs.New(&stream, obs.LevelDebug))
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 3}

	tr.remoteHashErr = unreachableSource()

	var landed lifecycle.State
	for cycle := 1; cycle <= 3; cycle++ {
		landed = svc.processArtifact(ctx, source, bs, mustGetRow(t, journal, rec.Artifact))
	}
	if landed != lifecycle.Quarantined {
		t.Fatalf("precondition: the artifact ended at %s, want %s; this test only means anything if the pipeline really did hand it to an operator", landed, lifecycle.Quarantined)
	}

	quarantining, ordinary := transitionsInto(t, stream.String())
	if quarantining == nil {
		t.Fatalf("the event stream carries no lifecycle_transition into %s.\nstream:\n%s", lifecycle.Quarantined, stream.String())
	}
	if quarantining["level"] != "ERROR" {
		t.Errorf("the transition into %s was logged at %v; an operator asking `activity --follow --severity error` for a backup that did not happen has to be shown this line",
			lifecycle.Quarantined, quarantining["level"])
	}
	if quarantining["result"] != "error" {
		t.Errorf("the transition into %s states result %v, want \"error\"", lifecycle.Quarantined, quarantining["result"])
	}

	// The control, and it is the half that keeps the call site honest: a
	// site that classified every transition as a failure would pass every
	// assertion above and turn a routine cycle into a wall of red.
	if ordinary == nil {
		t.Fatalf("the event stream carries no ordinary transition at all, so nothing here proves the call site is classifying rather than shouting.\nstream:\n%s", stream.String())
	}
	if ordinary["level"] != "INFO" {
		t.Errorf("an ordinary transition (%v to %v) was logged at %v, want INFO", ordinary["from"], ordinary["to"], ordinary["level"])
	}
	if _, stated := ordinary["result"]; stated {
		t.Errorf("an ordinary transition (%v to %v) states a result (%v); which resting states read as good news is a decision about a screen",
			ordinary["from"], ordinary["to"], ordinary["result"])
	}
}

// transitionsInto splits the stream's lifecycle_transition lines into the
// first one that landed in an exceptional state and the first that did
// not, which is the pair every assertion above needs.
func transitionsInto(t *testing.T, stream string) (exceptional, ordinary map[string]any) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(stream), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("the event stream carries a line that is not JSON: %q", line)
		}
		if event["event"] != obs.EventLifecycleTransition {
			continue
		}
		to, _ := event["to"].(string)
		if lifecycle.IsExceptionalState(lifecycle.State(to)) {
			if exceptional == nil {
				exceptional = event
			}
			continue
		}
		if ordinary == nil {
			ordinary = event
		}
	}
	return exceptional, ordinary
}
