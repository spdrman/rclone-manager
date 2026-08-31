package webhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/service"
)

// readSurfaceRouter is issue #211's routes over the plain syncFakeBackend,
// whose fields a test arranges directly. One helper rather than one per
// file: every route below is exercised the same way, and a second copy of
// this boilerplate is a second place for the platform/gate wiring to drift.
type readSurfaceRouter struct {
	router  http.Handler
	backend *syncFakeBackend
}

func newReadSurfaceRouter(t *testing.T) readSurfaceRouter {
	t.Helper()
	backend := newSyncFakeBackend()
	return readSurfaceRouter{
		router: NewRouter(RouterConfig{
			Platform:      allowingPlatform("alice"),
			Backend:       backend,
			Gate:          alwaysPassGate{},
			BinaryVersion: "test",
			Commit:        "test",
		}),
		backend: backend,
	}
}

func (r readSurfaceRouter) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func (r readSurfaceRouter) post(t *testing.T, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
}

func mustStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, want, rec.Body.String())
	}
}

var testArtifactFixture = service.Artifact{
	ID:                "production/postgres/backup.dump",
	BackupSetID:       "production/postgres",
	SourceName:        "production",
	SetName:           "postgres",
	Name:              "backup.dump",
	RemotePath:        "/backups/backup.dump",
	LocalPath:         "/data/backups/backup.dump",
	State:             "COMPLETE",
	DiscoveredAt:      time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC),
	UpdatedAt:         time.Date(2026, 8, 30, 9, 5, 0, 0, time.UTC),
	SizeBytes:         4096,
	Checksum:          "abc123",
	ChecksumAlgorithm: "sha256",
	Validation:        "passed",
}

var testQuarantinedFixture = service.Artifact{
	ID:               "production/postgres/bad.dump",
	BackupSetID:      "production/postgres",
	SourceName:       "production",
	SetName:          "postgres",
	Name:             "bad.dump",
	State:            "QUARANTINED",
	DiscoveredAt:     time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC),
	UpdatedAt:        time.Date(2026, 8, 30, 8, 1, 0, 0, time.UTC),
	Validation:       "failed",
	Quarantined:      true,
	QuarantineReason: "recomputed hash does not match",
}

// ------------------------------------------------------------- backups ---

func TestListArtifacts_ReportsEveryFieldOnTheWire(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.artifacts = []service.Artifact{testArtifactFixture}

	rec := rt.get(t, "/api/v1/backups")
	mustStatus(t, rec, http.StatusOK)

	var body listArtifactsResponse
	decodeInto(t, rec, &body)
	if len(body.Artifacts) != 1 {
		t.Fatalf("len = %d, want 1", len(body.Artifacts))
	}
	a := body.Artifacts[0]
	if a.ID != testArtifactFixture.ID || a.BackupSetID != testArtifactFixture.BackupSetID {
		t.Errorf("identity = %+v", a)
	}
	if a.State != "COMPLETE" || a.Validation != "passed" {
		t.Errorf("state/validation = %q/%q", a.State, a.Validation)
	}
	if a.DiscoveredAt != "2026-08-30T09:00:00Z" {
		t.Errorf("DiscoveredAt = %q, want RFC3339", a.DiscoveredAt)
	}
	if a.SizeBytes != 4096 {
		t.Errorf("SizeBytes = %d", a.SizeBytes)
	}
	// A timestamp for an event that has not happened is OMITTED, never a
	// zero-valued date: "0001-01-01T00:00:00Z" reaching a screen renders
	// like a real date, and reads as one.
	if strings.Contains(rec.Body.String(), "remote_source_removed_at") {
		t.Errorf("the body carries remote_source_removed_at for a source that is still there: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "0001-01-01") {
		t.Errorf("a zero time reached the wire: %s", rec.Body.String())
	}
}

// TestListArtifacts_PassesTheSetFilterThrough. The unfiltered request is
// the positive control: without it, a filter that never reached the
// backend at all would look the same as one that did.
func TestListArtifacts_PassesTheSetFilterThrough(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.artifacts = []service.Artifact{testArtifactFixture}

	rt.get(t, "/api/v1/backups")
	if got := rt.backend.lastArtifactFilter; got.BackupSetID != "" || got.QuarantinedOnly {
		t.Fatalf("an unfiltered request reached the backend as %+v, so the assertion below would prove nothing", got)
	}

	rt.get(t, "/api/v1/backups?setId=production%2Fpostgres")
	if got := rt.backend.lastArtifactFilter; got.BackupSetID != "production/postgres" {
		t.Errorf("filter = %+v, want BackupSetID production/postgres", got)
	}
}

// TestListArtifacts_AnUnknownSetFilterIsRefusedRatherThanAnsweredWithNothing.
// The rule is issue #187's, and this is the API side of it: an empty list
// has to keep meaning "this backup set exists and holds no backups yet".
func TestListArtifacts_AnUnknownSetFilterIsRefusedRatherThanAnsweredWithNothing(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnArtifacts = fmt.Errorf("%w: gone/away", service.ErrBackupSetNotFound)

	rec := rt.get(t, "/api/v1/backups?setId=gone/away")
	mustStatus(t, rec, http.StatusNotFound)
	if code := errorCodeOf(t, rec); code != "BACKUP_SET_NOT_FOUND" {
		t.Errorf("code = %q, want BACKUP_SET_NOT_FOUND", code)
	}
}

// TestListArtifacts_AKnownSetWithNoBackupsIsAnEmptyArray is the other half
// of that rule, and the control for it: the empty answer still exists, and
// it is an empty ARRAY rather than a null, so a client mapping over the
// field does not have to guard against it being absent.
func TestListArtifacts_AKnownSetWithNoBackupsIsAnEmptyArray(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.get(t, "/api/v1/backups?setId=production%2Fpostgres")
	mustStatus(t, rec, http.StatusOK)

	var body listArtifactsResponse
	decodeInto(t, rec, &body)
	if len(body.Artifacts) != 0 {
		t.Errorf("len = %d, want 0", len(body.Artifacts))
	}
	if !strings.Contains(rec.Body.String(), `"artifacts":[]`) {
		t.Errorf("empty result is not an empty array: %s", rec.Body.String())
	}
}

func TestListArtifacts_AFailedReadIs500Internal(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnArtifacts = errors.New("journal is unreadable: /var/lib/secret.db")

	rec := rt.get(t, "/api/v1/backups")
	mustStatus(t, rec, http.StatusInternalServerError)
	if code := errorCodeOf(t, rec); code != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL", code)
	}
	// The underlying error names a filesystem path. It must not be echoed.
	if strings.Contains(rec.Body.String(), "/var/lib") {
		t.Errorf("the response echoed the underlying error: %s", rec.Body.String())
	}
}

