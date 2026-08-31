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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/csrf"
	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
	"github.com/spdrman/rclone-manager/apps/common/webhost"
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

// newStack builds the composition with NO gateway configured at the edge,
// which is the shipped default: the UI host faces the LAN and can prove
// nothing about who it is talking to. engineTrusts is the trusted-peer
// range the ENGINE is configured with; in the shipped topology that range
// has to contain the UI host, which is why loopbackCIDRs is the realistic
// value and not a weakened one.
func newStack(t *testing.T, engineTrusts []string) *stack {
	return newStackWithEdgeGateway(t, engineTrusts, nil)
}

// newStackWithEdgeGateway is newStack with the edge told where its
// platform gateway is. edgeTrusts nil means the edge trusts nobody.
func newStackWithEdgeGateway(t *testing.T, engineTrusts, edgeTrusts []string) *stack {
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
	uiCfg := serve.UIConfig{
		Upstream: upstream,
		StaticFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>ui</title>")}},
	}
	if edgeTrusts != nil {
		compiled, cErr := (&profile.Gateway{TrustedPeers: edgeTrusts, UsernameHeader: p.Gateway.UsernameHeader}).Compile()
		if cErr != nil {
			t.Fatalf("compile edge gateway: %v", cErr)
		}
		uiCfg.Gateway = compiled
	}
	edge := httptest.NewServer(serve.NewUI(uiCfg))
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
		// Not a skip (issue #87's review, M3). A registry edit that
		// removes every Gateway, or a bug in IDs()/Profile(), would turn
		// the strongest strip assertion in this file into a silent pass,
		// and a skip does not fail a gate. An empty header table is not a
		// reason to stop measuring, it is a reason to stop the build.
		t.Fatal("no profile declares an identity header, so this test would pass without stripping anything; the registry, IDs() or Profile() has changed under it")
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
		defer func() { _ = conn.Close() }()
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

// ---------------------------------------------------------------------
// The positive control for the whole boundary
// ---------------------------------------------------------------------

// TestTheTrustedGatewayPathStillWorksThroughTheEdge is issue #87's
// required positive control: with the edge told where its platform
// gateway is, a request from that gateway authenticates end to end,
// through both hops, on the same composition every attack above ran
// against.
//
// Without this, every refusal in this file could equally be explained by
// the identity path having been broken outright, and "refused" and
// "nothing works" would be the same green.
func TestTheTrustedGatewayPathStillWorksThroughTheEdge(t *testing.T) {
	s := newStackWithEdgeGateway(t, loopbackCIDRs, loopbackCIDRs)

	req := mustRequest(t, http.MethodGet, s.edgeURL+"/api/v1/backup-sets")
	req.Header.Set(s.header, "operator")
	code, _, body := do(t, req)
	if code != http.StatusOK {
		t.Fatalf("the trusted-gateway path returned %d, want 200: %s\n"+
			"the boundary now refuses the gateway it is configured to believe, so every refusal in this file proves nothing", code, body)
	}
	if !strings.Contains(string(body), "backup_sets") {
		t.Errorf("the trusted-gateway response is not the backup-set list: %s", body)
	}

	// The same edge, same listener, same request shape, from a peer the
	// edge does not trust: the whole point of configuring a gateway is
	// that it is a boundary and not a switch.
	untrustedEdge := newStackWithEdgeGateway(t, loopbackCIDRs, untrustedCIDRs)
	req2 := mustRequest(t, http.MethodGet, untrustedEdge.edgeURL+"/api/v1/backup-sets")
	req2.Header.Set(untrustedEdge.header, "operator")
	if code, _, body := do(t, req2); code != http.StatusUnauthorized {
		t.Fatalf("an edge configured to trust a range this request did not come from answered %d, want 401: %s", code, body)
	}
}

// TestTheTrustedGatewayStillCannotSetAnotherProfilesIdentity. A gateway
// is trusted to assert the identity of the profile it belongs to, and
// nothing else. A header belonging to a profile this process is not
// running has no provenance even coming from the right peer, and letting
// it through would make the next profile added to the table a change in
// what today's deployments believe.
//
// The rule is exercised TODAY rather than when a second gateway profile
// lands (issue #87's review, M3). The previous version of this test hid
// its only assertion about another profile's header behind
// `if len(profile.IdentityHeaders()) > 1`, which is false while UGOS is
// the only profile with a Gateway, so the branch that implements the rule
// had never had a second declared header run through it. A check first
// trusted at the exact moment it stops being trivially true is not a
// check.
//
// The shape below gets both arms out of a one-entry table without a seam
// into the registry: compile a gateway whose own UsernameHeader is a
// SYNTHETIC name that no profile declares, and plant the real, declared
// X-Ugos-User alongside it. Sanitize from the trusted peer then has to
// keep the synthetic header (this gateway owns it) and remove the real
// one (this gateway does not), which is exactly the two-profile case.
func TestTheTrustedGatewayStillCannotSetAnotherProfilesIdentity(t *testing.T) {
	const (
		synthetic = "X-Synthetic-Gateway-User"
		benign    = "X-Benign-Passthrough"
	)
	declared := profile.UGOS.Profile().Gateway.UsernameHeader

	// Non-vacuity, both directions. The assertion "the real header was
	// removed" proves nothing if no profile declares it (Sanitize would
	// never have looked at it), and the synthetic header has to be
	// genuinely undeclared or the two arms collapse into one.
	table := profile.IdentityHeaders()
	if len(table) == 0 {
		t.Fatal("no profile declares an identity header, so Sanitize has nothing to remove and this test cannot fail")
	}
	if !slices.Contains(table, declared) {
		t.Fatalf("%s is not in IdentityHeaders() %v, so removing it would prove nothing about the ownership rule", declared, table)
	}
	if slices.Contains(table, synthetic) {
		t.Fatalf("%s is now a declared identity header, so it can no longer stand in for a header this gateway alone owns", synthetic)
	}

	t.Run("Sanitize", func(t *testing.T) {
		compiled, err := (&profile.Gateway{TrustedPeers: loopbackCIDRs, UsernameHeader: synthetic}).Compile()
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}

		h := http.Header{}
		h.Set(synthetic, "operator")
		h.Set(declared, "attacker")
		h.Set(benign, "keep-me")
		compiled.Sanitize(h, "127.0.0.1:5000")

		if h.Get(synthetic) != "operator" {
			t.Error("the trusted gateway's own identity header was stripped, so the gateway path cannot work at all")
		}
		if got := h.Get(declared); got != "" {
			t.Errorf("%s survived Sanitize as %q from a gateway that does not own it: a trusted gateway may assert the identity of ITS OWN profile and no other", declared, got)
		}
		if h.Get(benign) != "keep-me" {
			t.Error("Sanitize removed a header that is none of its business")
		}
	})

	// The same rule at the hop, not only on the method: a recording
	// upstream reports exactly which headers crossed NewUI when the edge
	// is configured with the synthetic gateway.
	t.Run("through the edge", func(t *testing.T) {
		recorder, seen := recordingUpstream(t)

		compiled, err := (&profile.Gateway{TrustedPeers: loopbackCIDRs, UsernameHeader: synthetic}).Compile()
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		upstream, err := url.Parse(recorder.URL)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		edge := httptest.NewServer(serve.NewUI(serve.UIConfig{
			Upstream: upstream,
			StaticFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ui")}},
			Gateway:  compiled,
		}))
		t.Cleanup(edge.Close)

		req := mustRequest(t, http.MethodGet, edge.URL+"/api/v1/backup-sets")
		req.Header.Set(synthetic, "operator")
		req.Header.Set(declared, "attacker")
		req.Header.Set(benign, "keep-me")
		if code, _, _ := do(t, req); code != http.StatusNoContent {
			t.Fatalf("the recording upstream answered %d, want 204; the request never crossed the hop so nothing below proves anything", code)
		}

		got := seen()
		if got == nil {
			t.Fatal("the recording upstream never ran")
		}
		if got.Get(synthetic) != "operator" {
			t.Errorf("%s = %q at the upstream, want %q: the edge dropped the header its own configured gateway owns", synthetic, got.Get(synthetic), "operator")
		}
		if v := got.Values(declared); len(v) != 0 {
			t.Errorf("%s crossed the proxy hop as %q, set by a gateway that owns a different profile's header", declared, v)
		}
		if got.Get(benign) != "keep-me" {
			t.Error("an unrelated header did not cross the hop, so the assertion above could pass for the wrong reason")
		}
	})
}

