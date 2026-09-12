// Package csrf implements the double-submit-cookie CSRF pattern shared by
// every mutating HTTP surface this repository serves
// (docs/EPIC-B-multi-nas.md §3.6/§13A's "CSRF protection"): the cookie is
// set by EnsureCookie on any response that doesn't already carry one, and
// a legitimate same-origin client (ui/shared/src/api/client.ts) reads its
// value and echoes it back as HeaderName on every state-changing request;
// Verify then checks the two match, in constant time. A cross-site
// attacker can make a victim's browser SEND the cookie automatically, but
// cannot READ it (browsers enforce same-origin for both document.cookie
// and fetch/XHR response/cookie access), so it cannot construct a
// matching header value - the entire defense.
//
// This package exists so apps/common/auth/local (which owns login/enroll/
// logout) and apps/common/webhost (whose own mutating routes, e.g.
// POST /api/v1/operations, are reachable regardless of which auth backend
// a provider ultimately wires up) verify the exact same thing rather than
// keeping two independent copies of the same check to drift apart -
// issue #119's review flagged that, before this package existed, "there
// is currently no shared CSRF primitive webhost or a future
// non-local-auth backend could reach for" and webhost's own mutating
// route had no CSRF check at all. Neither package depends on the other
// for this: both depend on this one instead.
package csrf

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
)

// CookieName and HeaderName name the double-submit cookie/header pair
// every consumer of this package uses. Not HttpOnly (see EnsureCookie):
// the whole pattern requires client-side JavaScript to read the cookie
// so it can echo it back as HeaderName.
const (
	CookieName = "backupd_csrf"
	HeaderName = "X-CSRF-Token"
)

// LegacyCookieName is the name CookieName had before the project was
// renamed to backupd (#794), kept readable for one release.
//
// Unlike the session cookie, where the compat window only spares a
// credential, here it is load bearing for a live page. The client half
// of this pattern is JavaScript that READS the cookie by name
// (ui/shared/src/api/client.ts), so an upgrade is guaranteed to have
// already-loaded and cached bundles in the field echoing whatever value
// they found under the old name. Issuing a fresh token under the new
// name only, and comparing against that, would reject every one of
// those requests with 403 CSRF_TOKEN_MISMATCH until each browser
// happened to reload - a rename presenting as the exact failure this
// package exists to produce for an attack.
//
// So EnsureCookie carries an existing old-name token FORWARD onto the
// new name rather than minting a second, different one (below), and
// Verify accepts either name. Both halves then see the same value under
// the name each knows, and the old name leaves the wire on its own as
// each client's session cookie jar turns over.
const LegacyCookieName = "bm_csrf"

// cookieNames are the names a read accepts, in precedence order: the
// current name wins whenever it carries a value. Package-level so a read
// does not allocate to iterate it.
var cookieNames = []string{CookieName, LegacyCookieName}

// readToken returns the double-submit token r carries under any accepted
// name, or "" for none. An empty value counts as absent so a cleared
// cookie cannot shadow a name further down the list.
func readToken(r *http.Request) string {
	for _, name := range cookieNames {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// ErrMissingCookie means the request carried no CSRF cookie at all (or an
// empty one) - most commonly a client that never loaded a page from this
// origin first, so EnsureCookie never had a chance to issue one.
var ErrMissingCookie = errors.New("csrf: missing cookie")

// ErrHeaderMismatch means a CSRF cookie was present but HeaderName either
// was not sent or did not match it (byte-for-byte, in constant time).
var ErrHeaderMismatch = errors.New("csrf: missing or mismatched header")

// EnsureCookie issues a CSRF cookie for any request that doesn't already
// carry one. It has to run in front of EVERY response an HTTP surface
// serves, not just its own mutating routes, so a legitimate client always
// has a token to read and echo back before it ever needs one: the very
// first page load is what has to set this cookie, since the first
// state-changing request a fresh browser session makes is what will need
// to echo it.
//
// "Already carries one" spans both accepted names for the compat window
// (LegacyCookieName), and a request that carries only the old name has
// that exact value re-issued under the current one instead of a fresh
// token. Minting a new value there would leave the two names holding two
// different tokens, and a cached client still echoing the old name's
// value would then fail Verify - which prefers the current name - on
// every mutating request. Carrying the value forward makes both halves
// agree no matter which name either side reads.
//
// secure decides the issued cookie's own Secure flag, given the request
// that triggered issuance: a plain `func(r *http.Request) bool { return
// r.TLS != nil }` for a handler that terminates TLS itself (or never
// does), or something that additionally trusts a forwarded-proto header
// from one specific, verified reverse-proxy hop for a handler that
// doesn't (apps/common/auth/local.Config.TrustForwardedHeaders is exactly
// that case) - this package deliberately knows nothing about proxies
// itself, only that Secure-ness is the caller's own call to make.
func EnsureCookie(secure func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if existing := readToken(r); existing == "" {
				if fresh, genErr := randomToken(32); genErr == nil {
					setCookie(w, r, fresh, secure)
				}
			} else if _, err := r.Cookie(CookieName); err != nil {
				setCookie(w, r, existing, secure)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Verify reports whether r carries a valid double-submit CSRF token: its
// HeaderName header matches its CSRF cookie, byte-for-byte, in constant
// time. Either accepted cookie name counts (CookieName first, then
// LegacyCookieName - see that constant for the window). A non-nil return
// is always ErrMissingCookie or ErrHeaderMismatch (check with
// errors.Is), letting each caller choose its own error response shape/
// code for the two cases - apps/common/auth/local and
// apps/common/webhost each have their own, incompatible error body
// conventions, and this package doesn't referee between them.
func Verify(r *http.Request) error {
	cookie := readToken(r)
	if cookie == "" {
		return ErrMissingCookie
	}
	header := r.Header.Get(HeaderName)
	if header == "" || subtle.ConstantTimeCompare([]byte(header), []byte(cookie)) != 1 {
		return ErrHeaderMismatch
	}
	return nil
}

// setCookie writes value under the current CookieName. Not HttpOnly: the
// double-submit pattern requires the page's own JavaScript to read it.
func setCookie(w http.ResponseWriter, r *http.Request, value string, secure func(*http.Request) bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		Secure:   secure(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// randomToken returns n bytes of crypto/rand, base64url encoded. Callers
// pass 32, which is well past what an attacker could reach by guessing;
// the parameter exists so the size is visible at the call site rather than
// buried here.
//
// The error is returned rather than panicked on, and EnsureCookie's caller
// simply issues no cookie when it fires. A process whose randomness source
// has failed cannot issue a safe token at all, and serving a predictable
// one would be worse than serving none: without a cookie the check refuses
// every request, which is the direction to fail in.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
