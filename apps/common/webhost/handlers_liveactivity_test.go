package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/service"
)

// GET /api/v1/activity/live on the wire (issue #573).
//
// The interesting cases here are all about the same thing: the strip is
// pinned and always present, so this route has to be answerable at every
// moment of a deployment's life, and every one of those answers has to be
// distinguishable from the others. A set nothing has happened to, a set
// mid-transfer and a set that stopped with failures must not serialise to
// the same JSON, and none of them may serialise to a shape a client would
// draw as a full bar.
//
// The contract check at the bottom is the same discipline
// handlers_operations_progress_test.go already applies to the stage enum:
// a value this package serves and api/v1/openapi.json does not name is a
// value no client is obliged to handle.

func newLiveActivityTestRouter(t *testing.T) (http.Handler, *syncFakeBackend) {
	t.Helper()
	backend := newSyncFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		BinaryVersion: "test",
		Commit:        "test",
	})
	return router, backend
}

func getLiveActivity(t *testing.T, router http.Handler, query string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/live"+query, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/activity/live%s = %d, want 200; body: %s", query, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func liveSets(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["sets"].([]any)
	if !ok {
		t.Fatalf("the response carries no sets array: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for i, e := range raw {
		set, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("set %d is %#v, want an object", i, e)
		}
		out = append(out, set)
	}
	return out
}

// transferringSet is the mockup's first panel: a set mid-transfer, with a
// denominator, a rate and a tail.
func transferringSet() service.LiveActivitySet {
	total := 41
	started := time.Date(2026, 9, 7, 0, 16, 29, 0, time.UTC)
	return service.LiveActivitySet{
		BackupSetID:        "api-server/var-backups",
		Active:             true,
		Stage:              "transferring",
		Artifact:           "dpkg.status.2.gz",
		ArtifactsCompleted: 26,
		ArtifactsTotal:     &total,
		ProgressBasis:      service.LiveActivityBasisArtifacts,
		BytesTransferred:   int64p(1048576),
		BytesTotal:         int64p(4194304),
		BytesPerSecond:     int64p(4300000),
		StartedAt:          &started,
		OldestSequence:     1,
		LatestSequence:     2,
		Events: []service.LiveActivityEvent{
			{
				Sequence: 1,
				At:       started,
				Level:    "info",
				Event:    "discovery",
				Scope:    service.LiveActivityScopeSet,
				Message:  "discovery pass complete",
				Fields: []service.LiveActivityField{
					{Key: "backup_set", Value: "api-server/var-backups"},
					{Key: "discovered", Value: "41"},
					{Key: "pending", Value: "26"},
				},
			},
			{
				Sequence: 2,
				At:       started.Add(9 * time.Second),
				Level:    "info",
				Event:    "lifecycle_transition",
				Scope:    service.LiveActivityScopeSet,
				Message:  "lifecycle transition",
				Fields: []service.LiveActivityField{
					{Key: "artifact", Value: "api-server/var-backups/dpkg.status.2.gz"},
					{Key: "from", Value: "DISCOVERED"},
					{Key: "to", Value: "TRANSFERRING"},
				},
			},
		},
	}
}

func TestLiveActivity_SerializesASetMidTransfer(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: time.Date(2026, 9, 7, 0, 16, 38, 0, time.UTC),
		PollAfter:  time.Second,
		Sets:       []service.LiveActivitySet{transferringSet()},
	})

	body := getLiveActivity(t, router, "")
	if got := body["poll_after_ms"]; got != float64(1000) {
		t.Errorf("poll_after_ms = %v, want 1000: a client that has to pick its own cadence either hammers an idle NAS or watches a transfer too slowly", got)
	}
	sets := liveSets(t, body)
	if len(sets) != 1 {
		t.Fatalf("the response carries %d sets, and one was served", len(sets))
	}
	set := sets[0]

	if set["backup_set_id"] != "api-server/var-backups" {
		t.Errorf("backup_set_id = %v", set["backup_set_id"])
	}
	if set["active"] != true {
		t.Error("a set mid-transfer serialises as inactive, so nothing on the bar would move")
	}
	if set["stage"] != "transferring" || set["artifact"] != "dpkg.status.2.gz" {
		t.Errorf("stage/artifact = %v/%v, want transferring and dpkg.status.2.gz", set["stage"], set["artifact"])
	}
	if set["artifacts_completed"] != float64(26) || set["artifacts_total"] != float64(41) {
		t.Errorf("the fraction serialises as %v of %v, want 26 of 41", set["artifacts_completed"], set["artifacts_total"])
	}
	if set["progress_basis"] != "artifacts" {
		t.Errorf("progress_basis = %v; a bar whose reader has to infer what it counts gets misread", set["progress_basis"])
	}
	if set["bytes_per_second"] != float64(4300000) {
		t.Errorf("bytes_per_second = %v", set["bytes_per_second"])
	}
	if _, ok := set["finished_at"]; ok {
		t.Error("a set whose pass is still running carries a finish time")
	}

	events, ok := set["events"].([]any)
	if !ok || len(events) != 2 {
		t.Fatalf("events = %#v, want the two that were served", set["events"])
	}
	first, _ := events[0].(map[string]any)
	if first["event"] != "discovery" || first["level"] != "info" || first["scope"] != "set" {
		t.Errorf("the first event serialises as %v/%v/%v, want discovery/info/set", first["event"], first["level"], first["scope"])
	}
	if first["message"] != "discovery pass complete" {
		t.Errorf("message = %v", first["message"])
	}
	fields, ok := first["fields"].([]any)
	if !ok || len(fields) != 3 {
		t.Fatalf("fields = %#v, want the three that were served", first["fields"])
	}
	pair, _ := fields[1].(map[string]any)
	if pair["key"] != "discovered" || pair["value"] != "41" {
		t.Errorf("the second field serialises as %v=%v, want discovered=41: without the event's own fields a client can only reprint its message", pair["key"], pair["value"])
	}
	if set["oldest_sequence"] != float64(1) || set["latest_sequence"] != float64(2) {
		t.Errorf("the buffer bounds serialise as %v..%v; a client cannot notice it missed a line without them", set["oldest_sequence"], set["latest_sequence"])
	}
}

