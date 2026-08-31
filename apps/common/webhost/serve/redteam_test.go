// redteam_test.go is issue #87 (B5.1)'s forge-and-replay harness: the
// adversarial half of the trusted-gateway boundary, driven against the
// canonical runtime as it is actually composed in production rather than
// against any one component in isolation.
//
// # Why this suite exists next to parity_test.go rather than inside it
//
// parity_test.go already proves the FUNCTIONAL boundary: an untrusted
// peer is refused, a missing identity is refused differently, an
// unparsable address is untrusted. Every one of those drives
// serve.NewEngine directly, which is the shape a developer reaches for
// and is not the shape production runs. container/compose.yaml ships TWO
// services from one image: the engine, with no published port, and the
// UI host, which is the only thing on a LAN-facing port and which reverse
// proxies /api/v1 straight through to the engine. In that topology the
// engine's only possible direct peer is the UI host, so a gateway
// profile's trusted-peer range has to contain the UI host or the
// deployment cannot authenticate at all - and at that point every
// question parity_test.go answers about "an untrusted peer" is being
// asked about the wrong hop.
//
// So this suite composes NewUI in front of NewEngine, exactly as
// apps/generic/cmd/backup-manager-web does, and attacks the composition.
//
// # Every attack proves it arrived before it asserts it was stopped
//
// A test that asserts "the request was refused" passes just as happily
// when the request never reached the code under test at all - a proxy
// that 502s, a route that 404s, a listener that was never up. Each attack
// below therefore carries an arrival oracle: either a recording upstream
// that shows exactly which headers crossed the hop, or an assertion on
// the refusal's own typed error code, which only the /api/v1 error writer
// produces. "Refused" and "nothing works" are never allowed to look the
// same here.
package serve_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/csrf"
	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
	"github.com/spdrman/rclone-manager/core/service"
)

// loopbackCIDRs is the trusted-peer range that CONTAINS whatever httptest
// connects from, and untrustedCIDRs is one that provably does not. Every
// synthetic peer in this file is one or the other; there is no real
// second machine and none is needed.
var (
	loopbackCIDRs  = []string{"127.0.0.0/8", "::1/128"}
	untrustedCIDRs = []string{"10.99.0.0/16"}
)

// stack is the two-service composition under attack: a UI host on a
// LAN-facing listener, reverse proxying to an engine that has no listener
// of its own that anything but the UI host can reach.
type stack struct {
	edgeURL   string
	engineURL string
	header    string
	profile   profile.Profile
}

// newStack builds the composition. engineTrusts is the trusted-peer range
// the ENGINE is configured with; in the shipped topology that range has
// to contain the UI host, which is why loopbackCIDRs is the realistic
// value and not a weakened one.
func newStack(t *testing.T, engineTrusts []string) *stack {
	t.Helper()

	configPath := writeTestConfig(t)
	backend, cleanup, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	authSvc, err := local.New(local.Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	p := profileFor(t, string(profile.UGOS))
	p.Gateway.TrustedPeers = engineTrusts
	adapter, err := p.Adapter(profile.AdapterConfig{LocalAuth: authSvc.Authenticator()})
	if err != nil {
		t.Fatalf("Adapter: %v", err)
	}

	engine := httptest.NewServer(serve.NewEngine(serve.EngineConfig{Platform: adapter, Backend: backend}))
	t.Cleanup(engine.Close)

	upstream, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatalf("parse engine URL: %v", err)
	}
	edge := httptest.NewServer(serve.NewUI(serve.UIConfig{
		Upstream: upstream,
		StaticFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>ui</title>")}},
	}))
	t.Cleanup(edge.Close)

	return &stack{edgeURL: edge.URL, engineURL: engine.URL, header: p.Gateway.UsernameHeader, profile: p}
}

// errorCode reads the typed error code out of an /api/v1 refusal. An
// empty return means the body was not one of this API's own error
// documents, which is itself the interesting answer: it means the refusal
// came from something other than the API.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	return doc.Error.Code
}

