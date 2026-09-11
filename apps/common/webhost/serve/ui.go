package serve

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/backupdproject/backupd/apps/common/auth/local"
	"github.com/backupdproject/backupd/apps/common/platform/profile"
	"github.com/backupdproject/backupd/apps/common/webhost"
)

// The UI half of the two-container split: the only process with a
// published port, and therefore the only hop that can tell a real client
// from a forged one.
//
// That position is what makes this file's three sanitising steps
// load-bearing rather than defensive. It deletes any X-Forwarded-For the
// client sent before recomputing it from the real peer address, because
// the engine behind it is configured to believe that header and a client
// that could set it would pick a fresh rate-limit bucket per request. It
// resolves the identity-header trust boundary here, at the hop the
// platform gateway actually connects to, rather than one hop further in
// where every request looks like it came from this container; the section
// comment further down, beside UIConfig.Gateway, has that argument in
// full, including why an unconditional strip would be wrong. And it
// deletes the upstream's copies of the browser security headers so this
// container is the single authority for what a browser is told, which is
// also the honest arrangement since it is the one talking to the browser.
//
// The proxy itself is net/http's, on purpose. Forwarding two path prefixes
// unchanged to one fixed upstream does not justify a second server in the
// image.
//
// The static handler falls back to the app shell for anything that is not
// a real file, which is what a client-side router needs for a hard refresh
// on a deep link to reach the app instead of a 404.

