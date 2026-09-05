package local

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Sessions: in memory, for the life of one process.
//
// A restart signs everybody out. That is a trade-off rather than a gap,
// and it is worth being explicit about which way it was made: persisting
// sessions would mean a second store to keep consistent with the first, a
// second thing to encrypt at rest, and a way for a stolen token to survive
// the restart an operator performed precisely because they suspected
// something. For one administrator on a single-process deployment, losing
// a session on restart costs one login.
//
// rotateSession is the one method here with a non-obvious shape, and its
// doc explains the race it exists to close. The short version is that
// "revoke everything" followed by "create one" is two locked steps, and
// two concurrent rotations can interleave them into a state with no live
// session at all, which locks the administrator out of their own account
// straight after a successful password change.
//
// The cookie helpers live here rather than in handler.go because Secure,
// HttpOnly and SameSite are session properties, and a route that sets the
// cookie without them would be a security bug that reads like a typo.

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

// generateSessionToken returns a fresh, cryptographically random opaque
// session token. Shared by create and rotateSession so both mint tokens
// the same way; neither holds m.mu while it runs, since it does not touch
// byTok at all.
func generateSessionToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("local: generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// create issues a brand new session for username and returns its opaque
// token and expiry.
func (m *sessionManager) create(username string) (token string, expiresAt time.Time, err error) {
	token, err = generateSessionToken()
	if err != nil {
		return "", time.Time{}, err
	}
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

// rotateSession atomically revokes every live session and installs a
// single new one for username, all under one m.mu acquisition - password
// rotation's session-invalidation guarantee (handler.go's
// handleRotatePassword): a stolen or forgotten-open session must not
// survive a password change just because nobody explicitly logged it
// out, while the operator performing the rotation must not be logged out
// by their own action. Every session in this package belongs to the one
// administrator this package supports (§13.4), so there is no per-user
// distinction to make here.
//
// This has to be one atomic step, not a revoke-everything call followed
// by a separate create() the way the two used to be composed: those are
// two independently-locked calls, so if two rotation requests from the
// same admin raced (a double-click before the button disables, two open
// tabs, a client retrying a timed-out POST), request B's revoke-all could
// fire after request A's create() had already installed A's new session,
// wiping out the very session A's own successful rotation just issued.
// Replacing the whole map in one locked step means the last rotateSession
// call to run always wins outright, and no interleaving of concurrent
// calls can ever leave zero live sessions.
func (m *sessionManager) rotateSession(username string) (token string, expiresAt time.Time, err error) {
	token, err = generateSessionToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt = m.now().Add(sessionTTL)

	m.mu.Lock()
	m.byTok = map[string]sessionRecord{token: {username: username, expiresAt: expiresAt}}
	m.mu.Unlock()
	return token, expiresAt, nil
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
// browser - only by apps/common/webhost/serve.NewUI's reverse proxy
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
