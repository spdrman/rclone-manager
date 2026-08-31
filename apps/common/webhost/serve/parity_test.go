// parity_test.go is issue #167's profile-parity suite. A runtime profile
// may change a trusted native authentication gateway, a notification
// bridge, a launch bridge and what capabilities are reported. It may not
// change what a lifecycle, retention or validation request answers. That
// distinction is the whole difference between a platform integration and a
// fork, and it is asserted here rather than described.
//
// One backend, two engines. Both profiles are wired over the SAME
// *service.BackupService instance, so the only variable between the two
// HTTP surfaces is the profile itself: two independently opened backends
// would differ in temp paths and revision stamps, and normalising that
// away is exactly the kind of leniency that turns a parity suite into a
// formality.
package serve_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
	"github.com/spdrman/rclone-manager/core/service"
)

// gatewayIdentityHeader is the header the synthetic trusted gateway sets.
// It has to match the ugos profile's own declared header, and the test
// reads it from the profile rather than restating it.
func profileFor(t *testing.T, id string) profile.Profile {
	t.Helper()
	p, err := profile.Lookup(id)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", id, err)
	}
	if p.Gateway != nil {
		// httptest connects over loopback, so loopback IS the synthetic
		// trusted peer here. Every other source address is the synthetic
		// untrusted one.
		p.Gateway.TrustedPeers = []string{"127.0.0.0/8", "::1/128"}
	}
	return p
}

type profileHarness struct {
	profile profile.Profile
	server  *httptest.Server
	client  *http.Client
	auth    *local.Service
}

// newProfileHarnesses opens one backend and stands up one engine per
// profile over it.
func newProfileHarnesses(t *testing.T) []*profileHarness {
	t.Helper()

	configPath := writeTestConfig(t)
	backend, cleanup, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("BackupService cleanup: %v", err)
		}
	})

	authSvc, err := local.New(local.Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	var out []*profileHarness
	for _, id := range profile.IDs() {
		p := profileFor(t, string(id))
		adapter, err := p.Adapter(profile.AdapterConfig{LocalAuth: authSvc.Authenticator()})
		if err != nil {
			t.Fatalf("profile %q Adapter: %v", id, err)
		}

		cfg := serve.EngineConfig{
			Platform:      adapter,
			Backend:       backend,
			BinaryVersion: "test",
			Commit:        "testcommit",
		}
		// Only a local-account profile mounts login/enrol routes at all;
		// a gateway profile has no login surface of its own, which is
		// itself one of the four things a profile is allowed to change.
		if p.Gateway == nil {
			cfg.AuthRoutes = authSvc.Handler()
		}

		srv := httptest.NewServer(serve.NewEngine(cfg))
		t.Cleanup(srv.Close)

		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatalf("cookiejar.New: %v", err)
		}
		out = append(out, &profileHarness{profile: p, server: srv, client: &http.Client{Jar: jar}, auth: authSvc})
	}

	if len(out) < 2 {
		t.Fatalf("the parity suite ran against %d profile(s); it proves nothing below two", len(out))
	}
	return out
}

// authenticate establishes a session the way this profile is meant to:
// local enrolment for a local-account profile, a gateway-set identity
// header for a gateway profile.
func (h *profileHarness) authenticate(t *testing.T) {
	t.Helper()
	if h.profile.Gateway == nil {
		enrollAndLogIn(t, &engineHarness{auth: h.auth}, h.client, h.server.URL)
		return
	}
	// A gateway profile authenticates per request; nothing to establish
	// up front. get() below sets the header.
}

func (h *profileHarness) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if g := h.profile.Gateway; g != nil {
		req.Header.Set(g.UsernameHeader, "operator")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, body
}

// parityRoutes are the lifecycle, retention and validation reads whose
// answers may not depend on the profile.
//
// volatile names the top-level JSON fields a route regenerates on every
// call regardless of who asked. Only the retention preview has any: it
// mints a fresh single-use plan (#96's stale-plan rejection depends on
// that), so its plan_id and expires_at differ between two consecutive
// calls to the SAME profile. Normalising them is not leniency about the
// profile boundary: normalizeVolatile below fails if a named field is
// absent or empty, so the list cannot quietly grow into "ignore the
// interesting part", and every verdict, count and revision in that same
// response is still compared byte for byte.
var parityRoutes = []struct {
	name     string
	path     string
	volatile []string
}{
	{name: "lifecycle: the backup-set list", path: "/api/v1/backup-sets"},
	{name: "lifecycle: one backup set's detail", path: "/api/v1/backup-sets/production/postgres-primary"},
	{name: "retention: the settings that drive it", path: "/api/v1/settings"},
	{
		name:     "retention: an immutable preview",
		path:     "/api/v1/backup-sets/production/postgres-primary/retention/preview",
		volatile: []string{"plan_id", "expires_at"},
	},
	{name: "validation: the registered validator catalog", path: "/api/v1/validators"},
	{name: "storage pressure", path: "/api/v1/system/storage"},
}