func TestGetArtifact_ReadsTheThreePartIdentity(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.artifacts = []service.Artifact{testArtifactFixture}

	rec := rt.get(t, "/api/v1/backups/production/postgres/backup.dump")
	mustStatus(t, rec, http.StatusOK)

	var body artifactResponse
	decodeInto(t, rec, &body)
	if body.ID != testArtifactFixture.ID {
		t.Errorf("ID = %q, want %q", body.ID, testArtifactFixture.ID)
	}
}

func TestGetArtifact_UnknownIDIs404ArtifactNotFound(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.get(t, "/api/v1/backups/production/postgres/missing.dump")
	mustStatus(t, rec, http.StatusNotFound)
	if code := errorCodeOf(t, rec); code != "ARTIFACT_NOT_FOUND" {
		t.Errorf("code = %q, want ARTIFACT_NOT_FOUND", code)
	}
}

// ---------------------------------------------------------- quarantine ---

// TestListQuarantine_AsksTheBackendForTheQuarantinedSubset. The plain
// /backups request is the control: it proves the flag is not simply always
// set.
func TestListQuarantine_AsksTheBackendForTheQuarantinedSubset(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.artifacts = []service.Artifact{testArtifactFixture, testQuarantinedFixture}

	all := rt.get(t, "/api/v1/backups")
	mustStatus(t, all, http.StatusOK)
	if rt.backend.lastArtifactFilter.QuarantinedOnly {
		t.Fatal("GET /backups asked for the quarantined subset, so the assertion below would prove nothing")
	}

	rec := rt.get(t, "/api/v1/quarantine")
	mustStatus(t, rec, http.StatusOK)
	if !rt.backend.lastArtifactFilter.QuarantinedOnly {
		t.Error("GET /quarantine did not ask for the quarantined subset")
	}

	var body listArtifactsResponse
	decodeInto(t, rec, &body)
	if len(body.Artifacts) != 1 || body.Artifacts[0].Name != "bad.dump" {
		t.Fatalf("body = %+v, want just the quarantined artifact", body.Artifacts)
	}
	if !body.Artifacts[0].Quarantined || body.Artifacts[0].QuarantineReason == "" {
		t.Errorf("a quarantined artifact reached the wire with no reason: %+v", body.Artifacts[0])
	}
}