func do(t *testing.T, req *http.Request) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// forge builds one attacker request: a plain HTTP client on the LAN,
// hitting the only published port, setting the provider-native identity
// header itself. It is NOT a browser, so it also controls both halves of
// the double-submit CSRF pair - which is exactly why CSRF is not, and
// was never meant to be, an authentication control.
func (s *stack) forge(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.edgeURL+path, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(s.header, "admin")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	const token = "attacker-chosen-csrf-token"
	req.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	req.Header.Set(csrf.HeaderName, token)
	return req
}

// ---------------------------------------------------------------------
// The headline attack: forge the identity header at the LAN-facing edge
// ---------------------------------------------------------------------

// TestForgedIdentityAtTheEdgeCannotRead is the read half. An
// unauthenticated client on the LAN sets the provider-native identity
// header on a request to the ONE published port and asks for the backup
// set list.
//
// The arrival oracle is the typed error code: a 401 carrying
// UNAUTHENTICATED came from apps/common/webhost's own auth middleware,
// which can only have run because the request crossed the proxy hop and
// reached the engine's /api/v1 router. A 502, a 404, or an empty body
// would mean the attack never landed and the refusal proves nothing.
func TestForgedIdentityAtTheEdgeCannotRead(t *testing.T) {
	s := newStack(t, loopbackCIDRs)

	code, _, body := do(t, s.forge(t, http.MethodGet, "/api/v1/backup-sets", ""))
	if code != http.StatusUnauthorized {
		t.Fatalf("a forged %s header from the LAN read %s and got %d: %s\n"+
			"the browser-facing edge forwarded a provider-native identity header it never authenticated, and the engine believed it because its only peer IS that edge",
			s.header, "/api/v1/backup-sets", code, body)
	}
	if got := errorCode(t, body); got != "UNAUTHENTICATED" {
		t.Errorf("error code = %q, want UNAUTHENTICATED; without it this 401 is not evidence the request reached the auth boundary at all (body %s)", got, body)
	}
}

// TestForgedIdentityAtTheEdgeCannotWriteSettings is the write half, and
// the one that makes this a vulnerability rather than an information
// leak. PATCH /api/v1/settings is authenticated and CSRF-protected and
// deliberately not behind the destructive gate (issue #171). An attacker
// who can forge the identity satisfies both of the controls that remain.
func TestForgedIdentityAtTheEdgeCannotWriteSettings(t *testing.T) {
	s := newStack(t, loopbackCIDRs)

	req := s.forge(t, http.MethodPatch, "/api/v1/settings", `{"retention":{"protect_last_known_good":false}}`)
	code, _, body := do(t, req)
	if code == http.StatusOK {
		t.Fatalf("a forged %s header from the LAN turned protect_last_known_good OFF and got 200: %s\n"+
			"FR-19's last-known-good protection was disabled by a request that never authenticated", s.header, body)
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("the forged settings write was refused with %d, want 401: %s", code, body)
	}
	if got := errorCode(t, body); got != "UNAUTHENTICATED" {
		t.Errorf("error code = %q, want UNAUTHENTICATED (body %s)", got, body)
	}
}

// TestForgedIdentityAtTheEdgeCannotDriveAnOutboundProbe. POST
// /api/v1/ssh/host-key-probe opens a real outbound TCP/SSH connection to
// a caller-supplied host:port. Reachable without authentication, that is
// a network-probing primitive pointed at the NAS's own internal network
// by anyone who can reach the published port.
func TestForgedIdentityAtTheEdgeCannotDriveAnOutboundProbe(t *testing.T) {
	s := newStack(t, loopbackCIDRs)

	req := s.forge(t, http.MethodPost, "/api/v1/ssh/host-key-probe", `{"host":"127.0.0.1","port":9}`)
	code, _, body := do(t, req)
	if code != http.StatusUnauthorized {
		t.Fatalf("a forged %s header from the LAN reached the host-key probe and got %d: %s\n"+
			"an unauthenticated caller can point the engine's outbound SSH dialler at any host:port it can reach", s.header, code, body)
	}
	if got := errorCode(t, body); got != "UNAUTHENTICATED" {
		t.Errorf("error code = %q, want UNAUTHENTICATED (body %s)", got, body)
	}
}

