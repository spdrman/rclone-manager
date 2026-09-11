package webhost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/backupd/core/cliecho"
	"github.com/spdrman/backupd/core/service"
)

// The parity guard, and the recording it rides on (issue #599).
//
// The first test is the one that earns the feature. EPIC G requires every
// capability to exist on both surfaces and nothing enforced it, so a
// UI-only feature was invisible until somebody went looking. Requiring
// every registered route to name either its command or its gap turns that
// audit into a build failure: a route added with neither fails here, and
// a route whose gap says only "no equivalent" fails too, because a gap
// that does not say what verb would have to exist is not actionable.

// recordingBackend is an ActionRecorder that keeps what it was handed.
type recordingBackend struct {
	mu      sync.Mutex
	actions []cliecho.APIAction
}

func (r *recordingBackend) RecordAPIAction(_ context.Context, a cliecho.APIAction) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actions = append(r.actions, a)
}

func (r *recordingBackend) recorded() []cliecho.APIAction {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cliecho.APIAction(nil), r.actions...)
}

func (r *recordingBackend) last(t *testing.T) cliecho.APIAction {
	t.Helper()
	got := r.recorded()
	if len(got) == 0 {
		t.Fatal("nothing was recorded, so this action left no trace an operator could read")
	}
	return got[len(got)-1]
}

// apiRoutePatterns is every /api/v1 route a router registers, keyed the
// way core/cliecho keys its table.
func apiRoutePatterns(t *testing.T, router http.Handler) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := chi.Walk(routableFor(t, router), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			// /health/live and /health/ready are unauthenticated infra
			// probes rather than operator actions, and neither has or
			// wants a verb.
			return nil
		}
		path := strings.TrimPrefix(route, "/api/v1")
		if path != "/" {
			path = strings.TrimSuffix(path, "/")
		}
		out[method+" "+path] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	return out
}

func TestEveryAPIRouteNamesItsCLIEquivalentOrTheGap(t *testing.T) {
	configured := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	// The setup surface a fresh install serves is a different, much
	// smaller table (issue #176), and its routes are exactly the ones a
	// new operator meets first. A route that only exists there needs an
	// answer just as much.
	unconfigured := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		FirstRun:      &fakeFirstRun{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	answered := make(map[string]bool, len(cliecho.Routes()))
	for _, r := range cliecho.Routes() {
		answered[r] = true
	}

	registered := apiRoutePatterns(t, configured)
	for route := range apiRoutePatterns(t, unconfigured) {
		registered[route] = true
	}

	var checked int
	for route := range registered {
		method, path, _ := strings.Cut(route, " ")
		checked++
		if !answered[method+" "+path] {
			t.Errorf("%s /api/v1%s has no answer in core/cliecho.\nEvery route this router registers must either build a `"+cliecho.Binary+"` command or carry an explicit entry saying there is none and what verb would have to exist. That is what turns EPIC G's CLI parity rule from a promise into something that fails visibly: a UI action with no command to name is a gap that shows up the first time anybody uses the feature, instead of at an audit nobody runs.",
				method, path)
			continue
		}
		line := cliecho.Echo(cliecho.Action{Method: method, Route: path})
		if len(line.Command) == 0 && strings.TrimSpace(line.GapDetail) == "" {
			t.Errorf("%s /api/v1%s prints a gap with no reason. \"No equivalent\" on its own is not actionable; say what verb would have to exist.", method, path)
		}
	}
	if checked == 0 {
		t.Fatal("chi.Walk found no /api/v1 routes, so this test would pass vacuously")
	}

	// The other direction, so the table cannot quietly accumulate entries
	// for routes that no longer exist and go on claiming coverage.
	for _, r := range cliecho.Routes() {
		if !registered[r] {
			t.Errorf("core/cliecho answers for %q and no router registers it; an entry for a route that no longer exists is coverage that is not there", r)
		}
	}
}

// TestEveryAPIRouteNamesItsCLIEquivalentOrTheGap_WouldCatchANewRoute is
// the control. The case above is a table comparison, and a table
// comparison whose failure path nobody has seen is a table comparison that
// silently stops comparing.
func TestEveryAPIRouteNamesItsCLIEquivalentOrTheGap_WouldCatchANewRoute(t *testing.T) {
	answered := make(map[string]bool)
	for _, r := range cliecho.Routes() {
		answered[r] = true
	}
	if answered["POST /a-route-nobody-registered"] {
		t.Fatal("cliecho claims to answer for a route that does not exist")
	}
	if !answered["PATCH /backup-sets/{source}/{set}"] {
		t.Fatal("cliecho does not answer for a route that does exist, so the lookup above is not the lookup the test performs")
	}
}

// What an action leaves behind, which before this was nothing at all.
// recordingRouter is a router with a real backup set in it and a recorder
// behind it, which is what an action worth recording needs.
func recordingRouter(t *testing.T) (http.Handler, *backupSetFakeBackend, *recordingBackend) {
	t.Helper()
	recorder := &recordingBackend{}
	backend := newBackupSetFakeBackend()
	if _, err := backend.CreateBackupSet(t.Context(), service.CreateBackupSetRequest{
		SourceName: "production", Name: "postgres-primary",
		Host: "prod-db-01.internal", Port: 22, User: "backup-agent",
		SSHKeyID: "key_test_1", KnownHostsLine: "prod-db-01.internal ssh-ed25519 AAAAfaketest",
		RemotePath: "/backups/postgresql", LocalPath: "/data/backups/production/postgres",
		CompletionStrategy: "marker",
	}); err != nil {
		t.Fatalf("seeding a backup set: %v", err)
	}
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          alwaysPassGate{},
		Recorder:      recorder,
		BinaryVersion: "test",
		Commit:        "test",
	})
	return router, backend, recorder
}

