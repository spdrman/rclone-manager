package local

import (
	"net/http"
	"strings"
)

// firstForwardedValue returns the first comma-separated entry of an
// X-Forwarded-* header value, trimmed of surrounding whitespace - the
// ORIGINAL client/hop's own value, since net/http/httputil.ReverseProxy
// (and any further proxy in a longer chain) appends its own hop rather
// than replacing what came before (see ProxyRequest.SetXForwarded's own
// doc). apps/generic/server.NewUI's reverse proxy is deliberately built
// to guarantee there is only ever one hop's worth of value here in this
// project's own shipped topology (it deletes any X-Forwarded-For a client
// sent it before recomputing its own), but this still reads the FIRST
// entry rather than assuming there is exactly one, since that is the
// correct interpretation of the header either way.
func firstForwardedValue(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// requestIsSecure reports whether r should be treated as having arrived
// over TLS for a Secure cookie flag's purposes: either it genuinely did
// (r.TLS != nil), or trustForwarded is enabled for this Service
// (Config.TrustForwardedHeaders, service.go) and the one verified,
// network-isolated peer that terminates/observes the real connection said
// so via X-Forwarded-Proto.
//
// # Why this is safe only when trustForwarded is true
//
// X-Forwarded-Proto is an ordinary request header: anyone who can reach
// this handler directly can set it to whatever they like. Trusting it
// unconditionally would let a plain HTTP client claim to be HTTPS and get
// a Secure cookie issued over an insecure connection - not a takeover on
// its own, but a false claim of security. It is only safe to trust here
// because, in the ONE deployment shape that ever sets
// Config.TrustForwardedHeaders (container/compose.yaml's two-container
// split), network isolation guarantees the ONLY thing that can ever be
// directly connected to this listener is apps/generic/server.NewUI's own
// reverse proxy - which the same compose file gives no other peer any way
// to route around - and that proxy always sets this header itself,
// derived from ITS OWN real connection to the actual browser, never
// copied from anything the browser sent. See ratelimit.go's remoteIP for
// the identical reasoning applied to X-Forwarded-For/rate-limiting.
func requestIsSecure(r *http.Request, trustForwarded bool) bool {
	if r.TLS != nil {
		return true
	}
	if !trustForwarded {
		return false
	}
	proto := firstForwardedValue(r.Header.Get("X-Forwarded-Proto"))
	return strings.EqualFold(proto, "https")
}