// recordingUpstream is the recording-upstream oracle several tests above
// build by hand: a server that captures the headers of the last request
// it was handed and answers 204. The returned func reads the capture
// safely from the test goroutine.
func recordingUpstream(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

// ---------------------------------------------------------------------
// The ENGINE hop's own strip (issue #87's review, M2)
// ---------------------------------------------------------------------
//
// Everything above this point attacks the edge. The engine's own
// StripUntrustedIdentity had no executing test at all: every engine-side
// identity test asserted a 401, which is the AUTHENTICATOR's refusal, and
// no test in this package ever observed the http.Header an engine handler
// was actually handed. The PR's stated property is the stronger one -
// gone before a handler, a middleware or a log line can observe it - and
// it is claimed for both constructors. A mutation run confirmed the gap:
// replacing NewEngine's returned chain with the un-stripped handler left
// the whole package green.
//
// The oracle here is a recording Authenticator. It is the first thing
// inside the engine that is handed the request's headers, so what it saw
// is what survived the strip.

// headerLog captures the http.Header an Authenticator was handed.
type headerLog struct {
	mu    sync.Mutex
	seen  http.Header
	calls int
}

func (l *headerLog) record(h http.Header) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if h != nil {
		l.seen = h.Clone()
	}
}

func (l *headerLog) observed(t *testing.T) http.Header {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls == 0 {
		t.Fatal("the recording authenticator was never called, so nothing below observed anything")
	}
	return l.seen
}

