package apiclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// The offline stand-in for a running engine, and why it is built out of
// the contract rather than out of a handful of hand-written routes.
//
// A fake whose routes are invented by the same person writing the client
// proves that the client agrees with the fake, which is not a fact anybody
// needs. This one takes its route table, its success statuses, and its
// per-route authentication and CSRF requirements straight from
// apicontract.Endpoints, so an operation the contract declares as
// authenticated is authenticated here without anybody remembering to say
// so, and a client that skips a credential the contract requires fails
// against this the same way it would fail against the real engine.
//
// The parts the contract does NOT describe are the ones written out
// literally below, deliberately, because they are exactly what a second
// implementation gets wrong: the cookie the runtime issues on every
// response that lacks one, the byte-for-byte double-submit comparison,
// and the fact that /auth's refusals are shaped differently from every
// other route's. Those three come from apps/common/csrf,
// apps/common/auth/local and apps/common/webhost, which core may not
// import, so contract_test.go pins them against api/v1/openapi.json
// instead of trusting this file.
//
// Nothing here listens on a fixed port, reads a file, or needs a network:
// the whole suite is an httptest server and a client, which is what keeps
// it as fast as scripts/install/install_docker_host.py's own suite.

// seenRequest is one request the engine was asked to serve, recorded
// before any of the checks below could refuse it. The tests assert on
// these rather than on the client's own account of what it did.
type seenRequest struct {
	Method     string
	Path       string
	Operation  string
	CSRFHeader string
	CSRFCookie string
	Session    string
}

// fakeEngine serves /api/v1 the way the engine container does.
type fakeEngine struct {
	t *testing.T

	// The enrolled administrator. An empty password means no account
	// exists, which the engine answers exactly as it answers a wrong one.
	username string
	password string

	// issueCSRFCookie is the runtime's EnsureCSRFCookie behaviour: on by
	// default, and turned off by the one test that asks what a client
	// does when the double-submit token it has to echo never arrives.
	issueCSRFCookie bool

	// answer replaces the default response for one operation, which is how
	// every "the engine said something the contract does not describe"
	// case is planted.
	answer map[string]http.HandlerFunc

	mu       sync.Mutex
	sessions map[string]bool
	csrf     map[string]bool
	seen     []seenRequest
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	return &fakeEngine{
		t:               t,
		username:        "operator",
		password:        "correct-horse-battery",
		issueCSRFCookie: true,
		answer:          map[string]http.HandlerFunc{},
		sessions:        map[string]bool{},
		csrf:            map[string]bool{},
	}
}

// start runs the engine on a loopback listener and returns its base URL,
// the shape a CLI running inside the engine's own container sees.
func (e *fakeEngine) start() string {
	srv := httptest.NewServer(e)
	e.t.Cleanup(srv.Close)
	return srv.URL
}

// requests returns what the engine was asked for, in order.
func (e *fakeEngine) requests() []seenRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]seenRequest, len(e.seen))
	copy(out, e.seen)
	return out
}

// reset forgets every request recorded so far, so a test can look at what
// one call did without the sign-in handshake in front of it.
func (e *fakeEngine) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = nil
}

// operations returns just the operation ids, which is what most assertions
// about "what did the client do" actually care about.
func (e *fakeEngine) operations() []string {
	var out []string
	for _, r := range e.requests() {
		out = append(out, r.Operation)
	}
	return out
}

// route is one contract operation with its path template compiled.
type route struct {
	ep apicontract.Endpoint
	re *regexp.Regexp
}

var pathParameter = regexp.MustCompile(`\{[^}]+\}`)

// routes compiles apicontract.Endpoints once. A path template becomes a
// pattern whose parameters match one segment each, so the engine routes
// /backup-sets/production/pg the same way chi does.
var routes = func() []route {
	out := make([]route, 0, len(apicontract.Endpoints))
	for _, ep := range apicontract.Endpoints {
		var b strings.Builder
		b.WriteString("^")
		last := 0
		for _, m := range pathParameter.FindAllStringIndex(ep.Path, -1) {
			b.WriteString(regexp.QuoteMeta(ep.Path[last:m[0]]))
			b.WriteString(`[^/]+`)
			last = m[1]
		}
		b.WriteString(regexp.QuoteMeta(ep.Path[last:]))
		b.WriteString("$")
		out = append(out, route{ep: ep, re: regexp.MustCompile(b.String())})
	}
	return out
}()

// lookupRoute finds the contract operation a (method, path) pair names.
func lookupRoute(method, path string) (apicontract.Endpoint, bool) {
	rel, ok := strings.CutPrefix(path, apicontract.BasePath)
	if !ok {
		return apicontract.Endpoint{}, false
	}
	for _, r := range routes {
		if r.ep.Method == method && r.re.MatchString(rel) {
			return r.ep, true
		}
	}
	return apicontract.Endpoint{}, false
}

