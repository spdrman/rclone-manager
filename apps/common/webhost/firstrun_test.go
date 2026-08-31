package webhost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// errActivationForTest stands in for whatever goes wrong when a
// first-run install writes its configuration successfully but cannot then
// open a service against it in-process (a state volume that turns out to
// be read-only, say). The configuration is durably on disk either way,
// which is the whole point of the branch it drives.
var errActivationForTest = errors.New("activation failed for test")

// fakeFirstRun is a FirstRunClient double. It records what it was asked
// to do, so a test can prove a route reached the first-run surface rather
// than merely returning a plausible status.
type fakeFirstRun struct {
	configured  bool
	created     *service.CreateBackupSetRequest
	createErr   error
	imported    []byte
	importErr   error
	probed      string
	activated   int
	activateErr error
}

func (f *fakeFirstRun) Configured() bool { return f.configured }

func (f *fakeFirstRun) ImportSSHKey(_ context.Context, raw []byte) (service.SSHKeyRef, error) {
	f.imported = raw
	if f.importErr != nil {
		return service.SSHKeyRef{}, f.importErr
	}
	return service.SSHKeyRef{ID: "key_1", Algorithm: "ssh-ed25519", Fingerprint: "SHA256:test"}, nil
}

func (f *fakeFirstRun) ProbeHostKey(_ context.Context, host string, port int) (service.HostKeyProbe, error) {
	f.probed = host
	return service.HostKeyProbe{Algorithm: "ssh-ed25519", Fingerprint: "SHA256:test", KnownHostsLine: host + " ssh-ed25519 AAAAtest"}, nil
}

func (f *fakeFirstRun) TestConnection(_ context.Context, _ service.ConnectionTestRequest) (service.ConnectionTestResult, error) {
	return service.ConnectionTestResult{OK: true}, nil
}

func (f *fakeFirstRun) CreateInitialConfig(_ context.Context, req service.CreateBackupSetRequest) (service.BackupSet, error) {
	if f.createErr != nil {
		return service.BackupSet{}, f.createErr
	}
	f.created = &req
	f.configured = true
	return service.BackupSet{ID: "api/" + req.Name, SourceName: "api", Name: req.Name, Host: req.Host}, nil
}

func (f *fakeFirstRun) activate(context.Context) error {
	f.activated++
	return f.activateErr
}

// unconfiguredRouter builds the router a fresh install actually serves:
// authenticated, with a first-run client and no backend at all.
func unconfiguredRouter(fr *fakeFirstRun) http.Handler {
	return NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		FirstRun:      fr,
		OnConfigured:  fr.activate,
		BinaryVersion: "test",
		Commit:        "test",
	})
}

// firstRunSafeRoutes names every /api/v1 route an instance with no
// configuration is allowed to answer, and why. Everything else must
// refuse. A future route reaching an unconfigured instance without an
// entry here is a regression, not an omission: see
// TestUnconfiguredInstanceRefusesEveryRouteOutsideTheSetupSurface below,
// which walks the CONFIGURED router's own table rather than this list, so
// this map can never make the test vacuous by shrinking.
var firstRunSafeRoutes = map[string]bool{
	// Read-only, and the only way a client learns it is looking at an
	// unconfigured instance at all.
	"GET /api/v1/system/version":      true,
	"GET /api/v1/system/capabilities": true,
	"GET /api/v1/system/first-run":    true,

	// The setup flow itself. Each is either read-only in effect
	// (host-key-probe, test-connection) or the setup write it exists for
	// (ssh-keys, first-run), and none of them touches, moves or deletes a
	// byte of backup data, because there is none yet.
	"POST /api/v1/ssh-keys":                    true,
	"POST /api/v1/ssh/host-key-probe":          true,
	"POST /api/v1/backup-sets/test-connection": true,
	"POST /api/v1/system/first-run":            true,

	// The wizard's step 5 picklist. Read-only, and its contents are
	// code-defined rather than deployment-defined, so it says nothing
	// about a configuration that does not exist.
	"GET /api/v1/validators": true,
}

