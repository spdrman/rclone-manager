package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func TestCmdServe_FailsFastOnAMissingConfigFile(t *testing.T) {
	if got := run([]string{"serve", "--config", "/does/not/exist/config.yaml"}); got == 0 {
		t.Error("run([\"serve\"]) against a missing config file = 0, want a non-zero exit code")
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

// TestNewHTTPServer_SetsTimeouts is issue #119's review finding that
// neither http.Server this binary builds set any request-level
// timeout at all (the standard Go "Slowloris" gap): both cmdServe and
// cmdServeUI build their *http.Server through this one helper now, so
// this is the one place that needs to prove the timeouts are actually
// set.
func TestNewHTTPServer_SetsTimeouts(t *testing.T) {
	s := newHTTPServer(":0", http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout is not set (Slowloris protection)")
	}
	if s.ReadTimeout <= 0 {
		t.Error("ReadTimeout is not set")
	}
	if s.WriteTimeout <= 0 {
		t.Error("WriteTimeout is not set")
	}
	if s.IdleTimeout <= 0 {
		t.Error("IdleTimeout is not set")
	}
}

// TestCmdServe_AcceptsTrustForwardedHeadersAndPublicBaseURLFlags proves
// both new flags are actually registered on serve's own flag set: the
// command still fails fast on the missing config file (exit 1, from
// fail()), never flag.ContinueOnError's own exit 2 for an unrecognized
// flag - if either flag weren't wired up, this would fail with 2 instead.
func TestCmdServe_AcceptsTrustForwardedHeadersAndPublicBaseURLFlags(t *testing.T) {
	got := run([]string{
		"serve",
		"--config", "/does/not/exist/config.yaml",
		"--trust-forwarded-headers",
		"--public-base-url", "http://example.test:8080",
	})
	if got == 0 {
		t.Error("run with a missing config file = 0, want non-zero")
	}
	if got == 2 {
		t.Error("run = 2 (flag-parsing failure), want a config-open failure - one of the new flags may not be registered")
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
