package local

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// testServer wires a fresh Service's Handler behind EnsureCSRFCookie,
// exactly as apps/generic's own composed handler does, and returns an
// httptest.Server plus a *http.Client carrying a cookie jar (so the
// session and CSRF cookies a real browser would keep are kept here too).
func testServer(t *testing.T) (*Service, *httptest.Server, *http.Client) {
	t.Helper()
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(false)(mux))
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return svc, server, &http.Client{Jar: jar}
}

// seedCSRFCookie makes a harmless GET so the client's cookie jar picks up
// EnsureCSRFCookie's cookie, exactly as a real browser's first page load
// would before it ever POSTs anything.
func seedCSRFCookie(t *testing.T, client *http.Client, server *httptest.Server) {
	t.Helper()
	resp, err := client.Get(server.URL + "/api/v1/auth/session")
	if err != nil {
		t.Fatalf("seed GET: %v", err)
	}
	resp.Body.Close()
}

func csrfTokenFromJar(t *testing.T, client *http.Client, server *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == CSRFCookieName {
			return c.Value
		}
	}
	t.Fatal("no CSRF cookie present in jar; call seedCSRFCookie first")
	return ""
}

// bootstrapToken extracts the current single-use enrollment token via
// PrintBootstrapNotice, the same one-way surface a real operator (reading
// container logs) has - there is no test-only getter, deliberately,
// so this test exercises the exact same string a container's log would
// print.
func currentBootstrapToken(t *testing.T, svc *Service) string {
	t.Helper()
	var buf bytes.Buffer
	if err := svc.PrintBootstrapNotice(&buf, ""); err != nil {
		t.Fatalf("PrintBootstrapNotice: %v", err)
	}
	const marker = "token: "
	s := buf.String()
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("no bootstrap token found in notice: %q", s)
	}
	fields := strings.Fields(s[i+len(marker):])
	if len(fields) == 0 {
		t.Fatalf("could not parse token out of notice: %q", s)
	}
	return strings.TrimSuffix(fields[0], "\n")
}

func postJSON(t *testing.T, client *http.Client, url string, body any, headers map[string]string) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	return resp
}

func TestHandler_EnrollWithoutBootstrapTokenIsRefused(t *testing.T) {
	_, server, client := testServer(t)
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)

	resp := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		credentialsRequest{Username: "bm-admin", Password: "correct-horse-battery"},
		map[string]string{CSRFHeaderName: csrf})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (BOOTSTRAP_TOKEN_INVALID)", resp.StatusCode, http.StatusForbidden)
	}
}

func TestHandler_EnrollThenLoginThenSessionThenLogout(t *testing.T) {
	svc, server, client := testServer(t)
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	token := currentBootstrapToken(t, svc)

	enrollResp := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		credentialsRequest{Username: "bm-admin", Password: "correct-horse-battery"},
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	enrollResp.Body.Close()
	if enrollResp.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll status = %d, want %d", enrollResp.StatusCode, http.StatusNoContent)
	}

	// Enrollment establishes a session immediately (matching the frontend's
	// EnrollmentPage.tsx -> onEnrolled -> straight into the app flow):
	// GET /session must already report authenticated, no separate login
	// needed.
	sessionResp, err := client.Get(server.URL + "/api/v1/auth/session")
	if err != nil {
		t.Fatalf("GET session: %v", err)
	}
	var got sessionResponse
	if err := json.NewDecoder(sessionResp.Body).Decode(&got); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	sessionResp.Body.Close()
	if sessionResp.StatusCode != http.StatusOK || got.Username != "bm-admin" {
		t.Fatalf("GET session after enroll: status=%d body=%+v, want 200 {bm-admin}", sessionResp.StatusCode, got)
	}

	logoutResp := postJSON(t, client, server.URL+"/api/v1/auth/logout", struct{}{}, map[string]string{CSRFHeaderName: csrf})
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d", logoutResp.StatusCode, http.StatusNoContent)
	}

	afterLogout, err := client.Get(server.URL + "/api/v1/auth/session")
	if err != nil {
		t.Fatalf("GET session after logout: %v", err)
	}
	afterLogout.Body.Close()
	if afterLogout.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET session after logout: status = %d, want %d", afterLogout.StatusCode, http.StatusUnauthorized)
	}

	// Now log back in with the same credentials.
	loginResp := postJSON(t, client, server.URL+"/api/v1/auth/login",
		credentialsRequest{Username: "bm-admin", Password: "correct-horse-battery"},
		map[string]string{CSRFHeaderName: csrf})
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusNoContent {
		t.Fatalf("login status = %d, want %d", loginResp.StatusCode, http.StatusNoContent)
	}
}