// recordingAuthenticator decorates another Authenticator, which is the
// shape gatewayOf's discarded type assertion used to answer nil for. It
// deliberately does NOT declare serve.IdentityBoundaryCarrier: that is
// what declaringAuthenticator below adds, and the difference between the
// two is the whole of the boundary-resolution finding.
type recordingAuthenticator struct {
	log  *headerLog
	next capabilities.Authenticator
}

func (a recordingAuthenticator) Authenticate(ctx context.Context, r capabilities.AuthRequest) (capabilities.AuthContext, error) {
	a.log.record(r.Headers)
	return a.next.Authenticate(ctx, r)
}

// declaringAuthenticator is recordingAuthenticator plus the declared
// boundary, i.e. what a real decorator (audit, metrics, rate limiting, a
// gateway with a local fallback) is expected to implement.
type declaringAuthenticator struct {
	recordingAuthenticator
	boundary *profile.CompiledGateway
}

func (a declaringAuthenticator) IdentityBoundary() *profile.CompiledGateway { return a.boundary }

// wrappedAdapter is a PlatformAdapter with its Authenticator replaced.
type wrappedAdapter struct {
	capabilities.PlatformAdapter
	auth capabilities.Authenticator
}

func (a wrappedAdapter) Authenticator() capabilities.Authenticator { return a.auth }

// engineParts builds the pieces both the engine strip test and its own
// positive control need: a real backend, and a UGOS-or-generic adapter
// whose Authenticator records what it was handed.
func engineParts(t *testing.T, profileID string, trusted []string) (capabilities.PlatformAdapter, webhost.BackupServiceClient, *headerLog, string) {
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

	p := profileFor(t, profileID)
	header := profile.UGOS.Profile().Gateway.UsernameHeader
	var boundary *profile.CompiledGateway
	if p.Gateway != nil {
		p.Gateway.TrustedPeers = trusted
		header = p.Gateway.UsernameHeader
		if boundary, err = p.Gateway.Compile(); err != nil {
			t.Fatalf("Compile: %v", err)
		}
	}

	adapter, err := p.Adapter(profile.AdapterConfig{LocalAuth: authSvc.Authenticator()})
	if err != nil {
		t.Fatalf("Adapter: %v", err)
	}

	log := &headerLog{}
	rec := recordingAuthenticator{log: log, next: adapter.Authenticator()}
	var wrapped capabilities.PlatformAdapter
	if boundary != nil {
		wrapped = wrappedAdapter{PlatformAdapter: adapter, auth: declaringAuthenticator{recordingAuthenticator: rec, boundary: boundary}}
	} else {
		wrapped = wrappedAdapter{PlatformAdapter: adapter, auth: rec}
	}
	return wrapped, backend, log, header
}

