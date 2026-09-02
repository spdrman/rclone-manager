package rclone

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// canary is a value that exists nowhere else in this repository, so a scan
// for it cannot be satisfied by anything but a real leak of what a
// resolver produced.
const canary = "CANARY-a2f4c1e07b9d43ab-DO-NOT-LOG"

const canaryAccessKeyID = "AKIA" + canary + "ID"

func canaryCredentialsText() string {
	return "[default]\n" +
		"aws_access_key_id = " + canaryAccessKeyID + "\n" +
		"aws_secret_access_key = " + canary + "\n"
}

// TestMediumCredentialHelperProcess is not a test. It is the executable a
// `command` credential resolver runs, re-executing this same test binary,
// which is the standard os/exec idiom for getting a real subprocess
// without shipping a fixture script.
//
// What it prints is chosen by a MODE named in argv, and the text itself is
// a constant compiled into this binary. Nothing hands the child the canary
// itself: not through argv, where a process listing would show it, and not
// through a file, which would put it on disk. It cannot come through the
// environment either, because runResolverCommand deliberately gives the
// child none, which is the property TestCommandResolverGetsNoAmbientEnvironment
// exists to prove.
func TestMediumCredentialHelperProcess(t *testing.T) {
	args := flag.Args()
	if len(args) < 2 || args[0] != helperMarker {
		t.Skip("helper process only; driven by the command-resolver tests in this file")
	}
	switch args[1] {
	case "credentials":
		fmt.Print(canaryCredentialsText())
	case "junk":
		fmt.Print("this is not credentials, and it carries " + canary)
	case "env-dump":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
	}
	os.Exit(0)
}

// helperMarker separates this binary's own flags from the helper's mode.
// Go's flag package stops parsing at "--", so everything after it lands in
// flag.Args() rather than failing as an unknown flag.
const helperMarker = "medium-creds-helper"

func helperCommand(mode string) []string {
	return []string{os.Args[0], "-test.run=^TestMediumCredentialHelperProcess$", "--", helperMarker, mode}
}

