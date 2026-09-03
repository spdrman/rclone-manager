package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_BackupSetVerbsRefuseEachOthersFlags is the composition's own
// test, and it exists because merging #350's `patch` and #356's `create`
// into one command created a hazard neither branch had on its own.
//
// Separately, each verb declared only its own flags, so passing the
// other's was an unknown flag and `flag` refused it. Together they share
// one FlagSet, so every flag parses for every verb, and the failure
// becomes silent: `backup-set patch --read-only` would have exited 0
// having changed nothing about the posture the operator just asked for,
// and `backup-set create --acknowledge-repoint` would have accepted an
// acknowledgement of a repoint that cannot happen on a set being created.
//
// Both directions, because a guard that only knows about one verb's flags
// is half a guard, and both are checked against a control that the same
// invocation without the wrong flag really does succeed. Without the
// control this would also pass against a command that refused everything.
func TestRun_BackupSetVerbsRefuseEachOthersFlags(t *testing.T) {
	t.Run("patch refuses create's flags", func(t *testing.T) {
		configPath := writeTestConfig(t)
		before, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		newLocal := filepath.Join(t.TempDir(), "moved")
		base := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary",
			"--local-path", newLocal}

		for _, wrong := range [][]string{
			{"--read-only"},
			{"--disabled"},
			{"--run"},
			{"--trust-host-key"},
			{"--ssh-key-id", "some-key"},
			{"--known-hosts-line", "example.com ssh-ed25519 AAAA"},
			{"--state-database", "/tmp/nope.db"},
		} {
			args := append(append([]string{}, base...), wrong...)
			out := captureStderr(t, func() {
				if got := run(args); got != 2 {
					t.Errorf("run(%v) = %d, want 2 (a usage error): %s is a create flag, and accepting it here changes nothing while exiting 0", args, got, wrong[0])
				}
			})
			if !strings.Contains(out, strings.TrimPrefix(wrong[0], "--")) {
				t.Errorf("the refusal for %s does not name the flag it refused: %q", wrong[0], out)
			}
		}

		after, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("a refused patch still wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
		}

		// The control. Without it every assertion above would also pass
		// against a patch verb that refused this invocation outright.
		if got := run(base); got != 0 {
			t.Fatalf("run(%v) = %d, want 0: the same patch without the create flag has to work, or the cases above prove nothing", base, got)
		}
	})

	t.Run("create refuses patch's flag", func(t *testing.T) {
		configPath := writeTestConfig(t)
		keyPath := writeTestPrivateKey(t)
		args := createArgs(configPath, keyPath, "api/acknowledged", "--acknowledge-repoint")
		out := captureStderr(t, func() {
			if got := run(args); got != 2 {
				t.Errorf("run(%v) = %d, want 2 (a usage error): there is nothing to repoint on a set that does not exist yet", args, got)
			}
		})
		if !strings.Contains(out, "acknowledge-repoint") {
			t.Errorf("the refusal does not name the flag it refused: %q", out)
		}

		// The control, again: the same create without the patch flag is a
		// real one, so the refusal above is about the flag.
		if got := run(createArgs(configPath, keyPath, "api/acknowledged")); got != 0 {
			t.Fatal("the same create without --acknowledge-repoint has to succeed, or the case above proves nothing")
		}
	})
}