func TestLiveActivity_SerializesASetThatStoppedWithFailures(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	total := 28
	started := time.Date(2026, 9, 7, 0, 16, 0, 0, time.UTC)
	finished := started.Add(55 * time.Second)
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: finished,
		PollAfter:  10 * time.Second,
		Sets: []service.LiveActivitySet{{
			BackupSetID:        "cicd-pipeline/var-backups",
			Active:             false,
			ArtifactsCompleted: 26,
			ArtifactsTotal:     &total,
			ProgressBasis:      service.LiveActivityBasisArtifacts,
			Failures:           2,
			StartedAt:          &started,
			FinishedAt:         &finished,
			OldestSequence:     1,
			LatestSequence:     1,
			Events: []service.LiveActivityEvent{{
				Sequence: 1,
				At:       finished,
				Level:    "error",
				Event:    "lifecycle_transition",
				Scope:    service.LiveActivityScopeSet,
				Message:  "lifecycle transition",
				Fields: []service.LiveActivityField{
					{Key: "artifact", Value: "cicd-pipeline/var-backups/dpkg.status.1.gz"},
					{Key: "to", Value: "FAILED"},
					{Key: "detail", Value: "md5 differs: source a70969a2 destination d41d8cd9 (empty)"},
				},
			}},
		}},
	})

	set := liveSets(t, getLiveActivity(t, router, ""))[0]
	if set["active"] != false {
		t.Error("a set that stopped serialises as active")
	}
	if set["failures"] != float64(2) {
		t.Errorf("failures = %v, want 2: the count is the difference between a finished cycle and one an operator has to go and look at", set["failures"])
	}
	if set["finished_at"] == nil || set["finished_at"] == "" {
		t.Errorf("finished_at = %v; an idle strip has nothing to say about the last cycle without it", set["finished_at"])
	}
	events, _ := set["events"].([]any)
	first, _ := events[0].(map[string]any)
	if first["level"] != "error" {
		t.Errorf("level = %v, want error", first["level"])
	}
	// The detail is what turns "integrity failure" into something an
	// operator can act on without opening a log.
	fields, _ := first["fields"].([]any)
	found := false
	for _, f := range fields {
		pair, _ := f.(map[string]any)
		if pair["key"] == "detail" {
			found = true
			if pair["value"] == "" {
				t.Error("the detail field serialises empty")
			}
		}
	}
	if !found {
		t.Errorf("the failing event carries no detail field: %v", fields)
	}
}