// TestTheEngineStripsAnUntrustedIdentityBeforeItsAuthenticatorSeesIt is
// the engine-hop equivalent of
// TestTheEdgeStripsTheIdentityHeaderBeforeForwarding, with the same two
// kinds of control: a benign header proving the strip is targeted rather
// than a blanket wipe, and a run of the identical request through the
// SAME adapter with the strip removed, proving the observation can see
// the header when it is there.
func TestTheEngineStripsAnUntrustedIdentityBeforeItsAuthenticatorSeesIt(t *testing.T) {
	// untrustedCIDRs excludes loopback, which is what httptest connects
	// from, so this request is a direct-LAN caller as far as the engine
	// can tell.
	adapter, backend, log, header := engineParts(t, string(profile.UGOS), untrustedCIDRs)

	engine := httptest.NewServer(serve.NewEngine(serve.EngineConfig{Platform: adapter, Backend: backend}))
	t.Cleanup(engine.Close)

	req := mustRequest(t, http.MethodGet, engine.URL+"/api/v1/backup-sets")
	req.Header[strings.ToLower(header)] = []string{"admin"}
	req.Header.Set("X-Benign-Passthrough", "keep-me")
	code, _, body := do(t, req)
	if code != http.StatusUnauthorized {
		t.Fatalf("a forged %s from an untrusted peer got %d, want 401: %s", header, code, body)
	}
	if got := errorCode(t, body); got != "UNAUTHENTICATED" {
		t.Fatalf("error code = %q, want UNAUTHENTICATED; without it this 401 is not evidence the request reached the auth boundary (body %s)", got, body)
	}

	seen := log.observed(t)
	if v := seen.Values(header); len(v) != 0 {
		t.Errorf("the engine's authenticator was handed %s = %q; refused and never observed are different claims, and issue #87 asks for the second", header, v)
	}
	if seen.Get("X-Benign-Passthrough") != "keep-me" {
		t.Error("X-Benign-Passthrough did not survive, so the assertion above would pass on an engine that wiped every header")
	}

	// The positive control, and the mutation this test exists because of:
	// the same adapter and the same request behind webhost.NewRouter
	// alone, which is what NewEngine wraps. The header MUST arrive there,
	// or the observation above proves nothing about the strip.
	unstripped := httptest.NewServer(webhost.NewRouter(webhost.RouterConfig{
		Platform: adapter, Backend: backend, BinaryVersion: "test", Commit: "test",
	}))
	t.Cleanup(unstripped.Close)

	control := mustRequest(t, http.MethodGet, unstripped.URL+"/api/v1/backup-sets")
	control.Header[strings.ToLower(header)] = []string{"admin"}
	if _, _, body := do(t, control); errorCode(t, body) == "" && !strings.Contains(string(body), "backup_sets") {
		t.Fatalf("the control request produced a body this API did not write: %s", body)
	}
	if v := log.observed(t).Values(header); len(v) == 0 {
		t.Fatal("the same request with StripUntrustedIdentity removed also arrived without the identity header, so this test cannot tell a working strip from a broken oracle")
	}
}

// TestTheGenericProfileEngineStripsRatherThanOnlyRefusing. The generic
// profile has no gateway, so gatewayOf resolves nil and the engine's
// strip removes EVERY profile's identity header. The existing
// TestTheGenericProfileNeverBelievesAnIdentityHeader asserts a status
// code, which a refusing authenticator satisfies without anything having
// been stripped; this asserts the header never reached the authenticator
// at all, which is what the engine has to guarantee on the day somebody
// restarts that same deployment with --profile=ugos.
func TestTheGenericProfileEngineStripsRatherThanOnlyRefusing(t *testing.T) {
	adapter, backend, log, header := engineParts(t, string(profile.Generic), nil)

	engine := httptest.NewServer(serve.NewEngine(serve.EngineConfig{Platform: adapter, Backend: backend}))
	t.Cleanup(engine.Close)

	req := mustRequest(t, http.MethodGet, engine.URL+"/api/v1/backup-sets")
	req.Header.Set(header, "admin")
	req.Header.Set("X-Benign-Passthrough", "keep-me")
	if code, _, body := do(t, req); code != http.StatusUnauthorized {
		t.Fatalf("the generic profile answered %d to a forged %s, want 401: %s", code, header, body)
	}

	seen := log.observed(t)
	if v := seen.Values(header); len(v) != 0 {
		t.Errorf("a generic-profile engine handed its authenticator %s = %q; a profile with no gateway has no reason to carry another profile's identity header inwards", header, v)
	}
	if seen.Get("X-Benign-Passthrough") != "keep-me" {
		t.Error("X-Benign-Passthrough did not survive, so the assertion above would pass on an engine that wiped every header")
	}
}

