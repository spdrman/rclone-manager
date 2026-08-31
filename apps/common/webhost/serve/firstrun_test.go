// firstrun_test.go is issue #176's acceptance test, and it is the only
// test in this repository that starts where a real operator starts: an
// empty state directory and no config.yaml. Every other suite writes a
// valid configuration as fixture setup, which is precisely why "serve
// refuses to start until a config exists" stayed invisible for as long as
// it did.
//
// It drives the whole journey a NAS administrator makes after installing
// the canonical image from an app store, over real HTTP, with a real
// cookie jar: open the UI, enroll with the single-use token from the
// container log, walk the setup flow, and end with a configured,
// serviceable instance in the SAME process - no shell, no hand-written
// YAML, no restart.
package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
	"github.com/spdrman/rclone-manager/apps/common/webhost"
	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
	"github.com/spdrman/rclone-manager/core/service"
)

// firstRunFixtureKey is a throwaway, unencrypted ed25519 private key
// generated with `ssh-keygen -t ed25519 -N ""`, purely so the setup flow
// has real, parseable key material to import. It authorizes access to
// nothing: no server anywhere trusts its public half.
const firstRunFixtureKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACBp+GVRkoZ43uOoQJDQigP4BrozoP43k7AmgbnQseFAOwAAAKBQ6XopUOl6
KQAAAAtzc2gtZWQyNTUxOQAAACBp+GVRkoZ43uOoQJDQigP4BrozoP43k7AmgbnQseFAOw
AAAEDKq1zBgGm7WYsbJ145K1QtwpfB3vkKU28PczLWa0D7KWn4ZVGShnje46hAkNCKA/gG
ujOg/jeTsCaBudCx4UA7AAAAF2JhY2t1cHNldHMtdGVzdC1maXh0dXJlAQIDBAUG
-----END OPENSSH PRIVATE KEY-----
`

// freshInstall is the app-store install an operator actually receives:
// a config directory with nothing in it, and a state directory that does
// not exist yet.
type freshInstall struct {
	configPath string
	statePath  string
	harness    *engineHarness
}

func newFreshInstall(t *testing.T) *freshInstall {
	t.Helper()

	root := t.TempDir()
	configPath := filepath.Join(root, "config", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	statePath := filepath.Join(root, "state", "state.db")

	// Positive control on the fixture: if either of these already exists,
	// this test is not exercising a fresh install at all.
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: %s already exists (err = %v)", configPath, err)
	}
	if _, err := os.Stat(filepath.Dir(statePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: %s already exists (err = %v)", filepath.Dir(statePath), err)
	}

	firstRun, err := service.NewFirstRun(service.FirstRunDefaults{
		ConfigPath:    configPath,
		StateDatabase: statePath,
	})
	if err != nil {
		t.Fatalf("service.NewFirstRun: %v", err)
	}

	authSvc, err := local.New(local.Config{StorePath: filepath.Join(root, "local-auth.json")})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	engine, err := serve.NewFirstRunEngine(serve.EngineConfig{
		Platform:              testPlatformAdapter{auth: authSvc},
		AuthRoutes:            authSvc.Handler(),
		TrustForwardedHeaders: authSvc.TrustForwardedHeaders(),
		FirstRun:              firstRun,
		Activate: func(ctx context.Context) (webhost.BackupServiceClient, func() error, error) {
			return service.Open(ctx, configPath)
		},
		BinaryVersion: "test",
		Commit:        "testcommit",
	})
	if err != nil {
		t.Fatalf("serve.NewFirstRunEngine: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("FirstRunEngine.Close: %v", err)
		}
	})

	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	return &freshInstall{
		configPath: configPath,
		statePath:  statePath,
		harness:    &engineHarness{server: srv, auth: authSvc, client: &http.Client{Jar: jar}},
	}
}

func (f *freshInstall) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, f.harness.server.URL+path, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		req.Header.Set(local.CSRFHeaderName, csrfToken(t, f.harness.client, f.harness.server.URL))
	}
	resp, err := f.harness.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// TestFreshInstall_ServesAFirstRunExperienceInsteadOfRefusingToStart is
// issue #176's behavioral contract, end to end:
//
//	GIVEN an empty state directory and no config.yaml
//	WHEN the administrator opens the web UI at the published port
//	THEN the application is listening and serves a setup flow that
//	     creates a valid configuration, rather than having refused to
//	     start.
func TestFreshInstall_ServesAFirstRunExperienceInsteadOfRefusingToStart(t *testing.T) {
	f := newFreshInstall(t)

	// The listener is up. Before this issue, the process would have
	// exited at service.Open and there would be nothing to talk to.
	status, _ := f.do(t, http.MethodGet, "/health/live", "")
	if status != http.StatusOK {
		t.Fatalf("/health/live status = %d, want 200; the process is not serving a fresh install", status)
	}

	// It is honest about not being ready: #157's flag, reused rather than
	// duplicated.
	status, _ = f.do(t, http.MethodGet, "/health/ready", "")
	if status != http.StatusServiceUnavailable {
		t.Errorf("/health/ready status = %d, want 503 on an unconfigured instance", status)
	}

	// Nothing is configurable before enrollment.
	status, _ = f.do(t, http.MethodGet, "/api/v1/system/first-run", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("/api/v1/system/first-run status = %d before enrolling, want 401", status)
	}

	enrollAndLogIn(t, f.harness, f.harness.client, f.harness.server.URL)

	status, body := f.do(t, http.MethodGet, "/api/v1/system/first-run", "")
	if status != http.StatusOK {
		t.Fatalf("/api/v1/system/first-run status = %d after enrolling, want 200 (%s)", status, body)
	}
	var firstRunStatus struct {
		Configured bool `json:"configured"`
	}
	if err := json.Unmarshal(body, &firstRunStatus); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if firstRunStatus.Configured {
		t.Fatal("configured = true on a fresh install")
	}

	// The setup flow's own steps: import a key, then write the first
	// configuration.
	keyBody, _ := json.Marshal(map[string]string{"private_key_pem": firstRunFixtureKey})
	status, body = f.do(t, http.MethodPost, "/api/v1/ssh-keys", string(keyBody))
	if status != http.StatusCreated {
		t.Fatalf("POST /api/v1/ssh-keys status = %d, want 201 (%s)", status, body)
	}
	var keyRef struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &keyRef); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	setup, _ := json.Marshal(map[string]any{
		"name":                "nightly",
		"host":                "prod-db-01.internal",
		"port":                22,
		"user":                "backup-agent",
		"ssh_key_id":          keyRef.ID,
		"known_hosts_line":    "prod-db-01.internal ssh-ed25519 AAAAfaketest",
		"remote_path":         "/backups/postgresql",
		"local_path":          filepath.Join(t.TempDir(), "backups"),
		"include":             []string{"*.dump"},
		"completion_strategy": "marker",
	})
	status, body = f.do(t, http.MethodPost, "/api/v1/system/first-run", string(setup))
	if status != http.StatusCreated {
		t.Fatalf("POST /api/v1/system/first-run status = %d, want 201 (%s)", status, body)
	}
	var created struct {
		BackupSet struct {
			ID string `json:"id"`
		} `json:"backup_set"`
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if created.BackupSet.ID != "api/nightly" {
		t.Errorf("backup_set.id = %q, want %q", created.BackupSet.ID, "api/nightly")
	}
	if created.RestartRequired {
		t.Error("restart_required = true; the operator was asked for a restart the whole issue exists to avoid")
	}

	// The config and the journal now exist, written by the application
	// rather than by an operator over SSH.
	if _, err := os.Stat(f.configPath); err != nil {
		t.Errorf("config was not written to %s: %v", f.configPath, err)
	}
	if _, err := os.Stat(f.statePath); err != nil {
		t.Errorf("state database was not created at %s: %v", f.statePath, err)
	}

	// And the same process, with no restart, now serves the application.
	status, body = f.do(t, http.MethodGet, "/health/ready", "")
	if status != http.StatusOK {
		t.Errorf("/health/ready status = %d after setup, want 200 (%s)", status, body)
	}
	status, body = f.do(t, http.MethodGet, "/api/v1/backup-sets", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/backup-sets status = %d after setup, want 200 (%s)", status, body)
	}
	var sets struct {
		BackupSets []struct {
			ID string `json:"id"`
		} `json:"backup_sets"`
	}
	if err := json.Unmarshal(body, &sets); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(sets.BackupSets) != 1 || sets.BackupSets[0].ID != "api/nightly" {
		t.Errorf("backup sets after setup = %+v, want exactly api/nightly", sets.BackupSets)
	}

	// Setup is a one-time door, and it is now shut.
	status, body = f.do(t, http.MethodPost, "/api/v1/system/first-run", string(setup))
	if status != http.StatusConflict {
		t.Errorf("second POST /api/v1/system/first-run status = %d, want 409 (%s)", status, body)
	}
}

// TestFreshInstall_RefusesBackupWorkUntilItIsConfigured is the other half
// of the same decision: the instance is up, but every route that would
// act on backup data refuses while there is no configuration behind it,
// rather than behaving unpredictably against a service that does not
// exist.
func TestFreshInstall_RefusesBackupWorkUntilItIsConfigured(t *testing.T) {
	f := newFreshInstall(t)
	enrollAndLogIn(t, f.harness, f.harness.client, f.harness.server.URL)

	refused := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/backup-sets", ""},
		{http.MethodPost, "/api/v1/backup-sets", "{}"},
		{http.MethodPost, "/api/v1/operations", "{}"},
		{http.MethodGet, "/api/v1/backup-sets/api/nightly/retention/preview", ""},
		{http.MethodPost, "/api/v1/backup-sets/api/nightly/retention/apply", "{}"},
		{http.MethodGet, "/api/v1/settings", ""},
		{http.MethodPatch, "/api/v1/settings", "{}"},
		{http.MethodGet, "/api/v1/system/storage", ""},
	}
	for _, tc := range refused {
		status, body := f.do(t, tc.method, tc.path, tc.body)
		if status != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503 while unconfigured (%s)", tc.method, tc.path, status, body)
			continue
		}
		var apiErr struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &apiErr); err != nil {
			t.Errorf("%s %s: body is not an API error: %v (%s)", tc.method, tc.path, err, body)
			continue
		}
		if apiErr.Error.Code != "NOT_CONFIGURED" {
			t.Errorf("%s %s: error code = %q, want NOT_CONFIGURED", tc.method, tc.path, apiErr.Error.Code)
		}
	}
}

// TestExistingInstall_StillRefusesToStartOnAnInvalidConfig is the
// negative control for the whole first-run decision. An absent
// configuration leads somewhere useful; a configuration that exists and
// does not validate is still a hard startup failure, because running
// degraded against a broken configuration, or offering to overwrite it,
// is worse than refusing.
func TestExistingInstall_StillRefusesToStartOnAnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("poll_interval: 0s\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := service.Open(context.Background(), configPath)
	if err == nil {
		t.Fatal("service.Open accepted an invalid config")
	}
	if errors.Is(err, service.ErrConfigAbsent) {
		t.Errorf("service.Open reported an invalid config as absent (%v); a first-run flow would then offer to overwrite it", err)
	}
}

// TestTheSetupSurfaceStripsAnUntrustedIdentityToo pins the half of issue
// #87's strip that only #176 could put at risk.
//
// #87 wrapped NewEngine's composition in StripUntrustedIdentity. #176
// then moved that composition into newEngineHandler so a FirstRunEngine
// could build the same surface twice, once unconfigured and once
// activated. Merging the two is exactly where the strip can be left
// behind on the setup path, and it would be invisible: every existing
// forge-and-replay test in redteam_test.go builds through NewEngine, so
// all of them would stay green while an UNCONFIGURED instance handed a
// forged identity header straight to its authenticator. That is the worst
// moment for it, because an instance with no configuration yet is one
// where naming yourself an admin has nothing to be refused by.
//
// The two cases share one request and differ only in whether the peer
// that sent it is inside the adapter's trusted set, which is what makes
// the negative meaningful: the trusted case proves this path reaches the
// authenticator with the header intact, so the untrusted case's absence
// is the strip and not a route that never authenticated.
func TestTheSetupSurfaceStripsAnUntrustedIdentityToo(t *testing.T) {
	forge := func(t *testing.T, trusted []string) (http.Header, string) {
		t.Helper()
		adapter, _, log, header := engineParts(t, string(profile.UGOS), trusted)

		root := t.TempDir()
		configPath := filepath.Join(root, "config", "config.yaml")
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		firstRun, err := service.NewFirstRun(service.FirstRunDefaults{
			ConfigPath:    configPath,
			StateDatabase: filepath.Join(root, "state", "state.db"),
		})
		if err != nil {
			t.Fatalf("service.NewFirstRun: %v", err)
		}

		engine, err := serve.NewFirstRunEngine(serve.EngineConfig{
			Platform: adapter,
			FirstRun: firstRun,
			Activate: func(ctx context.Context) (webhost.BackupServiceClient, func() error, error) {
				return service.Open(ctx, configPath)
			},
			BinaryVersion: "test",
			Commit:        "testcommit",
		})
		if err != nil {
			t.Fatalf("serve.NewFirstRunEngine: %v", err)
		}
		t.Cleanup(func() { _ = engine.Close() })

		srv := httptest.NewServer(engine)
		t.Cleanup(srv.Close)

		// Deliberately NOT the first-run route: an authenticated route, so
		// the adapter's authenticator is what sees (or does not see) the
		// header. The instance is unconfigured, so this cannot succeed;
		// what it can do is reach the auth boundary, which is the whole
		// question.
		req := mustRequest(t, http.MethodGet, srv.URL+"/api/v1/backup-sets")
		req.Header[strings.ToLower(header)] = []string{"admin"}
		req.Header.Set("X-Benign-Passthrough", "keep-me")
		do(t, req)
		return log.observed(t), header
	}

	// The oracle first: with the caller's own address inside the trusted
	// set, the header MUST arrive. If this ever stops holding, the
	// assertion below is proving nothing.
	seen, header := forge(t, loopbackCIDRs)
	if v := seen.Values(header); len(v) == 0 {
		t.Fatalf("a %s from a TRUSTED peer never reached the setup surface's authenticator, so this test cannot tell a working strip from a route that simply does not authenticate", header)
	}

	// untrustedCIDRs excludes loopback, which is what httptest connects
	// from, so the same request is now a direct-LAN forgery.
	seen, header = forge(t, untrustedCIDRs)
	if v := seen.Values(header); len(v) != 0 {
		t.Errorf("the setup surface handed its authenticator %s = %q; an unconfigured instance is exactly where a forged identity must be gone rather than merely refused (issue #87 via #176)", header, v)
	}
	if seen.Get("X-Benign-Passthrough") != "keep-me" {
		t.Error("X-Benign-Passthrough did not survive either, so the assertion above would pass on a setup surface that wiped every header")
	}
}
