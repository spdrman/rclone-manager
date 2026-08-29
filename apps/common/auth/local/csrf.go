package local

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
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
const (
	CSRFCookieName = "bm_csrf"
	CSRFHeaderName = "X-CSRF-Token"
)

// EnsureCSRFCookie issues a CSRF cookie for any request that doesn't
// already carry one. It has to run in front of EVERY response this
// process serves, not just this package's own routes - including the
// static UI and apps/common/webhost's /api/v1 group - so a legitimate
// client always has a token to read and echo back before it ever needs
// one: the very first page load is what has to set this cookie, since
// the login/enroll POST that follows is the first state-changing request
// a fresh browser session makes.
func EnsureCSRFCookie(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie(CSRFCookieName); err != nil {
			if token, genErr := randomToken(32); genErr == nil {
				http.SetCookie(w, &http.Cookie{
					Name: CSRFCookieName,
					// Deliberately NOT HttpOnly: the double-submit pattern
					// requires client-side JavaScript to read this value
					// so it can echo it back as CSRFHeaderName. It carries
					// no authority on its own (unlike the session cookie),
					// only proof that whoever sent the header could also
					// read this origin's own cookies.
					Value:    token,
					Path:     "/",
					Secure:   r.TLS != nil,
					SameSite: http.SameSiteStrictMode,
				})
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireCSRF refuses a state-changing request unless its CSRFHeaderName
// header matches its own CSRFCookieName cookie, in constant time.
func requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(CSRFCookieName)
		if err != nil || cookie.Value == "" {
			writeAuthError(w, http.StatusForbidden, "CSRF_TOKEN_MISSING", "missing CSRF cookie; reload the page and try again")
			return
		}
		header := r.Header.Get(CSRFHeaderName)
		if header == "" || subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) != 1 {
			writeAuthError(w, http.StatusForbidden, "CSRF_TOKEN_MISMATCH", "missing or mismatched "+CSRFHeaderName+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
