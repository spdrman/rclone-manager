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

func TestDisplayBaseURL(t *testing.T) {
	cases := map[string]string{
		":8080":           "http://localhost:8080",
		"0.0.0.0:8080":    "http://0.0.0.0:8080",
		"127.0.0.1:18080": "http://127.0.0.1:18080",
	}
	for addr, want := range cases {
		if got := displayBaseURL(addr); got != want {
			t.Errorf("displayBaseURL(%q) = %q, want %q", addr, got, want)
		}
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
