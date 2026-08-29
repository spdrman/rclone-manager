// Package server composes the generic Web host's HTTP surface into two
// separate handlers, one per container (project-owner requirement,
// folded into issue #82/B4.1 before merge): the engine
// (core service/scheduler + local auth + the versioned /api/v1 API, no
// published port - reachable only from the web-ui container over the
// internal Docker network) and the UI host (the shared static bundle
// plus a reverse proxy to the engine, the only container with a
// LAN-facing published port). Both run from the SAME binary/image
// (apps/generic/cmd/backup-manager-web), selected by which subcommand a
// container's `command:` invokes - matching the "one canonical image,
// vary command" principle already established for
// `/backup-manager`/`/backup-manager-web` themselves.
//
// # Engine routing (NewEngine)
//
//	EnsureCSRFCookie (every response gets a CSRF cookie, including the
//	first one that will ever need to echo it back)
//	     │
//	     ├── /api/v1/auth/*  -> apps/common/auth/local's own Handler()
//	     │                      (login/enroll/logout/session - reachable
//	     │                      WITHOUT an existing session, the opposite
//	     │                      of everything else under /api/v1)
//	     │
//	     └── /health/*, /api/v1/* -> apps/common/webhost's router (gated
//	                                 by Config.Auth's Authenticator via
//	                                 that package's own authMiddleware)
//
// No static UI here at all: everything else 404s. Serving the shared UI
// is the OTHER container's job (NewUI).
//
// # UI-host routing (NewUI)
//
//	EnsureCSRFCookie (same reasoning as above: the browser's very first
//	request - the static shell itself - is what has to carry this
//	cookie from here on, since this is the only container the browser
//	ever talks to directly)
//	     │
//	     ├── /health/*, /api/v1/* -> reverse-proxied to the engine,
//	     │                           unmodified path (net/http/httputil,
//	     │                           no new dependency)
//	     │
//	     └── everything else -> the static UI, falling back to
//	                            index.html for any path that isn't a real
//	                            asset (React Router's BrowserRouter needs
//	                            a hard refresh at a client-side route to
//	                            still get the app shell, not a 404)
package server

import (
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/webhost"
	"github.com/spdrman/rclone-manager/apps/generic/platform"
)

// EngineConfig is everything NewEngine needs to build the engine
// container's handler.
type EngineConfig struct {
	// Backend is the core/service.BackupService (or a test double) every
	// apps/common/webhost handler ultimately calls into.
	Backend webhost.BackupServiceClient

	// Auth backs both the /api/v1/auth/* routes and the Authenticator
	// apps/common/webhost's authMiddleware consults for everything else
	// under /api/v1.
	Auth *local.Service

	// Gate decides whether POST /api/v1/operations may run; nil means
	// apps/common/webhost's own NotYetImplementedGate default (destructive
	// operations fail closed until #92/B1.3 lands).
	Gate webhost.DestructiveGate

	BinaryVersion string
	Commit        string
}

// NewEngine composes cfg into the engine container's whole HTTP surface:
// local authentication plus the versioned /api/v1 API, and nothing else -
// see this package's doc comment for the full routing shape and why the
// static UI is deliberately not here.
func NewEngine(cfg EngineConfig) http.Handler {
	adapter := platform.New(cfg.Auth)

	apiRouter := webhost.NewRouter(webhost.RouterConfig{
		Platform:      adapter,
		Backend:       cfg.Backend,
		Gate:          cfg.Gate,
		BinaryVersion: cfg.BinaryVersion,
		Commit:        cfg.Commit,
	})

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", cfg.Auth.Handler()))
	mux.Handle("/health/", apiRouter)
	mux.Handle("/api/v1/", apiRouter)

	return local.EnsureCSRFCookie(mux)
}

// UIConfig is everything NewUI needs to build the UI-host container's
// handler.
type UIConfig struct {
	// Upstream is the engine container's own base URL as reachable over
	// the internal Docker network (e.g. "http://rclone-manager:8080" -
	// the compose service name, resolved through Docker's embedded DNS,
	// not a published host port: the engine has none).
	Upstream *url.URL

	// StaticFS is ui/shared's built static bundle, or a placeholder (see
	// apps/generic/webui). Rooted at the bundle's own top level (i.e.
	// StaticFS's "index.html" IS the app shell), not at a "dist"
	// subdirectory - a caller passing apps/generic/webui.Assets directly
	// should first fs.Sub it down to "dist" (webui.Assets embeds paths as
	// "dist/index.html", see that package's own doc).
	StaticFS fs.FS
}

// NewUI composes cfg into the UI-host container's whole HTTP surface: the
// shared static UI, plus a reverse proxy forwarding /health/* and
// /api/v1/* to the engine unchanged (same path, same method, same body) -
// see this package's doc comment for the full routing shape.
//
// A plain net/http/httputil.ReverseProxy is deliberately all this uses:
// no nginx, no new runtime dependency, matching the same "one canonical
// image" principle this split is itself built on - a dedicated reverse
// proxy is unwarranted complexity for "forward this path unchanged to one
// fixed upstream."
func NewUI(cfg UIConfig) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(cfg.Upstream)

	mux := http.NewServeMux()
	mux.Handle("/health/", proxy)
	mux.Handle("/api/v1/", proxy)
	mux.Handle("/", staticHandler(cfg.StaticFS))

	return local.EnsureCSRFCookie(mux)
}

// staticHandler serves fsys, falling back to index.html for any path
// that isn't a real file in fsys - the standard SPA-hosting pattern
// react-router-dom's BrowserRouter needs: a hard refresh (or a bookmark,
// or a shared link) at a client-side route like /sets/abc has to reach
// the same app shell the client-side router then renders into, not a
// 404 from whatever's actually serving the static files.
func staticHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(fsys, p); err != nil {
			// "/", not "/index.html": net/http's FileServer special-cases
			// a request path ending in "/index.html" by redirecting it to
			// "./" (so the file is never served under that literal URL),
			// which would otherwise turn this fallback into a redirect
			// loop for every client-side route. Asking for "/" gets the
			// same index.html content without tripping that redirect.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