func (e *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// EnsureCSRFCookie wraps the runtime's ENTIRE handler, not just its
	// mutating routes, so the cookie is issued before anything can refuse
	// the request for not carrying it.
	if e.issueCSRFCookie {
		if _, err := r.Cookie(csrfCookieName); err != nil {
			token := "csrf-" + fmt.Sprint(len(e.seen))
			e.mu.Lock()
			e.csrf[token] = true
			e.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: token, Path: "/"})
		}
	}

	// EscapedPath, not Path. A backup set whose id contains a slash or a
	// space arrives percent-encoded, and reading the decoded form would
	// route /backup-sets/a%2Fb as if it named two segments.
	escaped := r.URL.EscapedPath()
	ep, known := lookupRoute(r.Method, escaped)

	seen := seenRequest{Method: r.Method, Path: escaped, Operation: ep.ID, CSRFHeader: r.Header.Get(csrfHeaderName)}
	if c, err := r.Cookie(csrfCookieName); err == nil {
		seen.CSRFCookie = c.Value
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		seen.Session = c.Value
	}
	e.mu.Lock()
	e.seen = append(e.seen, seen)
	e.mu.Unlock()

	if !known {
		// The real router answers an unknown path with chi's own 404 and
		// no envelope at all, which is what a client reaching for an
		// operation this deployment does not serve actually meets.
		http.NotFound(w, r)
		return
	}

	if ep.Authenticated && !e.hasSession(r) {
		e.refuse(w, ep, http.StatusUnauthorized, apicontract.ErrorCodeUnauthenticated, "authentication required")
		return
	}
	if ep.CSRFRequired && !e.csrfMatches(r) {
		e.refuse(w, ep, http.StatusForbidden, apicontract.ErrorCodeCSRFTokenMismatch, "missing or mismatched "+csrfHeaderName+" header")
		return
	}

	if custom, ok := e.answer[ep.ID]; ok {
		custom(w, r)
		return
	}

	switch ep.ID {
	case "login":
		e.login(w, r, ep)
	case "logout":
		e.logout(w)
	default:
		e.succeed(w, ep)
	}
}

func (e *fakeEngine) hasSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[c.Value]
}

// csrfMatches is the double-submit comparison: the header has to equal the
// cookie the caller sent, and both have to be present.
func (e *fakeEngine) csrfMatches(r *http.Request) bool {
	c, err := r.Cookie(csrfCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return r.Header.Get(csrfHeaderName) == c.Value
}

func (e *fakeEngine) login(w http.ResponseWriter, r *http.Request, ep apicontract.Endpoint) {
	var req apicontract.CredentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		e.refuse(w, ep, http.StatusBadRequest, apicontract.ErrorCodeInvalidRequest, "malformed request body")
		return
	}
	// The real handler answers a wrong username, a wrong password and an
	// unenrolled deployment identically, so this does too: a client that
	// tried to tell them apart would be reading a distinction the service
	// deliberately refuses to make.
	if e.password == "" || req.Username != e.username || req.Password != e.password {
		e.refuse(w, ep, http.StatusUnauthorized, apicontract.ErrorCodeUnauthenticated, "that username and password combination was not accepted")
		return
	}
	token := fmt.Sprintf("session-%d", len(e.sessions)+1)
	e.mu.Lock()
	e.sessions[token] = true
	e.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true})
	w.WriteHeader(http.StatusNoContent)
}

func (e *fakeEngine) logout(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// succeed writes the operation's declared success status, with a body of
// the declared response schema's own generated type when it declares one.
func (e *fakeEngine) succeed(w http.ResponseWriter, ep apicontract.Endpoint) {
	if ep.ResponseSchema == "" {
		w.WriteHeader(ep.SuccessStatus)
		return
	}
	value, ok := apicontract.SchemaTypes[ep.ResponseSchema]
	if !ok {
		e.t.Fatalf("the contract names response schema %q for %s, and apicontract.SchemaTypes has no type for it", ep.ResponseSchema, ep.ID)
	}
	body, err := json.Marshal(value)
	if err != nil {
		e.t.Fatalf("marshalling %s: %v", ep.ResponseSchema, err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(ep.SuccessStatus)
	_, _ = w.Write(body)
}

// refuse writes the envelope the route that refused actually uses.
//
// The two are not the same, and the disagreement is real rather than an
// artifact of this fake: apps/common/auth/local answers flat
// ({code, message, correlationId}) because ui/shared's login page reads
// that, and apps/common/webhost nests code and message under an "error"
// key. Both shapes are in the contract, as AuthErrorResponse and
// ErrorResponse, and a client that parsed only one would read the other's
// refusals as an empty code.
func (e *fakeEngine) refuse(w http.ResponseWriter, ep apicontract.Endpoint, status int, code apicontract.ErrorCode, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Correlation-Id", "cid_fake")
	w.WriteHeader(status)
	if strings.HasPrefix(ep.Path, "/auth/") {
		_ = json.NewEncoder(w).Encode(apicontract.AuthErrorResponse{Code: code, Message: message, CorrelationID: "cid_fake"})
		return
	}
	var body apicontract.ErrorResponse
	body.Error.Code = code
	body.Error.Message = message
	_ = json.NewEncoder(w).Encode(body)
}

// signedIn returns a client already configured with this engine's
// credentials, which is the shape every test that is not about
// authentication itself wants.
func (e *fakeEngine) client(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: baseURL, Username: e.username, Password: e.password})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}