// TestTheEdgeStripsTheIdentityHeaderBeforeForwarding is issue #87's
// "refused with the header stripped rather than merely unauthorized"
// criterion, asserted where it is actually observable: a recording
// upstream standing in for the engine, which reports exactly what crossed
// the hop.
//
// The two positive controls matter as much as the assertion. If the edge
// simply dropped every header, or if the recorder never ran, the strip
// assertion would pass for the wrong reason - so the same request also
// has to prove that an unrelated header survived untouched AND that the
// proxy's own X-Forwarded-For arrived.
func TestTheEdgeStripsTheIdentityHeaderBeforeForwarding(t *testing.T) {
	var (
		mu   sync.Mutex
		seen http.Header
	)
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(recorder.Close)

	upstream, err := url.Parse(recorder.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	edge := httptest.NewServer(serve.NewUI(serve.UIConfig{
		Upstream: upstream,
		StaticFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ui")}},
	}))
	t.Cleanup(edge.Close)

	header := profile.UGOS.Profile().Gateway.UsernameHeader
	req, err := http.NewRequest(http.MethodGet, edge.URL+"/api/v1/backup-sets", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// Deliberately lower-cased on the wire: a strip keyed on an exact
	// byte match rather than on HTTP's own case-insensitive header
	// identity would miss this.
	req.Header[strings.ToLower(header)] = []string{"admin"}
	req.Header.Set("X-Benign-Passthrough", "keep-me")

	code, _, _ := do(t, req)
	if code != http.StatusNoContent {
		t.Fatalf("the recording upstream answered %d, want 204; the request never crossed the hop so nothing below proves anything", code)
	}

	mu.Lock()
	got := seen
	mu.Unlock()
	if got == nil {
		t.Fatal("the recording upstream never ran")
	}
	if v := got.Values(header); len(v) != 0 {
		t.Errorf("%s crossed the proxy hop as %q; the edge forwards a provider-native identity header it never authenticated", header, v)
	}
	// Positive control 1: the strip is targeted, not a blanket wipe.
	if got.Get("X-Benign-Passthrough") != "keep-me" {
		t.Errorf("X-Benign-Passthrough = %q, want %q; if the edge dropped everything the assertion above would pass for the wrong reason",
			got.Get("X-Benign-Passthrough"), "keep-me")
	}
	// Positive control 2: the proxy really did rewrite headers on this
	// request, so an absent identity header means removed and not merely
	// never sent.
	if got.Get("X-Forwarded-For") == "" {
		t.Error("X-Forwarded-For is absent, so the proxy's own header rewriting did not run on this request")
	}
}

// TestTheEdgeStripsEveryProfilesIdentityHeader. A deployment running the
// generic profile still has to strip a header some OTHER profile in the
// table declares: the edge cannot know which profile the engine behind it
// was started with, and an engine that was in fact started with that
// profile would believe it.
func TestTheEdgeStripsEveryProfilesIdentityHeader(t *testing.T) {
	var (
		mu   sync.Mutex
		seen http.Header
	)
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(recorder.Close)

	upstream, _ := url.Parse(recorder.URL)
	edge := httptest.NewServer(serve.NewUI(serve.UIConfig{
		Upstream: upstream,
		StaticFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ui")}},
	}))
	t.Cleanup(edge.Close)

	var headers []string
	for _, id := range profile.IDs() {
		if g := id.Profile().Gateway; g != nil {
			headers = append(headers, g.UsernameHeader)
		}
	}
	if len(headers) == 0 {
		t.Skip("no profile declares an identity header, so there is nothing to strip")
	}

	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/v1/backup-sets", nil)
	for _, h := range headers {
		req.Header.Set(h, "admin")
	}
	if code, _, _ := do(t, req); code != http.StatusNoContent {
		t.Fatalf("the recording upstream answered %d, want 204", code)
	}

	mu.Lock()
	got := seen
	mu.Unlock()
	for _, h := range headers {
		if v := got.Values(h); len(v) != 0 {
			t.Errorf("%s crossed the proxy hop as %q", h, v)
		}
	}
}

// ---------------------------------------------------------------------
// Header smuggling into the engine
// ---------------------------------------------------------------------

// TestDuplicateIdentityHeaderIsRefusedRatherThanResolved. A gateway that
// APPENDS its header rather than replacing it (the difference between an
// nginx `add_header` and a `proxy_set_header`, and the default behaviour
// of any pass-through proxy) leaves two values on the wire: the client's
// and the gateway's. Go's Header.Get returns the FIRST, which is the
// client's, so a silent resolution here hands the attacker the identity.
//
// There is no correct value to pick, so the only safe answer is to refuse
// the request. The positive control is the same request with one value,
// which must still authenticate.
func TestDuplicateIdentityHeaderIsRefusedRatherThanResolved(t *testing.T) {
	s := newStack(t, loopbackCIDRs)

	req, _ := http.NewRequest(http.MethodGet, s.engineURL+"/api/v1/backup-sets", nil)
	req.Header.Add(s.header, "attacker")
	req.Header.Add(s.header, "operator")
	code, _, body := do(t, req)
	if code != http.StatusUnauthorized {
		t.Errorf("two %s values authenticated with %d: %s\nan ambiguous identity was silently resolved rather than refused", s.header, code, body)
	}

	// Positive control: one value from the same peer still works, so the
	// refusal above is about the ambiguity and not about the peer.
	ok, _ := http.NewRequest(http.MethodGet, s.engineURL+"/api/v1/backup-sets", nil)
	ok.Header.Set(s.header, "operator")
	if code, _, body := do(t, ok); code != http.StatusOK {
		t.Fatalf("the single-valued positive control returned %d, want 200: %s", code, body)
	}
}

// TestASmuggledPipelinedRequestIsStrippedToo. A request declaring both
// Content-Length and Transfer-Encoding is the classic proxy/origin desync
// primitive: the two hops disagree about where the body ends, so the tail
// of one request becomes the head of the next and arrives at the origin
// having skipped whatever the edge does to a request it can see.
//
// Both hops here are Go, which resolves the conflict the same way on both
// sides, so there is no desync to exploit. What this test actually pins is
// the stronger property that makes that resolution irrelevant: the strip
// is per-request at the edge, so a request the edge only ever saw as a
// pipelined follow-on is stripped exactly like the first one.
//
// The pipelining control is what keeps this from being vacuous. A
// well-formed pipelined pair must deliver TWO requests to the upstream, so
// "the forged identity never arrived" cannot be explained away by the
// second request having been dropped on the floor.
func TestASmuggledPipelinedRequestIsStrippedToo(t *testing.T) {
	header := profile.UGOS.Profile().Gateway.UsernameHeader

	var (
		mu      sync.Mutex
		arrived []string
	)
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrived = append(arrived, r.Method+" "+r.URL.Path+" identity="+r.Header.Get(header))
		mu.Unlock()
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(recorder.Close)

	upstream, err := url.Parse(recorder.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	edge := httptest.NewServer(serve.NewUI(serve.UIConfig{
		Upstream: upstream,
		StaticFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ui")}},
	}))
	t.Cleanup(edge.Close)
	host := strings.TrimPrefix(edge.URL, "http://")

	// speak writes raw bytes at the edge's listener and drains whatever
	// responses come back, returning how many it managed to read.
	speak := func(raw string) int {
		conn, dialErr := net.DialTimeout("tcp", host, 5*time.Second)
		if dialErr != nil {
			t.Fatalf("dial edge: %v", dialErr)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, wErr := io.WriteString(conn, raw); wErr != nil {
			t.Fatalf("write: %v", wErr)
		}
		br := bufio.NewReader(conn)
		responses := 0
		for {
			resp, rErr := http.ReadResponse(br, nil)
			if rErr != nil {
				return responses
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			responses++
		}
	}

	// The pipelining control first: two well-formed requests back to back,
	// the second carrying the forged identity.
	mu.Lock()
	arrived = nil
	mu.Unlock()
	speak("GET /api/v1/backup-sets HTTP/1.1\r\nHost: " + host + "\r\n\r\n" +
		"GET /api/v1/backup-sets HTTP/1.1\r\nHost: " + host + "\r\n" + header + ": admin\r\nConnection: close\r\n\r\n")
	mu.Lock()
	pipelined := append([]string(nil), arrived...)
	mu.Unlock()
	if len(pipelined) < 2 {
		t.Fatalf("a well-formed pipelined pair delivered %d request(s) to the upstream (%v); the smuggling assertion below would pass for the wrong reason", len(pipelined), pipelined)
	}
	for _, a := range pipelined {
		if strings.HasSuffix(a, "identity=admin") {
			t.Errorf("a pipelined follow-on request carried the forged identity to the engine: %q", a)
		}
	}

	// Now the desync attempt itself.
	mu.Lock()
	arrived = nil
	mu.Unlock()
	speak("POST /api/v1/backup-sets HTTP/1.1\r\nHost: " + host + "\r\n" +
		"Content-Length: 6\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"0\r\n\r\n" +
		"GET /api/v1/backup-sets HTTP/1.1\r\nHost: " + host + "\r\n" + header + ": admin\r\nConnection: close\r\n\r\n")
	mu.Lock()
	smuggled := append([]string(nil), arrived...)
	mu.Unlock()
	for _, a := range smuggled {
		if strings.HasSuffix(a, "identity=admin") {
			t.Errorf("a smuggled request carrying a forged identity reached the engine: %q", a)
		}
	}
}

func mustRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

// ---------------------------------------------------------------------
// The refusal must not oracle which check failed
// ---------------------------------------------------------------------

// TestRefusalsDoNotOracleWhetherThePeerWasTrusted. The profile layer keeps
// ErrUntrustedPeer and ErrNoGatewayIdentity distinct on purpose, because
// an operator debugging a misconfigured gateway has to tell them apart.
// The HTTP boundary must not: a caller who can distinguish "your peer is
// not trusted" from "your peer is trusted but sent no identity" has been
// handed a map of the trust boundary, one probe at a time.
func TestRefusalsDoNotOracleWhetherThePeerWasTrusted(t *testing.T) {
	trusted := newStack(t, loopbackCIDRs)
	untrusted := newStack(t, untrustedCIDRs)

	// Trusted peer, no identity header at all.
	aCode, _, aBody := do(t, mustRequest(t, http.MethodGet, trusted.engineURL+"/api/v1/backup-sets"))
	// Untrusted peer, identity header set.
	bReq := mustRequest(t, http.MethodGet, untrusted.engineURL+"/api/v1/backup-sets")
	bReq.Header.Set(untrusted.header, "admin")
	bCode, _, bBody := do(t, bReq)

	if aCode != http.StatusUnauthorized || bCode != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want 401 for both: %s / %s", aCode, bCode, aBody, bBody)
	}
	var a, b map[string]any
	if err := json.Unmarshal(aBody, &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := json.Unmarshal(bBody, &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a["error"].(map[string]any)["code"] != b["error"].(map[string]any)["code"] ||
		a["error"].(map[string]any)["message"] != b["error"].(map[string]any)["message"] {
		t.Errorf("the two refusals differ, so a caller can tell a trusted peer from an untrusted one by probing:\n  trusted-peer-no-identity: %s\n  untrusted-peer-with-identity: %s", aBody, bBody)
	}

	// Positive control: the two cases really are different underneath, so
	// the equality above is a deliberate collapse and not an accident of
	// both paths being the same code.
	g, err := (&profile.Gateway{TrustedPeers: loopbackCIDRs, UsernameHeader: trusted.header}).Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, errUntrusted := g.Authenticate(context.Background(), capabilities.AuthRequest{RemoteAddr: "10.0.0.1:1", Headers: http.Header{}})
	_, errNoIdentity := g.Authenticate(context.Background(), capabilities.AuthRequest{RemoteAddr: "127.0.0.1:1", Headers: http.Header{}})
	if errUntrusted == nil || errNoIdentity == nil || errUntrusted.Error() == errNoIdentity.Error() {
		t.Errorf("the profile layer does not distinguish the two cases (%v / %v), so the collapse asserted above is not actually hiding anything", errUntrusted, errNoIdentity)
	}
}

// ---------------------------------------------------------------------
// The generic profile must never believe an identity header
// ---------------------------------------------------------------------

// TestTheGenericProfileNeverBelievesAnIdentityHeader. Selecting a runtime
// profile may supply an identity; it may never decide what that identity
// may do, and a profile with no gateway has no identity to supply at all.
// A request carrying another profile's header, from the most trusted peer
// there is, must still be unauthenticated.
func TestTheGenericProfileNeverBelievesAnIdentityHeader(t *testing.T) {
	configPath := writeTestConfig(t)
	backend, cleanup, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	authSvc, err := local.New(local.Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	p := profile.Generic.Profile()
	adapter, err := p.Adapter(profile.AdapterConfig{LocalAuth: authSvc.Authenticator()})
	if err != nil {
		t.Fatalf("Adapter: %v", err)
	}
	engine := httptest.NewServer(serve.NewEngine(serve.EngineConfig{
		Platform:   adapter,
		AuthRoutes: authSvc.Handler(),
		Backend:    backend,
	}))
	t.Cleanup(engine.Close)

	req := mustRequest(t, http.MethodGet, engine.URL+"/api/v1/backup-sets")
	req.Header.Set(profile.UGOS.Profile().Gateway.UsernameHeader, "admin")
	code, _, body := do(t, req)
	if code != http.StatusUnauthorized {
		t.Fatalf("the generic profile answered %d to a request carrying another profile's identity header: %s", code, body)
	}
}

// ---------------------------------------------------------------------
// The browser surface
// ---------------------------------------------------------------------

// TestTheBrowserFacingEdgeRefusesToBeFramed. The shared UI drives every
// destructive route this product has. Served with no anti-framing header,
// it can be loaded invisibly inside an attacker's page and clicked
// through by the operator's own cursor - the one attack the double-submit
// CSRF token does nothing about, because a framed same-origin document
// reads its own cookie perfectly well.
func TestTheBrowserFacingEdgeRefusesToBeFramed(t *testing.T) {
	s := newStack(t, loopbackCIDRs)

	for _, path := range []string{"/", "/sets/production", "/index.html"} {
		t.Run(path, func(t *testing.T) {
			code, headers, body := do(t, mustRequest(t, http.MethodGet, s.edgeURL+path))
			if code != http.StatusOK {
				t.Fatalf("GET %s returned %d, so this response is not the app shell and proves nothing: %s", path, code, body)
			}
			if len(body) == 0 {
				t.Fatal("the app shell came back empty")
			}
			xfo := headers.Get("X-Frame-Options")
			csp := headers.Get("Content-Security-Policy")
			if !strings.EqualFold(xfo, "DENY") && !strings.Contains(csp, "frame-ancestors") {
				t.Errorf("neither X-Frame-Options nor a frame-ancestors directive is set (X-Frame-Options=%q, Content-Security-Policy=%q); the admin console can be framed", xfo, csp)
			}
			if got := headers.Get("X-Content-Type-Options"); !strings.EqualFold(got, "nosniff") {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if headers.Get("Referrer-Policy") == "" {
				t.Error("Referrer-Policy is unset, so the console's own URLs leak to anything it links out to")
			}
		})
	}
}

// TestTheAPISurfaceIsAlsoNosniff. The engine's JSON travels back through
// the same browser. An error document sniffed as HTML is a reflected-XSS
// primitive wherever a message echoes caller-supplied text.
func TestTheAPISurfaceIsAlsoNosniff(t *testing.T) {
	s := newStack(t, loopbackCIDRs)

	req := mustRequest(t, http.MethodGet, s.edgeURL+"/api/v1/backup-sets")
	req.Header.Set(s.header, "operator")
	code, headers, body := do(t, req)
	// Whatever the answer is, it came from the API; that is all this
	// assertion needs.
	if errorCode(t, body) == "" && code != http.StatusOK {
		t.Fatalf("GET /api/v1/backup-sets returned %d with a body this API did not write: %s", code, body)
	}
	if got := headers.Get("X-Content-Type-Options"); !strings.EqualFold(got, "nosniff") {
		t.Errorf("X-Content-Type-Options = %q on an /api/v1 response, want nosniff", got)
	}
}