// TestNewEngineRefusesANativeAuthAdapterWhoseBoundaryDoesNotResolve pins
// the other half of M2. gatewayOf used to be
// `gw, _ := platform.Authenticator().(*profile.CompiledGateway)`, whose
// discarded second value turned any decorated Authenticator into nil,
// which StripUntrustedIdentity reads as strip-everything: a gateway
// deployment that authenticates nobody, with no diagnostic anywhere. That
// is the exact operator experience serve-ui's own startup refusal exists
// to prevent one hop over, so the engine refuses at construction too.
func TestNewEngineRefusesANativeAuthAdapterWhoseBoundaryDoesNotResolve(t *testing.T) {
	adapter, backend, _, header := engineParts(t, string(profile.UGOS), loopbackCIDRs)

	// The undeclared decorator: same adapter, but its Authenticator is
	// wrapped in something that answers no boundary.
	undeclared := wrappedAdapter{
		PlatformAdapter: adapter,
		auth:            recordingAuthenticator{log: &headerLog{}, next: adapter.Authenticator()},
	}
	if !undeclared.Capabilities().NativeAuth {
		t.Fatal("the adapter under test does not declare NativeAuth, so the refusal this test is about cannot apply to it")
	}

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Error("NewEngine accepted a NativeAuth adapter whose trusted-peer boundary does not resolve; that deployment strips every identity header and authenticates nobody, silently")
			}
		}()
		_ = serve.NewEngine(serve.EngineConfig{Platform: undeclared, Backend: backend})
	}()

	// The positive control: the SAME decorator, declaring the boundary,
	// builds and authenticates end to end. Without it the refusal above
	// would be satisfied by a NewEngine that refuses every decorator.
	engine := httptest.NewServer(serve.NewEngine(serve.EngineConfig{Platform: adapter, Backend: backend}))
	t.Cleanup(engine.Close)

	req := mustRequest(t, http.MethodGet, engine.URL+"/api/v1/backup-sets")
	req.Header.Set(header, "operator")
	if code, _, body := do(t, req); code != http.StatusOK {
		t.Fatalf("a declared boundary on a decorated Authenticator answered %d, want 200: %s", code, body)
	}
}

// ---------------------------------------------------------------------
// The comma-joined identity (issue #87's review, M4)
// ---------------------------------------------------------------------

// TestACommaJoinedIdentityHeaderIsRefusedRatherThanResolved is
// TestDuplicateIdentityHeaderIsRefusedRatherThanResolved's other wire
// form. A proxy that CONCATENATES rather than adding a second field line
// sends one value, `attacker, operator`, which Header.Values reports as
// length 1 and which the multi-value refusal therefore never saw. The
// resolved username is written straight into Actor on a retention apply,
// a backup-set create and an operation submit, so a username naming two
// callers is the field an operator would read to attribute a destructive
// apply.
func TestACommaJoinedIdentityHeaderIsRefusedRatherThanResolved(t *testing.T) {
	s := newStack(t, loopbackCIDRs)

	for _, joined := range []string{"attacker, operator", "attacker;operator", "attacker operator"} {
		t.Run(joined, func(t *testing.T) {
			req := mustRequest(t, http.MethodGet, s.engineURL+"/api/v1/backup-sets")
			req.Header.Set(s.header, joined)
			code, _, body := do(t, req)
			if code != http.StatusUnauthorized {
				t.Fatalf("%s: %q authenticated with %d: %s\nan identity naming two callers was resolved to one rather than refused", s.header, joined, code, body)
			}
			if got := errorCode(t, body); got != "UNAUTHENTICATED" {
				t.Errorf("error code = %q, want UNAUTHENTICATED (body %s)", got, body)
			}
		})
	}

	// Positive control: an ordinary username from the same peer still
	// authenticates, so the refusals above are about the value and not
	// about the peer or a broken identity path.
	ok := mustRequest(t, http.MethodGet, s.engineURL+"/api/v1/backup-sets")
	ok.Header.Set(s.header, "operator")
	if code, _, body := do(t, ok); code != http.StatusOK {
		t.Fatalf("the single-name positive control returned %d, want 200: %s", code, body)
	}
}