// TestUnconfiguredInstanceRefusesEveryRouteOutsideTheSetupSurface is
// issue #176's "what is reachable before configuration exists" decision,
// enforced structurally rather than by a hand-kept list. It walks the
// route table of the CONFIGURED router (every route this package will
// ever serve) and fires an authenticated request at each one against an
// UNCONFIGURED instance. Anything not on the setup allowlist has to
// refuse with 503 NOT_CONFIGURED.
func TestUnconfiguredInstanceRefusesEveryRouteOutsideTheSetupSurface(t *testing.T) {
	configured := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	router := unconfiguredRouter(&fakeFirstRun{})

	var refused, allowed int
	err := chi.Walk(routableFor(t, configured), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/health/") {
			return nil
		}
		if firstRunSafeRoutes[method+" "+route] {
			allowed++
			return nil
		}
		refused++

		path := strings.ReplaceAll(route, "{id}", "op_1")
		path = strings.ReplaceAll(path, "{source}", "api")
		path = strings.ReplaceAll(path, "{set}", "nightly")
		path = strings.ReplaceAll(path, "/*", "/api/nightly")
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		attachValidCSRF(req)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want %d on an unconfigured instance", method, route, rec.Code, http.StatusServiceUnavailable)
			return nil
		}
		// Assert WHY it refused, not merely that it did: a 503 from some
		// unrelated failure would otherwise read as a pass.
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s: body is not an API error: %v (%s)", method, route, err, rec.Body.String())
			return nil
		}
		if body.Error.Code != "NOT_CONFIGURED" {
			t.Errorf("%s %s: error code = %q, want %q", method, route, body.Error.Code, "NOT_CONFIGURED")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if refused == 0 {
		t.Fatal("walked no refusable routes; this test would pass vacuously")
	}
	if allowed == 0 {
		t.Fatal("matched no first-run-safe routes; the allowlist no longer names anything the router registers")
	}
}

// TestUnconfiguredInstanceStillRequiresAuthentication is the ordering
// decision issue #176 asks to make explicit: an instance reachable on the
// network with no configuration must not be configurable by whoever
// reaches the port first. Enrollment comes before setup, and the setup
// surface itself sits inside the authenticated group.
func TestUnconfiguredInstanceStillRequiresAuthentication(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      noAuthWiredAdapter{},
		FirstRun:      &fakeFirstRun{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	var checked int
	registered := map[string]bool{}
	err := chi.Walk(routableFor(t, router), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/health/") {
			return nil
		}
		checked++
		registered[method+" "+route] = true
		req := httptest.NewRequest(method, strings.ReplaceAll(route, "/*", "/x"), strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		attachValidCSRF(req)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want %d (an unconfigured instance must not be configurable by an unauthenticated caller)", method, route, rec.Code, http.StatusUnauthorized)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if checked == 0 {
		t.Fatal("chi.Walk found no /api/v1 routes; this test would pass vacuously")
	}

	// The small table IS issue #176's answer to "what is reachable before
	// configuration exists", and until now it was enforced by reading it.
	// TestUnconfiguredInstanceRefusesEveryRouteOutsideTheSetupSurface
	// walks the CONFIGURED router, so a route registered ONLY here never
	// appears in that walk and is never compared against anything. That
	// is exactly what a well-meaning future first-run feature looks like.
	// This walk already has the small router in front of it, so pinning
	// the set costs the two loops below.
	for route := range registered {
		if !firstRunSafeRoutes[route] {
			t.Errorf("%s is served by an UNCONFIGURED instance but is not in firstRunSafeRoutes; every route reachable before setup has to be listed and justified there", route)
		}
	}
	for route := range firstRunSafeRoutes {
		if !registered[route] {
			t.Errorf("firstRunSafeRoutes names %s, which the unconfigured router does not register; the allowlist is describing a surface that no longer exists", route)
		}
	}
	// Positive control on the collection itself: a route from the full
	// application must NOT be in the set, or the walk picked up the
	// configured router and the equality above is meaningless.
	if registered["GET /api/v1/backup-sets"] {
		t.Fatal("the walk collected the CONFIGURED router's routes; this test is not looking at the setup surface at all")
	}
}

// TestUnconfiguredInstanceIsNotReady reuses #157's readiness flag rather
// than inventing a second "is this usable" mechanism: an instance with no
// configuration has not run §46.1's startup sequence, so it reports not
// ready on both the probe an orchestrator hits and the authenticated
// version endpoint a client reads.
func TestUnconfiguredInstanceIsNotReady(t *testing.T) {
	router := unconfiguredRouter(&fakeFirstRun{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/health/ready status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/v1/system/version status = %d, want 200 (a client has to be able to ask what it is talking to)", rec.Code)
	}
	var version struct {
		Ready      bool `json:"ready"`
		Configured bool `json:"configured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &version); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if version.Ready {
		t.Error("system/version reported ready = true on an unconfigured instance")
	}
	if version.Configured {
		t.Error("system/version reported configured = true on an unconfigured instance")
	}

	// Positive control: the same two answers flip on a configured
	// instance, so neither assertion above is passing because the field
	// is simply never true.
	configured := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		BinaryVersion: "test",
		Commit:        "test",
	})
	rec = httptest.NewRecorder()
	configured.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/version", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &version); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !version.Ready || !version.Configured {
		t.Errorf("configured instance reported ready = %v, configured = %v; want both true", version.Ready, version.Configured)
	}
}

// TestFirstRunStatus_TellsAClientWhichModeItIsIn is what the shared UI
// reads to decide whether to render the setup flow or the application.
func TestFirstRunStatus_TellsAClientWhichModeItIsIn(t *testing.T) {
	router := unconfiguredRouter(&fakeFirstRun{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/first-run", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Configured bool `json:"configured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if body.Configured {
		t.Error("configured = true on an unconfigured instance")
	}

	// Positive control: a configured instance answers the same route with
	// configured = true, so the field is not simply hardcoded.
	configured := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		BinaryVersion: "test",
		Commit:        "test",
	})
	rec = httptest.NewRecorder()
	configured.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/first-run", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("configured instance status = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !body.Configured {
		t.Error("configured = false on a configured instance")
	}
}

