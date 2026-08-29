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
