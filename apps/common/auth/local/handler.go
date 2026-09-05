// The five HTTP routes this package serves, and the two conventions that
// hold them together.
//
// The first convention is that a refusal never says more than it has to.
// Login answers a wrong username, a wrong password and an entirely
// unenrolled deployment with the same code and the same message, and pays
// Argon2id's cost on every one of those branches against a fixed dummy
// hash, so that neither the body nor the clock distinguishes them. The
// exception is enrollment, where ENROLLMENT_CLOSED and
// BOOTSTRAP_TOKEN_INVALID are deliberately distinct: an operator who
// pasted a stale token needs to know which of those happened, and by that
// point the account exists, so there is nothing left to protect.
//
// The second is errorCodeStatus. Every code this file emits is registered
// there with the one status it is served at, and writeAuthError panics on
// a mismatch rather than serving it. That is a deliberately violent
// reaction to a small inconsistency, and it exists because the small
// inconsistency already happened: BOOTSTRAP_TOKEN_INVALID drifted to 403
// while the published contract still said 401 (#289), and nothing noticed
// until somebody read both files side by side.
package local

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
)

// BootstrapTokenHeader carries the single-use enrollment secret §49.1
// requires (bootstrap.go). ui/shared's enrollment page reads it from the
// URL the printed bootstrap notice gives the operator
// (?token=... on /enroll) and attaches it here, rather than adding a
// visible form field EnrollmentPage.tsx does not have: the design canvas
// (docs/design/Backup Manager.dc.html) does not show one either, and a
// URL-carried, one-time secret is the same shape 1Password/Grafana/etc.
// use for exactly this kind of first-run claim link.
const BootstrapTokenHeader = "X-Bootstrap-Token"

// minPasswordLength mirrors ui/shared/src/auth/EnrollmentPage.tsx's own
// MIN_LENGTH constant. Enforced here too (not just client-side) because a
// client-side check is a UX nicety, not a security boundary: nothing
// stops a direct POST /api/v1/auth/enroll from skipping the browser
// entirely.
const minPasswordLength = 12

// loginRequest/enrollRequest mirror ui/shared/src/api/client.ts's
// `login`/`enrollAdministrator` request bodies exactly
// ({username, password} JSON).
type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// rotatePasswordRequest mirrors ui/shared/src/api/client.ts's
// `rotatePassword` request body ({currentPassword, newPassword} JSON).
type rotatePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

type sessionResponse struct {
	Username string `json:"username"`
}

// authErrorResponse mirrors ui/shared/src/api/contracts.ts's ApiError
// interface field-for-field (code/message/correlationId, all top-level -
// NOT nested under an "error" key the way apps/common/webhost's own
// errors.go shapes its responses; that package's routes are not the ones
// ui/shared/src/api/client.ts calls yet, and this package's routes ARE,
// so this matches the consumer that actually exists).
type authErrorResponse struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlationId"`
}

// errorCodeStatus binds each of this package's own error codes to the one
// HTTP status it is always served at. writeAuthError below checks every
// call against it, so a call site that passes a status other than the one
// this package has already committed to for that code fails immediately
// (in whichever test exercises that call site) instead of shipping
// quietly - which is what let BOOTSTRAP_TOKEN_INVALID drift to 403 while
// api/v1/openapi.json still declared it a 401 (#289). contract_test.go
// checks this same map against the contract itself, so the two
// comparisons together cover both edges: handler-vs-registry here,
// registry-vs-contract there.
var errorCodeStatus = map[string]int{
	"UNAUTHENTICATED":         http.StatusUnauthorized,
	"BOOTSTRAP_TOKEN_INVALID": http.StatusUnauthorized,
	"ENROLLMENT_CLOSED":       http.StatusForbidden,
	"CSRF_TOKEN_MISSING":      http.StatusForbidden,
	"CSRF_TOKEN_MISMATCH":     http.StatusForbidden,
	"INVALID_REQUEST":         http.StatusBadRequest,
	"RATE_LIMITED":            http.StatusTooManyRequests,
	"INTERNAL_ERROR":          http.StatusInternalServerError,
}

