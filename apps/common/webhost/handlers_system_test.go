// The three system reads, plus one standing constraint on all of them.
//
// That constraint is the leak test: nothing these endpoints serve may
// spell rclone or sqlite. Those are implementation choices this product
// does not put on the wire, and a field name that leaks one is the kind of
// thing that arrives by copying a struct rather than by anybody deciding.
//
// The not-ready case is the other one to read. Readiness is asked of the
// backend, and this pins that no backend means not ready, because the
// previous version derived it from a value that was non-empty for every
// backend that could be constructed at all, which made it a flag that
// could not report false.
package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

func newTestRouterForSystem(t *testing.T, caps capabilities.PlatformCapabilities) http.Handler {
	t.Helper()
	return NewRouter(RouterConfig{
		Platform:      fakePlatformAdapter{caps: caps, auth: fakeAuthenticator{authenticated: true, username: "alice"}},
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "9.9.9",
		Commit:        "deadbeef",
	})
}

func doGet(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestSystemVersion_ReportsBinaryVersionAndCommit(t *testing.T) {
	router := newTestRouterForSystem(t, capabilities.PlatformCapabilities{})
	rec := doGet(t, router, "/api/v1/system/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["core_version"] != "9.9.9" {
		t.Errorf("core_version = %v, want %q", body["core_version"], "9.9.9")
	}
	if body["commit"] != "deadbeef" {
		t.Errorf("commit = %v, want %q", body["commit"], "deadbeef")
	}
	if body["go_version"] == "" || body["go_version"] == nil {
		t.Error("go_version is missing/empty")
	}
	if body["engine_version"] == "" || body["engine_version"] == nil {
		t.Error("engine_version is missing/empty")
	}
	// issue #118 item 5: config_revision must be a structured field on this
	// read endpoint, not something a client can only learn by deliberately
	// triggering a 409 and scraping an error message.
	if body["config_revision"] != "rev-1" {
		t.Errorf("config_revision = %v, want %q (from the backend's own ConfigRevision())", body["config_revision"], "rev-1")
	}
	// Issue #104 (B3.4): the startup-readiness flag must be visible on
	// this endpoint, and it is read from the backend itself
	// (BackupServiceClient.Ready), which is the only thing that knows
	// whether docs/EPIC-B-multi-nas.md §46.1's startup sequence completed.
	if body["ready"] != true {
		t.Errorf("ready = %v, want true for a router built with a working backend", body["ready"])
	}
}

// TestSystemVersion_ReportsNotReadyWhenNoBackendIsWired is this issue's
// negative control for the Ready field above: a router built with no
// backend at all (the same "not fully wired yet" case healthReady
// already handles) must report ready: false, not omit the field or
// default it to true.
func TestSystemVersion_ReportsNotReadyWhenNoBackendIsWired(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      fakePlatformAdapter{caps: capabilities.PlatformCapabilities{}, auth: fakeAuthenticator{authenticated: true, username: "alice"}},
		Backend:       nil,
		Gate:          alwaysPassGate{},
		BinaryVersion: "9.9.9",
		Commit:        "deadbeef",
	})
	rec := doGet(t, router, "/api/v1/system/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["ready"] != false {
		t.Errorf("ready = %v, want false for a router with no backend wired", body["ready"])
	}
}

func TestSystemCapabilities_ReflectsThePlatformAdapter(t *testing.T) {
	router := newTestRouterForSystem(t, capabilities.PlatformCapabilities{
		NativeAuth:        true,
		StoragePicker:     true,
		EmbeddedWindow:    false,
		AppStorePackaging: true,
	})
	rec := doGet(t, router, "/api/v1/system/capabilities")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]bool{
		"native_auth":          true,
		"native_notifications": false,
		"storage_picker":       true,
		"embedded_window":      false,
		"app_store_packaging":  true,
	}
	for field, wantVal := range want {
		got, ok := body[field]
		if !ok {
			t.Errorf("response missing field %q", field)
			continue
		}
		if got != wantVal {
			t.Errorf("%s = %v, want %v", field, got, wantVal)
		}
	}
}

// TestSystemEndpoints_NeverLeakRcloneOrSQLiteFieldNames is the RED plan's
// explicit contract test: "/api/v1/system/version and
// /api/v1/system/capabilities responses contain no rclone-native or
// SQLite-schema field names." It checks the raw response bytes, not a
// hand-picked list of fields, precisely so a future field added to either
// response is covered automatically instead of only the fields this test's
// author thought to name.
func TestSystemEndpoints_NeverLeakRcloneOrSQLiteFieldNames(t *testing.T) {
	router := newTestRouterForSystem(t, capabilities.PlatformCapabilities{NativeAuth: true})

	forbidden := []string{"rclone", "sqlite"}
	for _, path := range []string{"/api/v1/system/version", "/api/v1/system/capabilities"} {
		rec := doGet(t, router, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", path, rec.Code)
		}
		body := strings.ToLower(rec.Body.String())
		for _, word := range forbidden {
			if strings.Contains(body, word) {
				t.Errorf("GET %s response contains forbidden substring %q:\n%s", path, word, rec.Body.String())
			}
		}
	}
}

