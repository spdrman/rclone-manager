package local

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SessionCookieName is the HTTP-only session cookie this package issues
// on a successful login or enrollment, and reads on every subsequent
// authenticated request (via sessionAuthenticator, authenticator.go).
const SessionCookieName = "bm_session"

// sessionTTL is a fixed lifetime from creation, not a sliding one: simple
// to reason about, and 24h is generous for a single-administrator admin
// console that is not the kind of surface an operator expects to stay
// signed into indefinitely across many days.
const sessionTTL = 24 * time.Hour

type sessionRecord struct {
	username  string
	expiresAt time.Time
}

// sessionManager is a process-local, in-memory session store: a process
// restart signs every session out, which is an accepted trade-off (this
// package's own doc explains why) rather than an oversight. Every method
// is safe for concurrent use.
type sessionManager struct {
	mu    sync.Mutex
	byTok map[string]sessionRecord
	now   func() time.Time
}

func newSessionManager(now func() time.Time) *sessionManager {
	return &sessionManager{byTok: map[string]sessionRecord{}, now: now}
}

// create issues a brand new session for username and returns its opaque
// token and expiry.
func (m *sessionManager) create(username string) (token string, expiresAt time.Time, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("local: generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	expiresAt = m.now().Add(sessionTTL)

	m.mu.Lock()
	m.byTok[token] = sessionRecord{username: username, expiresAt: expiresAt}
	m.mu.Unlock()
	return token, expiresAt, nil
}

// lookup returns the username a live token belongs to, and whether it is
// currently valid. An expired entry is evicted, not merely reported as
// invalid, so this map cannot grow without bound across a long-running
// process.
func (m *sessionManager) lookup(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.byTok[token]
	if !ok {
		return "", false
	}
	if m.now().After(rec.expiresAt) {
		delete(m.byTok, token)
		return "", false
	}
	return rec.username, true
}

// revoke invalidates token immediately, regardless of its expiry.
func (m *sessionManager) revoke(token string) {
	m.mu.Lock()
	delete(m.byTok, token)
	m.mu.Unlock()
}

// revokeAll invalidates every live session immediately, regardless of
// expiry - password rotation's session-invalidation guarantee
// (handler.go's handleRotatePassword): a stolen or forgotten-open session
// must not survive a password change just because nobody explicitly
// logged it out. Every session in this package belongs to the one
// administrator this package supports (§13.4), so there is no per-user
// distinction to make here.
func (m *sessionManager) revokeAll() {
	m.mu.Lock()
	m.byTok = map[string]sessionRecord{}
	m.mu.Unlock()
}

// tokenFromRequest reads the session cookie from a real *http.Request
// (the HTTP handlers in handler.go use this); sessionAuthenticator
// (authenticator.go) reads the same cookie out of a raw header string
// instead, since capabilities.AuthRequest carries only headers, not a
// *http.Request.
func tokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// setSessionCookie writes token as this package's session cookie:
// HttpOnly (§3.6 - never readable by page JavaScript, so an XSS bug
// cannot exfiltrate it) and SameSite=Strict (never sent on a cross-site
// request, the first line of defense against CSRF alongside the
// double-submit check in csrf.go). Secure is decided by requestIsSecure
// (forwarded.go): true when the request itself arrived over TLS, or
// (only when trustForwarded is set - see Config.TrustForwardedHeaders)
// when the one verified reverse-proxy hop in front of this Service says
// it did via X-Forwarded-Proto.
//
// # Why this can't just be r.TLS != nil
//
// apps/generic's two-container split means this Service's own handler
// (the engine, `serve`) is, in production, never reached directly by a
// browser - only by apps/generic/server.NewUI's reverse proxy
// (`serve-ui`), over a plain HTTP internal Docker network connection.
// r.TLS is therefore permanently nil here regardless of whether an
// operator put real TLS in front of `serve-ui`'s own published port: a
// bare r.TLS != nil check would silently make Secure impossible to ever
// set to true in the shipped topology, defeating a deployment that did
// everything right on its own end. docs/deployment.md does not mandate
// TLS termination at all (many LAN deployments run plain HTTP
// throughout), so Secure still correctly stays false in that case - this
// is about not FALSELY forcing it false when TLS genuinely is in front of
// the deployment, just not visible to this exact process.
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time, trustForwarded bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   requestIsSecure(r, trustForwarded),
		SameSite: http.SameSiteStrictMode,
	})
}

// clearSessionCookie expires the session cookie immediately, for logout.
// See setSessionCookie's own doc for why Secure isn't simply r.TLS != nil.
func clearSessionCookie(w http.ResponseWriter, r *http.Request, trustForwarded bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsSecure(r, trustForwarded),
		SameSite: http.SameSiteStrictMode,
	})
}