func TestLiveActivity_SerializesAnIdleSetWithNothingToSay(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC),
		PollAfter:  10 * time.Second,
		Sets: []service.LiveActivitySet{{
			BackupSetID:   "nas-media/photos",
			ProgressBasis: service.LiveActivityBasisUnknown,
		}},
	})

	body := getLiveActivity(t, router, "")
	if got := body["poll_after_ms"]; got != float64(10000) {
		t.Errorf("poll_after_ms = %v, want 10000 while nothing is running", got)
	}
	set := liveSets(t, body)[0]
	if set["active"] != false {
		t.Error("a set with no cycle serialises as active")
	}
	if set["progress_basis"] != "unknown" {
		t.Errorf("progress_basis = %v, want unknown", set["progress_basis"])
	}
	if _, ok := set["artifacts_total"]; ok {
		t.Error("a set with nothing discovered carries an artifacts_total; a denominator of zero draws as a finished cycle")
	}
	if set["artifacts_completed"] != float64(0) {
		t.Errorf("artifacts_completed = %v, want 0", set["artifacts_completed"])
	}
	events, ok := set["events"].([]any)
	if !ok {
		t.Fatalf("events = %#v; an empty feed is an empty array, never a missing key a client has to guard", set["events"])
	}
	if len(events) != 0 {
		t.Errorf("events = %v, want none", events)
	}
}

func TestLiveActivity_PassesTheFilterAndTheCursorThrough(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.setLiveActivity(service.LiveActivity{PollAfter: time.Second})

	getLiveActivity(t, router, "?backup_set=alpha%2Fnightly&since=42&limit=10")

	got := backend.lastLiveActivityRequest()
	if got.BackupSetID != "alpha/nightly" {
		t.Errorf("the backend was asked for backup set %q, and the request named alpha/nightly", got.BackupSetID)
	}
	if got.Since != 42 {
		t.Errorf("the backend was asked for events after %d, and the request said 42", got.Since)
	}
	if got.Limit != 10 {
		t.Errorf("the backend was given limit %d, and the request said 10", got.Limit)
	}
}

func TestLiveActivity_AnUnparseableCursorIsTheBeginningRatherThanARefusal(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.setLiveActivity(service.LiveActivity{PollAfter: time.Second})

	// A strip is only ever trying to render a list. Refusing over a
	// malformed number would blank the panel an operator went to look at.
	getLiveActivity(t, router, "?since=not-a-number&limit=banana")
	if got := backend.lastLiveActivityRequest(); got.Since != 0 || got.Limit != 0 {
		t.Errorf("an unparseable cursor and limit reached the backend as %d/%d, and both should read as absent", got.Since, got.Limit)
	}
}

func TestLiveActivity_RefusesAnUnknownBackupSetWith404(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.errOnLiveActivity = service.ErrBackupSetNotFound

	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/live?backup_set=alpha%2Fgone", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET for a backup set this deployment does not have = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
}

func TestLiveActivity_IsUnreachableWithoutAuthentication(t *testing.T) {
	backend := newSyncFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      fakePlatformAdapter{auth: fakeAuthenticator{authenticated: false}},
		Backend:       backend,
		BinaryVersion: "test",
		Commit:        "test",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/live", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated GET = %d, want 401", rec.Code)
	}
}

// TestLiveActivity_ScopesAndBasesAreExactlyTheContractsEnums holds the two
// small vocabularies this feed introduces to the contract, in both
// directions. A value core/service can produce and the contract does not
// name is a value no client is obliged to handle; a value the contract
// names and nothing produces is a branch every client writes for nothing.
func TestLiveActivity_ScopesAndBasesAreExactlyTheContractsEnums(t *testing.T) {
	path := filepath.Join("..", "..", "..", "api", "v1", "openapi.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the contract at %s: %v", path, err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the contract: %v", err)
	}

	for _, c := range []struct {
		schema, property string
		want             []string
	}{
		{"LiveActivityEvent", "scope", []string{service.LiveActivityScopeDeployment, service.LiveActivityScopeSet}},
		{"LiveActivitySet", "progress_basis", []string{service.LiveActivityBasisArtifacts, service.LiveActivityBasisUnknown}},
		// The stage names are the SAME closed set OperationProgress.stage
		// already declares, and they are registered here for the reason
		// internal/retention's managed-complete guard exists: a closed
		// vocabulary written down twice is a closed vocabulary that
		// drifts, and this feed is the second place it is written down.
		{"LiveActivitySet", "stage", service.OperationStages},
		{"LiveActivitySet", "outcome", service.LiveActivityOutcomes},
		// How an EVENT went, which is a different vocabulary from how a
		// set's pass ended one row up, and is spelled `result` rather
		// than `outcome` for exactly that reason. Registered here
		// because internal/obs owns it, the contract writes it down a
		// second time, and a closed vocabulary written down twice is one
		// that drifts (issue #625).
		{"LiveActivityEvent", "result", service.LiveActivityEventResults},
	} {
		declared := doc.Components.Schemas[c.schema].Properties[c.property].Enum
		if len(declared) == 0 {
			t.Errorf("the contract declares no %s.%s enum, so this comparison would pass vacuously", c.schema, c.property)
			continue
		}
		if len(declared) != len(c.want) {
			t.Errorf("%s.%s: the contract declares %v and core/service produces %v", c.schema, c.property, declared, c.want)
			continue
		}
		for i := range declared {
			if declared[i] != c.want[i] {
				t.Errorf("%s.%s value %d: the contract says %q, core/service says %q", c.schema, c.property, i, declared[i], c.want[i])
			}
		}
	}
}