func TestRevalidateArtifact_ReportsTheVerdict(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.revalidateResult = service.ArtifactCheck{Checked: true, Passed: false, Reason: "hash no longer matches"}

	rec := rt.post(t, "/api/v1/quarantine/production/postgres/bad.dump/revalidate", "")
	mustStatus(t, rec, http.StatusOK)

	if got := rt.backend.lastRevalidated; got != "production/postgres/bad.dump" {
		t.Errorf("the handler asked about %q, want production/postgres/bad.dump", got)
	}

	var body artifactCheckResponse
	decodeInto(t, rec, &body)
	if !body.Checked || body.Passed || body.Reason != "hash no longer matches" {
		t.Errorf("body = %+v", body)
	}
}

func TestRetryArtifactIngestion_Returns204AndNamesTheArtifact(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.post(t, "/api/v1/quarantine/production/postgres/bad.dump/retry", "")
	mustStatus(t, rec, http.StatusNoContent)
	if got := rt.backend.lastRetried; got != "production/postgres/bad.dump" {
		t.Errorf("the handler asked about %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 carried a body: %s", rec.Body.String())
	}
}

// Reinstating is the third quarantine action (issue #220), and its wire
// shape has to carry two different things at once: whether the artifact
// actually moved, and what the checks found. A caller that only learned
// "it did not work" could not tell "the copy is bad" from "the copy is
// fine but this needs the validator to run".
func TestReinstateArtifact_ReportsWhetherTheArtifactMovedAndWhatWasFound(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.reinstateResult = service.ArtifactReinstatement{
		Checked:    true,
		Passed:     true,
		Reason:     "recomputed hash still matches the hash recorded at verification",
		Reinstated: true,
		State:      "COMMITTED",
	}

	rec := rt.post(t, "/api/v1/quarantine/production/postgres/bad.dump/reinstate", "")
	mustStatus(t, rec, http.StatusOK)

	if got := rt.backend.lastReinstated; got != "production/postgres/bad.dump" {
		t.Errorf("the handler asked about %q, want production/postgres/bad.dump", got)
	}

	var body artifactReinstateResponse
	decodeInto(t, rec, &body)
	if !body.Reinstated || body.State != "COMMITTED" || !body.Passed || body.Reason == "" {
		t.Errorf("body = %+v", body)
	}
}

// A failing check is a verdict about the backup, not a failed request, so
// it comes back 200 with reinstated false, exactly the way revalidate
// reports a failing verdict. Only a refusal the operator has to change
// something to clear is a 409.
func TestReinstateArtifact_ReportsAFailingCheckAsAVerdictNotAnError(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.reinstateResult = service.ArtifactReinstatement{
		Checked: true,
		Passed:  false,
		Reason:  "local final file now hashes to abc, but the sha256 hash recorded at verification was def",
	}

	rec := rt.post(t, "/api/v1/quarantine/production/postgres/bad.dump/reinstate", "")
	mustStatus(t, rec, http.StatusOK)

	var body artifactReinstateResponse
	decodeInto(t, rec, &body)
	if body.Reinstated {
		t.Error("a failing check reported reinstated = true")
	}
	if body.State != "" {
		t.Errorf("state = %q, want it omitted when nothing moved", body.State)
	}
	if body.Passed || body.Reason == "" {
		t.Errorf("body = %+v, want the verdict and its reason", body)
	}
}

