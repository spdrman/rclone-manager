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

	// Ready is issue #104 (B3.4)'s startup-readiness flag
	// (docs/EPIC-B-multi-nas.md §46.1/§36): true once this process has a
	// backend wired that has actually completed its startup sequence
	// (state-dir validation, the startup lock, the pending-migration
	// check, any migration, and the shared journal lock it holds
	// afterwards — see core/service's startup.go).
	//
	// It is read from the backend itself (BackupServiceClient.Ready),
	// which is the only thing that knows. It used to be re-derived here
	// as "the backend reports a non-empty config revision", which was
	// true of every BackupService that could be constructed, including
	// one built without running the startup sequence at all: a flag on
	// §36's destructive-operation precondition that could not be false.
	//
	// Today a process whose startup sequence failed exits instead of
	// serving, so a client will normally only ever observe true here and
	// a connection error otherwise; the reason it failed goes to the
	// process's own log (core/service.Open). Serving a degraded process
	// that answers "not ready, and here is why" is a bigger change than
	// this endpoint, and is deliberately not what this field claims.
	Ready bool `json:"ready"`

	// Configured is issue #176's fresh-install flag: false means this
	// process is listening with no configuration on disk at all and is
	// serving the setup flow, not the application. It is a different
	// question from Ready above, and both answers matter separately: an
	// unconfigured instance is never ready, but a not-ready instance is
	// not necessarily unconfigured.
	//
	// A client that only wants to know which screen to render can read
	// GET /api/v1/system/first-run instead, which asks exactly this and
	// nothing else. It is repeated here so a client that already fetches
	// version does not need a second round trip.
	Configured bool `json:"configured"`
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
		Ready:          isReady(h.backend),
		Configured:     h.configured(),
	})
}

// capabilitiesResponse is GET /api/v1/system/capabilities' response shape:
// a direct, field-for-field mirror of
// apps/common/platform/capabilities.PlatformCapabilities, translated to
// snake_case JSON, plus the platform identifier those capabilities belong
// to. This is the platform's own capability declaration (§3.4), not a
// core/ concept, so it never touches core/service at all.
//
// Platform is issue #166 (B6.2)'s one addition to this shape, and it is
// additive rather than a reshape: the contract (api/v1/openapi.json)
// documents platform differences as CAPABILITY DATA, and a reading with
// no platform in it cannot be attributed to a profile at all - an
// operator looking at a support bundle, or an adapter conformance run
// comparing two profiles, has to be able to say WHOSE capabilities these
// are. It is emphatically not an invitation to branch on: a client that
// switches on this value instead of on the booleans beside it is the
// exact pattern #81's standing constraint forbids, and
// ui/shared's contract.conformance.test.ts fails on it.
type capabilitiesResponse struct {
	Platform            string `json:"platform"`
	NativeAuth          bool   `json:"native_auth"`
	NativeNotifications bool   `json:"native_notifications"`
	StoragePicker       bool   `json:"storage_picker"`
	EmbeddedWindow      bool   `json:"embedded_window"`
	AppStorePackaging   bool   `json:"app_store_packaging"`
}

func (h *handlers) systemCapabilities(w http.ResponseWriter, r *http.Request) {
	c := h.platform.Capabilities()
	writeJSON(w, http.StatusOK, capabilitiesResponse{
		Platform:            string(h.platform.ID()),
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
// wired, and that backend completed §46.1's startup sequence", which is
// what an orchestrator wanting to know whether to send traffic here is
// actually asking. It carries no reason for a false — this route is
// unauthenticated, and why a process is not ready is operational detail
// that belongs in the process's log and on the authenticated
// /system/version, not on a probe anyone can hit.
func (h *handlers) healthReady(w http.ResponseWriter, r *http.Request) {
	if !isReady(h.backend) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// isReady is the one definition of "ready" both healthReady and
// systemVersion's Ready field use, so the two can never quietly disagree
// about what readiness means: a backend is wired, and that backend says
// its own startup sequence completed.
func isReady(backend BackupServiceClient) bool {
	return backend != nil && backend.Ready()
}