func TestResolveCredentialsFromEachSource(t *testing.T) {
	t.Run("file is never opened by this process", func(t *testing.T) {
		// The file source's whole property is that this adapter does not
		// read it, so what is asserted here is the OPTIONS it produces,
		// not any parsed content: env_auth on, the path passed through,
		// and no static key set anywhere.
		dir := t.TempDir()
		path := filepath.Join(dir, "offsite.creds")
		if err := os.WriteFile(path, []byte("this is deliberately not credentials text"), 0o600); err != nil {
			t.Fatalf("writing the credentials file: %v", err)
		}

		cfg, err := mediumAuthOptions(transport.Medium{
			ID:          "offsite_s3",
			Type:        transport.MediumTypeS3,
			Credentials: transport.MediumCredentials{File: path},
		})
		if err != nil {
			t.Fatalf("mediumAuthOptions: %v", err)
		}
		if got, _ := cfg.Get("env_auth"); got != "true" {
			t.Errorf("env_auth = %q, want %q; rclone only consults the shared credentials file when it is set", got, "true")
		}
		if got, _ := cfg.Get("shared_credentials_file"); got != path {
			t.Errorf("shared_credentials_file = %q, want %q", got, path)
		}
		for _, option := range []string{"access_key_id", "secret_access_key", "session_token"} {
			if got, ok := cfg.Get(option); ok {
				t.Errorf("the file source set %s = %q; setting a static key here would defeat env_auth and would mean this process read the file", option, got)
			}
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("BACKUP_S3_CANARY", canaryCredentialsText())
		cfg, err := mediumAuthOptions(transport.Medium{
			ID:          "offsite_s3",
			Type:        transport.MediumTypeS3,
			Credentials: transport.MediumCredentials{Env: "BACKUP_S3_CANARY"},
		})
		if err != nil {
			t.Fatalf("mediumAuthOptions: %v", err)
		}
		assertStaticCredentials(t, cfg)
	})

	t.Run("command", func(t *testing.T) {
		cfg, err := mediumAuthOptions(transport.Medium{
			ID:          "offsite_s3",
			Type:        transport.MediumTypeS3,
			Credentials: transport.MediumCredentials{Command: helperCommand("credentials")},
		})
		if err != nil {
			t.Fatalf("mediumAuthOptions: %v", err)
		}
		assertStaticCredentials(t, cfg)
	})
}

func assertStaticCredentials(t *testing.T, cfg interface{ Get(string) (string, bool) }) {
	t.Helper()
	if got, _ := cfg.Get("access_key_id"); got != canaryAccessKeyID {
		t.Errorf("access_key_id = %q, want the resolved one", got)
	}
	if got, _ := cfg.Get("secret_access_key"); got != canary {
		t.Errorf("secret_access_key did not come through as resolved")
	}
	if got, _ := cfg.Get("env_auth"); got != "false" {
		t.Errorf("env_auth = %q, want %q: a static key is configured, so the ambient chain must not be consulted", got, "false")
	}
}

// TestCommandResolverGetsNoAmbientEnvironment proves the hardening
// runResolverCommand's doc claims, rather than trusting it: the child gets
// a fixed minimal environment, so a variable this process holds for
// something unrelated does not reach it.
//
// It is written as a resolver failure on purpose. The helper prints
// whatever helperEnvStdout says; with the parent's environment withheld
// the helper sees nothing, prints nothing, and the resolver refuses an
// empty result. A child that DID inherit the environment would print
// valid credentials and this test would pass silently, which is exactly
// the inversion that makes it a real assertion.
func TestCommandResolverGetsNoAmbientEnvironment(t *testing.T) {
	// The helper is invoked with no mode, so it prints nothing, and the
	// resolver refuses an empty result. A child that inherited this
	// process's environment could still find nothing to print, so the
	// stronger half is the assertion below: the child must not see a
	// variable this process holds.
	t.Setenv("MEDIUM_CREDS_LEAK_PROBE", "a value this process holds for something unrelated")
	if _, err := resolveCredentialsFromCommand(helperCommand("")); err == nil {
		t.Fatal("the command resolver accepted an empty result")
	}

	out, err := runResolverCommand(helperCommand("env-dump"), maxResolvedCredentialsSize)
	if err != nil {
		t.Fatalf("running the env-dump helper: %v", err)
	}
	dumped := out.buf.String()
	if strings.Contains(dumped, "MEDIUM_CREDS_LEAK_PROBE") {
		t.Errorf("the child inherited this process's environment; it saw:\n%s", dumped)
	}
	if !strings.Contains(dumped, "PATH=") {
		t.Errorf("the child saw no PATH at all, so this assertion is not actually reading its environment; it saw:\n%s", dumped)
	}
}

func TestResolveCredentialsRefusesWhatIsNotCredentials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  error
	}{
		{"empty", "", errCredentialsEmpty},
		{"whitespace only", "   \n\t\n", errCredentialsEmpty},
		{"an HTML login page", "<html><body>Please sign in</body></html>", errCredentialsUnreadable},
		{"a JSON blob", `{"accessKeyId":"AKIA","secretAccessKey":"s"}`, errCredentialsUnreadable},
		{"keys with no profile header", "aws_access_key_id = AKIA\naws_secret_access_key = s", errCredentialsUnreadable},
		{"a profile with no keys", "[default]\n", errCredentialsIncomplete},
		{"a profile with only the id", "[default]\naws_access_key_id = AKIA\n", errCredentialsIncomplete},
		{"two profiles and no default", "[prod]\naws_access_key_id = A\naws_secret_access_key = s\n[dev]\naws_access_key_id = B\naws_secret_access_key = t\n", errCredentialsAmbiguousProfile},
		{"a wrapped secret", "[default]\naws_access_key_id = AKIA\naws_secret_access_key = abc def\n", errCredentialsMalformedValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSharedCredentials([]byte(tc.value))
			if err == nil {
				t.Fatalf("parseSharedCredentials accepted %q", tc.value)
			}
			if err != tc.want {
				t.Errorf("parseSharedCredentials refused with %v, want %v", err, tc.want)
			}
			if trimmed := strings.TrimSpace(tc.value); trimmed != "" && strings.Contains(err.Error(), trimmed) {
				t.Errorf("the refusal echoed the bytes that failed: %v", err)
			}
		})
	}
}