// TestQuarantineActions_MapEveryRefusalToItsDeclaredStatus. Each row is a
// refusal an operator reaches by clicking a button on a screen that has
// gone stale, so each must be a typed answer rather than a 500.
func TestQuarantineActions_MapEveryRefusalToItsDeclaredStatus(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		arrange    func(*syncFakeBackend)
		wantStatus int
		wantCode   string
	}{
		{
			name:       "revalidating a backup that is not there",
			target:     "/api/v1/quarantine/production/postgres/gone.dump/revalidate",
			arrange:    func(b *syncFakeBackend) { b.errOnRevalidate = fmt.Errorf("%w: gone", service.ErrArtifactNotFound) },
			wantStatus: http.StatusNotFound,
			wantCode:   "ARTIFACT_NOT_FOUND",
		},
		{
			name:       "revalidating a backup that is not quarantined",
			target:     "/api/v1/quarantine/production/postgres/backup.dump/revalidate",
			arrange:    func(b *syncFakeBackend) { b.errOnRevalidate = fmt.Errorf("%w: x", service.ErrArtifactNotQuarantined) },
			wantStatus: http.StatusConflict,
			wantCode:   "ARTIFACT_NOT_QUARANTINED",
		},
		{
			name:       "retrying a backup with no source left to re-ingest",
			target:     "/api/v1/quarantine/production/postgres/lost.dump/retry",
			arrange:    func(b *syncFakeBackend) { b.errOnRetry = fmt.Errorf("%w: lost", service.ErrArtifactIrrecoverable) },
			wantStatus: http.StatusConflict,
			wantCode:   "ARTIFACT_IRRECOVERABLE",
		},
		{
			name:       "retrying a backup that is not quarantined",
			target:     "/api/v1/quarantine/production/postgres/backup.dump/retry",
			arrange:    func(b *syncFakeBackend) { b.errOnRetry = fmt.Errorf("%w: x", service.ErrArtifactNotQuarantined) },
			wantStatus: http.StatusConflict,
			wantCode:   "ARTIFACT_NOT_QUARANTINED",
		},
		{
			name:       "reinstating a backup that is not there",
			target:     "/api/v1/quarantine/production/postgres/gone.dump/reinstate",
			arrange:    func(b *syncFakeBackend) { b.errOnReinstate = fmt.Errorf("%w: gone", service.ErrArtifactNotFound) },
			wantStatus: http.StatusNotFound,
			wantCode:   "ARTIFACT_NOT_FOUND",
		},
		{
			name:       "reinstating a backup that is not quarantined",
			target:     "/api/v1/quarantine/production/postgres/backup.dump/reinstate",
			arrange:    func(b *syncFakeBackend) { b.errOnReinstate = fmt.Errorf("%w: x", service.ErrArtifactNotQuarantined) },
			wantStatus: http.StatusConflict,
			wantCode:   "ARTIFACT_NOT_QUARANTINED",
		},
		{
			name:   "reinstating on evidence that is not enough to re-trust the backup",
			target: "/api/v1/quarantine/production/postgres/backup.dump/reinstate",
			arrange: func(b *syncFakeBackend) {
				b.errOnReinstate = fmt.Errorf("%w: nothing that could have failed was checked", service.ErrReinstatementRefused)
			},
			wantStatus: http.StatusConflict,
			wantCode:   "REINSTATEMENT_REFUSED",
		},
		{
			name:       "an unclassified failure",
			target:     "/api/v1/quarantine/production/postgres/backup.dump/retry",
			arrange:    func(b *syncFakeBackend) { b.errOnRetry = errors.New("disk error under /var/lib/state.db") },
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newReadSurfaceRouter(t)
			tc.arrange(rt.backend)

			rec := rt.post(t, tc.target, "")
			mustStatus(t, rec, tc.wantStatus)
			if code := errorCodeOf(t, rec); code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if strings.Contains(rec.Body.String(), "/var/lib") {
				t.Errorf("the response echoed an underlying error: %s", rec.Body.String())
			}
		})
	}
}

// ------------------------------------------------------------ activity ---

func TestListActivity_ReportsTheTransitionLog(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.activity = []service.ActivityEvent{
		{
			ArtifactID: "production/postgres/backup.dump", BackupSetID: "production/postgres",
			SourceName: "production", SetName: "postgres", ArtifactName: "backup.dump",
			From: "COMMITTED", To: "COMPLETE",
			OccurredAt: time.Date(2026, 8, 30, 9, 5, 0, 0, time.UTC),
			Detail:     "remote source released",
		},
		{
			ArtifactID: "production/postgres/backup.dump", BackupSetID: "production/postgres",
			SourceName: "production", SetName: "postgres", ArtifactName: "backup.dump",
			From: "", To: "DISCOVERED",
			OccurredAt: time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC),
		},
	}

	rec := rt.get(t, "/api/v1/activity")
	mustStatus(t, rec, http.StatusOK)

	var body listActivityResponse
	decodeInto(t, rec, &body)
	if len(body.Events) != 2 {
		t.Fatalf("len = %d, want 2", len(body.Events))
	}
	if body.Events[0].To != "COMPLETE" || body.Events[0].From != "COMMITTED" {
		t.Errorf("first event = %+v", body.Events[0])
	}
	if body.Events[0].OccurredAt != "2026-08-30T09:05:00Z" {
		t.Errorf("OccurredAt = %q", body.Events[0].OccurredAt)
	}
	// The first transition leaves nothing, so "from" is omitted rather
	// than sent as an empty string a client would have to special-case.
	second, err := json.Marshal(body.Events[1])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(second), `"from"`) {
		t.Errorf("the discovery event carries a from: %s", second)
	}
}