// TestLiveActivity_SerializesTheFactsThatKeepTheFeedHonest covers the
// four fields a client cannot do without and that nothing above reaches:
// which process this reading came from, how the last pass ended, whether
// a limit cut the reading short, and whether the caller's cursor fell off
// the back of the buffer.
//
// They matter on the wire rather than only in core/service because each
// one exists to stop a browser presenting a claim it cannot support: a
// dead process's log shown as live, a failed pass shown as an idle one, a
// page shown as the end of the feed, and a hole shown as continuity.
func TestLiveActivity_SerializesTheFactsThatKeepTheFeedHonest(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	set := transferringSet()
	set.Outcome = service.LiveActivityOutcomeFailed
	set.Truncated = true
	set.Dropped = true
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: time.Date(2026, 9, 7, 0, 16, 38, 0, time.UTC),
		Epoch:      "7f3a91c2d0b45e68",
		PollAfter:  time.Second,
		Sets:       []service.LiveActivitySet{set},
	})

	body := getLiveActivity(t, router, "")
	if body["epoch"] != "7f3a91c2d0b45e68" {
		t.Errorf("the reading carries epoch %v; without it a client that polled across a restart holds a dead process's lines and asks for everything after a sequence the live one has not reached",
			body["epoch"])
	}
	got := liveSets(t, body)[0]
	if got["outcome"] != service.LiveActivityOutcomeFailed {
		t.Errorf("the set serialises outcome %v, want %q", got["outcome"], service.LiveActivityOutcomeFailed)
	}
	if got["truncated"] != true {
		t.Errorf("the set serialises truncated %v, and this reading was cut short", got["truncated"])
	}
	if got["dropped"] != true {
		t.Errorf("the set serialises dropped %v, and this caller's cursor fell off the back of the buffer", got["dropped"])
	}

	// The two flags are always present, never absent-meaning-false: a
	// client reading an absent key as "no gap" is a client that would
	// read a server that stopped sending them the same way.
	plain := transferringSet()
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: time.Date(2026, 9, 7, 0, 16, 38, 0, time.UTC),
		Epoch:      "7f3a91c2d0b45e68",
		PollAfter:  time.Second,
		Sets:       []service.LiveActivitySet{plain},
	})
	quiet := liveSets(t, getLiveActivity(t, router, ""))[0]
	for _, key := range []string{"truncated", "dropped"} {
		if v, ok := quiet[key]; !ok || v != false {
			t.Errorf("a reading with nothing wrong serialises %q as %v (present=%v), want an explicit false", key, v, ok)
		}
	}
	if _, ok := quiet["outcome"]; ok {
		t.Errorf("a set whose pass has not ended serialises an outcome anyway (%v); absent is how the feed says it has no verdict yet", quiet["outcome"])
	}
}

// The deployment bucket on the wire (issues #593 and #599).
//
// It is a separate key rather than a synthetic set, and the two cases
// below are why. A client has to be able to tell "the deployment has said
// nothing" from "I did not ask", because the first is a quiet system and
// the second is a narrowed reading, and a bucket that serialised the same
// way for both would have the global terminal go blank whenever a set
// page happened to be the thing polling.

func deploymentBucket() *service.LiveActivityDeployment {
	return &service.LiveActivityDeployment{
		Events: []service.LiveActivityEvent{{
			Sequence: 412,
			At:       time.Date(2026, 9, 7, 0, 16, 21, 0, time.UTC),
			Level:    "info",
			Event:    "cycle_start",
			Scope:    service.LiveActivityScopeDeployment,
			Message:  "cycle starting",
			Fields:   []service.LiveActivityField{{Key: "cycle_id", Value: "c1"}},
		}},
		OldestSequence: 400,
		LatestSequence: 412,
	}
}

