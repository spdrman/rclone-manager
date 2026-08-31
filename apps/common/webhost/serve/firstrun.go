// firstrun.go is the composition half of issue #176: how a provider app
// serves an instance that has no configuration yet, and how that same
// process becomes a fully configured one without a restart.
//
// It lives here, beside NewEngine, rather than in any one provider's
// main.go, because all five packaged platforms (TrueNAS, Unraid,
// OpenMediaVault, Synology, UGOS) install the same canonical image and
// hit the same fresh-install problem. A provider supplies its own paths
// and its own PlatformAdapter; the sequence is identical.
package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/webhost"
)

// ErrNoActivator is returned by NewFirstRunEngine when cfg carries a
// first-run surface but no way to open a backend once setup completes.
// Refusing at construction is the point: the alternative is an instance
// that runs a whole setup flow and then cannot do anything with the
// configuration it just wrote.
var ErrNoActivator = errors.New("serve: a first-run engine needs both FirstRun and Activate")

// FirstRunEngine is the HTTP surface of an instance that starts with no
// configuration. It is three things at once, deliberately, because a host
// has to drive all three off the same activation moment:
//
//   - an http.Handler, serving the setup surface until setup completes
//     and the full application afterwards;
//   - a Scheduler, which RunEngine can be handed exactly as it is handed
//     a *service.BackupService, and which simply waits for a backend to
//     exist before it starts ticking;
//   - a Closer, so whatever activation opened (a journal, a shared
//     journal lock) is released on shutdown by the same process that
//     opened it.
//
// Activation happens at most once, on the POST that writes the first
// configuration. There is no path back: an instance that has been
// configured stays configured for the life of the process, and the setup
// route refuses from that moment on (apps/common/webhost's firstrun.go).
type FirstRunEngine struct {
	cfg EngineConfig

	// handler is swapped exactly once, from the setup surface to the full
	// application. Every in-flight and subsequent request reads it with a
	// lock-free Load, so a request arriving during activation gets one
	// coherent handler rather than a torn read — the same reason
	// core/service.BackupService keeps its own configState behind an
	// atomic.Pointer.
	handler atomic.Pointer[http.Handler]

	// activateMu serializes activation against itself, so two setup
	// submissions racing each other cannot both open a backend. The
	// configuration write underneath is already exclusive
	// (core/service's writeConfigExclusively), so at most one of them can
	// reach here having actually created anything; this keeps the loser
	// from opening a second journal handle on its way to finding out.
	activateMu sync.Mutex
	backend    atomic.Pointer[activated]

	// ready is closed on activation, which is what lets RunOnSchedule
	// below block until there is something to schedule instead of
	// returning immediately and leaving the process with no scheduler
	// for the rest of its life.
	ready     chan struct{}
	readyOnce sync.Once
}

// activated bundles what one successful activation produced, so the
// backend and the cleanup that releases it can never be observed
// independently.
type activated struct {
	backend webhost.BackupServiceClient
	cleanup func() error
}

// NewFirstRunEngine composes the engine surface of an unconfigured
// instance. cfg.Backend must be nil (that is what "unconfigured" means),
// and cfg.FirstRun and cfg.Activate must both be set.
func NewFirstRunEngine(cfg EngineConfig) (*FirstRunEngine, error) {
	if cfg.FirstRun == nil || cfg.Activate == nil {
		return nil, ErrNoActivator
	}
	if cfg.Backend != nil {
		return nil, fmt.Errorf("serve: a first-run engine must start with no backend")
	}

	e := &FirstRunEngine{cfg: cfg, ready: make(chan struct{})}
	h := newEngineHandler(cfg, nil, e.activate)
	e.handler.Store(&h)
	return e, nil
}

func (e *FirstRunEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*e.handler.Load()).ServeHTTP(w, r)
}