// TestListActivity_LimitIsAdvisoryInBothDirections: a page that is only
// trying to render a list must not be failed over a query parameter.
func TestListActivity_LimitIsAdvisoryInBothDirections(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 0},
		{"?limit=25", 25},
		{"?limit=not-a-number", 0},
		{"?limit=-3", -3},
	} {
		t.Run("limit"+tc.query, func(t *testing.T) {
			rt := newReadSurfaceRouter(t)
			rec := rt.get(t, "/api/v1/activity"+tc.query)
			mustStatus(t, rec, http.StatusOK)
			if got := rt.backend.lastActivityLimit; got != tc.want {
				t.Errorf("limit reached the backend as %d, want %d", got, tc.want)
			}
		})
	}
}

func TestListActivity_AFailedReadIs500Internal(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnActivity = errors.New("boom")

	rec := rt.get(t, "/api/v1/activity")
	mustStatus(t, rec, http.StatusInternalServerError)
	if code := errorCodeOf(t, rec); code != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL", code)
	}
}

// ---------------------------------------------------------- operations ---

// TestListOperations_IsNoLongerA405 is the direct regression for what
// issue #211 measured: the contract declared POST on this path and
// nothing else, so the shared UI's live-operations poll got a 405 from
// every real backend.
func TestListOperations_IsNoLongerA405(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.operationList = []service.Operation{
		{ID: "op_2", Status: "running", Action: "run_cycle", CreatedAt: time.Date(2026, 8, 30, 9, 1, 0, 0, time.UTC)},
		{ID: "op_1", Status: "completed", Action: "run_cycle", CreatedAt: time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)},
	}

	rec := rt.get(t, "/api/v1/operations")
	mustStatus(t, rec, http.StatusOK)

	var body listOperationsResponse
	decodeInto(t, rec, &body)
	if len(body.Operations) != 2 {
		t.Fatalf("len = %d, want 2", len(body.Operations))
	}
	if body.Operations[0].OperationID != "op_2" || body.Operations[0].Status != "running" {
		t.Errorf("first operation = %+v", body.Operations[0])
	}
	if body.Operations[0].CreatedAt != "2026-08-30T09:01:00Z" {
		t.Errorf("CreatedAt = %q", body.Operations[0].CreatedAt)
	}
}

