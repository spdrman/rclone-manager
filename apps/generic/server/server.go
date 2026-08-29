// Package server composes the generic Web host's whole HTTP surface
// (docs/EPIC-B-multi-nas.md §9.2): local authentication, the versioned
// /api/v1 API, and the shared static UI, all behind one handler.
//
// # Routing
//
//	EnsureCSRFCookie (every request gets a CSRF cookie, including the
//	first one that will ever need to echo it back)
//	     │
//	     ├── /api/v1/auth/*  -> apps/common/auth/local's own Handler()
//	     │                      (login/enroll/logout/session - reachable
//	     │                      WITHOUT an existing session, the opposite
//	     │                      of everything else under /api/v1)
//	     │
//	     ├── /health/*       -> apps/common/webhost's router
//	     ├── /api/v1/*       -> apps/common/webhost's router (gated by
//	     │                      Config.Auth's Authenticator via that
//	     │                      package's own authMiddleware)
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
	"strings"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/webhost"
	"github.com/spdrman/rclone-manager/apps/generic/platform"
)

// Config is everything New needs to build the composed handler.
type Config struct {
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

	// StaticFS is ui/shared's built static bundle, or a placeholder (see
	// apps/generic/webui). Rooted at the bundle's own top level (i.e.
	// StaticFS's "index.html" IS the app shell), not at a "dist"
	// subdirectory - a caller passing apps/generic/webui.Assets directly
	// should first fs.Sub it down to "dist" (webui.Assets embeds paths as
	// "dist/index.html", see that package's own doc).
	StaticFS fs.FS
}

// New composes cfg into the generic Web host's whole HTTP surface. See
// this package's doc comment for the routing shape.
func New(cfg Config) http.Handler {
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