// activate is what apps/common/webhost calls once the first configuration
// is durably on disk: open a real backend against it and swap this
// process over to serving the application. An error here leaves the
// setup surface in place and is reported to the operator as
// restart_required, never as a failed setup — the configuration exists
// either way, so a restart genuinely does finish the job.
func (e *FirstRunEngine) activate(ctx context.Context) error {
	e.activateMu.Lock()
	defer e.activateMu.Unlock()

	if e.backend.Load() != nil {
		return nil
	}

	backend, cleanup, err := e.cfg.Activate(ctx)
	if err != nil {
		return err
	}

	// The new handler keeps cfg.FirstRun, so GET /system/first-run keeps
	// answering (configured: true now) and a late setup submission is
	// refused with a 409 that says why. onConfigured is nil on it: there
	// is nothing left to activate.
	h := newEngineHandler(e.cfg, backend, nil)
	e.handler.Store(&h)
	e.backend.Store(&activated{backend: backend, cleanup: cleanup})
	e.readyOnce.Do(func() { close(e.ready) })
	return nil
}

// Backend returns the backend activation opened, or nil while this
// instance is still unconfigured.
func (e *FirstRunEngine) Backend() webhost.BackupServiceClient {
	if a := e.backend.Load(); a != nil {
		return a.backend
	}
	return nil
}

// Close releases whatever activation opened. It is safe to call on an
// instance that was never configured, which is exactly the case a
// `defer` in a provider's main has to cover.
func (e *FirstRunEngine) Close() error {
	a := e.backend.Load()
	if a == nil || a.cleanup == nil {
		return nil
	}
	return a.cleanup()
}

// PollInterval satisfies Scheduler. It is never the interval actually
// used: there is no configuration to read one from until activation, so
// RunOnSchedule below reads the real value off the activated backend
// instead of trusting what RunEngine passes it. Zero is returned rather
// than a guess, so nothing can silently schedule on a made-up interval.
func (e *FirstRunEngine) PollInterval() time.Duration { return 0 }

// RunOnSchedule satisfies Scheduler by waiting for activation and then
// delegating to the real backend's own loop, on its own configured
// interval. A process that is shut down before it is ever configured
// returns nil: never having been configured is not a scheduler failure.
//
// The interval argument is deliberately ignored, for the reason
// PollInterval gives above.
func (e *FirstRunEngine) RunOnSchedule(ctx context.Context, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return nil
	case <-e.ready:
	}

	scheduler, ok := e.Backend().(Scheduler)
	if !ok {
		// A backend that cannot schedule is not an error either: the HTTP
		// surface is still serving, and RunEngine's own nil-scheduler
		// case already describes a host with nothing to tick.
		return nil
	}
	return scheduler.RunOnSchedule(ctx, scheduler.PollInterval())
}

// newEngineHandler is NewEngine's body, with the backend and the
// activation callback passed in rather than read off cfg, so the same
// composition builds both the setup surface (nil backend, an activation
// callback) and the application (a real backend, nothing left to
// activate).
func newEngineHandler(cfg EngineConfig, backend webhost.BackupServiceClient, onConfigured func(context.Context) error) http.Handler {
	apiRouter := webhost.NewRouter(webhost.RouterConfig{
		Platform:      cfg.Platform,
		Backend:       backend,
		Gate:          cfg.Gate,
		FirstRun:      cfg.FirstRun,
		OnConfigured:  onConfigured,
		BinaryVersion: cfg.BinaryVersion,
		Commit:        cfg.Commit,
	})

	mux := http.NewServeMux()
	if cfg.AuthRoutes != nil {
		mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", cfg.AuthRoutes))
	}
	mux.Handle("/health/", apiRouter)
	mux.Handle("/api/v1/", apiRouter)

	handler := local.EnsureCSRFCookie(cfg.TrustForwardedHeaders)(mux)

	// The identity strip, on the request path rather than described in a
	// doc comment. A gateway Authenticator decides whether to BELIEVE the
	// header; this deletes it outright for an untrusted peer, so nothing
	// downstream - a handler, a log line, a future middleware - can read
	// a value that was never trusted. serve.NewUI runs the same strip one
	// hop out, where the client's own address is still visible; see
	// IdentitySanitizer's doc for why both hops do it.
	//
	// It wraps the whole composition, so it covers the setup surface as
	// well as the application: an unconfigured instance is exactly when
	// an unauthenticated caller must not be able to name itself an admin
	// through a header nobody has decided to trust yet.
	if cfg.Platform != nil {
		if s, ok := cfg.Platform.Authenticator().(IdentitySanitizer); ok {
			inner := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.Sanitize(r.Header, r.RemoteAddr)
				inner.ServeHTTP(w, r)
			})
		}
	}

	return handler
}