// TestSubmitOperation_StillOwnsPOSTOnTheSamePath: adding GET must not have
// widened what POST does, and the shared path is exactly where that would
// go unnoticed.
func TestSubmitOperation_StillOwnsPOSTOnTheSamePath(t *testing.T) {
	backend := newSyncFakeBackend()
	router := NewRouter(RouterConfig{
		Platform: allowingPlatform("alice"), Backend: backend,
		Gate: NotYetImplementedGate{}, BinaryVersion: "test", Commit: "test",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations",
		strings.NewReader(`{"action":"run_cycle","config_revision":"rev-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "still-gated")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: POST /operations is still behind the destructive gate", rec.Code)
	}
	if code := errorCodeOf(t, rec); code != "DESTRUCTIVE_OPERATIONS_DISABLED" {
		t.Errorf("code = %q, want DESTRUCTIVE_OPERATIONS_DISABLED", code)
	}
}

// -------------------------------------------------------------- health ---

func TestSystemHealth_ReportsEveryBackupSetsVerdict(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	free := uint64(500_000_000_000)
	rt.backend.health = service.HealthReport{
		GeneratedAt: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
		BackupSets: []service.BackupSetHealth{
			{
				BackupSetID: "production/postgres", SourceName: "production", SetName: "postgres",
				State: "HEALTHY", Reason: "a known-good backup landed 20 minutes ago",
				NewestGoodBackupAt: time.Date(2026, 8, 30, 9, 40, 0, 0, time.UTC),
				StaleAfter:         24 * time.Hour,
				FreeBytes:          free, FreeBytesKnown: true,
			},
			{
				BackupSetID: "production/media", SourceName: "production", SetName: "media",
				State: "STALE", Reason: "no known-good backup inside the freshness window",
				StaleAfter: 24 * time.Hour,
			},
		},
	}

	rec := rt.get(t, "/api/v1/system/health")
	mustStatus(t, rec, http.StatusOK)

	var body healthResponse
	decodeInto(t, rec, &body)
	if body.GeneratedAt != "2026-08-30T10:00:00Z" {
		t.Errorf("GeneratedAt = %q", body.GeneratedAt)
	}
	if len(body.BackupSets) != 2 {
		t.Fatalf("len = %d, want 2", len(body.BackupSets))
	}
	if body.BackupSets[0].State != "HEALTHY" || body.BackupSets[0].Reason == "" {
		t.Errorf("first verdict = %+v", body.BackupSets[0])
	}
	if body.BackupSets[0].StaleAfterSeconds != 86400 {
		t.Errorf("StaleAfterSeconds = %d, want 86400", body.BackupSets[0].StaleAfterSeconds)
	}
	if body.BackupSets[0].FreeBytes != free || !body.BackupSets[0].FreeBytesKnown {
		t.Errorf("free space = %d / known %v", body.BackupSets[0].FreeBytes, body.BackupSets[0].FreeBytesKnown)
	}

	// The second set has never produced a good backup and its free space
	// could not be read. Both must be ABSENT rather than zero: a zero
	// free_bytes reads as "the disk is full", and a zero timestamp renders
	// as a date in the year 1.
	second, err := json.Marshal(body.BackupSets[1])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(second), "newest_good_backup_at") {
		t.Errorf("a set with no good backup carries a timestamp: %s", second)
	}
	if strings.Contains(string(second), `"free_bytes":`) {
		t.Errorf("an unavailable capacity reading reached the wire as a number: %s", second)
	}
	if body.BackupSets[1].FreeBytesKnown {
		t.Error("FreeBytesKnown is true for a reading that was not taken")
	}
}

// TestSystemHealth_ReportsNoProcessOrBuildFact. Failure-safety invariant
// 14: process liveness is not evidence of backup freshness, and the way
// that gets violated is one endpoint reporting both.
func TestSystemHealth_ReportsNoProcessOrBuildFact(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.health = service.HealthReport{
		GeneratedAt: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
		BackupSets: []service.BackupSetHealth{
			{BackupSetID: "production/postgres", State: "HEALTHY", Reason: "fresh"},
		},
	}

	rec := rt.get(t, "/api/v1/system/health")
	mustStatus(t, rec, http.StatusOK)

	// The router is built with BinaryVersion "test" and Commit "test", and
	// GET /system/version does report both. This body must not.
	for _, forbidden := range []string{"version", "commit", "ready", "go_version"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("the health body carries %q, which is /system/version's answer: %s", forbidden, rec.Body.String())
		}
	}

	// The positive control for that scan: the endpoint that IS supposed to
	// carry those really does, so the four assertions above are about
	// where the fields are and not about the words being unfindable.
	version := rt.get(t, "/api/v1/system/version")
	mustStatus(t, version, http.StatusOK)
	for _, expected := range []string{"core_version", "commit", "ready", "go_version"} {
		if !strings.Contains(version.Body.String(), expected) {
			t.Fatalf("GET /system/version does not carry %q, so the scan above proves nothing: %s", expected, version.Body.String())
		}
	}
}

// Issue #227. A reinstated backup's remote source is preserved forever,
// and the count of them has to reach the API, not only the CLI: the Web UI
// and any scraper read this endpoint, and "how many remote sources am I
// holding" is a question asked months after the reinstatement, not at the
// moment of it.
//
// Two sets with different counts, and a zero that must be PRESENT rather
// than omitted. Unlike free_bytes, zero here is a real reading: the count
// comes from the journal the health pass already holds open, and a read
// that fails makes the whole report a 500 (the test below) instead of a
// reassuring zero.
func TestSystemHealth_ReportsReinstatedRemoteRetainedCount(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.health = service.HealthReport{
		GeneratedAt: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
		BackupSets: []service.BackupSetHealth{
			{
				BackupSetID: "production/postgres", SourceName: "production", SetName: "postgres",
				State: "HEALTHY", Reason: "fresh", StaleAfter: 24 * time.Hour,
				ReinstatedRemoteRetainedCount: 3,
			},
			{
				BackupSetID: "production/media", SourceName: "production", SetName: "media",
				State: "HEALTHY", Reason: "fresh", StaleAfter: 24 * time.Hour,
			},
		},
	}

	rec := rt.get(t, "/api/v1/system/health")
	mustStatus(t, rec, http.StatusOK)

	var body healthResponse
	decodeInto(t, rec, &body)
	if len(body.BackupSets) != 2 {
		t.Fatalf("len = %d, want 2", len(body.BackupSets))
	}
	if body.BackupSets[0].ReinstatedRemoteRetainedCount != 3 {
		t.Errorf("ReinstatedRemoteRetainedCount = %d, want 3", body.BackupSets[0].ReinstatedRemoteRetainedCount)
	}
	if body.BackupSets[1].ReinstatedRemoteRetainedCount != 0 {
		t.Errorf("second set ReinstatedRemoteRetainedCount = %d, want 0", body.BackupSets[1].ReinstatedRemoteRetainedCount)
	}

	second, err := json.Marshal(body.BackupSets[1])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(second), `"reinstated_remote_retained_count":0`) {
		t.Errorf("a set holding no reinstated remote sources omitted the count instead of reporting 0; an absent field reads as \"this build does not know\": %s", second)
	}
}

// Issue #245. A backup set the transport refuses to connect to backs up
// nothing on every cycle, and until this landed no read surface could say
// so: the alert pass computed the fact transiently and handed it to a
// notification sink, so the sets list showed the set as merely stale with
// a live run control beside it.
//
// halt_reason is omitted, never empty-stringed, for a set nothing is known
// about. That polarity is the whole point of the field being optional:
// absent means "no refusal has been observed", which is a different claim
// from "this set is fine", and #231 is the reminder of what a fabricated
// definite value costs.
func TestSystemHealth_ReportsWhyASetCouldNotBeConnectedTo(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.health = service.HealthReport{
		GeneratedAt: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
		BackupSets: []service.BackupSetHealth{
			{
				BackupSetID: "production/auth-config", SourceName: "production", SetName: "auth-config",
				State: "STALE", Reason: "no known-good backup inside the freshness window",
				StaleAfter: 24 * time.Hour,
				HaltReason: "HOST_KEY_CHANGED",
			},
			{
				BackupSetID: "production/postgres", SourceName: "production", SetName: "postgres",
				State: "HEALTHY", Reason: "fresh", StaleAfter: 24 * time.Hour,
			},
		},
	}

	rec := rt.get(t, "/api/v1/system/health")
	mustStatus(t, rec, http.StatusOK)

	var body healthResponse
	decodeInto(t, rec, &body)
	if len(body.BackupSets) != 2 {
		t.Fatalf("len = %d, want 2", len(body.BackupSets))
	}
	if body.BackupSets[0].HaltReason != "HOST_KEY_CHANGED" {
		t.Errorf("HaltReason = %q, want HOST_KEY_CHANGED", body.BackupSets[0].HaltReason)
	}

	refused, err := json.Marshal(body.BackupSets[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(refused), `"halt_reason":"HOST_KEY_CHANGED"`) {
		t.Errorf("the refused set did not carry halt_reason on the wire: %s", refused)
	}

	reachable, err := json.Marshal(body.BackupSets[1])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(reachable), "halt_reason") {
		t.Errorf("a set with no observed refusal carries halt_reason anyway: %s", reachable)
	}
	// The positive control for that absence: this scan finds the key when
	// it IS there (the marshalled refused set above), so "not found" means
	// omitted rather than the substring being unfindable in either body.
	if !strings.Contains(string(refused), "halt_reason") {
		t.Fatalf("the same scan finds nothing in a body that does carry halt_reason, so the absence above proves nothing: %s", refused)
	}
}

func TestSystemHealth_AFailedComputationIs500Internal(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnHealth = errors.New("statfs: no such file or directory")

	rec := rt.get(t, "/api/v1/system/health")
	mustStatus(t, rec, http.StatusInternalServerError)
	if code := errorCodeOf(t, rec); code != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL", code)
	}
}

// ------------------------------------------------------------- catalog ---

// TestCatalogScanAndRebuild_ShareOneShapeAndDifferOnlyInDryRun.
func TestCatalogScanAndRebuild_ShareOneShapeAndDifferOnlyInDryRun(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.catalog = service.CatalogReport{
		Scanned: 47, Reconstructed: 2, AlreadyPresent: 45,
		Failures: []service.CatalogFailure{
			{BackupSetID: "production/postgres", Path: "/data/manifests/bad.json", Reason: "unreadable"},
		},
	}

	scan := rt.post(t, "/api/v1/catalog/scan", "")
	mustStatus(t, scan, http.StatusOK)
	var scanBody catalogReportResponse
	decodeInto(t, scan, &scanBody)
	if !scanBody.DryRun {
		t.Error("a scan reported dry_run false")
	}
	if scanBody.Scanned != 47 || scanBody.Reconstructed != 2 || scanBody.AlreadyPresent != 45 {
		t.Errorf("counts = %+v", scanBody)
	}
	if len(scanBody.Failures) != 1 || scanBody.Failures[0].Reason != "unreadable" {
		t.Errorf("failures = %+v", scanBody.Failures)
	}

	rebuild := rt.post(t, "/api/v1/catalog/rebuild", "")
	mustStatus(t, rebuild, http.StatusOK)
	var rebuildBody catalogReportResponse
	decodeInto(t, rebuild, &rebuildBody)
	if rebuildBody.DryRun {
		t.Error("a rebuild reported dry_run true")
	}
	if rebuildBody.Scanned != scanBody.Scanned || rebuildBody.Reconstructed != scanBody.Reconstructed {
		t.Errorf("the rebuild and its own dry run disagree: %+v vs %+v", rebuildBody, scanBody)
	}
}

func TestCatalogRoutes_AFailedPassIs500Internal(t *testing.T) {
	for _, target := range []string{"/api/v1/catalog/scan", "/api/v1/catalog/rebuild"} {
		t.Run(target, func(t *testing.T) {
			rt := newReadSurfaceRouter(t)
			rt.backend.errOnCatalog = errors.New("boom")

			rec := rt.post(t, target, "")
			mustStatus(t, rec, http.StatusInternalServerError)
			if code := errorCodeOf(t, rec); code != "INTERNAL" {
				t.Errorf("code = %q, want INTERNAL", code)
			}
		})
	}
}

// --------------------------------------------------- enabling a set ---

func TestSetBackupSetEnabled_PassesTheIdAndFlagThrough(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			rt := newReadSurfaceRouter(t)

			rec := rt.post(t, "/api/v1/backup-sets/production/postgres/enabled",
				fmt.Sprintf(`{"enabled":%v}`, enabled))
			mustStatus(t, rec, http.StatusOK)

			got := rt.backend.lastSetEnabled
			if got.id != "production/postgres" {
				t.Errorf("id = %q, want production/postgres", got.id)
			}
			if got.enabled != enabled {
				t.Errorf("enabled = %v, want %v", got.enabled, enabled)
			}

			var body backupSetResponse
			decodeInto(t, rec, &body)
			if body.Disabled == enabled {
				t.Errorf("the response reports Disabled = %v for enabled = %v", body.Disabled, enabled)
			}
		})
	}
}

func TestSetBackupSetEnabled_UnknownSetIs404(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnSetEnabled = fmt.Errorf("%w: gone", service.ErrBackupSetNotFound)

	rec := rt.post(t, "/api/v1/backup-sets/production/gone/enabled", `{"enabled":true}`)
	mustStatus(t, rec, http.StatusNotFound)
	if code := errorCodeOf(t, rec); code != "BACKUP_SET_NOT_FOUND" {
		t.Errorf("code = %q, want BACKUP_SET_NOT_FOUND", code)
	}
}

func TestSetBackupSetEnabled_AMalformedBodyIs400(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.post(t, "/api/v1/backup-sets/production/postgres/enabled", `{"enabled":`)
	mustStatus(t, rec, http.StatusBadRequest)
	if code := errorCodeOf(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
}

// ------------------------------------------- test-connection, two modes ---

// TestTestConnection_ByBackupSetIdUsesThePersistedConfiguration is the
// rename issue #211 asks for: the shared UI's "Test connection" on an
// existing set used to POST /backup-sets/{id}/test-connection, which no
// runtime ever served.
func TestTestConnection_ByBackupSetIdUsesThePersistedConfiguration(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.persistedConnectionResult = service.ConnectionTestResult{OK: true}

	rec := rt.post(t, "/api/v1/backup-sets/test-connection", `{"backup_set_id":"production/postgres"}`)
	mustStatus(t, rec, http.StatusOK)

	if got := rt.backend.lastTestedBackupSetID; got != "production/postgres" {
		t.Errorf("the handler tested %q, want production/postgres", got)
	}

	var body testConnectionResponse
	decodeInto(t, rec, &body)
	if !body.OK {
		t.Errorf("OK = false: %+v", body)
	}
}

// TestTestConnection_RefusesBothModesAtOnce. Silently preferring one mode
// is how a caller ends up shown a green result for something it did not
// ask about.
func TestTestConnection_RefusesBothModesAtOnce(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.post(t, "/api/v1/backup-sets/test-connection",
		`{"backup_set_id":"production/postgres","host":"elsewhere.internal","port":22,"user":"u","ssh_key_id":"k","known_hosts_line":"l"}`)
	mustStatus(t, rec, http.StatusBadRequest)
	if code := errorCodeOf(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
	if rt.backend.lastTestedBackupSetID != "" {
		t.Error("the refused request still reached the backend")
	}
}

func TestTestConnection_UnknownBackupSetIs404(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnTestPersisted = fmt.Errorf("%w: gone", service.ErrBackupSetNotFound)

	rec := rt.post(t, "/api/v1/backup-sets/test-connection", `{"backup_set_id":"production/gone"}`)
	mustStatus(t, rec, http.StatusNotFound)
	if code := errorCodeOf(t, rec); code != "BACKUP_SET_NOT_FOUND" {
		t.Errorf("code = %q, want BACKUP_SET_NOT_FOUND", code)
	}
}
