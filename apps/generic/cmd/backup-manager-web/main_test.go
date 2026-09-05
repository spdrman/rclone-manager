// The command dispatch and the flag surface, tested through run() rather
// than through the process.
//
// The refusals are what most of this file is. A binary that starts with a
// misconfiguration and discovers it later is a binary an operator finds
// broken at the worst moment, so an unsupported auth mode and a
// configuration that does not validate both have to stop the process
// before it binds anything.
//
// One case is the opposite, and it is the one that changed the shape of
// the fixture: a missing configuration file is no longer a refusal at all.
// It is the first-run state, and serve is expected to start, listen and
// offer the setup flow. That is why the invalid-config fixture writes a
// file that parses and fails validation rather than pointing at a path
// that does not exist, and why it also redirects the auth store: with the
// store opened before the configuration, leaving it at the container
// default would make a failure ambiguous between the two.
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// invalidConfigArgs is the "serve refuses and exits" fixture every test
// below that drives cmdServe needs, and it changed shape with issue #176.
//
// It used to be a path that did not exist. That is now the FIRST-RUN
// state: serve starts anyway, listens, and offers the setup flow, so a
// test built on a missing path no longer exits at all — it binds a port
// and blocks until the process is signalled, which is exactly how this
// helper came to exist. A configuration that EXISTS and does not validate
// is the refusal that remains, and it is what these tests want.
//
// --auth-store is redirected into the same temp directory on purpose. The
// local-auth store is opened BEFORE the configuration now (main.go's own
// comment on why enrollment has to come first), so leaving it at the
// container default would make a failure here ambiguous: the exit code
// would be the same whether serve refused the config or could not open a
// store under /data. Pointing it somewhere writable keeps the assertion
// about the config and nothing else.
func invalidConfigArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	// Parses as YAML, fails config.Validate: no sources, no state
	// database, a non-positive poll interval.
	if err := os.WriteFile(configPath, []byte("poll_interval: 0s\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	args := []string{"serve", "--config", configPath, "--auth-store", filepath.Join(dir, "local-auth.json")}
	return append(args, extra...)
}

func TestRun_NoArgsPrintsUsageAndFails(t *testing.T) {
	if got := run(nil); got != 2 {
		t.Errorf("run(nil) = %d, want 2", got)
	}
}

func TestRun_UnknownCommandFails(t *testing.T) {
	if got := run([]string{"not-a-real-command"}); got != 2 {
		t.Errorf("run([\"not-a-real-command\"]) = %d, want 2", got)
	}
}

func TestCmdServe_RejectsAnUnsupportedAuthMode(t *testing.T) {
	if got := run([]string{"serve", "--auth-mode", "ugos"}); got != 2 {
		t.Errorf("run([\"serve\", \"--auth-mode\", \"ugos\"]) = %d, want 2 (only \"local\" is implemented)", got)
	}
}

// TestCmdServe_FailsFastOnAnInvalidConfigFile is the half of the old
// "fails fast on a missing config" contract that issue #176 kept. A
// configuration that exists and does not validate is still a hard startup
// failure, deliberately: it is an operator's declared intent, and running
// degraded against it, or offering to replace it in a setup flow, is
// worse than refusing.
//
// The other half of that old contract is gone on purpose, and
// TestCmdServe_ServesTheFirstRunFlowRatherThanExitingOnAMissingConfig
// below is what replaced it.
func TestCmdServe_FailsFastOnAnInvalidConfigFile(t *testing.T) {
	if got := run(invalidConfigArgs(t)); got == 0 {
		t.Error("run(serve) against an invalid config file = 0, want a non-zero exit code")
	}
}

// TestCmdServe_ServesTheFirstRunFlowRatherThanExitingOnAMissingConfig is
// issue #176's contract at this binary's own boundary: a config path that
// does not exist is a fresh install, not a misconfiguration, and cmdServe
// has to build a first-run engine rather than return.
//
// It cannot simply call run(): that blocks in ListenAndServe until the
// process is signalled, which is the whole point. So it asserts the same
// thing one step in, on the branch cmdServe takes — core/service.Open
// reports ErrConfigAbsent for a missing file and does not for a broken
// one — with the invalid case right beside it as the control that proves
// this is a real distinction and not a constant.
func TestCmdServe_ServesTheFirstRunFlowRatherThanExitingOnAMissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "config.yaml")
	_, _, err := service.Open(context.Background(), missing)
	if !errors.Is(err, service.ErrConfigAbsent) {
		t.Fatalf("service.Open against a missing config = %v, want an error matching ErrConfigAbsent; cmdServe would exit instead of serving the setup flow", err)
	}

	// The packaged mount is a DIRECTORY (#196), so --config naming it is
	// the spelling an operator is invited to type. An empty one is the
	// same fresh install, and it has to reach the same branch: statting
	// the directory rather than the file inside it finds something
	// present on a completely empty install, and this binary would exit
	// at startup on the one deployment shape issue #176 is for.
	emptyDir := t.TempDir()
	_, _, err = service.Open(context.Background(), emptyDir)
	if !errors.Is(err, service.ErrConfigAbsent) {
		t.Fatalf("service.Open against an empty config DIRECTORY = %v, want an error matching ErrConfigAbsent", err)
	}

	invalid := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(invalid, []byte("poll_interval: 0s\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, _, err = service.Open(context.Background(), invalid)
	if err == nil {
		t.Fatal("service.Open accepted an invalid config")
	}
	if errors.Is(err, service.ErrConfigAbsent) {
		t.Errorf("service.Open reported an invalid config as absent (%v); cmdServe would offer to overwrite a real deployment's configuration", err)
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("BACKUP_MANAGER_WEB_TEST_VAR", "")
	if got := envOrDefault("BACKUP_MANAGER_WEB_TEST_VAR", "fallback"); got != "fallback" {
		t.Errorf("envOrDefault(unset) = %q, want %q", got, "fallback")
	}

	t.Setenv("BACKUP_MANAGER_WEB_TEST_VAR", "set-value")
	if got := envOrDefault("BACKUP_MANAGER_WEB_TEST_VAR", "fallback"); got != "set-value" {
		t.Errorf("envOrDefault(set) = %q, want %q", got, "set-value")
	}
}

func TestEnvBoolOrDefault(t *testing.T) {
	const key = "BACKUP_MANAGER_WEB_TEST_BOOL_VAR"

	t.Setenv(key, "")
	if got := envBoolOrDefault(key, false); got != false {
		t.Errorf("envBoolOrDefault(unset, false) = %v, want false", got)
	}
	if got := envBoolOrDefault(key, true); got != true {
		t.Errorf("envBoolOrDefault(unset, true) = %v, want true", got)
	}

	t.Setenv(key, "true")
	if got := envBoolOrDefault(key, false); got != true {
		t.Errorf("envBoolOrDefault(\"true\", false) = %v, want true", got)
	}

	t.Setenv(key, "1")
	if got := envBoolOrDefault(key, false); got != true {
		t.Errorf("envBoolOrDefault(\"1\", false) = %v, want true", got)
	}

	t.Setenv(key, "false")
	if got := envBoolOrDefault(key, true); got != false {
		t.Errorf("envBoolOrDefault(\"false\", true) = %v, want false", got)
	}

	// An unparsable value must not silently flip a security-relevant
	// default the wrong way - it falls back to def, exactly like an
	// unset value.
	t.Setenv(key, "not-a-bool")
	if got := envBoolOrDefault(key, false); got != false {
		t.Errorf("envBoolOrDefault(\"not-a-bool\", false) = %v, want false (fall back to def)", got)
	}
}

// issue #119's review finding that neither http.Server this binary built
// set any request-level timeout at all (the standard Go "Slowloris" gap)
// is now proven at its source: apps/common/webhost/serve's own
// TestNewHTTPServer_SetsTimeouts, since serve.NewHTTPServer is what both
// cmdServe and cmdServeUI build their *http.Server through (issue #129).

// TestCmdServe_AcceptsTrustForwardedHeadersAndPublicBaseURLFlags proves
// both flags are actually registered on serve's own flag set: the command
// still fails fast on the invalid config file (exit 1, from fail()),
// never flag.ContinueOnError's own exit 2 for an unrecognized flag - if
// either flag weren't wired up, this would fail with 2 instead.
//
// --state-database (issue #176) is checked here too, for the same reason
// and in the same way: it is a serve flag, and a flag nothing registers
// is indistinguishable from one nothing reads until a run exits 2.
func TestCmdServe_AcceptsTrustForwardedHeadersAndPublicBaseURLFlags(t *testing.T) {
	got := run(invalidConfigArgs(t,
		"--trust-forwarded-headers",
		"--public-base-url", "http://example.test:8080",
		"--state-database", "/data/state/state.db",
	))
	if got == 0 {
		t.Error("run with an invalid config file = 0, want non-zero")
	}
	if got == 2 {
		t.Error("run = 2 (flag-parsing failure), want a config-open failure - one of the flags may not be registered")
	}
}

func TestCmdServeUI_RejectsAnInvalidUpstream(t *testing.T) {
	if got := run([]string{"serve-ui", "--upstream", "://not-a-valid-url"}); got != 2 {
		t.Errorf("run([\"serve-ui\", \"--upstream\", \"://not-a-valid-url\"]) = %d, want 2", got)
	}
}

func TestCmdServeUI_RejectsAnUpstreamWithNoHost(t *testing.T) {
	if got := run([]string{"serve-ui", "--upstream", "/just-a-path"}); got != 2 {
		t.Errorf("run([\"serve-ui\", \"--upstream\", \"/just-a-path\"]) = %d, want 2 (missing scheme/host)", got)
	}
}

func TestLocalHealthcheckURL(t *testing.T) {
	cases := map[string]string{
		":8080":           "http://127.0.0.1:8080/",
		"0.0.0.0:8080":    "http://127.0.0.1:8080/",
		"127.0.0.1:18080": "http://127.0.0.1:18080/",
		"not-a-host-port": "http://127.0.0.1not-a-host-port/",
	}
	for addr, want := range cases {
		if got := localHealthcheckURL(addr); got != want {
			t.Errorf("localHealthcheckURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestCmdHealthcheck_SucceedsAgainstA200Response(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if got := run([]string{"healthcheck", "--url", srv.URL}); got != 0 {
		t.Errorf("run([\"healthcheck\", \"--url\", %q]) = %d, want 0", srv.URL, got)
	}
}

func TestCmdHealthcheck_FailsAgainstA5xxResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if got := run([]string{"healthcheck", "--url", srv.URL}); got == 0 {
		t.Error("run([\"healthcheck\"]) against a 503 response = 0, want non-zero")
	}
}

func TestCmdHealthcheck_FailsWhenNothingIsListening(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	if got := run([]string{"healthcheck", "--url", closedURL}); got == 0 {
		t.Error("run([\"healthcheck\"]) against a closed port = 0, want non-zero")
	}
}
