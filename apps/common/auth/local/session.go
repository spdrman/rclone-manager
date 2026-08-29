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
// double-submit check in csrf.go). Secure is set only when the request
// itself arrived over TLS (r.TLS != nil): docs/deployment.md does not
// mandate TLS termination in front of this container (many LAN
// deployments run plain HTTP behind their own reverse proxy, or none at
// all), and an unconditional Secure flag would make the cookie silently
// never take effect - and login therefore silently never work - on any
// deployment that hasn't put TLS in front of it. A deployment that
// terminates TLS gets Secure automatically; one that doesn't still gets a
// working, HttpOnly, SameSite=Strict cookie rather than none at all.
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearSessionCookie expires the session cookie immediately, for logout.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}
