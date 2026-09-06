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
	CookieName = "bm_csrf"
	HeaderName = "X-CSRF-Token"
)

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
			if _, err := r.Cookie(CookieName); err != nil {
				if token, genErr := randomToken(32); genErr == nil {
					http.SetCookie(w, &http.Cookie{
						Name:     CookieName,
						Value:    token,
						Path:     "/",
						Secure:   secure(r),
						SameSite: http.SameSiteStrictMode,
					})
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Verify reports whether r carries a valid double-submit CSRF token: its
// HeaderName header matches its CookieName cookie, byte-for-byte, in
// constant time. A non-nil return is always ErrMissingCookie or
// ErrHeaderMismatch (check with errors.Is), letting each caller choose
// its own error response shape/code for the two cases -
// apps/common/auth/local and apps/common/webhost each have their own,
// incompatible error body conventions, and this package doesn't referee
// between them.
func Verify(r *http.Request) error {
	cookie, err := r.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return ErrMissingCookie
	}
	header := r.Header.Get(HeaderName)
	if header == "" || subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) != 1 {
		return ErrHeaderMismatch
	}
	return nil
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