// UIConfig is everything NewUI needs to build the UI-host container's
// handler.
type UIConfig struct {
	// Upstream is the engine's own base URL as reachable from the UI
	// host - e.g. over an internal Docker network
	// (http://backupd:8080, the compose service name resolved
	// through Docker's embedded DNS), never a published host port.
	Upstream *url.URL

	// StaticFS is the shared UI's built static bundle, or a placeholder,
	// rooted at the bundle's own top level (i.e. StaticFS's "index.html"
	// IS the app shell), not at a "dist" subdirectory - a caller passing
	// a go:embed FS that embeds paths as "dist/index.html" should first
	// fs.Sub it down to "dist" (see e.g. apps/generic/webui's own doc).
	StaticFS fs.FS

	// Gateway is the peer set THIS hop may believe a provider-native
	// identity header from. It is the actual trust boundary of the whole
	// product: this is the only service with a LAN-facing published port,
	// so this is the only hop where "did a gateway send this, or did a
	// LAN client" is still a question the network can answer.
	//
	// nil means nothing is trusted here, so every identity header any
	// profile declares is removed from every inbound request. That is the
	// right default and it is what the shipped topology gets: an engine
	// behind this proxy trusts this proxy by necessity, so a header this
	// proxy passes through unexamined is a header the engine believes.
	// A deployment that really does sit behind a platform gateway sets
	// this to that gateway's range, HERE, at the hop the gateway actually
	// connects to — not one hop further in, where every request looks
	// like it came from this container.
	//
	// See StripUntrustedIdentity (security.go) for what peer-based trust
	// does and does not settle under Docker port publishing.
	Gateway *profile.CompiledGateway

	// ProxyResponseHeaderTimeout overrides defaultProxyResponseHeaderTimeout
	// below. Zero means use the default; this only exists so a test can
	// use a short timeout instead of waiting out a multi-second one for
	// real.
	ProxyResponseHeaderTimeout time.Duration

	// Logger is where this hop's own failures go (issue #730). Nil means
	// the same stdout JSON default NewRouter uses, so a host that wired
	// none still gets the one line that says the browser's request died
	// HERE rather than at the engine — the distinction the reported
	// symptom ("TypeError: Failed to fetch" in the browser, a clean 401
	// from curl) cannot be made from the browser's side at all.
	Logger webhost.Logger

	// Debug turns on the per-request upstream trace below regardless of
	// the environment. It only exists so a test can assert the trace
	// without setting process-wide environment; a deployment turns it on
	// with RM_DEBUG=1 or LOG_LEVEL=debug, which webhost.DebugEnabled
	// reads.
	Debug bool
}

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
// Stripping unconditionally at this hop instead would be wrong for the
// deployment this defends: on UGOS the platform gateway sits UPSTREAM of
// this process, so an unconditional delete removes the legitimate
// identity and native authentication stops working entirely. That is why
// UIConfig.Gateway carries a trust boundary rather than a bare header
// name, and why the boundary is a *profile.CompiledGateway rather than
// an interface: exactly one thing decides which peers may be believed,
// and both hops read it. See StripUntrustedIdentity (security.go) for the
// strip itself, and for the one shape peer-based trust does not settle.

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

	logger := cfg.Logger
	if logger == nil {
		logger = webhost.NewStdoutLogger()
	}
	debug := cfg.Debug || webhost.DebugEnabled()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.Upstream)
			pr.Out.Header.Del("X-Forwarded-For")
			// The identity header gets the same treatment one layer
			// out, and deliberately not here as well. It is deleted
			// from r.Header by StripUntrustedIdentity before this
			// proxy is reached at all (see the return below), so by
			// the time pr.Out is copied from it there is nothing left
			// to sanitize, and a second strip on pr.Out would be a
			// second place for the two to disagree about which peers
			// are trusted.
			pr.SetXForwarded()
			// Issue #730. The engine logs every request under the id the
			// edge minted (webhost.RequestScope), so forwarding it here
			// is what makes the two containers of one deployment produce
			// lines that can be joined: without it the proxy's record of
			// a request and the engine's record of the same request name
			// two different ids and an operator has to guess.
			//
			// The clock the ErrorHandler below reports from starts in
			// that same middleware, which is why nothing here stashes a
			// start time anymore: the context value it would have
			// allocated per request - on a path that is overwhelmingly
			// successful - is one the correlation id already pays for.
			if id := webhost.CorrelationIDFrom(pr.In.Context()); id != "" {
				pr.Out.Header.Set(webhost.CorrelationHeader, id)
			}
		},
		Transport: &debugTransport{base: transport, logger: logger, debug: debug},
		// Issue #730: a UGREEN deployment where the browser gets
		// "TypeError: Failed to fetch" — no HTTP response at all — for
		// /api/v1/activity while curl to the same route answers 401.
		// Every way this proxy can fail to produce a response looks
		// exactly like that from JavaScript, and before this there was
		// no ErrorHandler, so net/http's default wrote a bare 502 to
		// its own logger and this process said nothing.
		//
		// This one ALWAYS logs, at Warn, whatever the log level: a
		// failure to answer the browser at all is not a debug-only
		// event, and an operator who has to turn a knob on before the
		// fault is recorded has already lost the occurrence that
		// prompted them. It fires for an unreachable engine and for
		// ResponseHeaderTimeout (above); a failure while streaming a
		// body the upstream already started never reaches here, by
		// httputil's design, since the status line is long gone.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("correlation_id", webhost.CorrelationIDFrom(r.Context())),
				slog.String("client_attempt_id", webhost.ClientAttemptIDOf(r)),
				slog.String("error", err.Error()),
			}
			// Appended only when the clock actually ran. Reporting zero
			// for "nobody timed this" would claim an instant failure for
			// exactly the request whose duration is the interesting part
			// - "the engine refused instantly" and "we sat on
			// ResponseHeaderTimeout for five seconds" are the two
			// answers this line exists to tell apart.
			if elapsed, ok := webhost.RequestElapsed(r.Context()); ok {
				attrs = append(attrs, slog.Int64("elapsed_ms", elapsed.Milliseconds()))
			}
			logger.Event(r.Context(), slog.LevelWarn, "proxy_error", "reverse proxy could not answer", attrs...)
			// The status net/http's own default ErrorHandler writes,
			// unchanged: this exists to record the failure, not to
			// change what a client sees.
			w.WriteHeader(http.StatusBadGateway)
		},
		// The engine sets the same browser response headers this
		// container does, so without this an /api/v1 response would carry
		// each of them twice once ReverseProxy has ADDED the upstream's
		// copy to the one already set here. Deleting the upstream's copy
		// makes this container the single authority for what a browser
		// is told, which is also the honest arrangement: it is the one
		// talking to the browser.
		ModifyResponse: func(res *http.Response) error {
			for _, h := range []string{"X-Frame-Options", "X-Content-Type-Options", "Referrer-Policy", "Content-Security-Policy"} {
				res.Header.Del(h)
			}
			// Same rule, same reason, for the correlation id: this
			// container already set it (webhost.RequestScope, below) and
			// the engine echoed the value it was forwarded, so keeping
			// the upstream's copy would mean two identical
			// X-Correlation-Id headers on one response - ReverseProxy
			// ADDS the upstream's headers to the ones already set here.
			// One authority for what the browser is told.
			res.Header.Del(webhost.CorrelationHeader)
			return nil
		},
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
	// StripUntrustedIdentity runs before the proxy, so the identity
	// header is gone from r.Header before the proxy's Rewrite ever
	// copies headers into the outbound request. It runs per request, so
	// a pipelined follow-on is scrubbed exactly like the request in
	// front of it.
	//
	// webhost.RequestScope is outside it, and is the only thing that
	// legitimately sits there: it mints this request's correlation id
	// and starts its clock (issue #730), which every layer below then
	// reads - the proxy's forwarded header, the ErrorHandler's
	// elapsed_ms, the upstream trace - and which the response carries
	// back whether the request was proxied, served from the static
	// bundle, or refused. It touches no header the strip cares about.
	return webhost.RequestScope(
		StripUntrustedIdentity(cfg.Gateway)(
			SecurityHeaders(
				local.EnsureCSRFCookie(false)(mux))))
}