func TestHandler_EnrollmentIsSingleShot(t *testing.T) {
	svc, server, client := testServer(t)
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	token := currentBootstrapToken(t, svc)

	first := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		credentialsRequest{Username: "bm-admin", Password: "correct-horse-battery"},
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	first.Body.Close()
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("first enroll status = %d, want %d", first.StatusCode, http.StatusNoContent)
	}

	// A second enrollment attempt, even reusing the (already-consumed)
	// token, must be refused: enrollment is permanently closed the moment
	// an administrator exists (§49.1).
	second := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		credentialsRequest{Username: "someone-else", Password: "another-long-password"},
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	second.Body.Close()
	if second.StatusCode != http.StatusForbidden {
		t.Fatalf("second enroll status = %d, want %d (ENROLLMENT_CLOSED)", second.StatusCode, http.StatusForbidden)
	}
}

func TestHandler_LoginRejectsWrongPassword(t *testing.T) {
	svc, server, client := testServer(t)
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	token := currentBootstrapToken(t, svc)

	enrollResp := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		credentialsRequest{Username: "bm-admin", Password: "correct-horse-battery"},
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	enrollResp.Body.Close()

	wrong := postJSON(t, client, server.URL+"/api/v1/auth/login",
		credentialsRequest{Username: "bm-admin", Password: "not-the-right-password"},
		map[string]string{CSRFHeaderName: csrf})
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-password login status = %d, want %d", wrong.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandler_StateChangingRoutesRequireMatchingCSRFHeader(t *testing.T) {
	_, server, client := testServer(t)
	seedCSRFCookie(t, client, server)
	// Deliberately no CSRFHeaderName header at all, even though the CSRF
	// cookie is present in the jar (and will be sent automatically): this
	// is exactly the shape of a forged cross-site request, which can make
	// the browser send the cookie but cannot read it to also set the
	// header.
	resp := postJSON(t, client, server.URL+"/api/v1/auth/login",
		credentialsRequest{Username: "bm-admin", Password: "irrelevant"}, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("login without X-CSRF-Token header: status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestHandler_LoginIsRateLimitedPerIP(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json"), LoginRateLimit: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(false)(mux))
	t.Cleanup(server.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)

	var last *http.Response
	for i := 0; i < 3; i++ {
		last = postJSON(t, client, server.URL+"/api/v1/auth/login",
			credentialsRequest{Username: "bm-admin", Password: "wrong"},
			map[string]string{CSRFHeaderName: csrf})
		last.Body.Close()
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd login attempt (limit is 2) status = %d, want %d", last.StatusCode, http.StatusTooManyRequests)
	}
}

