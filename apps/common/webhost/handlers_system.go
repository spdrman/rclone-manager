package webhost

import (
	"net/http"

	"github.com/spdrman/rclone-manager/core/service"
)

// versionResponse is GET /api/v1/system/version's response shape
// (docs/EPIC-B-multi-nas.md §15.1). Field names are chosen so nothing here
// ever spells "rclone" or "sqlite" — see
// TestSystemEndpoints_NeverLeakRcloneOrSQLiteFieldNames, and
// core/service.Version's own doc for why that is enforced at the source
// of these values, not just here.
//
// ConfigRevision (issue #118 item 5) is what a client needs before it can
// ever legitimately submit its first POST /api/v1/operations: without a
// structured place to read it, the only way to learn the current
// config_revision was to deliberately trigger a 409 and scrape it out of
// an error message's prose, which the CONFIG_REVISION_STALE response
// itself documents as free to change without notice. This read endpoint
// is that structured place; the 409 body also carries it as a top-level
// field now (see errors.go's configRevisionStaleResponse), so nothing
// downstream of the very first write has to fall back to scraping either.
type versionResponse struct {
	APIVersion     string `json:"api_version"`
	CoreVersion    string `json:"core_version"`
	Commit         string `json:"commit"`
	GoVersion      string `json:"go_version"`
	EngineVersion  string `json:"engine_version"`
	ConfigRevision string `json:"config_revision"`
}

func (h *handlers) systemVersion(w http.ResponseWriter, r *http.Request) {
	v := service.BuildVersion(h.binaryVersion, h.commit)
	// h.backend can be nil the same way healthReady already allows for
	// (a RouterConfig built without one, in principle); reporting an empty
	// ConfigRevision in that case rather than panicking matches this
	// package's existing "not fully wired yet is a degraded response, not
	// a crash" posture.
	var configRevision string
	if h.backend != nil {
		configRevision = h.backend.ConfigRevision()
	}
	writeJSON(w, http.StatusOK, versionResponse{
		APIVersion:     "v1",
		CoreVersion:    v.CoreVersion,
		Commit:         v.Commit,
		GoVersion:      v.GoVersion,
		EngineVersion:  v.EngineVersion,
		ConfigRevision: configRevision,
	})
}

// capabilitiesResponse is GET /api/v1/system/capabilities' response shape:
// a direct, field-for-field mirror of
// apps/common/platform/capabilities.PlatformCapabilities, translated to
// snake_case JSON. This is the platform's own capability declaration
// (§3.4), not a core/ concept, so it never touches core/service at all.
type capabilitiesResponse struct {
	NativeAuth          bool `json:"native_auth"`
	NativeNotifications bool `json:"native_notifications"`
	StoragePicker       bool `json:"storage_picker"`
	EmbeddedWindow      bool `json:"embedded_window"`
	AppStorePackaging   bool `json:"app_store_packaging"`
}

func (h *handlers) systemCapabilities(w http.ResponseWriter, r *http.Request) {
	c := h.platform.Capabilities()
	writeJSON(w, http.StatusOK, capabilitiesResponse{
		NativeAuth:          c.NativeAuth,
		NativeNotifications: c.NativeNotifications,
		StoragePicker:       c.StoragePicker,
		EmbeddedWindow:      c.EmbeddedWindow,
		AppStorePackaging:   c.AppStorePackaging,
	})
}

// healthLive is GET /health/live: a bare liveness probe, deliberately
// outside the /api/v1 authenticated group (§17 requires authentication on
// /api/, not on infrastructure health checks a load balancer or
// orchestrator hits with no credentials at all).
func healthLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// healthReady is GET /health/ready: ready means "a BackupServiceClient is
// wired and can report its own configuration revision", the cheapest
// check available that still proves the backend is actually up rather
// than merely that this process is listening.
func (h *handlers) healthReady(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil || h.backend.ConfigRevision() == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
