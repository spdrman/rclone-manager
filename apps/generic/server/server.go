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
	"time"

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

	// cfg.Auth.TrustForwardedHeaders() is the SAME setting cfg.Auth
	// already consults internally for rate limiting and its own session
	// cookie's Secure flag (apps/common/auth/local.Config.TrustForwardedHeaders) -
	// reusing it here, rather than a second, independently-configured
	// field on EngineConfig, is what keeps this CSRF cookie's Secure flag
	// from being able to drift out of sync with the session cookie's own.
	return local.EnsureCSRFCookie(cfg.Auth.TrustForwardedHeaders())(mux)
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

	// ProxyResponseHeaderTimeout overrides defaultProxyResponseHeaderTimeout
	// below. Zero means use the default; this only exists so a test can
	// use a short timeout instead of waiting out a multi-second one for
	// real.
	ProxyResponseHeaderTimeout time.Duration
}

// defaultProxyResponseHeaderTimeout bounds how long the reverse proxy
// below waits for the engine to even START responding (send response
// headers) once it has accepted a connection - issue #119's review,
// empirically demonstrated: a connection-REFUSED engine fails fast and
// correctly (502 in ~1ms), but an engine that accepts a connection and
// then never responds at all hangs the proxied request indefinitely,
// bounded only by whatever timeout the calling browser's own fetch()
// happens to set, which for a bare fetch() is no timeout at all. This is
// a same-host, single-hop, internal Docker network call to a process from
// this exact same image - a few seconds is already generous, not a real
// operation's actual latency budget (every route this proxies has its own
// separate response-body streaming, so this timeout only ever bounds the
// wait for the FIRST byte back, never a legitimately slow but
// already-answering request).
const defaultProxyResponseHeaderTimeout = 5 * time.Second

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
//
// # X-Forwarded-For/-Proto: exactly one hop's worth, never the client's own
//
// The Rewrite func below explicitly deletes any X-Forwarded-For header
// the incoming request already carries before calling SetXForwarded,
// which recomputes it from THIS request's own RemoteAddr (the real
// external client, since nothing sits between the browser and this
// process in the shipped topology) - never from whatever a client sent.
// Skipping that delete would let a client set its own X-Forwarded-For
// directly against this container's published port, and
// ProxyRequest.SetXForwarded's own documented append-to-existing behavior
// would then treat that CLIENT-CHOSEN value as the trusted "original
// client" once it reaches apps/common/auth/local's own
// Config.TrustForwardedHeaders=true engine (ratelimit.go's remoteIP reads
// the first entry), letting an attacker rotate a fake header on every
// request to evade rate limiting entirely - exactly the vulnerability
// that makes trusting this header anywhere at all conditional on there
// being only one, verified, network-isolated hop in between. Once
// deleted, SetXForwarded sets X-Forwarded-For to this request's own
// RemoteAddr and X-Forwarded-Proto to "https"/"http" based on this
// request's own r.TLS (never a header), giving the engine exactly what it
// needs to fix both apps/common/auth/local.Config.TrustForwardedHeaders
// findings (the rate-limit collapse and the Secure cookie flag) without
// this container needing to know anything about that config itself.
func NewUI(cfg UIConfig) http.Handler {
	timeout := cfg.ProxyResponseHeaderTimeout
	if timeout <= 0 {
		timeout = defaultProxyResponseHeaderTimeout
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.Upstream)
			pr.Out.Header.Del("X-Forwarded-For")
			pr.SetXForwarded()
		},
		Transport: transport,
	}

	mux := http.NewServeMux()
	mux.Handle("/health/", proxy)
	mux.Handle("/api/v1/", proxy)
	mux.Handle("/", staticHandler(cfg.StaticFS))

	// false: this container is the actual internet-facing edge (the only
	// one with a published port) and already observes its own real TLS
	// status directly via r.TLS - it must never trust a forwarded header
	// from just anyone hitting its published port the way the engine (on
	// the OTHER side of this exact proxy) is allowed to trust headers
	// THIS proxy itself sets.
	return local.EnsureCSRFCookie(false)(mux)
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
