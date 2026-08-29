package webhost

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

// RouterConfig is everything NewRouter needs to build the /api/v1 surface.
type RouterConfig struct {
	// Platform supplies both the Authenticator every /api/v1 request is
	// checked against (auth.go) and the capabilities GET
	// /api/v1/system/capabilities reports.
	Platform capabilities.PlatformAdapter

	// Backend is the core/service.BackupService adapter (or a test
	// double satisfying BackupServiceClient) every handler calls into.
	Backend BackupServiceClient

	// Gate decides whether POST /api/v1/operations may actually run. If
	// nil, NewRouter uses NotYetImplementedGate — a caller has to name a
	// different gate on purpose to change this, never gets one by
	// omission.
	Gate DestructiveGate

	BinaryVersion string
	Commit        string
}

// handlers bundles what the HTTP methods in handlers_system.go and
// handlers_operations.go need, built once by NewRouter.
type handlers struct {
	platform      capabilities.PlatformAdapter
	backend       BackupServiceClient
	binaryVersion string
	commit        string
}

// NewRouter builds the /api/v1 HTTP surface plus /health/live and
// /health/ready. See this package's doc comment for the layering; see
// auth.go and gate.go for exactly what "authenticated" and "the
// destructive gate has passed" mean.
//
// Every /api/v1 route is registered inside one chi.Router.Route group
// with authMiddleware applied through r.Use, so nothing added to that
// group in the future can accidentally skip it — there is no second way
// to register a route under /api/v1 in this function that bypasses that
// r.Use call. /health/live and /health/ready are registered outside that
// group, deliberately: see healthLive's doc for why.
//
// The return type is http.Handler, not *chi.Mux, deliberately: the "no
// route bypasses auth" proof above (TestNoAPIRouteBypassesAuthentication)
// is airtight for what this function itself registers, but is not a
// structural guarantee about what a caller could do with the concrete
// value if this returned one — nothing would stop a future caller (a
// provider's own main.go, say) from registering more routes directly on a
// returned *chi.Mux, entirely outside this package's test reach. No
// current caller needs any *chi.Mux-specific method, so there is nothing
// to lose by only ever handing back the interface.
func NewRouter(cfg RouterConfig) http.Handler {
	gate := cfg.Gate
	if gate == nil {
		gate = NotYetImplementedGate{}
	}

	h := &handlers{
		platform:      cfg.Platform,
		backend:       cfg.Backend,
		binaryVersion: cfg.BinaryVersion,
		commit:        cfg.Commit,
	}

	r := chi.NewRouter()

	r.Get("/health/live", healthLive)
	r.Get("/health/ready", h.healthReady)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(authMiddleware(cfg.Platform))

		r.Get("/system/version", h.systemVersion)
		r.Get("/system/capabilities", h.systemCapabilities)

		r.With(requireDestructiveGate(gate)).Post("/operations", h.submitOperation)
		r.Get("/operations/{id}", h.getOperation)
	})

	return r
}