func TestLiveActivity_SerializesTheDeploymentsOwnLog(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: time.Date(2026, 9, 7, 0, 16, 30, 0, time.UTC),
		Epoch:      "e1",
		PollAfter:  time.Second,
		Sets:       []service.LiveActivitySet{transferringSet()},
		Deployment: deploymentBucket(),
	})

	body := getLiveActivity(t, router, "")
	bucket, ok := body["deployment"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries no deployment bucket: %v", body)
	}
	events, ok := bucket["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("the deployment bucket carries %v", bucket["events"])
	}
	event, ok := events[0].(map[string]any)
	if !ok {
		t.Fatalf("the deployment's event is %#v, want an object", events[0])
	}
	if event["scope"] != service.LiveActivityScopeDeployment {
		t.Errorf("the deployment's own line is scoped %v", event["scope"])
	}
	if event["sequence"] != float64(412) {
		t.Errorf("the deployment's line reports sequence %v; the counter is one counter for the whole process and the terminal orders both buckets by it", event["sequence"])
	}
	// The two honesty flags are present even when nothing is missing, for
	// the same reason a set's are: an absent key and a false one must not
	// be the same reading.
	for _, key := range []string{"truncated", "dropped"} {
		if _, present := bucket[key]; !present {
			t.Errorf("the deployment bucket omits %q; a client cannot tell a server that has nothing missing from one that stopped saying", key)
		}
	}
	if bucket["latest_sequence"] != float64(412) || bucket["oldest_sequence"] != float64(400) {
		t.Errorf("the deployment bucket reports bounds %v..%v", bucket["oldest_sequence"], bucket["latest_sequence"])
	}
}

// A deployment with no configured backup sets is the case that used to
// have no answer at all: the reading was built by walking the configured
// sets, so a fresh install served "sets": [] and the bucket behind it was
// unreachable. That is exactly the operator clicking through a wizard
// with nothing set up yet.
func TestLiveActivity_ServesTheDeploymentWithNoConfiguredSets(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: time.Date(2026, 9, 7, 0, 16, 30, 0, time.UTC),
		Epoch:      "e1",
		PollAfter:  10 * time.Second,
		Deployment: deploymentBucket(),
	})

	body := getLiveActivity(t, router, "")
	if sets := liveSets(t, body); len(sets) != 0 {
		t.Fatalf("a deployment with nothing configured reported %d sets", len(sets))
	}
	bucket, ok := body["deployment"].(map[string]any)
	if !ok {
		t.Fatalf("a deployment with nothing configured carries no deployment bucket, so the wizard's own lines reach no screen: %v", body)
	}
	if events, _ := bucket["events"].([]any); len(events) != 1 {
		t.Errorf("the deployment bucket carries %d events", len(events))
	}
}

// scope=deployment is the request half of the same distinction, and it is
// what a terminal following the deployment's log alone asks for. It is
// advisory like every other parameter on this route: an unknown value is
// ignored rather than refused.
func TestLiveActivity_ScopeDeploymentNarrowsTheReading(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: time.Date(2026, 9, 7, 0, 16, 30, 0, time.UTC),
		Epoch:      "e1",
		Deployment: deploymentBucket(),
	})

	getLiveActivity(t, router, "?scope=deployment")
	if got := backend.lastLiveActivityRequest(); !got.DeploymentOnly {
		t.Errorf("scope=deployment reached the service as %+v; a caller asking for the deployment's log alone got every set's as well", got)
	}

	getLiveActivity(t, router, "?scope=nonsense")
	if got := backend.lastLiveActivityRequest(); got.DeploymentOnly {
		t.Errorf("an unrecognised scope narrowed the reading to the deployment; every parameter on this route is advisory, because blanking the panel an operator went to look at over a query string is the one thing this feed exists not to do")
	}
}

// A reading narrowed to one backup set omits the bucket rather than
// sending an empty one, which is the distinction the omitempty on the
// wire exists for.
func TestLiveActivity_ASetScopedReadingOmitsTheDeploymentBucket(t *testing.T) {
	router, backend := newLiveActivityTestRouter(t)
	backend.setLiveActivity(service.LiveActivity{
		ObservedAt: time.Date(2026, 9, 7, 0, 16, 30, 0, time.UTC),
		Epoch:      "e1",
		Sets:       []service.LiveActivitySet{transferringSet()},
	})

	body := getLiveActivity(t, router, "?backup_set=api-server/var-backups")
	if _, present := body["deployment"]; present {
		t.Errorf("a reading narrowed to one set still carries a deployment bucket: %v", body["deployment"])
	}
}