// TestResolveCredentialsAcceptsASingleNamedProfile is the accepting
// counterpart to the ambiguity refusal above: one profile is unambiguous
// whatever it is called.
func TestResolveCredentialsAcceptsASingleNamedProfile(t *testing.T) {
	resolved, err := parseSharedCredentials([]byte("; a comment\n[offsite]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = secretvalue\naws_session_token = tokenvalue\n"))
	if err != nil {
		t.Fatalf("parseSharedCredentials: %v", err)
	}
	if resolved.accessKeyID.Reveal() != "AKIAEXAMPLE" {
		t.Errorf("access key id did not come through")
	}
	if !resolved.hasSessionToken || resolved.sessionToken.Reveal() != "tokenvalue" {
		t.Error("the session token did not come through")
	}
}

func TestMediumAuthOptionsRequiresExactlyOneSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds transport.MediumCredentials
	}{
		{"none", transport.MediumCredentials{}},
		{"file and env", transport.MediumCredentials{File: "/x", Env: "Y"}},
		{"env and command", transport.MediumCredentials{Env: "Y", Command: []string{"/bin/true"}}},
		{"all three", transport.MediumCredentials{File: "/x", Env: "Y", Command: []string{"/bin/true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mediumAuthOptions(transport.Medium{ID: "m", Type: transport.MediumTypeS3, Credentials: tc.creds})
			if err == nil {
				t.Fatal("mediumAuthOptions accepted it")
			}
			if category, ok := transport.CategoryOf(err); !ok || category != transport.Configuration {
				t.Errorf("classified as %v (recognised=%v), want %s", category, ok, transport.Configuration)
			}
		})
	}
}

func TestCredentialsFileCustody(t *testing.T) {
	t.Run("a world-readable file is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "offsite.creds")
		if err := os.WriteFile(path, []byte("[default]\n"), 0o644); err != nil {
			t.Fatalf("writing the credentials file: %v", err)
		}
		err := checkCredentialsFileCustody("offsite_s3", path)
		if err == nil {
			t.Fatal("a 0644 credentials file was accepted")
		}
		if !strings.Contains(err.Error(), "0644") {
			t.Errorf("the refusal does not name the mode it found: %v", err)
		}
	})

	t.Run("a world-writable containing directory is refused", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "creds")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("creating the directory: %v", err)
		}
		// Chmod after Mkdir, not a mode argument to it: the process umask
		// silently strips the very bits this case is about, which would
		// leave the test passing against a check that never ran.
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatalf("widening the directory: %v", err)
		}
		path := filepath.Join(dir, "offsite.creds")
		if err := os.WriteFile(path, []byte("[default]\n"), 0o600); err != nil {
			t.Fatalf("writing the credentials file: %v", err)
		}
		if err := checkCredentialsFileCustody("offsite_s3", path); err == nil {
			t.Fatal("a credentials file inside a world-writable directory was accepted")
		}
	})

	t.Run("a 0600 file in a private directory is accepted", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "creds")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("creating the directory: %v", err)
		}
		path := filepath.Join(dir, "offsite.creds")
		if err := os.WriteFile(path, []byte("[default]\n"), 0o600); err != nil {
			t.Fatalf("writing the credentials file: %v", err)
		}
		if err := checkCredentialsFileCustody("offsite_s3", path); err != nil {
			t.Errorf("a correctly-protected credentials file was refused: %v", err)
		}
	})

	t.Run("a missing file is refused as configuration", func(t *testing.T) {
		err := checkCredentialsFileCustody("offsite_s3", filepath.Join(t.TempDir(), "absent.creds"))
		if err == nil {
			t.Fatal("a missing credentials file was accepted")
		}
		if category, ok := transport.CategoryOf(err); !ok || category != transport.Configuration {
			t.Errorf("classified as %v (recognised=%v), want %s", category, ok, transport.Configuration)
		}
	})
}

