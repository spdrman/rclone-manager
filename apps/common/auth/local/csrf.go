// This package's side of the shared CSRF primitive.
//
// The actual double-submit implementation lives in apps/common/csrf,
// because apps/common/webhost needs the identical check for its own
// mutating routes and two copies of a security check are two copies that
// drift. What is left here is the part that is genuinely this package's:
// the middleware wiring, and the error bodies.
//
// The error bodies are why this is not just an alias. requireCSRF turns
// csrf's two sentinel errors into this package's own JSON error shape and
// its own codes, which are not webhost's; ui/shared reads these, and the
// two packages settled on incompatible envelopes long before this one
// existed. csrf.Verify deliberately refuses to referee that, so each side
// translates.
package local

import (
	"errors"
	"net/http"

	"github.com/spdrman/rclone-manager/apps/common/csrf"
)

// CSRFCookieName and CSRFHeaderName implement the double-submit cookie
// CSRF pattern (§3.6/§13A's "CSRF protection"): the cookie is set by
// EnsureCSRFCookie on any response that doesn't already carry one, and a
// legitimate same-origin client (ui/shared/src/api/client.ts) reads its
// value and echoes it back as CSRFHeaderName on every state-changing
// request. requireCSRF then checks the two match. A cross-site attacker
// can make a victim's browser SEND the cookie automatically, but cannot
// READ it (browsers enforce same-origin for both document.cookie and
// fetch/XHR response/cookie access), so it cannot construct a matching
// header value - which is the entire defense.
//
// Both are re-exported from apps/common/csrf, the actual implementation
// this package's own routes AND apps/common/webhost's mutating routes
// (POST /api/v1/operations) share - see that package's own doc for why.
const (
	CSRFCookieName = csrf.CookieName
	CSRFHeaderName = csrf.HeaderName
)

// EnsureCSRFCookie issues a CSRF cookie for any request that doesn't
// already carry one. It has to run in front of EVERY response this
// process serves, not just this package's own routes - including the
// static UI and apps/common/webhost's /api/v1 group - so a legitimate
// client always has a token to read and echo back before it ever needs
// one: the very first page load is what has to set this cookie, since
// the login/enroll POST that follows is the first state-changing request
// a fresh browser session makes.
//
// trustForwardedProto controls whether the issued cookie's Secure flag
// additionally trusts an X-Forwarded-Proto header when the request itself
// didn't arrive over TLS - see requestIsSecure (forwarded.go) and
// Config.TrustForwardedHeaders (service.go) for exactly when that is
// safe. Pass false from any handler chain that talks to arbitrary/
// untrusted clients directly (apps/common/webhost/serve.NewUI, the
// browser-facing container, which already observes its own real TLS
// status via r.TLS and must never trust a forwarded header from just
// anyone hitting its published port) - only apps/common/webhost/serve.NewEngine
// ever has a reason to pass true, and only when its own Auth Service was
// itself configured to trust its one verified reverse-proxy peer.
func EnsureCSRFCookie(trustForwardedProto bool) func(http.Handler) http.Handler {
	return csrf.EnsureCookie(func(r *http.Request) bool {
		return requestIsSecure(r, trustForwardedProto)
	})
}

// requireCSRF refuses a state-changing request unless its CSRFHeaderName
// header matches its own CSRFCookieName cookie, in constant time (see
// apps/common/csrf.Verify).
func requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := csrf.Verify(r)
		switch {
		case err == nil:
			next.ServeHTTP(w, r)
		case errors.Is(err, csrf.ErrMissingCookie):
			writeAuthError(w, http.StatusForbidden, "CSRF_TOKEN_MISSING", "missing CSRF cookie; reload the page and try again")
		default:
			writeAuthError(w, http.StatusForbidden, "CSRF_TOKEN_MISMATCH", "missing or mismatched "+CSRFHeaderName+" header")
		}
	})
}
