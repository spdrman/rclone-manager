package webhost

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/backupdproject/backupd/apps/common/platform/capabilities"
)

// The route table, which is where this package's security tiering
// actually lives.
//
// Every route is one of three tiers, and the tier is decided by what the
// route touches rather than by how the button reads. Read-only routes
// carry neither CSRF nor the destructive gate. State-changing but
// non-destructive routes carry CSRF: they write configuration, move a
// journal row, or open an outbound connection, none of which touches
// backup data. The two routes that can delete backup data carry both.
// docs/EPIC-B-multi-nas.md §50 is the source of those buckets, and the
// long comments below are the per-route argument for which one applies,
// several of which were argued out in review and would otherwise be
// re-litigated by the next person adding a route.
//
// Two structural decisions make the tiering hold rather than merely
// describe it. Authentication is applied once through r.Use on the whole
// /api/v1 group, so there is no way to add a route inside this function
// that skips it. And the return type is http.Handler rather than *chi.Mux,
// so a caller cannot register further routes onto the value this returns,
// outside the reach of the test that proves no route bypasses auth.
//
// The unconfigured branch near the top is the same idea again: a fresh
// install gets a different, much smaller table rather than this one with
// guards sprinkled through it, so a route added below is unreachable
// before setup by construction instead of by somebody remembering.

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

	// Recorder, when non-nil, is where every action taken through this
	// API is recorded so an operator can read it (issue #599,
	// actionlog.go). Nil means nothing is recorded, which is what a
	// handler test wants and what a host that has not wired one gets:
	// the API still serves, it is just invisible.
	//
	// It is its own field rather than a method on Backend because it is
	// not part of the read/write seam this package talks to core/
	// through. A recorder is a place lines go, and a host is free to
	// wire the same BackupService into both or neither.
	Recorder ActionRecorder
	// Logger is where a refusal this package cannot explain to the
	// client goes instead (#598). Nil means the default below, which
	// writes to this process's own stdout: a host has to opt OUT of
	// logging its 500s, never get silence by omission, because the
	// correlation id every one of those responses carries is worth
	// nothing if it matches no line anywhere.
	//
	// It is an interface, and its one method is exactly
	// core/internal/obs.Logger's Event (obs.Level is an alias for
	// slog.Level, so that type satisfies this as it stands). A host
	// inside core/ can therefore hand its own obs.Logger straight in and
	// get redaction and the FR-23 event catalog for free; this module
	// cannot import that package itself, because obs is internal to the
	// core module and this is a different module.
	Logger Logger

	BinaryVersion string
	Commit        string
}

// Logger is the narrow seam this package writes to. See
// RouterConfig.Logger for why it is an interface and what satisfies it.
type Logger interface {
	Event(ctx context.Context, level slog.Level, event, msg string, attrs ...slog.Attr)
}

// stdoutLogger is what a host that named no Logger gets: newline-delimited
// JSON on stdout, one object per event, with the event name in its own
// field. The same shape core/internal/obs writes, so a deployment running
// the engine and this host in one image produces one parseable stream
// rather than two formats.
type stdoutLogger struct{ base *slog.Logger }

func (l stdoutLogger) Event(ctx context.Context, level slog.Level, event, msg string, attrs ...slog.Attr) {
	l.base.LogAttrs(ctx, level, msg, append([]slog.Attr{slog.String("event", event)}, attrs...)...)
}

// NewStdoutLogger is the package default Logger, exported so a sibling
// surface that is not built by NewRouter (apps/common/webhost/serve's
// UI host, which has its own config struct) writes through the same
// seam and the same shape rather than inventing a second one.
func NewStdoutLogger() Logger { return newStdoutLogger() }

func newStdoutLogger() Logger {
	return stdoutLogger{base: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: envLogLevel()}))}
}