func postFirstRun(t *testing.T, router http.Handler, body string, withCSRF bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/first-run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if withCSRF {
		attachValidCSRF(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestCompleteFirstRun_WritesTheConfigAndActivatesTheBackend is the
// end of the operator's journey through this package: one authenticated,
// CSRF-protected POST turns an unconfigured instance into a configured
// one, in the same process, with no restart.
func TestCompleteFirstRun_WritesTheConfigAndActivatesTheBackend(t *testing.T) {
	fr := &fakeFirstRun{}
	router := unconfiguredRouter(fr)

	rec := postFirstRun(t, router, validCreateBody, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if fr.created == nil {
		t.Fatal("CreateInitialConfig was never called")
	}
	if fr.created.Name != "postgres-primary" {
		t.Errorf("Name = %q, want %q", fr.created.Name, "postgres-primary")
	}
	if fr.activated != 1 {
		t.Errorf("activated %d times, want exactly 1", fr.activated)
	}

	var body struct {
		BackupSet struct {
			ID string `json:"id"`
		} `json:"backup_set"`
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if body.BackupSet.ID != "api/postgres-primary" {
		t.Errorf("backup_set.id = %q, want %q", body.BackupSet.ID, "api/postgres-primary")
	}
	if body.RestartRequired {
		t.Error("restart_required = true after a successful activation")
	}
}

// TestCompleteFirstRun_ReportsARestartWhenActivationFails is the honest
// half of the same call: the configuration is durably written either way,
// so a failure to activate in-process must not read as a failed setup.
func TestCompleteFirstRun_ReportsARestartWhenActivationFails(t *testing.T) {
	fr := &fakeFirstRun{activateErr: errActivationForTest}
	router := unconfiguredRouter(fr)

	rec := postFirstRun(t, router, validCreateBody, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !body.RestartRequired {
		t.Error("restart_required = false even though activation failed")
	}
}

// TestCompleteFirstRun_RequiresCSRF keeps the one write a fresh install
// exposes to the network from being forgeable cross-site, exactly like
// every other write in this package.
func TestCompleteFirstRun_RequiresCSRF(t *testing.T) {
	fr := &fakeFirstRun{}
	router := unconfiguredRouter(fr)

	rec := postFirstRun(t, router, validCreateBody, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 without a CSRF token", rec.Code)
	}
	if fr.created != nil {
		t.Error("CreateInitialConfig ran for a request with no CSRF token")
	}
}

// TestCompleteFirstRun_RefusesOnceConfigured closes the setup route the
// moment a configuration exists, so it can never be used to replace a
// live deployment's configuration.
func TestCompleteFirstRun_RefusesOnceConfigured(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		FirstRun:      &fakeFirstRun{configured: true},
		BinaryVersion: "test",
		Commit:        "test",
	})

	rec := postFirstRun(t, router, validCreateBody, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 once configured (%s)", rec.Code, rec.Body.String())
	}
	// The typed code as well as the status, because this refusal is what
	// justifies the route's entry in destructiveGateExemptRoutes
	// (router_test.go): the gate is not what stops a setup submission
	// from touching a live configuration, THIS is, and an exemption
	// resting on an untested claim is how a gate walk gets defeated
	// rather than satisfied.
	if got := responseErrorCode(rec.Body.String()); got != "ALREADY_CONFIGURED" {
		t.Fatalf("error code = %q, want ALREADY_CONFIGURED (%s)", got, rec.Body.String())
	}
}

// TestCompleteFirstRun_MapsAnInvalidRequestTo400 proves the handler
// reports why a setup submission was refused rather than collapsing every
// core/service error into one status.
func TestCompleteFirstRun_MapsAnInvalidRequestTo400(t *testing.T) {
	fr := &fakeFirstRun{createErr: service.ErrInvalidRequest}
	router := unconfiguredRouter(fr)

	rec := postFirstRun(t, router, validCreateBody, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if fr.activated != 0 {
		t.Error("activation ran even though the configuration was never written")
	}
}

// TestSetupRoutesReachTheFirstRunClientWhileUnconfigured proves the three
// shared setup routes are wired to the first-run surface, not left
// pointing at a backend that does not exist.
func TestSetupRoutesReachTheFirstRunClientWhileUnconfigured(t *testing.T) {
	fr := &fakeFirstRun{}
	router := unconfiguredRouter(fr)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/ssh-keys", strings.NewReader(`{"private_key_pem":"-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n"}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("POST /api/v1/ssh-keys status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if len(fr.imported) == 0 {
		t.Error("the first-run client never saw the imported key")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/ssh/host-key-probe", strings.NewReader(`{"host":"prod-db-01.internal","port":22}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("POST /api/v1/ssh/host-key-probe status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if fr.probed != "prod-db-01.internal" {
		t.Errorf("the first-run client probed %q, want %q", fr.probed, "prod-db-01.internal")
	}
}

// TestConfigured_PrefersALiveBackendOverTheFileOnDisk pins which of two
// authorities owns one fact. Both survive activation: the configured
// router keeps FirstRun set so GET /system/first-run keeps answering, and
// FirstRun answers by statting a file. Removing config.yaml to reset an
// instance is a plausible thing to do on a NAS, and when the file was
// asked first it flipped a fully running, fully configured process back
// into setup: every operator's browser sent to the wizard, and
// completeFirstRun's guard opened, which would write a SECOND "first"
// configuration and report restart_required false while the process kept
// serving the configuration that was no longer on disk.
func TestConfigured_PrefersALiveBackendOverTheFileOnDisk(t *testing.T) {
	// configured: false is the state a deleted config file produces.
	fr := &fakeFirstRun{configured: false}
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		FirstRun:      fr,
		BinaryVersion: "test",
		Commit:        "test",
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/first-run", nil))
	var body struct {
		Configured bool `json:"configured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !body.Configured {
		t.Error("configured = false on a running instance whose config file was deleted; the UI would send every operator to the setup wizard")
	}

	if rec := postFirstRun(t, router, validCreateBody, true); rec.Code != http.StatusConflict {
		t.Fatalf("POST status = %d, want 409 on a running instance (%s)", rec.Code, rec.Body.String())
	}
	if fr.created != nil {
		t.Error("a second first configuration was written on a live, already-configured instance")
	}

	// Positive control: the identical first-run client with no backend
	// behind it answers false and admits the write. Without this, both
	// assertions above would also pass against a handler that simply
	// hardcoded true.
	unconfigured := unconfiguredRouter(&fakeFirstRun{configured: false})
	rec = httptest.NewRecorder()
	unconfigured.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/first-run", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if body.Configured {
		t.Error("configured = true on an instance with no backend and no config file")
	}
	if rec := postFirstRun(t, unconfigured, validCreateBody, true); rec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d on a genuinely unconfigured instance, want 201 (%s)", rec.Code, rec.Body.String())
	}
}

// TestCompleteFirstRun_ActivationOutlivesTheRequest keeps a
// process-lifetime resource from being built under a context Go cancels
// the moment the handler returns, and that a browser tab or a reverse
// proxy timeout can cancel long before that. What activation opens is a
// journal handle and the shared journal lock, and a cancellation landing
// inside state.Open surfaces as an error that is neither
// ErrUnknownSchemaVersion nor ErrSchemaDrift, so the migration failure
// path falls through to the snapshot restore, which rename-overwrites the
// journal's files. Whether an operator closed a tab must not decide
// whether that runs.
func TestCompleteFirstRun_ActivationOutlivesTheRequest(t *testing.T) {
	fr := &fakeFirstRun{}

	// Declared before the router so the activation closure can reach the
	// request it is called from, and assigned after: observing "the
	// request was cancelled and activation carried on" needs both sides
	// of one handler call.
	var requestContext context.Context
	var cancelRequest context.CancelFunc
	var activationErr, requestErr error
	var hasDeadline bool

	router := NewRouter(RouterConfig{
		Platform: allowingPlatform("alice"),
		FirstRun: fr,
		OnConfigured: func(ctx context.Context) error {
			// Cancel the request from inside activation, which is what a
			// closed tab or a proxy timeout does.
			cancelRequest()
			_, hasDeadline = ctx.Deadline()
			activationErr = ctx.Err()
			requestErr = requestContext.Err()
			return nil
		},
		BinaryVersion: "test",
		Commit:        "test",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/first-run", strings.NewReader(validCreateBody))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	requestContext, cancelRequest = ctx, cancel
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	// The positive control for the negative assertion below: unless the
	// request context really was cancelled at that instant, "activation
	// was not cancelled" proves nothing at all.
	if requestErr == nil {
		t.Fatal("the request context was not cancelled, so this test cannot tell a detached activation from an attached one")
	}
	if activationErr != nil {
		t.Errorf("activation ran on a cancelled context (%v); a closed browser tab would reach the journal's restore path", activationErr)
	}
	if !hasDeadline {
		t.Error("the detached activation context has no deadline; a hung open would hold the request for the life of the process")
	}
}
