package serve

import (
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
)

// UIConfig is everything NewUI needs to build the UI-host container's
// handler.
type UIConfig struct {
	// Upstream is the engine's own base URL as reachable from the UI
	// host - e.g. over an internal Docker network
	// (http://rclone-manager:8080, the compose service name resolved
	// through Docker's embedded DNS), never a published host port.
	Upstream *url.URL

	// StaticFS is the shared UI's built static bundle, or a placeholder,
	// rooted at the bundle's own top level (i.e. StaticFS's "index.html"
	// IS the app shell), not at a "dist" subdirectory - a caller passing
	// a go:embed FS that embeds paths as "dist/index.html" should first
	// fs.Sub it down to "dist" (see e.g. apps/generic/webui's own doc).
	StaticFS fs.FS

	// Identity, when non-nil, is the trust boundary this proxy sanitizes
	// the platform identity header against before forwarding a request
	// upstream: the header survives only for a request that arrived from
	// a configured gateway peer. A gateway profile MUST supply one -
	// see IdentitySanitizer's own doc for why this hop is the one that
	// owns the strip.
	Identity IdentitySanitizer

	// ProxyResponseHeaderTimeout overrides defaultProxyResponseHeaderTimeout
	// below. Zero means use the default; this only exists so a test can
	// use a short timeout instead of waiting out a multi-second one for
	// real.
	ProxyResponseHeaderTimeout time.Duration
}

// IdentitySanitizer removes a platform identity header from a request
// that did not arrive from a trusted gateway peer, and leaves it alone
// for one that did. *apps/common/platform/profile.CompiledGateway is the
// implementation; this interface exists so this package depends on the
// behaviour rather than on the profile table.
//
// # Which hop owns the strip
//
// This one, and the engine as well. The two are not redundant, they
// answer for different peers, and the reason the decision is written
// down here is that it is topology-dependent and getting it wrong in
// either direction is silent.
//
// The engine has no published port: its only peer is this process, over
// the internal network. So the engine's own trust test can only ever say
// "this came from web-ui", which is true of a forged header and a
// genuine one alike. This process is the hop that CAN tell them apart,
// because it is the one with a published port and therefore the one
// whose RemoteAddr is the real client's. A client hitting that port with
// its own X-Ugos-User is outside the gateway range and loses the header
// here; a request the platform's gateway actually proxied arrives from
// the gateway's own address and keeps it.
//
// Stripping unconditionally instead would be wrong for the deployment
// this defends: on UGOS the platform gateway sits UPSTREAM of this
// process, so an unconditional delete removes the legitimate identity
// and native authentication stops working entirely. That is why this
// carries the same trust boundary the engine does rather than a bare
// header name.
type IdentitySanitizer interface {
	Sanitize(h http.Header, remoteAddr string)
}

// StripAll is the IdentitySanitizer for a gateway profile with no
// declared trust boundary: it removes the named header from every
// request, whoever sent it. That is the fail-closed reading of "there is
// no gateway here", and it is deliberately not a refusal to start,
// because this process's other job is serving the right UI bundle and
// nothing about a bundle depends on the identity header. Native
// authentication does not work in that configuration, which is correct:
// the engine refuses to start on the same profile with no range, so the
// deployment was never going to authenticate anyone anyway.
type StripAll string

func (h StripAll) Sanitize(header http.Header, _ string) { header.Del(string(h)) }

// defaultProxyResponseHeaderTimeout bounds how long the reverse proxy
// below waits for the engine to even START responding (send response
// headers) once it has accepted a connection - issue #119's review,
// empirically demonstrated: a connection-REFUSED engine fails fast and
// correctly (502 in ~1ms), but an engine that accepts a connection and
// then never responds at all hangs the proxied request indefinitely,
// bounded only by whatever timeout the calling browser's own fetch()
// happens to set, which for a bare fetch() is no timeout at all. This is
// a same-host, single-hop, internal call to a process from this exact
// same image - a few seconds is already generous, not a real operation's
// actual latency budget (every route this proxies has its own separate
// response-body streaming, so this timeout only ever bounds the wait for
// the FIRST byte back, never a legitimately slow but already-answering
// request).
const defaultProxyResponseHeaderTimeout = 5 * time.Second

// NewUI composes cfg into the UI-host container's whole HTTP surface: the
// shared static UI, plus a reverse proxy forwarding /health/* and
// /api/v1/* to the engine unchanged (same path, same method, same body) -
// see this package's doc comment for the full routing shape.
//
// A plain net/http/httputil.ReverseProxy is deliberately all this uses:
// no nginx, no new runtime dependency - a dedicated reverse proxy is
// unwarranted complexity for "forward this path unchanged to one fixed
// upstream."
//
// # X-Forwarded-For/-Proto: exactly one hop's worth, never the client's own
//
// The Rewrite func below explicitly deletes any X-Forwarded-For header
// the incoming request already carries before calling SetXForwarded,
// which recomputes it from THIS request's own RemoteAddr (the real
// external client, assuming nothing sits between the browser and this
// process) - never from whatever a client sent. Skipping that delete
// would let a client set its own X-Forwarded-For directly against this
// container's published port, and ProxyRequest.SetXForwarded's own
// documented append-to-existing behavior would then treat that
// CLIENT-CHOSEN value as the trusted "original client" once it reaches an
// engine whose apps/common/auth/local.Config.TrustForwardedHeaders is
// true (ratelimit.go's remoteIP reads the first entry), letting an
// attacker rotate a fake header on every request to evade rate limiting
// entirely - exactly the vulnerability that makes trusting this header
// anywhere at all conditional on there being only one, verified,
// network-isolated hop in between. Once deleted, SetXForwarded sets
// X-Forwarded-For to this request's own RemoteAddr and X-Forwarded-Proto
// to "https"/"http" based on this request's own r.TLS (never a header),
// giving the engine exactly what it needs to fix both
// apps/common/auth/local.Config.TrustForwardedHeaders findings (the
// rate-limit collapse and the Secure cookie flag) without this container
// needing to know anything about that config itself.
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
			// Same reasoning as the delete above, one header along: a
			// client-supplied identity header reaching the engine is
			// believed there, because the engine trusts this hop. The
			// trust test runs against pr.In.RemoteAddr, the address of
			// whoever actually connected to this process, never against
			// anything the request carried.
			if cfg.Identity != nil {
				cfg.Identity.Sanitize(pr.Out.Header, pr.In.RemoteAddr)
			}
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