// DebugEnabled reports whether this process was started with diagnostics
// on. It exists so a sibling package building its own surface
// (apps/common/webhost/serve) reads the same environment in the same
// way rather than growing a second answer to "are we in debug".
func DebugEnabled() bool { return envLogLevel() == slog.LevelDebug }

// envLogLevel is the one place this process decides how loud it is, and
// the default is unchanged: INFO, exactly what this handler emitted
// before there was anything to configure. An operator diagnosing a
// report we cannot reproduce (issue #730: a browser that gets no HTTP
// response at all while curl gets a clean 401) sets LOG_LEVEL=debug, or
// BACKUPD_DEBUG=1 as the shortcut, and gets the debug events this
// package and serve/ui.go emit; nobody who sets neither sees one extra
// line.
//
// BACKUPD_DEBUG wins over LOG_LEVEL because it is the shortcut an
// operator is told to set over a phone call, and an unparseable
// LOG_LEVEL falls back to INFO rather than refusing to start: a typo in
// a diagnostic knob must never take a backup host down.
//
// RM_DEBUG is the same shortcut under this project's old name
// (rclone-manager, issue #794) and is DEPRECATED: it is still honoured
// so an upgrade does not silently turn a diagnosing operator's logs back
// off, and it will be dropped a release after BACKUPD_DEBUG. The two are
// OR'd rather than ranked because neither has ever had an "off" value -
// only the documented 1 means anything.
//
// core/internal/obs.LevelFromEnv is the other reader of these same
// variables, with the same precedence and the same fallback, and it is
// what the ENGINE builds its sink from. Two readers rather than one
// shared helper because apps/ may import core/ and never the reverse,
// and core/internal is unreachable from here by construction. They have
// to agree: a deployment where the two containers answered "how loud am
// I" differently is the half of #730 where an operator got the proxy
// trace and nothing from the process it describes. That includes the
// deprecated alias: an operator who upgrades one container before the
// other must not end up with one of them silently quiet.
func envLogLevel() slog.Level {
	if debugShortcutEnv() {
		return slog.LevelDebug
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// debugShortcutEnv reports whether the one-variable debug shortcut is
// set, under its own name or under the deprecated RM_DEBUG alias. Only
// the documented "1" counts, under either name: a knob whose typos mean
// something is a knob that surprises the operator reading it back. The
// engine's core/internal/obs.debugShortcut is the same two lines, for
// the import-direction reason envLogLevel's own doc gives.
func debugShortcutEnv() bool {
	return os.Getenv("BACKUPD_DEBUG") == "1" || os.Getenv("RM_DEBUG") == "1"
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

	// logger is RouterConfig.Logger, resolved: never nil after NewRouter,
	// so internalError (refusal.go) has nothing to branch on.
	logger Logger

	// debug is envLogLevel() == slog.LevelDebug, resolved once here
	// rather than per request. It gates the diagnostic events a handler
	// emits in addition to its normal work (issue #730), so a default
	// INFO deployment does not even pay for building their attributes.
	debug bool
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

	logger := cfg.Logger
	if logger == nil {
		logger = newStdoutLogger()
	}

	h := &handlers{
		platform:      cfg.Platform,
		backend:       cfg.Backend,
		binaryVersion: cfg.BinaryVersion,
		commit:        cfg.Commit,
		gate:          gate,
		firstRun:      cfg.FirstRun,
		onConfigured:  cfg.OnConfigured,
		logger:        logger,
		debug:         envLogLevel() == slog.LevelDebug,
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

	// Outermost, and over the health probes as well as /api/v1: the id
	// this mints is on every response this router produces, and the
	// clock it starts is the only place a later hop can read how long
	// the request has been in this process (requestscope.go). Registered
	// before any route below, which is chi's own requirement for a
	// root-level Use.
	r.Use(RequestScope)

	r.Get("/health/live", healthLive)
	r.Get("/health/ready", h.healthReady)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(authMiddleware(cfg.Platform))
		// After authentication, so a recorded line can name the actor,
		// and over the WHOLE group, so it covers the refusals that never
		// reach a handler at all: a CSRF failure and a
		// destructive-operations denial are exactly the refusals an
		// operator most needs to see, and neither one gets as far as a
		// handler body. See actionlog.go for why this is middleware
		// rather than a line in every handler.
		r.Use(recordActions(cfg.Recorder))

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
		// Issue #316: the read-only CRUD-parity counterpart to /enabled
		// immediately above, registered the same way and for the same
		// reason (a fixed source/set arity, and a literal "/read-only"
		// tail a catch-all would swallow).
		r.With(requireCSRF).Post("/backup-sets/{source}/{set}/read-only", h.setBackupSetReadOnly)
		// Issue #333: one backup set's own retention policy, as a
		// sub-resource with three methods rather than as keys on the set
		// itself. PUT because an override replaces the deployment's whole
		// chain and is never merged with it, and DELETE because "go back
		// to inheriting" cannot be spelled as a value on a request where
		// an absent field already means "leave this alone" (see
		// handlers_backupsetretention.go's own doc for both).
		//
		// Registered with the same two named segments and literal tail as
		// /enabled and /read-only above, and ahead of the "/backup-sets/*"
		// catch-all in the same way the retention/preview route already
		// is: chi's trie matches static, then param, then catch-all, so a
		// GET on this exact shape reaches this handler and every other GET
		// still falls through to getBackupSet.
		//
		// CSRF on the two writes, no destructive gate on any of them: this
		// writes configuration and moves no backup data. It does change
		// what a later retention apply would delete, in both directions,
		// which is precisely the case the comment on PATCH /settings below
		// already settles for turning FR-19's protection off — the apply
		// is the gated act and it re-reads the policy at plan time.
		r.Get("/backup-sets/{source}/{set}/retention", h.getBackupSetRetention)
		r.With(requireCSRF).Put("/backup-sets/{source}/{set}/retention", h.setBackupSetRetention)
		r.With(requireCSRF).Delete("/backup-sets/{source}/{set}/retention", h.clearBackupSetRetention)
		// Issue #350: the edit half of backup-set CRUD. Registered as two
		// named segments with no tail, which is why it needs a method chi
		// can tell apart from getBackupSet's "/backup-sets/*" catch-all
		// below: PATCH and GET are different methods, so the two coexist
		// on overlapping paths without either shadowing the other.
		//
		// PATCH, because this is a partial edit of a resource and this
		// package already spells that PATCH at /settings; see the
		// handler's own doc for why not PUT, and for why it carries
		// requireCSRF and not requireDestructiveGate (creation's
		// precedent, not the gate's: the gate is for run_immediately and
		// for retention apply, not for writing a set).
		r.With(requireCSRF).Patch("/backup-sets/{source}/{set}", h.updateBackupSet)
		// Issue #391: removing one backup set's configuration. Registered
		// on the same two named segments as the PATCH above, which is why
		// it can share a path with it and with getBackupSet's
		// "/backup-sets/*" catch-all below: chi tells the three apart by
		// method.
		//
		// requireCSRF and NOT requireDestructiveGate, the same tier as the
		// PATCH beside it and as POST /backup-sets
		// (destructiveGateExemptRoutes, router_test.go). The word on the
		// button is "Remove" and the dialog is styled destructive, but
		// §50's bucket is decided by what is touched, not by how it
		// reads: this writes configuration, and every byte of backup data
		// the set collected stays on storage and stays listed. Gating it
		// would also put it out of reach of an operator who has not
		// turned destructive operations on, for an operation whose whole
		// point is to STOP this manager doing things.
		r.With(requireCSRF).Delete("/backup-sets/{source}/{set}", h.removeBackupSet)
		// Issue #350's edit hold. The read is read-only (§50) and carries
		// neither CSRF nor the gate; both writes carry CSRF and, like
		// every other route in this group, not the destructive gate. A
		// hold STOPS work rather than starting or deleting any, so
		// gating it would mean an operator who has not turned
		// destructive operations on cannot safely edit a set at all.
		//
		// Registered as two named segments plus a static tail, the same
		// shape as the retention preview above, so chi's own node
		// ordering matches them ahead of the "/backup-sets/*" catch-all.
		r.Get("/backup-sets/{source}/{set}/edit-hold", h.getBackupSetEditHold)
		r.With(requireCSRF).Post("/backup-sets/{source}/{set}/edit-hold", h.takeBackupSetEditHold)
		r.With(requireCSRF).Post("/backup-sets/{source}/{set}/edit-hold/release", h.releaseBackupSetEditHold)
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

		// Issue #419: the operator route out of FAILED. It carries
		// requireCSRF and not requireDestructiveGate, exactly like the
		// quarantine retry below and for the same reason one state along:
		// it moves a journal row back into the pipeline and cannot reach
		// a remote delete at all, because FAILED is only ever reached
		// before COMMITTED.
		r.With(requireCSRF).Post("/backups/{source}/{set}/{name}/retry", h.retryFailedIngestion)
		r.Get("/activity", h.listActivity)
		// Issue #573: what each backup set is DOING right now, as
		// opposed to the durable record of what happened that the route
		// immediately above serves. Read-only (§50), so neither CSRF nor
		// the destructive gate, exactly like its neighbour.
		//
		// Registered as a static child of "/activity" rather than as a
		// sub-path of a param route, so there is no shape it could be
		// confused with: "/activity" has no wildcard, and chi matches a
		// static segment before anything else, so the two coexist
		// whichever order they are registered in.
		r.Get("/activity/live", h.getLiveActivity)
		r.Get("/quarantine", h.listQuarantine)

		// The three operator actions a quarantined backup has. All carry
		// requireCSRF; none carries requireDestructiveGate, and their
		// handlers' own docs record why: revalidate writes nothing at all,
		// retry moves a journal row back into the pipeline without
		// touching a local file or a remote object, and reinstate moves a
		// journal row back to the durable state it already held, which
		// strictly REDUCES the set of remote objects this manager will
		// ever delete (issue #220: a reinstated backup is refused by
		// FR-15's delete gate permanently).
		r.With(requireCSRF).Post("/quarantine/{source}/{set}/{name}/revalidate", h.revalidateArtifact)
		r.With(requireCSRF).Post("/quarantine/{source}/{set}/{name}/retry", h.retryArtifactIngestion)
		r.With(requireCSRF).Post("/quarantine/{source}/{set}/{name}/reinstate", h.reinstateArtifact)

		// Issue #443: prove one declared storage medium works before a
		// cycle carrying a real backup finds out for an operator. CSRF,
		// because it writes a probe object to real storage and deletes it
		// again; not the destructive gate, because the only object it can
		// reach is the one it just wrote (see handlers_mediums.go).
		r.With(requireCSRF).Post("/storage-mediums/{id}/preflight", h.preflightStorageMedium)

		// G2.2 (issue #594): the storage-destination write surface, so a
		// destination can be added without hand-editing config.yaml, and
		// the candidate probe that proves one BEFORE it is written down.
		//
		// The static "/storage-mediums/preflight" is registered here,
		// beside the "{id}" routes rather than buried among them, because
		// the order it reads in is load-bearing to a person even though
		// it is not to chi: chi's trie matches static before param, so
		// "preflight" can never be read as a medium id whichever order
		// these lines appear in, and writing them adjacent is what makes
		// that visible to whoever adds the next route.
		//
		// CSRF on every write, and on the candidate probe for the reason
		// the by-id probe above carries it: it writes a real object into
		// somebody's bucket and deletes it again. Not the destructive
		// gate on any of them. Declaring a destination MOVES NOTHING
		// (artifacts arrive only once a retention tier names it, which is
		// a separate write with its own disclosure), removing one deletes
		// no backup data and is refused outright while any copy names it,
		// and the probe can reach only the object it just wrote. Gating
		// these would train an operator to click through the
		// acknowledgment that actually matters.
		r.With(requireCSRF).Post("/storage-credentials", h.importStorageCredentials)
		r.Get("/storage-mediums", h.listStorageMediums)
		r.With(requireCSRF).Post("/storage-mediums", h.createStorageMedium)
		r.With(requireCSRF).Post("/storage-mediums/preflight", h.preflightStorageMediumCandidate)
		r.Get("/storage-mediums/{id}", h.getStorageMedium)
		r.With(requireCSRF).Put("/storage-mediums/{id}", h.updateStorageMedium)
		r.With(requireCSRF).Delete("/storage-mediums/{id}", h.removeStorageMedium)
		r.With(requireCSRF).Put("/storage-mediums/{id}/default", h.setDefaultStorageMedium)
		r.Get("/storage-mediums/{id}/usage", h.getStorageMediumUsage)

		// The configuration routes (I2.2, issue #669), which speak in
		// manifest field ids. Gated exactly as their spec-shaped
		// neighbours above are, and for the identical reasons: the read
		// is a read, and the two writes carry CSRF and not the
		// destructive gate because nothing they can reach touches a
		// backup - the probe writes and deletes only the object it
		// generated a key for, and the configure write changes a
		// declaration.
		r.Get("/storage-mediums/{id}/configuration", h.getStorageMediumConfiguration)
		r.With(requireCSRF).Post("/storage-mediums/{id}/configuration/preflight", h.preflightStorageMediumConfiguration)
		r.With(requireCSRF).Put("/storage-mediums/{id}/configuration", h.configureStorageMedium)

		// Issue #211: FR-9 catalog recovery, the API expression of
		// `backupd catalog rebuild` and its --dry-run. Rebuild only
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

		// EPIC I (#664): the registered-backend catalogue #668's
		// add-a-destination picker renders, and the naming rules its
		// name field refuses against. Read-only for the route above's
		// reason one step further on — a manifest decides what a
		// destination may BE, including which rclone backend it dials,
		// so a client-extensible catalogue would put FR-4's gate on the
		// far side of the network from the binary it constrains. Static
		// path, so the "/backup-sets/*" catch-all above cannot shadow
		// it.
		r.Get("/backends", h.listBackends)

		r.With(requireCSRF).Post("/ssh-keys", h.importSSHKey)
		// The other way to end up holding a key reference (#592):
		// selecting one this machine already holds, by the opaque handle
		// the candidate scan gave it. Its own route rather than a second
		// mode of the one above, because pasting material this host has
		// never seen and choosing a key it already found are different
		// acts, and because a shared body would have had to stop
		// requiring private_key_pem, which is a promise POST /ssh-keys
		// has already made. Same tier, so the same CSRF and the same
		// absence of a destructive gate.
		r.With(requireCSRF).Post("/ssh-keys/from-candidate", h.importSSHKeyFromCandidate)
		r.With(requireCSRF).Post("/ssh/host-key-probe", h.probeHostKey)

		// Issue #592: the two reads this surface never had. Everything
		// else under /ssh is a write or a probe, which is exactly why an
		// imported key's id crossed the wire once and could never be
		// asked for again.
		//
		// Read-only under §50 and unlike their neighbours in the truest
		// sense: neither opens an outbound connection to anything, so
		// neither carries requireCSRF, matching every other GET here. The
		// candidate scan reads a fixed, constant set of locations decided
		// in core, never a caller-supplied path, so there is no request
		// shape that turns it into a filesystem oracle.
		r.Get("/ssh-keys", h.listSSHKeys)
		r.Get("/ssh/key-candidates", h.listSSHKeyCandidates)

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