// debugTransport is the second half of issue #730's instrumentation: the
// per-request record of what the engine actually answered, which is the
// only place a response the BROWSER rejected can still be described.
// Content-Encoding and Transfer-Encoding are in the attribute list for
// exactly that reason — a body whose declared encoding does not match
// its bytes, or a length that disagrees with what is sent, is refused by
// the browser's HTTP stack before any of it reaches fetch(), producing
// precisely the reported "TypeError: Failed to fetch" against a route
// curl reads without complaint.
//
// It emits TWO events per traced request, and the split is the point.
// proxy_upstream_headers is the status line and the framing the engine
// DECLARED; proxy_upstream_complete is what the body turned out to be
// once it had all been copied to the browser. A header-phase line on its
// own cannot see the failure #730 looks like from JavaScript at all: a
// Content-Length of 4 kB followed by 900 bytes and a dropped connection
// produces a perfectly ordinary-looking first line, a browser that
// refuses the response outright, and — before the completion event — no
// record anywhere in this process that anything went wrong.
//
// Off by default and free when off: with debug false this is one
// interface call straight through to the cloned *http.Transport, with no
// clock read, no attribute slice built and no wrapper around the body.
type debugTransport struct {
	base   http.RoundTripper
	logger webhost.Logger
	debug  bool
}

func (d *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !d.debug {
		return d.base.RoundTrip(req)
	}

	start := time.Now()
	res, err := d.base.RoundTrip(req)
	elapsed := time.Since(start).Milliseconds()

	// Both events name them, so a trace of one request can be picked out
	// of a log interleaving several, and so the proxy's two lines and
	// the engine's own line for the same request all join up.
	ids := []slog.Attr{
		slog.String("correlation_id", webhost.CorrelationIDFrom(req.Context())),
		slog.String("client_attempt_id", webhost.ClientAttemptIDOf(req)),
	}

	if err != nil {
		// The ErrorHandler logs this one too, from one layer out. Two
		// lines for one failure is the right trade for a diagnostic
		// build: this one is the only place that can say the failure
		// was in the transport rather than in ModifyResponse.
		d.logger.Event(req.Context(), slog.LevelWarn, "proxy_error", "upstream round trip failed",
			append(ids,
				slog.String("method", req.Method),
				slog.String("path", req.URL.Path),
				slog.Int64("elapsed_ms", elapsed),
				slog.String("error", err.Error()))...)
		return res, err
	}

	d.logger.Event(req.Context(), slog.LevelDebug, "proxy_upstream_headers", "upstream response headers received",
		append(ids,
			slog.String("method", req.Method),
			slog.String("path", req.URL.Path),
			slog.Int("status", res.StatusCode),
			slog.Int64("content_length", res.ContentLength),
			slog.String("content_encoding", res.Header.Get("Content-Encoding")),
			slog.String("transfer_encoding", strings.Join(res.TransferEncoding, ",")),
			slog.Int64("elapsed_ms", elapsed))...)

	if res.Body != nil {
		ctx, method, path := req.Context(), req.Method, req.URL.Path
		declared := res.ContentLength
		res.Body = &observedBody{ReadCloser: res.Body, report: func(o *observedBody) {
			level := slog.LevelDebug
			// A body that ended in a read error, or short of what it
			// declared, is the reported fault itself rather than a
			// diagnostic detail - so it is reported at Warn even though
			// only a debug deployment gets this far.
			if o.readErr != "" || (declared >= 0 && o.read != declared) {
				level = slog.LevelWarn
			}
			d.logger.Event(ctx, level, "proxy_upstream_complete", "upstream response body finished",
				append(ids,
					slog.String("method", method),
					slog.String("path", path),
					slog.Int64("content_length", declared),
					slog.Int64("bytes_read", o.read),
					slog.Bool("eof", o.eof),
					slog.String("read_error", o.readErr),
					slog.String("close_error", o.closeErr),
					slog.Int64("elapsed_ms", time.Since(start).Milliseconds()))...)
		}}
	}
	return res, nil
}

// observedBody is a transparent ReadCloser around the upstream body: it
// changes nothing about what the browser receives, and counts what went
// past.
//
// Transparency is the whole requirement. It buffers nothing (a copy of
// every proxied body is a memory cost proportional to what a deployment
// transfers), it alters no byte and no error, and it holds the reported
// facts rather than deciding what they mean - which is why report takes
// the observer itself instead of a widening argument list.
//
// Reported exactly once, from Close. httputil.ReverseProxy closes the
// body it was given on every path it can take, including the ones where
// copying to the client failed part way, so Close is the one moment that
// exists for both a clean transfer and a truncated one. The sync.Once is
// not decoration: a Close called twice must not double-report and make
// one request look like two.
type observedBody struct {
	io.ReadCloser

	read     int64
	eof      bool
	readErr  string
	closeErr string

	once   sync.Once
	report func(*observedBody)
}

func (o *observedBody) Read(p []byte) (int, error) {
	n, err := o.ReadCloser.Read(p)
	o.read += int64(n)
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		o.eof = true
	default:
		// First error only: an exhausted reader keeps returning the same
		// one, and the first is the one that describes what happened.
		if o.readErr == "" {
			o.readErr = err.Error()
		}
	}
	return n, err
}

func (o *observedBody) Close() error {
	err := o.ReadCloser.Close()
	if err != nil {
		o.closeErr = err.Error()
	}
	o.once.Do(func() { o.report(o) })
	return err
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