func TestAnAPIActionIsRecordedWithItsActorAndItsCommand(t *testing.T) {
	router, _, recorder := recordingRouter(t)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/backup-sets/production/postgres-primary",
		strings.NewReader(`{"stale_after_seconds":172800}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	got := recorder.last(t)
	if got.Actor != "alice" {
		t.Errorf("the action was recorded as actor %q; two operators administering one deployment have to be able to tell each other apart", got.Actor)
	}
	if got.Route != "/backup-sets/{source}/{set}" || got.Method != http.MethodPatch {
		t.Errorf("the action names itself %s %s", got.Method, got.Route)
	}
	if got.BackupSetID != "production/postgres-primary" {
		t.Errorf("the action names backup set %q, so it would land on the wrong feed", got.BackupSetID)
	}
	// Bare, with no prompt: this is what reaches the journal, and a script
	// reading it wants a command it can hand to a shell. The "$ " is the
	// terminal's to draw, the same way it draws "# " in front of a gap.
	if want := cliecho.Binary + " backup-set patch production/postgres-primary --stale-after 48h"; got.Command != want {
		t.Errorf("the action echoes\n  %s\nwant\n  %s", got.Command, want)
	}
	if got.Status != http.StatusOK {
		t.Errorf("the action reports status %d", got.Status)
	}
}

// A refusal is the case this whole file exists for. An operator watching
// a button do nothing cannot tell "it refused" from "it failed" from "it
// was never wired up", and only the first of those has a reason worth
// printing.
func TestARefusalIsRecordedWithItsReason(t *testing.T) {
	recorder := &recordingBackend{}
	router := NewRouter(RouterConfig{
		Platform: allowingPlatform("alice"),
		Backend:  newSyncFakeBackend(),
		// The gate that has not been proven for this deployment, which is
		// every deployment until #92.
		Gate:          NotYetImplementedGate{},
		Recorder:      recorder,
		BinaryVersion: "test",
		Commit:        "test",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations",
		strings.NewReader(`{"action":"run_cycle","config_revision":"rev-1"}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /operations = %d, want 403", rec.Code)
	}

	got := recorder.last(t)
	if got.Status != http.StatusForbidden {
		t.Errorf("the refusal was recorded with status %d", got.Status)
	}
	if got.ErrorCode != "DESTRUCTIVE_OPERATIONS_DISABLED" {
		t.Errorf("the refusal was recorded with code %q; a refusal that does not say what refused it is a button that did nothing", got.ErrorCode)
	}
	if got.Message == "" {
		t.Error("the refusal was recorded with no message, so the terminal has nothing to print but a number")
	}
	// This refusal never reaches a handler: requireDestructiveGate turns
	// it away first. That is exactly why the recording is middleware.
	if got.Route != "/operations" {
		t.Errorf("the refusal names route %q", got.Route)
	}
	// And it is the named gap, not a misleading `backupd run`.
	if got.Command != "" {
		t.Errorf("run_cycle echoed the command %q; `backupd run` opens the service in the operator's own process and runs a cycle THERE", got.Command)
	}
	if !strings.Contains(got.GapDetail, "not in this engine") {
		t.Errorf("the gap does not say why `backupd run` is not the answer: %q", got.GapDetail)
	}
}

// A read is not an action. A dashboard polling five routes a second would
// bury the one line an operator is looking for under thousands of its own
// reads.
func TestAReadIsNotRecorded(t *testing.T) {
	recorder := &recordingBackend{}
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		Recorder:      recorder,
		BinaryVersion: "test",
		Commit:        "test",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/live", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/activity/live = %d, want 200", rec.Code)
	}
	if got := recorder.recorded(); len(got) != 0 {
		t.Errorf("a read was recorded as an action: %+v", got)
	}
}

// The handler still sees the body this middleware read on its way past,
// which is the one way a recorder could break the API it is describing.
func TestRecordingDoesNotConsumeTheRequestBody(t *testing.T) {
	router, backend, _ := recordingRouter(t)
	_ = backend

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/backup-sets/production/postgres-primary",
		strings.NewReader(`{"host":"10.0.0.99"}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := backend.lastUpdate(); got.Host == nil || *got.Host != "10.0.0.99" {
		t.Fatalf("the handler received %+v; the recorder read the body and did not put it back", got)
	}
}