// writeAuthError serves one refusal, and panics if the caller asked for a
// status this package has not already committed to for that code.
//
// A panic is a harsh answer to what looks like a small inconsistency, and
// it is chosen over a log line or a silent correction because the small
// inconsistency is exactly what shipped last time: the status for one code
// drifted away from the published contract and nothing noticed for a
// release. A panic fires in whichever test exercises the call site, which
// is before anybody deploys it; a corrected status would hide the drift,
// and a log line would be read by nobody.
//
// The correlation ID goes into both the header and the body on purpose.
// ui/shared reads the header, and an operator quoting an error reads the
// body, and having the same value in both is what lets somebody match a
// screenshot to a log.
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	if want, ok := errorCodeStatus[code]; ok && want != status {
		panic(fmt.Sprintf("local: writeAuthError: %q is registered at status %d, called with %d", code, want, status))
	}
	cid := correlationID()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// ui/shared/src/api/client.ts reads this off every non-2xx response's
	// X-Correlation-Id header (falling back to "unavailable" if absent -
	// which, before this, it always was for this package's own routes,
	// even though the same value was already in the JSON body below).
	w.Header().Set("X-Correlation-Id", cid)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(authErrorResponse{
		Code:          code,
		Message:       message,
		CorrelationID: cid,
	})
}

// correlationID is a short, opaque, per-response identifier an operator
// could quote when asking for help; it carries no session or credential
// material.
func correlationID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "cid_unavailable"
	}
	return "cid_" + base64.RawURLEncoding.EncodeToString(b)
}

// Handler returns the http.Handler serving this package's five routes,
// relative to whatever prefix the caller mounts it at (apps/generic
// mounts it at /api/v1/auth, matching ui/shared's own expectation):
//
//	POST /login    - {username, password} -> 204 + session cookie
//	POST /enroll   - {username, password} + X-Bootstrap-Token -> 204 + session cookie
//	POST /password - {currentPassword, newPassword}, session required -> 204 + fresh session cookie
//	POST /logout   - -> 204, session cookie cleared
//	GET  /session  - -> 200 {username} if authenticated, 401 otherwise
//
// login/enroll/logout/password are wrapped in requireCSRF (they mutate
// server-side state - a new or cleared session, or the persisted password
// hash - so they need the same protection as any other state-changing
// route); login/enroll/password are also rate-limited by remote IP. GET
// /session deliberately has neither: it mutates nothing, and gating a
// read with CSRF or a login-attempt budget would only make the page's
// own "am I signed in" check flaky for no security benefit.
//
// # This handler alone is NOT self-sufficient - callers MUST also wrap
//
// requireCSRF checks a cookie that has to already exist before it can
// ever match anything, and this Handler never issues that cookie itself:
// a caller MUST additionally apply EnsureCSRFCookie to its ENTIRE
// top-level handler, not just the http.Handler this method returns, or
// every single login/enroll/logout attempt will unconditionally 403 with
// CSRF_TOKEN_MISSING, since the very first page a fresh browser session
// loads is what has to seed the cookie before any POST here can ever echo
// it back. This is easy to miss (nothing here fails to compile if you
// don't), and it's the first thing every future consumer of this package
// - the reusable auth for every provider besides UGOS - needs to get
// right that a type signature alone can't enforce. apps/common/webhost/serve's
// NewEngine/NewUI (both wrap their ENTIRE composed mux, auth routes
// included) are the reference example to copy, not routes-only wrapping.
func (s *Service) Handler() http.Handler {
	r := chi.NewRouter()
	r.With(requireCSRF).Post("/login", s.handleLogin)
	r.With(requireCSRF).Post("/enroll", s.handleEnroll)
	r.With(requireCSRF).Post("/password", s.handleRotatePassword)
	r.With(requireCSRF).Post("/logout", s.handleLogout)
	r.Get("/session", s.handleSession)
	return r
}