// ---------------------------------------------------------------------
// Two hops, two peer sets (issue #87's review, M1)
// ---------------------------------------------------------------------
//
// Every composed test above this point runs both hops over httptest, so
// both peers are 127.0.0.1 and the two boundaries the whole fix is about
// are the same address in every proof. That suite would pass equally on a
// build where the edge sanitised against the engine's range, which is
// exactly the defect this section exists to catch: container/compose.yaml
// used to interpolate ONE variable into both hops, whose correct values
// the file's own comments describe as mutually exclusive.
//
// So this section gives the two hops genuinely different peers. httptest
// fixes RemoteAddr, so each hop is wrapped in a test-only middleware that
// rewrites it: the engine always sees the edge's internal address, and
// the edge sees whichever synthetic client the case is about. Nothing
// under test is modified, and both wrappers sit strictly OUTSIDE the
// constructor they front, so the strip is still the outermost thing in
// the handler chain it belongs to.

const (
	// The platform gateway, on the LAN in front of the edge.
	gatewayPeer = "192.168.10.5:41000"
	// An ordinary LAN client. It presents as the compose bridge address
	// because that is what Docker's userland port publishing does to
	// traffic arriving at a published port, which is the collapse
	// security.go documents and the reason the edge cannot simply trust
	// the internal network.
	lanClientPeer = "172.18.0.9:41000"
	// The edge itself, as the engine sees it on the internal network.
	edgeInternalPeer = "172.18.0.4:41000"
)

var (
	// What serve-ui's --trusted-gateway has to name: the GATEWAY.
	gatewayCIDRs = []string{"192.168.10.0/24"}
	// What serve's --trusted-upstream has to name: the internal network
	// the edge reaches the engine over.
	internalCIDRs = []string{"172.18.0.0/16"}
	// The only single value that lets a gateway deployment authenticate
	// at all, which is why one variable for both hops has no safe answer.
	unionCIDRs = []string{"192.168.10.0/24", "172.18.0.0/16"}
)

// peerSwitch rewrites the RemoteAddr of every request passing through it.
type peerSwitch struct{ addr atomic.Value }

func (p *peerSwitch) set(addr string) { p.addr.Store(addr) }

func (p *peerSwitch) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if addr, ok := p.addr.Load().(string); ok && addr != "" {
			r.RemoteAddr = addr
		}
		next.ServeHTTP(w, r)
	})
}

// fixedPeer is peerSwitch for a hop whose peer never changes.
func fixedPeer(addr string, next http.Handler) http.Handler {
	p := &peerSwitch{}
	p.set(addr)
	return p.wrap(next)
}

// splitStack is the two-service composition with the two hops on
// genuinely different networks.
type splitStack struct {
	edgeURL string
	header  string
	client  *peerSwitch
}

func newSplitStack(t *testing.T, engineTrusts, edgeTrusts []string) *splitStack {
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

	engine := httptest.NewServer(fixedPeer(edgeInternalPeer,
		serve.NewEngine(serve.EngineConfig{Platform: adapter, Backend: backend})))
	t.Cleanup(engine.Close)

	upstream, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatalf("parse engine URL: %v", err)
	}
	edgeGateway, err := (&profile.Gateway{TrustedPeers: edgeTrusts, UsernameHeader: p.Gateway.UsernameHeader}).Compile()
	if err != nil {
		t.Fatalf("compile edge gateway: %v", err)
	}

	client := &peerSwitch{}
	edge := httptest.NewServer(client.wrap(serve.NewUI(serve.UIConfig{
		Upstream: upstream,
		StaticFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ui")}},
		Gateway:  edgeGateway,
	})))
	t.Cleanup(edge.Close)

	return &splitStack{edgeURL: edge.URL, header: p.Gateway.UsernameHeader, client: client}
}

// asPeer drives one identity-carrying read from the given synthetic
// client address and returns the status.
func (s *splitStack) asPeer(t *testing.T, addr string) (int, []byte) {
	t.Helper()
	s.client.set(addr)
	req := mustRequest(t, http.MethodGet, s.edgeURL+"/api/v1/backup-sets")
	req.Header.Set(s.header, "operator")
	code, _, body := do(t, req)
	return code, body
}