// observableOutputs collects everything FR-33 says must never contain a
// credential, for one resolution through one source: the error a failing
// resolve produced, every log line written while it ran, the Medium
// descriptor rendered under every verb a debugging operator reaches for,
// and that descriptor's JSON.
//
// The rclone OPTIONS are deliberately not in here, because the resolved
// key genuinely IS in them: that is the one place it is allowed to be, on
// its way into rclone. What this asserts is that it goes nowhere else.
func observableOutputs(t *testing.T, medium transport.Medium) string {
	t.Helper()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var out strings.Builder

	if _, err := mediumAuthOptions(medium); err != nil {
		out.WriteString(err.Error())
		out.WriteString("\n")
	}

	// A resolve that failed and a resolve that succeeded leak by different
	// routes, so both are exercised: the failure through its error, the
	// success through everything it may have logged on the way.
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		fmt.Fprintf(&out, verb+"\n", medium)
	}
	encoded, err := json.Marshal(medium)
	if err != nil {
		t.Fatalf("json.Marshal(medium): %v", err)
	}
	out.Write(encoded)
	out.WriteString("\n")
	out.WriteString(logs.String())
	return out.String()
}

// TestMediumCredentialCanary is FR-33's enforcement. A known canary is
// resolved through each of the three sources, and it must appear in none
// of the outputs FR-33 names.
func TestMediumCredentialCanary(t *testing.T) {
	credsDir := t.TempDir()
	credsPath := filepath.Join(credsDir, "offsite.creds")
	// The FILE source's canary is the path, not the secret: the whole
	// point of that source is that this process never reads the content,
	// so the content this test writes is deliberately inert.
	if err := os.WriteFile(credsPath, []byte("[default]\naws_access_key_id = inert\naws_secret_access_key = inert\n"), 0o600); err != nil {
		t.Fatalf("writing the credentials file: %v", err)
	}

	t.Setenv("BACKUP_S3_CANARY", canaryCredentialsText())

	for _, tc := range []struct {
		name  string
		creds transport.MediumCredentials
	}{
		{"file", transport.MediumCredentials{File: credsPath}},
		{"env", transport.MediumCredentials{Env: "BACKUP_S3_CANARY"}},
		{"command", transport.MediumCredentials{Command: helperCommand("credentials")}},
		{"a command whose output is not credentials at all", transport.MediumCredentials{Command: helperCommand("junk")}},
		// A resolver that fails is where an implementation is most
		// tempted to quote what it got, so the failing shapes are canaried
		// as hard as the succeeding ones.
		{"env holding a canary that is not credentials at all", transport.MediumCredentials{Env: "BACKUP_S3_CANARY_JUNK"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BACKUP_S3_CANARY_JUNK", "this is not credentials, and it carries "+canary)
			medium := transport.Medium{
				ID:          "offsite_s3",
				Type:        transport.MediumTypeS3,
				Bucket:      "nas-backups",
				Prefix:      "rclone-manager",
				Credentials: tc.creds,
			}
			observed := observableOutputs(t, medium)
			if strings.Contains(observed, canary) {
				t.Errorf("the canary reached an observable output:\n%s", observed)
			}
		})
	}
}

// TestMediumCredentialCanaryFindsAPlantedLeak is the positive control, and
// it is the half that makes the test above mean anything. An absence
// assertion that cannot fail is not an assertion, and this repository has
// hit that shape more than once.
//
// The planted violation is FR-33's own: a build that logs the resolved
// medium configuration verbatim. It is planted here by doing exactly that,
// with the real resolved options, into the same log the canary scan reads,
// and the scan must find it.
func TestMediumCredentialCanaryFindsAPlantedLeak(t *testing.T) {
	t.Setenv("BACKUP_S3_CANARY", canaryCredentialsText())
	medium := transport.Medium{
		ID:          "offsite_s3",
		Type:        transport.MediumTypeS3,
		Bucket:      "nas-backups",
		Credentials: transport.MediumCredentials{Env: "BACKUP_S3_CANARY"},
	}

	cfg, err := mediumAuthOptions(medium)
	if err != nil {
		t.Fatalf("mediumAuthOptions: %v", err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// THE PLANTED VIOLATION.
	slog.Info("resolved medium config", "medium", medium.ID, "options", fmt.Sprintf("%v", cfg))

	if !strings.Contains(logs.String(), canary) {
		t.Fatalf("the canary scan did not find a deliberately planted verbatim log of the resolved config; "+
			"the gate in TestMediumCredentialCanary is therefore not proven to be able to fail. Log was:\n%s", logs.String())
	}
}
