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

	// gate is the same DestructiveGate requireDestructiveGate wraps
	// POST /api/v1/operations in (below). createBackupSet
	// (handlers_backupsets.go) also consults it directly, NOT through
	// that middleware: a plain "just persist" create is deliberately
	// exempt from the gate at the route level (destructiveGateExemptRoutes,
	// router_test.go — creating a backup set never touches remote or
	// local backup data by itself), but request.run_immediately turns
	// that same call into "also start a run_cycle", the exact action
	// requireDestructiveGate exists to block — so createBackupSet checks
	// gate itself, conditionally, only on that branch, rather than the
	// route being gated unconditionally (mandatory review finding M3, PR
	// #155).
	gate DestructiveGate
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
		gate:          gate,
	}

	r := chi.NewRouter()

	r.Get("/health/live", healthLive)
	r.Get("/health/ready", h.healthReady)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(authMiddleware(cfg.Platform))

		r.Get("/system/version", h.systemVersion)
		r.Get("/system/capabilities", h.systemCapabilities)

		r.With(requireCSRF, requireDestructiveGate(gate)).Post("/operations", h.submitOperation)
		r.Get("/operations/{id}", h.getOperation)

		// Issue #146 (B2.7): the add-backup-set wizard's (#98) write path.
		// Every POST here carries requireCSRF: create-backup-set and
		// ssh-key-import are state-changing but non-destructive
		// (docs/EPIC-B-multi-nas.md §50), never wrapped in
		// requireDestructiveGate at the route level (createBackupSet
		// checks that gate itself, but only for its own run_immediately
		// branch — see that handler's own doc, handlers_backupsets.go).
		// host-key-probe and test-connection are read-only in effect
		// (§50: "probe host key", "test SSH" — neither trusts nor
		// persists anything) but each still opens a real outbound
		// TCP/SSH connection to a caller-supplied host:port, which is
		// exactly the side effect CSRF protection exists for regardless
		// of a route's destructive-gate tier (mandatory review finding
		// M5, PR #155) — without it, a cross-site `<form
		// enctype="text/plain">` POST could turn this server into a
		// network-probing primitive against an admin's own internal/NAS
		// network with no token of any kind. Both used to be listed as
		// CSRF-exempt read-only routes alongside GET /system/version and
		// GET /operations/{id} above; that was the gap.
		//
		// test-connection's own path segment ("test-connection") is
		// registered as a static route, not folded into
		// /backup-sets/*, precisely because it runs BEFORE a backup set
		// has an id at all (the wizard's pre-save check); chi matches a
		// static child before a wildcard sibling, so this never collides
		// with getBackupSet's own "/backup-sets/*" route below.
		//
		// getBackupSet uses chi's bare "*" catch-all, not a
		// "{id:.*}"-style regexp param: chi's regexp params are matched
		// per PATH SEGMENT (split on "/") even when the regexp itself
		// would otherwise span one, so "{id:.*}" only ever matches a
		// single segment and 404s on a real "source/name" id — proven
		// directly against chi/v5 v5.3.1 while building this route. "*"
		// is chi's own documented way to capture the rest of the path,
		// read back with chi.URLParam(r, "*").
		r.With(requireCSRF).Post("/backup-sets", h.createBackupSet)
		r.Get("/backup-sets", h.listBackupSets)
		r.With(requireCSRF).Post("/backup-sets/test-connection", h.testConnection)
		r.Get("/backup-sets/*", h.getBackupSet)

		r.With(requireCSRF).Post("/ssh-keys", h.importSSHKey)
		r.With(requireCSRF).Post("/ssh/host-key-probe", h.probeHostKey)
	})

	return r
}