// handleLogin implements POST /login. Every failure path answers
// identically, and every failure path pays the same Argon2id cost, so
// neither the response nor its timing tells a caller whether they guessed
// the username right. The two dummy-hash calls below are what buy the
// second half of that, and they are not optional: without them, a wrong
// username returns in microseconds while a wrong password for the right
// username takes the full hash, which hands over the enrolled username one
// guess at a time.
func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimiter.Allow(remoteIP(r, s.trustForwardedHeaders)) {
		writeAuthError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many login attempts; wait before trying again")
		return
	}

	var req credentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed request body")
		return
	}

	admin, err := s.store.Admin()
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	if admin == nil {
		// No administrator exists yet: no credentials could possibly be
		// correct. Reported the same way as a wrong password (below)
		// rather than a distinct code, since there is only ever one
		// possible username in this model and nothing is gained by
		// letting a caller distinguish "no account yet" from "wrong
		// password" - both mean "you cannot sign in right now." Still
		// runs the same dummy Argon2id comparison the other no-match
		// branch below does, for the same timing reason.
		_ = verifyPassword(dummyPasswordHash(), req.Password)
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "that username and password combination was not accepted")
		return
	}

	if req.Username != admin.Username {
		// Deliberately still pays Argon2id's own cost here, against a
		// fixed dummy hash that has no real corresponding password,
		// instead of returning immediately: without this, a wrong
		// username fails fast while a wrong password for the RIGHT
		// username fails slow (verifyPassword below actually runs),
		// letting a timing-only observer discover the enrolled username
		// one guess at a time without ever needing a correct password.
		_ = verifyPassword(dummyPasswordHash(), req.Password)
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "that username and password combination was not accepted")
		return
	}
	if err := verifyPassword(admin.PasswordHash, req.Password); err != nil {
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "that username and password combination was not accepted")
		return
	}

	token, expiresAt, err := s.sessions.create(admin.Username)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	setSessionCookie(w, r, token, expiresAt, s.trustForwardedHeaders)
	w.WriteHeader(http.StatusNoContent)
}

// dummyPasswordHash is a fixed, valid-format Argon2id hash with no real
// corresponding password, computed once on first use. handleLogin's
// no-match branches (no administrator yet, or a submitted username that
// doesn't match the enrolled one) verify the submitted password against
// THIS instead of skipping password verification entirely, so a caller
// cannot use response timing to learn whether a candidate username is the
// enrolled administrator before a real password hash is ever compared at
// all - login has to take the same Argon2id-shaped time on every branch.
var dummyPasswordHash = sync.OnceValue(func() string {
	hash, err := hashPassword("not-a-real-password-only-ever-used-to-burn-cpu-time")
	if err != nil {
		// hashPassword only fails if crypto/rand.Read fails, which would
		// mean this process cannot generate secure randomness at all -
		// already fatal for every other credential this package issues
		// (sessions, bootstrap tokens, CSRF tokens). There is no
		// meaningful fallback to degrade to.
		panic(fmt.Sprintf("local: precompute dummy password hash: %v", err))
	}
	return hash
})

// handleEnroll implements POST /enroll, the one route that can create the
// administrator account.
//
// The order of its checks is the interesting part. Whether an
// administrator already exists is decided before the bootstrap token is
// even looked at, so a stale token can never produce a refusal that hints
// it would have worked; and the store's own Enroll guard is checked again
// afterwards, because the read above and the write below are not atomic
// and a concurrent enrollment can land in between.
func (s *Service) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.enrollLimiter.Allow(remoteIP(r, s.trustForwardedHeaders)) {
		writeAuthError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many enrollment attempts; wait before trying again")
		return
	}

	existing, err := s.store.Admin()
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	if existing != nil {
		// §49.1: enrollment is single-shot and irreversible. This is
		// checked before the bootstrap token even, so a stale/reused
		// token can never look like it "would have worked" once an
		// administrator already exists.
		writeAuthError(w, http.StatusForbidden, "ENROLLMENT_CLOSED", "an administrator account already exists")
		return
	}

	if !s.bootstrap.consume(r.Header.Get(BootstrapTokenHeader)) {
		// 401, not 403: this is a credential the caller presented that
		// wasn't accepted (the bootstrap token), same class of refusal as
		// UNAUTHENTICATED, which is exactly where api/v1/openapi.json's
		// own x-error-classes files it (issue #289). ENROLLMENT_CLOSED
		// above stays a 403 - that one really is "you are who you say,
		// but this route isn't open to you," a genuine authorization
		// refusal rather than a rejected credential.
		writeAuthError(w, http.StatusUnauthorized, "BOOTSTRAP_TOKEN_INVALID", "missing, expired or already-used bootstrap token")
		return
	}

	var req credentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed request body")
		return
	}
	if req.Username == "" {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "username is required")
		return
	}
	if len(req.Password) < minPasswordLength {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "password must be at least 12 characters")
		return
	}

	hash, err := hashPassword(req.Password)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}

	err = s.store.Enroll(AdminRecord{
		Username:     req.Username,
		PasswordHash: hash,
		CreatedAt:    s.now().UTC(),
	})
	if errors.Is(err, ErrAlreadyEnrolled) {
		// Lost a race against a concurrent enrollment between the Admin()
		// check above and this call - still correctly refused, just via
		// the store's own guard rather than this handler's own read.
		writeAuthError(w, http.StatusForbidden, "ENROLLMENT_CLOSED", "an administrator account already exists")
		return
	}
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}

	token, expiresAt, err := s.sessions.create(req.Username)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	setSessionCookie(w, r, token, expiresAt, s.trustForwardedHeaders)
	w.WriteHeader(http.StatusNoContent)
}