// normalizeVolatile blanks the named top-level fields and returns the
// re-encoded document, refusing if a named field is missing or empty.
func normalizeVolatile(t *testing.T, body []byte, fields []string) string {
	t.Helper()
	if len(fields) == 0 {
		return string(body)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	for _, f := range fields {
		v, ok := doc[f]
		if !ok {
			t.Fatalf("%q is listed as volatile but the response does not carry it, so the normalisation is describing a field that no longer exists: %s", f, body)
		}
		if s, isString := v.(string); isString && s == "" {
			t.Fatalf("%q is listed as volatile but came back empty, so blanking it hides nothing and proves nothing: %s", f, body)
		}
		doc[f] = "<volatile>"
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out)
}

func TestProfileSelectionIsInertForTheBackupDomain(t *testing.T) {
	harnesses := newProfileHarnesses(t)
	for _, h := range harnesses {
		h.authenticate(t)
	}

	for _, route := range parityRoutes {
		t.Run(route.name, func(t *testing.T) {
			wantCode, wantBody := harnesses[0].get(t, route.path)
			if wantCode != http.StatusOK {
				t.Fatalf("%s under profile %q returned %d, so this route contributes nothing to the parity proof: %s",
					route.path, harnesses[0].profile.ID, wantCode, wantBody)
			}
			want := normalizeVolatile(t, wantBody, route.volatile)
			for _, h := range harnesses[1:] {
				gotCode, gotBody := h.get(t, route.path)
				if gotCode != wantCode {
					t.Errorf("%s: profile %q answered %d, profile %q answered %d",
						route.path, harnesses[0].profile.ID, wantCode, h.profile.ID, gotCode)
				}
				if got := normalizeVolatile(t, gotBody, route.volatile); got != want {
					t.Errorf("%s differs between profiles %q and %q\n  %s\n  %s",
						route.path, harnesses[0].profile.ID, h.profile.ID, want, got)
				}
			}
		})
	}
}

// TestProfileParityWouldNoticeADifference is the positive control. The
// suite above compares whole response bodies; if that comparison were
// somehow inert, /system/capabilities would compare equal too, and it must
// not: reporting what the host platform can do is one of the four things a
// profile IS allowed to change.
func TestProfileParityWouldNoticeADifference(t *testing.T) {
	harnesses := newProfileHarnesses(t)
	for _, h := range harnesses {
		h.authenticate(t)
	}

	seen := map[string]string{}
	for _, h := range harnesses {
		code, body := h.get(t, "/api/v1/system/capabilities")
		if code != http.StatusOK {
			t.Fatalf("capabilities under %q returned %d: %s", h.profile.ID, code, body)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal capabilities: %v", err)
		}
		platform, _ := got["platform"].(string)
		if platform != string(h.profile.PlatformID) {
			t.Errorf("profile %q reports platform %q, want %q", h.profile.ID, platform, h.profile.PlatformID)
		}
		seen[string(h.profile.ID)] = string(body)
	}

	if len(seen) < 2 {
		t.Fatal("fewer than two profiles reported capabilities")
	}
	var first string
	for _, body := range seen {
		if first == "" {
			first = body
			continue
		}
		if body == first {
			t.Fatalf("every profile reported identical capabilities (%s), so the comparison used by the parity suite cannot distinguish two profiles at all", first)
		}
	}
}

// TestSpoofedIdentityCannotReachAProtectedRouteThroughTheEngine is the
// end-to-end half of the trusted-gateway boundary: not the authenticator
// in isolation, but a real request over a real listener.
func TestSpoofedIdentityCannotReachAProtectedRouteThroughTheEngine(t *testing.T) {
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

	p := profileFor(t, "ugos")
	// The synthetic UNTRUSTED peer: loopback is what httptest actually
	// connects from, so declaring a trusted range that excludes loopback
	// is how a direct-LAN caller is simulated without a second machine.
	p.Gateway.TrustedPeers = []string{"10.99.0.0/16"}
	adapter, err := p.Adapter(profile.AdapterConfig{LocalAuth: authSvc.Authenticator()})
	if err != nil {
		t.Fatalf("Adapter: %v", err)
	}
	srv := httptest.NewServer(serve.NewEngine(serve.EngineConfig{Platform: adapter, Backend: backend}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/backup-sets", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(p.Gateway.UsernameHeader, "admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a forged identity header from an untrusted peer returned %d, want 401: %s", resp.StatusCode, body)
	}

	// The positive control on the same listener shape: the identical
	// request from the peer the profile actually trusts must succeed, so
	// the 401 above is about trust and not about everything being refused.
	p.Gateway.TrustedPeers = []string{"127.0.0.0/8", "::1/128"}
	trustedAdapter, err := p.Adapter(profile.AdapterConfig{LocalAuth: authSvc.Authenticator()})
	if err != nil {
		t.Fatalf("Adapter: %v", err)
	}
	trusted := httptest.NewServer(serve.NewEngine(serve.EngineConfig{Platform: trustedAdapter, Backend: backend}))
	t.Cleanup(trusted.Close)

	req2, err := http.NewRequest(http.MethodGet, trusted.URL+"/api/v1/backup-sets", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req2.Header.Set(p.Gateway.UsernameHeader, "admin")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("the gateway-authenticated positive control returned %d, want 200: %s", resp2.StatusCode, body2)
	}
}
