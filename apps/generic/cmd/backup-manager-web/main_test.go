package main

import "testing"

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