// TestTheTwoHopsNeedTwoPeerSets is M1's composition proof, and the test
// that would have caught the shipped configuration.
//
// Each case configures the two hops the way one operator decision would,
// and asks the same two questions: can the platform gateway sign in, and
// is an unauthenticated LAN client still refused. A correct deployment
// answers yes and no. Every single-value configuration answers wrong to
// one of them, which is the finding stated as a table: the value that
// lets the engine authenticate is the value that makes the edge believe
// the LAN.
func TestTheTwoHopsNeedTwoPeerSets(t *testing.T) {
	cases := []struct {
		name         string
		engineTrusts []string
		edgeTrusts   []string
		wantGateway  int
		wantLAN      int
		why          string
	}{
		{
			name:         "two variables, each naming its own hop",
			engineTrusts: internalCIDRs,
			edgeTrusts:   gatewayCIDRs,
			wantGateway:  http.StatusOK,
			wantLAN:      http.StatusUnauthorized,
			why:          "the only configuration that both authenticates the gateway and refuses the LAN, and it needs two variables to express",
		},
		{
			name:         "one variable, set to the gateway range",
			engineTrusts: gatewayCIDRs,
			edgeTrusts:   gatewayCIDRs,
			wantGateway:  http.StatusUnauthorized,
			wantLAN:      http.StatusUnauthorized,
			why:          "the engine's only possible peer is the edge, which is not in the gateway's range, so nobody can sign in",
		},
		{
			name:         "one variable, set to the internal range",
			engineTrusts: internalCIDRs,
			edgeTrusts:   internalCIDRs,
			wantGateway:  http.StatusUnauthorized,
			wantLAN:      http.StatusOK,
			why:          "the edge now believes the internal network, which under userland port publishing is what a LAN client presents as: the forgery this whole issue is about, with the strip in place and every one of its own tests passing",
		},
		{
			name:         "one variable, set to the union of both",
			engineTrusts: unionCIDRs,
			edgeTrusts:   unionCIDRs,
			wantGateway:  http.StatusOK,
			wantLAN:      http.StatusOK,
			why:          "the only single value that lets the gateway authenticate, and it authenticates the LAN client too",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSplitStack(t, tc.engineTrusts, tc.edgeTrusts)

			if code, body := s.asPeer(t, gatewayPeer); code != tc.wantGateway {
				t.Errorf("the platform gateway got %d, want %d (%s): %s", code, tc.wantGateway, tc.why, body)
			}
			if code, body := s.asPeer(t, lanClientPeer); code != tc.wantLAN {
				t.Errorf("an unauthenticated LAN client got %d, want %d (%s): %s", code, tc.wantLAN, tc.why, body)
			}
		})
	}
}

// TestTheEdgeAndTheEngineDoNotShareOneBoundary is the same finding stated
// as the property rather than the table, and it is the assertion that
// fails on a build where one hop sanitises against the other's range.
//
// The two ranges below are disjoint, so a hop using the wrong one cannot
// accidentally agree with the right answer, which is precisely what
// loopback-everywhere composition could never rule out.
func TestTheEdgeAndTheEngineDoNotShareOneBoundary(t *testing.T) {
	s := newSplitStack(t, internalCIDRs, gatewayCIDRs)

	code, body := s.asPeer(t, gatewayPeer)
	if code != http.StatusOK {
		t.Fatalf("the correctly split configuration answered %d, want 200: %s\n"+
			"the edge trusts %v and the engine trusts %v; if this fails, one hop is being evaluated against the other's peer set",
			code, body, gatewayCIDRs, internalCIDRs)
	}
	if !strings.Contains(string(body), "backup_sets") {
		t.Fatalf("the authenticated response is not the backup-set list: %s", body)
	}

	// The negative half, on the same listeners: the address the ENGINE
	// trusts is not an address the EDGE trusts, so a client arriving from
	// the internal range is stripped and refused.
	if code, body := s.asPeer(t, lanClientPeer); code != http.StatusUnauthorized {
		t.Fatalf("a client from the engine's own trusted range authenticated through the edge with %d, want 401: %s\n"+
			"the edge is trusting the engine's peer set, which is the single-variable configuration this test exists to forbid", code, body)
	}
}
