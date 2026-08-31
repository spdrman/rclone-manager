package webhost

import (
	"context"
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

	// FirstRun, when non-nil, is the setup surface of an instance that
	// may have no configuration yet (issue #176, firstrun.go). Combined
	// with a nil Backend it selects a completely different, much smaller
	// route table: see newUnconfiguredRouter for exactly what a fresh
	// install serves, and why that is a separate table rather than the
	// full one with guards on it.
	//
	// It is also set alongside a non-nil Backend once setup has
	// completed, so GET /api/v1/system/first-run keeps answering and a
	// POST to it keeps refusing with 409 rather than 404.
	FirstRun FirstRunClient

	// OnConfigured, when non-nil, is called by POST
	// /api/v1/system/first-run once the first configuration is durably
	// written: the host's chance to open a real backend against that file
	// and start serving the application without a restart
	// (apps/common/webhost/serve does exactly this). An error here is
	// reported to the caller as restart_required, never as a failed
	// setup — the configuration is on disk either way.
	OnConfigured func(context.Context) error

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

	// firstRun and onConfigured are RouterConfig's own fields of the same
	// names; see firstrun.go for what each is for.
	firstRun     FirstRunClient
	onConfigured func(context.Context) error
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
		firstRun:      cfg.FirstRun,
		onConfigured:  cfg.OnConfigured,
	}

	// An instance with a first-run surface and no backend has no
	// configuration yet, and serves a deliberately tiny route table
	// instead of this one (issue #176). Branching here, rather than
	// guarding each route below, is what makes a route added to this
	// function in future unreachable on a fresh install by construction
	// rather than by somebody remembering — see newUnconfiguredRouter's
	// own doc.
	if cfg.Backend == nil && cfg.FirstRun != nil {
		return newUnconfiguredRouter(h, cfg.Platform)
	}

	r := chi.NewRouter()

	r.Get("/health/live", healthLive)
	r.Get("/health/ready", h.healthReady)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(authMiddleware(cfg.Platform))

		r.Get("/system/version", h.systemVersion)
		r.Get("/system/capabilities", h.systemCapabilities)
		// Issue #176: the same two routes a fresh install serves, kept on
		// the configured router so a client that asks always gets an
		// answer, and so a setup submission arriving after setup already
		// completed is refused with a 409 that says why rather than a 404
		// that does not. See firstrun.go.
		//
		// The POST is deliberately NOT behind requireDestructiveGate: a
		// gated setup route is an instance that can never be configured
		// through its own UI, since turning the gate on is itself part of
		// the configuration it would be blocking. What keeps it safe is
		// the 409 above, not the gate. Full argument, and the test that
		// pins it, at destructiveGateExemptRoutes (router_test.go).
		r.Get("/system/first-run", h.firstRunStatus)
		r.With(requireCSRF).Post("/system/first-run", h.completeFirstRun)
		// Issue #104 (B3.4): FR-21's existing capacity refusal, surfaced
		// honestly. Read-only, same as the two routes above.
		r.Get("/system/storage", h.systemStorage)
		// Issue #211: FR-24's backup-freshness verdict, authenticated and
		// inside /api/v1. Deliberately NOT the same thing as /health/live
		// and /health/ready above, which are unauthenticated probes that
		// answer "should traffic come here" and say nothing about whether
		// backups are landing (failure-safety invariant 14). Read-only.
		r.Get("/system/health", h.systemHealth)

		r.With(requireCSRF, requireDestructiveGate(gate)).Post("/operations", h.submitOperation)
		r.Get("/operations/{id}", h.getOperation)
		// Issue #211: the list counterpart of the polling read above. GET
		// on this path used to be a 405, which is what the shared UI's
		// live-operations poll had been receiving. Read-only; the POST
		// beside it is still the gated submit route, because a verb is not
		// a synonym.
		r.Get("/operations", h.listOperations)

		// Preview is read-only (docs/EPIC-B-multi-nas.md §50 lists "preview
		// retention" under Read-only/low risk) so it carries neither
		// requireCSRF nor requireDestructiveGate, exactly like GET
		// /operations/{id} above. Apply is the one route in this whole
		// package that can delete local restore points, so it carries
		// both, exactly like POST /operations.
		//
		// The preview route is registered as two param segments plus a
		// static tail, so chi's own node ordering (static, then param,
		// then catch-all) matches it ahead of the "/backup-sets/*"
		// catch-all registered below, and a GET that does not match this
		// shape still falls through to getBackupSet as before. That
		// ordering is a property of chi's trie, not of registration
		// order, so the two routes can coexist here in either order;
		// handlers_retention_test.go drives every one of its cases
		// through this very NewRouter, which is what proves the preview
		// route is actually reached rather than swallowed by the
		// catch-all.
		r.Get("/backup-sets/{source}/{set}/retention/preview", h.previewRetention)
		r.With(requireCSRF, requireDestructiveGate(gate)).Post("/backup-sets/{source}/{set}/retention/apply", h.applyRetention)

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
		// Issue #211: enabling and disabling a persisted set. Two named
		// segments rather than the catch-all below, because this route
		// needs a literal "/enabled" tail that a catch-all would swallow;
		// a backup set id is always exactly source/name, so the arity is
		// fixed. CSRF but no destructive gate: nothing reachable from here
		// touches backup data (destructiveGateExemptRoutes, router_test.go,
		// and the handler's own doc).
		r.With(requireCSRF).Post("/backup-sets/{source}/{set}/enabled", h.setBackupSetEnabled)
		r.Get("/backup-sets/*", h.getBackupSet)

		// Issue #211: the backups this deployment actually holds, and the
		// subset being held for a human. Both read-only (§50).
		//
		// getArtifact takes three named segments rather than a catch-all:
		// an artifact id is exactly source/set/name (model.NewArtifactID
		// refuses a name containing "/"), so a route that says so lets the
		// router answer a malformed id with a 404 instead of a handler
		// having to interpret one.
		r.Get("/backups", h.listArtifacts)
		r.Get("/backups/{source}/{set}/{name}", h.getArtifact)
		r.Get("/activity", h.listActivity)
		r.Get("/quarantine", h.listQuarantine)

		// The two operator actions a quarantined backup has. Both carry
		// requireCSRF; neither carries requireDestructiveGate, and their
		// handlers' own docs record why: revalidate writes nothing at all,
		// and retry moves a journal row back into the pipeline without
		// touching a local file or a remote object.
		r.With(requireCSRF).Post("/quarantine/{source}/{set}/{name}/revalidate", h.revalidateArtifact)
		r.With(requireCSRF).Post("/quarantine/{source}/{set}/{name}/retry", h.retryArtifactIngestion)

		// Issue #211: FR-9 catalog recovery, the API expression of
		// `backup-manager catalog rebuild` and its --dry-run. Rebuild only
		// ever adds records whose recovery manifests are already on disk
		// and never removes or overwrites one, so it carries CSRF but not
		// the destructive gate; see handlers_catalog.go for the argument
		// in full.
		r.With(requireCSRF).Post("/catalog/scan", h.scanCatalog)
		r.With(requireCSRF).Post("/catalog/rebuild", h.rebuildCatalog)

		// Issue #162 (B3.2 follow-up): the registered-validator catalog
		// the wizard's step 5 picklist reads. Read-only (§50), so no
		// CSRF and no destructive gate, exactly like the GET routes
		// above; there is deliberately no write counterpart, since a
		// client-extensible catalog is the arbitrary-command surface §26
		// Step 5 forbids. Registered as a static path, so it can never
		// be shadowed by the "/backup-sets/*" catch-all above.
		r.Get("/validators", h.listValidators)

		r.With(requireCSRF).Post("/ssh-keys", h.importSSHKey)
		r.With(requireCSRF).Post("/ssh/host-key-probe", h.probeHostKey)

		// Issue #140 (B3.7): the one generic settings surface — a read
		// and a partial write covering every server-side setting the
		// shared Web UI administers, rather than a route per setting. See
		// handlers_settings.go's own doc for where "generic" stops
		// (an enumerated request type, never a config passthrough).
		//
		// The read is read-only under docs/EPIC-B-multi-nas.md §50 ("view
		// configuration"), so it carries neither requireCSRF nor
		// requireDestructiveGate, exactly like the GET routes above.
		//
		// The write carries requireCSRF and is deliberately NOT behind
		// requireDestructiveGate (destructiveGateExemptRoutes,
		// router_test.go). Editing configuration is §50's "state-changing
		// but non-destructive" bucket, the same one "create/edit backup
		// set" sits in: nothing reachable from this route touches, moves
		// or deletes a single byte of backup data. That includes the one
		// setting where the question is worth asking out loud — turning
		// FR-19's protect_last_known_good off, which internal/retention
		// calls a materially more dangerous configuration. It is
		// dangerous because it widens what a LATER retention apply may
		// delete, and that apply is POST /backup-sets/{source}/{set}/
		// retention/apply, which already re-reads the policy at plan
		// time. Putting a copy of the destructive gate here would move
		// nothing except the settings form, which would be permanently
		// inert until #92 lands.
		//
		// Issue #87 (B5.1) red-teamed that argument and kept the
		// conclusion while replacing the reason. This route used to be
		// justified by "the gate still stands between this deployment and
		// every deletion", and the gate cannot carry that: DestructiveGate
		// is a static, deployment-wide attestation (gate.go) that #92
		// flips to true once and for good, after which it stands between
		// nothing and nothing.
		//
		// What actually holds, before and after #92, is one enforced
		// rule: a retention plan is bound to the configuration revision it
		// was computed against, so any settings write in between makes the
		// plan the operator approved stale and the apply is refused by
		// name (RETENTION_PLAN_STALE). That is what makes this route
		// unable to widen an ALREADY-APPROVED deletion, and it is pinned
		// at THIS boundary by settings_gate_test.go, not only a layer
		// down. Obtaining a wider plan after the write is one more call:
		// a fresh preview, whose body carries the widened DELETE list.
		//
		// Deliberately NOT claimed here, because the API does not enforce
		// it (issue #87's review, M6): that a human re-confirms that
		// preview. ApplyRetentionRequest carries only plan_id, nothing
		// binds an apply to a preview anybody looked at, and an API caller
		// can PATCH here, GET the preview and POST the apply with no human
		// in the sequence. The re-confirmation is a property of the
		// shipped UI flow (ui/shared/src/pages/SettingsPage.tsx), which is
		// where #140's operator-facing confirmation lives, and whoever
		// tiers the next mutating route should not inherit it as though it
		// were part of the HTTP contract.
		//
		// That argument is a chain, not a claim, so it is pinned by tests
		// rather than by this comment: core/service's
		// TestApplyRetentionPlan_ASettingsWriteBetweenPreviewAndApplyIsStale
		// drives a settings write in between a preview and its apply and
		// asserts the apply is refused with ErrRetentionPlanStale and
		// nothing is deleted, with a control proving the same plan applies
		// when no settings write intervenes, and
		// TestAnUngatedSettingsWriteCannotApplyAnAlreadyApprovedPlan
		// (settings_gate_test.go) drives the identical sequence through
		// these two routes over a real BackupService. If a later change made this
		// route reuse the previous config revision, or made plan staleness
		// tolerant of a config move, that test fails rather than this
		// route quietly becoming an ungated way to widen an
		// already-approved deletion. Both of those drive a plan that
		// carries a real DELETE and assert the refused apply removed
		// nothing, so neither can pass over an empty verdict list.
		//
		// This mirrors, rather than contradicts, createBackupSet's own
		// conditional gate check: that branch is gated because
		// run_immediately literally starts a run_cycle, the exact action
		// the gate exists to block. No branch of this route starts
		// anything.
		//
		// Registered as a static path, so it can never be shadowed by the
		// "/backup-sets/*" catch-all above.
		r.Get("/settings", h.getSettings)
		r.With(requireCSRF).Patch("/settings", h.updateSettings)
	})

	return r
}