// handleRotatePassword implements POST /password. It requires an already
// authenticated session (unlike login/enroll, which have to work WITHOUT
// one) - the session cookie identifies who is rotating, and
// currentPassword is then checked against that same administrator's
// stored hash, the same "prove you know the secret, not just that you
// hold a cookie" shape login itself uses. A successful rotation revokes
// every OTHER live session and issues a fresh one for the request that
// performed it as a single atomic step (sessionManager.rotateSession), so
// a stolen or forgotten-open session elsewhere does not survive a
// password change, while the operator doing the rotation is not logged
// out by their own action - including by a second, racing rotation
// request of their own (double-click, duplicate tab, a retried POST).
func (s *Service) handleRotatePassword(w http.ResponseWriter, r *http.Request) {
	if !s.rotateLimiter.Allow(remoteIP(r, s.trustForwardedHeaders)) {
		writeAuthError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many password change attempts; wait before trying again")
		return
	}

	username, ok := s.sessions.lookup(tokenFromRequest(r))
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "no active session")
		return
	}

	var req rotatePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed request body")
		return
	}

	admin, err := s.store.Admin()
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	if admin == nil || admin.Username != username {
		// A live session implies an administrator exists and named this
		// session's own username at creation time (handleLogin/handleEnroll);
		// this branch guards against that invariant somehow not holding
		// rather than assuming it always will.
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "no active session")
		return
	}

	if err := verifyPassword(admin.PasswordHash, req.CurrentPassword); err != nil {
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "current password is incorrect")
		return
	}

	if len(req.NewPassword) < minPasswordLength {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "password must be at least 12 characters")
		return
	}

	hash, err := hashPassword(req.NewPassword)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	if err := s.store.SetPassword(hash); err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}

	token, expiresAt, err := s.sessions.rotateSession(admin.Username)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	setSessionCookie(w, r, token, expiresAt, s.trustForwardedHeaders)
	w.WriteHeader(http.StatusNoContent)
}

// handleLogout implements POST /logout, and always succeeds. A caller with
// no session, an expired one or a token this process has never heard of
// gets the same 204 and the same cleared cookie, because there is nothing
// to protect here and every distinguishable answer would only tell an
// unauthenticated caller something about which tokens are live.
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := tokenFromRequest(r); token != "" {
		s.sessions.revoke(token)
	}
	clearSessionCookie(w, r, s.trustForwardedHeaders)
	w.WriteHeader(http.StatusNoContent)
}

// handleSession implements GET /session, which is what the UI calls on
// load to find out whether to render the app or the login page. It is the
// one route here with neither a CSRF check nor a rate limit: it changes
// nothing, and gating it would make that first render flaky in exchange
// for protecting a read that reveals only what the caller's own cookie
// already proves.
func (s *Service) handleSession(w http.ResponseWriter, r *http.Request) {
	username, ok := s.sessions.lookup(tokenFromRequest(r))
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "no active session")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(sessionResponse{Username: username})
}