func TestHealthLive_ReportsOK(t *testing.T) {
	router := newTestRouterForSystem(t, capabilities.PlatformCapabilities{})
	rec := doGet(t, router, "/health/live")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealthReady_ReportsOKWhenBackendIsConfigured(t *testing.T) {
	router := newTestRouterForSystem(t, capabilities.PlatformCapabilities{})
	rec := doGet(t, router, "/health/ready")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
}

// TestReadiness_ReportsFalseWhenTheBackendItselfIsNotReady is the review's
// M4 fix at this layer. The previous definition of ready — the backend
// reports a non-empty config revision — could only be false when there was
// no backend at all, a shape production never produces, so the flag §36
// puts in front of a destructive operation had no reachable false state.
//
// This drives a fully wired backend that answers the question honestly,
// and asserts both surfaces that share isReady move together. The
// config-revision assertion is the control: it is non-empty throughout, so
// a ready:false here can only come from the backend's own answer.
func TestReadiness_ReportsFalseWhenTheBackendItselfIsNotReady(t *testing.T) {
	backend := newSyncFakeBackend()
	backend.notReady = true
	router := NewRouter(RouterConfig{
		Platform:      fakePlatformAdapter{caps: capabilities.PlatformCapabilities{}, auth: fakeAuthenticator{authenticated: true, username: "alice"}},
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "9.9.9",
		Commit:        "deadbeef",
	})

	rec := doGet(t, router, "/api/v1/system/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["ready"] != false {
		t.Errorf("ready = %v, want false for a wired backend that reports its own startup sequence did not complete", body["ready"])
	}
	if body["config_revision"] != "rev-1" {
		t.Errorf("config_revision = %v, want %q: the old definition of readiness was derived from this value, so it has to still be non-empty for the assertion above to mean anything", body["config_revision"], "rev-1")
	}

	ready := doGet(t, router, "/health/ready")
	if ready.Code != http.StatusServiceUnavailable {
		t.Errorf("/health/ready status = %d, want 503: the readiness probe and the version endpoint share one definition and must never disagree", ready.Code)
	}

	// The positive control: the same router with a ready backend answers
	// the other way on both surfaces.
	backend.notReady = false
	if again := doGet(t, router, "/health/ready"); again.Code != http.StatusOK {
		t.Errorf("/health/ready status = %d for a ready backend, want 200", again.Code)
	}
}

// TestHealthLive_AnswersForAnUnconfiguredUnauthenticatedInstance is the
// behavioural half of the runtime contract's start-gate rule (issue
// #167's review, M3). container/compose.yaml gates web-ui's startup on
// the engine reporting healthy, so whatever the engine's healthcheck
// asks is what stands between an operator and the only LAN-facing
// listener. This pins the two properties that make /health/live usable
// as that gate and `backup-manager status` unusable: it answers for a
// backend whose startup sequence never completed, which is what a fresh
// unconfigured install looks like, and it answers with no session, which
// is what a healthcheck subprocess has.
//
// The /health/ready assertion is the control. Both probes are on the
// same router, behind the same backend, and they disagree, so a 200 from
// /health/live cannot be a router that answers 200 to everything.
func TestHealthLive_AnswersForAnUnconfiguredUnauthenticatedInstance(t *testing.T) {
	backend := newSyncFakeBackend()
	backend.notReady = true
	router := NewRouter(RouterConfig{
		Platform:      fakePlatformAdapter{caps: capabilities.PlatformCapabilities{}, auth: fakeAuthenticator{}},
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "9.9.9",
		Commit:        "deadbeef",
	})

	if live := doGet(t, router, "/health/live"); live.Code != http.StatusOK {
		t.Errorf("/health/live status = %d, want 200: the start gate has to pass for an instance nobody has configured yet, or a fresh install never brings up its own UI", live.Code)
	}
	if ready := doGet(t, router, "/health/ready"); ready.Code != http.StatusServiceUnavailable {
		t.Errorf("/health/ready status = %d, want 503: without this the 200 above could just be a router that answers 200 to anything", ready.Code)
	}
	if api := doGet(t, router, "/api/v1/system/version"); api.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/system/version status = %d, want 401: the second half of the same control, since a healthcheck subprocess carries no session and /health/live must not need one", api.Code)
	}
}