// TestHandler_TrustedForwardedForKeepsRateLimitBucketsPerClient is issue
// #119's review's central regression test, at the full HTTP-handler
// level rather than remoteIP in isolation: apps/generic's two-container
// split means every request this Service's handler ever sees, in
// production, arrives from the SAME direct peer (apps/generic/server.NewUI's
// reverse proxy) regardless of which real external client made it -
// modelled here by every request in this test sharing the same
// httptest.Server connection (so the same RemoteAddr host) while carrying
// DIFFERENT X-Forwarded-For values, exactly as the real proxy's own
// forwarded requests would. Without Config.TrustForwardedHeaders, this
// would collapse into one shared bucket (TestHandler_LoginIsRateLimitedPerIP
// already proves that shape is what happens by default); with it
// enabled, each forwarded client keeps its own budget.
func TestHandler_TrustedForwardedForKeepsRateLimitBucketsPerClient(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json"), LoginRateLimit: 2, TrustForwardedHeaders: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(true)(mux))
	t.Cleanup(server.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)

	attempt := func(forwardedFor string) *http.Response {
		return postJSON(t, client, server.URL+"/api/v1/auth/login",
			credentialsRequest{Username: "bm-admin", Password: "wrong"},
			map[string]string{CSRFHeaderName: csrf, "X-Forwarded-For": forwardedFor})
	}

	// Exhaust client A's own budget (limit is 2).
	for i := 0; i < 2; i++ {
		attempt("203.0.113.10").Body.Close()
	}
	blockedA := attempt("203.0.113.10")
	blockedA.Body.Close()
	if blockedA.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("client A's 3rd attempt status = %d, want %d", blockedA.StatusCode, http.StatusTooManyRequests)
	}

	// A DIFFERENT client (different X-Forwarded-For), arriving over the
	// exact same underlying connection/RemoteAddr, must have its own,
	// untouched budget.
	stillAllowedB := attempt("203.0.113.99")
	stillAllowedB.Body.Close()
	if stillAllowedB.StatusCode == http.StatusTooManyRequests {
		t.Fatal("a different client (different X-Forwarded-For) was rate-limited by client A's own attempts - this is the exact rate-limit collapse the fix is for")
	}
}

// TestHandler_SessionCookieIsSecureWhenForwardedProtoIsTrustedAndHTTPS is
// issue #119's review's central regression test for the Secure-cookie
// finding: with Config.TrustForwardedHeaders enabled, a plaintext request
// to this Service's own handler (httptest.NewServer never uses TLS, so
// r.TLS is nil here exactly as it always is behind
// apps/generic/server.NewUI's reverse proxy) whose X-Forwarded-Proto says
// "https" must still get a Secure session cookie.
func TestHandler_SessionCookieIsSecureWhenForwardedProtoIsTrustedAndHTTPS(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json"), TrustForwardedHeaders: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(true)(mux))
	t.Cleanup(server.Close)

	// No cookie jar: this test reads the raw Set-Cookie response header
	// itself rather than letting a jar/client hide it.
	client := &http.Client{}
	seedResp, err := client.Get(server.URL + "/api/v1/auth/session")
	if err != nil {
		t.Fatalf("seed GET: %v", err)
	}
	seedResp.Body.Close()

	var csrfVal string
	for _, c := range seedResp.Cookies() {
		if c.Name == CSRFCookieName {
			csrfVal = c.Value
		}
	}
	if csrfVal == "" {
		t.Fatal("no CSRF cookie issued by the seed GET")
	}

	token := currentBootstrapToken(t, svc)
	body, err := json.Marshal(credentialsRequest{Username: "bm-admin", Password: "correct-horse-battery"})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/auth/enroll", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeaderName, csrfVal)
	req.Header.Set(BootstrapTokenHeader, token)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfVal})

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie issued on enroll")
	}
	if !sessionCookie.Secure {
		t.Error("session cookie Secure = false, want true (plaintext connection, but X-Forwarded-Proto: https and TrustForwardedHeaders enabled)")
	}
}

func TestWriteAuthError_SetsCorrelationIdHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAuthError(rec, http.StatusBadRequest, "INVALID_REQUEST", "bad request")

	if got := rec.Header().Get("X-Correlation-Id"); got == "" {
		t.Error("X-Correlation-Id header is empty, want a generated correlation id on every auth error response")
	}
}
